package image

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// lz4Decompress expands one LZ4 block into dst and returns the byte count.
//
// Written out rather than taken as a dependency because this is the whole format:
// a token, some literals, and a back-reference, repeated. The alternative was a
// module for sixty lines of decoding, on a path where every guest read passes
// through it. Compression is not implemented -- sealing a layer is upstream's job.
//
// This is the LZ4 *block* format, not the framed one. A ZFile stores each block's
// compressed length in its index, so there is no frame header to carry it.
func lz4Decompress(src, dst []byte) (int, error) {
	var sp, dp int

	for sp < len(src) {
		token := int(src[sp])
		sp++

		// Literals: length in the token's high nibble, extended by 0xff bytes.
		litLen := token >> 4
		if litLen == 0x0f {
			for {
				if sp >= len(src) {
					return 0, errors.New("image: lz4 block ends inside a literal length")
				}
				n := int(src[sp])
				sp++
				litLen += n
				if n != 0xff {
					break
				}
			}
		}

		if litLen > 0 {
			if sp+litLen > len(src) {
				return 0, fmt.Errorf("image: lz4 block claims %d literal bytes, %d remain",
					litLen, len(src)-sp)
			}
			if dp+litLen > len(dst) {
				return 0, errors.New("image: lz4 literals overrun the output buffer")
			}
			copy(dst[dp:dp+litLen], src[sp:sp+litLen])
			sp += litLen
			dp += litLen
		}

		// The last sequence in a block is literals only, with no match after it.
		if sp >= len(src) {
			break
		}
		if sp+2 > len(src) {
			return 0, errors.New("image: lz4 block ends inside a match offset")
		}

		offset := int(binary.LittleEndian.Uint16(src[sp : sp+2]))
		sp += 2
		if offset == 0 {
			return 0, errors.New("image: lz4 match offset is zero, which would copy from itself")
		}
		if offset > dp {
			return 0, fmt.Errorf("image: lz4 match reaches %d bytes back, before the start of "+
				"the %d bytes decoded", offset, dp)
		}

		// Match: length in the low nibble, plus the format's 4-byte minimum.
		matchLen := token & 0x0f
		if matchLen == 0x0f {
			for {
				if sp >= len(src) {
					return 0, errors.New("image: lz4 block ends inside a match length")
				}
				n := int(src[sp])
				sp++
				matchLen += n
				if n != 0xff {
					break
				}
			}
		}
		matchLen += 4

		if dp+matchLen > len(dst) {
			return 0, errors.New("image: lz4 match overruns the output buffer")
		}

		// Byte at a time, because a match may overlap its own output: an offset of 1
		// with a length of 20 is how the format encodes a run of one repeated byte,
		// and copy() would read the source before those bytes exist.
		for i := 0; i < matchLen; i++ {
			dst[dp+i] = dst[dp+i-offset]
		}
		dp += matchLen
	}

	return dp, nil
}
