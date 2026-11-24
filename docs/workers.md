# Workers: run shards on another machine

> **Experimental.** Workers have been tested on one machine and in automated tests, not yet on separate real hardware.
> Expect rough edges, and tell us what you find.

One machine is the primary: it holds the suites, the queue, the results and the dashboard. Another machine can join it
as a worker and run some of the shards of a run, so a suite that does not fit on one laptop still finishes quickly.

The worker keeps nothing of yours but a copy of the repository it fetches from the primary. It checks out the
commit under test, prepares its own workspace, creates its own database and stack, runs its shards, and sends the
report, the log and the artifacts (screenshots, traces) back. You read the results on the primary, as for any run.

## Set it up

On the primary, let the worker port listen on the network and restart `localci serve`:

```yaml
server:
  listen: lan          # default: local (this machine only)
  worker_port: 7358
```

Then ask for a join token:

```bash
localci workers token --host <address of the primary>
```

It prints one command with a single-use token and the fingerprint of the primary's certificate. Run it on the worker
machine (it needs `git`, and whatever your suite needs to prepare and run: Node, Docker, a database):

```bash
localci worker join https://<primary>:7358 --token <token> --fingerprint <fingerprint> --name sim-1
```

`join` saves the worker's credential in its own data directory and starts taking shards. After a restart use
`localci worker run`. `--capacity n` sets how many shards it runs at once (default: what the machine can take).

`localci workers` lists the workers, whether each is online and which shards it is running.
`localci workers revoke <name>` removes one: its running shards go to another machine, every later call it makes is
refused (it stops with "revoked") and it does not come back online by itself. The name stays taken, so joining again
means a new name and a new token.

## What to know

- The worker connects to the primary, never the other way round, and only to the worker port. The dashboard and the
  API stay on the primary's own machine.
- The worker trusts exactly the certificate whose fingerprint you gave it. Compare the fingerprint on the primary
  (`localci workers token` prints it) with the one you paste; a different certificate is refused.
- The certificate is made once, the first time the primary serves workers, and never replaced on its own: if its files
  in the data directory cannot be read or do not match, the primary says so, names the files and keeps workers off
  instead of making a new one. Remove both files to start over, then give every worker the new fingerprint.
- Runs of pull requests from forks stay on the primary unless you set `workers.forks: true`: a fork's code would
  otherwise run on another machine.
- Shard 0 (the serial group, reruns, flake probes), exploratory runs and test checkpoints that need the database always
  stay on the primary.
- A worker that goes silent loses its shards. The tests it had not reported run again, on another worker or on the
  primary, and the run still finishes.
- Tests that ran on a worker add to the memo and timing history but not to coverage or write-set learning for
  affected-tests selection; those keep learning from the shards that run on the primary.
