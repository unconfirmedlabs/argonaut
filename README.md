# argonaut

`argonaut` is a single static Go binary for AWS Nitro Enclave companion work:

- VSOCK to TCP bridging in both directions.
- One-shot boot configuration delivery over `VSOCK:7777`.
- NSM attestation and hardware RNG access through a subprocess protocol.

The same binary runs on the EC2 parent instance and inside the enclave.

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

`id` is an opaque non-empty string up to 64 bytes with no whitespace/control
characters. `nonce` and `userData` are optional. Hex strings are lowercase in
responses; requests accept any valid Go hex input.

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
input before it can affect `/etc/hosts` or listeners. Attestation freshness is
the verifier's responsibility; verifiers should send a nonce and verify that the
returned NSM attestation document contains it.

## Verification

Before tagging a release:

```bash
go test ./...
go test -race ./...
go vet ./...
go test -run=Fuzz -fuzz=FuzzDecodeNsmResponse -fuzztime=10s
go test -run=Fuzz -fuzz=FuzzHandleNsmLine -fuzztime=10s
go test -run=Fuzz -fuzz=FuzzParseConfig -fuzztime=10s
```

Run a real Nitro smoke test before publishing binaries; local tests cannot
exercise `/dev/nsm` or AF_VSOCK on non-Nitro hosts.

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
traffic generation. Benchmark output is written under the generated workdir as
TSV, JSON, and vegeta binary result files.

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
IP when it can discover it. Set `ARGONAUT_CI_KEEP_INSTANCE=1` only for debugging;
otherwise cleanup terminates the spot instance and removes temporary AWS
resources.
