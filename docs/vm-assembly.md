# microVM Assembly: from block device to a connectable agent

> 中文版:[zh/vm-assembly.md](zh/vm-assembly.md)

> The status-marker convention is defined in [architecture.md](architecture.md) §0.
> Implementation: `internal/node/runtime/fc_linux.go` (assembly), `internal/node/image/devmapper_linux.go` (the block device).

One create is **952ms** (image already cached): `runtime_create` 234ms + `agent_ready` 770ms.
This document covers what happens inside those 234ms, and **which steps' ordering cannot be
moved**.

The latter is the main reason this document exists — there are two ordering constraints, and
getting either wrong produces **silently wrong behaviour** rather than an error.

## 1. The full order ✅

```
① image.Prepare        assemble /dev/mapper/bean-<id> (shared base + per-sandbox CoW)
                       on restore: the CoW must be backfilled within this step ← ordering constraint A
② os.Symlink           link the agent disk into the sandbox directory
③ exec firecracker     cwd = the sandbox directory ← the premise for relative paths
④ waitAPIReady         poll for the API socket to appear
⑤ PUT /machine-config  vCPU / memory / track_dirty_pages
⑥ PUT /cpu-config      CPU feature mask ← ordering constraint B (must precede ⑨)
⑦ PUT /boot-source     kernel + cmdline
⑧ PUT /drives/agent    the agent disk, root device
   PUT /drives/rootfs   the user image, second disk
   PUT /vsock           CID 3, a relative UDS path
⑨ PUT /actions         InstanceStart
```

restore takes the same ①–④ and then swaps in a single `PUT /snapshot/load` (with `ResumeVM`) —
because the snapshot already contains the whole machine configuration. **That is also why not
one of ⑤–⑧ may happen before the load**: Firecracker refuses to load a snapshot into an instance
that already has boot resources configured.

## 2. Ordering constraint A: the CoW must be backfilled before device assembly ✅

dm-snapshot reads the exception table into kernel memory at the moment of `dmsetup create`, and
**never reads it back afterwards**.

So writing bytes into the CoW backend of an **already-activated** device leaves the kernel
unaware of those chunks, and the device keeps serving the base image.
`image.PrepareOptions.SeedWritable` exists precisely to insert at the right moment:

```go
createSparse(cowPath, sizeMiB)
opts.SeedWritable(cowPath)      // ← the backfill goes here
attachLoop(cowPath, false)
dmsetup create ...              // ← the exception table is fixed at this instant
```

**The consequence of getting it wrong**: completely silent on a full snapshot. An immediate read
after restore hits the page cache the memory snapshot brought back, and after `drop_caches` the
same file reads back as all zeroes, while `ls` still shows the correct size, with no EIO and
nothing in dmesg. The metadata lives in the memory image and the data lives on the block device,
the two disagree, and ext4 has no reason to be suspicious. The full attribution is in
[decisions §3.0](decisions.md).

## 3. Ordering constraint B: the CPU mask must precede InstanceStart ✅

The guest **reads CPUID once during early boot and caches it** — glibc picks its string routines
from that (whether `memcpy` goes via AVX2 or SSE2). Changing the CPUID view afterwards is too
late: the guest is already using instructions it believes exist.

```go
// Masking has to happen before InstanceStart. A guest reads CPUID once
// during early boot and caches what it found ...
if cfg := cpuConfigFor(r.CPUTemplate); cfg != nil {
    vm.client.put(ctx, "/cpu-config", cfg)
}
```

`track_dirty_pages` is another example of the same class of constraint, but by a different
mechanism: it needs KVM accounting from the guest's very first instruction, and it is **not
stored in the snapshot**. So:

- It can only be **node configuration** (`--track-dirty-pages`), never a per-snapshot parameter
- A guest without it that requests a diff must **error out explicitly** rather than downgrade to full
- `EnableDiffSnaps` has to be passed again at restore, otherwise a sandbox restored from a
  snapshot can only produce full snapshots afterwards — and that is exactly the scenario that
  most needs increments

## 4. Why the agent disk is the root device ✅

```
/drives/agent   agent.ext4      IsRootDevice: true,  ReadOnly: true   → /dev/vda
/drives/rootfs  <cow device>    IsRootDevice: false                   → /dev/vdb
```

The kernel execs init from whichever device it mounted as root. Putting the agent there means
**the user image carries no obligations at all** — no embedded `beand`, no init system, no
modified entrypoint. Once up, the agent pivots to `/dev/vdb` itself:

```
init=/bean/beand -- --listen vsock:1024 --pivot /dev/vdb
```

**Order determines the naming**: Firecracker assigns `vda`/`vdb` in registration order, and
`--pivot /dev/vdb` is hard-coded, so the agent disk has to be registered first. Reversing the
registration order presents as the guest failing to mount, and under the default configuration
with no serial console there is **no output whatsoever** — which is the reason `--debug-console`
exists.

The agent disk is symlinked into the sandbox directory rather than copied: one inode, zero
copies, and it lets its drive path be relative (see §5).

## 5. Every path is relative ✅

Drive paths and the vsock UDS are all relative to the VMM's working directory, and that working
directory is set to the sandbox's own directory:

