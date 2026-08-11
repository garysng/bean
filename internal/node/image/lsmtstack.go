package image

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// lsmtStack presents a chain of read-only overlaybd layers as one virtual disk.
//
// The merge rule is the whole of it: where two layers map the same range, the newer
// one wins. Upstream encodes that in the index entry's tag, where a lower tag is a
// newer layer -- the opposite of the order layers are listed in a manifest, which is
// oldest first. Getting the direction wrong serves the base image's version of a file
// that a later layer replaced, and nothing reports an error, because both answers are
// structurally valid.
type lsmtStack struct {
	// mappings is the merged index, sorted by virtual offset and non-overlapping.
	mappings []stackMapping
	// layers are the opened layer files, indexed by the layer field of a mapping.
	layers []io.ReaderAt
	// virtualSize is the size of the disk the stack presents, in bytes.
	virtualSize int64
	// remote is true when any layer is read over the network rather than from a file.
	// Carried on the stack because that is where the knowledge is: the caller assembling
	// the sources knows which are remote, and everything downstream would otherwise have
	// to be told.
	remote bool
}

// stackMapping is a merged extent: a run of the virtual disk and which layer holds it.
type stackMapping struct {
	// offset and length are in sectors, as the format stores them.
	offset uint64
	length uint32
	// moffset is the physical offset within the layer, in sectors.
	moffset uint64
	zeroed  bool
	// layer indexes into lsmtStack.layers.
	layer int
}

func (m stackMapping) end() uint64 { return m.offset + uint64(m.length) }

// openLSMTStack opens a chain of layer files, oldest first.
//
// Oldest first is the order a manifest lists them and the order the overlaybd config
// carries, so taking that order here means no caller has to reverse it -- and a
// reversal that happens in one of two call sites is exactly the bug this ordering
// avoids.
func openLSMTStack(paths []string) (*lsmtStack, func() error, error) {
	srcs := make([]layerSource, 0, len(paths))
	for _, p := range paths {
		srcs = append(srcs, layerSource{Path: p})
	}
	return openLSMTStackFrom(srcs)
}

// layerSource says where one layer's bytes come from: a local file, or a remote blob read
// through range requests.
//
// A struct rather than two entry points because a chain is legitimately mixed -- a node can
// hold the base locally and read a leaf remotely -- and a caller that had to sort a chain
// into two lists would also have to remember to keep them in order, which is the merge's
// one load-bearing property.
type layerSource struct {
	// Path names a local file. Takes precedence: a layer already on disk is never fetched.
	Path string
	// Remote reads the layer over the network. Ignored when Path is set.
	Remote io.ReaderAt
	// RemoteSize is the blob's length, which a remote reader cannot infer from a stat.
	RemoteSize int64
	// Label identifies the layer in errors -- a digest for a remote one, since a caller
	// looking at "open layer failed" needs to know which.
	Label string
}

func (s layerSource) label() string {
	if s.Label != "" {
		return s.Label
	}
	return s.Path
}

