package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
)

// ErrNotFound is returned when a run does not exist.
var ErrNotFound = errors.New("store: not found")

const runCols = `id, suite, kind, heads, "trigger", requested_by, state, reason, created, started, finished, fork, affected, infra`

type scanner interface{ Scan(...any) error }

func scanRun(sc scanner) (Run, error) {
	var r Run
	var heads sql.NullString
	err := sc.Scan(&r.ID, &r.Suite, &r.Kind, &heads, &r.Trigger, &r.RequestedBy, &r.State, &r.Reason,
		&r.Created, &r.Started, &r.Finished, &r.Fork, &r.Affected, &r.Infra)
	if heads.Valid {
		r.Heads = []byte(heads.String)
	}
	return r, err
}

// PutRun inserts or replaces a run (Created defaults to now), then applies the retention policy.
func (s *Store) PutRun(ctx context.Context, r Run) error {
	if r.Created == 0 {
		r.Created = nowMS()
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO runs(`+runCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET suite=excluded.suite, kind=excluded.kind, heads=excluded.heads,
			"trigger"=excluded."trigger", requested_by=excluded.requested_by, state=excluded.state,
			reason=excluded.reason, created=excluded.created, started=excluded.started, finished=excluded.finished,
			fork=excluded.fork, affected=excluded.affected, infra=excluded.infra`,
			r.ID, r.Suite, r.Kind, nullJSON(r.Heads), r.Trigger, r.RequestedBy, r.State, r.Reason,
			r.Created, r.Started, r.Finished, r.Fork, r.Affected, r.Infra)
		return err
	})
	if err != nil {
		return err
	}
	return s.pruneIfOver(ctx)
}

