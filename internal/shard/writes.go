package shard

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Write sets are measured with a log the core installs in the database it measures: statement-level triggers on
// every user table append the table's name to localci.writes. Postgres's own counters cannot give this
// (pg_stat_user_tables shows a write from an idle connection up to about 10 s late), so they are used only as a
// hint for reads, which can then only be late, never wrong.

// DefaultLogSchema is the schema the core adds to a measured database unless the suite names another with
// `writes.schema`; it never appears in a write or read set.
const DefaultLogSchema = "localci"

// logMark is the comment the core puts on its log schema. Install refuses a schema of that name without it: it
// belongs to the app.
const logMark = "localci"

var logSchemaRE = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,47}$`)

// CheckLogSchema says whether name can be the write log's schema: a plain lowercase identifier that is not one
// Postgres owns.
func CheckLogSchema(name string) error {
	if !logSchemaRE.MatchString(name) || strings.HasPrefix(name, "pg_") || name == "public" || name == "information_schema" {
		return fmt.Errorf("invalid schema name %q: use lowercase letters, digits and underscores (at most 48), not pg_*, public or information_schema", name)
	}
	return nil
}

// SchemaTakenError says the database already has a schema of the log's name that localci did not make.
type SchemaTakenError struct{ Schema string }

func (e *SchemaTakenError) Error() string {
	return fmt.Sprintf("the database already has a schema named %q that is not localci's write log", e.Schema)
}

const takenMarker = "localci_schema_taken:"

// installSQL makes the write log in schema %[1]s; every statement is safe to run again. Tables get their trigger
// when they have none, so a database restored from a checkpoint without the log is covered at the next call, and
// an event trigger gives one to every table created later (it needs a superuser; without one the creation
// is not logged and Measure compares the table lists instead). The functions are security definer: an app role that
// cannot touch the schema still writes through them. Each row keeps the id of the transaction that wrote it: the
// order of seq is not the order of commits.
const installSQL = `
do $m$ begin
  if exists (select 1 from pg_namespace where nspname = '%[1]s')
     and coalesce(obj_description((select oid from pg_namespace where nspname = '%[1]s'), 'pg_namespace'), '') <> '%[2]s' then
    raise exception '` + takenMarker + `%[1]s';
  end if;
end $m$;
create schema if not exists %[1]s;
comment on schema %[1]s is '%[2]s';
create table if not exists %[1]s.writes(seq bigserial primary key, xid xid8 not null default pg_current_xact_id(), tbl text not null);
create or replace function %[1]s.note() returns trigger language plpgsql security definer
  set search_path = pg_catalog as $f$
begin insert into %[1]s.writes(tbl) values (TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME); return null; end $f$;
do $d$ declare r record; begin
  for r in select n.nspname s, c.relname t from pg_class c join pg_namespace n on n.oid = c.relnamespace
    where c.relkind in ('r', 'p') and n.nspname <> '%[1]s' and n.nspname <> 'information_schema'
      and n.nspname !~ '^pg_' and not exists (select 1 from pg_trigger g where g.tgrelid = c.oid and g.tgname = '%[1]s_w')
  loop
    execute format('create trigger %[1]s_w after insert or update or delete or truncate on %%I.%%I for each statement execute function %[1]s.note()', r.s, r.t);
  end loop;
end $d$;
create or replace function %[1]s.ddl() returns event_trigger language plpgsql security definer
  set search_path = pg_catalog as $f$
declare r record;
begin
  for r in select schema_name, objid from pg_event_trigger_ddl_commands()
    where object_type = 'table' and schema_name <> '%[1]s' and schema_name <> 'information_schema' and schema_name !~ '^pg_'
  loop
    execute format('create trigger %[1]s_w after insert or update or delete or truncate on %%s for each statement execute function %[1]s.note()', r.objid::regclass);
    insert into %[1]s.writes(tbl) values (r.schema_name || '.' || (select relname from pg_class where oid = r.objid));
  end loop;
end $f$;
do $e$ begin
  if not exists (select 1 from pg_event_trigger where evtname = '%[1]s_ddl') then
    create event trigger %[1]s_ddl on ddl_command_end when tag in ('CREATE TABLE', 'CREATE TABLE AS', 'SELECT INTO')
      execute function %[1]s.ddl();
  end if;
