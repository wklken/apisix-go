# GC Hot-Path P0 Measurement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Establish gateway-owned allocation rows and bounded tail/GC soak evidence before modifying the P1 hot paths.

**Architecture:** Add focused benchmarks next to the request pipeline and detached metrics finalizer, then include them in the immutable benchmark corpus. Extend the opt-in proxy soak with a fixed-size latency histogram and `runtime/metrics` snapshots; measurement state stays test-only and bounded.

**Tech Stack:** Go 1.26 `testing`, `runtime/metrics`, `sync/atomic`, repository benchmark runner, pinned benchstat.

**Spec:** `docs/superpowers/specs/2026-08-18-gc-hotpath-p0-p1.md`

## Implementation Outcome

The immutable baseline/current comparison was rerun from `origin/master`
`42db7bf2` with identical benchmark sources and metadata. The production-shaped
pipeline row includes request phase, streaming executor, and log executor
prepare/seal/finalize work. The detached metrics row exercises the effective
Prometheus log binding. Results are recorded under
`.cache/bench/gc-hotpath-pr`; loopback and microbenchmark numbers remain
comparative evidence rather than production capacity claims.

## Global Constraints

- Run every Go command as `bash -lc 'source .envrc && ...'` from the repository root.
- Preserve every existing benchmark row and soak correctness threshold.
- Do not add dependencies or production configuration.
- Keep all benchmark/profile outputs under ignored `.cache/bench`.
- Do not claim production RPS from these benchmarks.
- The current fast-plan-impl run has local-mutation authority only; the commit steps below are handoff commands and must not be executed without separate commit authority.

---

## File Map

- Create `pkg/plugin/executor_benchmark_test.go`: stable production-shaped static/dynamic request-pipeline allocation rows.
- Modify `pkg/plugin/prometheus/benchmark_test.go`: benchmark detached snapshot metric finalization alongside the scrape benchmark.
- Create `pkg/route/proxy_soak_metrics_test.go`: fixed-size latency histogram and runtime-metric delta helpers with deterministic tests.
- Modify `pkg/route/proxy_soak_test.go`: observe request latency and report percentile/runtime deltas.
- Modify `Makefile`: register the new benchmark packages and corpus files.
- Modify `docs/performance/proxy-runtime-acceptance.md`: document the new evidence and interpretation boundary.

### Task 1: Add stable request-pipeline and snapshot-finalizer benchmarks

**Files:**
- Create: `pkg/plugin/executor_benchmark_test.go`
- Modify: `pkg/plugin/prometheus/benchmark_test.go`
- Modify: `Makefile:10-26`

**Interfaces:**
- Consumes: `NewRequestPipeline`, `NewStreamingResponseExecutor`, `prometheus.Plugin.RunLogPhase`, `base.LogSnapshot`.
- Produces: `BenchmarkRequestPipelineHotPath/static-unresolved`, `BenchmarkRequestPipelineHotPath/consumer-resolved`, and `BenchmarkSnapshotMetricsFinalizer/default`.

- [ ] **Step 1: Add the production-shaped request-pipeline benchmark**

Use one immutable request-stage binding with the registered factory identity `request-id`, an empty streaming executor, a no-op logger binding, and a resolver that returns the input request. Build the handler and log executor before `ResetTimer`; construct a fresh lifecycle, request, and recorder inside each iteration so request-local context never leaks:

```go
func BenchmarkRequestPipelineHotPath(b *testing.B) {
	static := BindPlugin("request-id", benchmarkRequestPhase{}, ScopeRoute, ResourceProvenance{Kind: ResourceRoute, ID: "bench"})
	streaming, err := NewStreamingResponseExecutor(nil)
	if err != nil { b.Fatal(err) }
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	for _, resolved := range []bool{false, true} {
		name := "static-unresolved"
		if resolved { name = "consumer-resolved" }
		b.Run(name, func(b *testing.B) {
			resolver := func(r *http.Request) (ConsumerResolution, error) {
				result := ConsumerResolution{Request: r, Resolved: resolved}
				if resolved { result.Bindings = []Binding{static} }
				return result, nil
			}
			handler := NewRequestPipeline([]Binding{static}, resolver).
				WithStreamingResponseExecutor(streaming).
				Then(terminal)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bench", nil))
			}
		})
	}
}
```

