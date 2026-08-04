package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Placement accounting lives in the database rather than in a scheduler's
// memory. That is what allows more than one gateway replica to run: two
// replicas reading the same free capacity and both placing a sandbox would
// oversell a node, so the commit is done as a conditional update inside a
// transaction and the database arbitrates.
//
// A node's row is the single source of truth for what has been promised to
// it. A scheduler's in-memory view is a cache that can be rebuilt from
// here at any time (see LoadNodes).

// ErrCapacityChanged reports that a node's capacity was taken by someone
// else between scoring and committing. Callers retry with fresh state.
var ErrCapacityChanged = errors.New("node capacity changed")

// NodeRecord is a node's persisted capacity and liveness.
type NodeRecord struct {
	ID       string            `json:"id"`
	Region   string            `json:"region"`
	Labels   map[string]string `json:"labels,omitempty"`
	Runtimes []string          `json:"runtimes,omitempty"`

	// Allocatable already includes the overcommit factor.
	CPUAllocatable    float64 `json:"cpuAllocatable"`
	MemoryAllocateMiB int64   `json:"memoryAllocatableMiB"`
	DiskAllocateMiB   int64   `json:"diskAllocatableMiB"`
	GPUCount          int32   `json:"gpuCount"`

	// Committed is the sum of specs placed here, which is what admission
	// checks against — not live usage, so a burst cannot oversell.
	CPUCommitted    float64 `json:"cpuCommitted"`
	MemoryCommitMiB int64   `json:"memoryCommittedMiB"`
	DiskCommitMiB   int64   `json:"diskCommittedMiB"`
	GPUCommitted    int32   `json:"gpuCommitted"`

	// DiskUsedMiB is what the node measured on its own filesystem, as opposed to
	// DiskCommitMiB which sums what sandboxes were promised. The two differ by
	// orders of magnitude because a sandbox's disk request is nominal while its
	// layer is sparse — a 20 GiB request costing 44 KiB.
	//
	// Reported, not used for admission. Placement stays on the commitment ledger,
	// which cannot be oversold by a burst; this exists so the gap is visible before
	// it becomes an incident, and because the node's own low-disk floor is the
	// thing that actually protects it (see node.DiskGuard).
	DiskUsedMiB int64 `json:"diskUsedMiB"`

	// CreateInFlight bounds concurrent creates so a burst becomes a
	// pipeline instead of a stampede.
	CreateInFlight int `json:"createInFlight"`
	MaxCreates     int `json:"maxCreates"`

	// CachedImages maps image ref to what the node reported about it: size for
	// affinity scoring, digest for finding a warm snapshot.
	//
	// Reported by UpdateNodeStatus, not by the heartbeat. The node is the authority
	// on its own caches.
	CachedImages map[string]CachedImage `json:"cachedImages,omitempty"`
	NVMeCache    bool                   `json:"nvmeCache"`

	// CPUVendor, CPUFamily and CPUTemplate decide where a memory snapshot may be
	// restored. A guest kernel branches on vendor and family, and no template can
	// hide either, so restoring across them corrupts the guest instead of failing
	// cleanly. The model is not recorded on purpose: masking instruction-set
	// features is what buys portability across models.
	CPUVendor   string `json:"cpuVendor,omitempty"`
	CPUFamily   int32  `json:"cpuFamily,omitempty"`
	CPUTemplate string `json:"cpuTemplate,omitempty"`

	State         string    `json:"state"`
	AdvertiseAddr string    `json:"advertiseAddr,omitempty"`
	LastHeartbeat time.Time `json:"lastHeartbeat"`
}

// Reservation is the resource claim a placement makes.
type Reservation struct {
	SandboxID string
	CPU       float64
	MemoryMiB int64
	DiskMiB   int64
	GPU       int32
	// SpreadKey groups related sandboxes so placements can be spread.
	SpreadKey string
}

