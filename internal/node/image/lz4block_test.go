package image

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// lz4Literal encodes data as a single literals-only block, which is what a real
// compressor emits for incompressible input.
func lz4Literal(data []byte) []byte {
	var out []byte
	n := len(data)
	if n < 0x0f {
		out = append(out, byte(n<<4))
	} else {
		out = append(out, 0xf0)
		rem := n - 0x0f
		for rem >= 0xff {
			out = append(out, 0xff)
			rem -= 0xff
		}
		out = append(out, byte(rem))
	}
	return append(out, data...)
}

// lz4Match encodes literals followed by a back-reference, the shape that makes LZ4
// a compressor at all.
func lz4Match(literals []byte, offset, matchLen int) []byte {
	litLen := len(literals)
	ml := matchLen - 4

	token := byte(0)
	var ext []byte
	if litLen < 0x0f {
		token |= byte(litLen << 4)
	} else {
		token |= 0xf0
		rem := litLen - 0x0f
		for rem >= 0xff {
			ext = append(ext, 0xff)
			rem -= 0xff
		}
		ext = append(ext, byte(rem))
	}

	var mExt []byte
	if ml < 0x0f {
		token |= byte(ml)
	} else {
		token |= 0x0f
		rem := ml - 0x0f
		for rem >= 0xff {
			mExt = append(mExt, 0xff)
			rem -= 0xff
		}
		mExt = append(mExt, byte(rem))
	}

	out := []byte{token}
	out = append(out, ext...)
	out = append(out, literals...)
	off := make([]byte, 2)
	binary.LittleEndian.PutUint16(off, uint16(offset))
	out = append(out, off...)
	return append(out, mExt...)
}

func TestLZ4DecompressLiteralsOnly(t *testing.T) {
	for _, want := range [][]byte{
		[]byte("short"),
		bytes.Repeat([]byte("x"), 14),  // just under the nibble limit
		bytes.Repeat([]byte("y"), 15),  // exactly at it, so a length byte follows
		bytes.Repeat([]byte("z"), 300), // past 0xff, so the length extends twice
	} {
		dst := make([]byte, len(want)+64)
		n, err := lz4Decompress(lz4Literal(want), dst)
		if err != nil {
			t.Fatalf("literal block of %d bytes: %v", len(want), err)
		}
		if !bytes.Equal(dst[:n], want) {
			t.Errorf("literal block of %d bytes did not round trip", len(want))
		}
	}
}

// An overlapping match is the format's run-length encoding, and it is the one case
// a copy() implementation gets wrong.
//
// With offset 1 and length 20, every byte after the first is copied from the byte
// the loop just wrote. copy() reads its whole source before writing, so it would
// produce whatever was in the buffer instead of the repeated byte.
func TestLZ4DecompressOverlappingMatchIsARun(t *testing.T) {
	block := lz4Match([]byte("A"), 1, 20)
	dst := make([]byte, 64)
	n, err := lz4Decompress(block, dst)
	if err != nil {
		t.Fatalf("overlapping match: %v", err)
	}
	want := bytes.Repeat([]byte("A"), 21)
	if !bytes.Equal(dst[:n], want) {
		t.Errorf("overlapping match produced %q, want %q", dst[:n], want)
	}
}

func TestLZ4DecompressNonOverlappingMatch(t *testing.T) {
	// "abcd" then a match 4 back for 4 bytes: "abcdabcd".
	block := lz4Match([]byte("abcd"), 4, 4)
	dst := make([]byte, 64)
	n, err := lz4Decompress(block, dst)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if got := string(dst[:n]); got != "abcdabcd" {
		t.Errorf("got %q, want %q", got, "abcdabcd")
	}
}

// A match reaching before the start of the output is refused rather than read.
//
// This is the difference between an error and reading whatever the buffer held. On a
// guest read path that buffer is reused across requests, so accepting it would serve
// one sandbox bytes from another sandbox's read.
func TestLZ4DecompressRefusesMatchBeforeStart(t *testing.T) {
	dst := make([]byte, 64)
	// Two literals, then a match 100 bytes back: nothing is there yet.
	if _, err := lz4Decompress(lz4Match([]byte("ab"), 100, 8), dst); err == nil {
		t.Fatal("a match reaching before the output start was accepted, so it read stale " +
			"bytes from the buffer")
	}
	// Offset zero would copy a byte from itself.
	if _, err := lz4Decompress(lz4Match([]byte("ab"), 0, 8), dst); err == nil {
		t.Fatal("a zero match offset was accepted")
	}
}

func TestLZ4DecompressRefusesOutputOverrun(t *testing.T) {
	big := lz4Literal(bytes.Repeat([]byte("q"), 100))
	if _, err := lz4Decompress(big, make([]byte, 10)); err == nil {
		t.Fatal("literals larger than the destination were accepted, overrunning it")
	}
	if _, err := lz4Decompress(lz4Match([]byte("abcd"), 4, 200), make([]byte, 10)); err == nil {
		t.Fatal("a match larger than the destination was accepted")
	}
}

// The decoder agrees with the reference LZ4 implementation, not just with itself.
//
// Every other test here builds its own blocks, so a misreading of the format shared
// between the encoder helpers and the decoder would pass all of them. These vectors
// come from the lz4 CLI, which this code shares nothing with.
func TestLZ4DecompressMatchesReferenceImplementation(t *testing.T) {
	if len(lz4RealVectors) == 0 {
		t.Fatal("no reference vectors, so nothing here checks against real LZ4 output")
	}
	for _, v := range lz4RealVectors {
		dst := make([]byte, len(v.plain))
		n, err := lz4Decompress(v.block, dst)
		if err != nil {
			t.Errorf("%s: %v", v.name, err)
			continue
		}
		if n != len(v.plain) || !bytes.Equal(dst[:n], v.plain) {
			t.Errorf("%s: decoded %d bytes, want %d, and the content differs",
				v.name, n, len(v.plain))
		}
	}
}

func TestLZ4DecompressRefusesTruncatedBlock(t *testing.T) {
	dst := make([]byte, 64)
	// A token claiming 20 literals with none following.
	if _, err := lz4Decompress([]byte{0xf0, 0x05}, dst); err == nil {
		t.Fatal("a block whose literals are missing was accepted")
	}
	// Literals present, then a single byte where a 2-byte match offset belongs.
	if _, err := lz4Decompress([]byte{0x10, 'a', 0x01}, dst); err == nil {
		t.Fatal("a block truncated inside a match offset was accepted")
	}
}