`benchmarkRequestPhase` implements `Plugin` plus `base.RequestPhasePlugin`; its request phase returns `base.ContinueRequest(r)` and it owns no mutable request-external state.

- [ ] **Step 2: Add the detached snapshot metrics benchmark**

Create one initialized Prometheus `Plugin` and one immutable `base.LogSnapshot` containing route/service/matched URI, consumer, upstream latency, method/host/path, and request/LLM variables. Reuse the snapshot because `RunLogPhase` must be read-only:

```go
func BenchmarkSnapshotMetricsFinalizer(b *testing.B) {
	if err := metrics.Init(); err != nil { b.Fatal(err) }
	p := &Plugin{}
	if err := p.Init(); err != nil { b.Fatal(err) }
	if err := p.PostInit(); err != nil { b.Fatal(err) }
	snapshot := benchmarkMetricsSnapshot()
	b.Run("default", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := p.RunLogPhase(snapshot); err != nil { b.Fatal(err) }
		}
	})
}
```

- [ ] **Step 3: Register the corpus**

Add `./pkg/plugin` and `./pkg/plugin/prometheus` to `BENCH_PACKAGES`. Add these exact files to `BENCH_CORPUS_FILES`:

```make
	pkg/plugin/executor_benchmark_test.go \
	pkg/plugin/prometheus/benchmark_test.go \
```

- [ ] **Step 4: Run the benchmark smoke gate**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/plugin/prometheus -run "^$" -bench "^(BenchmarkRequestPipelineHotPath|BenchmarkSnapshotMetricsFinalizer)$" -benchmem -benchtime=100ms -count=1 -cpu=1'
```

Expected: all three rows report `ns/op`, `B/op`, and `allocs/op`; no request leaks state into a later iteration.

- [ ] **Step 5: Prepare the measurement-only commit command**

Do not execute without commit authority:

```bash
git add Makefile pkg/plugin/executor_benchmark_test.go pkg/plugin/prometheus/benchmark_test.go
git commit -m "test(perf): benchmark request hot paths"
```

### Task 2: Add bounded latency and runtime-GC soak evidence

**Files:**
- Create: `pkg/route/proxy_soak_metrics_test.go`
- Modify: `pkg/route/proxy_soak_test.go:20-200`
- Modify: `docs/performance/proxy-runtime-acceptance.md`

**Interfaces:**
- Produces: `soakLatencyHistogram.Observe(time.Duration)`, `Snapshot() soakLatencySnapshot`, and `soakRuntimeSnapshot`.
- Consumes: cumulative `runtime/metrics` samples at warmup and completion.

- [ ] **Step 1: Implement and test a fixed-size latency histogram**

Use fixed microsecond upper bounds and one overflow bucket; never store individual request latencies:

```go
var soakLatencyBounds = [...]uint64{
	50, 100, 200, 500, 1_000, 2_000, 5_000, 10_000,
	20_000, 50_000, 100_000, 200_000, 500_000, 1_000_000,
	2_000_000, 5_000_000, 10_000_000, 30_000_000, 60_000_000,
}

