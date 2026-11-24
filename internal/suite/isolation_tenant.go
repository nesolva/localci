package suite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
	"gopkg.in/yaml.v3"
)

// Tenant isolation (see the roadmap's Contracts): all the parallel shards of a run share one stack and one
// database, and each shard works in a tenant made by the suite's `isolation_tenant_flow`, a flow declared in the
// test package. The shard's test command runs that flow once before its first test and hands the outputs to every
// test as the `tenant` fixture. Detection is the shared window of shared-db (database-wide counters flag suspects,
// a rerun alone confirms), and the serial group and the reruns (shard 0) keep a stack and database of their own.
//
// The test command and the core meet in one file per shard for the whole run, named in $LOCALCI_TENANT_FILE:
// {"flow":…,"outputs":…} once the flow ran, {"flow":…,"error":…} when it failed. Every test command of the shard
// (shard 0 runs the serial group and each rerun as its own) finds the tenant there instead of making it again on
// the same stack and database.

const (
	tenantFlowKey = "isolation_tenant_flow"
	// TenantFlowEnv names the flow to the test command; it is set on every invocation of a tenant run.
	tenantFlowEnv = "LOCALCI_TENANT_FLOW"
	tenantFileEnv = "LOCALCI_TENANT_FILE"
)

var flowName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// tenantFlow reads the suite's `isolation_tenant_flow` from its raw node ("" when it has none).
func tenantFlow(s *Suite) string {
	if s.Raw != nil && s.Raw.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(s.Raw.Content); i += 2 {
			if s.Raw.Content[i].Value == tenantFlowKey {
				return s.Raw.Content[i+1].Value
			}
		}
	}
	return ""
}

// checkTenantKeys validates what tenant isolation needs; checkIsolationKeys calls it.
func checkTenantKeys(s *Suite) error {
	flow := tenantFlow(s)
	if flow != "" && !flowName.MatchString(flow) {
		return fmt.Errorf("%s: %q is not a flow name", tenantFlowKey, flow)
	}
	if s.Isolation != shard.LevelTenant {
		return nil
	}
	if flow == "" {
		return fmt.Errorf("isolation: tenant needs %s (the flow that makes a shard's tenant)", tenantFlowKey)
	}
	if len(s.Stack) == 0 {
		return errors.New("isolation: tenant needs a stack section (the one stack every shard shares)")
	}
	return nil
}

// tenantAvailable says whether `auto` may pick tenant for the suite.
func tenantAvailable(s *Suite) bool { return tenantFlow(s) != "" && len(s.Stack) > 0 }

// tenantEnv is what a tenant run adds to every process of the shard's environment.
func (e *shardEnv) tenantEnv() []string {
	ex := e.ex
	if ex.iso == nil || ex.iso.tenant == "" {
		return nil
	}
	return []string{tenantFlowEnv + "=" + ex.iso.tenant, tenantFileEnv + "=" + e.tenantFile()}
}

// tenantFile is the shard's tenant file, kept with the run's logs.
func (e *shardEnv) tenantFile() string {
	return filepath.Join(e.ex.logDir, fmt.Sprintf("shard-%d.tenant.json", e.n))
}

// planTenant lays the run out for tenant isolation: the plan's shards all run against the one stack, and `shards`
// left to auto counts them with the stack paid once.
func (ex *execution) planTenant(ids []shard.TestID, medians map[shard.TestID]time.Duration, serial map[shard.TestID]bool, n int) {
	iso := ex.iso
	iso.tenant = tenantFlow(ex.suite)
	if ex.suite.Shards == 0 && ex.suite.Isolation == shard.LevelTenant {
		parallel := 0
		for _, id := range ids {
			if !serial[id] {
				parallel++
			}
		}
		c := ex.r.d.Sched.Capacity()
		per := ex.shardCost()
		n = shard.TenantCount(capacity.Machine{CPUs: c.CPUs, FreeMemory: c.Memory}, shard.Cost{CPUs: per.CPUs, Memory: per.Memory}, parallel)
	}
	ex.layout = shard.Plan(ids, medians, n, serial)
	iso.nShared = len(ex.layout.Shards)
}

