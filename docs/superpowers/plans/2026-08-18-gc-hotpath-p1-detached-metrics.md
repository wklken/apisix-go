# GC Hot-Path P1 Detached Metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record Prometheus request metrics directly from the detached log snapshot without rebuilding `http.Request`, URL, headers, contexts, or variable maps.

**Architecture:** Add one typed `HTTPRequestMetricContext` value consumed by the existing metric-label and series-tracker path. The production snapshot finalizer constructs it from already-detached fields; the existing live-request API remains a compatibility wrapper over the same implementation.

**Tech Stack:** Go 1.26, existing Prometheus client, detached APISIX log snapshot, focused tests and benchmarks.

**Spec:** `docs/superpowers/specs/2026-08-18-gc-hotpath-p0-p1.md`

## Implementation Outcome

The latest `master` assigns route-level request metrics to
`prometheus.Plugin.RunLogPhase`, so the implementation follows that owner rather
than the older request-context location. The final immutable comparison reduced
the detached finalizer by 21.34% to 25.66% `time/op`, 33.93% to 33.94% `B/op`,
and 19.64% `allocs/op` (56 to 45) without changing metric or label contracts.

## Global Constraints

- Run every Go command as `bash -lc 'source .envrc && ...'`.
- Preserve every metric name, label name/order/value, series budget, expiration, overflow, and active-connection contract.
- Do not mutate snapshot maps or retain request-local mutable maps beyond the synchronous call.
- Keep `RecordHTTPRequest(*http.Request, HTTPRequestMetrics)` source-compatible.
- Do not add pools, dependencies, configuration, goroutines, or unsafe code.
- The current fast-plan-impl run has local-mutation authority only; commit commands are handoff commands.

---

## File Map

- Modify `pkg/apisix/log/snapshot.go`: retain detached URL path as a scalar `Path` field.
- Modify `pkg/apisix/log/snapshot_test.go`: prove path detachment.
- Modify `pkg/observability/metrics/prometheus.go`: add typed metric context and route all request-metric label resolution through it.
- Modify `pkg/observability/metrics/prometheus_test.go`: prove live and detached inputs produce identical labels.
- Modify `pkg/plugin/prometheus/plugin.go`: delete request reconstruction and submit detached metrics directly.
- Modify `pkg/plugin/prometheus/plugin_test.go`: cover extra labels and response-source semantics from detached snapshots.

### Task 1: Carry detached request path explicitly

**Files:**
- Modify: `pkg/apisix/log/snapshot.go:25-50,422-445`
- Modify: `pkg/apisix/log/snapshot_test.go`

**Interfaces:**
- Produces: `RequestLogSnapshot.Path string`.
- Consumes: `r.URL.Path` at snapshot construction time.

- [ ] **Step 1: Add the failing detachment assertion**

Build a snapshot from `https://gateway.test/orders/42?q=one`, mutate the original request URL after construction, and assert:

```go
if got := snapshot.Request.Path; got != "/orders/42" {
	t.Fatalf("snapshot path = %q, want /orders/42", got)
}
```

- [ ] **Step 2: Store the scalar path**

Add `Path string` beside `URL string` and initialize it with `r.URL.Path` in `BuildSnapshotFromOwnedInputs`. Do not parse `URI` or `URL` later.

- [ ] **Step 3: Run snapshot tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/apisix/log -run "^TestBuildSnapshot" -count=1'
```

Expected: PASS.

### Task 2: Add a detached request-metrics context with a compatibility wrapper

**Files:**
- Modify: `pkg/observability/metrics/prometheus.go:100-120,480-690`
- Modify: `pkg/observability/metrics/prometheus_test.go`

**Interfaces:**
- Produces:

```go
type HTTPRequestMetricContext struct {
	Method         string
	Host           string
	Path           string
	APISIXVars     map[string]any
	RequestVars    map[string]any
	ResponseSource apisixctx.ResponseSource
}

func RecordHTTPRequestContext(HTTPRequestMetricContext, HTTPRequestMetrics)
```

- Retains: `func RecordHTTPRequest(*http.Request, HTTPRequestMetrics)`.

- [ ] **Step 1: Add equivalence tests first**

Install isolated metric vectors, construct one live request with method/host/path/APISIX/request vars and one equivalent detached context, then record each into a separate freshly installed vector set. Assert equal values for HTTP status, three latency observations, ingress/egress bandwidth, configured `$host`, `$uri`, `$request_method`, arbitrary APISIX/request extra labels, and `response_source`.

Also assert a detached AI request records LLM latency/token families exactly as the live wrapper.

- [ ] **Step 2: Implement the detached context helpers**

Move `RecordHTTPRequest` logic to `RecordHTTPRequestContext`. Add context-based helpers:

```go
func (c HTTPRequestMetricContext) requestVarString(key string) string
func (c HTTPRequestMetricContext) apisixVarString(key string) string
func (c HTTPRequestMetricContext) variable(entry HTTPRequestMetrics, variable string) string
func (c HTTPRequestMetricContext) responseSource(upstreamLatency int64) string
```

Use `fmt.Sprint` exactly where the live helpers do so non-string scalar behavior remains unchanged. `$host`, `$uri`, and `$request_method` read the typed scalar fields. An explicit detached `ResponseSource` wins; then `$response_source`; then the existing upstream-latency fallback.

- [ ] **Step 3: Keep the live API as a thin wrapper**

```go
func RecordHTTPRequest(r *http.Request, entry HTTPRequestMetrics) {
	context := HTTPRequestMetricContext{}
	if r != nil {
		context.Method = r.Method
		context.Host = r.Host
		if r.URL != nil { context.Path = r.URL.Path }
		context.APISIXVars = apisixctx.GetApisixVars(r)
		context.RequestVars = apisixctx.GetRequestVars(r)
	}
	RecordHTTPRequestContext(context, entry)
}
```

Do not change `BeginLLMRequest`; it owns a live request whose decrement closure intentionally retains stable labels.

- [ ] **Step 4: Run metrics behavior tests and race gate**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "^(TestPrometheus|TestRecordHTTPRequest|TestHTTPMetric|TestLLMMetric|TestMetricSeries)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/observability/metrics -run "^(TestRecordHTTPRequest|TestMetricSeries)" -count=1'
```

