#!/usr/bin/env bash
# Launch a temporary spot EC2 instance and run the inbound Nitro benchmark.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

remote_env="ARGONAUT_BENCH_KEEP_WORKDIR=1 ARGONAUT_BENCH_RESULTS_DIR=benchmark-results"
for name in \
  ARGONAUT_BENCH_HTTP_PORT \
  ARGONAUT_BENCH_HTTP_VSOCK_PORT \
  ARGONAUT_BENCH_CPUS \
  ARGONAUT_BENCH_MEMORY \
  ARGONAUT_BENCH_MAX_CONNECTIONS \
  ARGONAUT_BENCH_RATES \
  ARGONAUT_BENCH_PAYLOADS \
  ARGONAUT_BENCH_DURATION \
  ARGONAUT_BENCH_VEGETA_VERSION; do
  if [[ -n "${!name:-}" ]]; then
    remote_env+=" $name=$(printf '%q' "${!name}")"
  fi
done

export ARGONAUT_CI_INSTANCE_NAME="${ARGONAUT_CI_INSTANCE_NAME:-argonaut-nitro-bench}"
export ARGONAUT_CI_FETCH_PATH="${ARGONAUT_CI_FETCH_PATH:-benchmark-results}"
export ARGONAUT_CI_RUN_COMMAND="${ARGONAUT_CI_RUN_COMMAND:-$remote_env scripts/nitro-bench.sh}"

exec "$SCRIPT_DIR/aws-spot-nitro-smoke.sh"
