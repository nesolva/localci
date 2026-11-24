package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
)

var (
	// ErrBadToken is a join token that does not exist, was already used, or expired.
	ErrBadToken = errors.New("join token is not valid (wrong, used or expired)")
	// ErrNameTaken is a worker name that is already registered (revoked ones included).
	ErrNameTaken = errors.New("a worker with that name already exists")
	// ErrBadCredential is a worker credential that is unknown or revoked.
	ErrBadCredential = errors.New("worker credential is not valid")
	// ErrNoWorker is a worker name that is not registered.
	ErrNoWorker = errors.New("no such worker")
	// ErrBadName is a worker name that is not a short label.
	ErrBadName = errors.New("worker name must be 1-40 letters, digits, '-' or '_'")
)

// joinTokenTTL is how long an unused join token stays redeemable.
const joinTokenTTL = 24 * 60 * 60 * 1000

var workerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$`)

// WorkerRow is a registered worker.
type WorkerRow struct {
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
	Created  int64  `json:"created"`
	Revoked  bool   `json:"revoked"`
	LastSeen int64  `json:"lastSeen"`
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewJoinToken creates a single-use join token. Only its hash is stored; the token is returned once.
func (s *Store) NewJoinToken(ctx context.Context) (string, error) {
	tok, err := newSecret()
	if err != nil {
		return "", err
	}
	_, err = s.w.ExecContext(ctx, `INSERT INTO join_tokens(hash, created) VALUES(?,?)`, hashSecret(tok), nowMS())
	return tok, err
}

// MaxWorkerCapacity bounds the capacity a worker may advertise: it is the worker's own claim, so it is not trusted.
const MaxWorkerCapacity = 256

func clampCapacity(c int) int { return min(max(c, 1), MaxWorkerCapacity) }

// Join spends a join token and registers a worker under name, returning its long-lived credential (stored hashed).
// The token is checked before anything about the name, so a wrong token gets the same answer whatever the name and
// the call cannot be used to find out which names exist. Spending is one conditional UPDATE, so two joins with the
// same token cannot both succeed. A refused join (name taken, bad name) does not spend the token.
func (s *Store) Join(ctx context.Context, token, name string, capacity int) (string, error) {
	capacity = clampCapacity(capacity)
	cred, err := newSecret()
	if err != nil {
		return "", err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		now := nowMS()
		var valid int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM join_tokens WHERE hash=? AND used=0 AND created>?`,
			hashSecret(token), now-joinTokenTTL).Scan(&valid); err != nil {
			return err
		}
		if valid != 1 {
			return ErrBadToken
		}
		if !workerName.MatchString(name) {
			return ErrBadName
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workers WHERE name=?`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrNameTaken
		}
		res, err := tx.ExecContext(ctx, `UPDATE join_tokens SET used=? WHERE hash=? AND used=0 AND created>?`,
			now, hashSecret(token), now-joinTokenTTL)
		if err != nil {
			return err
		}
		if k, _ := res.RowsAffected(); k != 1 {
			return ErrBadToken
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workers(name, cred_hash, capacity, created, last_seen) VALUES(?,?,?,?,?)`,
			name, hashSecret(cred), capacity, now, now)
		return err
	})
	if err != nil {
		return "", err
	}
	return cred, nil
}

// WorkerByCredential finds the live (not revoked) worker a credential belongs to.
func (s *Store) WorkerByCredential(ctx context.Context, cred string) (WorkerRow, error) {
	var w WorkerRow
	var revoked int
	err := s.r.QueryRowContext(ctx, `SELECT name, capacity, created, revoked, last_seen FROM workers WHERE cred_hash=?`,
		hashSecret(cred)).Scan(&w.Name, &w.Capacity, &w.Created, &revoked, &w.LastSeen)
	if err == sql.ErrNoRows || revoked == 1 {
		return WorkerRow{}, ErrBadCredential
	}
	w.Revoked = revoked == 1
	return w, err
}

// TouchWorker records that a worker was heard from, with the capacity it advertises now. A name the store does not
// hold, or one that was revoked, is ErrBadCredential.
func (s *Store) TouchWorker(ctx context.Context, name string, capacity int) error {
	capacity = clampCapacity(capacity)
	res, err := s.w.ExecContext(ctx, `UPDATE workers SET capacity=?, last_seen=? WHERE name=? AND revoked=0`, capacity, nowMS(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBadCredential
	}
	return nil
}

// RevokeWorker makes a worker's credential stop working. The row stays so the name cannot be reused.
func (s *Store) RevokeWorker(ctx context.Context, name string) error {
	res, err := s.w.ExecContext(ctx, `UPDATE workers SET revoked=1 WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoWorker
	}
	return nil
}

// Workers lists every registered worker, revoked ones included, by name.
func (s *Store) Workers(ctx context.Context) ([]WorkerRow, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT name, capacity, created, revoked, last_seen FROM workers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkerRow
	for rows.Next() {
		var w WorkerRow
		var revoked int
		if err := rows.Scan(&w.Name, &w.Capacity, &w.Created, &revoked, &w.LastSeen); err != nil {
			return nil, err
		}
		w.Revoked = revoked == 1
		out = append(out, w)
	}
	return out, rows.Err()
}
