#!/usr/bin/env bash
# Simulated second machine: a Docker container is the worker, this machine is the primary. The container has its own
# filesystem and network and reaches the primary only through the worker TLS listener (host.docker.internal), with a
# fresh join token. The same steps work on a real second machine: build the binary for it, then run the `localci
# worker join` line below there instead of in the container.
#
#   scripts/sim-worker-trial.sh            # needs docker, go, git, node/npm and python3
#
# Environment: IMAGE (default: the Playwright image of the version the example pins), WORK (scratch directory, made
# with mktemp when unset), PORT and WORKER_PORT (the primary's dashboard and worker listener), SLOW_MS (how long
# every page of the example app waits, so test time dominates the fixed costs of a shard).
set -euo pipefail

cd "$(dirname "$0")/.."
SRC=$PWD
PW_VERSION=$(node -p "require('./node_modules/@playwright/test/package.json').version")
IMAGE=${IMAGE:-mcr.microsoft.com/playwright:v${PW_VERSION}-noble}
WORK=${WORK:-$(mktemp -d)}
PORT=${PORT:-17357}
WORKER_PORT=${WORKER_PORT:-17358}
NAME=sim-1
CONTAINER=localci-trial-$NAME
SLOW_MS=${SLOW_MS:-1500}
mkdir -p "$WORK/bin" "$WORK/repo"
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
now() { python3 -c 'import time; print(time.time())'; }
secs() { python3 -c "print(round($2 - $1, 1))"; }

SERVE_PID=
cleanup() {
  [ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null || true
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# 1. Binaries: one for this machine, one for the container's architecture. Nothing is built inside the repository.
case $(docker info --format '{{.Architecture}}') in aarch64|arm64) ARCH=arm64 ;; *) ARCH=amd64 ;; esac
log "building localci for this machine and for linux/$ARCH"
go build -trimpath -o "$WORK/bin/localci" ./cmd/localci
GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/localci-linux" ./cmd/localci

# 2. The repository under test: the example shop with the Playwright package it depends on (a workspace package,
#    not on npm), as a git repository whose `prepare` installs and builds everything on each machine.
log "making the trial repository"
(cd "$SRC" && git ls-files -z package.json package-lock.json packages/playwright examples/acme-shop | tar --null -T - -cf -) | tar -xf - -C "$WORK/repo"
git -C "$WORK/repo" init -q -b main
git -C "$WORK/repo" remote add origin https://github.com/acme/shop.git
git -C "$WORK/repo" add -A
git -C "$WORK/repo" -c user.name=trial -c user.email=trial@localhost commit -q -m "trial"

cat > "$WORK/config.yaml" <<YAML
version: 1
repos:
  - name: acme/shop
    checks: false
server:
  listen: lan
suites:
  shop:
    repos: [acme/shop]
    shards: 2
    timeout: 20m
    prepare:
      - repo: acme/shop
        run: 'npm ci --no-audit --no-fund && npm run build -w packages/playwright && chmod +x packages/playwright/dist/cli.js && ln -sf ../localci-playwright/dist/cli.js node_modules/.bin/localci-playwright'
        cache_key: [package-lock.json, package.json]
        keep: [node_modules, packages/playwright/dist]
    database:
      driver: command
      template: {seed: "echo '{\"tenants\":{}}' > {{db.url}}"}
      create: "cp {{template}} {{name}}"
      drop: "rm -f {{name}}"
    test:
      repo: acme/shop
      list: 'cd examples/acme-shop && npx localci-playwright list'
      run: 'cd examples/acme-shop && npx playwright test'
      env:
        PORT: '{{port "web"}}'
        DB_PATH: '{{db.url}}'
        SLOW_MS: '$SLOW_MS'
YAML

# 3. The primary.
# The CLI finds the socket and the write token in the standard data directory, which XDG_DATA_HOME moves here.
export XDG_DATA_HOME=$WORK/xdg
# The primary clones acme/shop from the trial repository instead of GitHub (git's env config, only for this process).
GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.$WORK/repo.insteadOf" GIT_CONFIG_VALUE_0=https://github.com/acme/shop.git \
"$WORK/bin/localci" serve --config "$WORK/config.yaml" --data-dir "$XDG_DATA_HOME/localci" --server-port "$PORT" \
  --server-worker-port "$WORKER_PORT" > "$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 60); do
  "$WORK/bin/localci" workers >/dev/null 2>&1 && break
  sleep 1
