// Package store provides the control-plane state store (SQLite for P0).
// Domain types and state constants live in types.go.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by operations that require an existing record.
var ErrNotFound = errors.New("not found")

// ErrInUse is returned when deleting a record something else still needs.
var ErrInUse = errors.New("in use")

// Event is one lifecycle event; the type strings follow
// sandbox.lifecycle.* (docs/api-design.md §3.8).
type Event struct {
	ID        int64             `json:"-"`
	Type      string            `json:"type"`
	Timestamp time.Time         `json:"timestamp"`
	SandboxID string            `json:"sandboxId"`
	Data      map[string]string `json:"data,omitempty"`
	Version   string            `json:"version"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite single-writer

	// WAL so that another process can read this database while the control plane
	// writes it. bean-proxy resolves placement on the data path, and in the default
	// rollback-journal mode a reader and a writer lock each other out -- which would
	// present as the proxy stalling whenever a sandbox is created.
	//
	// A property of the file rather than of a connection, so it is set once here by
	// the writer and only verified by OpenReadOnly.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL on %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing database for reading only.
//
// Distinct from Open because Open runs migrate(), which is DDL: a second process
// calling it against a database the first process owns attempts schema writes on
// someone else's file. Measured -- bean-proxy calling Open failed with
// "database is locked (SQLITE_BUSY)" and never started, while the log line saying so
// looked like a transient contention problem rather than a wrong call.
//
// Read-only is enforced by SQLite through the URI mode rather than by this package
// declining to write. A reader that could write is one bug away from being a second
// writer to the placement ledger, and the proxy sits on the data path where such a
// write would be least expected.
//
// WAL matters here: without it a reader and a writer on one SQLite file block each
// other, so the proxy would stall on every control-plane write. The writer sets the
// journal mode -- it is a property of the file, not of a connection -- and this
// verifies rather than assumes it, because a rollback-journal database would produce
// exactly the intermittent stalls that get blamed on the network.
func OpenReadOnly(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	// More than one, unlike Open: readers do not serialise against each other under
	// WAL, and the proxy serves concurrent requests.
	db.SetMaxOpenConns(4)

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		db.Close()
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if !strings.EqualFold(mode, "wal") {
		db.Close()
		return nil, fmt.Errorf("%s is in %s journal mode, not WAL: a reader and the "+
			"control plane's writer would block each other on every write", path, mode)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	// Records are stored as JSON blobs with the few query dimensions
	// promoted to columns; this keeps schema churn low while the domain
	// types are still moving.
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS sandboxes (
  id TEXT PRIMARY KEY,
  data TEXT NOT NULL,
  state TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sandbox_id TEXT NOT NULL,
  type TEXT NOT NULL,
  ts INTEGER NOT NULL,
  data TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_sandbox ON events(sandbox_id, id);
CREATE TABLE IF NOT EXISTS snapshots (
  id TEXT PRIMARY KEY,
  data TEXT NOT NULL,
  state TEXT NOT NULL,
  sandbox_id TEXT NOT NULL,
  ref_count INTEGER NOT NULL DEFAULT 0,
  base_id TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_sandbox ON snapshots(sandbox_id);
-- Deleting a snapshot has to find its descendants, since a diff cannot be
-- restored once its base is gone.
CREATE INDEX IF NOT EXISTS idx_snapshots_base ON snapshots(base_id);
CREATE TABLE IF NOT EXISTS images (
  ref TEXT PRIMARY KEY,
  data TEXT NOT NULL,
  state TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  -- Promoted so a caller's own images can be listed without decoding every
  -- blob. Empty means unowned, which reads as "visible to everyone": that is
  -- what an imported public ref is, and what every image from before this
  -- column became.
  owner TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS prewarm_jobs (
  id TEXT PRIMARY KEY,
  data TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS nodes (
  id TEXT PRIMARY KEY,
  region TEXT NOT NULL,
  labels TEXT NOT NULL DEFAULT '{}',
  runtimes TEXT NOT NULL DEFAULT '[]',
  cpu_alloc REAL NOT NULL DEFAULT 0,
  mem_alloc INTEGER NOT NULL DEFAULT 0,
  disk_alloc INTEGER NOT NULL DEFAULT 0,
  gpu_count INTEGER NOT NULL DEFAULT 0,
  cpu_committed REAL NOT NULL DEFAULT 0,
  mem_committed INTEGER NOT NULL DEFAULT 0,
  disk_committed INTEGER NOT NULL DEFAULT 0,
  gpu_committed INTEGER NOT NULL DEFAULT 0,
  create_in_flight INTEGER NOT NULL DEFAULT 0,
  max_creates INTEGER NOT NULL DEFAULT 16,
  cached_images TEXT NOT NULL DEFAULT '{}',
  nvme_cache INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'READY',
  advertise_addr TEXT NOT NULL DEFAULT '',
  last_heartbeat INTEGER NOT NULL DEFAULT 0,
  -- The CPU a node's guests run on, which decides whether a memory snapshot
  -- taken elsewhere can be restored here. Empty means the node has not reported
  -- it, and a restore treats that as "cannot confirm compatible" rather than
  -- as permission.
  cpu_vendor TEXT NOT NULL DEFAULT '',
  cpu_family INTEGER NOT NULL DEFAULT 0,
  cpu_template TEXT NOT NULL DEFAULT '',
  -- What the node measured, as opposed to disk_committed which sums what
  -- sandboxes were promised. A sandbox's disk request is nominal and its layer is
  -- sparse, so the two differ by orders of magnitude. Advisory: placement stays on
  -- the commitment ledger, and the node's own floor is what protects it.
  disk_used_mib INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_nodes_region_state ON nodes(region, state);
-- One reservation per sandbox: the primary key makes Reserve idempotent
-- and lets Release find the exact amounts to give back.
CREATE TABLE IF NOT EXISTS reservations (
  sandbox_id TEXT PRIMARY KEY,
  node_id TEXT NOT NULL,
  cpu REAL NOT NULL,
  mem_mib INTEGER NOT NULL,
  disk_mib INTEGER NOT NULL,
  gpu INTEGER NOT NULL DEFAULT 0,
  spread_key TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reservations_node ON reservations(node_id);
CREATE INDEX IF NOT EXISTS idx_reservations_spread ON reservations(spread_key);
CREATE TABLE IF NOT EXISTS builds (
  id TEXT PRIMARY KEY,
  data TEXT NOT NULL,
  state TEXT NOT NULL,
  tag TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_builds_state ON builds(state);
CREATE TABLE IF NOT EXISTS registry_credentials (
  host TEXT PRIMARY KEY,
  username TEXT NOT NULL,
  secret BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
`)
	if err != nil {
		return err
	}
	return s.addMissingColumns()
}