// openLSMTStackFrom opens a chain from mixed local and remote sources, oldest first.
func openLSMTStackFrom(srcs []layerSource) (*lsmtStack, func() error, error) {
	if len(srcs) == 0 {
		return nil, nil, errors.New("image: an lsmt stack needs at least one layer")
	}

	var files []*os.File
	closeAll := func() error {
		var errs []error
		for _, f := range files {
			errs = append(errs, f.Close())
		}
		return errors.Join(errs...)
	}

	layers := make([]*lsmtLayer, 0, len(srcs))
	readers := make([]io.ReaderAt, 0, len(srcs))
	anyRemote := false
	for _, src := range srcs {
		var base io.ReaderAt
		var baseSize int64

		switch {
		case src.Path != "":
			f, err := os.Open(src.Path)
			if err != nil {
				_ = closeAll()
				return nil, nil, fmt.Errorf("image: open layer %s: %w", src.Path, err)
			}
			files = append(files, f)

			st, err := f.Stat()
			if err != nil {
				_ = closeAll()
				return nil, nil, fmt.Errorf("image: stat layer %s: %w", src.Path, err)
			}
			base, baseSize = f, st.Size()

		case src.Remote != nil:
			anyRemote = true
			if src.RemoteSize <= 0 {
				_ = closeAll()
				return nil, nil, fmt.Errorf("image: remote layer %s has no size, and the "+
					"tar and trailer are both located from the end of the blob",
					src.label())
			}
			base, baseSize = src.Remote, src.RemoteSize

		default:
			_ = closeAll()
			return nil, nil, fmt.Errorf("image: layer %s names neither a file nor a remote "+
				"source", src.label())
		}

		// Three containers, outermost first. bean seals with `overlaybd-commit -z -t`, so
		// a layer is: a tar (from -t, which makes it a valid OCI blob), holding a ZFile
		// (from -z, block-compressed so any one block can be expanded alone), holding the
		// LSMT index and its extents.
		//
		// Unwrapping in that order is what lets each reader stay unaware of the one
		// outside it: the index addresses uncompressed positions, and the ZFile beneath
		// turns those into block reads. It is also why a remote layer needs nothing extra
		// here -- every level below reads through io.ReaderAt, so a range-reading base
		// substitutes for a file without any of them knowing.
		payload, size, err := openSealedLayerPayload(base, baseSize)
		if err != nil {
			_ = closeAll()
			return nil, nil, fmt.Errorf("image: open layer %s: %w", src.label(), err)
		}
		if z, zerr := openZFile(payload, size); zerr == nil {
			payload = z
			size = z.size()
		}

		layer, err := openLSMTLayer(payload, size)
		if err != nil {
			_ = closeAll()
			return nil, nil, fmt.Errorf("image: open layer %s: %w", src.label(), err)
		}
		layers = append(layers, layer)
		readers = append(readers, payload)
	}

	stack := &lsmtStack{layers: readers, remote: anyRemote}
	stack.mappings = mergeLSMTLayers(layers)
	if len(stack.mappings) == 0 {
		_ = closeAll()
		return nil, nil, errors.New("image: the layer chain maps no data at all")
	}

	// The topmost layer decides the disk's size: a later layer may grow the
	// filesystem, and sizing from the base would present a device smaller than the
	// filesystem on it. That failure is a guest that will not boot, reported as a
	// geometry error the caller never sees.
	for _, l := range layers {
		if int64(l.virtualSize) > stack.virtualSize {
			stack.virtualSize = int64(l.virtualSize)
		}
	}
	return stack, closeAll, nil
}

// interval is a half-open run of sectors, [start, end).
type interval struct {
	start, end uint64
}

// mergeLSMTLayers flattens a chain into one non-overlapping index.
//
// Layer by layer from the top down, emitting only the parts of each mapping that no
// newer layer has already claimed. A newer layer's range can land in the middle of an
// older one, so the older mapping has to be *split* rather than trimmed at one end --
// a single high-water mark gets this wrong in the most common case there is, a small
// file replaced inside a large base extent, and the result is the older layer winning.
//
// The claimed set is rebuilt once per layer by a linear merge of two sorted lists
// rather than spliced per mapping, which would be quadratic on an index with tens of
// thousands of extents.
func mergeLSMTLayers(layers []*lsmtLayer) []stackMapping {
	var out []stackMapping
	var claimed []interval

	// Newest first: the last layer in the chain is the top one.
	for i := len(layers) - 1; i >= 0; i-- {
		mappings := layers[i].mappings

		// Walk this layer's mappings against the claimed set, both sorted by offset, so
		// the pointer into claimed only moves forward.
		ci := 0
		for _, m := range mappings {
			pos, end := m.offset, m.end()

			// Skip claimed ranges that finish before this mapping starts.
			for ci < len(claimed) && claimed[ci].end <= pos {
				ci++
			}
			// Emit the gaps between claimed ranges, from ci onward. j is a local cursor:
			// ci itself must not advance past a claimed range that the *next* mapping
			// also overlaps.
			for j := ci; pos < end; {
				if j >= len(claimed) || claimed[j].start >= end {
					// Nothing claimed ahead within this mapping: the rest is free.
					out = append(out, sliceMapping(m, pos, end, i))
					break
				}
				if claimed[j].start > pos {
					out = append(out, sliceMapping(m, pos, claimed[j].start, i))
				}
				if claimed[j].end > pos {
					pos = claimed[j].end
				}
				j++
			}
		}

		// Fold this layer's ranges into the claimed set for the layers below.
		claimed = unionIntervals(claimed, mappingIntervals(mappings))
	}

	sort.Slice(out, func(a, b int) bool { return out[a].offset < out[b].offset })
	return out
}

