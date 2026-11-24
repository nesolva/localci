package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Finding is one thing the exploratory agent reported. Everything in it came from an agent driving the app, so
// the explore package makes it printable and confines its paths before it gets here.
type Finding struct {
	ID         int64    `json:"id"`
	Run        string   `json:"run"`
	Suite      string   `json:"suite"`
	Goal       string   `json:"goal,omitempty"`
	Severity   string   `json:"severity"`
	Title      string   `json:"title"`
	Steps      []string `json:"steps"`
	Screenshot string   `json:"screenshot,omitempty"` // absolute path inside the run's log directory
	Test       string   `json:"test,omitempty"`       // a draft test saved under the run, never committed
	TS         int64    `json:"ts"`
}

const findingCols = `id, run, suite, goal, severity, title, steps, screenshot, test, ts`

// AddFindings stores findings of one run, setting their ids and, when missing, their time.
func (s *Store) AddFindings(ctx context.Context, fs []Finding) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for i := range fs {
			f := &fs[i]
			if f.TS == 0 {
				f.TS = nowMS()
			}
			steps, _ := json.Marshal(append([]string{}, f.Steps...))
			res, err := tx.ExecContext(ctx, `INSERT INTO findings(run, suite, goal, severity, title, steps, screenshot, test, ts)
				VALUES(?,?,?,?,?,?,?,?,?)`, f.Run, f.Suite, f.Goal, f.Severity, f.Title, string(steps), f.Screenshot, f.Test, f.TS)
			if err != nil {
				return err
			}
			f.ID, _ = res.LastInsertId()
		}
		return nil
	})
}

// Findings lists findings newest first: those with an id above after, and, when notBefore is above 0, found at or
// after that time (unix ms). run, when set, limits it to one run.
func (s *Store) Findings(ctx context.Context, after, notBefore int64, run string) ([]Finding, error) {
	q := `SELECT ` + findingCols + ` FROM findings WHERE id>? AND ts>=?`
	args := []any{after, notBefore}
	if run != "" {
		q += ` AND run=?`
		args = append(args, run)
	}
	rows, err := s.r.QueryContext(ctx, q+` ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Finding{}
	for rows.Next() {
		var f Finding
		var steps string
		if err := rows.Scan(&f.ID, &f.Run, &f.Suite, &f.Goal, &f.Severity, &f.Title, &steps, &f.Screenshot, &f.Test, &f.TS); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(steps), &f.Steps) != nil || f.Steps == nil {
			f.Steps = []string{}
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// LastFindingID is the newest finding's id, 0 when there is none: the cursor a `--since` listing ends with.
func (s *Store) LastFindingID(ctx context.Context) int64 {
	var id int64
	s.r.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM findings`).Scan(&id)
	return id
}
