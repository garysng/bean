# jailer: what it costs, what it breaks, and what to do first

> The status-marker convention is defined in [architecture.md](architecture.md) §0.
> Implementation: nothing yet. `grep -rn jailer --include='*.go'` returns 1 hit, a comment.
> Related: `internal/node/runtime/fc_linux.go` (startVMM, drive/vsock registration),
> `internal/node/runtime/uffd_linux.go` (page-fault handler),
> `internal/node/network/setup_linux.go` (netns creation), GitHub #20.

This document exists to answer one question before anyone writes code: **can jailer be adopted
without breaking snapshot portability?** The answer is yes, but not for the reason the current
code comments give, and not without changing three things that are load-bearing today.

The headline correction: the premise written into `startVMM` and
[vm-assembly.md](vm-assembly.md) §5 — that relative paths are *the only* way a snapshot moves
between sandboxes because Firecracker "refuses to override the vsock path on load" — **is no
longer true upstream**, and separately, **the relative-path scheme is not actually doing what the
comment claims** even today. Both are established below from source and API spec, not inference.

## 0. Status summary

| Item | Status | Note |
|---|---|---|
| jailer wired into noded | 📐 | No code |
| Host cgroup around the VMM | 📐 | No code. `overcommit.go:30` and `cmd/noded/main.go:99` both cite its absence as the reason memory overcommit stays at 1.0 |
| Privilege drop / device allowlist | 📐 | No code. The VMM is root in the host mount namespace |
| Relative-path snapshot portability | ⚠️ | Works, but two of the three "relative" paths are symlinks to absolute targets outside the sandbox dir (§3). That is invisible today and fatal under chroot |
| `vsock_override` on load | ✅ upstream (FC 1.16.0), 📐 here | Removes the constraint the current design is built around |

## 1. What jailer actually does 📐

Verified by reading `src/jailer/src/env.rs` and `src/jailer/src/chroot.rs` on
`firecracker-microvm/firecracker@main`. This is source, not documentation paraphrase.

**Chroot layout.** `<chroot-base-dir>/<exec-file-name>/<id>/root`, default base `/srv/jailer`
(`env.rs:181-190`). For bean that would be `/srv/jailer/firecracker/<sandbox-id>/root`.

**The binary is copied, not linked or bind-mounted.** `copy_exec_to_chroot` (`env.rs:490`)
does a real `fs::copy` into `<chroot_dir>/firecracker`. Upstream's stated reason is memory
isolation: a copy means the new process shares no page-cache-backed text with any other
Firecracker. The cost is one binary copy per sandbox create — a few MiB of write per create,
against a 234ms `runtime_create` budget. **Unmeasured here.** It must be measured before
adoption, because it lands directly in the create path.

**Ordering.** `run()` (`env.rs:647`) does: copy exec → `join_netns` → `setrlimit` → cgroup setup
→ open `/dev/null` if daemonizing → `chroot()` → create `/`, `/dev`, `/dev/net`, `/run` at 0700
→ `mknod` `/dev/kvm` (10:232), `/dev/net/tun` (10:200), `/dev/urandom` (1:9), and
`/dev/userfaultfd` if present on the host (minor discovered by parsing `/proc/misc`,
`env.rs:427`) → drop to `--uid`/`--gid` → exec `/firecracker`.

Two consequences of that order matter. **cgroups and netns are joined before the chroot**,
because they cannot be done after it. And **the privilege drop is last**, so everything the jail
needs must already be chowned to the target uid.

**The working directory becomes `/`.** This is the crux, and it is explicit in `chroot.rs`:
`unshare(CLONE_NEWNS)` → `mount(NULL, "/", MS_SLAVE|MS_REC)` → bind-mount the chroot dir over
itself → `set_current_dir(path)` → `mkdir old_root` → `pivot_root(".", "old_root")` →
**`chdir("/")`** → `umount2("old_root", MNT_DETACH)` → `rmdir`. The comment on the `chdir` reads
"pivot_root doesn't guarantee that we will be in `/` at this point, so switch to `/`
explicitly."

So `cmd.Dir = vm.dir` has no analogue under jailer. **The cwd is not selectable.** It is always
the jail root.

## 2. Does the relative-path property survive? ✅ (verified)

Yes — and this is the good news, for a reason that is easy to get backwards.

