package image

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Concurrent reads and writes of one backend do not race.
//
// Reads used to be serialised by the queue's single thread, so the copy-on-write bitmap and
// the overlay needed no lock. Handing slow reads to workers removed that guarantee: several
// requests now touch `owned` and the overlay at once, and a torn bitmap serves a block from
// the wrong source -- which surfaces as `EXT4-fs error: reading directory` and a virtio
// `I/O error`, not as anything that looks like a data race.
func TestBackendIsSafeUnderConcurrentIO(t *testing.T) {
	dir := t.TempDir()
	path := writeCountedExtentLayer(t, dir, "race.lsmt", 128, 64)
	b, err := newLSMTBackend([]string{path}, filepath.Join(dir, "o.img"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_ = os.Getpid()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := make([]byte, 4096)
			for i := 0; i < 200; i++ {
				off := int64((w*200+i)%1024) * 4096
				if i%3 == 0 {
					if _, err := b.WriteAt(buf, off); err != nil {
						t.Errorf("write at %d: %v", off, err)
						return
					}
					continue
				}
				if _, err := b.ReadAt(buf, off); err != nil {
					t.Errorf("read at %d: %v", off, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	_ = context.Background()
}
