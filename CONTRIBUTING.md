# Contributing

## Before you open a PR

```bash
make preflight
```

That is exactly what CI runs, in the same order: build, `gofmt`, `go vet`, the
ASCII check (including unpushed commit messages), then the coverage run. A
hand-assembled sweep is not the same check — the one step you skip locally is
the one that fails in CI.

## Language

Prose documentation may be written in any language: `docs/` holds the English
versions and `docs/zh/` the Chinese ones. **Everything else is ASCII** — code,
comments, scripts, configuration, commit messages and branch names.

The reason is not preference. Someone who cannot read Chinese should be able to
work on every file that is not documentation, and `git log` should stay readable
to everyone, which it stops being as soon as half the history needs translating.

`hack/check-ascii.sh` enforces it and runs as part of `make lint`. It rejects
only CJK, not everything non-ASCII: em-dashes, arrows and box-drawing characters
are used deliberately in comments and diagrams.

Editing a doc under `docs/` usually means editing its `docs/zh/` counterpart too.

## Testing

Most of the interesting behaviour needs a KVM host, root, and device-mapper, so
those tests **skip** rather than fail on a developer machine — `go test ./...`
stays green without proving much. Cross-compile and run on a real host for
anything touching the microVM tier:

```bash
GOOS=linux GOARCH=amd64 go test -c -o /tmp/img.test ./internal/node/image/
scp /tmp/img.test root@host:/tmp/ && ssh root@host /tmp/img.test
```

Two rules worth stating, because both were learned from bugs that passed a full
green suite:

**Verify through the real persistence layer.** When state exists in both memory
and on disk, a test that reads memory proves nothing. A silent
filesystem-corruption bug passed three layers of tests: unit tests checked the
tar round-trip (correct — the data *was* written), end-to-end tests read the
file from inside the guest (page-cache hit), and `dmsetup status` inspected the
wrong device. None read the restored block device. Snapshot assertions must
`drop_caches` first.

**Then break the fix and confirm the test fails.** For that bug every file-level
assertion was green against the broken implementation, so this was the only way
to know the new test was worth anything.

## Docs carry status markers

Design docs mark per-section delivery status (✅ implemented / ⚠️ partial /
📐 design only), because writing intent and reality the same way is what made
networking and jailer look shipped when they were not. The convention is defined
in [docs/architecture.md](docs/architecture.md) §0.

**Authority order: code > `status.md` > `decisions.md` > design docs.** If you
implement something a design doc describes, update that doc's marker in the same
PR — a stale 📐 is worse than no doc.

## Commits and PRs

- Keep the subject line under ~70 characters, in the imperative.
- ASCII only, in messages and branch names (see above).
- Say what changed and why; the traps in this codebase are mostly ordering
  constraints that look fine from every vantage point except the one that
  matters, so the *why* is the part worth writing down.
