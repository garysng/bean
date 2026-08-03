//go:build ignore

// Answers one question: how much storage does flattening OCI layers into one
// ext4 file per image actually cost, compared to a store that keeps the layers?
//
// internal/node/image/convert_linux.go unpacks every layer of an image, in
// order, into a single filesystem. That is what makes "hand the platform any
// image" work with no template to build. The cost is easy to state and easy to
// get wrong: layer sharing is gone. Two images sharing 90% of their layers cost
// two full ext4 files, not 1.1.
//
// The obvious assumption is that this is a rounding error, because the images an
// eval set uses are "basically the same image". It is the opposite: sameness is
// exactly what a flattening store cannot exploit. For a SWE-bench-shaped set --
// thousands of task images that are one common base plus a small patch -- the
// shared part is most of the bytes, and it is paid for once per image.
//
// That was an argument, not a number, and the decision it informs (whether to
// prioritise the overlaybd integration in GitHub #32, which keeps layer
// structure) should rest on evidence. So this counts.
//
// What it reports, and how each number is obtained:
//
//	layer sharing      MEASURED from manifests: distinct layer digests versus
//	                   total layer references, and the bytes of each. Registry
//	                   manifests state blob sizes, so no download is needed.
//	flattened content  MEASURED with --stream: every unique layer is fetched
//	                   once, gunzipped, and its tar entries are replayed into an
//	                   in-memory path map that applies overwrites and whiteouts
//	                   the same way extractTar does. The sum of surviving file
//	                   sizes is what the ext4 has to hold. Without --stream this
//	                   is ESTIMATED from compressed bytes and labelled as such.
//	provisioned size   COMPUTED by replicating Converter.sizeFor exactly. This
//	                   is the file's apparent size and what Cached() reports to
//	                   the scheduler.
//
// What it does NOT measure: ext4 metadata overhead. That needs mkfs.ext4 and
// root on Linux, and this tool deliberately runs unprivileged on any host, so
// the number is reported as a gap rather than guessed at.
//
// Usage:
//
//	go run hack/layer-amplification.go                       # manifests only
//	go run hack/layer-amplification.go --stream              # + real content
//	go run hack/layer-amplification.go --stream python:3.12-slim alpine:3.20
//
// Anonymous pulls of public images only. Reads registries; writes nothing
// outside its blob cache under TMPDIR.
package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/gob"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/garysng/bean/internal/node/image"
)

// The default set is the python-slim family plus the debian base they are built
// on. It is the right default for three reasons: the sharing is real (all three
// python tags reference the identical debian rootfs blob, not a rebuild of it),
// it has the shape of the target workload (one common base, a small distinct
// patch per image), and it is small enough that --stream finishes in a couple of
// minutes on a normal connection, so the measured path is actually usable.
//
// --set swebench swaps in real SWE-bench task images, which are the workload the
// storage question is actually about. They are ~1.2 GiB compressed each, so
// --stream on that set is a long download; the manifest-only run is cheap and
// gives the sharing ratio, which is the part that decides the question.
var imageSets = map[string][]string{
	"python": {
		"debian:bookworm-slim",
		"python:3.11-slim-bookworm",
		"python:3.12-slim-bookworm",
		"python:3.13-slim-bookworm",
	},
	"swebench": {
		"swebench/sweb.eval.x86_64.django_1776_django-11133:latest",
		"swebench/sweb.eval.x86_64.django_1776_django-11179:latest",
		"swebench/sweb.eval.x86_64.django_1776_django-12983:latest",
		"swebench/sweb.eval.x86_64.sympy_1776_sympy-20590:latest",
		"swebench/sweb.eval.x86_64.sympy_1776_sympy-24152:latest",
		"swebench/sweb.eval.x86_64.astropy_1776_astropy-12907:latest",
	},
}

// assumedCompressionRatio converts compressed layer bytes to unpacked bytes when
// --stream was not used. It is an assumption, not a measurement, and every line
// derived from it is labelled. gzip on distro and python-package content usually
// lands between 2.0x and 3.0x.
//
// It cancels out of the amplification ratio as long as it is applied to both
// sides, which is why the ratio survives without --stream even though the
// absolute byte figures do not. --stream exists to check that it really does
// cancel, i.e. that the shared layers and the per-image layers compress alike.
const assumedCompressionRatio = 2.5

