package suite

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
	"gopkg.in/yaml.v3"
)

// Isolation levels beyond isolated (see the roadmap's Contracts): shards share one database when the tables their
// tests write are disjoint. The core measures what each test writes with a log it installs in the database the
// test runs in (shard.Postgres.Observe); a suite whose engine cannot be measured sets `writes: report` and its
// reporter says it. This file owns the suite keys, the hooks and the run-time steps; the pure parts live in
// package shard.

const (
	writesCounters = "counters" // the core measures (default)
	writesReport   = "report"   // the report says
)

// writesNode is the suite's `writes:` value from its raw node: a word (`writes: report`) or a mapping
// (`writes: {mode: report, schema: my_log}`).
func writesNode(s *Suite) *yaml.Node {
	if s.Raw != nil && s.Raw.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(s.Raw.Content); i += 2 {
			if s.Raw.Content[i].Value == "writes" {
				return s.Raw.Content[i+1]
			}
		}
	}
	return nil
}

func writesKey(s *Suite, key string) (string, bool) {
	n := writesNode(s)
	if n == nil || n.Kind != yaml.MappingNode {
		return "", false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1].Value, true
		}
	}
	return "", false
}

// writesMode reads how the suite's write sets are learned: counters (the default) or report.
func writesMode(s *Suite) string {
	n := writesNode(s)
	switch {
	case n == nil:
		return writesCounters
	case n.Kind == yaml.MappingNode:
		if m, ok := writesKey(s, "mode"); ok {
			return m
		}
		return writesCounters
	}
	return n.Value
}

// writesSchema is the schema of the write log the core adds to the database it measures: `writes.schema`, by
// default one named localci.
func writesSchema(s *Suite) string {
	if v, ok := writesKey(s, "schema"); ok {
		return v
	}
	return shard.DefaultLogSchema
}

// checkIsolationKeys validates what the isolation levels need; Parse calls it once the suite is read.
func checkIsolationKeys(s *Suite) error {
	if m := writesMode(s); m != writesCounters && m != writesReport {
		return fmt.Errorf("writes: %q is not known (use counters or report)", m)
	}
	if err := shard.CheckLogSchema(writesSchema(s)); err != nil {
		return fmt.Errorf("writes.schema: %w", err)
	}
	if s.Isolation == shard.LevelSharedDB && s.Database == nil {
		return errors.New("isolation: shared-db needs a database section")
	}
	return nil
}

// isoRun is what one run keeps for its isolation level.
type isoRun struct {
	pg      *shard.Postgres // nil: the engine cannot be measured
	measure bool            // learn write sets where one test at a time touches a database
	report  bool            // `writes: report`
	schema  string          // the write log's schema (`writes.schema`)
	known   map[shard.TestID]shard.Access
	trusted map[shard.TestID]bool // known sets last replaced by a run on every repository's default branch
	shared  string                // the shared database, "" when none
	nShared int                   // shards 1..nShared use it
	tenant  string                // the tenant flow when the run uses tenant isolation (isolation_tenant.go)
	stack   *shardEnv

	mu       sync.Mutex
	begin    map[string]shard.Observation // test -> observation at its begin, own database
	last     map[string]int64             // database -> the oldest running transaction at the last test's end
	spans    map[shard.TestID]shard.Span  // shared database: where in the log each test ran
	alone    map[string][]string          // test -> tables written when run alone (shard 0)
	open     map[string]int64             // shared database: the log position where a tenant flow began, by shard and step
	quiet    map[string]shard.Span        // shared database: where the tenant flow ran, by shard and step; closed spans only
	suspects []shard.Suspect
}

// logSchema is the schema the run's write log lives in.
func (i *isoRun) logSchema() string { return cmp.Or(i.schema, shard.DefaultLogSchema) }

func (r *Runner) execFor(run string) *execution {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[run]
}

