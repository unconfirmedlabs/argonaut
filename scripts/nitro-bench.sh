#!/usr/bin/env bash
# Inbound argonaut benchmark on a Nitro Enclaves-capable EC2 instance.
#
# This builds a benchmark-only EIF with argonaut plus a deterministic HTTP
# server. Host-side vegeta drives traffic through:
#   vegeta -> TCP:<httpPort> -> argonaut host -> VSOCK -> argonaut enclave -> HTTP server
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARGONAUT_REPO_ROOT="$ROOT"
source "$ROOT/scripts/lib/repro.sh"
argonaut_export_repro_env
if [[ -n "${ARGONAUT_REQUIRE_PINNED_GO:-}" ]]; then
  argonaut_verify_go_toolchain
fi

WORKDIR="${ARGONAUT_BENCH_WORKDIR:-$(mktemp -d)}"
KEEP_WORKDIR="${ARGONAUT_BENCH_KEEP_WORKDIR:-}"
EXPORT_RESULTS_DIR="${ARGONAUT_BENCH_RESULTS_DIR:-}"
EXPORT_RAW_RESULTS="${ARGONAUT_BENCH_EXPORT_RAW:-}"

HTTP_PORT="${ARGONAUT_BENCH_HTTP_PORT:-18080}"
HTTP_VSOCK_PORT="${ARGONAUT_BENCH_HTTP_VSOCK_PORT:-3000}"
ENCLAVE_CPUS="${ARGONAUT_BENCH_CPUS:-2}"
ENCLAVE_MEMORY="${ARGONAUT_BENCH_MEMORY:-1024}"
MAX_CONNECTIONS="${ARGONAUT_BENCH_MAX_CONNECTIONS:-4096}"
RATES="${ARGONAUT_BENCH_RATES:-100 250 500 1000}"
PAYLOADS="${ARGONAUT_BENCH_PAYLOADS:-0 1024 32768}"
DURATION="${ARGONAUT_BENCH_DURATION:-15s}"
VEGETA_VERSION="${ARGONAUT_BENCH_VEGETA_VERSION:-v12.12.0}"

IMAGE_TAG="${ARGONAUT_BENCH_IMAGE_TAG:-argonaut-bench:$SOURCE_DATE_EPOCH}"
EIF_PATH="$WORKDIR/argonaut-bench.eif"
CONSOLE_LOG="$WORKDIR/enclave-console.log"
HOST_LOG="$WORKDIR/argonaut-host.log"
CONFIG_FILE="$WORKDIR/config.json"
RESULTS_DIR="$WORKDIR/results"
VEGETA_BIN="$WORKDIR/bin/vegeta"

ENCLAVE_ID=""
HOST_PID=""
CONSOLE_PID=""

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "[bench] missing required command: $1" >&2
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
  [[ -n "$CONSOLE_PID" ]] && kill "$CONSOLE_PID" 2>/dev/null
  [[ -n "$ENCLAVE_ID" ]] && run_root nitro-cli terminate-enclave --enclave-id "$ENCLAVE_ID" >/dev/null 2>&1
  docker image rm "$IMAGE_TAG" >/dev/null 2>&1
  if [[ -z "$KEEP_WORKDIR" ]]; then
    rm -rf "$WORKDIR"
  else
    echo "[bench] kept workdir: $WORKDIR"
  fi
}
trap cleanup EXIT