// ext4BlockSize is used to round file sizes up to whole blocks, which is closer
// to what a filesystem allocates than the raw content sum. It still excludes
// inodes, directory blocks and the journal.
const ext4BlockSize = 4096

// defaultDiskMiB mirrors noded's --default-disk-mib, which becomes
// Converter.DefaultSizeMiB and floors every image's provisioned size.
const defaultDiskMiB = 2048

func main() {
	stream := flag.Bool("stream", false,
		"fetch and unpack every distinct layer to measure real content bytes")
	setName := flag.String("set", "python", "built-in image set: python or swebench")
	cacheDir := flag.String("cache", filepath.Join(os.TempDir(), "bean-layer-amp"),
		"where per-layer walk results are cached between runs")
	timeout := flag.Duration("timeout", 2*time.Hour, "overall deadline")
	flag.Parse()

	refs := flag.Args()
	source := fmt.Sprintf("built-in set %q", *setName)
	if len(refs) == 0 {
		var ok bool
		refs, ok = imageSets[*setName]
		if !ok {
			fatalf("unknown set %q; known sets: python, swebench", *setName)
		}
	} else {
		source = "refs given on the command line"
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := measure(ctx, refs, source, *stream, *cacheDir); err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "layer-amplification: "+format+"\n", args...)
	os.Exit(1)
}

// imageFacts is what one image contributes to the totals.
type imageFacts struct {
	ref      string
	manifest *image.Manifest
	// compressed is the sum of this image's layer blob sizes, counting a layer
	// once per reference, which is what the flattening store effectively pays.
	compressed int64
	// contentBytes is the size of the flattened tree after overwrites and
	// whiteouts, set only when --stream ran.
	contentBytes int64
	// fileCount is how many regular files survive flattening, --stream only.
	fileCount int
	// provisionedMiB is the ext4 file's apparent size per Converter.sizeFor.
	provisionedMiB int64
}

func measure(ctx context.Context, refs []string, source string, stream bool, cacheDir string) error {
	reg := image.NewRegistry(nil)

	fmt.Printf("bean layer-flattening amplification\n")
	fmt.Printf("images: %d (%s)\n", len(refs), source)
	if stream {
		fmt.Printf("mode:   --stream, content bytes are measured by unpacking layers\n")
	} else {
		fmt.Printf("mode:   manifests only, content bytes are ESTIMATED (run --stream to measure)\n")
	}
	hr()

	// Layer digests are collected across the whole set, which is the only place
	// sharing is visible: an image cannot tell you what it shares with another.
	layerSize := map[string]int64{}
	layerRefs := map[string]int{}
	var totalRefs int

	facts := make([]imageFacts, 0, len(refs))
	for _, ref := range refs {
		parsed, err := image.ParseReference(ref)
		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		m, err := reg.FetchManifest(ctx, parsed)
		if err != nil {
			return fmt.Errorf("fetch manifest for %s: %w", ref, err)
		}

		f := imageFacts{ref: ref, manifest: m}
		for _, l := range m.Layers {
			f.compressed += l.Size
			layerSize[l.Digest] = l.Size
			layerRefs[l.Digest]++
			totalRefs++
		}
		f.provisionedMiB = provisionedSizeMiB(m)
		facts = append(facts, f)

		fmt.Printf("  %-58s %2d layers  %8s compressed\n",
			truncate(ref, 58), len(m.Layers), human(f.compressed))
	}

	uniqueCompressed := sumOf(layerSize)
	var totalCompressed int64
	for _, f := range facts {
		totalCompressed += f.compressed
	}

	var uniqueRaw int64
	if stream {
		var err error
		uniqueRaw, err = streamContent(ctx, reg, facts, layerSize, cacheDir)
		if err != nil {
			return err
		}
	}

	report(facts, layerSize, layerRefs, totalRefs, totalCompressed, uniqueCompressed,
		uniqueRaw, stream)
	return nil
}

// provisionedSizeMiB replicates Converter.sizeFor. It is duplicated rather than
// called because sizeFor is unexported and linux-only, and a measurement tool
// must not change production code to make itself observable. If sizeFor changes,
// this goes stale -- the formula is stated in the output so a reader can check.
func provisionedSizeMiB(m *image.Manifest) int64 {
	var compressed int64
	for _, l := range m.Layers {
		compressed += l.Size
	}
	sizeMiB := (compressed >> 20) * 3
	if sizeMiB < defaultDiskMiB {
		sizeMiB = defaultDiskMiB
	}
	if sizeMiB < 256 {
		sizeMiB = 256
	}
	return sizeMiB
}