// UpsertNode registers or refreshes a node, preserving committed
// accounting: a node that re-registers after a restart must not appear
// empty, or the scheduler would oversell it.
func (s *Store) UpsertNode(n *NodeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	labels, err := marshalJSON(n.Labels)
	if err != nil {
		return err
	}
	runtimes, err := marshalJSON(n.Runtimes)
	if err != nil {
		return err
	}
	cached, err := marshalJSON(n.CachedImages)
	if err != nil {
		return err
	}
	if n.MaxCreates <= 0 {
		n.MaxCreates = 16
	}
	_, err = s.db.Exec(`
INSERT INTO nodes(id, region, labels, runtimes, cpu_alloc, mem_alloc, disk_alloc, gpu_count,
                  max_creates, cached_images, nvme_cache, state, advertise_addr, last_heartbeat,
                  cpu_vendor, cpu_family, cpu_template)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  region=excluded.region, labels=excluded.labels, runtimes=excluded.runtimes,
  cpu_alloc=excluded.cpu_alloc, mem_alloc=excluded.mem_alloc,
  disk_alloc=excluded.disk_alloc, gpu_count=excluded.gpu_count,
  max_creates=excluded.max_creates, cached_images=excluded.cached_images,
  nvme_cache=excluded.nvme_cache, state=excluded.state,
  advertise_addr=excluded.advertise_addr, last_heartbeat=excluded.last_heartbeat,
  cpu_vendor=excluded.cpu_vendor, cpu_family=excluded.cpu_family,
  cpu_template=excluded.cpu_template`,
		n.ID, n.Region, labels, runtimes, n.CPUAllocatable, n.MemoryAllocateMiB,
		n.DiskAllocateMiB, n.GPUCount, n.MaxCreates, cached, n.NVMeCache,
		n.State, n.AdvertiseAddr, n.LastHeartbeat.UnixMilli(),
		n.CPUVendor, n.CPUFamily, n.CPUTemplate)
	return err
}

