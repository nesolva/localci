package suite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/shard"
)

// dbSet is the database machinery of one run: the template and a way to clone and drop shard databases.
type dbSet struct {
	drv    *shard.Tracker
	tpl    string
	urlFor func(name string) (string, error)
}

// setupDB builds (or finds) the template database and sweeps leftovers of runs that never cleaned up.
func (ex *execution) setupDB() error {
	d := ex.suite.Database
	if d == nil {
		return nil
	}
	var drv shard.DB
	var urlFor func(string) (string, error)
	switch d.Driver {
	case "command":
		dbDir := filepath.Join(ex.state, "db")
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			return err
		}
		drv = &shard.Command{CreateCmd: d.Create, DropCmd: d.Drop, SnapshotCmd: d.Snapshot,
			StateDir: filepath.Join(ex.state, "templates"), Dir: dbDir}
		urlFor = func(name string) (string, error) {
			if d.URL == "" {
				return filepath.Join(dbDir, name), nil
			}
			return env{dbName: name}.expand(d.URL)
		}
	case "postgres":
		pg := &shard.Postgres{URL: d.URL, Container: d.Container, User: d.User, LockDir: shard.LockDir(ex.r.d.DataDir)}
		drv, urlFor = pg, pg.DatabaseURL
	}
	set := &dbSet{drv: shard.Track(drv, filepath.Join(ex.state, "ledger")), urlFor: urlFor}
	if dropped, err := set.drv.Sweep(ex.ctx); err != nil || len(dropped) > 0 {
		ex.emit(0, "db.sweep", "", "", map[string]any{"dropped": dropped, "error": fmt.Sprint(err)})
	}

	var files []string
	for _, k := range d.Template.Key {
		repo, glob, _ := strings.Cut(k, ":")
		matched, err := globFiles(ex.dirs[repo], []string{glob})
		if err != nil {
			return fmt.Errorf("database.template.key: %w", err)
		}
		for _, f := range matched {
			files = append(files, filepath.Join(ex.dirs[repo], f))
		}
	}
	key, err := shard.Key(files)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(d.Template.Seed))
	key += hex.EncodeToString(sum[:3]) // a changed seed command is a new template too

	seed := func(ctx context.Context, name string) error {
		url, err := urlFor(name)
		if err != nil {
			return err
		}
		cmd, err := env{dbName: name, dbURL: url}.expand(d.Template.Seed)
		if err != nil {
			return err
		}
		log := filepath.Join(ex.logDir, "template.log")
		res, err := exec.Run(ctx, exec.Spec{Cmd: cmd, Dir: ex.dirs[d.Template.Repo], LogPath: log, KillGrace: 5 * time.Second,
			Env: []string{"LOCALCI_DB_NAME=" + name, "LOCALCI_DB_URL=" + url}})
		if err != nil {
			return err
		}
		if !res.OK() {
			return fmt.Errorf("exit %d: %s", res.ExitCode, strings.Join(tail(log, 8), "\n"))
		}
		return nil
	}
	if set.tpl, err = set.drv.Template(ex.ctx, key, seed); err != nil {
		return fmt.Errorf("database template: %w", err)
	}
	ex.db = set
	return nil
}
