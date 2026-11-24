// Package explore is the nightly exploratory agent: on a schedule it starts one isolated shard of a suite's stack,
// runs an agent command against it with the goals the team wrote, and stores what the agent reports (steps, a
// screenshot, a draft test when it can) for the morning. It never gates anything: no status is posted, and a finding
// is not a failed run.
//
// Everything the agent writes is untrusted, like a test report: titles and steps are made printable and capped, and
// a screenshot path that leaves the run's output directory is dropped.
package explore