// SetRunState moves a run to state. started/finished are stamped with now the first time the run reaches
// running / a terminal state.
func (s *Store) SetRunState(ctx context.Context, id, state, reason string) error {
	now := nowMS()
	res, err := s.w.ExecContext(ctx, `UPDATE runs SET state=?, reason=?,
		started = CASE WHEN ?=? AND started=0 THEN ? ELSE started END,
		finished = CASE WHEN ? AND finished=0 THEN ? ELSE finished END WHERE id=?`,
		state, reason, state, StateRunning, now, Terminal(state), now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetRunInfra records why a run is an infrastructure failure. The first reason stays: it reports whether it was
// the one that set it.
func (s *Store) SetRunInfra(ctx context.Context, id, infra string) (bool, error) {
	res, err := s.w.ExecContext(ctx, `UPDATE runs SET infra=? WHERE id=? AND infra=''`, infra, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Run returns one run, or ErrNotFound.
func (s *Store) Run(ctx context.Context, id string) (Run, error) {
	r, err := scanRun(s.r.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// Runs returns the runs touched by an event with seq > since, newest first; since <= 0 returns every run.
func (s *Store) Runs(ctx context.Context, since int64) ([]Run, error) {
	q := `SELECT ` + runCols + ` FROM runs`
	args := []any{}
	if since > 0 {
		q += ` WHERE id IN (SELECT DISTINCT run FROM events WHERE seq > ?)`
		args = append(args, since)
	}
	rows, err := s.r.QueryContext(ctx, q+` ORDER BY created DESC, rowid DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecoverStale marks runs a crash left preparing or running as queued again and returns them. Call it once at
// start-up, before anything schedules.
func (s *Store) RecoverStale(ctx context.Context) ([]Run, error) {
	var out []Run
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE state IN (?,?) ORDER BY created, rowid`,
			StatePreparing, StateRunning)
		if err != nil {
			return err
		}
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				rows.Close()
				return err
			}
			r.State = StateQueued
			out = append(out, r)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE runs SET state=?, started=0 WHERE state IN (?,?)`,
			StateQueued, StatePreparing, StateRunning)
		return err
	})
	return out, err
}

// PutShard inserts or replaces a shard.
func (s *Store) PutShard(ctx context.Context, sh Shard) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO shards(run,n,state,worker,log_path,cost_cpu,cost_mem) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(run,n) DO UPDATE SET state=excluded.state, worker=excluded.worker, log_path=excluded.log_path,
		cost_cpu=excluded.cost_cpu, cost_mem=excluded.cost_mem`,
		sh.Run, sh.N, sh.State, sh.Worker, sh.LogPath, sh.CostCPU, sh.CostMem)
	return err
}

// Shards returns a run's shards in order.
func (s *Store) Shards(ctx context.Context, run string) ([]Shard, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT run,n,state,worker,log_path,cost_cpu,cost_mem FROM shards WHERE run=? ORDER BY n`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Shard
	for rows.Next() {
		var sh Shard
		if err := rows.Scan(&sh.Run, &sh.N, &sh.State, &sh.Worker, &sh.LogPath, &sh.CostCPU, &sh.CostMem); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// PutTest inserts or replaces one attempt of a test (keyed by run, test id and retry).
func (s *Store) PutTest(ctx context.Context, t Test) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO tests(run,shard,test,file,title,line,status,ms,retry,failure,class,evidence)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run,test,retry) DO UPDATE SET shard=excluded.shard, file=excluded.file, title=excluded.title,
		line=excluded.line, status=excluded.status, ms=excluded.ms, failure=excluded.failure, class=excluded.class,
		evidence=excluded.evidence`,
		t.Run, t.Shard, t.ID, t.File, t.Title, t.Line, t.Status, t.MS, t.Retry, nullJSON(t.Failure), t.Class,
		nullJSON(t.Evidence))
	return err
}

// Failures returns the tests whose last attempt in the run did not pass (a test that passed on a retry is not one).
func (s *Store) Failures(ctx context.Context, run string) ([]Test, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT run,shard,test,file,title,line,status,ms,retry,failure,class,evidence
		FROM tests t WHERE run=? AND status IN ('failed','timedOut','interrupted')
		AND NOT EXISTS (SELECT 1 FROM tests t2 WHERE t2.run=t.run AND t2.test=t.test AND t2.retry>t.retry)
		ORDER BY shard, rowid`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Test
	for rows.Next() {
		var t Test
		var failure, evidence sql.NullString
		if err := rows.Scan(&t.Run, &t.Shard, &t.ID, &t.File, &t.Title, &t.Line, &t.Status, &t.MS, &t.Retry,
			&failure, &t.Class, &evidence); err != nil {
			return nil, err
		}
		if failure.Valid {
			t.Failure = []byte(failure.String)
		}
		if evidence.Valid {
			t.Evidence = []byte(evidence.String)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PutStep inserts or replaces a step (typically at step.begin, with OK nil).
func (s *Store) PutStep(ctx context.Context, st Step) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO steps(run,test,step,parent,title,kind,start,ms,ok) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run,test,step) DO UPDATE SET parent=excluded.parent, title=excluded.title, kind=excluded.kind,
		start=excluded.start, ms=excluded.ms, ok=excluded.ok`,
		st.Run, st.Test, st.ID, st.Parent, st.Title, st.Kind, st.Start, st.MS, nullBool(st.OK))
	return err
}

// EndStep records the duration and outcome of a step already stored.
func (s *Store) EndStep(ctx context.Context, run, test, step string, ms int64, ok bool) error {
	_, err := s.w.ExecContext(ctx, `UPDATE steps SET ms=?, ok=? WHERE run=? AND test=? AND step=?`,
		ms, nullBool(&ok), run, test, step)
	return err
}

func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

// PutArtifact records a file a test produced; the same file is stored once.
func (s *Store) PutArtifact(ctx context.Context, a Artifact) error {
	_, err := s.w.ExecContext(ctx, `INSERT OR IGNORE INTO artifacts(run,test,kind,path) VALUES(?,?,?,?)`,
		a.Run, a.Test, a.Kind, a.Path)
	return err
}

// Artifacts returns a run's artifacts in the order they were recorded.
func (s *Store) Artifacts(ctx context.Context, run string) ([]Artifact, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT run,test,kind,path FROM artifacts WHERE run=? ORDER BY id`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.Run, &a.Test, &a.Kind, &a.Path); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Timeline returns the run's steps grouped by shard lane (ascending), each lane in start order. Offsets are
// relative to the run's start, or to its earliest step when the run has none.
func (s *Store) Timeline(ctx context.Context, run string) ([]Lane, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT COALESCE((SELECT t.shard FROM tests t WHERE t.run=s.run AND t.test=s.test
			ORDER BY t.retry DESC LIMIT 1), 0), s.test, s.step, s.parent, s.title, s.kind, s.start, s.ms, s.ok
		FROM steps s WHERE s.run=? ORDER BY s.start, s.rowid`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		shard int
		start int64
		st    TimelineStep
	}
	var all []row
	for rows.Next() {
		var r row
		var ok sql.NullBool
		if err := rows.Scan(&r.shard, &r.st.Test, &r.st.ID, &r.st.Parent, &r.st.Title, &r.st.Kind, &r.start,
			&r.st.MS, &ok); err != nil {
			return nil, err
		}
		if ok.Valid {
			v := ok.Bool
			r.st.OK = &v
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	base := all[0].start // rows are ordered by start
	var started int64
	if err := s.r.QueryRowContext(ctx, `SELECT started FROM runs WHERE id=?`, run).Scan(&started); err == nil &&
		started > 0 && started < base {
		base = started
	}
	byShard := map[int]*Lane{}
	for _, r := range all {
		l := byShard[r.shard]
		if l == nil {
			l = &Lane{Shard: r.shard}
			byShard[r.shard] = l
		}
		r.st.Offset = r.start - base
		l.Steps = append(l.Steps, r.st)
	}
	lanes := make([]Lane, 0, len(byShard))
	for _, l := range byShard {
		lanes = append(lanes, *l)
	}
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].Shard < lanes[j].Shard })
	return lanes, nil
}