exception when insufficient_privilege then null;
end $e$;
`

// WriteRow is one logged write: the position in the log, the transaction that wrote it and the table.
type WriteRow struct {
	Seq   int64
	Xid   int64
	Table string
}

// Counter is what Postgres counted for one table: rows written and scans started.
type Counter struct{ Writes, Reads int64 }

// Counters maps a table (schema omitted when it is public) to its counters.
type Counters map[string]Counter

// Observation is one look at a database: where the log ends, the rows logged after a position, the counters and,
// when asked, the tables there are.
type Observation struct {
	Seq      int64 // the highest sequence number committed
	Xmin     int64 // the oldest transaction still running (or the next one): everything below it has finished
	Writes   []WriteRow
	Counters Counters
	DDL      bool            // the log sees tables created later (the event trigger is there)
	Tables   map[string]bool // every user table, when asked; nil otherwise
}

// ObserveOpts says what Observe does.
type ObserveOpts struct {
	Install  bool   // make sure the log exists first (the core's own clone only, never a template)
	Schema   string // the log's schema; DefaultLogSchema when empty
	Since    int64  // return the rows logged after this sequence number; math.MaxInt64 for none
	XidFrom  int64  // when set, return the rows written by transactions from this id on instead of by Since
	Counters bool   // also read the per-table counters
	Tables   bool   // also list the user tables
}

// NoRows is ObserveOpts.Since for "only tell me where the log ends".
const NoRows = math.MaxInt64

// Observe reads the write log and, when asked, the counters, in one psql call.
func (p *Postgres) Observe(ctx context.Context, db string, o ObserveOpts) (Observation, error) {
	if err := checkIdent(db); err != nil {
		return Observation{}, err
	}
	if p.Container == "" && p.URL == "" {
		return Observation{}, fmt.Errorf("no valid connection url configured")
	}
	schema := cmp.Or(o.Schema, DefaultLogSchema)
	if err := CheckLogSchema(schema); err != nil {
		return Observation{}, err
	}
	where := fmt.Sprintf("seq > %d", o.Since)
	if o.XidFrom > 0 {
		where = fmt.Sprintf("xid >= '%d'::xid8", o.XidFrom)
	}
	sql := fmt.Sprintf(`select 's', coalesce(max(seq), 0)::text, pg_snapshot_xmin(pg_current_snapshot())::text,
  (exists (select 1 from pg_event_trigger where evtname = '%[1]s_ddl'))::int::text from %[1]s.writes
union all select 'w', seq::text, tbl, xid::text from %[1]s.writes where %[2]s`, schema, where)
	if o.Counters {
		sql += fmt.Sprintf(`
union all select 'c', schemaname || '.' || relname, (coalesce(n_tup_ins + n_tup_upd + n_tup_del, 0))::text,
  (coalesce(seq_scan, 0) + coalesce(idx_scan, 0))::text from pg_stat_user_tables where schemaname <> '%s'`, schema)
	}
	if o.Tables {
		sql += fmt.Sprintf(`