Expected: PASS with no label-order drift or race report.

### Task 3: Remove `http.Request` reconstruction from the snapshot finalizer

**Files:**
- Modify: `pkg/plugin/prometheus/plugin.go:70-170`
- Modify: `pkg/plugin/prometheus/plugin_test.go`

**Interfaces:**
- Consumes: `metrics.HTTPRequestMetricContext` and `base.LogSnapshot`.
- Deletes: private `requestFromSnapshot`.

- [ ] **Step 1: Add detached finalizer coverage**

Configure extra labels using `$host`, `$uri`, `$request_method`, one APISIX variable, and one request variable. Invoke `RunLogPhase` with no live request and assert the emitted status series contains all expected values plus `snapshot.Source`.

- [ ] **Step 2: Submit the detached context directly**

Replace `requestFromSnapshot(snapshot)` with:

```go
metricContext := metrics.HTTPRequestMetricContext{
	Method:         snapshot.Request.Method,
	Host:           snapshot.Request.Host,
	Path:           snapshot.Request.Path,
	APISIXVars:     snapshot.Request.APISIXVars,
	RequestVars:    snapshot.Request.RequestVars,
	ResponseSource: snapshot.Source,
}
```

Pass it to `metrics.RecordHTTPRequestContext`. Delete `requestFromSnapshot` and remove now-unused `net/url` and reconstruction imports.

- [ ] **Step 3: Run focused behavior and benchmark smoke gates**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/prometheus ./pkg/observability/metrics ./pkg/apisix/log -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/prometheus -run "^$" -bench "^BenchmarkSnapshotMetricsFinalizer$" -benchmem -benchtime=100ms -count=1 -cpu=1'
```

Expected: tests pass and the benchmark emits the unchanged row name.

- [ ] **Step 4: Prepare the commit command**

Do not execute without commit authority:

```bash
git add pkg/apisix/log/snapshot.go pkg/apisix/log/snapshot_test.go pkg/observability/metrics/prometheus.go pkg/observability/metrics/prometheus_test.go pkg/plugin/prometheus/plugin.go pkg/plugin/prometheus/plugin_test.go
git commit -m "perf(metrics): record detached request snapshots"
```

### Task 4: Compare the combined P1 result against the immutable baseline

**Files:**
- No tracked files change.
- Output: `.cache/bench/gc-hotpath-post-p1-20260818.*` and comparison output.

**Interfaces:**
- Consumes the unchanged P0 corpus and both P1 implementations.
- Produces the acceptance evidence for both P1 plans.

- [ ] **Step 1: Run current with identical settings**

Run:

```bash
bash -lc 'source .envrc && BENCH_PACKAGES="./pkg/plugin ./pkg/plugin/prometheus" BENCH_CORPUS_FILES="pkg/plugin/executor_benchmark_test.go pkg/plugin/prometheus/benchmark_test.go" BENCH_REGEX="^(BenchmarkRequestPipelineHotPath|BenchmarkSnapshotMetricsFinalizer)$" BENCH_TIME=1s BENCH_COUNT=10 BENCH_CPU=1,4 BENCH_P=1 bash scripts/benchmark.sh run gc-hotpath-post-p1-20260818'
```

- [ ] **Step 2: Compare and enforce acceptance**

Run:

```bash
bash -lc 'source .envrc && BENCH_PACKAGES="./pkg/plugin ./pkg/plugin/prometheus" BENCH_CORPUS_FILES="pkg/plugin/executor_benchmark_test.go pkg/plugin/prometheus/benchmark_test.go" BENCH_REGEX="^(BenchmarkRequestPipelineHotPath|BenchmarkSnapshotMetricsFinalizer)$" BENCH_TIME=1s BENCH_COUNT=10 BENCH_CPU=1,4 BENCH_P=1 bash scripts/benchmark.sh compare gc-hotpath-pre-p1-20260818 gc-hotpath-post-p1-20260818'
```

Expected: metadata matches. No affected row increases `B/op` or `allocs/op`; each owning optimized row reduces one allocation metric; no statistically significant `ns/op` regression exceeds 10%.

- [ ] **Step 3: Run combined code gates**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/plugin/prometheus ./pkg/observability/metrics ./pkg/apisix/log -count=1'
bash -lc 'source .envrc && make build'
```

Expected: PASS. Broad repository tests remain opt-in under repository rules.
