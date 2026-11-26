package store

// migrations run in order; the number of applied ones lives in PRAGMA user_version.
// Append new entries, never edit an applied one.
var migrations = []string{
	`
CREATE TABLE runs (
	id           TEXT PRIMARY KEY,
	suite        TEXT NOT NULL,
	kind         TEXT NOT NULL,
	heads        TEXT,
	"trigger"    TEXT NOT NULL DEFAULT '',
	requested_by TEXT NOT NULL DEFAULT '',
	state        TEXT NOT NULL,
	reason       TEXT NOT NULL DEFAULT '',
	created      INTEGER NOT NULL,
	started      INTEGER NOT NULL DEFAULT 0,
	finished     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX runs_created ON runs(created);
CREATE INDEX runs_state ON runs(state);

CREATE TABLE shards (
	run      TEXT NOT NULL,
	n        INTEGER NOT NULL,
	state    TEXT NOT NULL,
	worker   TEXT NOT NULL DEFAULT '',
	log_path TEXT NOT NULL DEFAULT '',
	cost_cpu REAL NOT NULL DEFAULT 0,
	cost_mem INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run, n)
);

CREATE TABLE tests (
	run      TEXT NOT NULL,
	shard    INTEGER NOT NULL DEFAULT 0,
	test     TEXT NOT NULL,
	file     TEXT NOT NULL DEFAULT '',
	title    TEXT NOT NULL DEFAULT '',
	line     INTEGER NOT NULL DEFAULT 0,
	status   TEXT NOT NULL,
	ms       INTEGER NOT NULL DEFAULT 0,
	retry    INTEGER NOT NULL DEFAULT 0,
	failure  TEXT,
	class    TEXT NOT NULL DEFAULT '',
	evidence TEXT,
	PRIMARY KEY (run, test, retry)
);

CREATE TABLE steps (
	run    TEXT NOT NULL,
	test   TEXT NOT NULL,
	step   TEXT NOT NULL,
	parent TEXT NOT NULL DEFAULT '',
	title  TEXT NOT NULL DEFAULT '',
	kind   TEXT NOT NULL DEFAULT '',
	start  INTEGER NOT NULL DEFAULT 0,
	ms     INTEGER NOT NULL DEFAULT 0,
	ok     INTEGER,
	PRIMARY KEY (run, test, step)
);

CREATE TABLE artifacts (
	id   INTEGER PRIMARY KEY AUTOINCREMENT,
	run  TEXT NOT NULL,
	test TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	path TEXT NOT NULL,
	UNIQUE (run, test, kind, path)
);

CREATE TABLE events (
	seq   INTEGER PRIMARY KEY AUTOINCREMENT,
	ts    INTEGER NOT NULL,
	run   TEXT NOT NULL DEFAULT '',
	shard INTEGER NOT NULL DEFAULT 0,
	kind  TEXT NOT NULL,
	test  TEXT NOT NULL DEFAULT '',
	step  TEXT NOT NULL DEFAULT '',
	data  TEXT
);
CREATE INDEX events_run ON events(run, seq);

CREATE TABLE durations (
	id   INTEGER PRIMARY KEY AUTOINCREMENT,
	test TEXT NOT NULL,
	ms   INTEGER NOT NULL,
	ts   INTEGER NOT NULL
);
CREATE INDEX durations_test ON durations(test, id);

CREATE TABLE serial (
	suite    TEXT NOT NULL,
	test     TEXT NOT NULL,
	evidence TEXT,
	since    INTEGER NOT NULL,
	PRIMARY KEY (suite, test)
);
`,
	`
CREATE TABLE checkpoints (
	suite   TEXT NOT NULL,
	key     TEXT NOT NULL,
	flow    TEXT NOT NULL,
	db      TEXT NOT NULL DEFAULT '',
	bytes   INTEGER NOT NULL DEFAULT 0,
	created INTEGER NOT NULL,
	used    INTEGER NOT NULL,
	PRIMARY KEY (suite, key)
);
`,
	`
CREATE TABLE writes (
	suite  TEXT NOT NULL,
	test   TEXT NOT NULL,
	tables TEXT NOT NULL,
	reads  TEXT NOT NULL,
	ts     INTEGER NOT NULL,
	PRIMARY KEY (suite, test)
);
`,
	`
ALTER TABLE runs ADD COLUMN fork     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN affected INTEGER NOT NULL DEFAULT 0;

CREATE TABLE coverage_map (
	suite   TEXT NOT NULL,
	test    TEXT NOT NULL,
	files   TEXT NOT NULL,
	commits TEXT NOT NULL,
	ts      INTEGER NOT NULL,
	PRIMARY KEY (suite, test)
);
`,
	`
ALTER TABLE runs ADD COLUMN infra TEXT NOT NULL DEFAULT '';
`,
	`
-- Whether a test's write set was last replaced by a run on every repository's default branch. Sets learned before
-- this column existed are not trusted: the next default-branch run replaces them.
ALTER TABLE writes ADD COLUMN trusted INTEGER NOT NULL DEFAULT 0;
`, `
-- Migration 7 (T-20): what the nightly exploratory agent found. Steps are a JSON array of printable lines; screenshot
-- and test are paths inside the run's log directory, or empty.
CREATE TABLE findings (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	run        TEXT NOT NULL,
	suite      TEXT NOT NULL,
	goal       TEXT NOT NULL DEFAULT '',
	severity   TEXT NOT NULL,
	title      TEXT NOT NULL,
	steps      TEXT NOT NULL DEFAULT '[]',
	screenshot TEXT NOT NULL DEFAULT '',
	test       TEXT NOT NULL DEFAULT '',
	ts         INTEGER NOT NULL
);
CREATE INDEX findings_run ON findings(run);
`, `
-- Migration 8 (T-19): networked workers. Join tokens are single use and stored hashed; a worker's long-lived credential
-- is stored hashed too. A revoked worker keeps its row so its name cannot be taken over.
CREATE TABLE join_tokens (
	hash    TEXT PRIMARY KEY,
	created INTEGER NOT NULL,
	used    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE workers (
	name      TEXT PRIMARY KEY,
	cred_hash TEXT NOT NULL UNIQUE,
	capacity  INTEGER NOT NULL DEFAULT 1,
	created   INTEGER NOT NULL,
	revoked   INTEGER NOT NULL DEFAULT 0,
	last_seen INTEGER NOT NULL DEFAULT 0
);
`, `
-- Migration 9: why a failed run failed, when the CLI's exit code depends on it (see Run.Outcome).
ALTER TABLE runs ADD COLUMN outcome TEXT NOT NULL DEFAULT '';
`,
}
