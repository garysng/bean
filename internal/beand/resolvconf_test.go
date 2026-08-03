package beand

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateResolverRejectsLoopback is the case the whole file exists for. A
// host running systemd-resolved has 127.0.0.53 in its own resolv.conf, and
// copying that into a guest points the guest at itself.
func TestValidateResolverRejectsLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.53", "127.0.0.1", "::1"} {
		err := ValidateResolver(addr)
		if err == nil {
			t.Fatalf("ValidateResolver(%q) accepted a loopback address; a guest "+
				"pointed at loopback resolves nothing while every layer below DNS "+
				"tests clean", addr)
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("ValidateResolver(%q) error does not name loopback as the "+
				"problem: %v", addr, err)
		}
	}
}

func TestValidateResolverRejectsNonAddress(t *testing.T) {
	// A hostname here is ignored by libc rather than reported, so it has to be
	// refused where it is typed.
	for _, addr := range []string{"dns.example.com", "", "8.8.8.8:53", "not-an-ip"} {
		if err := ValidateResolver(addr); err == nil {
			t.Errorf("ValidateResolver(%q) accepted a value resolv.conf cannot use", addr)
		}
	}
}

func TestValidateResolverAcceptsRoutableAddress(t *testing.T) {
	for _, addr := range []string{"8.8.8.8", "172.31.0.1", "10.0.0.53", "2001:4860:4860::8888"} {
		if err := ValidateResolver(addr); err != nil {
			t.Errorf("ValidateResolver(%q): %v", addr, err)
		}
	}
}

func TestWriteResolvConfWritesNameserver(t *testing.T) {
	root := t.TempDir()
	if err := WriteResolvConf(root, "8.8.8.8"); err != nil {
		t.Fatalf("WriteResolvConf: %v", err)
	}
	got := readResolvConf(t, root)
	if got != "nameserver 8.8.8.8\n" {
		t.Errorf("resolv.conf = %q, want a single nameserver line", got)
	}
}

// TestWriteResolvConfRefusesLoopback checks the guard is on the write path too,
// not only on the flag: the agent is a separate process from the node that
// validated the value, so it cannot assume the check already happened.
func TestWriteResolvConfRefusesLoopback(t *testing.T) {
	root := t.TempDir()
	if err := WriteResolvConf(root, "127.0.0.53"); err == nil {
		t.Fatal("WriteResolvConf accepted a loopback resolver")
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "resolv.conf")); !os.IsNotExist(err) {
		t.Error("a rejected resolver still produced a file; a guest would boot " +
			"pointed at itself")
	}
}

// TestWriteResolvConfIsIdempotent covers a sandbox restored from a snapshot,
// whose filesystem already carries the file this writes.
func TestWriteResolvConfIsIdempotent(t *testing.T) {
	root := t.TempDir()
	for i := range 3 {
		if err := WriteResolvConf(root, "10.0.0.53"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	got := readResolvConf(t, root)
	if got != "nameserver 10.0.0.53\n" {
		t.Errorf("resolv.conf = %q after three writes; the content must not "+
			"accumulate", got)
	}
}

// TestWriteResolvConfReplacesImageContent covers the image that shipped a
// resolv.conf naming its build machine's resolver. Merging would keep an
// unreachable server that libc spends its full timeout on before trying ours.
func TestWriteResolvConfReplacesImageContent(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := "nameserver 192.168.65.7\nsearch corp.internal\n"
	if err := os.WriteFile(filepath.Join(etc, "resolv.conf"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteResolvConf(root, "8.8.4.4"); err != nil {
		t.Fatalf("WriteResolvConf: %v", err)
	}
	got := readResolvConf(t, root)
	if strings.Contains(got, "192.168.65.7") {
		t.Errorf("resolv.conf still names the image's resolver: %q", got)
	}
	if got != "nameserver 8.8.4.4\n" {
		t.Errorf("resolv.conf = %q, want only the configured resolver", got)
	}
}

// TestWriteResolvConfReplacesSymlink covers the systemd-resolved layout, where
// /etc/resolv.conf is a link into /run. In a guest /run is a tmpfs the agent
// just mounted, so writing through the link lands nowhere libc reads.
func TestWriteResolvConfReplacesSymlink(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(etc, "resolv.conf")
	if err := os.Symlink("../run/systemd/resolve/stub-resolv.conf", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := WriteResolvConf(root, "8.8.8.8"); err != nil {
		t.Fatalf("WriteResolvConf: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("/etc/resolv.conf is still a symlink; the write did not land " +
			"where libc looks")
	}
	if got := readResolvConf(t, root); got != "nameserver 8.8.8.8\n" {
		t.Errorf("resolv.conf = %q", got)
	}
}

// TestWriteResolvConfCreatesMissingEtc covers a minimal image with no /etc.
func TestWriteResolvConfCreatesMissingEtc(t *testing.T) {
	root := t.TempDir()
	if err := WriteResolvConf(root, "8.8.8.8"); err != nil {
		t.Fatalf("WriteResolvConf on an image with no /etc: %v", err)
	}
	if got := readResolvConf(t, root); got != "nameserver 8.8.8.8\n" {
		t.Errorf("resolv.conf = %q", got)
	}
}

func readResolvConf(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "etc", "resolv.conf"))
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	return string(b)
}
