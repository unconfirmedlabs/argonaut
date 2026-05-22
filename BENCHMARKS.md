# argonaut Benchmarks

These are early hardware benchmark notes from short spot-instance runs. Treat
them as directional limits, not formal release claims.

## Environment

- Date: 2026-05-22
- Region: `us-west-1`
- EC2 instance: `c5.xlarge` spot
- Enclave allocation: 2 vCPU, 1024 MiB
- Path tested:

```text
host vegeta -> TCP:18080 -> argonaut host -> VSOCK -> argonaut enclave -> enclave HTTP server
```

- Per-connection argonaut logs disabled with `ARGONAUT_LOG_CONNECTIONS=0`.
- Argonaut connection limit set to `4096`.
- Load generator: `vegeta v12.12.0`

## Short-Run Results

Columns:

```text
rate payload_bytes requests throughput success_percent p50_ms p95_ms p99_ms max_ms error_count
```

### Small Responses, 10s Runs

```text
2000  0     20000  2000.12  100.00  0.66     1.11      5.24      38.79     0
4000  0     36450  1129.31   54.63  2838.13  9789.17  10055.59  13251.60  5970
8000  0     18810   489.85   45.72  6102.11 11442.82  12596.79  14946.10  1661
2000  1024  20000  2000.05  100.00  0.70     1.85      6.38      18.27     0
4000  1024  34691   872.58   48.52  4220.12 10938.55  11486.80  13152.21  3207
8000  1024  36398   916.40   50.13  3108.72 10227.28  11363.13  12756.89  4307
```

Interpretation: on this instance shape, 2000 req/s for small responses was
clean. 4000 req/s overloaded the path hard, with multi-second latency and
substantial failures.

### 1 MiB Responses, 10s Runs

```text
10   1048576   100  10.09  100.00  7.50     13.11     15.89     17.80     0
25   1048576   250  25.08  100.00  7.13     13.41     17.20     21.58     0
50   1048576   500  50.06  100.00  7.98     15.27     19.72     23.12     0
100  1048576  1000  99.76  100.00  114.38   328.13    404.95    511.41    0
200  1048576  1998  97.37   85.19  7756.15 10000.48  10012.01  10085.12  146
```

Interpretation: 50 req/s of 1 MiB responses was clean. 100 req/s still completed
with 100% success, but latency jumped sharply. 200 req/s overloaded the path.

## Practical Takeaway

For the tested `c5.xlarge` / 2-vCPU enclave shape:

- Small HTTP responses: stay at or below roughly 2000 req/s for clean short-run
  behavior.
- 1 MiB responses: stay well below 100 req/s if low latency matters; 50 req/s
  was clean in this run.

These results should be rerun with longer durations before publishing marketing
claims or SLO guidance.

## Resource Scaling Check

Follow-up spot runs tested 1 MiB responses on `c5.2xlarge` to separate host
headroom from enclave CPU allocation.

### c5.2xlarge, 4-vCPU / 4096 MiB enclave, 10s runs

```text
50   1048576   500  50.07   100.00  7.75     39.92     100.61    132.35    0
100  1048576  1000  100.03  100.00  6.91     67.45     113.17    130.41    0
150  1048576  1500  150.03  100.00  6.43     153.75    210.87    237.39    0
200  1048576  2000  164.22  100.00  1994.89  3114.25   3435.15   3499.17   0
300  1048576  3000  160.76   73.80  4190.73  7231.53   7758.54   9124.47   1
```

### c5.2xlarge, 2-vCPU / 4096 MiB enclave, 10s runs

```text
50   1048576   500  50.07   100.00  5.39     8.48      36.72     63.10     0
100  1048576  1000  94.05   100.00  6.24     1499.18   1669.41   1736.40   0
150  1048576  1500  121.97  100.00  2569.00  3390.46   3796.14   3869.84   0
200  1048576  2000  102.93   81.70  6475.67  9783.18   9999.35  10004.67  163
```

Interpretation: host-side headroom helps the 50 req/s case, but enclave CPU is a
major bottleneck for large responses. On the same `c5.2xlarge` host, moving from
2 to 4 enclave vCPUs shifted the clean 1 MiB response envelope from roughly 50
req/s to roughly 150 req/s. The path still hits a hard latency cliff around
200 req/s, so AF_VSOCK/copy/backpressure overhead remains material even with
more enclave CPU.
