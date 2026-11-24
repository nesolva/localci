// Package store is the SQLite state of localci: runs, shards, tests, steps, artifacts, the event log, measured
// durations and the serial group. One writer connection (so writers queue instead of hitting SQLITE_BUSY) and a pool
// of read-only connections that read concurrently in WAL mode.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Retention is how many of the newest runs keep their events, steps and artifacts.
const Retention = 1000

// Store is safe for concurrent use.
type Store struct {
	w *sql.DB // single writer connection
	r *sql.DB // readers
}

// Open creates or opens the database at path (parent directories included) and applies pending migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), DirMode); err != nil {
		return nil, err
	}
	w, err := openDB(path, false)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", path, err)
	}
	r, err := openDB(path, true)
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)
	// SQLite gives the WAL and shared-memory files the mode of the database file, so this covers all three.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, FileMode); err != nil && !errors.Is(err, fs.ErrNotExist) {
			w.Close()
			r.Close()
			return nil, err
		}
	}
	return &Store{w: w, r: r}, nil
}

func openDB(path string, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: strings.ReplaceAll(q.Encode(), "+", "%20")}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	var have int
	if err := db.QueryRow("PRAGMA user_version").Scan(&have); err != nil {
		return err
	}
	for i := have; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Close releases both connection pools.
func (s *Store) Close() error {
	err := s.r.Close()
	if e := s.w.Close(); err == nil {
		err = e
	}
	return err
}

func nowMS() int64 { return time.Now().UnixMilli() }

// inTx runs fn in one transaction on the writer connection.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}