done
"$WORK/bin/localci" workers >/dev/null 2>&1 || { tail -20 "$WORK/serve.log"; echo "the primary did not start" >&2; exit 3; }

# 4. The simulated machine: a fresh container, the binary and nothing else.
T0=$(now)
log "starting $CONTAINER from $IMAGE"
docker run -d --name "$CONTAINER" --add-host host.docker.internal:host-gateway \
  -v "$WORK/bin/localci-linux:/usr/local/bin/localci:ro" "$IMAGE" sleep infinity >/dev/null
docker exec "$CONTAINER" git --version >/dev/null
# The simulated machine shares nothing with this one but the binary: no other mount, no shared data directory.
MOUNTS=$(docker inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' "$CONTAINER")
[ "${MOUNTS% }" = "/usr/local/bin/localci" ] || { echo "unexpected mounts in the container: $MOUNTS" >&2; exit 3; }
JOIN=$("$WORK/bin/localci" workers token --host host.docker.internal | grep 'localci worker join')
JOIN=${JOIN//<name>/$NAME}
log "joining: ${JOIN%%--token*}--token … (single use)"
docker exec -d "$CONTAINER" sh -c "$JOIN --capacity 1 --data-dir /work > /work.log 2>&1"
for _ in $(seq 60); do
  "$WORK/bin/localci" workers | grep -q "$NAME · online" && break
  sleep 1
done
"$WORK/bin/localci" workers | grep -q "$NAME · online" || { docker exec "$CONTAINER" cat /work.log || true; echo "the worker did not come online" >&2; exit 3; }
T1=$(now)
log "worker online after $(secs "$T0" "$T1")s (container start, join, first heartbeat)"

# 5. Two runs of the suite, split between this machine and the worker. The first prepares both machines (npm ci,
#    build, database template); the second is what a team sees day to day.
split() {
  command -v sqlite3 >/dev/null || { echo "   (install sqlite3 to see how the tests split)"; return 0; }
  sqlite3 "$XDG_DATA_HOME/localci/localci.db" "select shard, count(*), sum(ms) from tests where run = '$1' group by shard" |
    awk -F'|' '{ printf "   shard %s: %d tests, %.1fs of test time (%s)\n", $1, $2, $3 / 1000, ($1 == 1 ? "this machine" : "worker") }'
}
cd "$WORK/repo"
for pass in cold warm; do
  log "run ($pass)"
  A=$(now)
  set +e
  "$WORK/bin/localci" run --suite shop --wait > "$WORK/run-$pass.txt" 2>&1
  CODE=$?
  set -e
  B=$(now)
  cat "$WORK/run-$pass.txt"
  ID=$(grep -o '#[0-9a-z]*' "$WORK/run-$pass.txt" | head -1 | tr -d '#')
  log "$pass run: exit $CODE in $(secs "$A" "$B")s"
  [ -n "$ID" ] && split "$ID"
  if [ "$pass" = cold ]; then
    # The container got the code through the worker listener only: its mirror's origin is the relay, not a host path.
    ORIGIN=$(docker exec "$CONTAINER" sh -c 'git -C /work/mirrors/acme__shop.git config remote.origin.url')
    log "the worker's mirror was fetched from: $ORIGIN"
    case $ORIGIN in */worker/v1/git/acme/shop) ;; *) echo "the worker's code did not come through the worker listener" >&2; exit 3 ;; esac
    docker exec "$CONTAINER" test ! -e "$WORK/repo" || { echo "the trial repository is visible inside the container" >&2; exit 3; }
  fi
  [ "$CODE" -eq 0 ] || { echo "the $pass run did not pass; logs are in $WORK" >&2; exit "$CODE"; }
done
"$WORK/bin/localci" workers
log "done; scratch directory $WORK"
