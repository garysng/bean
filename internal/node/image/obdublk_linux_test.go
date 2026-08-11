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

// Without lazy pull, a chain naming a layer this node does not hold is refused by name.
//
// A resolved chain can reference a layer by digest alone. With lazy pull that is read over
// range requests; without it there is nothing to open, and the useful error says which layer
// of which image -- an operator's next step is to prewarm that image here.
func TestLayerSourcesRefusesARemoteOnlyLayerWithoutLazyPull(t *testing.T) {
	p := NewOverlaybdProvider(t.TempDir(), t.TempDir(), t.TempDir(), nil, nil, 2048)
	lowers := []obdLayer{
		{File: filepath.Join(t.TempDir(), "local.lsmt"), Digest: "sha256:aaa"},
		{Digest: "sha256:bbb"},
	}
	_, err := p.layerSources(context.Background(), lowers, "alpine:3.20")
	if err == nil {
		t.Fatal("a chain with a remote-only layer was accepted with lazy pull off, so the " +
			"create would fail later inside the layer reader with no mention of which layer")
	}
	for _, want := range []string{"sha256:bbb", "alpine:3.20", "lazy-pull"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q, so an operator cannot tell what to do "+
				"about it: %v", want, err)
		}
	}
}

// With lazy pull on, the same chain yields a remote source rather than an error.
func TestLayerSourcesReadsRemoteLayersWhenLazyPullIsOn(t *testing.T) {
	p := NewOverlaybdProvider(t.TempDir(), t.TempDir(), t.TempDir(),
		NewRegistry(nil), nil, 2048)
	p.LazyPull = true

	lowers := []obdLayer{
		{File: "/a/base.lsmt", Digest: "sha256:local"},
		{Digest: "sha256:remote", Size: 4096, RepoBlobURL: "reg.example/test/img:latest"},
	}
	srcs, err := p.layerSources(context.Background(), lowers, "reg.example/test/img:latest")
	if err != nil {
		t.Fatalf("resolve a mixed chain with lazy pull on: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("got %d sources, want 2", len(srcs))
	}
	// Local wins where a file exists: it costs nothing per read, while a remote layer costs
	// a round trip per uncached chunk.
	if srcs[0].Path != "/a/base.lsmt" || srcs[0].Remote != nil {
		t.Errorf("the local layer was not read from its file: %+v", srcs[0])
	}
	if srcs[1].Remote == nil || srcs[1].Path != "" {
		t.Errorf("the remote layer did not become a range reader: %+v", srcs[1])
	}
	if srcs[1].RemoteSize != 4096 {
		t.Errorf("remote size = %d, want the manifest's 4096 -- taking it from the manifest "+
			"is what avoids a HEAD before the first read", srcs[1].RemoteSize)
	}
}

// A fully local chain passes through in order, since the stack's merge depends on it.
func TestLayerSourcesKeepsChainOrder(t *testing.T) {
	p := NewOverlaybdProvider(t.TempDir(), t.TempDir(), t.TempDir(), nil, nil, 2048)
	local := []obdLayer{
		{File: "/a/base.lsmt", Digest: "sha256:1"},
		{File: "/a/top.lsmt", Digest: "sha256:2"},
	}
	srcs, err := p.layerSources(context.Background(), local, "alpine:3.20")
	if err != nil {
		t.Fatalf("a fully local chain was refused: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("got %d sources, want 2", len(srcs))
	}
	paths := []string{srcs[0].Path, srcs[1].Path}
	if paths[0] != "/a/base.lsmt" || paths[1] != "/a/top.lsmt" {
		t.Errorf("paths = %v, want the chain in its original order: the merge treats the "+
			"last as the newest layer", paths)
	}
}
