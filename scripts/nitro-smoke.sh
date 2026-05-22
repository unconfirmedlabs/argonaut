#!/usr/bin/env bash
# Standalone argonaut Nitro smoke test.
#
# Run this on a Nitro Enclaves-enabled EC2 instance with nitro-cli, Docker, Go,
# jq, and curl installed. The script builds a minimal EIF containing argonaut and
# a smoke runner, then verifies real VSOCK and NSM behavior.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="${ARGONAUT_SMOKE_WORKDIR:-$(mktemp -d)}"
KEEP_WORKDIR="${ARGONAUT_SMOKE_KEEP_WORKDIR:-}"

HTTP_PORT="${ARGONAUT_SMOKE_HTTP_PORT:-18080}"
HTTP_VSOCK_PORT="${ARGONAUT_SMOKE_HTTP_VSOCK_PORT:-3000}"
HOST_ECHO_PORT="${ARGONAUT_SMOKE_HOST_ECHO_PORT:-18443}"
OUTBOUND_VSOCK_PORT="${ARGONAUT_SMOKE_OUTBOUND_VSOCK_PORT:-8100}"
SMOKE_HOST="${ARGONAUT_SMOKE_HOST:-argonaut-smoke.local}"
LOCAL_IP="${ARGONAUT_SMOKE_LOCAL_IP:-127.0.0.1}"
LOCAL_PORT="${ARGONAUT_SMOKE_LOCAL_PORT:-9443}"
ENCLAVE_CPUS="${ARGONAUT_SMOKE_CPUS:-2}"
ENCLAVE_MEMORY="${ARGONAUT_SMOKE_MEMORY:-1024}"

IMAGE_TAG="argonaut-smoke:$(date +%s)"
EIF_PATH="$WORKDIR/argonaut-smoke.eif"
CONSOLE_LOG="$WORKDIR/enclave-console.log"
HOST_LOG="$WORKDIR/argonaut-host.log"
ECHO_LOG="$WORKDIR/host-echo.log"
CONFIG_FILE="$WORKDIR/config.json"
HOSTS_BACKUP="$WORKDIR/hosts.backup"

ENCLAVE_ID=""
HOST_PID=""
ECHO_PID=""
CONSOLE_PID=""
HOSTS_BACKED_UP=""

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "[smoke] missing required command: $1" >&2
    exit 1
  }
}

run_root() {
  if [[ "${EUID:-$(id -u)}" -eq 0 ]]; then
    "$@"
  else
    sudo "$@"
  fi
}

cleanup() {
  set +e
  [[ -n "$HOST_PID" ]] && kill "$HOST_PID" 2>/dev/null
  [[ -n "$ECHO_PID" ]] && kill "$ECHO_PID" 2>/dev/null
  [[ -n "$CONSOLE_PID" ]] && kill "$CONSOLE_PID" 2>/dev/null
  [[ -n "$ENCLAVE_ID" ]] && run_root nitro-cli terminate-enclave --enclave-id "$ENCLAVE_ID" >/dev/null 2>&1
  if [[ -n "$HOSTS_BACKED_UP" && -f "$HOSTS_BACKUP" ]]; then
    run_root cp "$HOSTS_BACKUP" /etc/hosts
  fi
  docker image rm "$IMAGE_TAG" >/dev/null 2>&1
  if [[ -z "$KEEP_WORKDIR" ]]; then
    rm -rf "$WORKDIR"
  else
    echo "[smoke] kept workdir: $WORKDIR"
  fi
}
trap cleanup EXIT

fail_with_logs() {
  echo "[smoke] FAIL: $1" >&2
  echo "[smoke] workdir: $WORKDIR" >&2
  for log in "$CONSOLE_LOG" "$HOST_LOG" "$ECHO_LOG"; do
    if [[ -f "$log" ]]; then
      echo "===== $log =====" >&2
      tail -n 200 "$log" >&2 || true
    fi
  done
  exit 1
}

wait_for_log() {
  local pattern="$1"
  local file="$2"
  local timeout="$3"
  local start
  start="$(date +%s)"
  while true; do
    if [[ -f "$file" ]] && grep -Fq "$pattern" "$file"; then
      return 0
    fi
    if (( "$(date +%s)" - start >= timeout )); then
      return 1
    fi
    sleep 1
  done
}

need_cmd go
need_cmd docker
need_cmd nitro-cli
need_cmd jq
need_cmd curl

mkdir -p "$WORKDIR/image/etc" "$WORKDIR/image/dev" "$WORKDIR/image/tmp"
printf '127.0.0.1   localhost\n' > "$WORKDIR/image/etc/hosts"
touch "$WORKDIR/image/dev/.keep" "$WORKDIR/image/tmp/.keep"

