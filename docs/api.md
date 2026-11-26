# Local API

`localci serve` answers on two listeners.

| Listener | What it serves |
|---|---|
| Unix socket `api.sock` in the data directory (mode 0600) | The whole API. Calls that change anything carry the token from the `token` file in the data directory as `Authorization: Bearer <token>`. |
| `127.0.0.1:<server.port>` | The dashboard, read-only (`GET`) calls and the test hooks. Any other call is answered `403` with the socket's path, whatever token it carries. |

The CLI talks only to the socket. When nothing answers there it says localci is not running and exits 3; it never
falls back to the TCP port. `--socket <path>` or `$LOCALCI_SOCKET` point it at another socket.

If the data directory's path is too long for a socket path on your platform (104 bytes on macOS, 108 on Linux), the
socket is made in a private directory under `$XDG_RUNTIME_DIR` (Linux) or `$TMPDIR` (macOS), and its path is written
to `api.sock.path` in the data directory, which is where the CLI looks.

The data directory is private to your user (mode 0700; the database and the token 0600). `serve` tightens a directory
an older version left open, and `localci doctor` fails with the fix when a mode is looser.

Test hooks (`POST /runs/<run>/hooks/<area>/<action>`) are made by the shards of a run on the TCP address. Each run has
its own token in `$LOCALCI_HOOK_TOKEN`; see [the report protocol](protocol.md).

## Runs

| Call | Answer |
|---|---|
| `POST /api/runs` | `202 {runs, skipped, cursor}`. `400` when `failed` names a run that does not exist, is a check, belongs to another suite or has no failed tests; no run is created. |

A run that failed carries `outcome` when its tests are not the whole story: `timeout` (the suite's `timeout` stopped it)
or `no_tests_matched` (files, `grep` or `failed` selected no test). Branch on it, not on `reason`, which is text for
people. When the run also carries `infra`, it is an infrastructure failure first.

## Workers

| Call | Answer |
|---|---|
| `GET /api/workers` | `{workers}`: this machine, then each joined worker with `remote`, `capacity`, `status` (`online`, `offline`, `revoked`), the `shards` it runs and `last_seen`. |
| `POST /api/workers/token` | `{token, fingerprint, port, listen}`: a single-use join token. `503` when the worker listener is not running. |
| `POST /api/workers/<name>/revoke` | `{revoked}`. `404` for an unknown name. |

See [workers](workers.md).

## Exploratory agent

| Call | Answer |
|---|---|
| `GET /api/findings?since=<id\|12h>&run=<id>&limit=<n>` | `{findings, total, cursor}`, newest first; each finding has `id`, `run`, `suite`, `goal`, `severity` (`high`, `medium`, `low`), `title`, `steps`, `screenshot`, `test` (the saved draft test's path) and `ts`. `since` is a finding number or a duration. |
| `POST /api/explore` | `202 {run}`: an exploratory run is queued, as the schedule would. `409` when exploration is not configured or one is already queued or running. |

See [the exploratory agent](explore.md).
