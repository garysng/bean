//go:build linux

package image

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Lazy pull over ublk is refused, and the refusal says why.
//
// The two are incompatible for a structural reason, not a missing feature: lazy pull
// means the layer is a URL the daemon range-reads, and the reader in this process needs
// a file it can seek. A node started with both would accept placements and then fail
// every create against an image whose layers live only in the store. Refused before any
// layer work so the failure names the configuration rather than the image.
func TestPrepareUblkRefusesLazyPull(t *testing.T) {
	p := NewOverlaybdProvider(t.TempDir(), t.TempDir(), t.TempDir(), nil, nil, 2048)
	p.Ublk = true
	p.LazyPull = true

	_, err := p.prepareUblk(context.Background(), "sbx-1", "alpine:3.20", 2048, PrepareOptions{})
	if err == nil {
		t.Fatal("lazy pull with ublk was accepted, so every create against a remotely " +
			"stored image would fail later with no mention of the configuration")
	}
	if !strings.Contains(err.Error(), "lazy-pull") {
		t.Errorf("the refusal does not name the flag an operator has to change: %v", err)
	}
}

// Available() checks the transport the provider will actually use.
//
// The tcmu route needs the SCSI modules and the ublk route does not. Checking tcmu
// either way would refuse a node that can serve every create asked of it, and checking
// neither would accept a node that can serve none.
func TestOverlaybdAvailableChecksTheSelectedTransport(t *testing.T) {
	// A provider with no builder fails for that reason on whichever transport is
	// present, so the builder check is what both cases have in common. What differs is
	// which transport error can appear -- and a tcmu error must not appear when ublk is
	// selected.
	p := NewOverlaybdProvider(t.TempDir(), t.TempDir(), t.TempDir(), nil, nil, 2048)
	p.Ublk = true

	if err := p.Available(); err != nil {
		if strings.Contains(err.Error(), "tcmu") || strings.Contains(err.Error(), "target_core_user") {
			t.Errorf("a ublk-transport provider was refused for a tcmu reason: %v", err)
		}
	}
}

// A chain naming a layer that is not on this node is refused with the layer named.
//
// A resolved chain can reference a layer by digest alone, which the daemon would
// range-read. The reader here cannot, and the useful error says which layer and which
// image -- an operator's next step is to prewarm that image on this node.
func TestPrepareUblkNamesARemoteOnlyLayer(t *testing.T) {
	lowers := []obdLayer{
		{File: filepath.Join(t.TempDir(), "local.lsmt"), Digest: "sha256:aaa"},
		{Digest: "sha256:bbb"},
	}
	_, err := localLayerPaths(lowers, "alpine:3.20")
	if err == nil {
		t.Fatal("a chain with a remote-only layer was accepted, so the create would fail " +
			"later inside the layer reader with no mention of which layer")
	}
	for _, want := range []string{"sha256:bbb", "alpine:3.20"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q, so an operator cannot tell which "+
				"layer of which image to prewarm: %v", want, err)
		}
	}

	// A fully local chain passes through in order, since the stack's merge depends on it.
	local := []obdLayer{
		{File: "/a/base.lsmt", Digest: "sha256:1"},
		{File: "/a/top.lsmt", Digest: "sha256:2"},
	}
	paths, err := localLayerPaths(local, "alpine:3.20")
	if err != nil {
		t.Fatalf("a fully local chain was refused: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/a/base.lsmt" || paths[1] != "/a/top.lsmt" {
		t.Errorf("paths = %v, want the chain in its original order: the merge treats the "+
			"last as the newest layer", paths)
	}
}