// layerWalk is the record of one layer's tar entries: enough to replay the
// flattening without downloading the layer again.
type layerWalk struct {
	// Files maps a cleaned path to its size. A directory entry is recorded with
	// size -1 so a later opaque whiteout can be applied to it.
	Files map[string]int64
	// Whiteouts are the paths this layer deletes from the layers below.
	Whiteouts []string
	// Opaques are directories whose previous content this layer clears.
	Opaques []string
	// Raw is the uncompressed tar stream length, which is the closest thing to
	// "what a lazily-read layer store would hold uncompressed".
	Raw int64
}

// streamContent fetches every distinct layer once, records its entries, then
// replays each image's layer list to get the flattened content size.
//
// Fetching once and replaying is the point: the same shared base layer is walked
// a single time no matter how many images reference it, which is also exactly
// the saving a layer-preserving store would realise. A tool that re-downloaded
// per image would take as long as the thing it is measuring.
// It returns the uncompressed content bytes of the distinct layers, counted
// once each: the layer-preserving side of the measured comparison.
func streamContent(ctx context.Context, reg *image.Registry, facts []imageFacts,
	layerSize map[string]int64, cacheDir string) (int64, error) {

	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return 0, fmt.Errorf("create cache dir: %w", err)
	}

	// A layer digest is content-addressed, but the repository it is fetched from
	// is not part of the digest, so the first image referencing a layer supplies
	// the pull location.
	origin := map[string]image.Reference{}
	order := make([]string, 0, len(layerSize))
	for _, f := range facts {
		parsed, err := image.ParseReference(f.ref)
		if err != nil {
			return 0, err
		}
		for _, l := range f.manifest.Layers {
			if _, seen := origin[l.Digest]; !seen {
				origin[l.Digest] = parsed
				order = append(order, l.Digest)
			}
		}
	}

	fmt.Printf("unpacking %d distinct layers (%s compressed) to measure content\n",
		len(order), human(sumOf(layerSize)))

	// uniqueRaw sums each distinct layer's content once. Summing the whole tar
	// stream would include header padding, so only the entries that survive as
	// files are counted, on the same block-rounded basis as the flattened side.
	var uniqueRaw int64
	walks := make(map[string]*layerWalk, len(order))
	for i, digest := range order {
		w, cached, err := loadOrWalk(ctx, reg, origin[digest], digest, cacheDir)
		if err != nil {
			return 0, fmt.Errorf("walk layer %s: %w", short(digest), err)
		}
		walks[digest] = w
		for _, size := range w.Files {
			if size > 0 {
				uniqueRaw += roundUp(size, ext4BlockSize)
			}
		}
		note := ""
		if cached {
			note = " (cached)"
		}
		fmt.Printf("  [%2d/%2d] %s  %7s compressed -> %8s in %d entries%s\n",
			i+1, len(order), short(digest), human(layerSize[digest]),
			human(w.Raw), len(w.Files), note)
	}
	hr()

	for i := range facts {
		digests := make([]string, 0, len(facts[i].manifest.Layers))
		for _, l := range facts[i].manifest.Layers {
			digests = append(digests, l.Digest)
		}
		facts[i].contentBytes, facts[i].fileCount = flatten(digests, walks)
	}
	return uniqueRaw, nil
}

// flatten replays layers in order and returns the surviving content size.
//
// The rules match extractTar: a later regular file replaces an earlier one at
// the same path, a .wh. entry deletes a path, and .wh..wh..opq clears a
// directory's previous content. Sizes are rounded up to ext4 blocks because a
// filesystem allocates whole blocks, and a layer of many tiny files costs
// noticeably more than the sum of their bytes.
func flatten(digests []string, walks map[string]*layerWalk) (int64, int) {
	live := map[string]int64{}
	for _, d := range digests {
		w := walks[d]
		if w == nil {
			continue
		}
		for _, dir := range w.Opaques {
			prefix := dir + "/"
			for p := range live {
				if strings.HasPrefix(p, prefix) {
					delete(live, p)
				}
			}
		}
		for _, victim := range w.Whiteouts {
			delete(live, victim)
			prefix := victim + "/"
			for p := range live {
				if strings.HasPrefix(p, prefix) {
					delete(live, p)
				}
			}
		}
		for p, size := range w.Files {
			live[p] = size
		}
	}

	var total int64
	var files int
	for _, size := range live {
		if size < 0 {
			continue // directory marker, no content
		}
		files++
		total += roundUp(size, ext4BlockSize)
	}
	return total, files
}

