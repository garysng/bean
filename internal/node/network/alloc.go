package network

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// The pool keeps no authoritative state of its own. It is rebuilt by asking the
// host which namespaces exist.
//
// That is not a stylistic preference. A reference count living in process memory
// is lost on restart while the objects it counted are still on the host, and the
// consequence here is worse than the loop-device leak that taught us this
// (GitHub #16): reassigning an index that is still in use gives two sandboxes the
// same veth addresses, and the symptom is intermittent connectivity on both.
//
// So the host is the single authority, the same principle that has nodes report
// their own image cache rather than the control plane inferring it.

// Lister reports the namespaces present on the host. It is an interface so the
// allocator can be tested without creating namespaces.
type Lister interface {
	ListNamespaces() ([]string, error)
}

// Allocator hands out slots, refusing any the host already has.
type Allocator struct {
	// GuestCIDR is the subnet every guest sees. Identical across sandboxes.
	GuestCIDR string
	// Host lists existing namespaces so the pool can be rebuilt.
	Host Lister

	mu sync.Mutex
	// taken is a cache of what has been handed out, not the authority. Reserve
	// consults the host as well, so a stale cache cannot cause a double
	// assignment — only a slower search.
	taken map[int]string
}

// NewAllocator builds a pool over a guest subnet.
func NewAllocator(guestCIDR string, host Lister) *Allocator {
	return &Allocator{GuestCIDR: guestCIDR, Host: host, taken: map[int]string{}}
}

// Reserve assigns the lowest free slot to a sandbox.
//
// The host is consulted on every call rather than once at startup. Namespaces can
// appear from a previous incarnation of this process or from a concurrent
// operator action, and an index that looks free in memory but exists on the host
// is exactly the collision this must not produce.
func (a *Allocator) Reserve(sandboxID string) (*Layout, error) {
	if sandboxID == "" {
		return nil, fmt.Errorf("network: sandbox id required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	for idx, owner := range a.taken {
		if owner == sandboxID {
			// Already assigned. Returning the existing layout keeps a retried create
			// idempotent instead of leaking a second namespace for one sandbox.
			return LayoutFor(idx, a.GuestCIDR)
		}
	}

	onHost, err := a.hostIndices()
	if err != nil {
		return nil, err
	}

	for idx := 0; idx <= MaxIndex; idx++ {
		if _, ok := a.taken[idx]; ok {
			continue
		}
		if onHost[idx] {
			// Present on the host but unknown to this process: adopted rather than
			// reused. It may be serving a sandbox that predates this process, and the
			// cost of assuming otherwise is two sandboxes sharing addresses.
			a.taken[idx] = ""
			continue
		}
		layout, err := LayoutFor(idx, a.GuestCIDR)
		if err != nil {
			return nil, err
		}
		a.taken[idx] = sandboxID
		return layout, nil
	}
	return nil, fmt.Errorf("network: no free slot below %d; every namespace is in use, "+
		"which means sandbox accounting has let more onto this node than it can "+
		"address", MaxIndex)
}

// Release returns a slot. It is idempotent: teardown runs on more than one path,
// and a second release must not free a slot somebody else now holds.
func (a *Allocator) Release(sandboxID string) {
	if sandboxID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for idx, owner := range a.taken {
		if owner == sandboxID {
			delete(a.taken, idx)
			return
		}
	}
}

// LayoutOf returns a sandbox's assigned layout, or false if it has none.
func (a *Allocator) LayoutOf(sandboxID string) (*Layout, bool) {
	if sandboxID == "" {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for idx, owner := range a.taken {
		if owner == sandboxID {
			layout, err := LayoutFor(idx, a.GuestCIDR)
			if err != nil {
				return nil, false
			}
			return layout, true
		}
	}
	return nil, false
}

// hostIndices reads the namespaces the host holds.
func (a *Allocator) hostIndices() (map[int]bool, error) {
	if a.Host == nil {
		return nil, nil
	}
	names, err := a.Host.ListNamespaces()
	if err != nil {
		// Refuse rather than guess. Handing out an index without knowing what the
		// host holds is how two sandboxes end up sharing addresses, and that failure
		// is intermittent and hard to attribute — much worse than a failed create.
		return nil, fmt.Errorf("network: cannot list namespaces, so an index cannot "+
			"be assigned safely: %w", err)
	}
	out := map[int]bool{}
	for _, name := range names {
		if idx, ok := indexOfNetns(name); ok {
			out[idx] = true
		}
	}
	return out, nil
}

// indexOfNetns recovers a slot from a namespace name.
//
// Only bean's own prefix is recognised. A namespace belonging to something else on
// a shared host must be invisible to this pool: counting it would shrink the pool
// for no reason, and worse, a teardown that matched it could destroy another
// workload's networking.
func indexOfNetns(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, netnsPrefix)
	if !ok {
		return 0, false
	}
	idx, err := strconv.Atoi(rest)
	if err != nil || idx < 0 || idx > MaxIndex {
		return 0, false
	}
	return idx, true
}