// addMissingColumns brings an existing database up to the current schema.
//
// CREATE TABLE IF NOT EXISTS does nothing to a table that already exists, so a
// column added to the schema above would be missing from every database created
// before it — and the failure is a scan error at runtime, not at startup.
//
// Records stored as JSON blobs do not need this; only the columns promoted for
// querying do.
func (s *Store) addMissingColumns() error {
	// ALTER TABLE ADD COLUMN fails when the column is already there, and SQLite
	// has no IF NOT EXISTS for it, so a duplicate-column error is the expected
	// outcome on an up-to-date database rather than a problem.
	for _, stmt := range []string{
		`ALTER TABLE nodes ADD COLUMN cpu_vendor TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN cpu_family INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN cpu_template TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE snapshots ADD COLUMN base_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN disk_used_mib INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE images ADD COLUMN owner TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("migrate: %q: %w", stmt, err)
		}
	}
	// Indexes on migrated columns come last: on an old database the column does
	// not exist until the ALTER above runs, and CREATE INDEX would fail.
	if _, err := s.db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_images_owner ON images(owner)`); err != nil {
		return fmt.Errorf("migrate: index images(owner): %w", err)
	}
	return nil
}

// isDuplicateColumn reports SQLite's complaint about an already-present column.
func isDuplicateColumn(err error) bool {
	return strings.Contains(err.Error(), "duplicate column name")
}

