//go:build !linux

package reclaim

// NewLinuxHost reports that there is nothing to reconcile off Linux.
//
// Device-mapper and loop devices are Linux facilities, so a node built elsewhere
// cannot have leaked either. Returning nil rather than an error keeps noded
// buildable and testable on a developer's machine: the caller treats a nil host
// as "reconciliation disabled", which is the correct state here.
func NewLinuxHost(baseDir string) Host {
	return nil
}
