package store

import (
	"context"
	"encoding/json"
	"slices"
)

// WriteSet is what the core measured a test touching in the database: the tables it writes and the tables it reads.
type WriteSet struct {
	Suite, Test   string
	Tables, Reads []string
	// Replace says the run that learned this tested every repository at a commit on its default branch: the set
	// replaces what was stored. Any other run only adds tables.
	Replace bool
	// Trusted is set by Writes: the stored set was last replaced by such a run.
	Trusted bool
}

// PutWrites records what a run learned about a test. A run on every repository's default branch (w.Replace)
// replaces the stored set and marks it trusted; any other run keeps what is stored and adds its tables, so a
// branch can widen a set but never narrow it, and never makes an untrusted set trusted.
func (s *Store) PutWrites(ctx context.Context, w WriteSet) error {
	var tables, reads string
	var trusted int
	err := s.w.QueryRowContext(ctx, `SELECT tables, reads, trusted FROM writes WHERE suite=? AND test=?`, w.Suite, w.Test).Scan(&tables, &reads, &trusted)
	if err == nil && !w.Replace {
		w.Tables = union(w.Tables, decodeList(tables))
		w.Reads = union(w.Reads, decodeList(reads))
	}
	if w.Replace {
		trusted = 1
	}
	t, _ := json.Marshal(union(w.Tables, nil))
	r, _ := json.Marshal(union(w.Reads, nil))
	_, err = s.w.ExecContext(ctx, `INSERT INTO writes(suite,test,tables,reads,ts,trusted) VALUES(?,?,?,?,?,?)
		ON CONFLICT(suite,test) DO UPDATE SET tables=excluded.tables, reads=excluded.reads, ts=excluded.ts, trusted=excluded.trusted`,
		w.Suite, w.Test, string(t), string(r), nowMS(), trusted)
	return err
}

// Writes returns the stored sets of a suite, by test.
func (s *Store) Writes(ctx context.Context, suite string) (map[string]WriteSet, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT test, tables, reads, trusted FROM writes WHERE suite=?`, suite)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WriteSet{}
	for rows.Next() {
		w := WriteSet{Suite: suite}
		var t, r string
		var trusted int
		if err := rows.Scan(&w.Test, &t, &r, &trusted); err != nil {
			return nil, err
		}
		w.Tables, w.Reads, w.Trusted = decodeList(t), decodeList(r), trusted == 1
		out[w.Test] = w
	}
	return out, rows.Err()
}

func decodeList(s string) []string {
	var l []string
	_ = json.Unmarshal([]byte(s), &l)
	return l
}

func union(a, b []string) []string {
	out := append(append([]string{}, a...), b...)
	slices.Sort(out)
	return slices.Compact(out)
}