// marshalJSON encodes a value for a JSON column, using an empty object or
// array rather than SQL NULL so scans never have to handle nulls.
func marshalJSON(v any) (string, error) {
	if v == nil {
		return "null", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalJSON decodes a JSON column, tolerating empty and null values.
func unmarshalJSON(s string, out any) error {
	if s == "" || s == "null" {
		return nil
	}
	return json.Unmarshal([]byte(s), out)
}

// ---- sandboxes ----

func (s *Store) PutSandbox(sb *Sandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, err := json.Marshal(sb)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO sandboxes(id, data, state, created_at) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET data=excluded.data, state=excluded.state`,
		sb.ID, string(blob), string(sb.State), sb.CreatedAt.Unix())
	return err
}

// GetSandbox returns nil (no error) when the sandbox does not exist.
func (s *Store) GetSandbox(id string) (*Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var blob string
	err := s.db.QueryRow(`SELECT data FROM sandboxes WHERE id=?`, id).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sb Sandbox
	if err := json.Unmarshal([]byte(blob), &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// ListSandboxes filters by label and state; empty filters match everything.
func (s *Store) ListSandboxes(labelKey, labelVal string, state SandboxState) ([]*Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT data FROM sandboxes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		var sb Sandbox
		if err := json.Unmarshal([]byte(blob), &sb); err != nil {
			return nil, err
		}
		if state != "" && sb.State != state {
			continue
		}
		if labelKey != "" && sb.Labels[labelKey] != labelVal {
			continue
		}
		out = append(out, &sb)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSandbox(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM sandboxes WHERE id=?`, id)
	return err
}

// ---- events ----

func (s *Store) AppendEvent(e *Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(e.Data)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO events(sandbox_id, type, ts, data) VALUES(?,?,?,?)`,
		e.SandboxID, e.Type, e.Timestamp.UnixMilli(), string(data))
	return err
}

// ListEvents returns a sandbox's events oldest-first, capped by limit.
func (s *Store) ListEvents(sandboxID string, limit int) ([]*Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, sandbox_id, type, ts, data FROM events WHERE sandbox_id=? ORDER BY id DESC LIMIT ?`,
		sandboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		var ts int64
		var data string
		if err := rows.Scan(&e.ID, &e.SandboxID, &e.Type, &ts, &data); err != nil {
			return nil, err
		}
		e.Timestamp = time.UnixMilli(ts)
		e.Version = "v1"
		if data != "" && data != "null" {
			_ = json.Unmarshal([]byte(data), &e.Data)
		}
		out = append(out, &e)
	}
	// Reverse into chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// ---- snapshots ----

func (s *Store) PutSnapshot(snap *Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	// base_id is duplicated out of the JSON blob into its own column because
	// deletion has to ask "does anything descend from this", which is a query
	// over other rows rather than a field of the row being deleted.
	_, err = s.db.Exec(
		`INSERT INTO snapshots(id, data, state, sandbox_id, ref_count, base_id, created_at) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET data=excluded.data, state=excluded.state, base_id=excluded.base_id`,
		snap.ID, string(blob), string(snap.State), snap.SandboxID, snap.RefCount,
		snap.BaseID, snap.CreatedAt.Unix())
	return err
}

// GetSnapshot returns nil (no error) when the snapshot does not exist.
// RefCount is read from its column, not the JSON blob.
func (s *Store) GetSnapshot(id string) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getSnapshotLocked(id)
}

func (s *Store) getSnapshotLocked(id string) (*Snapshot, error) {
	var blob string
	var refCount int
	err := s.db.QueryRow(`SELECT data, ref_count FROM snapshots WHERE id=?`, id).
		Scan(&blob, &refCount)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(blob), &snap); err != nil {
		return nil, err
	}
	snap.RefCount = refCount
	return &snap, nil
}

func (s *Store) ListSnapshots(labelKey, labelVal string, state SnapshotState) ([]*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT data, ref_count FROM snapshots ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Snapshot
	for rows.Next() {
		var blob string
		var refCount int
		if err := rows.Scan(&blob, &refCount); err != nil {
			return nil, err
		}
		var snap Snapshot
		if err := json.Unmarshal([]byte(blob), &snap); err != nil {
			return nil, err
		}
		snap.RefCount = refCount
		if state != "" && snap.State != state {
			continue
		}
		if labelKey != "" && snap.Labels[labelKey] != labelVal {
			continue
		}
		out = append(out, &snap)
	}
	return out, rows.Err()
}

