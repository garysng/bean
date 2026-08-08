# S3 Storage Layer Design

> 中文版:[zh/s3-storage.md](zh/s3-storage.md)

> The status-marker convention is defined in [architecture.md](architecture.md) §0.
> Implementation: `internal/control/s3/` (the protocol layer), `internal/control/snapshot/` (the storage abstraction).

S3 is the platform's unified persistence backend — snapshot blobs live there, and by design
image blobs and artifacts should too. This document covers **how the protocol layer was
implemented in-house** and **why the abstraction above it looks the way it does**.

## 1. Why the AWS SDK is not pulled in ✅

Pulling in `aws-sdk-go-v2` means dozens of modules and hundreds of transitive dependencies,
while what we use is GET / PUT / DELETE / HEAD plus multipart upload — five operations.

The cost is handling SigV4's details ourselves, and that is not as simple as "compute an HMAC":
**compatibility is almost always lost at the canonicalisation step**, especially against
non-AWS implementations (MinIO, Ceph RGW, the S3-compatible layers of various clouds). So the
comment in `sign.go` says as much: the algorithm is fully specified by AWS, and the value here
is in getting the canonicalisation right.

The benefit is not only dependency size. Implementing it ourselves means:
- No hidden retry, connection-pool or region-resolution logic on the request path — when
  something goes wrong, all of it is visible
- Precise control over which headers participate in the signature (see §2.2), which is exactly
  what needs tuning when integrating with non-AWS implementations

## 2. Key points of the SigV4 implementation ✅

### 2.1 Four-layer derivation of the signing key

```
kDate    = HMAC("AWS4" + secretKey, dateStamp)
kRegion  = HMAC(kDate, region)
kService = HMAC(kRegion, "s3")
kSigning = HMAC(kService, "aws4_request")
```

Every layer narrows the scope once, so a leaked `kSigning` is only usable on that day, in that
region, for that service. That is SigV4's design benefit, and it is why this cannot be
simplified into a single HMAC.

### 2.2 Sign only the headers we control

```go
if lower == "host" || lower == "content-type" || strings.HasPrefix(lower, "x-amz-") {
```

The more headers are signed, the easier it is for a middlebox to invalidate the signature in
transit (a proxy adding `X-Forwarded-For`, adding `Via`, or normalising `Accept-Encoding` are
all common). So only `host`, `content-type` and `x-amz-*` are signed — the first two are
semantically required, and the last are ours.

Three canonicalisation details, any one of which produces a signature mismatch rather than a
clear error:

- **Names lowercased, sorted lexicographically, deduplicated**. Go's `http.Header` is in
  canonical-MIME form (`X-Amz-Date`), so it has to be lowercased
- **Values `TrimSpace`d**. The server trims; a client that does not will not match
- **`host` may not be in `req.Header`**. Go keeps it in the `req.Host` field, so it has to be
  added to the list separately and taken from `req.URL.Host`

### 2.3 An empty body still needs a payload hash

```go
emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
```

That is the SHA-256 of the empty byte string. S3 **requires** `X-Amz-Content-Sha256` to be
present even when the body is empty — omitting the header presents as a signature mismatch
rather than as "a required header is missing". Hard-coding the constant is more direct than
hashing the empty string every time.

### 2.4 The canonical URI and query

- The URI uses `req.URL.EscapedPath()`, falling back to `/` when empty. Using the unescaped
  path fails when the key contains spaces or CJK characters
- The query uses `req.URL.Query().Encode()` — Go's implementation happens to satisfy SigV4's
  requirement of "sorted by key, with key and value each escaped"

### 2.5 Clock skew

The signature carries `X-Amz-Date`, and the server usually tolerates ±15 minutes. **No clock
correction is done** — when the host clock is off by more than 15 minutes the request is
rejected with `RequestTimeTooSkewed`.
That decision is deliberate: a machine that far off has more serious problems (TLS, leases, log
ordering), and compensating for it inside the S3 client would only mask them.

## 3. Multipart upload ✅

```go
// S3 requires at least 5 MiB for all but the final part; 16 MiB keeps the
// part count low without holding much memory.
const DefaultPartSize = 16 << 20
```

**Why 16 MiB rather than 5 MiB**: the part count has a ceiling (10000), so 5 MiB parts mean a
maximum object of 50 GB; 16 MiB gets to 160 GB, while the memory footprint is just one part's
buffer. The 5 MiB lower bound is S3's hard requirement (the final part excepted), so it cannot
be smaller.

**Why not streaming signatures (`STREAMING-AWS4-HMAC-SHA256`)**: that requires writing a
signature header ahead of every chunk, whereas our writer side is an `io.Writer` (the snapshot
bundle streams straight in). Multipart upload buys a simpler implementation at the same cost:
each part is signed independently, reusing the single-shot path's `signer`.

### A failure must not leave a readable half-product

This is the `Blobs` interface's contract, and the main source of multipart upload's complexity:

```go
// Abort discards the upload and its parts, so a failed write does not leave
// a partial object.
func (u *Uploader) Abort()
```

Multipart upload satisfies this naturally — **the object does not exist before
`CompleteMultipartUpload`**. What an interrupted upload leaves behind is invisible parts,
reclaimed by a bucket lifecycle rule (that one needs operator configuration, otherwise they
accumulate and get billed).

## 4. The Blobs abstraction: why these four methods ✅

```go
type Blobs interface {
    Writer(id string) (io.WriteCloser, error)
    Reader(id string) (io.ReadCloser, error)
    Size(id string) (int64, error)
    Delete(id string) error
}
```

