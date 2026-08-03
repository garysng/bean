# Sandbox Networking: one netns per sandbox + a fixed guest address

> 中文版:[zh/network.md](zh/network.md)

> The status-marker convention is defined in [architecture.md](architecture.md) §0.
> Implementation: `internal/node/network/` (address pool, netns, NAT),
> `internal/node/runtime/fc_linux.go` (NIC registration and restore overrides).

Sandboxes have no network today, and SWE-bench-class tasks need `pip install` / `git clone` —
**this is the gap that makes bean unusable**, not an optimisation that can be sequenced later.

The core of this document is not "how to create a tap", which is three `ip` commands. The core
is **"a guest restored from a snapshot comes back with its original IP, and it may land on a
machine where someone is already using that IP"**. That single fact determines the whole design.

## 1. The constraint: a snapshot restore brings the original address back ✅ (confirmed)

A Firecracker snapshot contains the whole machine configuration, and the restored guest
**continues running with exactly the same network configuration, most importantly the same IP**
(upstream's `network-for-clones.md` says so explicitly). By default it will also go looking for
**the tap name that existed when the snapshot was created**.

So the fan-out scenario — deriving N instances from one prepared environment, which is exactly
eval's core usage — naturally produces **N guests with identical IP and MAC**. Three responses:

| Option | Does the guest side have to change | Cost |
|---|---|---|
| Allocate a different guest IP per sandbox | **Yes.** The guest has to renumber after restore | Needs an in-band channel to send the command (vsock), and the ARP cache has to time out |
| One netns per sandbox, fixed guest address | **No** | One netns + veth + two layers of NAT per sandbox |
| Bridge onto one shared segment | **Yes** | Same as above, and N identical MACs in one layer-2 domain are guaranteed to collide |

**The second one is chosen: zero changes on the guest side.** The reason is not convenience but
reliability — making the guest renumber adds one more "dispatch a command + wait for the guest
to execute it + wait for ARP to expire" to the restore path, and a failure at any of those steps
presents as "the sandbox came up but the network is intermittent", which is the hardest class of
fault to diagnose. Upstream's documentation also warns specifically that stale ARP entries can
have **the restored guest using the old link-layer address for as long as arp cache timeout
seconds**.

The fixed address has a further side effect that is good: the guest's cmdline and network
configuration are identical before and after the snapshot, which is one fewer thing to line up
at restore — the same logic as using the constant vsock CID 3
([vm-assembly.md](vm-assembly.md) §7).

### Two things confirmed by measurement

**Taps with the same name can coexist in different netns** (the premise this whole scheme rests on):

```
ip netns exec bean-probe-a ip tuntap add name beantap0 mode tap
ip netns exec bean-probe-b ip tuntap add name beantap0 mode tap
→ beantap0  DOWN  da:b8:ae:9e:9e:93     (netns a)
→ beantap0  DOWN  82:7d:d5:94:bb:cf     (netns b)
```

**Entering a netns does not change the working directory** (`hack/netns-cwd-probe.sh`):

```
outside netns: /tmp/bean-cwd-check
inside netns:  /tmp/bean-cwd-check
```

The second one matters more than it sounds: **snapshot portability depends entirely on
`cmd.Dir` + relative paths** (vm-assembly §5). If entering a netns changed the cwd, adding
networking would silently break snapshot restore. Verify first and then act, because that kind
of breakage raises no error.

## 2. Address layout 📐

```
Inside the guest (identical for every sandbox, so a snapshot can move anywhere)
  eth0    172.31.0.2/30
  default via 172.31.0.1

Inside the netns (one netns per sandbox, named bean-<sandboxID>)
  beantap0  172.31.0.1/30        ← the guest's gateway
  veth-in   10.<a>.<b>.2/30      ← unique per sandbox
  default via 10.<a>.<b>.1

Host
  veth-<idx>  10.<a>.<b>.1/30
  POSTROUTING -s 10.<a>.<b>.0/30 -o <uplink> -j MASQUERADE
```

### Why the guest segment uses 172.31.0.0/30

**The `172.16.0.0/12` that upstream's documentation recommends cannot be used** — on the host
Docker has already taken `172.17`, `172.18`, `172.19`, `172.20`, `172.21` and `172.22` (measured
with `ip route`), and this platform's design premise is coexisting on one machine with other
workloads. The consequence of colliding is sandbox traffic being eaten by Docker's MASQUERADE
rules, presenting as "the network works sometimes".

`172.31.0.0/30` sits at the tail of Docker's default allocation range, with the smallest
collision surface. **But it is still a choice that can collide**, so:
- On startup, check whether that segment is already claimed by a route, and if it is, **fail
  explicitly** rather than carrying on
- The segment is configurable (`--guest-subnet`), because no private segment is absolutely safe

A `/30` has only two usable addresses (gateway + guest), which is exactly what a point-to-point
link needs. A larger mask would just be waste, and it would make the invariant "one link per
sandbox" look breakable.

### Why the host side allocates by index

The addresses inside a netns can all be identical (that is the point of a netns), but **the host
end of the veth cannot** — those all live in the host's network namespace. So they are computed
from the sandbox's pool index:

```
10.<idx/64>.<idx%64*4>.1/30    host end
10.<idx/64>.<idx%64*4>.2/30    netns end
```

A `/30` steps by 4, so every `10.<a>.<b>.0/30` is an independent link. `10/8` holds
64 × 64 × 64 = 262144 of them, far beyond the number of sandboxes on one machine —
**the ceiling should not be set by the address space**, because that would turn into a strange
limit that needs explaining.

## 3. The address pool has to be rebuildable after noded restarts 📐

This one was taught to me by the loop device leak ([decisions.md](decisions.md), GitHub #16):
**the reference count lives in process memory, a restart loses it, and the thing on the host is
still there.** Back then the consequence was leaking one loop device per restart; here the
consequence is worse — reallocating an index that is already in use, and the veth addresses of
two sandboxes collide.

So the pool **maintains no authoritative state of its own** and instead rebuilds from the host:

```go
// On startup: list netns with the bean- prefix, parse out the index, mark it occupied
// On allocation: take the first free index
// On release: delete the netns (the veth goes with it), clear the NAT rules
```

**The host is the only authority**, the same principle as `Provider.Cached()` having the node
report what it holds ([image-pipeline.md](image-pipeline.md) §1). A ledger in the control plane
or in memory will diverge from reality either way.

After a restart it **takes over rather than cleans up**: an existing `bean-<id>` netns may be
serving a sandbox that was already running before the restart. Deciding what is an orphan means
comparing against the control plane's `SyncState` desired set, which belongs to host resource
reconciliation (GitHub #17) and is out of scope here.

## 4. restore: use network_overrides rather than changing the guest 📐

`fcNetOverride` (fc_api.go:151, defined and unused) exists for exactly this:

```json
"network_overrides": [{"iface_id": "eth0", "host_dev_name": "beantap0"}]
```

**But in our scheme it probably does not need to be used**: the tap name is `beantap0` in every
netns, so the name recorded in the snapshot happens to be right in the new netns. That is a
direct benefit of the "same-named taps coexisting across netns" property.

The reason to keep the field is that **it is the only escape hatch**: if some future scenario
has to change the tap name (say the way netns are organised changes once jailer is wired in,
GitHub #20), then without it the only option left is making the guest renumber.

**The ARP cache has to be cleared after restore**: upstream warns explicitly that a restored
guest may use the old link-layer address for as long as arp cache timeout seconds. The netns is
newly created, so the host-side neighbour table is clean; but the table inside the guest came
back with the snapshot. This one **needs verification on a real machine**, because it decides
whether an `ip neigh flush` has to be added in the agent.

## 5. Egress: two layers of MASQUERADE 📐

```
Inside the netns:  POSTROUTING -s 172.31.0.0/30 -o veth-in -j MASQUERADE
Host:              POSTROUTING -s 10.<a>.<b>.0/30 -o <uplink> -j MASQUERADE
```

Two layers because there are two address translations: guest segment → veth segment → host uplink.

**The rules have to be deletable precisely.** The host's `nat` table already carries Docker's six
MASQUERADE rules, and deleting one by mistake is catastrophic. So every rule matches precisely on
`-s <this sandbox's /30>`, and deletion uses `-D` with the same arguments — never `-F`, not ever.

**Ingress (DNAT) is not done.** What an eval task needs is egress (`pip install`, `git clone`),
not being reachable from outside. Exposing a port is the `bean-proxy` route
(architecture.md), going through the control plane rather than giving every sandbox a host port —
the latter would make port allocation another pool that has to be rebuilt after a restart.

## 6. DNS 📐

The guest's `/etc/resolv.conf` comes from the user's image, and what the image writes there could
be anything. Two approaches:

- **The agent writes resolv.conf**: after the pivot and before exec'ing the user command, write the host's resolver in
- **Have a dnsmasq inside the netns answer**: one more process to manage

The former is chosen. Writing a file is idempotent and inspectable, and **its failure mode is
explicit** (if the write fails, it errors), unlike an extra daemon which fails in the shape of
"resolution occasionally times out".

Which resolver address to write: **the host's `/etc/resolv.conf` cannot simply be copied** —
that may contain `127.0.0.53` (systemd-resolved), which from the guest's point of view is
itself. So take the host's upstream resolver, or have it specified by node configuration
(`--guest-dns`).

## 7. Phasing 📐

Networking is the one module where "half done is worse than not done": a sandbox whose network
works intermittently makes people doubt their own code rather than the platform. So three steps,
each requiring verification on a real machine:

1. **Egress for a single sandbox**. netns + tap + veth + two layers of NAT, built by hand,
   verifying that `ping 8.8.8.8` and `apk add curl` work inside the guest
2. **Address pool + concurrency**. N sandboxes with networking at once, not interfering with each
   other, and no duplicate allocation after noded restarts. Load testing has to cover the cleanup
   path for "creation failed halfway"
3. **Snapshot restore keeps the network**. A sandbox restored from a snapshot still has working
   networking — this step is the point of the whole document, and the most likely place to
   uncover the ARP problem

## 8. Not yet decided 📐

- **The network version of that guest-side ENOSPC class of problem**: when the tap is created but
  the guest never configured an address, what does it look like? It needs confirming whether it
  will, like the disk case, "look fine while not actually working"
- **MTU**: the uplink is 1500, and whether it needs lowering after two layers of encapsulation is untested
- **Bandwidth limiting**: Firecracker has a per-device rate limiter, unused.
  One sandbox saturating the uplink affects every sandbox on the machine
- **IPv6**: not considered at all
