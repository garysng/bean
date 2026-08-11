# exec / file transfer via the data plane, not the gateway

> Status: ✅ **implemented.** `exec` and file transfer no longer relay through
> bean-api when a proxy is configured: they reach the agent node-direct over the
> bean-proxy data plane. All six stages in §5 landed — token injection
> (`internal/node/portforward.go`), the domain on the create response
> (`internal/control/api/server.go`), the CLI client (`cli/dataplane.go`), the
> real-host probe (`hack/exec-via-proxy-probe.sh`) and the Python SDK client
> (`sdk/python/bean/_dataplane.py`). §5 is kept as the record of the order the
> work was done in. Authority order holds: code > `status.md` > `decisions.md` >
> design docs > this page.
>
> The path is opt-in, not the default: with `BEAN_PROXY_URL` unset the client
> stays on the gateway relay, which is what a single-node stack without a proxy
> needs.

> 中文版:[zh/exec-via-proxy.md](zh/exec-via-proxy.md)

---

## 0. Why

The README says `exec` and file transfer "no longer relay through the control
plane" (README §Also-working). The code does not match: `exec` today is three
gRPC hops, and the middle hop is bean-api.

```
client ──HTTP/JSON──► bean-api ──gRPC──► noded ──gRPC (vsock/tcp)──► agent
         POST /exec    handleExec         Exec passthrough           AgentService/Exec
```

- `handleExec` (`server.go:764`) decodes JSON, `resolveNode`, then
  `nodeClient.Exec` — a gRPC call to the node's **control** port. Its own metric
  is labelled "Exec round-trip latency **through the gateway**".
- noded's `Exec` (`grpc.go:185`) is a pure passthrough to the agent
  (`AgentConn` → `AgentService/Exec`).

So every exec and every file byte rides the control plane. That couples data-path
throughput and latency to the gateway, and makes the gateway a bottleneck for
exactly the high-volume operations (streaming files, frequent execs) it should
not be on. Fixing it makes the README true.

## 1. The transport already exists

The important discovery: **the node-direct transport is fully built** and already
carries gRPC end to end. No new HTTP surface on the agent, and no teaching the
proxy about exec, is required.

```
client ──gRPC (h2c)──► bean-proxy ──h2c──► noded PortForwarder ──h2c into netns──► agent:10001
        authority                 reverse-proxy              dial GuestIP:10001    AgentService/Exec
        10001-{sandbox}
```

- **bean-proxy already forwards gRPC.** For the agent port (`AgentGuestPort=10001`)
  it selects an h2c `http2.Transport` (`proxy.go:139-152`), and the whole server
  is wrapped in `h2c.NewHandler` (`bean-proxy/main.go:81`). The "does not speak
  gRPC" comment means it does not *originate or interpret* gRPC — it relays
  HTTP/2 byte-transparently, and deliberately picks h2c for port 10001.
- **noded's PortForwarder** does the same split (`portforward.go:193`,
  `transportFor` → h2c for 10001) and dials into the sandbox netns.
- **The agent's exec API *is* that gRPC.** `AgentService/Exec` on 10001 is exactly
  what the forwarder carries. So an exec is expressible as "a gRPC call to
  `10001-{sandbox}` through the proxy."

What does **not** exist is a client that speaks it. bean-api dials noded's
*control* gRPC port and calls `SandboxService/Exec` (a different service); no
client dials the proxy with authority `10001-{sandbox}` and calls
`AgentService/Exec` directly.

## 2. The knot: credentials vs reachability

This is the part that makes the change non-trivial, and it must be understood
before any code moves. The agent authenticates differently per tier, and the two
tiers that matter are in tension:

| tier | agent listener | auth | has guest IP? | PortForwarder can reach? |
|---|---|---|---|---|
| networked fc | `tcp:0.0.0.0:10001` | **required** — per-sandbox token (`beand/auth.go`, fail-closed) | yes | **yes** |
| no-network fc | `vsock:1024` | none (vsock isolation) | no | no |
| local (dev) | unix socket | none | no | no |

