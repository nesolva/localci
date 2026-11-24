package store

import "context"

// Attempts returns every attempt of every test of a run, oldest first within a test: what triage reads to tell a
// test that failed and then passed from one that failed every time.
func (s *Store) Attempts(ctx context.Context, run string) ([]Test, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+testCols+` FROM tests WHERE run=? ORDER BY shard, rowid, retry`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Test
	for rows.Next() {
		t, err := scanTest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetClass records what triage made of the last attempt of a test: its class and the evidence behind it.
func (s *Store) SetClass(ctx context.Context, run, test, class string, evidence []byte) error {
	_, err := s.w.ExecContext(ctx, `UPDATE tests SET class=?, evidence=? WHERE run=? AND test=?
		AND retry=(SELECT MAX(retry) FROM tests WHERE run=? AND test=?)`, class, nullJSON(evidence), run, test, run, test)
	return err
}

// FlakyTest is a test that was classed flaky, with how often since a point in time.
type FlakyTest struct {
	Test  string `json:"test"`
	Title string `json:"title"`
	File  string `json:"file"`
	Runs  int    `json:"runs"` // runs in which it was flaky
	Last  int64  `json:"last"` // creation time of the newest of them
}

// Flaky lists the tests classed flaky in runs created at or after since (unix ms), most frequent first. Fork runs
// are left out: their report is written by code nobody reviewed.
func (s *Store) Flaky(ctx context.Context, since int64) ([]FlakyTest, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT t.test, MAX(t.title), MAX(t.file), COUNT(DISTINCT t.run), MAX(r.created)
		FROM tests t JOIN runs r ON r.id=t.run
		WHERE t.class='flaky' AND r.fork=0 AND r.infra='' AND r.created>=?
		GROUP BY t.test ORDER BY COUNT(DISTINCT t.run) DESC, MAX(r.created) DESC, t.test`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlakyTest
	for rows.Next() {
		var f FlakyTest
		if err := rows.Scan(&f.Test, &f.Title, &f.File, &f.Runs, &f.Last); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// StepCounts is how many steps each test of a run recorded; a test with none has no entry.
func (s *Store) StepCounts(ctx context.Context, run string) (map[string]int, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT test, COUNT(*) FROM steps WHERE run=? GROUP BY test`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, rows.Err()
}
