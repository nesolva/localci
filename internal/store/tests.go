package store

import (
	"context"
	"database/sql"
	"errors"
)

const testCols = `run,shard,test,file,title,line,status,ms,retry,failure,class,evidence`

func scanTest(sc scanner) (Test, error) {
	var t Test
	var failure, evidence sql.NullString
	if err := sc.Scan(&t.Run, &t.Shard, &t.ID, &t.File, &t.Title, &t.Line, &t.Status, &t.MS, &t.Retry,
		&failure, &t.Class, &evidence); err != nil {
		return t, err
	}
	if failure.Valid {
		t.Failure = []byte(failure.String)
	}
	if evidence.Valid {
		t.Evidence = []byte(evidence.String)
	}
	return t, nil
}

// RunTests returns the last attempt of every test in the run, in the order they were recorded.
func (s *Store) RunTests(ctx context.Context, run string) ([]Test, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+testCols+` FROM tests t WHERE run=?
		AND NOT EXISTS (SELECT 1 FROM tests t2 WHERE t2.run=t.run AND t2.test=t.test AND t2.retry>t.retry)
		ORDER BY shard, rowid`, run)
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

// RunTest returns the last attempt of one test in a run, or ErrNotFound.
func (s *Store) RunTest(ctx context.Context, run, test string) (Test, error) {
	t, err := scanTest(s.r.QueryRowContext(ctx, `SELECT `+testCols+` FROM tests WHERE run=? AND test=?
		ORDER BY retry DESC LIMIT 1`, run, test))
	if errors.Is(err, sql.ErrNoRows) {
		return Test{}, ErrNotFound
	}
	return t, err
}
