package image

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// refToFilename maps an image reference to a safe filename. Refs contain
// slashes, colons and at-signs, all of which would either escape the directory
// or confuse path handling, so they are replaced rather than escaped: the
// result only has to be stable and collision-free, not reversible.
func refToFilename(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("image: reference required")
	}
	var b strings.Builder
	b.Grow(len(ref))
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			// Distinct separators would collide if all mapped to the same
			// character, so the codepoint is encoded.
			fmt.Fprintf(&b, "_%x", r)
		}
	}
	return b.String(), nil
}

// cloneSparse copies base to dst and grows it to sizeMiB. The copy is what
// gives each sandbox an independent writable rootfs; growing it afterwards is
// how a spec's disk bound is honoured without keeping a base image per size.
//
// The file is created sparse, so a 2 GiB rootfs whose image occupies 200 MiB
// consumes 200 MiB until the sandbox writes.
func cloneSparse(base, dst string, sizeMiB int64) error {
	src, err := os.Open(base)
	if err != nil {
		return fmt.Errorf("image: open base: %w", err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("image: stat base: %w", err)
	}
	want := sizeMiB << 20
	if info.Size() > want {
		return fmt.Errorf("image: base is %d MiB, larger than the requested %d MiB",
			info.Size()>>20, sizeMiB)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("image: create rootfs: %w", err)
	}
	defer out.Close()

	// Zero runs in the base image are skipped rather than written. A freshly
	// made filesystem is mostly empty, so copying it byte for byte would make
	// every sandbox cost the base image's provisioned size on disk — the thing
	// sparse files exist to avoid.
	if err := copySkippingZeros(out, src); err != nil {
		return fmt.Errorf("image: copy base: %w", err)
	}
	// Truncate up rather than writing zeroes: the tail stays unallocated.
	if err := out.Truncate(want); err != nil {
		return fmt.Errorf("image: size rootfs: %w", err)
	}
	return out.Sync()
}

// copySkippingZeros writes src to dst, seeking over all-zero blocks so they
// stay unallocated.
func copySkippingZeros(dst *os.File, src io.Reader) error {
	const chunk = 256 << 10
	buf := make([]byte, chunk)
	var offset int64
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			if !allZero(buf[:n]) {
				if _, werr := dst.WriteAt(buf[:n], offset); werr != nil {
					return werr
				}
			}
			offset += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