The agent's token check (`auth.go`) verifies a **per-sandbox** token
(`sbxtoken.Verify` against a hash published in MMDS), and it fails closed —
absent credential denies. Its entire purpose (per the code comment) is that once
the agent is served over TCP, the sandbox's *own root* can dial 10001, and only
the per-sandbox token distinguishes noded from that root.

Two facts collide:

1. **The proxy's `bean-node-token` does not satisfy the agent.** That token
   authenticates the proxy *to noded's forwarding port* (`proxy.go:242` comment);
   the agent checks a per-sandbox token, which is a different credential. Passing
   the node token through gets the request to the agent, where it is rejected.
2. **The per-sandbox token's plaintext lives only on noded.** It is minted on the
   node (`manager.go:213`); only the hash goes to MMDS. A client has no way to
   obtain it today.

And reachability cuts the other way: **PortForwarder needs a routable guest IP**
(`TargetFor` errors when `net_ == nil`, `portforward.go:98`), which only the
networked fc tier has — and that is the tier that requires the token. The
token-free tiers (vsock, local) have no guest IP, so the proxy cannot reach them.

**There is no runnable tier today where "go through the proxy without a token"
works**: with networking you need the token, without it the forwarder cannot
reach the agent. So *someone* on the path must present the per-sandbox token —
the only question is who. The design below answers it: **noded injects the
token**, so the client never holds it.

## 3. The design

The decision: **the client never touches the per-sandbox token.** noded injects
it at the forwarder, because noded is the one process that already holds the
plaintext. The client authenticates only to the platform's outer API-key layer;
everything inward of the proxy is the node's trust domain to manage. This is the
Daytona shape — the proxy layer owns agent auth, the caller holds one platform
credential — rather than the E2B shape where the client holds the per-sandbox
token and connects directly.

Three parts. The auth injection is the one with real security weight; the other
two are mechanical.

### 3.1 Auth: noded injects the token at the forwarder

The credential chain becomes:

```
client ──apikey──► bean-proxy ──node-token──► noded PortForwarder ──inject per-sandbox token──► agent
        (outer platform layer)   (existing boundary F)   (noded holds the plaintext)
```

- **client → proxy: apikey only.** We assume every request reaching the proxy has
  already cleared the platform's API-key layer (the final product wraps one). bean
  itself does not re-check here; the client presents no per-sandbox credential
  because it has none.
- **proxy → noded: `bean-node-token`.** Unchanged — the existing forwarding-port
  boundary (`portforward.go:295`).
- **noded → agent: per-sandbox token, injected by the PortForwarder.** Today the
  PortForwarder is a pure passthrough and does *not* inject the token (only noded's
  control-path `AgentConn` does, via `sbxtoken.WithAgentToken`). The change: when
  the forwarder proxies to `GuestIP:10001` for a sandbox, it looks up that
  sandbox's `agentToken` (already in `sandbox.agentToken` from the mint at
  `manager.go:213`) and sets the agent's auth metadata/header on the outbound
  request.