**`Delete` on a nonexistent blob is not an error.** The cleanup path has to be idempotent — the
snapshot record was deleted but the blob deletion failed, and on retry the blob is already
gone; that should not be an error. Treating "does not exist" as success means the cleanup logic
never has to distinguish "deleted it" from "there was nothing there".

**`Reader` returns `ErrBlobNotFound` when it is missing (not a generic error).** The layer above
has to turn that into a 404 `SNAPSHOT_DATA_MISSING` — the record is there but the data is gone,
which is a different failure from "the snapshot does not exist" and calls for different
operator handling.

**`Abort` is not in the interface.** A type assertion is used instead:

```go
func AbortWrite(blobs Blobs, id string, w io.WriteCloser) {
    if a, ok := w.(Aborter); ok {
        a.Abort()
        return
    }
    _ = w.Close()
    _ = blobs.Delete(id)
}
```

Reasoning: `DirBlobs`' "abort" is just deleting the temporary file, and its `Close` has to do a
rename anyway — the two are two faces of the same action, and forcing it into the interface
would give the local implementation an extra method existing only to satisfy the interface. The
assertion lets the **implementation that can do better** (S3's `AbortMultipartUpload` clears
every part in one call) take the fast path, with everything else on the generic "close then
delete" fallback.

### The local directory implementation's equivalence to S3

`DirBlobs` writes a temporary file + `rename`, S3 goes through a multipart commit. Both provide
the same guarantee: **before the write completes, a reader sees nothing at all**. So dev uses a
local directory and production uses S3, and the code above plus the tests need not distinguish
them at all.

## 5. Range reads ✅

`Client.GetRange` is the basis for two paths:

- **Snapshot restore**: although today the whole bundle is read as a stream, once `snapCache`
  hits only that one rootfs member is needed — split into a separate object, only the required
  range would have to be read
  (this is the optimisation direction for restore's remaining ~950ms, see status.md)
- **overlaybd lazy-pull**: the entire mechanism is block-level range reads. Measured: 8 HTTP
  206s are enough to mount and read files (decisions.md §3.1)

## 6. Credentials ⚠️

**Secrets are read only from environment variables; the endpoint may be a flag**:

```
--s3-endpoint (or BEAN_S3_ENDPOINT)   # not sensitive, either form is fine
BEAN_S3_ACCESS_KEY                    # environment variable only
BEAN_S3_SECRET_KEY                    # environment variable only
```

The distinction is deliberate: a flag shows up in `/proc/<pid>/cmdline`, where any local user
running `ps` can see it, so the keys have no corresponding flag. The endpoint is not sensitive,
so it gets a flag for convenience while debugging.

Environment variables are not strong protection either (`/proc/<pid>/environ` is equally
readable, just restricted to the same uid), but at least they are not in `ps`'s default output
and do not end up in shell history.

**noded talks to S3 directly under `--fc-overlaybd`** (`grep -rn BEAN_S3 cmd/noded/`
now returns 5 hits). It constructs an S3 client (`cmd/noded/main.go` `s3.New(...)` →
`NewS3BlobStore(...)`) from `BEAN_S3_ACCESS_KEY` / `BEAN_S3_SECRET_KEY`, and the
resulting `OverlaybdBlobs` store publishes and range-reads sealed layers
(`internal/node/image/obdblobstore.go`). Snapshot blobs for the dm-snapshot path
still flow node → gRPC → gateway → S3, but the node-side S3 access under overlaybd is
now real, and so is the need to manage its credentials.

### Known gaps 📐

In the design (security-and-startup §A5) a node **should not hold long-lived credentials** when
it does need to upload directly:

- **presigned URLs** are unimplemented — a node uploading artifacts, and artifacts uploaded
  directly from inside a sandbox, should both use short-lived URLs issued by the control plane
  and bound to a key prefix and content-length
- **STS read-only role rotation** is unimplemented — the node already range-reads blobs directly
  under `--fc-overlaybd-lazy-pull`, and it does so with long-lived `BEAN_S3_ACCESS_KEY` /
  `BEAN_S3_SECRET_KEY` rather than rotated STS credentials. That is the real gap today: what it
  needs is a read-only temporary credential rotated every 1h, scoped to the blob bucket prefix

Put differently: node-side S3 access is no longer hypothetical — the node holds long-lived
credentials the moment overlaybd is enabled, so the STS gap is a live concern, not a future one.
Presigned uploads for build outputs (#22) remain a prerequisite for node/sandbox artifact upload;
handing the node long-lived credentials out of laziness is already a substantive security
regression, not a deferred one.

## 7. Testing strategy ✅

Three layers:

| Layer | Location | What it verifies |
|---|---|---|
| Signature unit tests | `sign_test.go` | byte-level correctness of the canonical request — the easiest thing to get wrong and the hardest to debug |
| Protocol unit tests | `client_test.go` / `multipart_test.go` | a `httptest` fake server verifying request shape, part splitting and abort behaviour |
| Integration tests | `client_integration_test.go` / `s3blobs_test.go` | **against a real MinIO**, skipped when `BEAN_S3_ENDPOINT` is unset |

Why the integration tests are necessary: the `ErrBlobNotFound` mapping, the object genuinely
not existing after an abort, the boundaries of a range read — these are **the server's
behaviour** rather than our wrapper's, and a fake server can only verify what we sent, not how
a real S3 responds.

CI runs a real MinIO, so this layer is not "optional extra verification".
