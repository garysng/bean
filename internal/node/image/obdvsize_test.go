//go:build linux

package image

import "testing"

// TestDeviceIsNeverSmallerThanItsFilesystem pins the sizing rule a guest panic exposed.
//
// The device and the filesystem on it were sized from independent inputs: the writable
// layer from the caller's diskMiB, the base layer from vsizeForImage, which floors at
// 2 GB. A create with diskMiB=512 therefore built a 1 GB device over a 2 GB ext4, and
// the guest kernel refused to mount it -- "EXT4-fs (vdb): bad geometry: block count
// 524288 exceeds size of device (262144 blocks)". The agent's mount returned EINVAL,
// init exited 1, the kernel panicked, and the caller saw only a 20-second agent-health
// timeout.
//
// Checked as arithmetic rather than through a real device because the rule is
// arithmetic: no tcmu, no daemon and no registry are needed to establish that a device
// must not be smaller than the filesystem it presents.
func TestDeviceIsNeverSmallerThanItsFilesystem(t *testing.T) {
	p := &OverlaybdProvider{}

	cases := []struct {
		name    string
		sizeMiB int64
		lowerGB int64
		wantGB  int64
	}{
		// The measured failure.
		{"request below the base floor", 512, 2, 2},
		// A request that already clears the floor keeps its own size: the caller asked
		// for a writable layer that big and the base does not bound it from above.
		{"request above the base floor", 8192, 2, 8},
		// Exactly equal must not be nudged.
		{"request equal to the floor", 2048, 2, 2},
		// A chain that recorded no size imposes no floor, rather than defaulting to
		// one and quietly enlarging every device.
		{"chain with no recorded size", 512, 0, 1},
		// Sub-gigabyte requests still round up to a usable layer.
		{"sub-gigabyte request", 100, 0, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vsizeGB := (tc.sizeMiB + 1023) / 1024
			if lowerGB := p.lowerVsizeGB([]obdLayer{{VsizeGB: tc.lowerGB}}); vsizeGB < lowerGB {
				vsizeGB = lowerGB
			}
			if vsizeGB != tc.wantGB {
				t.Errorf("device sized %d GB over a %d GB filesystem, want %d GB.\n"+
					"A device smaller than its own filesystem fails in the guest, as a "+
					"kernel geometry error the caller never sees",
					vsizeGB, tc.lowerGB, tc.wantGB)
			}
		})
	}
}

// TestVsizeForImageFloorIsWhatTheDeviceMustClear records the coupling, so a change to
// the floor cannot be made without noticing that Prepare depends on it.
func TestVsizeForImageFloorIsWhatTheDeviceMustClear(t *testing.T) {
	p := &OverlaybdProvider{}
	// A tiny image: alpine's single layer is a few MiB, so the computed size is the
	// floor rather than anything derived from the content.
	got := p.vsizeForImage(&Manifest{Layers: []Descriptor{{Size: 3 << 20}}})
	if got < 2 {
		t.Fatalf("vsizeForImage floor is %d GB; Prepare raises the device to this "+
			"figure, so a floor below 1 would stop constraining anything", got)
	}
	if lower := p.lowerVsizeGB([]obdLayer{{VsizeGB: got}}); lower != got {
		t.Errorf("the floor computed for the image (%d GB) is not what reaches "+
			"Prepare (%d GB); the size is recorded on the first layer only", got, lower)
	}
}
