package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
)

var kindRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidKind reports whether k can be an event kind: it ends up in `event:` lines of the SSE stream, so it
// must never carry a newline or other framing.
func ValidKind(k string) bool { return kindRe.MatchString(k) }

// AppendEvent stores one event and returns its seq. TS defaults to now. It only persists; package event also
// broadcasts.
func (s *Store) AppendEvent(ctx context.Context, e Event) (int64, error) {
	if !ValidKind(e.Kind) {
		return 0, errors.New("store: event kind must be 1-64 characters of A-Z a-z 0-9 . _ -")
	}
	if len(e.Data) > 0 && !json.Valid(e.Data) {
		return 0, errors.New("store: event data is not valid JSON")
	}
	if e.TS == 0 {
		e.TS = nowMS()
	}
	res, err := s.w.ExecContext(ctx, `INSERT INTO events(ts,run,shard,kind,test,step,data) VALUES(?,?,?,?,?,?,?)`,
		e.TS, e.Run, e.Shard, e.Kind, e.Test, e.Step, nullJSON(e.Data))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EventsAfter returns up to limit events with seq > since, in order.
func (s *Store) EventsAfter(ctx context.Context, since int64, limit int) ([]Event, error) {
	return s.queryEvents(ctx, `SELECT seq,ts,run,shard,kind,test,step,data FROM events WHERE seq > ? ORDER BY seq LIMIT ?`,
		since, limit)
}

// RunEvents returns every event of a run, in order.
func (s *Store) RunEvents(ctx context.Context, run string) ([]Event, error) {
	return s.queryEvents(ctx, `SELECT seq,ts,run,shard,kind,test,step,data FROM events WHERE run=? ORDER BY seq`, run)
}

// LastSeq returns the newest seq, 0 when the log is empty.
func (s *Store) LastSeq(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT MAX(seq) FROM events`).Scan(&n)
	return n.Int64, err
}

func (s *Store) queryEvents(ctx context.Context, q string, args ...any) ([]Event, error) {
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var data sql.NullString
		if err := rows.Scan(&e.Seq, &e.TS, &e.Run, &e.Shard, &e.Kind, &e.Test, &e.Step, &data); err != nil {
			return nil, err
		}
		if data.Valid {
			e.Data = []byte(data.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
