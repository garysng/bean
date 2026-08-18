package node

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node/reclaim"
)

// LabelAdvertiseAddr carries a node's data-plane address through
// registration labels.
const LabelAdvertiseAddr = "bean.io/advertise-addr"

// LabelSandboxPortAddr carries the address of a node's Host-routed forwarding port.
//
// Separate from LabelAdvertiseAddr because they are different services on different
// ports: that one is the gRPC control interface the scheduler drives, this one is the
// HTTP path into a sandbox. bean-proxy needs this one, and reusing the other would
// point browser traffic at a gRPC listener.
//
// Absent when a node was started without --sandbox-port-listen, which is a node that
// cannot serve port exposure at all. A caller has to treat absence as "not available
// here" rather than assume a default port: a guessed port either refuses the
// connection or belongs to something else entirely.
const LabelSandboxPortAddr = "bean.io/sandbox-port-addr"

// Registrar keeps a node registered with the control plane: it registers
// once, then maintains the heartbeat stream and reconciles on restart.
// The node dials out, so no inbound path to the control plane is needed.
type Registrar struct {
	ControlPlane   string
	NodeID         string
	Region         string
	Labels         map[string]string
	BootstrapToken string
	Resources      *nodev1.NodeResources
	Runtimes       []string
	// Advertise is the address the control plane should dial for this
	// node's data plane. Empty means the control plane must already know.
	Advertise string
	// SandboxPortAddr is where this node serves Host-routed access into its
	// sandboxes. Empty when the node was started without that listener, which is a
	// node that cannot serve port exposure -- so it is advertised only when present
	// rather than defaulted, and a caller that finds it absent must say so.
	SandboxPortAddr string
	// ReclaimHost reclaims host resources a previous noded left behind. Nil
	// disables it, which is what the local runtime and every test that is not
	// about reconciliation want: there is nothing to reconcile without
	// device-mapper.
	ReclaimHost reclaim.Host
	// BaseDir and ImageDir bound what reconciliation may touch. See
	// internal/node/reclaim for why the boundary is a directory rather than a
	// list of resources.
	BaseDir  string
	ImageDir string
	// StatusInterval is how often the node's inventory is resent in full. Zero
	// takes the default. Exposed so a test does not have to wait a minute for the
	// periodic report it is asserting on.
	StatusInterval time.Duration

	mgr *Manager

	nodeToken string
	interval  time.Duration

	// reclaimed records that host reconciliation has run. It is a startup task,
	// not a periodic one: a reconnect to the control plane is not evidence that
	// anything on the host was orphaned, and re-running on every session would
	// mean racing the sandboxes this process has since created.
	reclaimed bool
}

func NewRegistrar(mgr *Manager, controlPlane, nodeID, region, bootstrapToken string,
	labels map[string]string, runtimes []string, res *nodev1.NodeResources) *Registrar {
	return &Registrar{
		ControlPlane:   controlPlane,
		NodeID:         nodeID,
		Region:         region,
		Labels:         labels,
		BootstrapToken: bootstrapToken,
		Resources:      res,
		Runtimes:       runtimes,
		mgr:            mgr,
		interval:       3 * time.Second,
	}
}

