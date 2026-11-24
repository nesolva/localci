---
name: localci
description: Run the project's end-to-end tests and pull-request checks with localci, wait for the result, read exactly what failed and fix it. Use whenever you change code that e2e tests cover, before saying a change works, or when asked to run, check, verify or debug the e2e suite or CI checks on this machine.
---

# localci

`localci` runs the project's e2e suite (sharded across this machine) and its GitHub pull-request checks, and answers
in a few lines. Use it instead of starting Playwright or the CI scripts yourself: it prepares the app, the database and
the ports for every shard, and it tests your working tree, uncommitted changes included.

The loop is **run, wait, show, triage, fix, run --failed**.

## Loop

1. `localci run --suite <name> --wait` from inside the repository. Leave `--suite` out when only one suite is configured.
   Without `--wait` it prints one line with the run id and returns at once; then `localci wait <id>` blocks until it ends.
2. The last line says how it ended: `#42 failed 2/128 · 5m12s · localci show 42`. Exit code: 0 passed, 1 tests failed,
   2 wrong usage or configuration, 3 infrastructure (server down, run broke before its tests, cancelled, or its stack or tenant flow broke: `infrastructure: <reason>`, not failed tests), 4 timeout.
3. `localci show 42` prints the first failure: test title and `file:line`, the failing step, expected vs actual, an
   accessibility-tree excerpt of the target, console errors, failed network calls and the paths of the screenshot and
   trace. `localci show 42 --all` prints every failure; `--full` lifts the 25-line limit per failure.
4. `localci triage 42` says why each test failed, before you change anything: one line per test with its class, then at
   most three lines of evidence. Act on the class:
   - `product-bug`: the app is wrong (a 5xx or a console error next to a failed assertion, or the target is gone and
     the change touched its page). Fix the app.
   - `test-bug`: a selector no longer matches, and the evidence names what the page has instead. When the evidence says
     `fix: replace "Pay" with "Place order"`, check that this is what the product means now (`localci triage 42 --json`
     has the diff's path in `patch.path`), then apply it with `localci triage 42 --apply`. It changes the working
     tree and nothing else, refuses a different checkout and a fork's pull request, and commits nothing.
   - `flaky`: it failed and then passed on the same tree (a passed run can have flaky tests). Say so; do not "fix" it.
     `localci runs --flaky` counts them over 30 days.
   - `infrastructure`: the stack, the machine or the report broke, not the code. Tell the user; do not change code.
   - `unknown`: no signal. Read the failure (step 3) and the test yourself.
5. Read the test and the code it exercises and fix the one that is wrong. The `aria:` line is the page as the test saw
   it: a disabled button, a missing label or an error banner usually explains the failure without opening the trace.
6. `localci run --failed 42 --wait` reruns only the tests that failed in run 42. When it passes, run the whole suite once
   more before you call the work done.

## While you work: affected tests and flows

- **Affected tests.** While iterating, `localci run --affected --wait` runs only the tests your diff can reach (per-test
  coverage of this suite), and the whole suite when it cannot tell safely. `localci run --affected --explain` runs
  nothing and says why each test was chosen, or why all of them were. It is for the inner loop only: the full suite still
  gates main, so run it once before you call the work done.
- **Flows.** Before you write login, sign-up or other setup steps in a test, run `localci flows`: one line per reusable
  flow with its inputs, outputs, how many tests use it and how long it takes. Call the one that exists
  (`await flow("login", { user })`) instead of writing the steps again; a flow restored from a checkpoint costs
  milliseconds where the steps would cost seconds. Write a new flow only when none fits. The dashboard's run page has a DAG
  tab that shows which flows a run used and which were restored.

## Other commands

| Command | Use |
|---|---|
| `localci run --affected --wait` | only the tests the diff can reach; `--explain` says why, running nothing |
| `localci flows` | the reusable flows: inputs, outputs, use, time and how often a checkpoint was restored |
| `localci run --checks --wait` | the pull-request checks of this checkout (the `.github/workflows` jobs), one line per check |
| `localci run --grep "pays" --wait` | only tests whose file and title match |
| `localci run tests/cart.spec.ts --wait` | only these test files |
| `localci status` | one screen: what runs, what waits, how the last runs ended |
| `localci queue`, `localci runs` | active runs, recent runs (10 lines; `--all` for more) |
| `localci logs 42` | the failing lines of the log and the path of the whole file (a check's output, or a shard's) |
| `localci eta 42` | time left |
| `localci runs --since 30m` | runs from the last 30 minutes; ends with `cursor <n>`, pass it back as `--since <n>` to see only what is new |
| `localci show 42 --since <n>` | only failures that appeared after cursor `n` |
| `localci triage 42 --test pays` | the class of one test (its id or part of its title) |
| `localci runs --flaky` | tests that failed and then passed, with the number of runs in the last 30 days |

Add `--json` to any command for one JSON object per line with stable field names.

## Rules

- Every list stops at 10 lines and says how to see more. Do not pipe output through other tools to shorten it.
- Paths in the output are absolute. Open a screenshot or read a log from there when the summary is not enough.
- A failure line that ends with `class: …` was classified by localci (for example a conflict between tests running in
  parallel). Rerun it alone before changing the app, and say so if it passes. `localci triage` gives the evidence.
- Run `localci triage <id>` before you change code for a failure; its class tells you whether the app, the test or
  neither needs a change. A patch it proposes is a suggestion: never apply one you have not read.
- If a command says localci is not running, tell the user to start it with `localci serve` (or `localci service install`).
  Do not start it yourself in the background without being asked.
- Never edit a test just to make it pass. A test that fails because the product changed on purpose gets updated
  together with the change; say which one you did.
- When the work is finished, report the run id and its final line.