// isoBegin and isoEnd are the OnTest hooks (installed by New).
func (r *Runner) isoBegin(tc TestCtx) {
	ex := r.execFor(tc.Run)
	if ex == nil || ex.iso == nil || ex.iso.pg == nil || tc.DB == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ex.ctx), 30*time.Second)
	defer cancel()
	iso := ex.iso
	if tc.DB == iso.shared {
		o, err := iso.pg.Observe(ctx, tc.DB, shard.ObserveOpts{Schema: iso.logSchema(), Since: shard.NoRows})
		if err != nil {
			ex.emit(tc.Shard, "db.writes.failed", tc.Test, "", map[string]string{"error": trim(err.Error(), 300)})
			return
		}
		iso.mu.Lock()
		if _, ok := iso.spans[shard.TestID(tc.Test)]; !ok {
			iso.spans[shard.TestID(tc.Test)] = shard.Span{From: o.Seq, Open: true}
		}
		iso.mu.Unlock()
		return
	}
	if !iso.measure {
		return
	}
	// One test at a time touches this database: what changes is the test's.
	o, err := iso.pg.Observe(ctx, tc.DB, shard.ObserveOpts{Install: true, Schema: iso.logSchema(), Since: shard.NoRows, Counters: true, Tables: true})
	if err != nil {
		ex.emit(tc.Shard, "db.writes.failed", tc.Test, "", map[string]string{"error": trim(err.Error(), 300)})
		return
	}
	iso.mu.Lock()
	// Rows of a transaction still open when the previous test ended were not in that test's window: this one
	// starts where the last one stopped, so they are counted (for the next test, never for nobody).
	if last := iso.last[tc.DB]; last > 0 && last < o.Xmin {
		o.Xmin = last
	}
	iso.begin[tc.Test] = o
	iso.mu.Unlock()
}

func (r *Runner) isoEnd(tc TestCtx) {
	ex := r.execFor(tc.Run)
	if ex == nil || ex.iso == nil || ex.iso.pg == nil || tc.DB == "" {
		return
	}
	iso := ex.iso
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ex.ctx), 30*time.Second)
	defer cancel()
	if tc.DB == iso.shared {
		o, err := iso.pg.Observe(ctx, tc.DB, shard.ObserveOpts{Schema: iso.logSchema(), Since: shard.NoRows})
		if err != nil {
			return // the span stays open: more tests count as overlapping, never fewer
		}
		iso.mu.Lock()
		id := shard.TestID(tc.Test)
		iso.spans[id] = shard.Span{From: iso.spans[id].From, To: o.Seq}
		iso.mu.Unlock()
		return
	}
	iso.mu.Lock()
	begin, ok := iso.begin[tc.Test]
	delete(iso.begin, tc.Test)
	iso.mu.Unlock()
	if !ok {
		return
	}
	end, err := iso.pg.Observe(ctx, tc.DB, shard.ObserveOpts{Schema: iso.logSchema(), Since: begin.Seq, XidFrom: begin.Xmin, Counters: true, Tables: true})
	if err != nil {
		ex.emit(tc.Shard, "db.writes.failed", tc.Test, "", map[string]string{"error": trim(err.Error(), 300)})
		return
	}
	iso.mu.Lock()
	if iso.last == nil {
		iso.last = map[string]int64{}
	}
	iso.last[tc.DB] = end.Xmin
	iso.mu.Unlock()
	ex.learned(tc.Shard, tc.Test, shard.Measure(begin, end))
}

// writeLearning pairs what a test was measured to write with how the attempt ended: a set is stored only for a
// test that passed. The measurement (the end hook) and the outcome (the report's test.end) arrive in either order.
type writeLearning struct {
	mu      sync.Mutex
	got     map[string]shard.Access
	outcome map[string]string
	once    sync.Once
	replace bool
}

// learned announces what a test touches and offers it for storing. Shard 0 is the serial group and the reruns:
// what a suspect wrote alone is what confirms it, whatever way the test ended.
func (ex *execution) learned(shardN int, test string, a shard.Access) {
	iso := ex.iso
	if shardN == 0 {
		iso.mu.Lock()
		iso.alone[test] = shard.Access{Writes: iso.alone[test]}.Union(a).Writes
		iso.mu.Unlock()
	}
	ex.emit(shardN, "db.writes", test, "", map[string]any{"tables": nonNil(a.Writes), "reads": nonNil(a.Reads)})
	ex.offerWrites(test, a)
}

// offerWrites hands over what a test was measured (or reported) to write; it is stored once the attempt is known
// to have passed. A fork's sets, and an infrastructure run's, are shown in the run, never stored (learns).
func (ex *execution) offerWrites(test string, a shard.Access) {
	if !ex.learns() {
		return
	}
	ex.wr.mu.Lock()
	if ex.wr.got == nil {
		ex.wr.got, ex.wr.outcome = map[string]shard.Access{}, map[string]string{}
	}
	ex.wr.got[test] = a
	ex.wr.mu.Unlock()
	ex.settleWrites(test)
}

