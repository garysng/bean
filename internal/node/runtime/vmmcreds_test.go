package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseVMMCredsOffByDefault is the configuration every existing deployment
// runs. No uid, no gid, nothing changed anywhere.
func TestParseVMMCredsOffByDefault(t *testing.T) {
	c, err := parseVMMCreds(0, 0, 0)
	if err != nil {
		t.Fatalf("parseVMMCreds(0, 0): %v", err)
	}
	if c.Enabled() {
		t.Fatalf("an unconfigured node got credentials: %+v", c)
	}
	// Every method has to be safe on the nil, because the launch path calls them
	// unconditionally rather than branching.
	if err := c.chown("/nonexistent/path"); err != nil {
		t.Errorf("chown on nil creds: %v", err)
	}
	if err := c.chownTree("/nonexistent/dir"); err != nil {
		t.Errorf("chownTree on nil creds: %v", err)
	}
	if err := c.ensureTraversable("/a", "/a/b"); err != nil {
		t.Errorf("ensureTraversable on nil creds: %v", err)
	}
	if s := c.Summary(); !strings.Contains(s, "no privilege drop") {
		t.Errorf("summary does not state that no drop happens: %q", s)
	}
}

// TestParseVMMCredsRefusesRoot pins that a half-configured node is an error
// rather than a silent no-op. A uid with no gid would read as "hardening on"
// while leaving the sandbox directory group-owned by root.
func TestParseVMMCredsRefusesRoot(t *testing.T) {
	for _, tc := range []struct{ uid, gid int }{
		{1000, 0}, {0, 1000}, {-1, 1000}, {1000, -5},
	} {
		if _, err := parseVMMCreds(tc.uid, tc.gid, 0); err == nil {
			t.Errorf("parseVMMCreds(%d, %d) was accepted", tc.uid, tc.gid)
		}
	}
}

// TestVMMCredsCarryTheKVMGroup is the accessibility question that decides whether
// any sandbox boots. /dev/kvm is root:kvm 0660 and shared with everything else
// using KVM on the host, so the group it already has is how the dropped uid
// reaches it -- chowning a host-wide device to bean's uid would take it away from
// other users.
func TestVMMCredsCarryTheKVMGroup(t *testing.T) {
	c, err := parseVMMCreds(1000, 1000, 104)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Groups) != 2 || c.Groups[0] != 1000 || c.Groups[1] != 104 {
		t.Errorf("Groups = %v, want [1000 104]: with Credential set Go calls "+
			"setgroups with exactly this list, so a group left out is not held",
			c.Groups)
	}
	// The primary gid is always present, and not duplicated when it is already the
	// kvm group.
	same, err := parseVMMCreds(1000, 104, 104)
	if err != nil {
		t.Fatal(err)
	}
	if len(same.Groups) != 1 || same.Groups[0] != 104 {
		t.Errorf("Groups = %v, want [104]", same.Groups)
	}
	if c.NoFile != defaultVMMNoFile || c.NProc != defaultVMMNProc {
		t.Errorf("rlimits = %d/%d, want %d/%d", c.NoFile, c.NProc,
			defaultVMMNoFile, defaultVMMNProc)
	}
	if s := c.Summary(); !strings.Contains(s, "phase 2") {
		t.Errorf("summary does not state the limit of a uid drop without a mount "+
			"namespace: %q", s)
	}
}