func roundUp(n, unit int64) int64 {
	if n == 0 {
		return 0
	}
	return ((n + unit - 1) / unit) * unit
}

// loadOrWalk returns a layer's walk, from cache when a previous run stored it.
//
// The cache holds walks, not blobs. A layer's entry list is a few hundred KiB
// where the blob is hundreds of MiB, and once walked the blob has no further
// use, so caching the result rather than the input keeps a repeated run cheap
// without filling the disk with the thing being measured.
func loadOrWalk(ctx context.Context, reg *image.Registry, ref image.Reference,
	digest, cacheDir string) (*layerWalk, bool, error) {

	cachePath := filepath.Join(cacheDir, strings.ReplaceAll(digest, ":", "_")+".gob")
	if f, err := os.Open(cachePath); err == nil {
		defer f.Close()
		var w layerWalk
		if err := gob.NewDecoder(bufio.NewReader(f)).Decode(&w); err == nil {
			return &w, true, nil
		}
		// A truncated cache entry from an interrupted run is not an error; the
		// layer is simply walked again.
	}

	w, err := walkLayer(ctx, reg, ref, digest)
	if err != nil {
		return nil, false, err
	}

	tmp := cachePath + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return w, false, nil // caching is an optimisation, not a requirement
	}
	bw := bufio.NewWriter(f)
	if err := gob.NewEncoder(bw).Encode(w); err == nil && bw.Flush() == nil {
		f.Close()
		os.Rename(tmp, cachePath)
	} else {
		f.Close()
		os.Remove(tmp)
	}
	return w, false, nil
}