// Run registers and then heartbeats until ctx is cancelled, reconnecting
// with backoff. It returns only when ctx is done.
func (r *Registrar) Run(ctx context.Context) error {
	conn, err := grpc.NewClient(r.ControlPlane, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial control plane: %w", err)
	}
	defer conn.Close()
	client := nodev1.NewNodeServiceClient(conn)

	backoff := time.Second
	for ctx.Err() == nil {
		if err := r.session(ctx, client); err != nil && ctx.Err() == nil {
			slog.Warn("control-plane session ended", logging.KeyError, err, "retryIn", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
	return ctx.Err()
}

// session performs one register -> reconcile -> heartbeat cycle.
func (r *Registrar) session(ctx context.Context, client nodev1.NodeServiceClient) error {
	labels := r.Labels
	if r.Advertise != "" || r.SandboxPortAddr != "" {
		// Carry the addresses in labels so the control plane and the proxy can route
		// to this node without a separate discovery mechanism.
		labels = make(map[string]string, len(r.Labels)+2)
		for k, v := range r.Labels {
			labels[k] = v
		}
		if r.Advertise != "" {
			labels[LabelAdvertiseAddr] = r.Advertise
		}
		if r.SandboxPortAddr != "" {
			labels[LabelSandboxPortAddr] = r.SandboxPortAddr
		}
	}
	resp, err := client.Register(ctx, &nodev1.RegisterRequest{
		BootstrapToken: r.BootstrapToken,
		NodeId:         r.NodeID,
		Region:         r.Region,
		Labels:         labels,
		Capabilities:   &nodev1.NodeCapabilities{Runtimes: r.Runtimes},
		Resources:      r.Resources,
	})
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	r.nodeToken = resp.NodeToken
	if resp.HeartbeatIntervalSeconds > 0 {
		r.interval = time.Duration(resp.HeartbeatIntervalSeconds) * time.Second
	}
	slog.Info("registered with control plane", logging.KeyNode, r.NodeID, "region", r.Region)

	stream, err := client.Heartbeat(ctx)
	if err != nil {
		return fmt.Errorf("open heartbeat: %w", err)
	}

	// Reconciliation runs after the heartbeat stream is open, and on its own
	// goroutine, because it is unbounded in time while the lease is not.
	//
	// It used to run here, synchronously, before the stream existed. Measured on a
	// 128-core host: a burst left 109 orphaned device-mapper mappings, each held open
	// by a firecracker process, and `dmsetup remove --retry` spends 4.806 seconds on
	// each one before giving up -- strictly serially. That is 8.7 minutes before the
	// first heartbeat could be sent, against a 45-second lease. The control plane
	// declared the node LOST at 50 seconds while noded was healthy and working, and
	// every create it had in flight was marked lost with it.
	//
	// The ordering is the whole fix. Reconciliation is a cleanup task whose duration
	// depends on how much mess the previous process left; the lease is a liveness
	// signal that must not depend on anything of the sort.
	go func() {
		if err := r.reconcile(ctx, client); err != nil {
			slog.Error("reconcile failed", logging.KeyError, err)
		}
	}()

	// The node's inventory goes up on its own schedule and, importantly, on its
	// own goroutine. See UpdateNodeStatus in node.proto for why the two reports
	// are separate messages; this is why they are also separate execution.
	//
	// UpdateNodeStatus is a unary call that blocks until the control plane
	// answers. Sharing this goroutine with the heartbeat would mean a control
	// plane that is slow to answer -- a GC pause, a lock, a slow query -- stalls
	// the heartbeat too, and the node loses its lease and has its sandboxes
	// reclaimed. That is a lease failure caused by an inventory report, which is
	// exactly the coupling splitting them was meant to remove.
	statusCtx, stopStatus := context.WithCancel(ctx)
	defer stopStatus()
	go r.reportStatusLoop(statusCtx, client)

	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			// A heartbeat renews the lease and nothing more: only the identity the
			// control plane authenticates and stamps last_heartbeat against. What
			// the node holds and how full it is travel on UpdateNodeStatus, off the
			// lease path, so a slow status report cannot stall a renewal.
			if err := stream.Send(&nodev1.HeartbeatRequest{
				NodeId:    r.NodeID,
				NodeToken: r.nodeToken,
			}); err != nil {
				return fmt.Errorf("heartbeat send: %w", err)
			}
			if _, err := stream.Recv(); err != nil {
				return fmt.Errorf("heartbeat recv: %w", err)
			}
		}
	}
}

// statusInterval is how often the full inventory is resent.
//
// Deliberately much slower than the heartbeat: this is a floor that bounds how
// stale the control plane's view can get, not the mechanism for keeping it
// current.
func (r *Registrar) statusInterval() time.Duration {
	if r.StatusInterval > 0 {
		return r.StatusInterval
	}
	return 60 * time.Second
}

// reportStatusLoop sends the inventory until ctx is done.
//
// One report at a time, by construction: this loop is the only caller and it
// blocks on each one. A design that fired reports from a timer without waiting
// could stack them up behind a slow control plane, and the newest -- the only one
// whose contents are still true -- would be last in the queue.
func (r *Registrar) reportStatusLoop(ctx context.Context, client nodev1.NodeServiceClient) {
	// Sent before the first tick so a freshly registered node is placeable for
	// image-affinity purposes immediately rather than a minute later.
	r.reportStatus(ctx, client)

	t := time.NewTicker(r.statusInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Periodic floor: bounds staleness if a kick was dropped or nothing
			// has changed in a while.
			r.reportStatus(ctx, client)
		case <-r.mgr.StatusKick():
			// A lifecycle change (create/destroy/pause/resume) asked for a report
			// now, so the ops disk view reflects it in seconds, not at the next tick.
			r.reportStatus(ctx, client)
		}
	}
}

