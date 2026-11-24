package store

import (
	"context"
	"database/sql"
	"errors"
)

// Checkpoint is a flow's saved result: its files live in the data directory, Database is the name of the
// snapshot ("" when the flow keeps no database). A row exists only once everything it names was written, so a
// checkpoint that was being saved when localci was killed has no row and is never restored.
type Checkpoint struct {
	Suite    string `json:"suite"`
	Key      string `json:"key"`
	Flow     string `json:"flow"`
	Database string `json:"db,omitempty"`
	Bytes    int64  `json:"bytes"`
	Created  int64  `json:"created"`
	Used     int64  `json:"used"`
}

// PutCheckpoint records a finished checkpoint (Created and Used default to now).
func (s *Store) PutCheckpoint(ctx context.Context, c Checkpoint) error {
	if c.Created == 0 {
		c.Created = nowMS()
	}
	if c.Used == 0 {
		c.Used = c.Created
	}
	_, err := s.w.ExecContext(ctx, `INSERT INTO checkpoints(suite,key,flow,db,bytes,created,used) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(suite,key) DO UPDATE SET flow=excluded.flow, db=excluded.db, bytes=excluded.bytes, used=excluded.used`,
		c.Suite, c.Key, c.Flow, c.Database, c.Bytes, c.Created, c.Used)
	return err
}

// Checkpoint returns one checkpoint, ErrNotFound when there is none.
func (s *Store) Checkpoint(ctx context.Context, suite, key string) (Checkpoint, error) {
	var c Checkpoint
	err := s.r.QueryRowContext(ctx, `SELECT suite,key,flow,db,bytes,created,used FROM checkpoints WHERE suite=? AND key=?`,
		suite, key).Scan(&c.Suite, &c.Key, &c.Flow, &c.Database, &c.Bytes, &c.Created, &c.Used)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// UseCheckpoint marks a checkpoint as just used, which is what the least-recently-used eviction orders by.
func (s *Store) UseCheckpoint(ctx context.Context, suite, key string) error {
	_, err := s.w.ExecContext(ctx, `UPDATE checkpoints SET used=? WHERE suite=? AND key=?`, nowMS(), suite, key)
	return err
}

// Checkpoints lists a suite's checkpoints, least recently used first.
func (s *Store) Checkpoints(ctx context.Context, suite string) ([]Checkpoint, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT suite,key,flow,db,bytes,created,used FROM checkpoints WHERE suite=? ORDER BY used, created, key`, suite)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Checkpoint
	for rows.Next() {
		var c Checkpoint
		if err := rows.Scan(&c.Suite, &c.Key, &c.Flow, &c.Database, &c.Bytes, &c.Created, &c.Used); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCheckpoint forgets a checkpoint.
func (s *Store) DeleteCheckpoint(ctx context.Context, suite, key string) error {
	_, err := s.w.ExecContext(ctx, `DELETE FROM checkpoints WHERE suite=? AND key=?`, suite, key)
	return err
}

// KindEvents returns the newest limit events of the given kinds, oldest first, leaving out those of fork runs: a
// fork's report is not trusted, so whatever is learned from events (flow times, inputs) never reads it.
func (s *Store) KindEvents(ctx context.Context, limit int, kinds ...string) ([]Event, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	q := `SELECT seq,ts,run,shard,kind,test,step,data FROM (SELECT * FROM events WHERE kind IN (?` +
		repeatComma(len(kinds)-1) + `) AND run NOT IN (SELECT id FROM runs WHERE fork=1) ORDER BY seq DESC LIMIT ?) ORDER BY seq`
	args := make([]any, 0, len(kinds)+1)
	for _, k := range kinds {
		args = append(args, k)
	}
	return s.queryEvents(ctx, q, append(args, limit)...)
}

func repeatComma(n int) string {
	out := ""
	for range n {
		out += ",?"
	}
	return out
}
