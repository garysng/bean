// Package store provides the control-plane state store (SQLite for P0).
// Domain types and state constants live in types.go.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
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
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_sandbox ON snapshots(sandbox_id);
CREATE TABLE IF NOT EXISTS images (
  ref TEXT PRIMARY KEY,
  data TEXT NOT NULL,
  state TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS prewarm_jobs (
  id TEXT PRIMARY KEY,
  data TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS registry_credentials (
  host TEXT PRIMARY KEY,
  username TEXT NOT NULL,
  secret BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
`)
	return err
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
	_, err = s.db.Exec(
		`INSERT INTO snapshots(id, data, state, sandbox_id, ref_count, created_at) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET data=excluded.data, state=excluded.state`,
		snap.ID, string(blob), string(snap.State), snap.SandboxID, snap.RefCount,
		snap.CreatedAt.Unix())
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
		`INSERT INTO images(ref, data, state, updated_at) VALUES(?,?,?,?)
		 ON CONFLICT(ref) DO UPDATE SET data=excluded.data, state=excluded.state,
		   updated_at=excluded.updated_at`,
		img.Ref, string(blob), string(img.State), img.UpdatedAt.Unix())
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

func (s *Store) ListImages() ([]*Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT data FROM images ORDER BY updated_at DESC`)
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
