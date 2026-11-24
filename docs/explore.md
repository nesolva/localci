# Exploratory agent

Every night an agent explores your app against goals you write, on a shard of its own, and reports what it found:
what is broken, confusing or inconsistent, with the steps to see it, a screenshot, and a draft test when it can write
one. Your tests cover the paths you thought of; this looks for the ones you did not.

Exploration never gates anything. It posts no status to a pull request, blocks no merge and fails no run you wait on.
It runs only on the default branch's heads, never on a pull request.

## Set it up

```yaml
explore:
  schedule: "0 2 * * *"        # cron, five fields, in the machine's time zone
  suite: e2e                   # the suite whose stack the agent explores
  base_url: http://127.0.0.1:{{port "web"}}
  goals:
    - buy as a new customer
    - invite a teammate and change their role
  agent:
    minutes: 30                # the agent is stopped after this long
```

That is the whole section: the agent runs Claude Code with the default command described [below](#what-the-default-command-allows).

| Key | Meaning |
|---|---|
| `schedule` | When a night starts. `0 2 * * *` is 02:00 every day. A night that is still running when the next one is due makes that one skip: they never queue behind each other. |
| `suite` | A suite from `suites`. localci starts one isolated shard of it (its own stack and database), so the agent can create, change and delete data freely. |
| `base_url` | Where the app listens in that shard. |
| `goals` | What to try, in your words (up to 20). The agent also tells you about anything wrong it notices on the way. |
| `agent.command` | The command that runs the agent. It reads its brief on standard input. The default is Claude Code in print mode, so it needs to be signed in (`claude /login`), or to have a provider's credentials (see `agent.env`). |
| `agent.minutes` | The time limit, 1 to 240. When it runs out the agent is stopped and what it already reported is kept. |
| `agent.env` | Names of environment variables to pass to the agent when they are set in localci's own environment, for example `[AWS_REGION]`. Names only, in capitals, no values and no wildcards. See [other providers](#other-providers). |

Without an `explore` section, exploration is off. With one, `schedule`, `suite`, `base_url` and at least one goal are all required, and a missing one is a config error.

## What the default command allows

When `agent.command` is not set, the agent is this command:

```
claude -p --output-format stream-json --verbose --allowedTools "Bash,Read,Write" --max-turns 60
```

`localci config` shows it, with where it came from.

**The agent runs commands unattended, on this machine, as you.** `--allowedTools "Bash,Read,Write"` lets it run any
shell command and read and write any file your user can, with nobody to approve each one. It needs that: without
Bash it could neither drive the app nor write its findings. What localci does around it is limited to this: the agent
runs in the test repository's workspace, it gets a small environment (your `PATH` and `HOME`, proxy and certificate
settings, and the `ANTHROPIC_*` and `CLAUDE_*` variables, not the rest of your shell's variables), and the app it
explores runs on a shard of its own with its own database. It is not sandboxed. Run it on a machine, and with
credentials, where that is acceptable. To allow less, set your own `agent.command`.

Two limits bound what a night costs, and whichever comes first ends it:

- `--max-turns 60` stops the agent after 60 turns of its conversation with the model.
- `agent.minutes` (30 by default) stops it after that much time.

Either way the findings it already reported are kept and the run counts as passed. The `explore.done` event says which
limit ended it, and records the turns and the cost the agent reported (on a subscription the cost is the list price,
not a charge). If your own command is not Claude Code, only `agent.minutes` applies.

## Other providers

The agent gets a minimal environment, so what Bedrock or Vertex need does not reach it by itself. Name those
variables in `agent.env` and localci passes each one that is set when the night starts:

```yaml
explore:
  agent:
    env:
      - CLAUDE_CODE_USE_BEDROCK
      - AWS_REGION
      - AWS_PROFILE
```

For Vertex it is `CLAUDE_CODE_USE_VERTEX`, `CLOUD_ML_REGION`, `ANTHROPIC_VERTEX_PROJECT_ID` and the Google
credentials variable your setup uses. A name that is not set is skipped, and a name that is not a plain variable name
is a config error. The variables are read from the environment `localci serve` runs in; the background service
installed by `localci service install` starts with its own small environment, so run `serve` from a shell that has
them, or put them in the service's environment yourself.

## Run it and read the result

```bash
localci explore                      # start a night now; it uses the agent's quota
localci wait <run>                   # the run id is printed by the line above
localci findings --run <run>         # title, severity, goal; 10 lines, --all for more
localci findings --run <run> --full  # also the steps, screenshot path and draft test
localci findings --since 12h         # what was found in the last 12 hours
```

`--since` also takes a finding number and ends with `cursor <n>`, so an agent reading the list picks up where it
stopped. The same findings are on the dashboard's Findings page: the newest night first, the worst finding first, its
steps and screenshot open.

## What the agent gets

The agent's brief lists your goals, the base URL, the flows your tests already use (`localci flows`) so it spends its
time elsewhere, and how to report. It reports by appending one JSON object per line to the file in
`$LOCALCI_FINDINGS`:

```json
{"goal": "buy as a new customer", "severity": "high", "title": "Paying with a card returns a 500", "steps": ["add an item", "pay"], "screenshot": "pay.png", "test": "test('pay', async ({ page }) => { /* … */ })"}
```

`severity` is `high` (a crash, data loss, or a goal that cannot be reached), `medium` or `low`. Screenshots go in
`$LOCALCI_OUTPUT_DIR`. The agent is untrusted like any code driving your app: titles and steps are shown as plain
printable text and cut to a sensible length, and a screenshot that is not inside the run's output directory is
dropped, so a finding can never make the dashboard serve some other file.

Draft tests are saved next to the run's other files and listed with the finding. localci never runs them and never
commits them: read one, fix it up and move it into your suite yourself.

## Try it on the sample

The sample shop in `examples/acme-shop` has a bug on a page no test visits: the receipt page of an order, linked from
the page that thanks you for it. It answers 500 when the shop runs with `ACME_ORDER_500=1`. Its own tests never open
that page, so they pass; the agent has to find it by using the shop.

Put a copy of `examples/acme-shop` in a repository of your own (the example uses `you/acme-shop`; localci clones
repositories by name from GitHub), then add this to your configuration:

```yaml
repos:
  - name: you/acme-shop
    checks: false
suites:
  shop:
    repos: [you/acme-shop]
    database:
      driver: command
      template: {seed: "echo '{\"tenants\":{}}' > {{db.url}}"}
      create: "cp {{template}} {{name}}"
      drop: "rm -f {{name}}"
    stack:
      - name: web
        repo: you/acme-shop
        run: node server.js
        env: {PORT: '{{port "web"}}', DB_PATH: '{{db.url}}', ACME_ORDER_500: "1"}
        ready: {http: 'http://127.0.0.1:{{port "web"}}/'}
    test:
      repo: you/acme-shop
      run: npx playwright test
      env: {PORT: '{{port "web"}}', DB_PATH: '{{db.url}}'}
    shards: 1
explore:
  schedule: "0 3 * * *"
  suite: shop
  base_url: http://127.0.0.1:{{port "web"}}
  goals:
    - "buy as a new customer: create a shop, sign in, add a product to the cart and pay"
    - "after buying, look at everything the order pages offer"
```

Then, with `localci serve` running:

```bash
localci explore
localci wait <run>
localci findings --run <run> --full
```

A night on the sample takes about ten minutes. The agent usually finds the 500 on the receipt page, with the steps to
reach it, and usually a few things about the sample that are rough by design (it is a fixture). Remove
`ACME_ORDER_500` from the stack's `env` and the receipt page works and that finding does not come back.
