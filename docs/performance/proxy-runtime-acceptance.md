# Proxy Runtime Acceptance

## Scope

The corpus covers weighted selection and a loopback request through route matching, plugin middleware, upstream selection, retry/timeout transport wrappers, ReverseProxy, and response copying. It does not represent public-network or cross-region production RPS.

The loopback benchmark corpus is exercised through the immutable baseline runner. The cached-environment harness (`perf(route): cache loopback benchmark environments`) keeps untimed setup flat so full-corpus runs complete in minutes instead of an hour. Accepted comparisons run on the repository's declared Go 1.26 toolchain and record the exact patch version in benchmark metadata.

The GC hot-path corpus adds focused `BenchmarkRequestPipelineHotPath` and `BenchmarkSnapshotMetricsFinalizer` rows. These rows isolate request-pipeline materialization and detached metrics finalization, but still include their test request and response fixtures. They are allocation-regression evidence, not production throughput claims.

## Comparative gates

- Primary row: `routes=100/plugins=none/nodes=10` ns/op.
- Reject a statistically significant slowdown greater than 10% in any affected row.
- Reject more than 512 additional B/op or 2 additional allocs/op in any affected row without an explicitly accepted retained object.
- Comparisons are invalid when benchmark metadata or corpus fingerprints differ.

## Stability gates

- Focused race tests report no races.
- Fault tests produce exact status and attempt counts:
  - Reset/EOF transport failures map to 502; GET retries once per configured retry, POST/PATCH retry only with an `Idempotency-Key` or `X-Idempotency-Key`.
  - Response-header inactivity maps to 504 and is retried like any transport failure.
  - Body inactivity on a committed response terminates the copy within the read timeout; Go's `ReverseProxy` aborts the client connection (`ErrAbortHandler`) rather than rewriting the committed status, so the client observes a transport error, never a truncated 200 or a 504 rewrite.
  - Cluster admission rejects requests beyond `max_in_flight` with 503 and resumes once a held body closes.
  - Active health probes quarantine a failing node and re-admit it after the configured consecutive successes.
- The 30-minute, concurrency-256 soak produces zero unexpected errors.
- Final goroutines are at most warmup baseline plus 32.
- Heap in use after two final GCs is at most 25% above the warmed five-minute sample.
- The measurement window reports request count and throughput plus bounded p50, p95, p99, and p999 latency estimates. Request latency uses fixed upper-bound buckets and stores no per-request samples.
- The same window reports allocation bytes, allocation bytes per request, GC CPU seconds, and p99/p999 deltas for runtime GC and scheduler-other pause histograms. Runtime histograms are cumulative counters, so only the end-minus-warmup delta is interpreted.
- Runtime allocation and pause deltas cover the entire single-process soak harness: its client workers, gateway, ten upstream test servers, and test machinery. Treat them as comparative harness evidence, never as gateway-only or production allocation figures.

`APISIX_GO_SOAK_DURATION=5s` is a wiring smoke for the measurement path only. It is not stability or tail-latency acceptance evidence; the accepted stability run remains 30 minutes at concurrency 256.

## Evidence location

Raw benchmark, benchstat, and pprof artifacts are stored under ignored `.cache/bench`. Delivery reports include the commands, HEAD SHA, hardware, Go version, affected rows, and any regressions; raw artifacts are never committed.
