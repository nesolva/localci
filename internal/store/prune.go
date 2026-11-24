package store

import (
	"context"
	"database/sql"
)

// Prune keeps the newest `keep` runs and deletes older finished ones with their shards, tests, steps, artifacts and
// events. A run that is still queued or active is never deleted. It returns how many runs were dropped.
func (s *Store) Prune(ctx context.Context, keep int) (int, error) {
	var dropped int
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM (
				SELECT id, state, ROW_NUMBER() OVER (ORDER BY created DESC, rowid DESC) AS rn FROM runs)
			WHERE rn > ? AND state IN (?,?,?,?)`, keep, StatePassed, StateFailed, StateCancelled, StateReused)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			for _, t := range []string{"events", "steps", "tests", "shards", "artifacts", "findings", "runs"} {
				col := "run"
				if t == "runs" {
					col = "id"
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE `+col+`=?`, id); err != nil {
					return err
				}
			}
		}
		dropped = len(ids)
		return nil
	})
	return dropped, err
}

// pruneIfOver applies Retention, cheaply when the run count is within it.
func (s *Store) pruneIfOver(ctx context.Context) error {
	var n int
	if err := s.w.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&n); err != nil {
		return err
	}
	if n <= Retention {
		return nil
	}
	_, err := s.Prune(ctx, Retention)
	return err
}