```go
cmd.Dir = vm.dir
```

**The reason is snapshot portability.** Firecracker **stores device paths and the vsock UDS path
in the machine state** and re-resolves them at load time — and it refuses to override the vsock
path at load.

So:
- Absolute paths → the restored VM goes looking for **the source sandbox's** files (and the source may already be destroyed)
- Relative paths → whichever sandbox directory the VMM starts in is the sandbox whose files it resolves to

This is the basis for "a snapshot can be restored into another sandbox, on another machine", and
it is delivered entirely by the single decision "cwd + relative paths", with no additional
path-rewriting logic.

## 6. Every item in the cmdline ✅

```
quiet reboot=k panic=-1 pci=off init=/bean/beand -- --listen vsock:1024 --pivot /dev/vdb
```

| Parameter | Purpose | Basis |
|---|---|---|
| `quiet` | do not attach the serial console | **Measured saving of 493ms** (1193ms → 700ms). 8250 UART writes are synchronous, and the kernel waits on hardware for every line it prints |
| `reboot=k` | use keyboard reset | FC has no ACPI, and this is the minimal usable reset method |
| `panic=-1` | do not reboot on panic | A crashed guest stays inspectable instead of entering a reboot loop |
| `pci=off` | skip PCI enumeration | FC has no PCI bus, so enumeration is pure waste |
| `init=/bean/beand` | the agent as PID 1 | See §4 |
| everything after `--` | arguments passed to beand | The kernel hands the part after `--` to init verbatim |

**The trade-off between `quiet` and debuggability**: the kernel still has the 8250 driver
compiled in — `--debug-console` just adds `console=ttyS0` back. A failed boot has no other
source of evidence, so that capability cannot be given up, but it should not cost 493ms on every
boot. This one is learned from e2b (in its `fc-kernels` config `CONFIG_SERIAL_8250=y` is on).

## 7. Why vsock can use constants ✅

```go
const agentVsockPort = 1024
const guestCID = 3
```

Neither needs allocating: **every VM has its own vsock namespace**, so there is nothing to
collide with. CID 3 is the smallest value available to a guest (0–2 are reserved by the protocol).

The benefit of constants is that the guest's cmdline does not depend on host state — which makes
the cmdline identical before and after a snapshot, one fewer thing to line up at restore.

## 8. The dm-snapshot table ✅

```
0 <base_sectors> snapshot <base_loop> <cow_loop> P 8
```

- **`P`** = persistent. Exceptions are stored in the CoW's metadata area, so the device can be
  torn down and reassembled — which is the premise for a snapshot capturing the CoW layer and
  replaying it elsewhere. `N` (non-persistent) lives only in memory
- **`8`** = chunk size in sectors, i.e. 4 KiB. It is chosen for being small enough: a single-block
  write copies 4 KiB rather than tens of KiB. The cost is more exception-table entries, but the
  measured cost is only 44 KiB per sandbox, which is not at the scale where a trade-off is needed

**The base is shared**: one read-only loop device serves every sandbox on the node using that
image. That is where "44 KiB per sandbox" comes from — compare `FileProvider`'s full copy per
sandbox.

The reference count lives in process memory, so after a restart the existing loop device has to
be **taken over** rather than newly created (otherwise one leaks per restart; fixed, see GitHub #16).

## 9. cleanup: registered in order, executed in reverse ✅

```go
var cleanup []func()
defer func() {
    if err == nil { return }
    for i := len(cleanup) - 1; i >= 0; i-- { cleanup[i]() }
}()
```

Every step pushes its own undo onto the stack as soon as it succeeds, and on failure they run in
reverse. **Reverse order is mandatory** — the dm mapping holds the loop device and the loop
device holds the file, so the mapping has to come down before the detach, and the detach before
the file is deleted. With the order right, a failed create leaves no VMM process, no device and
no files.

Why this matters: **a leaked microVM occupies memory the scheduler believes is free**. An orphan
FC process does not go away by itself, and the scheduler only looks at the committed-quantity
ledger, so a leak presents as "the node looks like it has capacity when it does not".

## 10. Why waitAPIReady is necessary ✅

There is a window between Firecracker starting and it creating the API socket, and a request sent
during that window gets "connection refused" — **an error that is hard to tell apart from
"misconfiguration"**.

So it polls for the socket to appear (5ms interval, 5s ceiling) rather than firing the first
request straight away. 5ms because this wait is usually on the order of tens of milliseconds, and
a fixed sleep would either waste time or be unreliable.

## 11. What does not exist yet 📐

Absent from the assembly path:

- **A NIC**. There is no network device in `fcMachineConfig`, and sandboxes have no network (GitHub #21)
- **balloon**. Memory reclamation cannot lean on it, so memory overcommit is one mechanism short (see noded-design §3.2)
- **jailer**. firecracker is exec'd directly, with no chroot / privilege drop / device allowlist (GitHub #20)
- **cgroup**. The FC process has no resource limit on the host

The first two are missing capabilities and the last two are missing defence in depth — the
hardware virtualisation boundary is still there, but the consequence of an FC/KVM vulnerability
is host root rather than a low-privilege user inside a chroot.
