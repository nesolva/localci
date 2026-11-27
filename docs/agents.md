# Using localci from an agent

localci is built to be driven by a coding agent: short output, one line per item, stable exit codes, and `--json` for
anything a script wants. The skill is the instruction sheet that teaches the loop.

## Install the skill

```bash
localci skill install              # into ~/.claude/skills
localci skill install --dir <dir>  # or a folder you choose
```

The skill is one file, [`skill/localci/SKILL.md`](../skill/localci/SKILL.md). Any agent that reads Markdown
instructions can use it: copy the file where your agent looks for them.

## The loop

**Run, wait, show, triage, fix, run --failed.**

```bash
localci run --suite shop --wait     # prints the last line: "#42 failed 2/128 · 5m12s · localci show 42"
localci show 42                     # the first failure: step, expected vs actual, page excerpt, console, network, paths
localci triage 42                   # why: product-bug, test-bug, flaky, infrastructure or unknown, with evidence
localci run --failed 42 --wait      # only the tests that failed in run 42
```

Exit codes: 0 passed, 1 tests failed, 2 wrong usage or configuration, 3 infrastructure (the server is down, or the
run broke before its tests did), 4 timeout. When `wait` follows several runs, it exits with the highest of their codes.

A run that ran nothing you asked for never exits 0:

- Files, directories or `--grep` that select no test end the run failed with `no tests matched <filter>`, exit 2. So
  does `--failed` when none of those tests is listed any more (renamed or removed). A directory selects every spec
  under it, the suite's test directory and the repository root included.
- `--failed <id>` is refused with exit 2, and no run is created, when run `<id>` is a check or a run of another suite,
  or has no failed tests.
- A run stopped by its [suite's `timeout`](suites.md) ends failed with `timed out after <d>`, exit 4. `wait --timeout`
  giving up exits 4 too; the run goes on.
- `--affected` that selects nothing passes, exit 0, and the line says `no tests affected`.

The last line of a run ends with why it ended that way when there is more to say than the counts. A script reads the
`outcome` field of `--json` (`timeout`, `no_tests_matched`), not the text of the reason.

A failure report has what an agent needs to act without opening a trace: the failing step, expected and actual, an
accessibility-tree excerpt around the target, the last console errors and failed requests, and the absolute paths of
the screenshot and trace.

## Other commands an agent uses

| Command | Use |
|---|---|
| `localci run --affected --wait` | only the tests the diff can reach; `--explain` says why, running nothing |
| `localci flows` | reusable setup flows, so the agent calls one instead of rewriting it |
| `localci run --checks --wait` | the pull-request checks of this checkout |
| `localci triage <id> --apply` | applies a proposed selector fix to the working tree; never commits |
| `localci findings` | what the [nightly exploratory agent](explore.md) found |
| `localci status`, `queue`, `runs`, `logs`, `eta`, `wait` | what is running and how runs ended |

Every list stops at 10 lines and says how to see more. Add `--json` for one JSON object per line with fixed field
names, and `--since <cursor>` to see only what is new.

## What the agent may do on its own

The skill tells the agent to start a run, read it and fix the code or test that is wrong. It does not start
`localci serve` in the background unless asked, never edits a test only to make it pass, and never applies a proposed
patch it has not read. `triage --apply` refuses a different checkout and a pull request from a fork.

## Setting up a new repository

An agent with this page and the skill can set up a Playwright repository from nothing:

1. `npm i -D @nesolva/localci-playwright`, then wrap the Playwright config: `export default withLocalci(defineConfig({...}))`
   and import `test` and `expect` from `@nesolva/localci-playwright` in the specs.
2. Add a suite under `suites:` in `~/.config/localci/config.yaml`, starting from [the suites page](suites.md). [`examples/`](../examples) has a sample, a Postgres
   stack and a cross-repository suite to start from, and [the configuration reference](config.md) lists every key.
3. `localci doctor` says what the machine is missing. `localci serve` starts the server (or `localci service install`
   keeps it running at login).
4. `localci run --suite <name> --wait` from inside the repository, then the loop above.
