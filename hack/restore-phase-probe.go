//go:build ignore

// Measures where a restore's ~940ms of unpacking actually goes.
//
// The restore total is ~950ms of which `PUT /snapshot/load` is 7ms, so the rest
// is this program's subject. What it is NOT is guest boot: a restored guest does
// not boot. That means the 940ms is ours to remove, unlike the 770ms a cold
// create spends in the kernel.
//
// Attributing it by reasoning would repeat an earlier mistake: the concurrency
// investigation "obviously" pointed at max_creates and was wrong (it was the core
// count). So this decomposes the path into the three candidates and times each
// against a real bundle:
//
//	gunzip        — the bundle is gzip BestSpeed; a 512 MiB memory image has to
//	                be inflated even when only the rootfs member is wanted
//	tar walk      — header parsing and member seeking
//	sparse write  — writing the memory image out to disk
//
// Usage:
//
//	go run hack/restore-phase-probe.go --bundle /path/to/bundle.tar.gz
//
// The bundle is whatever the node stored: /var/lib/bean/sandboxes/.snapshots holds
// unpacked entries, so use a blob from the control plane's snapshot store, or
// produce one with `bean snapshot create` and read it back from S3.
package main

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	bundle := flag.String("bundle", "", "path to a snapshot bundle (tar.gz)")
	flag.Parse()
	if *bundle == "" {
		fmt.Fprintln(os.Stderr, "usage: restore-phase-probe.go --bundle <file>")
		os.Exit(64)
	}

	st, err := os.Stat(*bundle)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("bundle: %s (%d bytes on disk)\n", *bundle, st.Size())
	fmt.Println("-----------------------------------------------------------")

	// 1. Inflate only. Discarding the output isolates decompression from any
	// filesystem cost, which is the number that decides whether compression is
	// the thing to attack.
	inflated, gunzipDur := timeGunzip(*bundle)
	rate := float64(inflated) / gunzipDur.Seconds() / (1 << 20)
	fmt.Printf("gunzip          %7.0f ms   %d bytes inflated (%.0f MiB/s)\n",
		float64(gunzipDur.Milliseconds()), inflated, rate)

	// 2. Inflate plus tar walk, still discarding member contents. The difference
	// from the previous line is what header parsing and seeking cost.
	members, walkDur := timeTarWalk(*bundle)
	fmt.Printf("gunzip + walk   %7.0f ms   %d members\n",
		float64(walkDur.Milliseconds()), len(members))
	fmt.Printf("  → tar walk alone: %.0f ms\n",
		float64((walkDur - gunzipDur).Milliseconds()))
	for _, m := range members {
		fmt.Printf("    %-24s %12d bytes\n", m.name, m.size)
	}

	// 3. Inflate, walk, and write the memory image to disk. The difference is the
	// write cost — the part a range read over separate objects would avoid
	// entirely for a cache hit.
	written, fullDur := timeFullExtract(*bundle)
	fmt.Printf("gunzip + write  %7.0f ms   %d bytes written\n",
		float64(fullDur.Milliseconds()), written)
	fmt.Printf("  → write alone:   %.0f ms\n",
		float64((fullDur - walkDur).Milliseconds()))

	fmt.Println("-----------------------------------------------------------")
	fmt.Println("Reading the largest cost here is the point. If gunzip dominates,")
	fmt.Println("compression is on the critical path of every restore including a")
	fmt.Println("cache hit, and storing members as separate uncompressed objects")
	fmt.Println("lets a hit skip the memory image altogether (s3-storage.md §5).")
	fmt.Println("If the write dominates instead, the cache is already doing its job")
	fmt.Println("and the remaining cost is the one restore that populates it.")
}

func timeGunzip(path string) (int64, time.Duration) {
	f, err := os.Open(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		fatal(err)
	}
	defer zr.Close()

	start := time.Now()
	n, err := io.Copy(io.Discard, zr)
	if err != nil {
		fatal(err)
	}
	return n, time.Since(start)
}

type member struct {
	name string
	size int64
}

func timeTarWalk(path string) ([]member, time.Duration) {
	f, err := os.Open(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		fatal(err)
	}
	defer zr.Close()

	start := time.Now()
	var out []member
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fatal(err)
		}
		// Discarded rather than kept: this line measures the walk, and holding the
		// contents would add allocation to the number.
		n, err := io.Copy(io.Discard, tr)
		if err != nil {
			fatal(err)
		}
		out = append(out, member{hdr.Name, n})
	}
	return out, time.Since(start)
}

func timeFullExtract(path string) (int64, time.Duration) {
	f, err := os.Open(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		fatal(err)
	}
	defer zr.Close()

	dir, err := os.MkdirTemp("", "bean-restore-probe-*")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(dir)

	start := time.Now()
	var written int64
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fatal(err)
		}
		out, err := os.Create(dir + "/" + sanitise(hdr.Name))
		if err != nil {
			fatal(err)
		}
		n, err := io.Copy(out, tr)
		out.Close()
		if err != nil {
			fatal(err)
		}
		written += n
	}
	// Synced before stopping the clock: without it the cost of getting the bytes
	// to disk is hidden in the page cache, which is exactly the mistake that let a
	// silently corrupted filesystem pass three layers of tests.
	syncDir(dir)
	return written, time.Since(start)
}

func sanitise(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		if name[i] == '/' || name[i] == '.' && i == 0 {
			out = append(out, '_')
			continue
		}
		out = append(out, name[i])
	}
	return string(out)
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "restore-phase-probe:", err)
	os.Exit(70)
}
