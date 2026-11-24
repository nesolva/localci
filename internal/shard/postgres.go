package shard

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Postgres is the driver for PostgreSQL through its own tools (createdb -T,
// dropdb, psql); no Go driver. It reaches the server with URL, or by running
// the tools inside a Docker container.
type Postgres struct {
	URL       string // connection URI of a maintenance database, used when Container is empty
	Container string // name of a running container to `docker exec` into
	User      string // role inside the container (default postgres)
	// LockDir holds the file locks that make two localci processes building the same template or checkpoint on
	// this server take turns, and that tell the sweep a build is still alive. Empty: this process only.
	LockDir string

	once        onceMap
	verMu       sync.Mutex
	verKnown    bool
	hasFileCopy bool
}

var _ DB = (*Postgres)(nil)

// DatabaseURL is URL pointing at database name, for the stack of a shard.
func (p *Postgres) DatabaseURL(name string) (string, error) {
	u, err := url.Parse(p.URL)
	if err != nil || p.URL == "" {
		return "", fmt.Errorf("no valid connection url configured")
	}
	u.Path = "/" + name
	return u.String(), nil
}

// complete marks a finished template (see Template).
const complete = "localci:complete"

// conn returns the connection string to hand to a tool with the password
// removed, and the environment that carries the password instead: argv is
// visible in ps to every local user, the environment is not.
func (p *Postgres) conn(db string) (target string, env []string) {
	target = db
	if p.Container == "" {
		target = p.URL
		if db != "" {
			if u, err := p.DatabaseURL(db); err == nil {
				target = u
			}
		}
		if u, err := url.Parse(target); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok {
				env = append(env, "PGPASSWORD="+pw)
				u.User = url.User(u.User.Username())
				target = u.String()
			}
		}
	}
	return target, env
}

func (p *Postgres) role() string {
	if p.User == "" {
		return "postgres"
	}
	return p.User
}