// reportStatus sends what this node holds, and swallows the error after logging.
//
// The node is the authority on its own caches, so it reports rather than the
// control plane inferring. Only categories that have something to say are set:
// an absent category means "unchanged", so a runtime with no image cache -- the
// local tier -- sends nothing rather than an empty map that would read as "this
// node has dropped every image".
func (r *Registrar) reportStatus(ctx context.Context, client nodev1.NodeServiceClient) {
	req := &nodev1.UpdateNodeStatusRequest{
		NodeId:    r.NodeID,
		NodeToken: r.nodeToken,
	}
	if cached := r.mgr.CachedImages(); cached != nil {
		images := make(map[string]*nodev1.CachedImage, len(cached))
		for ref, img := range cached {
			images[ref] = &nodev1.CachedImage{
				SizeBytes: img.SizeBytes,
				Digest:    img.Digest,
				Warm:      img.Warm,
			}
		}
		req.Images = &nodev1.ImageInventory{Images: images}
	}
	// Usage rides here rather than on the heartbeat: it is inventory the ops view
	// and the scheduler's soft load score read, not a lease signal, so it must not
	// share the renewal path. Disk is measured (statfs) rather than summed from
	// sandbox requests -- a sandbox's disk request is nominal, the sparse layer
	// behind a 20 GiB request holds kilobytes, so summing would overstate the node
	// by orders of magnitude. CPU and memory are the node's real utilisation, CPU
	// sampled as the delta since the last report (hence measured here, on the
	// steady status cadence). Always set, so unlike the image inventory there is no
	// "nothing to say" case: the report always carries a fresh usage figure.
	cpuPct, memPct := r.mgr.LoadSample()
	req.Usage = &nodev1.NodeUsage{
		DiskUsedMib:    r.mgr.DiskUsedMiB(),
		CpuUsedPercent: cpuPct,
		MemUsedPercent: memPct,
	}
	if _, err := client.UpdateNodeStatus(ctx, req); err != nil {
		slog.Warn("node status report failed; image affinity, warm-snapshot "+
			"lookups, and the disk-usage view use a stale view until the next one",
			logging.KeyNode, r.NodeID, logging.KeyError, err)
	}
}

// reconcile compares control-plane expectations against local state and
// destroys orphans. Missing sandboxes are reported by omission in the next
// heartbeat; the control plane decides whether to recreate them.
func (r *Registrar) reconcile(ctx context.Context, client nodev1.NodeServiceClient) error {
	resp, err := client.SyncState(ctx, &nodev1.SyncStateRequest{
		NodeId: r.NodeID, NodeToken: r.nodeToken,
	})
	if err != nil {
		return err
	}
	expected := map[string]bool{}
	for _, spec := range resp.Expected {
		expected[spec.SandboxId] = true
	}
	for _, st := range r.mgr.Statuses() {
		if !expected[st.SandboxId] {
			slog.Info("destroying orphan sandbox", logging.KeySandbox, st.SandboxId)
			if err := r.mgr.Destroy(ctx, st.SandboxId, true); err != nil {
				slog.Error("cannot destroy orphan sandbox", logging.KeySandbox, st.SandboxId, logging.KeyError, err)
			}
		}
	}

	// Host resources come after the sandbox pass, and only once. The destroys
	// above release what this process knows about, so anything still on the host
	// afterwards has no owner in memory — which is exactly the population
	// reconciliation is looking for. Running it before the destroys would find
	// those resources still mapped and correctly decline to touch them.
	r.reclaimHost(expected)
	return nil
}

// reclaimHost returns host resources left by a previous noded, once, using the
// control plane's expected set to tell an orphan from a sandbox that predates
// this process.
//
// The expected set is what makes this safe, so a failure to obtain it must not
// reach here: reconcile only calls this after SyncState has succeeded. A
// reconciliation pass run against an empty set because a lookup failed would
// classify every running sandbox on the node as garbage.
func (r *Registrar) reclaimHost(expected map[string]bool) {
	if r.ReclaimHost == nil || r.reclaimed {
		return
	}
	r.reclaimed = true
	rec := &reclaim.Reconciler{
		BaseDir:  r.BaseDir,
		ImageDir: r.ImageDir,
		Host:     r.ReclaimHost,
		Metrics:  r.mgr.Metrics(),
	}
	if _, err := rec.Run(expected); err != nil {
		// Not fatal. A node that cannot reconcile still runs sandboxes; it just
		// keeps holding whatever the previous process leaked, which is the
		// behaviour before this existed.
		slog.Error("host resource reconciliation failed", logging.KeyError, err)
	}
}