// TestChownTreeDoesNotFollowSymlinks is the assertion that keeps the walk from
// taking ownership of things that are not this sandbox's.
//
// Two names in a sandbox directory are symlinks out of it: agent.ext4 to the
// node's shared agent disk, and rootfs.img to /dev/mapper. A walk that followed
// them would hand a shared asset, or a device node, to one sandbox's identity from
// a function whose stated job is that sandbox's own files.
//
// Ownership itself needs root, so what is asserted is the target's unchanged
// state, which holds either way and is what the bug would break.
func TestChownTreeDoesNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared-agent.ext4")
	if err := os.WriteFile(shared, []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(base, "sandbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(dir, "agent.ext4")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "console.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	c := &vmmCreds{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	if err := c.chownTree(dir); err != nil {
		t.Fatalf("chownTree: %v", err)
	}

	// The link must still be a link, and its target untouched. A walk that
	// followed it would have chowned the shared file, and on a root-run node that
	// is the bug: every other sandbox on the host reads that same inode.
	st, err := os.Lstat(filepath.Join(dir, "agent.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Error("the agent disk link was replaced by the walk")
	}
	after, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != before.Mode() {
		t.Errorf("the shared agent disk changed mode from %v to %v: the walk "+
			"followed a symlink out of the sandbox directory", before.Mode(), after.Mode())
	}
}

// TestEnsureTraversableOpensThePathToTheSnapshotState covers the restore case.
//
// The snapshot cache is 0700 because noded was its only reader, and Firecracker
// opens the machine state file itself by absolute path. A dropped uid that cannot
// traverse to it fails the load. This is separate from the memory image, which
// Firecracker never opens at all: noded mmaps it and serves faults over the UFFD
// socket, which is what makes fork cheap.
func TestEnsureTraversableOpensThePathToTheSnapshotState(t *testing.T) {
	// Built at 0700 explicitly rather than using t.TempDir's own mode, which is
	// not 0700 on every platform. 0700 is what noded creates BaseDir as
	// (fc_tier_linux.go), and the point of the test is what happens to that.
	base := filepath.Join(t.TempDir(), "sandboxes")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(base, ".snapshots", "snap-1")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(cacheDir, "state")
	if err := os.WriteFile(state, []byte("machine state"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &vmmCreds{UID: 1000, GID: 1000}
	if err := c.ensureTraversable(base, state); err != nil {
		t.Fatalf("ensureTraversable: %v", err)
	}

	for _, dir := range []string{base, filepath.Join(base, ".snapshots"), cacheDir} {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o001 == 0 {
			t.Errorf("%s is %#o: the dropped uid cannot traverse to the machine "+
				"state, so the restore fails with a permission error", dir, st.Mode().Perm())
		}
		// Read is deliberately not granted: the uid has no reason to list the
		// shared cache, and o+x without o+r allows only "open a path I was told".
		if st.Mode().Perm()&0o004 != 0 {
			t.Errorf("%s is %#o: the cache was made listable, which the VMM does not "+
				"need", dir, st.Mode().Perm())
		}
	}

	st, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o004 == 0 {
		t.Errorf("the machine state is %#o and unreadable by the VMM uid", st.Mode().Perm())
	}

	// A path outside the base directory is refused rather than chmodded: this
	// widens permissions, so the one thing it must not do is walk somewhere it was
	// not pointed.
	if err := c.ensureTraversable(base, filepath.Join(base, "..", "elsewhere")); err == nil {
		t.Error("ensureTraversable accepted a path outside the base directory")
	}
}

// TestCheckSharedAssetsCatchesUnreadableNodeAssets is the startup check. Each of
// these fails every create on the node and none is diagnosable from the symptom:
// an unreadable kernel is a guest that does not boot, an unreadable agent disk is
// a guest that boots with no init.
//
// The check is by mode rather than by opening the file, because noded is root and
// an open succeeds whatever the mode says -- that false pass is the point.
func TestCheckSharedAssetsCatchesUnreadableNodeAssets(t *testing.T) {
	dir := t.TempDir()
	readable := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(readable, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(dir, "agent.ext4")
	if err := os.WriteFile(private, []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}

	if bad := checkSharedAssets(readable); len(bad) != 0 {
		t.Errorf("a world-readable asset was reported: %v", bad)
	}
	bad := checkSharedAssets(readable, private)
	if len(bad) != 1 || !strings.Contains(bad[0], "agent.ext4") {
		t.Errorf("checkSharedAssets = %v, want the unreadable agent disk named", bad)
	}
	if missing := checkSharedAssets(filepath.Join(dir, "absent")); len(missing) != 1 {
		t.Errorf("a missing asset was not reported: %v", missing)
	}
	// An empty path is the "not configured on this node" case and must not be
	// reported: AgentDiskPath is optional in FCTierConfig.
	if none := checkSharedAssets(""); len(none) != 0 {
		t.Errorf("an unset path was reported: %v", none)
	}
}