// testOutcome records how an attempt of a test ended (the report's status).
func (ex *execution) testOutcome(test, status string) {
	if !ex.learns() || (ex.iso == nil && writesMode(ex.suite) != writesReport) {
		return
	}
	ex.wr.mu.Lock()
	if ex.wr.outcome == nil {
		ex.wr.got, ex.wr.outcome = map[string]shard.Access{}, map[string]string{}
	}
	ex.wr.outcome[test] = status
	ex.wr.mu.Unlock()
	ex.settleWrites(test)
}

func (ex *execution) settleWrites(test string) {
	ex.wr.mu.Lock()
	a, haveSet := ex.wr.got[test]
	status, haveStatus := ex.wr.outcome[test]
	if haveSet && haveStatus {
		delete(ex.wr.got, test)
		delete(ex.wr.outcome, test)
	}
	ex.wr.mu.Unlock()
	if !haveSet || !haveStatus || status != "passed" {
		return
	}
	if err := ex.r.d.Store.PutWrites(ex.bg, store.WriteSet{Suite: ex.suite.Name, Test: test, Tables: a.Writes, Reads: a.Reads, Replace: ex.replaces()}); err != nil {
		ex.emit(0, "db.writes.failed", test, "", map[string]string{"error": trim(err.Error(), 300)})
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// reportedWrites handles a `writes` line of the report: it is used only when the suite says `writes: report`
// (engines without a log to measure); anywhere else the line stays an ordinary stored event. Only the write set
// is known then, so conflicts are write-write. It is stored like a measured set: when the test passed.
func (i *ingest) reportedWrites(b *report.Writes) {
	ex := i.ex
	if writesMode(ex.suite) != writesReport || !ex.learns() {
		return
	}
	ex.mu.Lock()
	_, listed := ex.tests[b.Test]
	ex.mu.Unlock()
	if !listed || len(b.Tables) > 500 {
		return
	}
	var tables []string
	for _, t := range b.Tables {
		if t != "" && len(t) <= 128 && !strings.ContainsAny(t, " \t\r\n\x00") {
			tables = append(tables, t)
		}
	}
	slices.Sort(tables)
	tables = slices.Compact(tables)
	ex.emit(i.n, "db.writes", b.Test, "", map[string]any{"tables": nonNil(tables), "reads": []string{}})
	ex.offerWrites(b.Test, shard.Access{Writes: tables})
}

// tenantFlowBegin and tenantFlowEnd mark where in the shared database's log the tenant flow ran. What the flow
// writes is the core's setup, not any test's: rows inside the window flag no suspect. The window is closed only
// when both looks at the log worked; otherwise the flow's rows stay in the spans and may flag tests, the safe side.
func (i *ingest) tenantFlowBegin(b *report.FlowBegin) {
	ex := i.ex
	iso := ex.iso
	if iso == nil || iso.pg == nil || iso.shared == "" || iso.tenant == "" || b.Flow != iso.tenant || i.n < 1 || i.n > iso.nShared {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ex.ctx), 30*time.Second)
	defer cancel()
	o, err := iso.pg.Observe(ctx, iso.shared, shard.ObserveOpts{Schema: iso.logSchema(), Since: shard.NoRows})
	if err != nil {
		return
	}
	iso.mu.Lock()
	iso.open[fmt.Sprint(i.n, ":", b.Step)] = o.Seq
	iso.mu.Unlock()
}

func (i *ingest) tenantFlowEnd(b *report.FlowEnd) {
	ex := i.ex
	iso := ex.iso
	if iso == nil || iso.pg == nil || iso.shared == "" {
		return
	}
	key := fmt.Sprint(i.n, ":", b.Step)
	iso.mu.Lock()
	from, ok := iso.open[key]
	delete(iso.open, key)
	iso.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ex.ctx), 30*time.Second)
	defer cancel()
	o, err := iso.pg.Observe(ctx, iso.shared, shard.ObserveOpts{Schema: iso.logSchema(), Since: shard.NoRows})
	if err != nil {
		return
	}
	iso.mu.Lock()
	iso.quiet[key] = shard.Span{From: from, To: o.Seq}
	iso.mu.Unlock()
}