This is the **only** security-weighted change, and it is deliberately confined to
noded. The plaintext token stays a noded-only secret exactly as it is today — it
is *not* surfaced to bean-api or the client. The agent's check (`beand/auth.go`)
stays fail-closed and unchanged: a request that reaches the agent without the
token (e.g. the sandbox's own root dialing 10001) is still rejected. The
forwarder injecting it does not weaken that — it means the *legitimate* proxied
path now carries the credential the agent already demands.

One thing to verify in implementation: the PortForwarder must inject only for the
agent port (10001), not for arbitrary user ports it also forwards. A user's own
server on port 8080 must not receive the agent token.

### 3.2 Addressing: the sandbox returns its domain, the client builds the URL

create returns the sandbox's **domain** (or the proxy base the client should
use). The CLI/SDK constructs the request URL from it — `{port}-{sandbox}` against
that domain — at call time. The proxy only forwards, and every port mapping is
open by default (per-port access control is a separate, unbuilt feature —
[#50](https://github.com/garysng/bean/issues/50)), so no registration call or
host-port pool is involved. This is the E2B subdomain shape, but with the domain
handed back by the server rather than assembled from client-side convention.

- **`create` response** gains the domain/proxy base for the sandbox.
- **CLI/SDK** build `10001-{sandbox}` (for the agent) or `{port}-{sandbox}` (for a
  user port) against that domain and issue the call.
- Falls back to the current bean-api path when no proxy domain is configured, so
  this is additive — a single-node/dev setup is unaffected.

### 3.3 Client: a data-plane gRPC client

CLI and SDK today know only `BEAN_BASE_URL` → bean-api REST. The data-plane path
adds a **gRPC client** that dials the proxy with authority `10001-{sandbox}` and
calls `AgentService/Exec` / `ReadFile` / `WriteFile` / `ListDir` / `DeleteFile`
directly. No per-sandbox token is attached — the client presents nothing beyond
whatever the outer apikey layer requires.

No proxy or forwarder protocol change for transport — both already do h2c to port
10001. The proxy resolves the node via `NodeAddrFor` (already implemented) and
forwards; the forwarder dials `GuestIP:10001` in the netns and now injects the
agent token (§3.1). The transport is entirely reuse; only the token injection is
new.

The Python SDK is stdlib-`urllib` only; a gRPC client means either a `grpcio`
dependency or an h2c/framed alternative. That cost is real and is part of "change
the SDK too."

## 4. Verification can only be real on the fc tier

This constrains how "e2e real verification" is even possible, and it must be
stated plainly:

- The **local tier has no guest network** (`local.go`: unix-socket agent;
  `manager.go:197` provisions networking only with `--guest-subnet`). `TargetFor`
  hits `net_ == nil` for every local sandbox — the PortForwarder cannot reach it.
- `--guest-subnet` needs Linux + KVM + `--uplink` (`cmd/noded/main.go:411`).
- The current e2e stack (`tests/e2e/e2e_test.go`) starts only bean-api + noded,
  `--runtime local`, no bean-proxy, no `--sandbox-port-listen`.

So a genuine "exec through the proxy" e2e **must run on the fc tier on a real
Linux/KVM host**, with a stack that additionally starts bean-proxy and runs noded
with `--sandbox-port-listen`. It cannot be exercised in the local-tier CI stack.
The verification plan is therefore a `hack/` script (in the shape of
`guest-egress-probe.sh`) that, on a real microVM host: creates a sandbox, execs
through the proxy, asserts the output, round-trips a file through the proxy, and
asserts the bytes — then, to prove auth still fails closed, dials `10001-{sandbox}`
through the forwarder path *without* noded's injection (simulating the sandbox's
own root) and asserts the agent denies. That last step matters: the whole security
argument is that only the noded-injected path carries the token, and the agent
still rejects everything else.

## 5. Staged plan

1. **Token injection in the forwarder** (§3.1) — the PortForwarder injects the
   per-sandbox `agentToken` for the agent port only. This is the one
   security-weighted change and the thing the whole path depends on.
2. **`create` returns the sandbox domain** (§3.2) — surfaced in the response so
   the client can build data-plane URLs.
3. **Data-plane gRPC client in the CLI** (§3.3), building `{port}-{sandbox}`
   against the returned domain, falling back to the gateway path when no proxy
   domain is configured.
4. **Real-host e2e** (§4) — the `hack/` probe asserting exec + file round-trip
   through the proxy, including the fail-closed check (uninjected dial is denied).
5. **SDK** — add the gRPC data-plane client to the Python SDK (the `grpcio`
   dependency decision lands here).
6. **Docs** — once shipped, correct the README so "no longer relay through the
   control plane" is backed by the data-plane path being the default, and note
   the gateway path as the fallback.

Files this will touch: `internal/node/portforward.go` (inject the agent token for
port 10001 — the one security-weighted change), `internal/control/api/server.go`
(return the sandbox domain in the create response), `cli/cli.go` (proxy client),
`sdk/python/bean/__init__.py` (gRPC client), a new `hack/` probe, and the e2e/doc
updates. Note the shift from the earlier draft: the client no longer fetches or
holds the per-sandbox token, so there is no token endpoint and no secret
surfaced to bean-api — the plaintext stays noded-only. The proxy (`proxy.go`)
needs no changes; the forwarder (`portforward.go`) does, and that injection is
the crux of the whole design.
