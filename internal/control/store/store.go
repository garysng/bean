// Package store provides the control-plane state store (SQLite for P0).
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type SandboxRecord struct {
	ID           string            `json:"id"`
	Image        string            `json:"image"`
	State        string            `json:"state"`
	Reason       string            `json:"reason,omitempty"`
	NodeID       string            `json:"nodeId"`
	CPU          float64           `json:"cpu"`
	MemoryMiB    int64             `json:"memoryMiB"`
	DiskMiB      int64             `json:"diskMiB"`
	Labels       map[string]string `json:"labels,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
	IdleTimeout  *int64            `json:"idleTimeoutSeconds,omitempty"`
	OnIdle       string            `json:"onIdle,omitempty"`
	LastActivity time.Time         `json:"lastActivityAt"`
}

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
`)
	return err
}

func (s *Store) PutSandbox(r *SandboxRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO sandboxes(id, data, state, created_at) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET data=excluded.data, state=excluded.state`,
		r.ID, string(blob), r.State, r.CreatedAt.Unix())
	return err
}

func (s *Store) GetSandbox(id string) (*SandboxRecord, error) {
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
	var r SandboxRecord
	if err := json.Unmarshal([]byte(blob), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) ListSandboxes(labelKey, labelVal, state string) ([]*SandboxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT data FROM sandboxes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SandboxRecord
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		var r SandboxRecord
		if err := json.Unmarshal([]byte(blob), &r); err != nil {
			return nil, err
		}
		if state != "" && r.State != state {
			continue
		}
		if labelKey != "" && r.Labels[labelKey] != labelVal {
			continue
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSandbox(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM sandboxes WHERE id=?`, id)
	return err
}

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
	// reverse to chronological
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

var ErrNotFound = fmt.Errorf("not found")