// SnapshotChain returns the snapshots a restore needs, ordered base-first with
// the requested snapshot last. A self-contained snapshot yields itself alone.
//
// Order is the caller's contract with the merge: a diff's pages overwrite its
// base's, so replaying a chain out of order produces a coherent-looking memory
// image assembled from stale pages. Nothing downstream can detect that, which is
// why the ordering is established here, once, by walking base links.
//
// The ancestors need no reference of their own. A base cannot be deleted while
// anything descends from it (see DeleteSnapshot), so the leaf's own reference
// keeps the whole chain alive for as long as the restore holds it.
func (s *Store) SnapshotChain(id string) ([]*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var chain []*Snapshot
	seen := map[string]bool{}
	for next := id; next != ""; {
		// A cycle cannot arise from the write path, which only ever points at an
		// existing snapshot. Detecting one anyway keeps a corrupted row from
		// hanging the restore instead of failing it.
		if seen[next] {
			return nil, fmt.Errorf("snapshot chain from %s cycles at %s", id, next)
		}
		seen[next] = true

		snap, err := s.getSnapshotLocked(next)
		if err != nil {
			return nil, err
		}
		if snap == nil {
			// Reached by a diff whose base is gone. DeleteSnapshot prevents this,
			// so it means the records were damaged some other way; either way the
			// guest cannot be reconstructed.
			return nil, fmt.Errorf("%w: snapshot %s needs base %s, which is missing",
				ErrNotFound, id, next)
		}
		// Prepended: the walk goes leaf to root, and the merge needs root to leaf.
		chain = append([]*Snapshot{snap}, chain...)
		next = snap.BaseID
	}
	return chain, nil
}

// AcquireSnapshot increments the reference count so an in-progress restore
// cannot have its source deleted. Returns the snapshot for convenience.
func (s *Store) AcquireSnapshot(id string) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.getSnapshotLocked(id)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, ErrNotFound
	}
	if snap.State != SnapshotReady {
		return nil, fmt.Errorf("snapshot %s is %s, not %s", id, snap.State, SnapshotReady)
	}
	if _, err := s.db.Exec(`UPDATE snapshots SET ref_count = ref_count + 1 WHERE id=?`, id); err != nil {
		return nil, err
	}
	snap.RefCount++
	return snap, nil
}

// ReleaseSnapshot decrements the reference count, never below zero.
func (s *Store) ReleaseSnapshot(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE snapshots SET ref_count = MAX(ref_count - 1, 0) WHERE id=?`, id)
	return err
}

// DeleteSnapshot refuses while restores still reference the snapshot.
func (s *Store) DeleteSnapshot(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := s.getSnapshotLocked(id)
	if err != nil {
		return err
	}
	if snap == nil {
		return ErrNotFound
	}
	if snap.RefCount > 0 {
		return fmt.Errorf("%w: snapshot %s has %d active restore(s)", ErrInUse, id, snap.RefCount)
	}
	// A diff holds only what changed since its base, so deleting the base leaves
	// its descendants unrestorable — they would fail at merge time, long after
	// the deletion that caused it, on a different machine. Refusing here is the
	// only point where the cause is still visible.
	//
	// This reuses ErrInUse rather than adding an error: to a caller both mean
	// "something still needs this", and both map to the same 409.
	var children int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM snapshots WHERE base_id=?`, id).Scan(&children); err != nil {
		return fmt.Errorf("count snapshot descendants: %w", err)
	}
	if children > 0 {
		return fmt.Errorf("%w: snapshot %s is the base of %d incremental snapshot(s)",
			ErrInUse, id, children)
	}
	_, err = s.db.Exec(`DELETE FROM snapshots WHERE id=?`, id)
	return err
}

// ---- images ----

func (s *Store) PutImage(img *Image) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	img.UpdatedAt = time.Now()
	blob, err := json.Marshal(img)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO images(ref, data, state, updated_at, owner) VALUES(?,?,?,?,?)
		 ON CONFLICT(ref) DO UPDATE SET data=excluded.data, state=excluded.state,
		   updated_at=excluded.updated_at, owner=excluded.owner`,
		img.Ref, string(blob), string(img.State), img.UpdatedAt.Unix(), img.Owner)
	return err
}

// GetImage returns nil (no error) when the image is not registered.
func (s *Store) GetImage(ref string) (*Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var blob string
	err := s.db.QueryRow(`SELECT data FROM images WHERE ref=?`, ref).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var img Image
	if err := json.Unmarshal([]byte(blob), &img); err != nil {
		return nil, err
	}
	return &img, nil
}

// ListImages returns images most recently updated first.
//
// An empty owner lists everything, which is the operator's view and the
// behaviour of every deployment that has no identity source. A non-empty owner
// lists that owner's images together with the unowned ones, because unowned
// means visible to everyone: excluding them would make an upgraded deployment
// look like it had lost the base images it is still perfectly able to run.
func (s *Store) ListImages(owner string) ([]*Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT data FROM images ORDER BY updated_at DESC`
	args := []any{}
	if owner != "" {
		query = `SELECT data FROM images WHERE owner=? OR owner=''
		         ORDER BY updated_at DESC`
		args = append(args, owner)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Image
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		var img Image
		if err := json.Unmarshal([]byte(blob), &img); err != nil {
			return nil, err
		}
		out = append(out, &img)
	}
	return out, rows.Err()
}