// LoadNodes returns every node with its current accounting, which is how a
// scheduler rebuilds its view after a restart.
func (s *Store) LoadNodes() ([]*NodeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
SELECT id, region, labels, runtimes, cpu_alloc, mem_alloc, disk_alloc, gpu_count,
       cpu_committed, mem_committed, disk_committed, gpu_committed,
       create_in_flight, max_creates, cached_images, nvme_cache, state,
       advertise_addr, last_heartbeat,
       cpu_vendor, cpu_family, cpu_template, disk_used_mib
FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NodeRecord
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetNode returns one node, or nil when it is unknown.
func (s *Store) GetNode(id string) (*NodeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`
SELECT id, region, labels, runtimes, cpu_alloc, mem_alloc, disk_alloc, gpu_count,
       cpu_committed, mem_committed, disk_committed, gpu_committed,
       create_in_flight, max_creates, cached_images, nvme_cache, state,
       advertise_addr, last_heartbeat,
       cpu_vendor, cpu_family, cpu_template, disk_used_mib
FROM nodes WHERE id=?`, id)
	n, err := scanNode(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return n, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanNode(sc rowScanner) (*NodeRecord, error) {
	var n NodeRecord
	var labels, runtimes, cached string
	var hb int64
	if err := sc.Scan(&n.ID, &n.Region, &labels, &runtimes,
		&n.CPUAllocatable, &n.MemoryAllocateMiB, &n.DiskAllocateMiB, &n.GPUCount,
		&n.CPUCommitted, &n.MemoryCommitMiB, &n.DiskCommitMiB, &n.GPUCommitted,
		&n.CreateInFlight, &n.MaxCreates, &cached, &n.NVMeCache, &n.State,
		&n.AdvertiseAddr, &hb,
		&n.CPUVendor, &n.CPUFamily, &n.CPUTemplate, &n.DiskUsedMiB); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(labels, &n.Labels); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(runtimes, &n.Runtimes); err != nil {
		return nil, err
	}
	// Rows written before the value carried a digest hold a bare number where an
	// object is now expected, so a failure to decode is repaired rather than
	// returned. Refusing would make every node written by an older control plane
	// unloadable, which takes the whole cluster's placement down on upgrade; the
	// worst case here is one node's affinity being empty until its next report,
	// which arrives within the status interval.
	if err := unmarshalJSON(cached, &n.CachedImages); err != nil {
		var sizes map[string]int64
		if err2 := unmarshalJSON(cached, &sizes); err2 != nil {
			return nil, err
		}
		n.CachedImages = make(map[string]CachedImage, len(sizes))
		for ref, size := range sizes {
			// No digest: this row predates the field. Left empty rather than guessed,
			// so a warm-snapshot lookup misses and boots instead of matching on
			// something invented here.
			n.CachedImages[ref] = CachedImage{SizeBytes: size}
		}
	}
	n.LastHeartbeat = time.UnixMilli(hb)
	return &n, nil
}

// Reserve commits a reservation against a node if — and only if — the node
// still has room. The check and the update happen in one statement, so two
// replicas racing on the same node cannot both succeed: the loser sees
// ErrCapacityChanged and re-scores against fresh state.
func (s *Store) Reserve(nodeID string, res *Reservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.Exec(`
UPDATE nodes SET
  cpu_committed = cpu_committed + ?,
  mem_committed = mem_committed + ?,
  disk_committed = disk_committed + ?,
  gpu_committed = gpu_committed + ?,
  create_in_flight = create_in_flight + 1
WHERE id = ?
  AND state = 'READY'
  AND cpu_committed + ? <= cpu_alloc
  AND mem_committed + ? <= mem_alloc
  AND disk_committed + ? <= disk_alloc
  AND gpu_committed + ? <= gpu_count
  AND (max_creates <= 0 OR create_in_flight < max_creates)`,
		res.CPU, res.MemoryMiB, res.DiskMiB, res.GPU, nodeID,
		res.CPU, res.MemoryMiB, res.DiskMiB, res.GPU)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: node %s no longer fits the request", ErrCapacityChanged, nodeID)
	}

	// Recording the reservation lets Release be idempotent and lets a
	// reconcile detect reservations whose sandbox no longer exists.
	if _, err := tx.Exec(`
INSERT INTO reservations(sandbox_id, node_id, cpu, mem_mib, disk_mib, gpu, spread_key, created_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(sandbox_id) DO NOTHING`,
		res.SandboxID, nodeID, res.CPU, res.MemoryMiB, res.DiskMiB, res.GPU,
		res.SpreadKey, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// Release returns a reservation's capacity. It is idempotent: releasing an
// already-released reservation is a no-op, so cleanup paths and retries do
// not have to coordinate.
func (s *Store) Release(sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var nodeID string
	var cpu float64
	var mem, disk int64
	var gpu int32
	err = tx.QueryRow(`
SELECT node_id, cpu, mem_mib, disk_mib, gpu FROM reservations WHERE sandbox_id=?`,
		sandboxID).Scan(&nodeID, &cpu, &mem, &disk, &gpu)
	if err == sql.ErrNoRows {
		return nil // already released
	}
	if err != nil {
		return err
	}

	// MAX(...) guards against a committed value drifting negative if a
	// node row were ever rewritten out from under a reservation.
	if _, err := tx.Exec(`
UPDATE nodes SET
  cpu_committed = MAX(cpu_committed - ?, 0),
  mem_committed = MAX(mem_committed - ?, 0),
  disk_committed = MAX(disk_committed - ?, 0),
  gpu_committed = MAX(gpu_committed - ?, 0)
WHERE id = ?`, cpu, mem, disk, gpu, nodeID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM reservations WHERE sandbox_id=?`, sandboxID); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishCreate clears the in-flight marker once a create settles, whether
// it succeeded or failed. The reservation itself stays until Release.
func (s *Store) FinishCreate(nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE nodes SET create_in_flight = MAX(create_in_flight - 1, 0) WHERE id=?`, nodeID)
	return err
}

// SpreadCounts returns how many sandboxes of a spread group sit on each
// node, which is what anti-affinity scoring needs.
func (s *Store) SpreadCounts(spreadKey string) (map[string]int, error) {
	if spreadKey == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT node_id, COUNT(*) FROM reservations WHERE spread_key=? GROUP BY node_id`,
		spreadKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var node string
		var n int
		if err := rows.Scan(&node, &n); err != nil {
			return nil, err
		}
		out[node] = n
	}
	return out, rows.Err()
}

// SetNodeState updates liveness. Returns true when the state actually
// changed, so a caller can act exactly once on a transition (for example,
// marking sandboxes lost when a node's lease expires) even with several
// replicas sweeping concurrently.
func (s *Store) SetNodeState(nodeID, state string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE nodes SET state=? WHERE id=? AND state!=?`, state, nodeID, state)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// CachedImage is what a node reported about one image it holds.
type CachedImage struct {
	// SizeBytes drives the scheduler's image-affinity term.
	SizeBytes int64 `json:"sizeBytes"`
	// Digest identifies the content independently of the tag it arrived under, and
	// is empty for an image with no manifest or one reported before this existed.
	//
	// It is what a warm snapshot must be keyed on: a tag that has moved names
	// different content, and serving a snapshot captured from the old content
	// restores successfully into the wrong environment.
	Digest string `json:"digest,omitempty"`
	// Warm reports that the node holds a warm snapshot for this image, so a create
	// placed there restores instead of booting a kernel -- measured at 0.13 s of
	// host CPU against 0.62 s. The scheduler scores on it.
	//
	// Reported by the node and never inferred here. The node is the only party that
	// knows whether the bundle is on its disk and whether it matches its own CPU,
	// and a control plane that guessed would send work to a node that then boots.
	Warm bool `json:"warm,omitempty"`
}

// PutNodeImages replaces a node's image inventory.
//
// A full replacement rather than a merge, because the node sends its complete set
// and is the authority on it: merging would keep an image the node has since
// evicted, and the scheduler would keep sending work to a node that has to pull.
//
// Separate from TouchNode so a lease renewal and an inventory report cannot
// interfere. They used to share the heartbeat, which meant this JSON blob was
// re-serialised and rewritten every few seconds for a value that rarely changes.
func (s *Store) PutNodeImages(nodeID string, images map[string]CachedImage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cached, err := marshalJSON(images)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE nodes SET cached_images=? WHERE id=?`, cached, nodeID)
	return err
}

// TouchNode records a heartbeat and clears a non-terminal doubt state.
// diskUsedMiB is recorded from the same heartbeat, and a zero is written through
// rather than skipped: a node that stops being able to measure itself should read
// as "not reported" instead of holding its last known figure forever.
//
// The image inventory is deliberately not here; see PutNodeImages.
func (s *Store) TouchNode(nodeID string, diskUsedMiB int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
UPDATE nodes SET last_heartbeat=?, disk_used_mib=?,
  state = CASE WHEN state IN ('SUSPECT','LOST') THEN 'READY' ELSE state END
WHERE id=?`, time.Now().UnixMilli(), diskUsedMiB, nodeID)
	return err
}

// StaleNodes returns nodes whose last heartbeat is older than the cutoff,
// excluding those already in the given state.
func (s *Store) StaleNodes(olderThan time.Time, excludeStates ...string) ([]*NodeRecord, error) {
	nodes, err := s.LoadNodes()
	if err != nil {
		return nil, err
	}
	excluded := map[string]bool{}
	for _, st := range excludeStates {
		excluded[st] = true
	}
	var out []*NodeRecord
	for _, n := range nodes {
		if excluded[n.State] {
			continue
		}
		if n.LastHeartbeat.Before(olderThan) {
			out = append(out, n)
		}
	}
	return out, nil
}

// OrphanReservations returns reservations whose sandbox is gone or
// terminal, so their capacity can be reclaimed. Without this, a gateway
// that dies mid-create would leak capacity permanently.
func (s *Store) OrphanReservations() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
SELECT r.sandbox_id, s.state
FROM reservations r
LEFT JOIN sandboxes s ON s.id = r.sandbox_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		var state sql.NullString
		if err := rows.Scan(&id, &state); err != nil {
			return nil, err
		}
		// No sandbox row, or one that will never run again.
		if !state.Valid || IsTerminal(SandboxState(state.String)) {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}