// command builds the tool invocation: locally with the password in the
// environment, or through docker exec with PGPASSWORD forwarded by name only
// (-e PGPASSWORD), so its value is never on the docker command line.
func (p *Postgres) command(ctx context.Context, tool, db string, args ...string) *exec.Cmd {
	if p.Container != "" {
		full := []string{"exec", "-i"}
		cmd := exec.CommandContext(ctx, "docker")
		if pw, ok := os.LookupEnv("PGPASSWORD"); ok && pw != "" {
			full = append(full, "-e", "PGPASSWORD")
		}
		full = append(full, p.Container, tool, "-U", p.role())
		cmd.Args = append(append([]string{"docker"}, full...), append(p.target(tool, db), args...)...)
		return cmd
	}
	target, env := p.conn(db)
	cmd := exec.CommandContext(ctx, tool, append(p.target(tool, target), args...)...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd
}

func (p *Postgres) tool(ctx context.Context, tool string, args ...string) *exec.Cmd {
	if p.Container != "" {
		return p.command(ctx, tool, "postgres", args...)
	}
	return p.command(ctx, tool, "", args...)
}

// target selects the maintenance database in the form each tool wants.
func (p *Postgres) target(tool, db string) []string {
	if tool == "psql" {
		return []string{"-X", "-v", "ON_ERROR_STOP=1", "-d", db}
	}
	return []string{"--maintenance-db=" + db}
}

func (p *Postgres) run(ctx context.Context, tool string, args ...string) (string, error) {
	out, err := p.tool(ctx, tool, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", tool, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *Postgres) exists(ctx context.Context, name string) (bool, error) {
	out, err := p.run(ctx, "psql", "-At", "-c", fmt.Sprintf("select 1 from pg_database where datname = '%s'", name))
	return out == "1", err
}

// isComplete reports whether the template name exists and was finished.
func (p *Postgres) isComplete(ctx context.Context, name string) (bool, error) {
	out, err := p.run(ctx, "psql", "-At", "-c", fmt.Sprintf(
		"select 1 from pg_database where datname = '%s' and shobj_description(oid, 'pg_database') = '%s'", name, complete))
	return out == "1", err
}

// Exec runs SQL against database name; seeds use it to load schema and data.
func (p *Postgres) Exec(ctx context.Context, name, sql string) error {
	if err := checkIdent(name); err != nil {
		return err
	}
	if p.Container == "" && p.URL == "" {
		return fmt.Errorf("no valid connection url configured")
	}
	cmd := p.command(ctx, "psql", name, "-c", sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("psql: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Template builds the template under a temporary name, marks it finished and
// only then renames it to its final name, so a kill or a cancelled seed can
// never leave something under the final name that looks finished. A template
// found without the mark (an older build, or a rename that never happened) is
// dropped and rebuilt.
func (p *Postgres) Template(ctx context.Context, key string, seed Seed) (string, error) {
	name := dbPrefix + "tpl_" + key
	if err := checkIdent(name); err != nil {
		return "", err
	}
	unlock, err := p.once.lockIn(ctx, p.LockDir, name)
	if err != nil {
		return "", err
	}
	defer unlock()
	if ok, err := p.isComplete(ctx, name); err != nil || ok {
		return name, err
	}
	if err := p.dropDetached(ctx, name); err != nil { // unfinished leftover, if any
		return "", err
	}
	tmp := fmt.Sprintf("%s_b%d", name, time.Now().UnixNano())
	if _, err := p.run(ctx, "createdb", tmp); err != nil {
		_ = p.dropDetached(ctx, tmp)
		return "", err
	}
	if err := seed(ctx, tmp); err != nil {
		_ = p.dropDetached(ctx, tmp)
		return "", fmt.Errorf("seed template %s: %w", name, err)
	}
	// Flush the template once, so the first clone does not pay for it. Needs
	// superuser or pg_checkpoint; without it the clones simply pay instead.
	_, _ = p.run(ctx, "psql", "-At", "-c", "checkpoint")
	_, err = p.run(ctx, "psql", "-At", "-c", fmt.Sprintf("comment on database %s is '%s'", tmp, complete))
	if err == nil {
		err = p.rename(ctx, tmp, name)
	}
	if err != nil {
		// Another process may have finished the same template meanwhile.
		ok, _ := p.isComplete(context.WithoutCancel(ctx), name)
		_ = p.dropDetached(ctx, tmp)
		if ok {
			return name, nil
		}
		return "", fmt.Errorf("finish template %s: %w", name, err)
	}
	return name, nil
}

// rename renames the finished template. PostgreSQL refuses while any session
// is connected to it, and seed tools with a connection pool can leave a
// backend behind for a moment after they return: those are terminated first,
// and the rename is retried briefly while they go away.
func (p *Postgres) rename(ctx context.Context, from, to string) error {
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(200 * time.Millisecond):
			}
		}
		_, _ = p.run(ctx, "psql", "-At", "-c", fmt.Sprintf(
			"select pg_terminate_backend(pid) from pg_stat_activity where datname = '%s' and pid <> pg_backend_pid()", from))
		if _, err = p.run(ctx, "psql", "-At", "-c", fmt.Sprintf("alter database %s rename to %s", from, to)); err == nil {
			return nil
		}
	}
	return err
}

// dropDetached drops name even when ctx is already cancelled (the likeliest
// reason a seed failed), under its own timeout.
func (p *Postgres) dropDetached(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	return p.Drop(ctx, name)
}

func (p *Postgres) Clone(ctx context.Context, template, name string) error {
	for _, v := range []string{template, name} {
		if err := checkIdent(v); err != nil {
			return err
		}
	}
	args := []string{"-T", template, name}
	if p.fileCopy(ctx) {
		// Copies the files instead of writing every page to the WAL: much
		// faster for a template of tens of megabytes.
		args = append([]string{"--strategy=file_copy"}, args...)
	}
	_, err := p.run(ctx, "createdb", args...)
	return err
}

// fileCopy reports whether the server knows CREATE DATABASE ... STRATEGY (15+).
// The answer is cached once the server gave one; a failure caused by the
// context (cancelled, timed out) is not an answer and is asked again.
func (p *Postgres) fileCopy(ctx context.Context) bool {
	p.verMu.Lock()
	defer p.verMu.Unlock()
	if p.verKnown {
		return p.hasFileCopy
	}
	out, err := p.run(ctx, "psql", "-At", "-c", "show server_version_num")
	if err != nil && ctx.Err() != nil {
		return false
	}
	n, _ := strconv.Atoi(out)
	p.hasFileCopy = err == nil && n >= 150000
	p.verKnown = true
	return p.hasFileCopy
}

func (p *Postgres) Drop(ctx context.Context, name string) error {
	if err := checkDroppable(name); err != nil {
		return err
	}
	_, err := p.run(ctx, "dropdb", "--if-exists", "--force", name)
	return err
}

// Snapshot copies the live database from to name, for a flow checkpoint. PostgreSQL refuses to copy a database
// that has sessions, so new connections to from are refused and the open ones terminated for the length of the
// copy (the application's pool reconnects once they are allowed again). Like a template, the copy is made under a
// temporary name, marked finished and only then renamed: a kill or a cancelled context never leaves something under
// name that Complete reports as finished. An already finished name is left alone.
func (p *Postgres) Snapshot(ctx context.Context, from, name string) error {
	if err := checkIdent(from); err != nil {
		return err
	}
	if err := checkDroppable(name); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s_b%d", name, time.Now().UnixNano())
	if len(tmp) > maxName {
		return fmt.Errorf("snapshot name %q is too long", name)
	}
	unlock, err := p.once.lockIn(ctx, p.LockDir, name)
	if err != nil {
		return err
	}
	defer unlock()
	if ok, err := p.isComplete(ctx, name); err != nil || ok {
		return err
	}
	if err := p.dropDetached(ctx, name); err != nil { // unfinished leftover, if any
		return err
	}
	err = p.copyLive(ctx, from, tmp)
	if err == nil {
		_, err = p.run(ctx, "psql", "-At", "-c", fmt.Sprintf("comment on database %s is '%s'", tmp, complete))
	}
	if err == nil {
		err = p.rename(ctx, tmp, name)
	}
	if err != nil {
		// Another process may have finished the same snapshot meanwhile.
		ok, _ := p.isComplete(context.WithoutCancel(ctx), name)
		_ = p.dropDetached(ctx, tmp)
		if ok {
			return nil
		}
		return fmt.Errorf("snapshot %s: %w", name, err)
	}
	return nil
}

// copyLive clones the live database from into to while nobody is connected to from.
func (p *Postgres) copyLive(ctx context.Context, from, to string) error {
	// Refusing new connections keeps a reconnecting pool from slipping in between the terminate and the copy. A
	// role that does not own the database cannot do it: the retries below then have to win the race.
	_, blocked := p.run(ctx, "psql", "-At", "-c", fmt.Sprintf("alter database %s with allow_connections false", from))
	if blocked == nil {
		defer func() {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			_, _ = p.run(dctx, "psql", "-At", "-c", fmt.Sprintf("alter database %s with allow_connections true", from))
		}()
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(200 * time.Millisecond):
			}
		}
		_, _ = p.run(ctx, "psql", "-At", "-c", fmt.Sprintf(
			"select pg_terminate_backend(pid) from pg_stat_activity where datname = '%s' and pid <> pg_backend_pid()", from))
		if err = p.Clone(ctx, from, to); err == nil {
			return nil
		}
		_ = p.dropDetached(ctx, to)
	}
	return err
}

// Complete reports whether database name exists and carries the completion mark.
func (p *Postgres) Complete(ctx context.Context, name string) (bool, error) {
	if err := checkIdent(name); err != nil {
		return false, err
	}
	return p.isComplete(ctx, name)
}

// Size is the on-disk size of database name in bytes, 0 when it does not exist.
func (p *Postgres) Size(ctx context.Context, name string) (int64, error) {
	if err := checkIdent(name); err != nil {
		return 0, err
	}
	out, err := p.run(ctx, "psql", "-At", "-c", fmt.Sprintf(
		"select coalesce((select pg_database_size(datname) from pg_database where datname = '%s'), 0)", name))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(out, 10, 64)
}