func (s *Store) DeleteImage(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM images WHERE ref=?`, ref)
	return err
}

// ---- builds ----

func (s *Store) PutBuild(b *ImageBuild) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b.UpdatedAt = time.Now()
	blob, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tag := ""
	if b.Plan != nil {
		tag = b.Plan.Tag
	}
	_, err = s.db.Exec(
		`INSERT INTO builds(id, data, state, tag, created_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET data=excluded.data, state=excluded.state`,
		b.ID, string(blob), string(b.State), tag, b.CreatedAt.Unix())
	return err
}

// GetBuild returns nil (no error) when the build does not exist.
func (s *Store) GetBuild(id string) (*ImageBuild, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var blob string
	err := s.db.QueryRow(`SELECT data FROM builds WHERE id=?`, id).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b ImageBuild
	if err := json.Unmarshal([]byte(blob), &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// ListBuilds returns builds newest first, optionally filtered by state.
func (s *Store) ListBuilds(state BuildState) ([]*ImageBuild, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT data FROM builds ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ImageBuild
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		var b ImageBuild
		if err := json.Unmarshal([]byte(blob), &b); err != nil {
			return nil, err
		}
		if state != "" && b.State != state {
			continue
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// ---- registry credentials ----
//
// Secrets are stored as ciphertext produced by the caller (see
// internal/control/secret). The store never sees plaintext, so a database
// dump alone cannot be used to pull private images.

func (s *Store) PutRegistryCredential(c *RegistryCredential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Host == "" {
		return fmt.Errorf("registry host required")
	}
	if len(c.SecretCiphertext) == 0 {
		return fmt.Errorf("secret ciphertext required")
	}
	now := time.Now()
	c.UpdatedAt = now
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	_, err := s.db.Exec(
		`INSERT INTO registry_credentials(host, username, secret, created_at, updated_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(host) DO UPDATE SET username=excluded.username,
		   secret=excluded.secret, updated_at=excluded.updated_at`,
		c.Host, c.Username, c.SecretCiphertext, c.CreatedAt.Unix(), c.UpdatedAt.Unix())
	return err
}

// GetRegistryCredential returns nil (no error) when the host has no
// credential, which means anonymous pulls.
func (s *Store) GetRegistryCredential(host string) (*RegistryCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c RegistryCredential
	var created, updated int64
	err := s.db.QueryRow(
		`SELECT host, username, secret, created_at, updated_at
		 FROM registry_credentials WHERE host=?`, host).
		Scan(&c.Host, &c.Username, &c.SecretCiphertext, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt = time.Unix(created, 0)
	c.UpdatedAt = time.Unix(updated, 0)
	return &c, nil
}

// ListRegistryCredentials returns credentials without their ciphertext, so
// the result is safe to serialise into an API response.
func (s *Store) ListRegistryCredentials() ([]*RegistryCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT host, username, created_at, updated_at FROM registry_credentials ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RegistryCredential
	for rows.Next() {
		var c RegistryCredential
		var created, updated int64
		if err := rows.Scan(&c.Host, &c.Username, &created, &updated); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(created, 0)
		c.UpdatedAt = time.Unix(updated, 0)
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteRegistryCredential(host string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM registry_credentials WHERE host=?`, host)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- prewarm jobs ----

func (s *Store) PutPrewarmJob(job *PrewarmJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO prewarm_jobs(id, data, created_at) VALUES(?,?,?)
		 ON CONFLICT(id) DO UPDATE SET data=excluded.data`,
		job.ID, string(blob), job.CreatedAt.Unix())
	return err
}

// GetPrewarmJob returns nil (no error) when the job does not exist.
func (s *Store) GetPrewarmJob(id string) (*PrewarmJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var blob string
	err := s.db.QueryRow(`SELECT data FROM prewarm_jobs WHERE id=?`, id).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var job PrewarmJob
	if err := json.Unmarshal([]byte(blob), &job); err != nil {
		return nil, err
	}
	return &job, nil
}