union all select 'u', '', '', ''
union all select 't', n.nspname || '.' || c.relname, '', '' from pg_class c join pg_namespace n on n.oid = c.relnamespace
  where c.relkind in ('r', 'p') and n.nspname <> '%s' and n.nspname <> 'information_schema' and n.nspname !~ '^pg_'`, schema)
	}
	if o.Install {
		sql = fmt.Sprintf(installSQL, schema, logMark) + sql
	}
	out, err := p.command(ctx, "psql", db, "-At", "-q", "-F", "\t", "-c", sql).CombinedOutput()
	if err != nil {
		if i := strings.Index(string(out), takenMarker); i >= 0 {
			return Observation{}, &SchemaTakenError{Schema: schema}
		}
		return Observation{}, fmt.Errorf("psql: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseObservation(string(out)), nil
}

// LogSchemaTaken says whether db has a schema of the log's name that localci did not make (it carries no mark).
// It only reads, so it is safe on a template.
func (p *Postgres) LogSchemaTaken(ctx context.Context, db, schema string) (bool, error) {
	if err := checkIdent(db); err != nil {
		return false, err
	}
	if err := CheckLogSchema(schema); err != nil {
		return false, err
	}
	if p.Container == "" && p.URL == "" {
		return false, fmt.Errorf("no valid connection url configured")
	}
	sql := fmt.Sprintf(`select exists (select 1 from pg_namespace where nspname = '%[1]s'
  and coalesce(obj_description(oid, 'pg_namespace'), '') <> '%[2]s')`, schema, logMark)
	out, err := p.command(ctx, "psql", db, "-At", "-q", "-c", sql).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("psql: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) == "t", nil
}

// IsSchemaTaken reports whether err says the log's schema belongs to the app.
func IsSchemaTaken(err error) (*SchemaTakenError, bool) {
	var e *SchemaTakenError
	ok := errors.As(err, &e)
	return e, ok
}

// parseObservation reads the tab-separated lines Observe asks for and skips anything else.
func parseObservation(out string) Observation {
	o := Observation{Counters: Counters{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) != 4 {
			continue
		}
		switch f[0] {
		case "s":
			o.Seq, _ = strconv.ParseInt(f[1], 10, 64)
			o.Xmin, _ = strconv.ParseInt(f[2], 10, 64)
			o.DDL = f[3] == "1"
		case "w":
			if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				xid, _ := strconv.ParseInt(f[3], 10, 64)
				o.Writes = append(o.Writes, WriteRow{Seq: n, Xid: xid, Table: tableName(f[2])})
			}
		case "u":
			o.Tables = map[string]bool{}
		case "t":
			if o.Tables != nil {
				o.Tables[tableName(f[1])] = true
			}
		case "c":
			w, err1 := strconv.ParseInt(f[2], 10, 64)
			r, err2 := strconv.ParseInt(f[3], 10, 64)
			if err1 == nil && err2 == nil {
				o.Counters[tableName(f[1])] = Counter{Writes: w, Reads: r}
			}
		}
	}
	return o
}

func tableName(t string) string { return strings.TrimPrefix(t, "public.") }

// Access is what a test touches: the tables it writes and the tables it reads (sorted, a table it writes counts
// as read too).
type Access struct{ Writes, Reads []string }

// Measure is the access of the test that ran between two observations of a database it had to itself. A row
// belongs to the test when the transaction that wrote it is not older than the first observation's oldest running
// transaction: the sequence number says nothing about commit order, so a transaction that took a low number and
// committed after a look would fall between two tests, while a transaction id never does. A transaction still open
// at the second look holds the next test's start back, so its rows are counted by both tests: too much, never too
// little. A table that appeared between the observations counts as written when the log could not see it being
// created.
func Measure(begin, end Observation) Access {
	writes := map[string]bool{}
	byXid := begin.Xmin > 0
	for _, w := range end.Writes {
		if (byXid && w.Xid >= begin.Xmin) || (!byXid && w.Seq > begin.Seq) {
			writes[w.Table] = true
		}
	}
	if !end.DDL && begin.Tables != nil && end.Tables != nil {
		for t := range end.Tables {
			if !begin.Tables[t] {
				writes[t] = true
			}
		}
	}
	reads := map[string]bool{}
	for t, a := range end.Counters {
		if a.Reads > begin.Counters[t].Reads {
			reads[t] = true
		}
	}
	for t := range writes {
		reads[t] = true
	}
	return Access{Writes: sortedKeys(writes), Reads: sortedKeys(reads)}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Union is the access of a test known from several runs: a set that shrank must not let two tests that once
// conflicted run together.
func (a Access) Union(b Access) Access {
	merge := func(x, y []string) []string {
		m := map[string]bool{}
		for _, t := range x {
			m[t] = true
		}
		for _, t := range y {
			m[t] = true
		}
		return sortedKeys(m)
	}
	return Access{Writes: merge(a.Writes, b.Writes), Reads: merge(a.Reads, b.Reads)}
}

// Span is where in the log a test ran: it began after position From and, unless Open, ended at To.
type Span struct {
	From, To int64
	Open     bool
}

// Suspect is a test that may have written a table nobody expected, next to the tests in Overlap.
type Suspect struct {
	Test        TestID
	Unexplained []string
	Overlap     []TestID
}

// Suspects attributes every logged row of a shared database to the tests whose span contains it. A table outside
// the union of those tests' known write sets flags all of them: with several tests running at once the log cannot
// say which one wrote it. A row nobody was running for (a background job, the seed) flags nobody, and neither does
// a row inside a quiet span: the core's own setup (the tenant flow) is not a test's write.
func Suspects(rows []WriteRow, spans map[TestID]Span, known map[TestID]Access, quiet ...Span) []Suspect {
	unexplained := map[TestID]map[string]bool{}
	overlap := map[TestID]map[TestID]bool{}
	ids := make([]TestID, 0, len(spans))
	for id := range spans {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, r := range rows {
		if slices.ContainsFunc(quiet, func(q Span) bool { return r.Seq > q.From && (q.Open || r.Seq <= q.To) }) {
			continue
		}
		var in []TestID
		for _, id := range ids {
			if s := spans[id]; r.Seq > s.From && (s.Open || r.Seq <= s.To) {
				in = append(in, id)
			}
		}
		if len(in) == 0 {
			continue
		}
		explained := false
		for _, id := range in {
			if slices.Contains(known[id].Writes, r.Table) {
				explained = true
				break
			}
		}
		if explained {
			continue
		}
		for _, id := range in {
			if unexplained[id] == nil {
				unexplained[id], overlap[id] = map[string]bool{}, map[TestID]bool{}
			}
			unexplained[id][r.Table] = true
			for _, o := range in {
				if o != id {
					overlap[id][o] = true
				}
			}
		}
	}
	var out []Suspect
	for _, id := range ids {
		if u := unexplained[id]; u != nil {
			var with []TestID
			for o := range overlap[id] {
				with = append(with, o)
			}
			slices.Sort(with)
			out = append(out, Suspect{Test: id, Unexplained: sortedKeys(u), Overlap: with})
		}
	}
	return out
}

// Confirm says whether the test, run alone, wrote one of the tables that were unexplained in the shared window.
func (s Suspect) Confirm(aloneWrites []string) bool {
	for _, t := range s.Unexplained {
		if slices.Contains(aloneWrites, t) {
			return true
		}
	}
	return false
}