// applyLevel re-plans the run for the suite's isolation level, after shard.Plan. It returns the layout to run
// and sets up ex.iso; with isolation: isolated it changes nothing. n is the shard count the plan used.
func (ex *execution) applyLevel(ids []shard.TestID, medians map[shard.TestID]time.Duration, serial map[shard.TestID]bool, n int) error {
	s := ex.suite
	if s.Isolation != shard.LevelSharedDB && s.Isolation != shard.LevelTenant && s.Isolation != "auto" {
		return nil
	}
	iso := &isoRun{report: writesMode(s) == writesReport, schema: writesSchema(s),
		begin: map[string]shard.Observation{}, last: map[string]int64{}, spans: map[shard.TestID]shard.Span{}, alone: map[string][]string{},
		open: map[string]int64{}, quiet: map[string]shard.Span{}}
	if ex.db != nil {
		iso.pg, _ = ex.db.drv.DB.(*shard.Postgres)
	}
	measurable := iso.pg != nil || iso.report
	iso.measure = iso.pg != nil && !iso.report

	stored, err := ex.r.d.Store.Writes(ex.bg, s.Name)
	if err != nil {
		return err
	}
	iso.known = map[shard.TestID]shard.Access{}
	iso.trusted = map[shard.TestID]bool{}
	for _, id := range ids {
		if w, ok := stored[string(id)]; ok {
			iso.trusted[id] = w.Trusted
			a := shard.Access{Writes: w.Tables, Reads: w.Reads}
			if iso.report {
				a.Reads = nil // only writes are known: conflicts are write-write
			}
			iso.known[id] = a
		}
	}
	ex.iso = iso

	level := s.Isolation
	if level == "auto" {
		var parallel []shard.TestID
		for _, id := range ids {
			if !serial[id] {
				parallel = append(parallel, id)
			}
		}
		c := ex.r.d.Sched.Capacity()
		per := ex.shardCost()
		ch := shard.Choose(shard.AutoInput{
			Machine: capacity.Machine{CPUs: c.CPUs, FreeMemory: c.Memory, DockerMemory: ex.r.d.Sched.DockerMemory()},
			Cost:    shard.Cost{CPUs: per.CPUs, Memory: per.Memory}, Tests: parallel, Known: iso.known, Trusted: iso.trusted,
			Measured: measurable, Fixed: s.Shards, Tenant: tenantAvailable(s)})
		level, n = ch.Level, ch.Shards
	}
	var sharedShards, isolatedShards int
	switch {
	case level == shard.LevelTenant:
		ex.planTenant(ids, medians, serial, n)
		sharedShards = iso.nShared
	case level == shard.LevelSharedDB && measurable:
		l := shard.PlanShared(ids, medians, iso.known, n, serial)
		ex.layout = shard.Layout{Shards: append(slices.Clone(l.Shared), l.Isolated...), Serial: l.Serial, Makespan: l.Makespan, Alone: l.Alone}
		iso.nShared = len(l.Shared)
		sharedShards, isolatedShards = len(l.Shared), len(l.Isolated)
	default:
		level, isolatedShards = shard.LevelIsolated, len(ex.layout.Shards)
	}
	ex.emit(0, "run.isolation", "", "", map[string]any{"level": level, "configured": s.Isolation, "shared_shards": sharedShards,
		"isolated_shards": isolatedShards, "known": len(iso.known), "tests": len(ids)})
	return nil
}

// startSharedDB makes the one database the shared shards use, with the write log on it, owned by the run.
func (ex *execution) startSharedDB(ctx context.Context) error {
	iso := ex.iso
	if iso == nil || iso.nShared == 0 || ex.db == nil {
		return nil
	}
	name := shard.SharedName(ex.instance(), ex.suite.Name, ex.id)
	if err := ex.db.drv.Clone(ctx, ex.db.tpl, name); err != nil {
		return fmt.Errorf("clone shared database: %w", err)
	}
	iso.shared = name
	if iso.pg == nil { // sets come from the report: no log to install
		return nil
	}
	if _, err := iso.pg.Observe(ctx, name, shard.ObserveOpts{Install: true, Schema: iso.logSchema(), Since: shard.NoRows}); err != nil {
		ex.dropShared()
		if t, ok := shard.IsSchemaTaken(err); ok {
			return schemaTaken(ex.suite, t.Schema)
		}
		return fmt.Errorf("shared database: %w", err)
	}
	return nil
}