// walkLayer reads one layer blob and records what it would write.
//
// Nothing is written to disk: the tar entry bodies are discarded and only their
// declared sizes are kept. That is what lets this run unprivileged, and it is
// also why ext4 metadata cannot be included -- there is no filesystem here to
// measure.
func walkLayer(ctx context.Context, reg *image.Registry, ref image.Reference,
	digest string) (*layerWalk, error) {

	blob, err := reg.FetchBlob(ctx, ref, digest)
	if err != nil {
		return nil, err
	}
	defer blob.Close()

	var src io.Reader
	// Layer media types are not trusted here for the same reason applyLayer does
	// not fully trust them: some registries are loose about the gzip suffix. The
	// magic bytes decide.
	br := bufio.NewReaderSize(blob, 1<<20)
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("open gzip layer: %w", err)
		}
		defer zr.Close()
		src = zr
	} else {
		src = br
	}

	w := &layerWalk{Files: map[string]int64{}}
	raw := &countingReader{r: src}
	tr := tar.NewReader(raw)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read layer: %w", err)
		}

		clean := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		base := path.Base(clean)
		if strings.HasPrefix(base, ".wh.") {
			dir := path.Dir(clean)
			if base == ".wh..wh..opq" {
				w.Opaques = append(w.Opaques, dir)
				continue
			}
			victim := path.Join(dir, strings.TrimPrefix(base, ".wh."))
			w.Whiteouts = append(w.Whiteouts, victim)
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeReg:
			w.Files[clean] = hdr.Size
		case tar.TypeDir:
			w.Files[clean] = -1
		case tar.TypeSymlink:
			// A symlink's target is stored in the inode for short targets, so it
			// costs no data block. Recorded at zero so it still participates in
			// overwrites and whiteouts.
			w.Files[clean] = 0
		case tar.TypeLink:
			// writeEntry falls back to a full copy when the hard link cannot be
			// made, which is the common case across layers, so it is counted as
			// content rather than free.
			if size, ok := w.Files[strings.TrimPrefix(path.Clean("/"+hdr.Linkname), "/")]; ok && size > 0 {
				w.Files[clean] = size
			} else {
				w.Files[clean] = 0
			}
		default:
			// Device nodes and fifos are skipped by writeEntry, so they cost
			// nothing here either.
		}
	}
	w.Raw = raw.n
	return w, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func report(facts []imageFacts, layerSize map[string]int64, layerRefs map[string]int,
	totalRefs int, totalCompressed, uniqueCompressed, uniqueRaw int64, streamed bool) {

	hr()
	fmt.Printf("1. LAYER SHARING (measured from manifests)\n\n")
	fmt.Printf("   layer references across the set   %d\n", totalRefs)
	fmt.Printf("   distinct layer digests            %d\n", len(layerSize))
	fmt.Printf("   compressed bytes, counting each reference   %10s\n", human(totalCompressed))
	fmt.Printf("   compressed bytes, distinct digests only     %10s\n", human(uniqueCompressed))
	if uniqueCompressed > 0 {
		fmt.Printf("   sharing factor                             %10.2fx\n",
			float64(totalCompressed)/float64(uniqueCompressed))
	}
	fmt.Printf("\n   A layer-preserving store keeps the second figure. Flattening pays\n")
	fmt.Printf("   the first, because each image's copy of a shared layer ends up inside\n")
	fmt.Printf("   its own ext4 file with no way to point at the other copy.\n")

	shared := make([]string, 0, len(layerRefs))
	for d, n := range layerRefs {
		if n > 1 {
			shared = append(shared, d)
		}
	}
	sort.Slice(shared, func(i, j int) bool {
		return layerSize[shared[i]] > layerSize[shared[j]]
	})
	if len(shared) > 0 {
		fmt.Printf("\n   layers referenced by more than one image:\n")
		for _, d := range shared[:min(len(shared), 8)] {
			fmt.Printf("     %s  %8s  x%d\n", short(d), human(layerSize[d]), layerRefs[d])
		}
		if len(shared) > 8 {
			fmt.Printf("     ... and %d more\n", len(shared)-8)
		}
	} else {
		fmt.Printf("\n   No layer digest is referenced by more than one image in this set.\n")
		fmt.Printf("   There is no sharing here for any store to exploit, so flattening\n")
		fmt.Printf("   costs nothing extra on this set.\n")
	}

	hr()
	fmt.Printf("2. WHAT THE FLATTENED FORM COSTS\n\n")

	var contentTotal, provisionedTotal int64
	var fileTotal int
	for _, f := range facts {
		contentTotal += f.contentBytes
		provisionedTotal += f.provisionedMiB << 20
		fileTotal += f.fileCount
	}

	if streamed {
		fmt.Printf("   MEASURED. Every distinct layer was fetched and its tar entries\n")
		fmt.Printf("   replayed in manifest order, applying overwrites and whiteouts the\n")
		fmt.Printf("   way extractTar does. File sizes are rounded up to %d-byte blocks.\n\n",
			ext4BlockSize)
		for _, f := range facts {
			fmt.Printf("     %-52s %9s content, %6d files\n",
				truncate(f.ref, 52), human(f.contentBytes), f.fileCount)
		}
		fmt.Printf("\n   flattened content, summed over the set     %10s\n", human(contentTotal))
	} else {
		est := int64(float64(totalCompressed) * assumedCompressionRatio)
		uniqEst := int64(float64(uniqueCompressed) * assumedCompressionRatio)
		contentTotal = est
		fmt.Printf("   ESTIMATE, not a measurement. Derived as compressed bytes x %.1f,\n",
			assumedCompressionRatio)
		fmt.Printf("   an assumed gzip ratio for distro and python-package content. No\n")
		fmt.Printf("   layer was downloaded, so no file size was observed. Run --stream to\n")
		fmt.Printf("   replace this with a measurement.\n\n")
		fmt.Printf("   estimated unpacked bytes, per reference     %10s\n", human(est))
		fmt.Printf("   estimated unpacked bytes, distinct only     %10s\n", human(uniqEst))
	}

	fmt.Printf("\n   provisioned ext4 size, summed over the set  %10s\n", human(provisionedTotal))
	fmt.Printf("   COMPUTED, not measured: max(sum(compressed)>>20 * 3, %d MiB) per\n", defaultDiskMiB)
	fmt.Printf("   image, replicating Converter.sizeFor with noded's default disk size.\n")
	fmt.Printf("   The files are sparse, so this is apparent size -- what Cached()\n")
	fmt.Printf("   reports and what a disk has to be able to accommodate, not blocks\n")
	fmt.Printf("   allocated today.\n")

	hr()
	fmt.Printf("3. AMPLIFICATION\n\n")

	if uniqueCompressed > 0 {
		fmt.Printf("   on compressed layer bytes (MEASURED from manifests)\n")
		fmt.Printf("     flattened %10s / layer-preserving %10s  =  %.2fx\n",
			human(totalCompressed), human(uniqueCompressed),
			float64(totalCompressed)/float64(uniqueCompressed))
	}
	if streamed && uniqueRaw > 0 {
		fmt.Printf("\n   on unpacked content bytes (MEASURED by unpacking every layer)\n")
		fmt.Printf("     flattened %10s / layer-preserving %10s  =  %.2fx\n",
			human(contentTotal), human(uniqueRaw),
			float64(contentTotal)/float64(uniqueRaw))
		fmt.Printf("\n   The two ratios agreeing is the check that matters: it means the\n")
		fmt.Printf("   shared layers and the per-image layers compress alike, so the cheap\n")
		fmt.Printf("   manifest-only run can be trusted on other image sets.\n")
	}

	fmt.Printf("\n   WHAT THIS RATIO IS: the bytes a flattening store holds for this set\n")
	fmt.Printf("   divided by the bytes a store that deduplicates by layer digest would\n")
	fmt.Printf("   hold. It is computed on compressed blob sizes taken from the\n")
	fmt.Printf("   manifests, which is the one quantity that is measured exactly and\n")
	fmt.Printf("   identically on both sides.\n")

	fmt.Printf("\n   WHAT IT IS NOT: the ratio of bytes on disk today. Three things are\n")
	fmt.Printf("   outside it:\n")
	fmt.Printf("     - ext4 metadata (inodes, directory blocks, group descriptors).\n")
	fmt.Printf("       Measuring it needs mkfs.ext4 and root on Linux; this tool runs\n")
	fmt.Printf("       unprivileged, so the figure is absent rather than guessed.\n")
	fmt.Printf("     - the gap between provisioned and allocated size. The ext4 files\n")
	fmt.Printf("       are sparse and the provisioned figure above is the apparent one.\n")
	fmt.Printf("     - decompression, which differs per layer. Assumed uniform without\n")
	fmt.Printf("       --stream; it cancels out of the ratio only if the shared layers\n")
	fmt.Printf("       and the per-image layers compress alike, which --stream checks.\n")

	fmt.Printf("\n   ASSUMED: that a layer-preserving store deduplicates perfectly by\n")
	fmt.Printf("   digest and stores each distinct layer once. overlaybd does keep layer\n")
	fmt.Printf("   structure, but its own on-disk form is not measured here, so treat\n")
	fmt.Printf("   the layer-preserving side as a lower bound on what it would use.\n")

	if len(facts) > 1 {
		hr()
		fmt.Printf("4. EXTRAPOLATION\n\n")
		// The marginal cost of one more image is what matters at eval scale: the
		// shared base is a fixed cost for a layer store and a per-image cost for
		// a flattening one.
		perImage := totalCompressed / int64(len(facts))
		marginal := unsharedPerImage(facts, layerRefs)
		sharedFixed := totalCompressed/int64(len(facts)) - marginal

		fmt.Printf("   mean compressed bytes per image             %10s\n", human(perImage))
		fmt.Printf("   of which in layers no other image shares    %10s\n", human(marginal))
		fmt.Printf("   of which in layers shared across the set    %10s\n", human(sharedFixed))
		fmt.Println()
		for _, n := range []int64{100, 1000} {
			flat := perImage * n
			layered := uniqueCompressed - marginal*int64(len(facts)) + marginal*n
			fmt.Printf("   %5d images: flattened %9s, layer-preserving %9s  (%.1fx)\n",
				n, human(flat), human(layered), float64(flat)/float64(layered))
		}
		fmt.Printf("\n   The extrapolation holds the shared layers fixed and repeats only\n")
		fmt.Printf("   the layers referenced by a single image. It assumes new task images\n")
		fmt.Printf("   share the same base, which is what SWE-bench task images do but is\n")
		fmt.Printf("   an assumption about the workload, not a measurement of it.\n")
	}
	hr()
}

// unsharedPerImage is the mean compressed bytes per image in layers no other
// image in the set references. It is the marginal cost of one more image to a
// layer-preserving store.
func unsharedPerImage(facts []imageFacts, layerRefs map[string]int) int64 {
	if len(facts) == 0 {
		return 0
	}
	var total int64
	for _, f := range facts {
		seen := map[string]bool{}
		for _, l := range f.manifest.Layers {
			if seen[l.Digest] || layerRefs[l.Digest] > 1 {
				continue
			}
			seen[l.Digest] = true
			total += l.Size
		}
	}
	return total / int64(len(facts))
}

func sumOf(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

func short(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func hr() {
	fmt.Println(strings.Repeat("-", 74))
}