The relative-path scheme depends on the cwd being *the sandbox's own directory*, whatever that
is. Under jailer the cwd is *the jail root*, and **the jail root is per-sandbox** (`<id>` is in
the path). So `vsock.sock` resolves to `/srv/jailer/firecracker/<id>/root/vsock.sock` — this
sandbox's socket, not the source sandbox's. The indirection changes; the property holds.

Upstream states the requirement in exactly these terms
(`docs/snapshotting/snapshot-support.md`): host resources "need to be accessible at the same
**relative** paths to the new Firecracker process as they were to the original one." Jailer
satisfies that by construction, which is why upstream treats jailer as the normal way to run
snapshots and treats the no-jailer case as the awkward one — see §5.

**Verified**: cwd is jail root (source); jail root is per-`<id>` (source); upstream requires
matching relative paths (docs). **Inferred**: that no other path Firecracker records is
absolute. §3 shows that inference is where the real problem is, and it is not about cwd at all.

## 3. The actual blocker: three paths that are not what they look like ⚠️

The chroot breaks bean not through cwd but through **reachability**. Under chroot, a path
resolves inside the jail or not at all. Three things currently escape the sandbox directory:

**(a) Both drives are symlinks to absolute targets.** `PathOnHost` is relative
(`fc_linux.go:477`, `484`) but the files those names refer to are not local:

- `agent.ext4` is `os.Symlink(r.AgentDiskPath, ...)` (`fc_linux.go:309`) → `/var/lib/bean/assets/agent.ext4`
- `rootfs.img` is `os.Symlink("/dev/mapper/bean-<id>", ...)` (`image/devmapper_linux.go:157-162`)

A relative *name* whose *target* is absolute resolves fine with no chroot and dangles inside one.
The dm device is worse than the agent disk: a device node cannot be symlinked into a jail at all,
it has to be `mknod`'d there with the right major:minor, or bind-mounted. Jailer creates
`/dev/kvm`, `/dev/net/tun`, `/dev/urandom` and `/dev/userfaultfd` and **nothing else** —
`FOLDER_HIERARCHY` is exactly `["/", "/dev", "/dev/net", "/run"]` (`env.rs:65`). Upstream is
explicit that guest resources are the operator's job: the user "must create hard links for (or
copy) any resources which will be provided to the VM via the API." **The per-sandbox dm device
node is bean's to place, and there is no code for it.**

**(b) The kernel path is absolute.** `KernelImagePath: r.KernelPath` (`fc_linux.go:461`) →
`/var/lib/bean/assets/vmlinux`. Unreachable in a jail. Needs a hardlink or bind mount in.

**(c) `SnapshotPath` is absolute.** `fc_lifecycle_linux.go:485` passes `entry.StatePath`, which
is `filepath.Join(dir, snapshotStateFile)` under the shared `.snapshots` cache
(`snapcache_linux.go:194`) — deliberately *beside* the sandboxes so entries outlive them
(`fc_linux.go:156-158`). The memory image is worse: it is not passed as a path at all, it is
`mmap`'d by noded and served over the UFFD socket, and **that sharing is the point** — one
page-cache copy across every restore of a snapshot (`uffd_linux.go:95-99`), which is what makes
fork cheap. A naive per-jail copy of the memory image would destroy the economics of
`internal/control/api/fork.go`.

So the honest statement is: **snapshot portability survives jailing; asset reachability does
not.** The comment in `startVMM` names the wrong risk.

## 4. UFFD: the socket survives, the sharing needs care ⚠️

`BackendPath: uffdSockName` is relative (`fc_lifecycle_linux.go:489`) and the handler binds
`vm.dir/uffd.sock` (`fc_linux.go:146`). Under jailer the resolution rule is the same as for
vsock, so the *name* works — but noded binds the socket and Firecracker connects to it, and the
two now disagree about what `/` means. noded must bind at
`/srv/jailer/firecracker/<id>/root/uffd.sock`, i.e. the jail root as seen from outside, and
chown it to the jail uid before the privilege drop. Firecracker then reaches it as
`./uffd.sock`. **Direction matters and helps here**: the connect goes inward, and a unix socket
is reached by path at connect time, so nothing needs to escape the jail.

Two real hazards, both inferred and both cheap to check on a KVM host:

- The socket must exist *before* the load (`uffd_linux.go:74-77`) and must be writable by the
  dropped uid, not by root. Getting the chown wrong fails as a hang, not an error: Firecracker
  blocks forever on a fault nobody answers — which is why `uffdHandler.failed` exists.
- Firecracker passes the userfault fd over `SCM_RIGHTS` (`uffd_linux.go:182-190`). fd passing is
  namespace-agnostic, so this should be unaffected by the chroot. **Inferred**, not tested.
  Note jailer bothers to `mknod /dev/userfaultfd` — relevant only to who calls
  `userfaultfd(2)`; here Firecracker does, inside the jail.

## 5. jailer and one-netns-per-sandbox compose ✅ (verified)

They compose cleanly, and this is the one place where no redesign is needed.

`Env::join_netns` (`env.rs:651`) opens the path given to `--netns`, calls
`setns(fd, CLONE_NEWNET)`, closes it. **jailer joins an existing namespace; it never creates
one.** bean already creates namespaces itself (`ip netns add bean-<n>`,
`setup_linux.go:100-126`), and `ip netns add` puts a handle at `/var/run/netns/bean-<n>`, which
is exactly what `--netns` wants. Neither has to give.

Better: this closes a gap in the current code. The runtime **never enters the netns today** —
`grep -rn netns` over `internal/node/runtime/` returns nothing outside the network package.
Adopting jailer is what would actually attach the VMM to its namespace, so #20 and #21 are
complementary rather than competing.

The `network.md` §4 worry about namespace organisation having to change does not materialise:
tap naming is unaffected, `beantap0` is still right in the new namespace, and `network_overrides`
stays an unused escape hatch. Note the netns join happens **before** the chroot, so the tap
device is looked up in the joined namespace and `/dev/net/tun` is `mknod`'d after — that is why
jailer creates it, and upstream says so: required "to use multiple TAP interfaces when running
jailed."

## 6. `vsock_override` invalidates the stated premise ✅ (verified)

FC **1.16.0** added `vsock_override` to `SnapshotLoadParams` (CHANGELOG, PR #5323; present in
`src/firecracker/swagger/firecracker.yaml:1716`, an object with a `uds_path` key). The API
description: "Overrides the vsock device's UDS path on snapshot restore. This is useful for
restoring a snapshot with a different socket path than the one used when the snapshot was
created."

`docs/vsock.md` scopes the motivation to precisely bean's situation: "In certain environments
where the jailer is **not** used, restoring snapshots with vsock devices may be difficult"
because the same UDS path "cannot be multiplexed." Caveat worth recording: the override is a
**prefix** — "All connections on the restored VM will then be opened with `./v.sock.2` as a
prefix."

So the `startVMM` comment's "refuses to override it on load, so a relative path is the only way"
is **version-dependent and now false**. Two corrections follow:

1. The claim must be dated in the code comment, not stated absolutely.
2. **The pinned version is unknown and this is a documentation gap.** `hack/build-assets.sh:119`
   pins the *kernel* to the `firecracker-ci/v1.11` bucket, and nothing in the repo downloads or
   pins the Firecracker binary at all — `dev-fc-stack.sh:96` just points at
   `$ASSETS/firecracker` and assumes it is there. If the deployed binary is < 1.16.0,
   `vsock_override` does not exist. **Run on the KVM host: `/var/lib/bean/assets/firecracker
   --version`.** This cannot be established from a darwin checkout.

## 7. The alternative: cgroup + credential + device allowlist, no jailer 📐

Go can do a real subset of this directly, and it is worth being precise about which subset,
because §0.1 of [architecture.md](architecture.md) forbids "X covers Y so Z can wait" without an
itemised list.

Reachable with `SysProcAttr` plus writing cgroup files from noded:

| Control | Mechanism | Notes |
|---|---|---|
| cpu/memory/pids limits | write `cpu.max`, `memory.max`, `memory.swap.max=0`, `pids.max` in a cgroup v2 dir, then put the child's pid in `cgroup.procs` | This is the part `overcommit.go:30` is blocked on. **No path change, no snapshot risk** |
| Privilege drop | `SysProcAttr.Credential{Uid, Gid}` | Requires chowning the sandbox dir, the dm device node and `/dev/kvm` to that uid |
| No new privileges | `prctl(PR_SET_NO_NEW_PRIVS)` — needs a `fork/exec` hook or a tiny re-exec shim | Go's `SysProcAttr` has `NoNewPrivs` on Linux |
| netns | `setns` before exec, or keep `ip netns exec` | Achievable without jailer |

