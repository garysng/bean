//go:build linux

package runtime

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// BootLogTail returns the tail of a sandbox's guest console.
//
// The console is the only place a boot failure explains itself. Everything noded
// observes from the outside collapses to the same symptom -- the agent never answered
// -- whether the kernel found no root device, the agent rejected its own arguments,
// the vsock device was misconfigured, or the guest is merely slow. The distinguishing
// evidence is written by the guest to this file and nowhere else.
//
// Filtering is deliberate rather than tidy. A successful boot writes a few hundred
// lines of kernel initialisation, so the last N lines of a *failed* boot are usually
// the panic backtrace: register dumps and call frames, below the one line that says
// what actually went wrong. Selecting for that line and keeping the raw tail as a
// fallback is what makes the message useful in the error rather than a wall to scroll
// past.
func (r *FCRuntime) BootLogTail(id string, lines int) string {
	r.mu.Lock()
	vm := r.vms[id]
	r.mu.Unlock()

	dir := ""
	if vm != nil {
		dir = vm.dir
	} else {
		// A VM that failed early may already be out of the map, but its directory is
		// derived from the sandbox id and is still on disk until cleanup runs. This is
		// the case that matters most: the earlier the failure, the less noded knows.
		dir = filepath.Join(r.BaseDir, id)
	}

	f, err := os.Open(filepath.Join(dir, "console.log"))
	if err != nil {
		return ""
	}
	defer f.Close()

	// Bounded because a guest that boot-loops can write without limit, and this runs
	// on a failure path where the caller is about to log the result.
	const maxScan = 512 << 10
	if fi, statErr := f.Stat(); statErr == nil && fi.Size() > maxScan {
		if _, seekErr := f.Seek(fi.Size()-maxScan, 0); seekErr != nil {
			return ""
		}
	}

	var tail []string
	var salient []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 256<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		tail = append(tail, line)
		if len(tail) > lines {
			tail = tail[1:]
		}
		if isSalientBootLine(line) {
			salient = append(salient, line)
			if len(salient) > lines {
				salient = salient[1:]
			}
		}
	}

	if len(salient) > 0 {
		return strings.Join(salient, "; ")
	}
	return strings.Join(tail, "; ")
}

// isSalientBootLine reports whether a console line is likely to be the cause of a
// failed boot rather than part of the aftermath.
//
// The two Go-runtime patterns are here because the agent is a Go program running as
// init: when it rejects its own arguments it prints "flag provided but not defined"
// and exits, and the kernel's response is a panic that buries it. That exact
// sequence is what motivated this file.
func isSalientBootLine(line string) bool {
	for _, marker := range []string{
		"Kernel panic",
		"not syncing",
		"Attempted to kill init",
		"flag provided but not defined",
		"unknown flag",
		"Unable to mount root",
		"No filesystem could mount root",
		"VFS: Cannot open root device",
		"exec of init",
		"Failed to execute",
		"panic:",
	} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}
