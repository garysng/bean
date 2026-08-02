//go:build !linux

package runtime

// HostCPUIdentity reports nothing off Linux.
//
// The microVM tier only runs on Linux, so a node built elsewhere has no memory
// snapshots to constrain. Returning empty rather than an error keeps noded
// buildable and testable on a developer's machine, where the local tier is what
// runs.
func HostCPUIdentity() (vendor string, family int32, err error) {
	return "", 0, nil
}