**What jailer gives that this does not** — the honest list, since this is the comparison that
decides sequencing:

1. **Mount-namespace confinement.** `unshare(CLONE_NEWNS)` + `pivot_root` + `umount old_root`.
   A compromised VMM sees only the jail. `SysProcAttr.Credential` leaves the whole host
   filesystem *visible*, merely not all of it writable. This is the substantive difference and
   the one A3 is really about: "an FC or KVM vulnerability then means host root rather than a
   low-privilege user inside a chroot." A uid drop alone gives "a low-privilege user with a full
   view of the host filesystem."
2. **A `/dev` containing only four device nodes.** The device allowlist is a property of the
   jail's `/dev`, not of a syscall. Without a mount namespace there is no way to present a
   narrowed `/dev` to that process alone.
3. **fd hygiene and environment wipe.** jailer closes every inherited fd except stdio (from
   `/proc/<pid>/fd`) and clears inherited env vars. Go's `exec` leaks neither by default if
   written carefully, but jailer does it unconditionally.
4. **`setrlimit` `fsize`/`no-file`** (`no-file` defaults to 2048). Reachable in Go, not currently
   done.
5. **Not sharing binary text with other VMMs** — the reason for the copy.

Conversely, **what the cgroup-only path gives that jailer does not**: no new path resolution
semantics, so no risk to §3's assets or to the shared memory image that makes fork cheap. Zero
snapshot risk.

## 8. Recommendation 📐

**Split #20. The cgroup half is independent of the jailer half, and it is the half currently
blocking something else.**

**Phase 1 — cgroup v2 + credential drop + rlimits, no chroot.** Delivers items 3, 4 and the
resource-fairness enforcement that `overcommit.go` and `cmd/noded/main.go:99` both name as the
prerequisite for raising memory overcommit above 1.0. Touches no path. Cannot break restore or
fork.

**Phase 2 — jailer, but only after the asset-reachability work exists**, which is the real
content of adopting it and is not written yet:

1. `firecracker --version` on the KVM host; decide whether `vsock_override` is even available.
2. Place the kernel in the jail (hardlink if same filesystem, else bind mount).
3. Place the agent disk in the jail (hardlink; it is shared and read-only, so this is cheap).
4. **`mknod` the per-sandbox dm device inside the jail** with the major:minor of
   `/dev/mapper/bean-<id>`, replacing the symlink. This has no prototype and is the piece most
   likely to surprise.
5. Decide how `.snapshots` reaches the jail *without* per-jail copies of the memory image, since
   the shared read-only `mmap` is what makes fork cheap. Probably a read-only bind mount of the
   snapshot cache dir. **Unverified.**
6. Measure the per-create binary copy against the 234ms budget.

**Conclusion on the original question**: jailer can be adopted without breaking snapshot
portability — the per-sandbox jail root preserves the relative-path property, and netns
composes. But **it cannot be adopted as-is**, because five host assets the VMM currently reaches
by absolute path or symlink are unreachable inside a jail, and one of them is a device node.
The security gap A3 names is real; the fastest honest progress against it is Phase 1, which
carries none of that risk.

## 9. What must be run on a Linux KVM host

None of the following can be established from a darwin checkout. Do not treat any of it as
settled.

```sh
# 1. Which FC is deployed — decides whether vsock_override exists at all.
/var/lib/bean/assets/firecracker --version

# 2. Does the dm device work inside a jail at all? The single riskiest unknown.
#    Compare major:minor inside and out.
stat -c '%t:%T' /dev/mapper/bean-<id>
#    then mknod it into the jail root and boot a sandbox from it.

# 3. Cost of the per-create binary copy, against a 234ms runtime_create.
#    Time jailer's own setup, with the sandbox dir on the real filesystem.

# 4. UFFD across the jail: bind the socket at the jail root as the dropped uid,
#    restore, and confirm faults are served rather than the load hanging.
#    uffdHandler.Faults() distinguishes "never faulted" from "never answered".

# 5. network.md §4's open question, unrelated to jailer but in the same restore path:
#    whether a restored guest needs an `ip neigh flush`.
```
