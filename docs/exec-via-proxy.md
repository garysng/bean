# exec / file transfer via the data plane, not the gateway

> Status: 📐 **design.** This proposes moving `exec` and file transfer off the
> bean-api control-plane relay and onto the bean-proxy data plane, so they reach
> the agent node-direct — matching what the README already claims. Authority
> order holds: code > `status.md` > `decisions.md` > design docs > this page.

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
reach the agent. So credential delivery is not optional polish — it is on the
critical path.

## 3. The design

Three parts. Credential delivery is the one with real security weight; the other
two are mechanical.

### 3.1 Credential delivery

The client dialing the proxy must present the per-sandbox token the agent
expects. Options, with the trade-off:

- **Return it at create time.** `POST /v1/sandboxes` includes the token (once) in
  its response; the client keeps it for the sandbox's life. Simplest, one round
  trip, but the token now lives in the client and in create logs unless handled
  carefully.
- **A dedicated fetch endpoint.** `GET /v1/sandboxes/{id}/agent-token` returns it
  on demand, gated by the same API-key auth as everything else on bean-api. Keeps
  it out of the create response, costs a round trip, and gives one place to audit
  and later revoke.

Either way the plaintext, which today never leaves noded, must be surfaced to
bean-api and then to the client. That is a deliberate widening of where the
secret lives and should be called out as such: the token stops being a
noded-only secret and becomes a client-held bearer credential for the data
plane. Recommended: the **fetch endpoint**, because it is auditable and does not
bloat every create response with a secret.

### 3.2 Client: a data-plane gRPC client

CLI and SDK today know only `BEAN_BASE_URL` → bean-api REST. Add:

- **`BEAN_PROXY_URL`** — the proxy address for data-plane calls.
- A **gRPC client** that dials the proxy with authority `10001-{sandbox}` and
  calls `AgentService/Exec` / `ReadFile` / `WriteFile` / `ListDir` / `DeleteFile`
  directly, presenting the per-sandbox token as gRPC metadata (the same key the
  agent reads).
- Fallback: if `BEAN_PROXY_URL` is unset, keep the current bean-api path, so this
  is additive and nothing breaks for a single-node/dev setup.

The Python SDK is stdlib-`urllib` only; a gRPC client means either a `grpcio`
dependency or an h2c/framed alternative. That cost is real and is part of "change
the SDK too."

### 3.3 Addressing

No proxy or forwarder protocol change — both already do h2c to port 10001. The
client constructs authority `10001-{sandbox}`; the proxy resolves the node via
`NodeAddrFor` (already implemented) and forwards; the forwarder dials
`GuestIP:10001` in the netns. This part is entirely reuse.

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
`guest-egress-probe.sh`) that, on a real microVM host: creates a sandbox, fetches
its token, execs through the proxy, asserts the output, round-trips a file
through the proxy, and asserts the bytes — then breaks the token and asserts the
agent denies. That last step matters: proving the auth check still fails closed on
the data-plane path is the whole security argument.

## 5. Staged plan

1. **Credential delivery** (§3.1) — the fetch endpoint on bean-api, token
   surfaced from noded. Nothing else can be tested without it.
2. **Data-plane gRPC client in the CLI** (§3.2) behind `BEAN_PROXY_URL`, falling
   back to the gateway path when unset.
3. **Real-host e2e** (§4) — the `hack/` probe asserting exec + file round-trip
   through the proxy, including the fail-closed check.
4. **SDK** — add the gRPC data-plane client to the Python SDK (the `grpcio`
   dependency decision lands here).
5. **Docs** — once shipped, correct the README so "no longer relay through the
   control plane" is backed by the data-plane path being the default, and note
   the gateway path as the fallback.

Files this will touch: `internal/control/api/server.go` (token endpoint),
`internal/node/manager.go` (surface the token), `cli/cli.go` (proxy client),
`sdk/python/bean/__init__.py` (gRPC client), a new `hack/` probe, and the e2e/doc
updates. The proxy (`proxy.go`) and forwarder (`portforward.go`) need no changes —
which is the point.
