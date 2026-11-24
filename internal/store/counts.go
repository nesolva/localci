package store

import (
	"context"
	"database/sql"
	"errors"
)

// Counts is how a run's tests ended, by their last attempt.
type Counts struct {
	Total, Passed, Failed int
}

// TestCounts counts the tests of a run. Failed covers failed and timed out tests, and interrupted ones too unless
// the run is an infrastructure failure: tests the stack never let finish did not fail.
func (s *Store) TestCounts(ctx context.Context, run string) (Counts, error) {
	var infra string
	if err := s.r.QueryRowContext(ctx, `SELECT infra FROM runs WHERE id=?`, run).Scan(&infra); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Counts{}, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT status, COUNT(*) FROM tests t WHERE run=?
		AND NOT EXISTS (SELECT 1 FROM tests t2 WHERE t2.run=t.run AND t2.test=t.test AND t2.retry>t.retry)
		GROUP BY status`, run)
	if err != nil {
		return Counts{}, err
	}
	defer rows.Close()
	var c Counts
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return c, err
		}
		c.Total += n
		switch status {
		case "passed":
			c.Passed += n
		case "failed", "timedOut", "timedout":
			c.Failed += n
		case "interrupted":
			if infra == "" {
				c.Failed += n
			}
		}
	}
	return c, rows.Err()
}

// FirstEvent returns the first event of a kind in a run.
func (s *Store) FirstEvent(ctx context.Context, run, kind string) (Event, bool, error) {
	evs, err := s.queryEvents(ctx, `SELECT seq,ts,run,shard,kind,test,step,data FROM events WHERE run=? AND kind=?
		ORDER BY seq LIMIT 1`, run, kind)
	if err != nil || len(evs) == 0 {
		return Event{}, false, err
	}
	return evs[0], true, nil
}

// TestEnds returns the seq of the last test.end event of every test of a run, by test id.
func (s *Store) TestEnds(ctx context.Context, run string) (map[string]int64, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT test, MAX(seq) FROM events WHERE run=? AND kind='test.end' GROUP BY test`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var seq sql.NullInt64
		if err := rows.Scan(&id, &seq); err != nil {
			return nil, err
		}
		out[id] = seq.Int64
	}
	return out, rows.Err()
}