// RecordDuration stores how long a test took, keeping its 10 most recent measurements.
func (s *Store) RecordDuration(ctx context.Context, test string, ms int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO durations(test,ms,ts) VALUES(?,?,?)`, test, ms, nowMS()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM durations WHERE test=? AND id NOT IN
			(SELECT id FROM durations WHERE test=? ORDER BY id DESC LIMIT 10)`, test, test)
		return err
	})
}

// MedianDurations returns the median of each test's last 10 durations (ms); tests never measured are absent.
func (s *Store) MedianDurations(ctx context.Context, ids []string) (map[string]int64, error) {
	out := map[string]int64{}
	for len(ids) > 0 {
		n := min(len(ids), 500)
		chunk := ids[:n]
		ids = ids[n:]
		args := make([]any, n)
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.r.QueryContext(ctx, `SELECT test, ms FROM durations WHERE test IN (`+
			strings.TrimSuffix(strings.Repeat("?,", n), ",")+`) ORDER BY test, id DESC`, args...)
		if err != nil {
			return nil, err
		}
		samples := map[string][]int64{}
		for rows.Next() {
			var id string
			var ms int64
			if err := rows.Scan(&id, &ms); err != nil {
				rows.Close()
				return nil, err
			}
			if len(samples[id]) < 10 {
				samples[id] = append(samples[id], ms)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		for id, v := range samples {
			sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
			if len(v)%2 == 1 {
				out[id] = v[len(v)/2]
			} else {
				out[id] = (v[len(v)/2-1] + v[len(v)/2]) / 2
			}
		}
	}
	return out, nil
}

// AddSerial puts a test in the suite's serial group with its evidence (Since defaults to now).
func (s *Store) AddSerial(ctx context.Context, e SerialEntry) error {
	if e.Since == 0 {
		e.Since = nowMS()
	}
	_, err := s.w.ExecContext(ctx, `INSERT INTO serial(suite,test,evidence,since) VALUES(?,?,?,?)
		ON CONFLICT(suite,test) DO UPDATE SET evidence=excluded.evidence`, e.Suite, e.Test, nullJSON(e.Evidence), e.Since)
	return err
}

// RemoveSerial takes a test out of the serial group.
func (s *Store) RemoveSerial(ctx context.Context, suite, test string) error {
	_, err := s.w.ExecContext(ctx, `DELETE FROM serial WHERE suite=? AND test=?`, suite, test)
	return err
}

// Serial returns the suite's serial group, oldest first.
func (s *Store) Serial(ctx context.Context, suite string) ([]SerialEntry, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT suite,test,evidence,since FROM serial WHERE suite=? ORDER BY since, test`, suite)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SerialEntry
	for rows.Next() {
		var e SerialEntry
		var ev sql.NullString
		if err := rows.Scan(&e.Suite, &e.Test, &ev, &e.Since); err != nil {
			return nil, err
		}
		if ev.Valid {
			e.Evidence = []byte(ev.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
