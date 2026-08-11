# Security

## Reporting a vulnerability

Report privately through [GitHub's advisory
form](https://github.com/garysng/bean/security/advisories/new) rather than a
public issue.

## What bean is and is not, today

bean runs untrusted code behind a hardware boundary: one Firecracker microVM per
sandbox, its own network namespace, its own `/30`, and a per-sandbox credential
for the in-sandbox agent whose plaintext lives only on the node. The VMM itself
runs as an unprivileged uid, in a per-sandbox cgroup, with its own pid, mount and
network namespaces.

Two gaps are known and deliberate, not oversights — treat them as part of the
threat model when you deploy:

- **No jailer chroot.** The VMM is not confined by a `chroot` and has no device
  allowlist. [docs/jailer.md](docs/jailer.md) covers what that would add and
  why it is not next. [#20](https://github.com/garysng/bean/issues/20)
- **No per-port access control.** Any port on a sandbox is reachable by anything
  that can reach bean-proxy, so do not give a sandbox a port it would not want
  its caller to see. [#50](https://github.com/garysng/bean/issues/50)

The container tier (gVisor/runc) is a weaker boundary than the microVM tier by
construction. For untrusted code, prefer `--runtime fc`.

[docs/security-and-startup.md](docs/security-and-startup.md) holds the full
threat model. [docs/status.md](docs/status.md) is authoritative on what is
actually built — a design doc describing a control is not evidence the control
exists.

## Scope

This is a working system and an incomplete platform. It has not been through an
external security review, and it should not be treated as a hardened
multi-tenant boundary yet. Reports about the two gaps above are welcome but
already known; reports of anything that escapes the boundaries described in
`status.md` are what matter most.
