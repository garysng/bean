// noded is the node daemon: it manages sandbox lifecycle on one node and
// exposes SandboxService over gRPC for the control plane / gateway.
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	runtime2 "runtime"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/garysng/bean/internal/beand"
	"github.com/garysng/bean/internal/control/s3"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node"
	"github.com/garysng/bean/internal/node/image"
	"github.com/garysng/bean/internal/node/network"
	"github.com/garysng/bean/internal/node/reclaim"
	"github.com/garysng/bean/internal/node/runtime"
	"github.com/garysng/bean/internal/obs"
)

var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:7443", "gRPC listen address")
	rtName := flag.String("runtime", "local", "runtime: local|fc")
	agentBin := flag.String("agent-bin", "beand", "path to beand binary (local runtime)")
	baseDir := flag.String("base-dir", "/var/lib/bean/sandboxes", "sandbox base directory")
	nodeToken := flag.String("node-token", os.Getenv("BEAN_NODE_TOKEN"),
		"shared token required from callers (empty = no auth, loopback dev only)")
	controlPlane := flag.String("control-plane", "",
		"NodeService address; set to register with a control plane (multi-node mode)")
	nodeID := flag.String("node-id", "", "node id (default: derived from listen address)")
	region := flag.String("region", "local", "region this node belongs to")
	bootstrapToken := flag.String("bootstrap-token", os.Getenv("BEAN_BOOTSTRAP_TOKEN"),
		"token presented when registering")
	advertise := flag.String("advertise", "", "address the control plane should dial (default: --listen)")
	cpuAlloc := flag.Float64("cpu", 4, "allocatable vCPU advertised to the scheduler")
	memAlloc := flag.Int64("memory-mib", 8192, "allocatable memory (MiB)")
	diskAlloc := flag.Int64("disk-mib", 102400, "allocatable sandbox disk (MiB)")
	labelsFlag := flag.String("labels", "", "comma-separated node labels, e.g. pool=nvme,zone=a")
	metricsAddr := flag.String("metrics", "", "HTTP address for /metrics (empty = disabled)")
	fcBin := flag.String("firecracker-bin", "firecracker", "Firecracker binary (fc runtime)")
	fcKernel := flag.String("kernel", "/var/lib/bean/assets/vmlinux",
		"guest kernel image (fc runtime)")
	fcAgentDisk := flag.String("agent-disk", "",
		"read-only image holding beand, attached to every microVM (fc runtime)")
	imageDir := flag.String("image-dir", "/var/lib/bean/images",
		"prepared base images (fc runtime)")
	defaultDiskMiB := flag.Int64("default-disk-mib", 2048,
		"sandbox rootfs size when the spec does not bound it (fc runtime)")
	buildkitAddr := flag.String("buildkit-addr", "",
		"buildkitd address enabling image builds on this node, e.g. unix:///run/bean/buildkitd.sock")
	buildctlBin := flag.String("buildctl-bin", "buildctl", "BuildKit client binary")
	guestDNS := flag.String("guest-dns", "",
		"resolver the in-guest agent writes into /etc/resolv.conf. Empty leaves "+
			"the user image's own file alone, which is what a node with no sandbox "+
			"networking wants: without egress a nameserver is unreachable anyway, so "+
			"rewriting the file would only replace one failure with a less obvious "+
			"one. This must be the upstream resolver the host forwards to, not a copy "+
			"of the host's /etc/resolv.conf: that commonly holds 127.0.0.53 "+
			"(systemd-resolved), and inside a guest loopback names the guest")
	guestSubnet := flag.String("guest-subnet", "",
		"the /30 every sandbox's guest sees, e.g. 172.31.0.0/30. Empty leaves "+
			"sandboxes with no network interface at all, which is what they had "+
			"before this existed. Setting it gives each sandbox its own namespace, "+
			"tap and egress, and requires --uplink. The same addresses are used in "+
			"every sandbox on purpose: a restored snapshot resumes with the IP it "+
			"was taken with, so a constant is what lets one checkpoint fan out. This "+
			"node refuses to start if the range is already routed here -- colliding "+
			"means another subsystem's NAT rules eat sandbox traffic, and that shows "+
			"up as a network that works only sometimes")
	uplink := flag.String("uplink", "",
		"host interface sandbox egress leaves by, e.g. eth0. Required with "+
			"--guest-subnet: it is what the MASQUERADE rule matches on, and there is "+
			"no safe default because guessing wrong produces a sandbox that resolves "+
			"and routes but reaches nothing")
	debugConsole := flag.Bool("debug-console", false,
		"attach guests to the serial console; costs ~500ms per boot (fc runtime)")
	cpuTemplate := flag.String("cpu-template", "none",
		"mask guest CPU features so memory snapshots survive a move between CPU "+
			"generations: none|portable (fc runtime)")
	overcommitCPU := flag.Float64("overcommit-cpu", 1.0,
		"multiply allocatable CPU by this factor; evaluation workloads are bursty, "+
			"so 1.0 leaves capacity idle. Oversubscribing CPU degrades gracefully "+
			"(the kernel time-slices)")
	overcommitMemory := flag.Float64("overcommit-memory", 1.0,
		"multiply allocatable memory by this factor. Unlike CPU this does not "+
			"degrade gracefully — being wrong means a killed process — so raise it "+
			"only with measurements, and only with --fc-cgroups on: without a cgroup "+
			"around the VMM there is nothing in the kernel enforcing fairness when "+
			"the host comes under pressure")
	fcCgroups := flag.Bool("fc-cgroups", false,
		"put each sandbox's VMM in a cgroup with a memory ceiling, CPU quota and "+
			"pid cap from its own spec (fc runtime). Off by default because the "+
			"memory headroom above the guest's declared RAM is not yet measured "+
			"against real workloads, and a ceiling set too low is not a slow "+
			"sandbox but a killed one. Turning it on is the prerequisite for "+
			"raising --overcommit-memory: without it the committed quantity is only "+
			"the scheduler's ledger. Requires cgroup v2 (Ubuntu 22.04+, Debian 11+ "+
			"and RHEL 9+ already are); a v1 node refuses to start rather than run "+
			"unlimited, because v1 cannot cap swap and so cannot stop a guest at its "+
			"ceiling instead of letting it thrash the host")
	maxCreatesFlag := flag.Int("max-creates", 0,
		"how many creates this node will have in flight at once; 0 derives it from "+
			"the host's core count. A create is mostly a guest kernel boot, which is "+
			"CPU-bound, so the derived value scales with cores -- the previous fixed "+
			"default of 16 left a 128-core host idle on most of its cores while "+
			"refusing work. Raising this does not raise throughput past what the CPU "+
			"can do; it stops the limit being the binding constraint before the CPU "+
			"is. Exhaustion makes the scheduler wait rather than refuse (see "+
			"--create-wait), so a value that is too low costs latency under burst "+
			"rather than failed creates")
	fcWarmSnapshots := flag.Bool("fc-warm-snapshots", false,
		"boot one guest per image during prewarm and checkpoint it, so later creates "+
			"of that image restore instead of booting (fc runtime). This is the "+
			"throughput lever: a boot costs about 5 CPU-seconds of host CPU and a "+
			"restore costs almost none, so a node's create rate is bounded by "+
			"cores/5 until the boot is removed rather than made faster. Off by "+
			"default because each warm snapshot costs roughly one guest's memory on "+
			"disk, per image per CPU generation: set --warm-snapshot-high-mib to "+
			"bound that, or a node with many prewarmed images will fill its disk. A "+
			"miss always boots, so enabling this cannot make a create fail that "+
			"would otherwise have worked")
	fcOverlaybd := flag.Bool("fc-overlaybd", false,
		"assemble rootfs devices from overlaybd layers instead of flattening each "+
			"image into its own ext4 (fc runtime). Layers are shared by digest, so a "+
			"set of images built on one base stores that base once rather than once "+
			"per image: measured at 3.1x less disk for a SWE-bench-shaped set, and it "+
			"also removes the repeated conversion of shared layers, which is CPU the "+
			"flattening path pays per image. Needs the TCMU kernel modules, a running "+
			"overlaybd-tcmu, and the overlaybd binaries; a node started with this and "+
			"missing any of them fails at startup rather than falling back, because a "+
			"silent fallback gives the cluster a node whose storage behaviour is not "+
			"what was asked for")
	fcOverlaybdLazyPull := flag.Bool("fc-overlaybd-lazy-pull", false,
		"read layers from the registry on demand instead of converting them locally "+
			"(needs --fc-overlaybd). This is what removes the cold-pull wait -- 19.6% "+
			"of one layer's bytes were enough to mount and read a file, against "+
			"minutes to download a large image in full. **It only works on images "+
			"whose blobs are already sealed overlaybd layers**: a standard OCI layer "+
			"is a gzipped tar with no block index to seek into, so an ordinary image "+
			"from a registry cannot be read this way and a create naming one is "+
			"refused. Producing such images needs a conversion-and-push step this "+
			"node does not do. The other cost is that every block read then depends "+
			"on the registry still being reachable and still serving that digest")
	fcOverlaybdBinDir := flag.String("fc-overlaybd-bin-dir", "/opt/overlaybd/bin",
		"directory holding the overlaybd binaries (overlaybd-create, -apply, "+
			"-commit). Empty resolves them on PATH")
	fcOverlaybdS3Endpoint := flag.String("fc-overlaybd-s3-endpoint",
		os.Getenv("BEAN_S3_ENDPOINT"),
		"S3-compatible endpoint this node publishes sealed overlaybd layers to (or "+
			"BEAN_S3_ENDPOINT). This is what makes --fc-overlaybd-lazy-pull work for "+
			"ordinary images: a layer is converted once, published under its digest, "+
			"and every later create reading the same store skips the conversion "+
			"entirely -- including on other nodes. Credentials come from "+
			"BEAN_S3_ACCESS_KEY and BEAN_S3_SECRET_KEY, never a flag, so the secret "+
			"does not appear in the process command line")
	fcOverlaybdS3Bucket := flag.String("fc-overlaybd-s3-bucket", "bean-obd-layers",
		"bucket holding published overlaybd layers")
	fcOverlaybdS3Region := flag.String("fc-overlaybd-s3-region", "us-east-1",
		"S3 region for the published-layer bucket")
	fcOverlaybdS3PathStyle := flag.Bool("fc-overlaybd-s3-path-style", true,
		"address the bucket as a path rather than a subdomain, which MinIO and most "+
			"self-hosted stores need")
	fcOverlaybdReadURL := flag.String("fc-overlaybd-read-url", "",
		"URL prefix the overlaybd daemon reads published layers from. Defaults to "+
			"--fc-overlaybd-s3-endpoint, and is separate because the daemon resolves "+
			"it rather than this process: a node may write through an internal "+
			"endpoint while the daemon needs one reachable from where it runs. A "+
			"wrong value produces a device whose reads fail with the cause only in "+
			"overlaybd's log")
	fcVMMUid := flag.Int("fc-vmm-uid", 0,
		"run the VMM as this uid instead of root (fc runtime). 0 leaves it as "+
			"noded's own identity, which is what it has always been. The uid needs "+
			"to be in the group owning /dev/kvm, and this node's kernel and agent "+
			"disk must be world-readable; both are checked at startup. Note this "+
			"drops privilege without confining what the process can see -- the host "+
			"filesystem stays visible to it, and narrowing that needs the mount "+
			"namespace work in GitHub #20 phase 2")
	fcVMMGid := flag.Int("fc-vmm-gid", 0,
		"primary gid for --fc-vmm-uid. Required with it: the sandbox directory and "+
			"its block device are chowned to both, and a gid of 0 would leave them "+
			"group-owned by root")
	trackDirtyPages := flag.Bool("track-dirty-pages", false,
		"log guest writes so checkpoints can capture only what changed; must be on "+
			"from boot, so a guest started without it can never produce an "+
			"incremental snapshot (fc runtime)")
	minFreeDiskMiB := flag.Int64("min-free-disk-mib", 0,
		"refuse new sandboxes while the sandbox filesystem has less than this much "+
			"free; 0 disables. A sandbox whose sparse layer cannot allocate gets EIO "+
			"and its filesystem becomes unrecoverable while writes still appear to "+
			"succeed, so refusing a create is much cheaper than admitting one")
	minFreeDiskPct := flag.Float64("min-free-disk-percent", 0,
		"same floor as --min-free-disk-mib but against total capacity; the larger of "+
			"the two applies, so a percentage travels between differently sized nodes")
	snapCacheHighMiB := flag.Int64("snapshot-cache-high-mib", 0,
		"reclaim unpacked snapshots once the cache reaches this size; 0 leaves it "+
			"unbounded, which grows by roughly one guest's memory per distinct "+
			"snapshot restored and is invisible to the scheduler (fc runtime)")
	snapCacheLowMiB := flag.Int64("snapshot-cache-low-mib", 0,
		"size a reclaim brings the snapshot cache down to; must be below the high "+
			"mark, so eviction runs as an occasional batch rather than on every "+
			"restore past the trigger. 0 derives 80% of the high mark (fc runtime)")
	warmHighMiB := flag.Int64("warm-snapshot-high-mib", 0,
		"evict warm snapshots once they reach this size; 0 leaves them unbounded, "+
			"which grows by roughly one guest's memory per image per CPU generation "+
			"and is invisible to the scheduler. Deliberately a separate budget from "+
			"--snapshot-cache-high-mib: a cache entry can be re-unpacked from its "+
			"blob, while a warm snapshot can only be rebuilt by booting a guest, so a "+
			"burst of restores must not evict what makes creates cheap (fc runtime)")
	warmLowMiB := flag.Int64("warm-snapshot-low-mib", 0,
		"size an eviction brings the warm snapshots down to; must be below the high "+
			"mark. 0 derives 80% of it. Eviction is least-recently-restored, not "+
			"oldest: a warm snapshot is written once and read for weeks, so age since "+
			"creation says nothing about whether it earns its space (fc runtime)")
	logFormat := flag.String("log-format", "text", "log format: text|json")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	otlpEndpoint := flag.String("otlp-endpoint", os.Getenv("BEAN_OTLP_ENDPOINT"),
		"OTLP/gRPC collector for traces, e.g. localhost:4317 (empty = tracing off)")
	flag.Parse()

	logging.Setup(*logFormat, *logLevel)

	shutdownTracing, err := obs.SetupTracing(context.Background(), obs.TracingConfig{
		Endpoint: *otlpEndpoint, Service: "noded", Version: version, Insecure: true,
	})
	if err != nil {
		log.Fatalf("tracing: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(ctx); err != nil {
			slog.Warn("flushing traces", logging.KeyError, err)
		}
	}()

	if *nodeToken == "" && !isLoopback(*listen) {
		log.Fatalf("refusing to listen on %s without --node-token (or BEAN_NODE_TOKEN)", *listen)
	}

	// A misspelled template must stop the node rather than fall back to none:
	// the fallback silently produces snapshots bound to this host's CPU, and
	// nothing surfaces that until a restore elsewhere misbehaves.
	overcommit := node.Overcommit{CPU: *overcommitCPU, Memory: *overcommitMemory}
	if err := overcommit.Validate(); err != nil {
		log.Fatalf("--overcommit-*: %v", err)
	}
	if overcommit.Enabled() {
		// Stated at startup because it changes what the node admits, and a node
		// quietly accepting several times the work is hard to explain afterwards.
		slog.Warn("resource overcommit on",
			"cpu", *overcommitCPU, "memory", *overcommitMemory,
			"reportedCPU", overcommit.ApplyCPU(*cpuAlloc),
			"reportedMemoryMiB", overcommit.ApplyMemory(*memAlloc))
	}

	// A low mark defaulted from the high one rather than required alongside it:
	// the ratio is the part an operator has no basis to choose, while the size the
	// cache may reach is the part they do.
	snapCache := runtime.EvictionPolicy{HighBytes: *snapCacheHighMiB << 20, LowBytes: *snapCacheLowMiB << 20}
	if snapCache.HighBytes > 0 && snapCache.LowBytes == 0 {
		snapCache.LowBytes = snapCache.HighBytes / 5 * 4
	}
	if err := snapCache.Validate(); err != nil {
		log.Fatalf("--snapshot-cache-*: %v", err)
	}

	// Same derivation for the same reason, and a separate budget: see the flag's own
	// documentation for why the two must not share one.
	warmEvict := runtime.EvictionPolicy{HighBytes: *warmHighMiB << 20, LowBytes: *warmLowMiB << 20}
	if warmEvict.HighBytes > 0 && warmEvict.LowBytes == 0 {
		warmEvict.LowBytes = warmEvict.HighBytes / 5 * 4
	}
	if err := warmEvict.Validate(); err != nil {
		log.Fatalf("--warm-snapshot-*: %v", err)
	}
	// Refused rather than ignored: a bound set on a node that does not warm anything
	// reads as "warm snapshots are bounded here", and an operator who set it that way
	// has a wrong belief about the node rather than a harmless unused flag.
	if warmEvict.Enabled() && !*fcWarmSnapshots {
		log.Fatal("--warm-snapshot-high-mib is set but --fc-warm-snapshots is not: " +
			"there is nothing to bound, and leaving this accepted would read as a " +
			"node whose warm snapshots are bounded when it has none")
	}

	// Same reasoning: lazy pull is a property of the overlaybd path, so accepting it
	// on a node that flattens images would read as a node that reads layers on
	// demand when in fact it downloads every one in full.
	if *fcOverlaybdLazyPull && !*fcOverlaybd {
		log.Fatal("--fc-overlaybd-lazy-pull is set but --fc-overlaybd is not: " +
			"lazy pull is how the overlaybd path fetches layers, and the flattening " +
			"path has no equivalent, so this would read as a node that reads layers " +
			"on demand when it downloads each one in full")
	}

	tmpl, err := runtime.ParseCPUTemplate(*cpuTemplate)
	if err != nil {
		log.Fatalf("--cpu-template: %v", err)
	}

	// Derived from the host's cores, not from --cpu: the latter has already been
	// multiplied by --overcommit-cpu, and folding an oversubscription factor into a
	// physical concurrency limit would let an operator raise one and silently change
	// the other.
	maxCreates := *maxCreatesFlag
	if maxCreates <= 0 {
		maxCreates = derivedMaxCreates(runtime2.NumCPU())
	}
	slog.Info("create concurrency", "maxCreates", maxCreates,
		"cores", runtime2.NumCPU(), "derived", *maxCreatesFlag <= 0)

	// Checked at startup rather than at the first create, and fatally. An
	// operator deriving this from the host's /etc/resolv.conf gets 127.0.0.53 on
	// any systemd-resolved machine, and a guest pointed at loopback resolves
	// nothing while its route, NAT and ping to a literal address all test clean.
	// That misconfiguration has to stop the node it was typed on, not travel into
	// every sandbox the node then admits.
	if *guestDNS != "" {
		if err := beand.ValidateResolver(*guestDNS); err != nil {
			log.Fatalf("--guest-dns: %v", err)
		}
	}

	// Sandbox networking, off unless a subnet is named. Built before the runtime so
	// a misconfiguration stops the node here rather than at the first create: every
	// failure below is one an operator can fix by editing a flag, and none of them
	// are worth discovering one sandbox at a time.
	var netProv node.Provisioner
	if *guestSubnet != "" {
		if *uplink == "" {
			log.Fatal("--guest-subnet needs --uplink: the host MASQUERADE rule has to " +
				"name the interface egress leaves by, and a wrong guess produces a " +
				"sandbox that routes but reaches nothing")
		}
		host := network.NewHost(*uplink)
		if host == nil {
			log.Fatalf("--guest-subnet is set but this build cannot create network "+
				"namespaces (%s); sandbox networking needs linux", runtime2.GOOS)
		}
		// docs/network.md section 2. Refusing here is the whole point: an overlapping
		// route means sandbox traffic is matched by whoever already owns the range --
		// Docker holds six /16s in 172.16/12 on these hosts -- and the symptom is
		// intermittent connectivity rather than an error anyone can attribute.
		if err := network.CheckSubnetFree(*guestSubnet, host); err != nil {
			log.Fatalf("--guest-subnet: %v", err)
		}
		prov, err := network.NewProvisioner(*guestSubnet, host)
		if err != nil {
			log.Fatalf("--guest-subnet: %v", err)
		}
		netProv = prov
		slog.Info("sandbox networking on", "guestSubnet", *guestSubnet, "uplink", *uplink)
	} else {
		// Stated because it is the difference between "pip install fails because of
		// a proxy" and "pip install fails because this node gives sandboxes no
		// interface at all", and only one of those is worth debugging in the guest.
		slog.Warn("no --guest-subnet set; sandboxes on this node have no network " +
			"interface, so anything needing egress will fail inside the guest")
	}

	// The CPU identity is reported so the control plane can refuse to restore a
	// memory snapshot onto a CPU its guest cannot run on. A node that cannot
	// read it still starts: the effect is that it will not be chosen for
	// restores, which is the safe direction to fail in.
	cpuVendor, cpuFamily, err := runtime.HostCPUIdentity()
	if err != nil {
		slog.Warn("cannot read host CPU identity; this node will not be "+
			"eligible for snapshot restores", logging.KeyError, err)
	}

	var rt runtime.Runtime
	switch *rtName {
	case "local":
		localRT := runtime.NewLocalRuntime(*agentBin, *baseDir)
		localRT.GuestDNS = *guestDNS
		rt = localRT
	case "fc":
		// Built before the tier so a misconfigured store fails at startup rather than
		// on the first create that needed it.
		obdBlobs, obdIndex, err := overlaybdBlobStore(*fcOverlaybdS3Endpoint, *fcOverlaybdS3Bucket,
			*fcOverlaybdReadURL, *fcOverlaybdS3Region, *fcOverlaybdS3PathStyle)
		if err != nil {
			log.Fatalf("--fc-overlaybd-s3-endpoint: %v", err)
		}
		fcRT, err := runtime.NewFCTier(runtime.FCTierConfig{
			FirecrackerBin:  *fcBin,
			KernelPath:      *fcKernel,
			AgentDiskPath:   *fcAgentDisk,
			BaseDir:         *baseDir,
			ImageDir:        *imageDir,
			DefaultDiskMiB:  *defaultDiskMiB,
			BuildkitAddr:    *buildkitAddr,
			BuildctlBin:     *buildctlBin,
			DebugConsole:    *debugConsole,
			CPUTemplate:     tmpl,
			TrackDirtyPages: *trackDirtyPages,
			SnapshotCache:   snapCache,
			GuestDNS:        *guestDNS,
			Cgroups:         *fcCgroups,
			WarmSnapshots:   *fcWarmSnapshots,
			WarmEviction:    warmEvict,
			VMMUid:          *fcVMMUid,
			VMMGid:          *fcVMMGid,

			Overlaybd:         *fcOverlaybd,
			OverlaybdLazyPull: *fcOverlaybdLazyPull,
			OverlaybdBinDir:   *fcOverlaybdBinDir,
			OverlaybdBlobs:    obdBlobs,
			OverlaybdIndex:    obdIndex,
		})
		if err != nil {
			log.Fatalf("fc runtime: %v", err)
		}
		rt = fcRT
	default:
		log.Fatalf("runtime %q not supported (want local or fc)", *rtName)
	}

	mgr := node.NewManager(rt)
	// Assigned before Close is deferred, so the shutdown sweep tears down the
	// namespaces of anything still running rather than leaving one per sandbox for
	// the next process to find.
	mgr.Net = netProv
	defer mgr.Close()

	// The guard watches the base directory because that is where the sparse
	// copy-on-write layers live, which is the space that actually runs out.
	mgr.Disk = node.DiskGuard{
		Path:           *baseDir,
		MinFreeBytes:   *minFreeDiskMiB << 20,
		MinFreePercent: *minFreeDiskPct,
	}
	if err := mgr.Disk.Validate(); err != nil {
		log.Fatalf("--min-free-disk-*: %v", err)
	}
	if !mgr.Disk.Enabled() {
		// Stated because the failure it guards against is unrecoverable and silent:
		// a guest whose layer cannot allocate keeps reporting successful writes.
		slog.Warn("no low-disk floor set; a full disk will destroy the " +
			"copy-on-write layer of any sandbox that writes to it")
	} else if stats, err := mgr.Disk.Stat(); err == nil {
		slog.Info("low-disk admission floor set",
			"minFreeMiB", *minFreeDiskMiB, "minFreePercent", *minFreeDiskPct,
			"currentFreeBytes", stats.FreeBytes, "totalBytes", stats.TotalBytes)
	}

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	unaryAuth, streamAuth := node.TokenAuth(*nodeToken)
	// Trace extraction runs before auth so a rejected call still appears in the
	// trace: "the node refused the token" is a diagnosis, and it is only
	// visible if the span exists.
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(obs.UnaryServerTrace("noded"), unaryAuth),
		grpc.ChainStreamInterceptor(obs.StreamServerTrace("noded"), streamAuth),
	)
	nodev1.RegisterSandboxServiceServer(srv, node.NewGRPCServer(mgr))

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")
		srv.GracefulStop()
	}()

	// Metrics endpoint: scraped locally, so no auth and no sandbox contents.
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			mgr.RefreshGauges()
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			if err := mgr.Metrics().WritePrometheus(w); err != nil {
				slog.Error("cannot write metrics", logging.KeyError, err)
			}
		})
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		metricsSrv := &http.Server{
			Addr:              *metricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server stopped", logging.KeyError, err)
			}
		}()
		slog.Info("metrics listening", "addr", *metricsAddr)
	}

	// Multi-node mode: dial out to the control plane, register, then keep
	// the heartbeat alive. Nodes need no inbound path for this.
	if *controlPlane != "" {
		id := *nodeID
		if id == "" {
			id = "node-" + strings.ReplaceAll(*listen, ":", "-")
		}
		adv := *advertise
		if adv == "" {
			adv = *listen
		}
		reg := node.NewRegistrar(mgr, *controlPlane, id, *region, *bootstrapToken,
			parseLabels(*labelsFlag), []string{rt.Name()},
			// Scaled here rather than in the scheduler: the right factor depends on
			// what this node is for, and the scheduler treats what it receives as
			// the final allocatable figure.
			&nodev1.NodeResources{
				CpuAllocatable:       overcommit.ApplyCPU(*cpuAlloc),
				MemoryAllocatableMib: overcommit.ApplyMemory(*memAlloc),
				DiskSandboxesMib:     *diskAlloc,
				CpuVendor:            cpuVendor,
				CpuFamily:            cpuFamily,
				CpuTemplate:          string(tmpl),
				MaxCreates:           int32(maxCreates),
			})
		reg.Advertise = adv

		// Host reconciliation is enabled only for the microVM tier, and only in
		// multi-node mode. It is deliberately not a standalone startup step: what
		// separates an orphan from a sandbox that outlived the last noded is the
		// control plane's expected set, so it can only run once that is in hand.
		// A single-node node has nobody to ask, and the local tier creates no
		// device-mapper mappings or loop devices to leak.
		if *rtName == "fc" {
			reg.ReclaimHost = reclaim.NewLinuxHost(*baseDir)
			reg.BaseDir = *baseDir
			reg.ImageDir = *imageDir
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			if err := reg.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("registrar stopped", logging.KeyError, err)
			}
		}()
		slog.Info("registering with control plane",
			"controlPlane", *controlPlane, logging.KeyNode, id, "advertise", adv)
	}

	slog.Info("noded listening", "version", version, "runtime", rt.Name(), "addr", *listen)
	if err := srv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