echo "[smoke] building linux binaries"
(
  cd "$ROOT"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o "$WORKDIR/image/argonaut" .
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o "$WORKDIR/image/enclave-smoke-runner" ./scripts/nitro-smoke/enclave-runner
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o "$WORKDIR/host-echo" ./scripts/nitro-smoke/host-echo
)

cat > "$WORKDIR/image/Dockerfile" <<'EOF'
FROM scratch
COPY argonaut /argonaut
COPY enclave-smoke-runner /enclave-smoke-runner
COPY etc/ /etc/
COPY dev/ /dev/
COPY tmp/ /tmp/
ENTRYPOINT ["/enclave-smoke-runner"]
EOF

echo "[smoke] building Docker image $IMAGE_TAG"
docker build -q -t "$IMAGE_TAG" "$WORKDIR/image" >/dev/null

echo "[smoke] building EIF"
run_root nitro-cli build-enclave --docker-uri "$IMAGE_TAG" --output-file "$EIF_PATH" > "$WORKDIR/build-enclave.json"

echo "[smoke] starting enclave"
RUN_OUTPUT="$(run_root nitro-cli run-enclave \
  --cpu-count "$ENCLAVE_CPUS" \
  --memory "$ENCLAVE_MEMORY" \
  --eif-path "$EIF_PATH" \
  --debug-mode)"

ENCLAVE_ID="$(echo "$RUN_OUTPUT" | jq -r '.EnclaveID')"
ENCLAVE_CID="$(echo "$RUN_OUTPUT" | jq -r '.EnclaveCID')"
[[ -n "$ENCLAVE_ID" && "$ENCLAVE_ID" != "null" ]] || fail_with_logs "could not parse EnclaveID"
[[ -n "$ENCLAVE_CID" && "$ENCLAVE_CID" != "null" ]] || fail_with_logs "could not parse EnclaveCID"
echo "[smoke] enclave ID=$ENCLAVE_ID CID=$ENCLAVE_CID"

(run_root nitro-cli console --enclave-id "$ENCLAVE_ID") > "$CONSOLE_LOG" 2>&1 &
CONSOLE_PID=$!

wait_for_log "ARGONAUT_SMOKE_WAITING_FOR_CONFIG" "$CONSOLE_LOG" 60 ||
  fail_with_logs "enclave did not start config receiver"

echo "[smoke] preparing host-side DNS alias and echo server"
run_root cp /etc/hosts "$HOSTS_BACKUP"
HOSTS_BACKED_UP=1
if ! grep -Eq "^[[:space:]]*127\.0\.0\.1[[:space:]]+$SMOKE_HOST([[:space:]]|$)" /etc/hosts; then
  printf '\n127.0.0.1   %s\n' "$SMOKE_HOST" | run_root tee -a /etc/hosts >/dev/null
fi

"$WORKDIR/host-echo" "127.0.0.1:$HOST_ECHO_PORT" > "$ECHO_LOG" 2>&1 &
ECHO_PID=$!

cat > "$CONFIG_FILE" <<EOF
{
  "schemaVersion": "v0.1",
  "httpPort": $HTTP_PORT,
  "httpVsockPort": $HTTP_VSOCK_PORT,
  "httpTcpPort": $HTTP_VSOCK_PORT,
  "endpoints": [
    {
      "host": "$SMOKE_HOST",
      "vsockPort": $OUTBOUND_VSOCK_PORT,
      "tcpPort": $HOST_ECHO_PORT,
      "localIP": "$LOCAL_IP",
      "localPort": $LOCAL_PORT
    }
  ]
}
EOF

echo "[smoke] starting argonaut host"
"$WORKDIR/image/argonaut" host "$ENCLAVE_CID" "$CONFIG_FILE" > "$HOST_LOG" 2>&1 &
HOST_PID=$!

wait_for_log "ARGONAUT_SMOKE_READY_FOR_INBOUND" "$CONSOLE_LOG" 90 ||
  fail_with_logs "enclave did not finish NSM/outbound checks"

echo "[smoke] testing inbound bridge"
BODY=""
for _ in $(seq 1 30); do
  BODY="$(curl -fsS --max-time 2 "http://127.0.0.1:$HTTP_PORT/health" 2>/dev/null || true)"
  if [[ "$BODY" == "argonaut-inbound-ok" ]]; then
    break
  fi
  sleep 1
done
[[ "$BODY" == "argonaut-inbound-ok" ]] ||
  fail_with_logs "inbound bridge returned ${BODY:-<empty>}"

wait_for_log "ARGONAUT_SMOKE_OK" "$CONSOLE_LOG" 60 ||
  fail_with_logs "enclave did not report success"

echo "[smoke] PASS: real NSM, config VSOCK, outbound bridge, and inbound bridge verified"