// startShared makes the run's shared database and, for tenant isolation, the one stack on top of it.
func (ex *execution) startShared(ctx context.Context) error {
	if err := ex.startSharedDB(ctx); err != nil {
		return err
	}
	iso := ex.iso
	if iso == nil || iso.tenant == "" || iso.nShared == 0 {
		return nil
	}
	e, err := ex.openEnv(ctx, 1, nil)
	if err != nil {
		ex.dropShared() // the caller only drops it once the parallel part has started
		return fmt.Errorf("tenant stack: %w", err)
	}
	iso.mu.Lock()
	iso.stack = e
	iso.mu.Unlock()
	return nil
}

// borrowStack gives a parallel shard of a tenant run an environment on the shared stack: same database and ports,
// nothing of its own to start or stop.
func (ex *execution) borrowStack(n int, ids []shard.TestID) (*shardEnv, bool) {
	iso := ex.iso
	if iso == nil {
		return nil, false
	}
	iso.mu.Lock()
	st := iso.stack
	iso.mu.Unlock()
	if st == nil || n < 1 || n > iso.nShared {
		return nil, false
	}
	st.mu.Lock()
	ports := make(map[string]int, len(st.ports))
	for k, v := range st.ports {
		ports[k] = v
	}
	st.mu.Unlock()
	e := &shardEnv{ex: ex, n: n, ports: ports, dbName: st.dbName, dbURL: st.dbURL, shared: true}
	ex.mu.Lock()
	for _, id := range ids {
		ex.envOf[string(id)] = e
	}
	ex.mu.Unlock()
	return e, true
}

// stopTenant stops the shared stack once the parallel part is over and charges its cost to the shards that used
// it, so that the cost history keeps meaning "what one shard needs, stack included".
func (ex *execution) stopTenant() {
	iso := ex.iso
	if iso == nil {
		return
	}
	iso.mu.Lock()
	st := iso.stack
	iso.stack = nil
	iso.mu.Unlock()
	if st == nil {
		return
	}
	st.close()
	cost, cpu := st.cost()
	shards, err := ex.r.d.Store.Shards(ex.bg, ex.id)
	if err != nil {
		return
	}
	for _, sh := range shards {
		if sh.N >= 1 && sh.N <= iso.nShared {
			sh.CostMem += int64(cost.Memory)
			sh.CostCPU += cpu
			ex.r.d.Store.PutShard(ex.bg, sh)
		}
	}
}

// settleTenant reads what the tenant flow did in the invocation that ran from `started` on. A failed flow means
// the shard never got a tenant: its tests did not fail, the shard did, so their rows become interrupted (the
// status that is never rerun and never moves a test to the serial group) carrying the flow's failure, and the
// error ends the run as an infrastructure failure.
func (e *shardEnv) settleTenant(m mode, ids []shard.TestID, started time.Time) error {
	ex := e.ex
	if ex.iso == nil || ex.iso.tenant == "" {
		return nil
	}
	b, err := os.ReadFile(e.tenantFile())
	if err != nil {
		return nil
	}
	var got struct {
		Flow  string `json:"flow"`
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &got) != nil || got.Error == "" {
		return nil
	}
	// The flow's error is written by the code under test: one printable line without URL queries or secrets (it
	// reaches the forge's status), the same words in the failure, the event and the run's reason.
	msg := report.Reason(fmt.Sprintf("tenant flow %s failed: %s", ex.iso.tenant, report.Reason(report.Scrub(got.Error))))
	ex.setInfra(msg)
	failure, _ := json.Marshal(map[string]any{"step": "", "message": msg})
	mine := map[string]bool{}
	for _, id := range ids {
		mine[string(id)] = true
	}
	var rows []store.Test
	ex.mu.Lock()
	for i := range ex.results {
		a := &ex.results[i]
		if a.mode == m && mine[a.id] && a.failed() && !a.end.Before(started) {
			a.row.Status, a.row.Failure = "interrupted", failure
			rows = append(rows, a.row)
		}
	}
	ex.mu.Unlock()
	for _, r := range rows {
		ex.r.d.Store.PutTest(ex.bg, r)
	}
	ex.emit(e.n, "tenant.failed", "", "", map[string]string{"flow": ex.iso.tenant, "error": report.Reason(got.Error)})
	return fmt.Errorf("shard %d: %s", e.n, msg)
}
