// Package exec checks out sets of repo heads side by side (mirrors, working
// tree snapshots, warm workspaces) and runs commands and long-running services
// on them: process groups that never leave orphans, streamed logs, readiness
// probes and free ports.
package exec