// closeShared reads the shared database's log for writes nobody expected, then drops it. Call it when the
// parallel part is over.
func (ex *execution) closeShared() {
	ex.stopTenant()
	iso := ex.iso
	if iso == nil || iso.shared == "" {
		return
	}
	defer ex.dropShared()
	if iso.pg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ex.ctx), 60*time.Second)
	defer cancel()
	o, err := iso.pg.Observe(ctx, iso.shared, shard.ObserveOpts{Schema: iso.logSchema(), Since: 0})
	if err != nil {
		ex.emit(0, "db.writes.failed", "", "", map[string]string{"error": trim(err.Error(), 300)})
		return
	}
	iso.mu.Lock()
	quiet := make([]shard.Span, 0, len(iso.quiet))
	for _, q := range iso.quiet {
		quiet = append(quiet, q)
	}
	suspects := shard.Suspects(o.Writes, iso.spans, iso.known, quiet...)
	iso.suspects = suspects
	iso.mu.Unlock()
	for _, s := range suspects {
		ex.emit(0, "db.suspect", string(s.Test), "", map[string]any{"tables": s.Unexplained, "with": s.Overlap})
	}
}

func (ex *execution) dropShared() {
	iso := ex.iso
	if iso == nil || iso.shared == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ex.ctx), 30*time.Second)
	defer cancel()
	if err := ex.db.drv.Drop(ctx, iso.shared); err != nil {
		ex.emit(0, "db.drop.failed", "", "", map[string]string{"db": iso.shared, "error": err.Error()})
	}
	iso.shared = ""
}

// probes lists the suspects that did not fail in parallel: they are run alone only to see what they write, and
// that run never counts as a result of the test.
func (ex *execution) probes(failed []shard.TestID) []shard.TestID {
	if ex.iso == nil {
		return nil
	}
	ex.iso.mu.Lock()
	defer ex.iso.mu.Unlock()
	var out []shard.TestID
	for _, s := range ex.iso.suspects {
		if !slices.Contains(failed, s.Test) {
			out = append(out, s.Test)
		}
	}
	return out
}

// confirmSuspects settles every suspect with what it wrote alone: one that wrote a table of the unexplained
// overlap joins the serial group, the others are cleared. A suspect already moved by the fail-in-parallel
// detection is left to it.
func (ex *execution) confirmSuspects() error {
	iso := ex.iso
	if iso == nil {
		return nil
	}
	iso.mu.Lock()
	suspects := slices.Clone(iso.suspects)
	alone := map[string][]string{}
	for k, v := range iso.alone {
		alone[k] = v
	}
	iso.mu.Unlock()
	if len(suspects) == 0 {
		return nil
	}
	st := ex.r.d.Store
	entries, err := st.Serial(ex.bg, ex.suite.Name)
	if err != nil {
		return err
	}
	for _, s := range suspects {
		test := string(s.Test)
		if slices.ContainsFunc(entries, func(e store.SerialEntry) bool { return e.Test == test }) {
			continue
		}
		written, measured := alone[test]
		if !measured || !s.Confirm(written) {
			ex.emit(0, "db.suspect.cleared", test, "", map[string]any{"measured_alone": measured})
			continue
		}
		with := make([]string, len(s.Overlap))
		for i, o := range s.Overlap {
			with[i] = string(o)
		}
		evidence, _ := json.Marshal(map[string]any{"run": ex.id, "wrote_outside_known_set": s.Unexplained, "with": with})
		if err := st.AddSerial(ex.bg, store.SerialEntry{Suite: ex.suite.Name, Test: test, Evidence: evidence}); err != nil {
			return err
		}
		ex.mu.Lock()
		ex.moved++
		ex.mu.Unlock()
		ex.emit(0, "test.serial", test, "", map[string]any{"wrote_outside_known_set": s.Unexplained, "with": with})
	}
	return nil
}

// schemaTaken is the run error for an app database that has a schema of the write log's name that is not the log.
func schemaTaken(s *Suite, schema string) error {
	return fmt.Errorf("the database already has a schema named %q that is not localci's write log: set suites.%s.writes.schema to another name", schema, s.Name)
}

// checkLogSchema refuses a database of the app that has a schema of the write log's name the core did not make;
// the first look at a clone does it, before any test writes there.
func (ex *execution) checkLogSchema(ctx context.Context, db string) error {
	iso := ex.iso
	if iso == nil || iso.pg == nil || !iso.measure {
		return nil
	}
	taken, err := iso.pg.LogSchemaTaken(ctx, db, iso.logSchema())
	if err != nil {
		return fmt.Errorf("check the write log's schema: %w", err)
	}
	if taken {
		return schemaTaken(ex.suite, iso.logSchema())
	}
	return nil
}
