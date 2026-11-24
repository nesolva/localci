package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
)

// CoverageEntry is what one test touched: repo-qualified files ("owner/name:path") and the commit of every repo
// the entry was measured at.
type CoverageEntry struct {
	Test    string            `json:"test"`
	Files   []string          `json:"files"`
	Commits map[string]string `json:"commits"`
	TS      int64             `json:"ts"`
	// Merge adds Files to the stored entry instead of replacing it, and leaves its commits and time alone: the
	// entry is still as old as the run that measured it. A test with no stored entry gets this one as it is.
	Merge bool `json:"-"`
}

// CoverageMap returns the suite's map, test id → entry.
func (s *Store) CoverageMap(ctx context.Context, suite string) (map[string]CoverageEntry, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT test, files, commits, ts FROM coverage_map WHERE suite=?`, suite)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CoverageEntry{}
	for rows.Next() {
		var e CoverageEntry
		var files, commits string
		if err := rows.Scan(&e.Test, &files, &commits, &e.TS); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(files), &e.Files)
		json.Unmarshal([]byte(commits), &e.Commits)
		out[e.Test] = e
	}
	return out, rows.Err()
}

// DropCoverage forgets what the given tests reach: with no entry a test is always selected.
func (s *Store) DropCoverage(ctx context.Context, suite string, tests []string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, t := range tests {
			if _, err := tx.ExecContext(ctx, `DELETE FROM coverage_map WHERE suite=? AND test=?`, suite, t); err != nil {
				return err
			}
		}
		return nil
	})
}

// PutCoverage replaces the entries of the given tests (or extends them, for entries that say Merge). With keepOnly set (a full run), entries of tests that are
// not in it are dropped: those tests no longer exist.
func (s *Store) PutCoverage(ctx context.Context, suite string, entries []CoverageEntry, keepOnly []string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, e := range entries {
			if e.Merge {
				var oldFiles, oldCommits string
				var oldTS int64
				err := tx.QueryRowContext(ctx, `SELECT files, commits, ts FROM coverage_map WHERE suite=? AND test=?`, suite, e.Test).
					Scan(&oldFiles, &oldCommits, &oldTS)
				if err != nil && err != sql.ErrNoRows {
					return err
				}
				if err == nil {
					var have []string
					var commits map[string]string
					json.Unmarshal([]byte(oldFiles), &have)
					json.Unmarshal([]byte(oldCommits), &commits)
					e.Files = unionSorted(have, e.Files)
					e.Commits, e.TS = commits, oldTS
				}
			}
			files, _ := json.Marshal(e.Files)
			commits, _ := json.Marshal(e.Commits)
			if e.TS == 0 {
				e.TS = nowMS()
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO coverage_map(suite, test, files, commits, ts) VALUES(?,?,?,?,?)
				ON CONFLICT(suite, test) DO UPDATE SET files=excluded.files, commits=excluded.commits, ts=excluded.ts`,
				suite, e.Test, string(files), string(commits), e.TS); err != nil {
				return err
			}
		}
		if keepOnly == nil {
			return nil
		}
		keep := map[string]bool{}
		for _, id := range keepOnly {
			keep[id] = true
		}
		rows, err := tx.QueryContext(ctx, `SELECT test FROM coverage_map WHERE suite=?`, suite)
		if err != nil {
			return err
		}
		var drop []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !keep[id] {
				drop = append(drop, id)
			}
		}
		rows.Close()
		for _, id := range drop {
			if _, err := tx.ExecContext(ctx, `DELETE FROM coverage_map WHERE suite=? AND test=?`, suite, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func unionSorted(a, b []string) []string {
	set := map[string]bool{}
	for _, f := range a {
		set[f] = true
	}
	for _, f := range b {
		set[f] = true
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	slices.Sort(out)
	return out
}
