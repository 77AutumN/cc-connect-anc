// Package opsalerts owns a protected operations outbox, not a model tool.
package opsalerts

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

var ErrUnavailable = errors.New("operations outbox unavailable")

type Store struct {
	db     *sql.DB
	path   string
	file   os.FileInfo
	mu     sync.Mutex
	failed bool
}

type state struct {
	Schema   int
	Events   map[string]*event
	Messages map[string]*message
}

func emptyState() state { return state{1, map[string]*event{}, map[string]*message{}} }

func existingURL(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=rw"}).String()
}

func secureFile(path string) (os.FileInfo, error) {
	f, err := os.Lstat(path)
	d, de := os.Lstat(filepath.Dir(path))
	if !filepath.IsAbs(path) || err != nil || de != nil || !f.Mode().IsRegular() ||
		f.Mode().Perm()&0077 != 0 || f.Mode().Perm()&0200 == 0 || !d.IsDir() ||
		d.Mode()&os.ModeSymlink != 0 || d.Mode().Perm()&0077 != 0 {
		return nil, ErrUnavailable
	}
	return f, nil
}

// Initialize is explicit and refuses every existing path, including corrupt DBs.
func Initialize(path string) error {
	d, err := os.Lstat(filepath.Dir(path))
	if !filepath.IsAbs(path) || err != nil || !d.IsDir() || d.Mode()&os.ModeSymlink != 0 || d.Mode().Perm()&0077 != 0 {
		return ErrUnavailable
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrUnavailable
	}
	if err = f.Close(); err != nil {
		return ErrUnavailable
	}
	db, err := sql.Open("sqlite", existingURL(path))
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = db.Close() }()
	b, _ := json.Marshal(emptyState())
	_, err = db.Exec(`PRAGMA synchronous=FULL; CREATE TABLE ops_alert_state (id INTEGER PRIMARY KEY CHECK(id=1), payload TEXT NOT NULL); INSERT INTO ops_alert_state VALUES(1, ?);`, string(b))
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

func Open(path string) (*Store, error) {
	f, err := secureFile(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", existingURL(path))
	if err != nil {
		return nil, ErrUnavailable
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path, file: f}
	if _, err = db.Exec(`PRAGMA busy_timeout=1000; PRAGMA synchronous=FULL;`); err == nil {
		err = s.change(context.Background(), func(*state) error { return nil })
	}
	if err != nil {
		_ = db.Close()
		return nil, ErrUnavailable
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) change(ctx context.Context, fn func(*state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := secureFile(s.path)
	if s.failed || err != nil || !os.SameFile(f, s.file) {
		s.failed = true
		return ErrUnavailable
	}
	err = s.transaction(ctx, fn)
	if err != nil && ctx.Err() == nil {
		s.failed = true
		return ErrUnavailable
	}
	return err
}

func (s *Store) transaction(ctx context.Context, fn func(*state) error) error {
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if _, err = c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() { _, _ = c.ExecContext(context.Background(), "ROLLBACK") }()
	var data []byte
	if err = c.QueryRowContext(ctx, "SELECT payload FROM ops_alert_state WHERE id=1").Scan(&data); err != nil {
		return err
	}
	var st state
	if json.Unmarshal(data, &st) != nil || !validState(st) {
		return ErrUnavailable
	}
	if err = fn(&st); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if string(b) != string(data) {
		if _, err = c.ExecContext(ctx, "UPDATE ops_alert_state SET payload=? WHERE id=1", string(b)); err != nil {
			return err
		}
	}
	_, err = c.ExecContext(ctx, "COMMIT")
	return err
}

func validState(st state) bool {
	if st.Schema != 1 || st.Events == nil || st.Messages == nil {
		return false
	}
	for key, e := range st.Events {
		if e == nil || key != e.Key || e.ID == "" || !known(e.Category) || e.Count < 1 || e.First <= 0 || e.Last < e.First {
			return false
		}
	}
	for id, m := range st.Messages {
		if m == nil || id != m.ID || m.Event == "" || m.Order < 1 || m.UUID == "" || m.Text == "" || m.Binding == "" || m.Chat == "" || m.Attempts < 0 {
			return false
		}
	}
	return true
}
