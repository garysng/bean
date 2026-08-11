//go:build linux

package image

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Lazy pull over ublk is no longer refused: the route reads a remote layer itself.
//
// This test used to assert the opposite, on the reasoning that a lazily read layer is a URL
// while the reader needed a file. That was a missing backend, not an incompatibility -- every
// reader below the transport already took io.ReaderAt -- and it is implemented now, verified on
// hardware. Kept as the inverse assertion so the combination cannot be re-refused by accident.
//
// The failure a provider with no ublk device gives is about the kernel, which is what a
// machine without /dev/ublk-control should say; what must not appear is a refusal of the flag
// combination itself.
func TestPrepareUblkAcceptsLazyPull(t *testing.T) {
	p := NewOverlaybdProvider(t.TempDir(), t.TempDir(), t.TempDir(), NewRegistry(nil), nil, 2048)
	p.Ublk = true
	p.LazyPull = true

	_, err := p.prepareUblk(context.Background(), "sbx-1", "alpine:3.20", 2048, PrepareOptions{})
	// On a host with no ublk this fails for that reason, and on one with ublk it fails later
	// for want of a real image. Either is fine; what is not is a refusal naming the flag.
	if err != nil && strings.Contains(err.Error(), "lazy-pull cannot be served") {
		t.Errorf("lazy pull over ublk is refused, but it is implemented and verified: %v", err)
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
	_, err := p.layerSources(context.Background(), lowers, "alpine:3.20", layerSourceOpts{})
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
	srcs, err := p.layerSources(context.Background(), lowers, "reg.example/test/img:latest", layerSourceOpts{})
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
	srcs, err := p.layerSources(context.Background(), local, "alpine:3.20", layerSourceOpts{})
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

// A chain resolved from a snapshot reads its remote layers without lazy pull; a cold create's
// does not.
//
// The two look identical at this point -- a layer with no File -- and treating them the same is
// what broke restore. A cold create's image layer is remote by choice, because it could be
// converted from the registry, so refusing it respects the deployment decision lazy pull exists
// to offer. A restore's chain is remote by necessity: snapshotFSLowers only returns a remote
// reference after finding no local file, and a sealed snapshot layer has no registry blob behind
// it. Refusing that does not fall back to anything -- it just fails the restore.
func TestLayerSourcesHonoursAStoreOnlyChain(t *testing.T) {
	p := NewOverlaybdProvider(t.TempDir(), t.TempDir(), t.TempDir(), NewRegistry(nil), nil, 2048)
	p.LazyPull = false

	remoteOnly := []obdLayer{
		{Digest: "sha256:sealed", Size: 4096, RepoBlobURL: "http://store.example/bucket/blobs"},
	}

	// As a restore resolves it: no local file, and nowhere else to come from.
	srcs, err := p.layerSources(context.Background(), remoteOnly, "irrelevant",
		layerSourceOpts{storeOnly: true})
	if err != nil {
		t.Fatalf("a snapshot chain was refused with lazy pull off: %v -- a restore cannot then "+
			"read the layer its own checkpoint published", err)
	}
	if len(srcs) != 1 || srcs[0].Remote == nil {
		t.Errorf("the store-only layer did not become a range reader: %+v", srcs)
	}

	// As a cold create resolves it: still refused, so the deployment choice stands.
	_, err = p.layerSources(context.Background(), remoteOnly, "alpine:3.20", layerSourceOpts{})
	if err == nil {
		t.Error("a cold create's remote-only layer was accepted with lazy pull off, which " +
			"removes the choice lazy pull exists to offer")
	}
}
