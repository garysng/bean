//go:build linux

package runtime

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// HostCPUIdentity reads the fields a memory snapshot binds to.
//
// It reports the vendor and family but deliberately not the model. A guest
// kernel branches on vendor for errata workarounds and MSR access, and on family
// for a handful of the same, so those genuinely constrain where a memory
// snapshot can be restored. The model does not: masking instruction-set features
// (see cpu_template.go) is what makes a snapshot portable across models, and
// matching on model would throw that away — a snapshot could then only ever
// return to the exact CPU that produced it, which is the problem the template
// exists to solve.
func HostCPUIdentity() (vendor string, family int32, err error) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", 0, fmt.Errorf("read cpu identity: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "vendor_id":
			vendor = value
		case "cpu family":
			if _, err := fmt.Sscanf(value, "%d", &family); err != nil {
				return "", 0, fmt.Errorf("parse cpu family %q: %w", value, err)
			}
		}
		// Only the first processor block matters: a host with mixed CPUs is not
		// something Firecracker supports, so reading further would only re-read
		// the same values.
		if vendor != "" && family != 0 {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return "", 0, fmt.Errorf("scan cpuinfo: %w", err)
	}
	if vendor == "" {
		return "", 0, fmt.Errorf("no vendor_id in /proc/cpuinfo")
	}
	return vendor, family, nil
}
