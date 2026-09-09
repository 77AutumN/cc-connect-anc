// Package reminders owns durable one-shot reminders, independently of CRM.
// Its database and identity routes must never be exposed to agent processes.
package reminders

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

var ErrUnavailable = errors.New("reminder database unavailable")

// This small-team store uses one versioned document in SQLite. BEGIN IMMEDIATE
// serializes short state transitions across processes; network I/O never runs
// inside a transaction. No pruning silently discards pending work or receipts.
type Store struct {
	db     *sql.DB
	path   string
	file   os.FileInfo
	mu     sync.Mutex
	failed bool
}

type state struct {
	Schema   int                        `json:"schema"`
	Items    map[string]*Reminder       `json:"items"`
	Requests map[string]json.RawMessage `json:"requests"`
	Batches  map[string]*Batch          `json:"batches"`
}

type Reminder struct {
	ID          string `json:"id"`
	User        string `json:"user"`
	Chat        string `json:"chat"`
	Destination string `json:"destination"`
	Content     string `json:"content"`
	At          int64  `json:"at"`
	Version     int    `json:"version"`
	Status      string `json:"status"`
	Batch       string `json:"batch,omitempty"`
	Receipt     string `json:"receipt,omitempty"`
}

type Batch struct {
	ID                string   `json:"id"`
	User              string   `json:"user"`
	Destination       string   `json:"destination"`
	Items             []string `json:"items"`
	Text              string   `json:"text"`
	UUID              string   `json:"uuid"`
	FirstAttempt      int64    `json:"first_attempt"`
	Attempts          int      `json:"attempts"`
	Next              int64    `json:"next"`
	LeaseUntil        int64    `json:"lease_until"`
	Status            string   `json:"status"`
	Receipt           string   `json:"receipt,omitempty"`
	PossibleDuplicate bool     `json:"possible_duplicate"`
}

func emptyState() state {
	return state{1, map[string]*Reminder{}, map[string]json.RawMessage{}, map[string]*Batch{}}
}

// Initialize is an explicit host operation, never called by Open or recovery.
// It refuses an existing path. The operator must provide a protected directory.
func Initialize(path string) error {
	if !filepath.IsAbs(path) {
		return ErrUnavailable
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil || !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 || dir.Mode().Perm()&0077 != 0 {
		return ErrUnavailable
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrUnavailable
	}
	if err = f.Close(); err != nil {
		return ErrUnavailable
	}
	db, err := sql.Open("sqlite", sqliteExisting(path))
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = db.Close() }()
	data, _ := json.Marshal(emptyState())
	_, err = db.Exec(`PRAGMA synchronous=FULL; CREATE TABLE reminder_state (id INTEGER PRIMARY KEY CHECK(id=1), payload TEXT NOT NULL); INSERT INTO reminder_state VALUES (1, ?);`, string(data))
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

// Open never creates a missing database or resets malformed state.
func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrUnavailable
	}
	info, err := os.Lstat(path)
	dir, dirErr := os.Lstat(filepath.Dir(path))
	if err != nil || dirErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !dir.IsDir() || dir.Mode().Perm()&0077 != 0 || dir.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnavailable
	}
	db, err := sql.Open("sqlite", sqliteExisting(path))
	if err != nil {
		return nil, ErrUnavailable
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path, file: info}
	if _, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA synchronous=FULL;`); err == nil {
		err = s.change(context.Background(), func(*state) error { return nil })
	}
	if err != nil {
		_ = db.Close()
		return nil, ErrUnavailable
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func sqliteExisting(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=rw"}).String()
}

func (s *Store) change(ctx context.Context, fn func(*state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Lstat(s.path)
	if s.failed || err != nil || !os.SameFile(s.file, info) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		s.failed = true
		return ErrUnavailable
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		s.failed = true
		return ErrUnavailable
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		s.failed = true
		return ErrUnavailable
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }() // no-op after commit
	var data []byte
	var st state
	err = conn.QueryRowContext(ctx, "SELECT payload FROM reminder_state WHERE id=1").Scan(&data)
	if err != nil || json.Unmarshal(data, &st) != nil || !validState(st) {
		s.failed = true
		return ErrUnavailable
	}
	if err = fn(&st); err != nil {
		return err
	}
	data, err = json.Marshal(st)
	if err == nil {
		_, err = conn.ExecContext(ctx, "UPDATE reminder_state SET payload=? WHERE id=1", string(data))
	}
	if err == nil {
		_, err = conn.ExecContext(ctx, "COMMIT")
	}
	if err != nil {
		s.failed = true
		return ErrUnavailable
	}
	return nil
}

func validState(st state) bool {
	if st.Schema != 1 || st.Items == nil || st.Requests == nil || st.Batches == nil {
		return false
	}
	for id, r := range st.Items {
		if r == nil || r.ID != id || r.User == "" || r.Chat == "" || r.Destination == "" || r.Content == "" || r.At <= 0 || r.Version < 1 {
			return false
		}
		switch r.Status {
		case "pending", "sending", "retry", "paused", "cancelled", "sent":
		default:
			return false
		}
		if r.Batch != "" && st.Batches[r.Batch] == nil {
			return false
		}
	}
	for id, b := range st.Batches {
		if b == nil || b.ID != id || b.User == "" || b.Destination == "" || b.Text == "" || b.UUID == "" || b.Attempts < 0 {
			return false
		}
		switch b.Status {
		case "pending", "sending", "retry", "paused", "cancelled", "sent":
		default:
			return false
		}
		for _, item := range b.Items {
			r := st.Items[item]
			if r == nil || r.User != b.User || r.Destination != b.Destination {
				return false
			}
		}
	}
	for _, raw := range st.Requests {
		var result map[string]any
		if json.Unmarshal(raw, &result) != nil || result == nil || result["status"] == nil {
			return false
		}
	}
	return true
}
