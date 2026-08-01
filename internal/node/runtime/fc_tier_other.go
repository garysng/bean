//go:build !linux

package runtime

import (
	"errors"
	"runtime"
)

// NewFCTier reports that microVMs are unavailable. The tier needs KVM, so it is
// Linux-only; development on other platforms uses LocalRuntime, which exercises
// the same agent surface.
func NewFCTier(cfg FCTierConfig) (Runtime, error) {
	return nil, errors.New("fc tier requires linux with KVM (this build is " + runtime.GOOS + ")")
}