// sliceMapping cuts [from, to) out of a mapping, moving the physical offset with it.
//
// The offset has to move by exactly the amount the front was cut, or the slice reads
// the right number of bytes from the wrong place -- which is silent, because the bytes
// it finds are valid data from elsewhere in the layer. A zeroed run has no backing
// bytes, so its offset stays put.
func sliceMapping(m lsmtMapping, from, to uint64, layer int) stackMapping {
	moff := m.moffset
	if !m.zeroed {
		moff += from - m.offset
	}
	return stackMapping{
		offset:  from,
		length:  uint32(to - from),
		moffset: moff,
		zeroed:  m.zeroed,
		layer:   layer,
	}
}

func mappingIntervals(mappings []lsmtMapping) []interval {
	out := make([]interval, 0, len(mappings))
	for _, m := range mappings {
		out = append(out, interval{start: m.offset, end: m.end()})
	}
	return out
}

// unionIntervals merges two sorted, disjoint interval lists into one.
func unionIntervals(a, b []interval) []interval {
	merged := make([]interval, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		var next interval
		switch {
		case j >= len(b):
			next, i = a[i], i+1
		case i >= len(a):
			next, j = b[j], j+1
		case a[i].start <= b[j].start:
			next, i = a[i], i+1
		default:
			next, j = b[j], j+1
		}

		if n := len(merged); n > 0 && next.start <= merged[n-1].end {
			if next.end > merged[n-1].end {
				merged[n-1].end = next.end
			}
			continue
		}
		merged = append(merged, next)
	}
	return merged
}

// ReadAt serves the merged view: each range from whichever layer owns it, and zeros
// where no layer does.
//
// An unmapped range is not an error. A sparse image maps only the blocks it holds, and
// a filesystem reads its unallocated blocks expecting zeros.
func (s *lsmtStack) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("image: lsmt stack read at a negative offset")
	}
	if off >= s.virtualSize {
		return 0, io.EOF
	}
	if rem := s.virtualSize - off; int64(len(p)) > rem {
		p = p[:rem]
	}
	for i := range p {
		p[i] = 0
	}

	// The index is in sectors, so the request is widened to sector bounds and the
	// result copied back out of the middle. Requests from a ublk queue are already
	// sector-aligned; a caller that is not still gets a correct answer.
	startSector := uint64(off) / lsmtAlignment
	endSector := (uint64(off) + uint64(len(p)) + lsmtAlignment - 1) / lsmtAlignment

	i := sort.Search(len(s.mappings), func(i int) bool {
		return s.mappings[i].end() > startSector
	})
	for ; i < len(s.mappings); i++ {
		m := s.mappings[i]
		if m.offset >= endSector {
			break
		}
		if m.zeroed {
			// Already zero-filled above.
			continue
		}

		// Clip to the request, in sectors, then convert to bytes.
		from, to := m.offset, m.end()
		moff := m.moffset
		if from < startSector {
			moff += startSector - from
			from = startSector
		}
		if to > endSector {
			to = endSector
		}
		if from >= to {
			continue
		}

		virtByte := int64(from) * lsmtAlignment
		physByte := int64(moff) * lsmtAlignment
		length := int64(to-from) * lsmtAlignment

		// Where this run lands in the caller's buffer. Negative when the run starts
		// before the request, which happens because the query was widened to a sector
		// boundary.
		dstStart := virtByte - off
		if dstStart < 0 {
			physByte += -dstStart
			length -= -dstStart
			dstStart = 0
		}
		if dstStart >= int64(len(p)) || length <= 0 {
			continue
		}
		if dstStart+length > int64(len(p)) {
			length = int64(len(p)) - dstStart
		}

		n, err := s.layers[m.layer].ReadAt(p[dstStart:dstStart+length], physByte)
		if err != nil && !isEOF(err) {
			return 0, fmt.Errorf("image: read layer %d at %d: %w", m.layer, physByte, err)
		}
		// A short read inside a mapped run means the layer is truncated. The remainder
		// stays zero rather than holding whatever the buffer had.
		for j := int64(n); j < length; j++ {
			p[dstStart+j] = 0
		}
	}
	return len(p), nil
}
