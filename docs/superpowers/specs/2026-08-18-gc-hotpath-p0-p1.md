# GC Hot-Path P0/P1 Design

## Goal

Reduce allocation-driven GC pressure and request-tail jitter in the high-concurrency HTTP data path without changing APISIX request, response, logging, metrics, streaming, or consumer-override semantics.

## Confirmed baseline

- Go 1.26 is the repository target; every Go command sources `.envrc` first.
- The repository already owns an immutable benchmark runner and a full loopback proxy benchmark.
- The loopback benchmark includes client, gateway, and upstream HTTP work, so it cannot attribute all allocations to the gateway.
- `RequestPipeline.runResolved` currently merges bindings and rebuilds the post-resolution handler on every request, including the common no-authentication path.
- `prometheus.RunLogPhase` reconstructs an `http.Request`, URL, headers, contexts, and request-variable maps from an already-detached `LogSnapshot` before recording Prometheus metrics.
- The opt-in proxy soak checks errors, goroutines, and heap growth but does not report request latency percentiles or runtime GC/scheduler deltas.

## P0 measurement contract

1. Add stable `-benchmem` rows for the production-shaped static request pipeline and detached request-metrics finalizer.
2. Register those files in the repository benchmark corpus so immutable baseline/current comparisons reject corpus drift.
3. Extend the existing bounded soak with allocation-free request-latency observation and `runtime/metrics` deltas for allocation bytes, GC CPU, GC pauses, and scheduler pauses.
4. Report p50, p95, p99, and p999 request latency. Histogram storage must remain fixed-size for the full soak duration.
5. Benchmark and profile artifacts remain ignored under `.cache/bench`; no production RPS claim may be derived from loopback or microbenchmarks.

## P1 implementation contract

### Static request pipeline

- Prebuild the immutable post-resolution handler once for requests whose consumer resolver returns `Resolved=false` and no dynamic bindings.
- Continue invoking post-resolution hooks for every request.
- Continue using the dynamic merge/materialization path for every resolved consumer/group request, including resolved requests with zero plugin bindings.
- Reuse the complete immutable post-resolution handler only on the unresolved static path. Keep the resolved
  consumer/group construction path unchanged unless the benchmark proves that moving its streaming invariants
  does not regress CPU, bytes, or allocations.
- Preserve authentication ordering, static CORS behavior, request replacement, response buffering, logging finalization, streaming, and panic behavior.

### Detached metrics input

- Add a typed detached request-metrics input containing method, host, path, APISIX variables, request variables, and response source.
- Record request metrics directly from the detached `LogSnapshot`; do not reconstruct `http.Request`, URL, headers, or context maps.
- Retain `RecordHTTPRequest(*http.Request, HTTPRequestMetrics)` as a compatibility wrapper.
- Preserve all base labels, configured extra labels, status normalization, response-source fallback, LLM metrics, series limits, expiration, and overflow behavior.

## Acceptance

- Focused behavior tests pass before and after each change.
- `make build` passes after the combined code changes.
- The immutable current benchmark uses the exact P0 corpus/settings as the baseline.
- No affected row may add allocations or bytes per operation. An optimization is accepted only when its owning row reduces `allocs/op` or `B/op` without a statistically significant latency regression greater than 10%.
- Race checks cover the changed plugin and metrics packages.
- No dependency, configuration, protocol, public metric name, or cardinality contract changes.

## Non-goals

- Replacing `net/http` or `httputil.ReverseProxy` with fasthttp.
- Pooling `http.Request`, contexts, unbounded maps, response bodies, timers, or objects retained by asynchronous finalizers.
- Caching dynamic consumer execution plans without an independently reviewed capacity and invalidation design.
- Replacing the response capture wrapper before a dedicated compatibility and allocation benchmark exists.
- Setting production `GOGC`, `GOMEMLIMIT`, `GOMAXPROCS`, or container-memory values in repository defaults.