type soakLatencyHistogram struct {
	buckets [len(soakLatencyBounds) + 1]atomic.Uint64
}
```

`Observe` converts to rounded-up microseconds, uses `sort.Search`, and increments exactly one bucket. `Snapshot` copies counts. `Delta` subtracts a warm snapshot from an end snapshot. `Quantile` returns the selected bucket upper bound and uses `time.Duration(math.MaxInt64)` for overflow.

Add deterministic tests proving p50/p95/p99/p999 selection, warm/end subtraction, zero samples, and overflow.

- [ ] **Step 2: Capture supported runtime metrics without production changes**

Read these names using `runtime/metrics.Read`:

```go
var soakRuntimeMetricNames = [...]string{
	"/gc/heap/allocs:bytes",
	"/cpu/classes/gc/total:cpu-seconds",
	"/sched/pauses/total/gc:seconds",
	"/sched/pauses/total/other:seconds",
}
```

Copy histogram counts at snapshot time. Implement delta helpers that require identical bucket boundaries and calculate p99/p999 from count deltas. Tests must use synthetic `metrics.Float64Histogram` values; they must not force a real GC.

- [ ] **Step 3: Instrument only the opt-in soak**

Around each `client.Get` plus response drain/close, observe elapsed time once. At warmup and end, capture request counts, latency histogram, and runtime metrics. Report this exact evidence group:

```text
measurement requests, requests/second, p50, p95, p99, p999,
allocated bytes, allocated bytes/request, GC CPU seconds,
GC pause p99/p999, scheduler-other pause p99/p999
```

Keep the existing error, goroutine, and heap assertions unchanged.

- [ ] **Step 4: Run focused tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "^(TestSoakLatency|TestSoakRuntime)" -count=1'
bash -lc 'source .envrc && APISIX_GO_RUN_SOAK=1 APISIX_GO_SOAK_DURATION=5s go test ./pkg/route -run "^TestProxyRuntimeSoak$" -count=1 -v'
```

Expected: helper tests pass; the five-second smoke reports all requested fields, zero request errors, and preserves existing resource-bound assertions.

- [ ] **Step 5: Document the evidence boundary**

State that latency buckets are bounded approximations, runtime histograms are cumulative deltas, the five-second command is a wiring smoke only, and accepted stability evidence remains the 30-minute concurrency-256 soak.

- [ ] **Step 6: Prepare the soak-measurement commit command**

Do not execute without commit authority:

```bash
git add pkg/route/proxy_soak_test.go pkg/route/proxy_soak_metrics_test.go docs/performance/proxy-runtime-acceptance.md
git commit -m "test(perf): report proxy tail and GC soak metrics"
```

### Task 3: Record the immutable pre-P1 baseline

**Files:**
- No tracked files change.
- Output: `.cache/bench/gc-hotpath-pre-p1-20260818.txt` and `.meta`.

**Interfaces:**
- Consumes: Tasks 1-2 and unchanged P1 production files.
- Produces: the only valid baseline for the P1 comparison.

- [ ] **Step 1: Verify the runner and declare the hypothesis**

Run:

```bash
bash -lc 'source .envrc && make benchmark-runner-test'
```

Record:

```text
Hypothesis: prebuilding the unresolved static request pipeline and recording detached request metrics without reconstructing http.Request reduce allocation count/bytes without a statistically significant latency regression above 10%. No affected row may increase B/op or allocs/op.
```

- [ ] **Step 2: Run the immutable baseline**

Run:

```bash
bash -lc 'source .envrc && BENCH_PACKAGES="./pkg/plugin ./pkg/plugin/prometheus" BENCH_CORPUS_FILES="pkg/plugin/executor_benchmark_test.go pkg/plugin/prometheus/benchmark_test.go" BENCH_REGEX="^(BenchmarkRequestPipelineHotPath|BenchmarkSnapshotMetricsFinalizer)$" BENCH_TIME=1s BENCH_COUNT=10 BENCH_CPU=1,4 BENCH_P=1 bash scripts/benchmark.sh run gc-hotpath-pre-p1-20260818'
```

Expected: immutable result and metadata files are published exactly once. A pre-existing label is a hard stop.

- [ ] **Step 3: Verify baseline completeness and clean tracking**

Run:

```bash
rg -n '^Benchmark(RequestPipelineHotPath|SnapshotMetricsFinalizer)' .cache/bench/gc-hotpath-pre-p1-20260818.txt
sed -n '1,100p' .cache/bench/gc-hotpath-pre-p1-20260818.meta
git status --short
```

Expected: every row exists, metadata records the current snapshot, and `.cache` is absent from Git status.
