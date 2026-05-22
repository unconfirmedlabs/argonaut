# argonaut

`argonaut` is a single static Go binary for AWS Nitro Enclave companion work:

- VSOCK to TCP bridging in both directions.
- One-shot boot configuration delivery over `VSOCK:7777`.
- NSM attestation and hardware RNG access through a subprocess protocol.

The same binary runs on the EC2 parent instance and inside the enclave.

## Contents

- [Build](#build)
- [CLI](#cli)
- [Configuration](#configuration)
- [Bridges](#bridges)
- [NSM Protocol](#nsm-protocol)
- [Trust Model](#trust-model)
- [Verification](#verification)
- [Nitro Smoke Test](#nitro-smoke-test)
- [Nitro Inbound Benchmark](#nitro-inbound-benchmark)
- [Benchmark Results](#benchmark-results)
- [AWS Spot CI Runner](#aws-spot-ci-runner)

## Build

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o argonaut .
```

## CLI

```text
argonaut host <cid> <config-file>
argonaut enclave
argonaut config send <cid> <vsock-port>
argonaut config recv <vsock-port>
argonaut nsm
```

`host` reads a config file, validates the fields used by argonaut, sends the raw
file bytes to the enclave over `VSOCK:7777`, then starts the host-side bridges.
Unknown top-level config fields are allowed so application config can pass
through unchanged.

`enclave` reads JSON config from stdin, validates it, writes a managed
`/etc/hosts` block, then starts the enclave-side bridges.

`config send` and `config recv` are low-level one-shot VSOCK utilities. Config
payloads are capped at 1 MiB.

`nsm` owns `/dev/nsm` and speaks the protocol below on stdin/stdout. Logs go to
stderr.

## Configuration

The public v0.1 schema is:

```json
{
  "schemaVersion": "v0.1",
  "httpPort": 8080,
  "httpVsockPort": 3000,
  "httpTcpPort": 3000,
  "endpoints": [
    {
      "host": "fullnode.testnet.sui.io",
      "vsockPort": 8104,
      "tcpPort": 443,
      "localPort": 443
    }
  ]
}
```

Mode-specific requirements:

- `host`: requires `httpPort`, `httpVsockPort`, and `endpoints`.
- `enclave`: requires `httpVsockPort`, `httpTcpPort`, and `endpoints`.

Endpoint rules:

- `host` must be non-empty, at most 253 characters, and contain only ASCII
  letters, digits, `.`, `_`, and `-`.
- Unknown endpoint fields are rejected. Unknown top-level fields are still
  allowed so application config can pass through unchanged.
- Empty hostname labels are rejected.
- `vsockPort` must be in `1..65535` and unique across endpoints.
- `tcpPort` is optional and defaults to `443`.
- `localPort` is optional and defaults to `443`.
- `localIP` is optional and defaults to the generated endpoint loopback IP.
  When set, it must be an IPv4 loopback address.
- At most 191 endpoints are allowed.

By default, endpoint loopback addresses are assigned from `127.0.0.64` through
`127.0.0.254`. Set `localIP` only when the enclave runtime needs a specific
loopback address for that endpoint.
`/etc/hosts` is updated with a managed block:

```text
# argonaut begin
127.0.0.64   fullnode.testnet.sui.io
# argonaut end
```

Existing non-managed `/etc/hosts` content is preserved.

## Bridges

Host mode:

- Inbound: `TCP:<httpPort>` to `VSOCK:<cid>:<httpVsockPort>`.
- Outbound: `VSOCK:<endpoint.vsockPort>` to `TCP:<endpoint.host>:<endpoint.tcpPort>`.

Enclave mode:

- Inbound: `VSOCK:<httpVsockPort>` to `TCP:127.0.0.1:<httpTcpPort>`.
- Outbound: `TCP:127.0.0.x:<endpoint.localPort>` to `VSOCK:3:<endpoint.vsockPort>`.

Parent CID is currently fixed at `3`.

Runtime knobs:

- `ARGONAUT_MAX_CONNECTIONS` controls the per-listener active bridge connection
  limit. It defaults to `1024`.
- `ARGONAUT_IDLE_TIMEOUT` controls the per-direction bridge read/write idle
  timeout. It defaults to `5m`; set it to `0` to disable idle deadlines.
- `ARGONAUT_HTTP_BIND_ADDR` controls the host-mode inbound TCP bind address. It
  defaults to `0.0.0.0`; set it to `127.0.0.1` when another local proxy or load
  balancer should own public exposure.
- `ARGONAUT_LOG_CONNECTIONS=false` disables per-connection accept/close logs.
  This is useful for load tests where synchronous stderr logging can become the
  benchmark bottleneck.

## NSM Protocol

The v0.1 protocol is JSON Lines. Each request is one JSON object followed by
`\n`; each response is one JSON object followed by `\n`.

Attestation:

```json
{"id":"1","method":"ATT","publicKey":"<hex>","nonce":"<hex>","userData":"<hex>"}
{"id":"1","ok":true,"data":"<hex-attestation-doc>"}
```

Random:

```json
{"id":"2","method":"RND"}
{"id":"2","ok":true,"data":"<hex-random-bytes>"}
```

Errors:

```json
{"id":"1","ok":false,"error":"invalid_hex"}
```

`id` is an opaque non-empty printable ASCII token up to 64 bytes. Allowed
characters are letters, digits, `.`, `_`, `:`, and `-`. `nonce` and `userData`
are optional. Hex strings are lowercase in responses; requests accept any valid
Go hex input.

For compatibility with existing callers, `argonaut nsm` also accepts the legacy
space protocol:

```text
<id> ATT <hex-public-key>
<id> ATT <hex-public-key> <hex-nonce|-> <hex-user-data|->
<id> RND
```

Legacy responses remain:

```text
<id> OK <hex>
<id> ERR <reason>
```

New consumers should use JSON Lines.

## Trust Model

The EC2 host is untrusted. Config delivered from the host is treated as untrusted
input before it can affect `/etc/hosts` or listeners. This validation prevents
obvious parser and hosts-file abuse, but the config is still application policy:
use enclave-embedded allowlists, signed config, or verifier-side policy checks
when a deployment must prevent the host from choosing endpoints. Attestation
freshness is the verifier's responsibility; verifiers should send a nonce and
verify that the returned NSM attestation document contains it.

## Verification

Before tagging a release:

```bash
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
go test -run=Fuzz -fuzz=FuzzDecodeNsmResponse -fuzztime=10s
go test -run=Fuzz -fuzz=FuzzHandleNsmLine -fuzztime=10s
go test -run=Fuzz -fuzz=FuzzParseConfig -fuzztime=10s
```

Build release artifacts with a patched Go toolchain and run a real Nitro smoke
test before publishing binaries; local tests cannot exercise `/dev/nsm` or
AF_VSOCK on non-Nitro hosts.

## Nitro Smoke Test

Run the standalone hardware smoke test on an EC2 instance with Nitro Enclaves
enabled:

```bash
scripts/nitro-smoke.sh
```

Prerequisites:

- Nitro Enclaves-capable EC2 instance with enclave allocator configured.
- `nitro-cli`, Docker, Go, `jq`, and `curl`.
- Permission to run `nitro-cli` and update `/etc/hosts`; the script uses `sudo`
  when it is not already running as root.

The smoke test builds a minimal EIF containing `/argonaut` and an in-enclave
test runner. It verifies:

- `argonaut config recv 7777` receives host config over real VSOCK.
- `argonaut nsm` can read real `/dev/nsm`, return hardware random bytes, and
  return an attestation document for a JSON Lines `ATT` request with
  `publicKey`, `nonce`, and `userData`.
- `argonaut enclave` validates config and writes the managed `/etc/hosts` block.
- Enclave outbound traffic reaches a host-side TCP echo server through
  `TCP:127.0.0.1:9443 -> VSOCK:3:8100 -> TCP:argonaut-smoke.local:18443`.
- Host inbound traffic reaches an enclave HTTP endpoint through
  `TCP:127.0.0.1:18080 -> VSOCK:<cid>:3000 -> TCP:127.0.0.1:3000`.

The smoke EIF is launched with Nitro debug mode enabled so the script can read
the enclave console. Do not use the smoke-test launch flags for production
enclaves.

Useful environment overrides:

```bash
ARGONAUT_SMOKE_HTTP_PORT=18080
ARGONAUT_SMOKE_HTTP_VSOCK_PORT=3000
ARGONAUT_SMOKE_HOST_ECHO_PORT=18443
ARGONAUT_SMOKE_OUTBOUND_VSOCK_PORT=8100
ARGONAUT_SMOKE_HOST=argonaut-smoke.local
ARGONAUT_SMOKE_LOCAL_IP=127.0.0.1
ARGONAUT_SMOKE_LOCAL_PORT=9443
ARGONAUT_SMOKE_CPUS=2
ARGONAUT_SMOKE_MEMORY=1024
ARGONAUT_SMOKE_KEEP_WORKDIR=1
```

With `ARGONAUT_SMOKE_KEEP_WORKDIR=1`, the script keeps the generated EIF,
console log, host log, and temporary Docker build context for debugging.

## Nitro Inbound Benchmark

Run the hardware benchmark on an EC2 instance with Nitro Enclaves enabled:

```bash
scripts/nitro-bench.sh
```

The benchmark builds a benchmark-only EIF containing `/argonaut` and a
deterministic in-enclave HTTP server. Host-side `vegeta` drives traffic through
the real inbound path:

```text
vegeta -> TCP:<httpPort> -> argonaut host -> VSOCK -> argonaut enclave -> enclave HTTP server
```

The production `argonaut` binary does not include benchmark modes or benchmark
traffic generation. The generated workdir contains TSV, JSON, and text
summaries; raw vegeta binary result files are exported only when
`ARGONAUT_BENCH_EXPORT_RAW=1`.

The benchmark EIF is launched with Nitro debug mode enabled for observability.
Benchmark numbers are useful for bridge sizing, not for attestation claims.

Useful benchmark overrides:

```bash
ARGONAUT_BENCH_HTTP_PORT=18080
ARGONAUT_BENCH_HTTP_VSOCK_PORT=3000
ARGONAUT_BENCH_CPUS=2
ARGONAUT_BENCH_MEMORY=1024
ARGONAUT_BENCH_MAX_CONNECTIONS=4096
ARGONAUT_BENCH_RATES="100 250 500 1000"
ARGONAUT_BENCH_PAYLOADS="0 1024 32768"
ARGONAUT_BENCH_DURATION=15s
ARGONAUT_BENCH_KEEP_WORKDIR=1
ARGONAUT_BENCH_RESULTS_DIR=benchmark-results
ARGONAUT_BENCH_EXPORT_RAW=1
```

## Benchmark Results

These are early hardware benchmark notes from short spot-instance runs. Treat
them as directional limits, not formal release claims.

Common environment:

- Date: 2026-05-22
- Region: `us-west-1`
- Path:

```text
host vegeta -> TCP:18080 -> argonaut host -> VSOCK -> argonaut enclave -> enclave HTTP server
```

- `ARGONAUT_LOG_CONNECTIONS=0`
- `ARGONAUT_MAX_CONNECTIONS=4096`
- `vegeta v12.12.0`

`c5.xlarge`, 2-vCPU / 1024 MiB enclave, small responses, 10s runs:

| Rate | Payload bytes | Requests | Throughput | Success % | p50 ms | p95 ms | p99 ms | Max ms | Errors |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 2000 | 0 | 20000 | 2000.12 | 100.00 | 0.66 | 1.11 | 5.24 | 38.79 | 0 |
| 4000 | 0 | 36450 | 1129.31 | 54.63 | 2838.13 | 9789.17 | 10055.59 | 13251.60 | 5970 |
| 8000 | 0 | 18810 | 489.85 | 45.72 | 6102.11 | 11442.82 | 12596.79 | 14946.10 | 1661 |
| 2000 | 1024 | 20000 | 2000.05 | 100.00 | 0.70 | 1.85 | 6.38 | 18.27 | 0 |
| 4000 | 1024 | 34691 | 872.58 | 48.52 | 4220.12 | 10938.55 | 11486.80 | 13152.21 | 3207 |
| 8000 | 1024 | 36398 | 916.40 | 50.13 | 3108.72 | 10227.28 | 11363.13 | 12756.89 | 4307 |

On this shape, 2000 req/s for small responses was clean. 4000 req/s
overloaded the path hard, with multi-second latency and substantial failures.

`c5.xlarge`, 2-vCPU / 1024 MiB enclave, 1 MiB responses, 10s runs:

| Rate | Payload bytes | Requests | Throughput | Success % | p50 ms | p95 ms | p99 ms | Max ms | Errors |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 10 | 1048576 | 100 | 10.09 | 100.00 | 7.50 | 13.11 | 15.89 | 17.80 | 0 |
| 25 | 1048576 | 250 | 25.08 | 100.00 | 7.13 | 13.41 | 17.20 | 21.58 | 0 |
| 50 | 1048576 | 500 | 50.06 | 100.00 | 7.98 | 15.27 | 19.72 | 23.12 | 0 |
| 100 | 1048576 | 1000 | 99.76 | 100.00 | 114.38 | 328.13 | 404.95 | 511.41 | 0 |
| 200 | 1048576 | 1998 | 97.37 | 85.19 | 7756.15 | 10000.48 | 10012.01 | 10085.12 | 146 |

For this tested allocation, 50 req/s of 1 MiB responses was clean. 100 req/s
completed with 100% success, but latency jumped sharply. 200 req/s overloaded
the path.

Follow-up spot runs on `c5.2xlarge` tested 1 MiB responses to separate host
headroom from enclave CPU allocation.

`c5.2xlarge`, 4-vCPU / 4096 MiB enclave, 10s runs:

| Rate | Payload bytes | Requests | Throughput | Success % | p50 ms | p95 ms | p99 ms | Max ms | Errors |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 50 | 1048576 | 500 | 50.07 | 100.00 | 7.75 | 39.92 | 100.61 | 132.35 | 0 |
| 100 | 1048576 | 1000 | 100.03 | 100.00 | 6.91 | 67.45 | 113.17 | 130.41 | 0 |
| 150 | 1048576 | 1500 | 150.03 | 100.00 | 6.43 | 153.75 | 210.87 | 237.39 | 0 |
| 200 | 1048576 | 2000 | 164.22 | 100.00 | 1994.89 | 3114.25 | 3435.15 | 3499.17 | 0 |
| 300 | 1048576 | 3000 | 160.76 | 73.80 | 4190.73 | 7231.53 | 7758.54 | 9124.47 | 1 |

`c5.2xlarge`, 2-vCPU / 4096 MiB enclave, 10s runs:

| Rate | Payload bytes | Requests | Throughput | Success % | p50 ms | p95 ms | p99 ms | Max ms | Errors |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 50 | 1048576 | 500 | 50.07 | 100.00 | 5.39 | 8.48 | 36.72 | 63.10 | 0 |
| 100 | 1048576 | 1000 | 94.05 | 100.00 | 6.24 | 1499.18 | 1669.41 | 1736.40 | 0 |
| 150 | 1048576 | 1500 | 121.97 | 100.00 | 2569.00 | 3390.46 | 3796.14 | 3869.84 | 0 |
| 200 | 1048576 | 2000 | 102.93 | 81.70 | 6475.67 | 9783.18 | 9999.35 | 10004.67 | 163 |

On the same `c5.2xlarge` host, moving from 2 to 4 enclave vCPUs shifted the
clean 1 MiB response envelope from roughly 50 req/s to roughly 150 req/s. The
path still hits a hard latency cliff around 200 req/s, so AF_VSOCK copy and
backpressure overhead remain material even with more enclave CPU.

## AWS Spot CI Runner

To run the Nitro smoke test from a regular CI worker, use:

```bash
AWS_REGION=us-east-1 scripts/aws-spot-nitro-smoke.sh
```

To run the inbound benchmark from a regular CI worker, use:

```bash
AWS_REGION=us-east-1 scripts/aws-spot-nitro-bench.sh
```

The runner script provisions a temporary one-time spot EC2 instance, installs
Nitro Enclaves tooling, Docker, Go, `jq`, and `curl`, copies this repo to the
instance, runs the requested smoke or benchmark command, then terminates the
instance. By default it also creates and deletes a temporary EC2 key pair and
security group.

Defaults are intentionally cheap and disposable:

- Instance type: `c5.xlarge`
- AMI: latest Amazon Linux 2023 x86_64 from SSM
- Market: one-time spot with interruption behavior `terminate`
- Enclave allocator: 2 CPUs and 1024 MiB

Useful CI environment variables:

```bash
AWS_REGION=us-east-1
ARGONAUT_CI_INSTANCE_TYPE=c5.xlarge
ARGONAUT_CI_SPOT_MAX_PRICE=0.10
ARGONAUT_CI_SUBNET_ID=subnet-...
ARGONAUT_CI_SECURITY_GROUP_ID=sg-...
ARGONAUT_CI_KEY_NAME=existing-key
ARGONAUT_CI_PRIVATE_KEY_FILE=/path/to/existing-key.pem
ARGONAUT_CI_SSH_CIDR=203.0.113.10/32
ARGONAUT_CI_RUN_COMMAND="ARGONAUT_SMOKE_KEEP_WORKDIR=1 scripts/nitro-smoke.sh"
ARGONAUT_CI_FETCH_PATH=benchmark-results
ARGONAUT_CI_ARTIFACT_DIR=.argonaut-ci-artifacts/run
ARGONAUT_CI_KEEP_INSTANCE=1
```

If subnet, security group, or key pair values are omitted, the script uses a
default subnet and creates temporary SSH access scoped to the CI runner's public
IP. Set `ARGONAUT_CI_SSH_CIDR` explicitly if automatic public IP discovery is
unavailable. Set `ARGONAUT_CI_KEEP_INSTANCE=1` only for debugging; otherwise
cleanup terminates the spot instance and removes temporary AWS resources.