fail_with_logs() {
  echo "[bench] FAIL: $1" >&2
  echo "[bench] workdir: $WORKDIR" >&2
  for log in "$CONSOLE_LOG" "$HOST_LOG"; do
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

install_vegeta() {
  if command -v vegeta >/dev/null 2>&1; then
    VEGETA_BIN="$(command -v vegeta)"
    return
  fi
  mkdir -p "$WORKDIR/bin"
  echo "[bench] installing vegeta $VEGETA_VERSION"
  GOBIN="$WORKDIR/bin" go install "github.com/tsenart/vegeta/v12@$VEGETA_VERSION"
}

run_case() {
  local rate="$1"
  local payload="$2"
  local label="rate-${rate}_bytes-${payload}"
  local target_file="$RESULTS_DIR/$label.targets"
  local raw_file="$RESULTS_DIR/$label.bin"
  local txt_file="$RESULTS_DIR/$label.txt"
  local json_file="$RESULTS_DIR/$label.json"

  printf 'GET http://127.0.0.1:%s/bytes/%s\n' "$HTTP_PORT" "$payload" > "$target_file"
  echo "[bench] rate=$rate/s payload=${payload}B duration=$DURATION" >&2
  "$VEGETA_BIN" attack \
    -duration="$DURATION" \
    -rate="$rate" \
    -connections="$MAX_CONNECTIONS" \
    -timeout=10s \
    -targets="$target_file" > "$raw_file"
  "$VEGETA_BIN" report "$raw_file" > "$txt_file"
  "$VEGETA_BIN" report -type=json "$raw_file" > "$json_file"

  jq -r --arg rate "$rate" --arg payload "$payload" '
    [
      $rate,
      $payload,
      .requests,
      (.throughput | tostring),
      (.success * 100 | tostring),
      (.latencies["50th"] / 1000000 | tostring),
      (.latencies["95th"] / 1000000 | tostring),
      (.latencies["99th"] / 1000000 | tostring),
      (.latencies.max / 1000000 | tostring),
      ((.errors // []) | length | tostring)
    ] | @tsv
  ' "$json_file"
}

need_cmd go
need_cmd docker
need_cmd nitro-cli
need_cmd jq
need_cmd curl

ulimit -n 1048576 >/dev/null 2>&1 || true
mkdir -p "$WORKDIR/image/etc" "$WORKDIR/image/dev" "$WORKDIR/image/tmp" "$RESULTS_DIR"
printf '127.0.0.1   localhost\n' > "$WORKDIR/image/etc/hosts"
touch "$WORKDIR/image/dev/.keep" "$WORKDIR/image/tmp/.keep"

install_vegeta

echo "[bench] building linux binaries"
(
  argonaut_go_build "$WORKDIR/image/argonaut" .
  argonaut_go_build "$WORKDIR/image/enclave-bench-server" ./scripts/nitro-bench/enclave-server
)

cat > "$WORKDIR/image/Dockerfile" <<'EOF'
FROM scratch
COPY argonaut /argonaut
COPY enclave-bench-server /enclave-bench-server
COPY etc/ /etc/
COPY dev/ /dev/
COPY tmp/ /tmp/
ENTRYPOINT ["/enclave-bench-server"]
EOF

echo "[bench] building Docker image $IMAGE_TAG"
docker build -q -t "$IMAGE_TAG" "$WORKDIR/image" >/dev/null

echo "[bench] building EIF"
run_root nitro-cli build-enclave --docker-uri "$IMAGE_TAG" --output-file "$EIF_PATH" > "$WORKDIR/build-enclave.json"

echo "[bench] starting enclave"
RUN_OUTPUT="$(run_root nitro-cli run-enclave \
  --cpu-count "$ENCLAVE_CPUS" \
  --memory "$ENCLAVE_MEMORY" \
  --eif-path "$EIF_PATH" \
  --debug-mode)"

ENCLAVE_ID="$(echo "$RUN_OUTPUT" | jq -r '.EnclaveID')"
ENCLAVE_CID="$(echo "$RUN_OUTPUT" | jq -r '.EnclaveCID')"
[[ -n "$ENCLAVE_ID" && "$ENCLAVE_ID" != "null" ]] || fail_with_logs "could not parse EnclaveID"
[[ -n "$ENCLAVE_CID" && "$ENCLAVE_CID" != "null" ]] || fail_with_logs "could not parse EnclaveCID"
echo "[bench] enclave ID=$ENCLAVE_ID CID=$ENCLAVE_CID"

(run_root nitro-cli console --enclave-id "$ENCLAVE_ID") > "$CONSOLE_LOG" 2>&1 &
CONSOLE_PID=$!

wait_for_log "ARGONAUT_BENCH_WAITING_FOR_CONFIG" "$CONSOLE_LOG" 60 ||
  fail_with_logs "enclave did not start config receiver"

cat > "$CONFIG_FILE" <<EOF
{
  "schemaVersion": "v0.1",
  "httpPort": $HTTP_PORT,
  "httpVsockPort": $HTTP_VSOCK_PORT,
  "httpTcpPort": $HTTP_VSOCK_PORT,
  "endpoints": []
}
EOF

echo "[bench] starting argonaut host"
ARGONAUT_LOG_CONNECTIONS=0 ARGONAUT_MAX_CONNECTIONS="$MAX_CONNECTIONS" \
  "$WORKDIR/image/argonaut" host "$ENCLAVE_CID" "$CONFIG_FILE" > "$HOST_LOG" 2>&1 &
HOST_PID=$!

wait_for_log "ARGONAUT_BENCH_READY" "$CONSOLE_LOG" 90 ||
  fail_with_logs "enclave did not become ready"

for _ in $(seq 1 30); do
  if curl -fsS --max-time 2 "http://127.0.0.1:$HTTP_PORT/health" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -fsS --max-time 2 "http://127.0.0.1:$HTTP_PORT/health" >/dev/null ||
  fail_with_logs "health check through inbound bridge failed"

cat > "$RESULTS_DIR/context.txt" <<EOF
instance_path=host -> TCP:$HTTP_PORT -> argonaut host -> VSOCK:$ENCLAVE_CID:$HTTP_VSOCK_PORT -> argonaut enclave -> TCP:127.0.0.1:$HTTP_VSOCK_PORT
enclave_cpus=$ENCLAVE_CPUS
enclave_memory_mib=$ENCLAVE_MEMORY
argonaut_max_connections=$MAX_CONNECTIONS
duration=$DURATION
rates=$RATES
payloads=$PAYLOADS
EOF

echo "[bench] columns: rate payload_bytes requests throughput success_percent p50_ms p95_ms p99_ms max_ms error_count"
for payload in $PAYLOADS; do
  for rate in $RATES; do
    run_case "$rate" "$payload"
  done
done | tee "$RESULTS_DIR/summary.tsv"

if [[ -n "$EXPORT_RESULTS_DIR" ]]; then
  mkdir -p "$EXPORT_RESULTS_DIR"
  find "$RESULTS_DIR" -maxdepth 1 -type f \( \
    -name '*.json' -o \
    -name '*.txt' -o \
    -name '*.tsv' -o \
    -name '*.targets' \
  \) -exec cp {} "$EXPORT_RESULTS_DIR/" \;
  if [[ -n "$EXPORT_RAW_RESULTS" ]]; then
    find "$RESULTS_DIR" -maxdepth 1 -type f -name '*.bin' -exec cp {} "$EXPORT_RESULTS_DIR/" \;
  fi
  echo "[bench] copied results to: $EXPORT_RESULTS_DIR"
fi

echo "[bench] PASS: inbound benchmark complete"
echo "[bench] results: $RESULTS_DIR"