// parseLabels turns "k=v,k2=v2" into a map.
// overlaybdBlobStore builds the store sealed layers are published to, or nil when no
// endpoint is configured.
//
// Nil is the ordinary case and not an error: a node that converts layers locally needs
// no store, and lazy pull without one still works for images whose registry blobs are
// already sealed overlaybd layers. The tier logs a warning for that combination rather
// than refusing it.
//
// Credentials come from the environment only. A flag would put the secret key in the
// process command line, where every local user can read it -- the same reasoning the
// gateway applies to its own snapshot bucket.
func overlaybdBlobStore(endpoint, bucket, readURL, region string, pathStyle bool) (image.BlobStore, image.ImageIndex, error) {
	if endpoint == "" {
		return nil, nil, nil
	}
	client, err := s3.New(s3.Config{
		Endpoint:  endpoint,
		Region:    region,
		AccessKey: os.Getenv("BEAN_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("BEAN_S3_SECRET_KEY"),
		PathStyle: pathStyle,
	})
	if err != nil {
		return nil, nil, err
	}
	if readURL == "" {
		// Defaulted rather than required, because the two are the same whenever the
		// daemon and this process reach the store the same way -- which is the common
		// single-host case.
		readURL = endpoint
	}
	store, err := image.NewS3BlobStore(client, bucket, "blobs", readURL)
	if err != nil {
		return nil, nil, err
	}
	index, err := image.NewS3ImageIndex(client, bucket)
	if err != nil {
		return nil, nil, err
	}
	// Probed at startup because the failure is otherwise invisible until a create, and
	// then arrives as ENOENT from the kernel with the real reason only in overlaybd's
	// log. The daemon reads without credentials, so a bucket this process can write is
	// routinely one the daemon cannot read.
	//
	// A warning rather than fatal: the node still works, it just converts every layer
	// locally instead of reading published ones. Refusing to start would turn a
	// degraded optimisation into an outage.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.CheckReadable(ctx); err != nil {
		slog.Warn("overlaybd cannot read the blob store; layers will be converted locally "+
			"instead of read on demand", logging.KeyError, err)
	}
	return store, index, nil
}

func parseLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// isLoopback reports whether addr binds only to a loopback interface.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// derivedMaxCreates is how many creates a node with this many cores will run at
// once when an operator has not said.
//
// A create is dominated by a guest kernel boot, measured at roughly 0.62 CPU-seconds
// of host CPU on an AMD EPYC host. So concurrency that scales with cores is the
// right shape, and the question is only the divisor.
//
// Four per core, which is deliberately above one-boot-per-core: a boot is not
// CPU-saturated end to end -- it waits on disk for the rootfs and on the agent's
// vsock handshake -- so a core can carry more than one in flight without either
// finishing later than it would alone. It is a starting point rather than a measured
// optimum, and the measurement that would refine it is a throughput run across
// several concurrency levels on one host (GitHub #29).
//
// The floor exists because the derivation is meaningless on a small host: 4 on a
// single-core machine is already more than it can boot, and going below that would
// serialise creates for no gain. The previous behaviour was a fixed 16, which this
// keeps for a 4-core host and raises everywhere above it.
func derivedMaxCreates(cores int) int {
	const (
		perCore = 4
		floor   = 16
	)
	if n := cores * perCore; n > floor {
		return n
	}
	return floor
}
