//go:build !linux

package beand

import "errors"

// PivotToRootfs is only meaningful inside a Linux guest. The agent builds on
// other platforms so development and tests run there, where it serves a
// process-level sandbox and has no root to pivot to.
func PivotToRootfs(device string) error {
	return errors.New("beand: --pivot requires linux")
}
