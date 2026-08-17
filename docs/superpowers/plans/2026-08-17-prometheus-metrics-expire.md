# Prometheus Metric Series Expiration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add APISIX-compatible, bounded idle expiration for the seven dynamic HTTP and LLM Prometheus metric families, driven by one independently managed cleanup goroutine.

**Architecture:** Each family owns a reusable bounded `metricSeriesTracker` that coordinates vector updates, `lastSeen`, in-flight pins, overflow routing, and `DeleteLabelValues` under one read/write synchronization protocol. A single lifecycle-owned expiration runtime scans enabled trackers in bounded batches and is stopped and awaited by the server.

**Tech Stack:** Go 1.26, `github.com/prometheus/client_golang/prometheus` v1.23.2, standard-library contexts, mutexes, atomics, timers, and focused Go tests with the race detector.

**Spec:** `docs/superpowers/specs/2026-08-17-prometheus-metrics-expire-design.md`

## Global Constraints

- Cover exactly `http_status`, `http_latency`, `bandwidth`, `llm_latency`, `llm_prompt_tokens`, `llm_completion_tokens`, and `llm_active_connections`.
- Read expiration from each of `plugin_attr.prometheus.metrics.http_status.expire`, `http_latency.expire`, `bandwidth.expire`, `llm_latency.expire`, `llm_prompt_tokens.expire`, `llm_completion_tokens.expire`, and `llm_active_connections.expire` as non-negative whole seconds; missing or zero disables expiration.
- Keep `max_http_series`; add `max_llm_series` with default `10000`, minimum `100`, and maximum `100000`, applied independently per family.
- The dynamic index must never exceed its configured family limit; at most one all-label `__overflow__` child may exist beyond it.
- An expiration must delete the vector child and authoritative tracker entry in the same write-side critical section.
- Existing-series writes must remain concurrent and cannot race deletion into creating detached or untracked children.
- Never expire an `llm_active_connections` child while its in-flight count is non-zero.
- Run at most one expiration goroutine, start none when every expiration is zero, and stop and await it on shutdown and startup failure.
- Delete no more than 256 entries per family per scan.
- Do not add dependencies or change exporter address, URI, scrape handling, proxy/logger lifecycle metrics, or Prometheus caching.
- Run every Go command as `bash -lc 'source .envrc && ...'` from the isolated worktree.

---

## File Map

- Create `pkg/observability/metrics/series_tracker.go`: bounded admission, concurrent update/delete protocol, in-flight pins, and batch expiration.
- Create `pkg/observability/metrics/series_tracker_test.go`: tracker behavior, vector deletion, overflow bound, stale-candidate protection, active pins, and races.
- Create `pkg/observability/metrics/expiration_runtime.go`: one context-owned ticker goroutine over enabled trackers.
- Create `pkg/observability/metrics/expiration_runtime_test.go`: interval, start/stop, duplicate-start, and scan tests.
- Modify `pkg/observability/metrics/prometheus.go`: configuration parsing, seven tracker instances, HTTP/LLM write-path integration, and runtime entry point.
- Modify `pkg/observability/metrics/prometheus_test.go`: official configuration, LLM limit, request recording, and active-gauge regression coverage.
- Delete `pkg/observability/metrics/http_series_budget.go`: superseded HTTP-only budget implementation.
- Modify `pkg/observability/metrics/http_series_budget_test.go`, then rename it to `series_integration_test.go`: retain request-level budget/overflow assertions against the new tracker API.
- Modify `pkg/server/server.go`: own and stop the metrics expiration runtime.
- Modify `pkg/server/server_test.go`: shutdown and startup-failure lifecycle assertions.
- Modify `conf/config-default.yaml`: put `expire` under the seven official metric entries and document `max_llm_series`.
- Modify `docs/configuration.md`: document exact TTL, capacity, overflow, reset, restart, and cleanup-delay semantics.

### Task 1: Parse the Official Expiration and LLM Capacity Configuration

**Files:**
- Modify: `pkg/observability/metrics/prometheus.go:611-750`
- Modify: `pkg/observability/metrics/http_series_budget.go:11-16`
- Test: `pkg/observability/metrics/prometheus_test.go:49-176`
- Test: `pkg/observability/metrics/http_series_budget_test.go:142-183`

**Interfaces:**
- Produces: `prometheusMetricConfig.MaxLLMSeries int`
- Produces: `prometheusMetricConfig.Expires map[string]time.Duration`
- Produces: `parseSeriesLimit(raw any, fieldName string) (int, error)`
- Produces: `parseMetricExpires(raw any) (map[string]time.Duration, error)`
- Consumes: the existing seven metric-name constants and `plugin_attr.prometheus` map.

- [ ] **Step 1: Add failing table tests for `max_llm_series`**

Add cases parallel to the existing HTTP limit test, including default, minimum, maximum, integer-width input, string, boolean, fractional, below-minimum, and above-maximum values:

```go
func TestPrometheusMetricConfigLLMSeriesLimit(t *testing.T) {
	tests := []struct {
		name    string
		raw     any
		want    int
		wantErr bool
	}{
		{name: "default", want: defaultMaxMetricSeries},
		{name: "minimum", raw: minMetricSeries, want: minMetricSeries},
		{name: "maximum", raw: maxMetricSeries, want: maxMetricSeries},
		{name: "int64", raw: int64(250), want: 250},
		{name: "invalid string", raw: "1000", wantErr: true},
		{name: "invalid bool", raw: true, wantErr: true},
		{name: "invalid fractional", raw: 100.5, wantErr: true},
		{name: "below minimum", raw: minMetricSeries - 1, wantErr: true},
		{name: "above maximum", raw: maxMetricSeries + 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attr := map[string]any{}
			if test.raw != nil {
				attr["max_llm_series"] = test.raw
			}
			cfg, err := newPrometheusMetricConfig(attr)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "plugin_attr.prometheus.max_llm_series") {
					t.Fatalf("error = %v, want max_llm_series validation error", err)
				}
				return
			}
			if err != nil || cfg.MaxLLMSeries != test.want {
				t.Fatalf("config = %#v, error = %v, want MaxLLMSeries %d", cfg, err, test.want)
			}
		})
	}
}
```

- [ ] **Step 2: Add failing expiration parsing tests**

Cover all seven names in one valid table and invalid values separately:

```go
func TestPrometheusMetricConfigParsesMetricExpires(t *testing.T) {
	metricsConfig := map[string]any{}
	for index, name := range expirableMetricNames {
		metricsConfig[name] = map[string]any{"expire": index + 1}
	}
	cfg, err := newPrometheusMetricConfig(map[string]any{"metrics": metricsConfig})
	if err != nil {
		t.Fatalf("newPrometheusMetricConfig() error = %v", err)
	}
	for index, name := range expirableMetricNames {
		want := time.Duration(index+1) * time.Second
		if got := cfg.Expires[name]; got != want {
			t.Fatalf("Expires[%q] = %s, want %s", name, got, want)
		}
	}
}
```

Add cases proving omitted and zero values produce zero durations, while `-1`, `1.5`, `"60"`, `true`, and `uint64(math.MaxUint64)` return an error containing the concrete tested path, for example `plugin_attr.prometheus.metrics.http_status.expire`.

- [ ] **Step 3: Run the focused tests and observe the intended failure**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "TestPrometheusMetricConfig(LLMSeriesLimit|ParsesMetricExpires|RejectsInvalidMetricExpire)$" -count=1'
```

Expected: build or assertion failure because `MaxLLMSeries`, `Expires`, and the parser do not exist.

- [ ] **Step 4: Implement the shared limit and expiration parsers**

Move the HTTP-named bounds from `http_series_budget.go` into `prometheus.go` under shared metric names, update existing HTTP budget tests to use them, and use one strict integer parser for both capacity fields. Extend the config as follows:

```go
const (
	defaultMaxMetricSeries = 10000
	minMetricSeries        = 100
	maxMetricSeries        = 100000
)

var expirableMetricNames = []string{
	httpStatusMetric,
	httpLatencyMetric,
	bandwidthMetric,
	llmLatencyMetric,
	llmPromptMetric,
	llmCompleteMetric,
	llmActiveMetric,
}

type prometheusMetricConfig struct {
	MetricPrefix  string
	Buckets       []float64
	LLMBuckets    []float64
	ExtraLabels   map[string][]prometheusExtraLabel
	MaxHTTPSeries int
	MaxLLMSeries  int
	Expires       map[string]time.Duration
}
```

`parseMetricExpires` must iterate only `expirableMetricNames`, ignore missing family maps and missing `expire`, convert accepted signed/unsigned integer types to seconds without overflow, and reject every other explicit type. Reuse the parsed `metrics` map for `parseExtraLabels`; do not change extra-label behavior.

- [ ] **Step 5: Run the focused and package tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "TestPrometheusMetricConfig" -count=1'
bash -lc 'source .envrc && go test ./pkg/observability/metrics -count=1'
```

Expected: PASS.

- [ ] **Step 6: Commit the configuration contract**

```bash
git add pkg/observability/metrics/prometheus.go pkg/observability/metrics/prometheus_test.go pkg/observability/metrics/http_series_budget.go pkg/observability/metrics/http_series_budget_test.go
git commit -m "feat(metrics): parse series expiration settings"
```

### Task 2: Build the Bounded Concurrent Series Tracker

**Files:**
- Create: `pkg/observability/metrics/series_tracker.go`
- Create: `pkg/observability/metrics/series_tracker_test.go`

**Interfaces:**
- Consumes: ordered label tuples, a family limit, TTL, overflow counter, and the vector's `DeleteLabelValues` method.
- Produces: `newMetricSeriesTracker(limit int, labelCount int, expire time.Duration, overflow prometheus.Counter, deleteLabels func(...string) bool) *metricSeriesTracker`
- Produces: `(*metricSeriesTracker).withSeries(labels []string, update func([]string))`
- Produces: `(*metricSeriesTracker).acquireSeries(labels []string, increment func([]string), decrement func([]string)) func()`
- Produces: `(*metricSeriesTracker).expireSeries(now time.Time, maxDeletes int) int`
- Produces: private `expiredCandidates(now time.Time, max int) []metricSeriesCandidate` and `deleteExpired(candidates []metricSeriesCandidate, now time.Time) int` stages used by `expireSeries` and stale-candidate tests.
- Produces: `(*metricSeriesTracker).entryCount() int` for package tests.

- [ ] **Step 1: Write failing admission, reuse, and overflow tests**

Use a `CounterVec` registered in a private registry. Assert two exact tuples fit a limit of two, an existing tuple still updates when full, and an unseen third tuple updates only the all-`__overflow__` labels without increasing `entryCount()`:

```go
tracker.withSeries([]string{"route-a", "200"}, func(labels []string) {
	vector.WithLabelValues(labels...).Inc()
})
tracker.withSeries([]string{"route-b", "201"}, func(labels []string) {
	vector.WithLabelValues(labels...).Inc()
})
tracker.withSeries([]string{"route-c", "202"}, func(labels []string) {
	vector.WithLabelValues(labels...).Inc()
})
if got := tracker.entryCount(); got != 2 {
	t.Fatalf("entryCount() = %d, want 2", got)
}
```

Gather the registry and prove exactly three vector children exist: two exact and one synthetic overflow child.

- [ ] **Step 2: Write failing expiration and released-capacity tests**

Inject `tracker.now` with a mutable test clock. Record tuple A at `t0`, move the clock past TTL, call `expireSeries`, and assert:

- `DeleteLabelValues` removed A from registry gathering;
- `entryCount()` returned to zero;
- tuple B is admitted exactly rather than sent to overflow;
- a later recreation of A starts its counter at one.

- [ ] **Step 3: Write failing stale-candidate and disabled-expiration tests**

Call `expiredCandidates`, refresh the candidate through `withSeries`, then call `deleteExpired` with the stale candidate and prove the write-lock recheck preserves the series. A tracker with zero TTL must return zero deletions without invoking its delete callback.

- [ ] **Step 4: Write failing active-pin tests**

Use `acquireSeries` with a `GaugeVec`. Move time beyond TTL while the release closure is outstanding and assert zero deletions and gauge value one. Release, move another TTL beyond the release time, expire, and assert the child is absent.

- [ ] **Step 5: Write the concurrent safety test**

Run goroutines that repeatedly update known tuples, attempt new admissions, acquire/release active gauges, and call `expireSeries`. Assert the final index never exceeds its limit and each gathered exact child has a tracker entry unless it is the one synthetic overflow tuple.

- [ ] **Step 6: Run tracker tests and observe the intended failure**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "TestMetricSeriesTracker" -count=1'
```

Expected: build failure because the tracker implementation does not exist.

- [ ] **Step 7: Implement the minimal tracker**

Use immutable label copies and atomic timestamps/reference counts:

```go
type metricSeriesEntry struct {
	labels   []string
	lastSeen atomic.Int64
	inFlight atomic.Int64
}

type metricSeriesTracker struct {
	mu                sync.RWMutex
	limit             int
	expire            time.Duration
	entries           map[string]*metricSeriesEntry
	overflowLabels    []string
	overflowCounter   prometheus.Counter
	deleteLabelValues func(...string) bool
	now               func() time.Time
}
```

Existing-entry updates hold `RLock` through the vector callback. New admission double-checks under `Lock` and performs the first callback before unlock. Full trackers increment the overflow counter and invoke the callback with the single immutable overflow tuple. `expireSeries` discovers candidates under `RLock`, obtains `Lock`, rechecks the candidate's captured `lastSeen`, current `lastSeen`, `inFlight`, and the deadline, then calls `DeleteLabelValues` and removes the entry.

Use the existing length-prefixed tuple-key algorithm under the generalized name `metricSeriesTupleKey`.

- [ ] **Step 8: Run focused tests and the race detector**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "TestMetricSeriesTracker" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/observability/metrics -run "TestMetricSeriesTracker" -count=1'
```

Expected: PASS with no race report.

- [ ] **Step 9: Commit the tracker**

```bash
git add pkg/observability/metrics/series_tracker.go pkg/observability/metrics/series_tracker_test.go
git commit -m "feat(metrics): add bounded series tracker"
```

### Task 3: Route All Seven Metric Families Through the Tracker

**Files:**
- Modify: `pkg/observability/metrics/prometheus.go:31-492`
- Delete: `pkg/observability/metrics/http_series_budget.go`
- Modify and rename: `pkg/observability/metrics/http_series_budget_test.go` to `pkg/observability/metrics/series_integration_test.go`
- Modify: `pkg/observability/metrics/prometheus_test.go:66-132,342-506`

**Interfaces:**
- Consumes: `newMetricSeriesTracker`, `withSeries`, and `acquireSeries` from Task 2.
- Produces: seven package-global tracker pointers initialized alongside their vectors.
- Produces: `llmMetricSeriesOverflow *prometheus.CounterVec` registered with the default registry.
- Preserves: `RecordHTTPRequest`, `BeginLLMRequest`, and all public metric variable signatures.

- [ ] **Step 1: Rewrite request-level tests to demand canonical bounded overflow**

Replace assertions against `httpSeriesBudget.seen` with each tracker's `entryCount()`. For a limit of one, record two distinct requests and gather each vector. Assert the second request reaches all-`__overflow__` labels and each tracker still has one exact entry.

Add LLM cases for `llm_latency`, prompt tokens, completion tokens, and active connections, asserting each independently enforces `max_llm_series` and increments:

```text
apisix_llm_metric_series_overflow_total{metric="llm_latency"}
```

Repeat the assertion with the concrete `llm_prompt_tokens`, `llm_completion_tokens`, and `llm_active_connections` metric label values.

- [ ] **Step 2: Add a failing active-connection expiration integration test**

Install a private `LLMActiveConnections` vector and tracker with an injected clock. Call `BeginLLMRequest`, advance beyond TTL, scan, and gather value one. Call the returned release closure, advance beyond TTL again, scan, and assert the child is absent.

- [ ] **Step 3: Add failing HTTP and LLM delete/re-admit integration tests**

For one HTTP counter and one LLM counter, record tuple A, expire it, then record tuple B. Assert B retains its exact labels and neither family increments its overflow counter.

- [ ] **Step 4: Run the focused tests and observe the intended failure**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "Test(RecordHTTPRequest|RecordLLM|BeginLLM|MetricFamilyExpiration)" -count=1'
```

Expected: failures because production writes still bypass the new trackers.

- [ ] **Step 5: Initialize the seven trackers with vector-specific delete callbacks**

After creating vectors and overflow counters in `initMetrics`, construct:

```go
httpStatusSeries = newMetricSeriesTracker(
	metricConfig.MaxHTTPSeries,
	len(httpStatusLabels),
	metricConfig.Expires[httpStatusMetric],
	httpSeriesOverflow.WithLabelValues(httpStatusMetric),
	HttpStatus.DeleteLabelValues,
)
```

Repeat with the correct label count, TTL, overflow counter, and delete method for the remaining six families. In `initMetrics`, assign each `metricLabelNames(...)` result to a local such as `httpStatusLabels`, pass it both to the vector constructor and `len(...)` for its tracker, and do not introduce a registry abstraction.

- [ ] **Step 6: Move HTTP writes into tracker callbacks**

Replace `admitHTTPMetricLabels` plus a later vector update with one coordinated call:

```go
httpStatusSeries.withSeries(statusLabels, func(labels []string) {
	HttpStatus.WithLabelValues(labels...).Inc()
})
```

Apply the same pattern to every request/upstream/APISIX latency observation and ingress/egress bandwidth update. The tracker callback must contain the complete `WithLabelValues(...).Inc/Add/Observe` operation.

- [ ] **Step 7: Move LLM writes and active gauges into trackers**

Wrap the three request-completion LLM observations with their family trackers. Change `BeginLLMRequest` to return the release function created by:

```go
return llmActiveSeries.acquireSeries(
	labels,
	func(actual []string) { LLMActiveConnections.WithLabelValues(actual...).Inc() },
	func(actual []string) { LLMActiveConnections.WithLabelValues(actual...).Dec() },
)
```

Keep the current nil-vector no-op behavior.

- [ ] **Step 8: Remove the old budget and update test helpers**

Delete `http_series_budget.go`, rename the request-level test file, and replace every old symbol:

```bash
rg -n 'httpSeriesBudget|newHTTPSeriesBudget|admitHTTPMetricLabels|\.seen\b|httpSeriesTupleKey' pkg/observability/metrics
```

Expected after edits: no matches.

- [ ] **Step 9: Run package tests and the race detector**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -count=1'
bash -lc 'source .envrc && go test -race ./pkg/observability/metrics -count=1'
```

Expected: PASS with no race report.

- [ ] **Step 10: Commit family integration**

```bash
git add pkg/observability/metrics
git commit -m "feat(metrics): expire bounded HTTP and LLM series"
```

### Task 4: Add and Own the Independent Expiration Goroutine

**Files:**
- Create: `pkg/observability/metrics/expiration_runtime.go`
- Create: `pkg/observability/metrics/expiration_runtime_test.go`
- Modify: `pkg/observability/metrics/prometheus.go:31-125,300-352`
- Modify: `pkg/server/server.go:113-175,401-503,537-745`
- Test: `pkg/server/server_test.go:33-320`

**Interfaces:**
- Consumes: the seven initialized trackers and `expireSeries(now, 256)`.
- Produces: `StartExpiration(parent context.Context) (func(context.Context) error, error)`.
- Produces: server field `stopPrometheusExpiration func(context.Context) error`.

- [ ] **Step 1: Write failing runtime interval and scan tests**

Construct trackers with zero, 1-second, 10-second, and 5-minute expirations. Assert the runtime selects 1 second for the first positive set, 5 seconds for a 10-second minimum, and caps a 5-minute-only configuration at 1 minute. Assert a manual `scan(now)` calls only trackers with positive TTL and never passes a batch larger than 256.

- [ ] **Step 2: Write failing runtime lifecycle tests**

Use a short test interval or injectable ticker. Assert:

- all-zero TTL returns `nil, nil` and starts no goroutine;
- cancellation closes the runtime `done` channel;
- the returned stop function is idempotent and waits for `done`;
- a second `Start` while running returns `errExpirationRuntimeRunning`;
- after the first loop has exited, a later start succeeds.

- [ ] **Step 3: Run runtime tests and observe the intended failure**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "TestExpirationRuntime" -count=1'
```

Expected: build failure because the runtime does not exist.

- [ ] **Step 4: Implement the runtime**

Use a small internal ticker interface so tests do not sleep:

```go
type expirationTicker interface {
	Chan() <-chan time.Time
	Stop()
}

type expirationRuntime struct {
	mu        sync.Mutex
	trackers  []*metricSeriesTracker
	interval  time.Duration
	running   bool
	newTicker func(time.Duration) expirationTicker
}
```

`Start` creates a child context, sets `running` before launching exactly one goroutine, and returns an idempotent closure that cancels and selects between `done` and the supplied shutdown context. The loop scans on ticks only. Its defer resets `running`, stops the ticker, and closes `done`.

- [ ] **Step 5: Add failing server ownership tests**

Add a fake stop callback to a minimally constructed `Server`. Assert `shutdownAttempt` invokes it once and clears it only after success. If its wait context expires, assert `shutdownAttempt` returns `complete == false` so a later shutdown attempt can finish waiting. Add a startup-failure case proving the callback runs when a later listener start fails.

- [ ] **Step 6: Run server tests and observe the intended failure**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/server -run "Test(ServerShutdownStopsPrometheusExpiration|StartFailureStopsPrometheusExpiration)" -count=1'
```

Expected: failure because `Server` does not own the stop callback.

- [ ] **Step 7: Wire runtime startup and shutdown into `Server`**

After `metrics.Init()` in `Server.Start`, call `metrics.StartExpiration(ctx)`. Store a non-nil stop callback under `lifecycleMu` only if shutdown has not already been requested; otherwise stop it immediately and return `context.Canceled`.

During `shutdownAttempt`, stop and await expiration after the HTTP server has quiesced and before producer, route, cluster, and store teardown. If waiting returns an error, return `complete == false` and retain the callback for a later shutdown attempt. Clear it after a successful stop so repeated shutdown calls cannot invoke it again.

Initialize the package-global runtime after the seven trackers in `initMetrics`; `StartExpiration` delegates to it and is harmless when every TTL is zero.

- [ ] **Step 8: Run focused, package, and race tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "TestExpirationRuntime" -count=1'
bash -lc 'source .envrc && go test ./pkg/server -run "Test(ServerShutdownStopsPrometheusExpiration|StartFailureStopsPrometheusExpiration)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/observability/metrics ./pkg/server -count=1'
```

Expected: PASS with no race report.

- [ ] **Step 9: Commit lifecycle ownership**

```bash
git add pkg/observability/metrics/expiration_runtime.go pkg/observability/metrics/expiration_runtime_test.go pkg/observability/metrics/prometheus.go pkg/server/server.go pkg/server/server_test.go
git commit -m "feat(metrics): run bounded expiration cleanup"
```

### Task 5: Document the Runtime Contract

**Files:**
- Modify: `conf/config-default.yaml:448-474`
- Modify: `docs/configuration.md:82-100`

**Interfaces:**
- Consumes: the final field names and behavior from Tasks 1-4.
- Produces: checked-in configuration examples aligned with the runtime parser.

- [ ] **Step 1: Correct the default configuration example**

Replace the incorrect top-level commented `expire` with nested examples for all seven families. Keep `extra_labels` under the same family maps. Add commented `max_http_series: 10000` and `max_llm_series: 10000` immediately before `metrics:`.

- [ ] **Step 2: Update the authoritative configuration table**

Replace the current Prometheus paragraph with exact statements that:

- both capacity settings apply independently per family and accept integers from 100 through 100000;
- the seven nested expiration settings use whole seconds and zero disables them;
- expiration is idle since the last observation, not since the last scrape;
- without an expired backlog, deletion normally occurs by the next scan; each family drains larger backlogs in 256-entry batches over later scans;
- recreated counters and histograms restart at zero;
- overflow uses one all-`__overflow__` child per family;
- configuration changes require restart.

- [ ] **Step 3: Review documentation against source names**

Run:

```bash
rg -n 'max_(http|llm)_series|metrics:|expire:|__overflow__|llm_metric_series_overflow' conf/config-default.yaml docs/configuration.md pkg/observability/metrics
git diff --check
```

Expected: every documented field and metric name has a production-code match; no whitespace errors.

- [ ] **Step 4: Commit documentation**

```bash
git add conf/config-default.yaml docs/configuration.md
git commit -m "docs(metrics): document bounded series expiration"
```

### Task 6: Refactor Audit, Review, and Final Verification

**Files:**
- Inspect: every file changed since `origin/master`
- Modify: only files with defects found in this feature's implementation.

**Interfaces:**
- Consumes: all completed tasks.
- Produces: merge-ready branch evidence without unrelated cleanup.

- [ ] **Step 1: Format touched Go files and inspect formatting-only changes**

Run:

```bash
bash -lc 'source .envrc && golangci-lint fmt'
git diff --stat
git diff --check
```

Revert only unrelated formatting introduced by the formatter; preserve every feature-owned formatting change.

- [ ] **Step 2: Perform the required deleted/renamed-symbol audit**

Run:

```bash
rg -n 'httpSeriesBudget|newHTTPSeriesBudget|newHTTPSeriesBudgetWithTail|admitHTTPMetricLabels|httpSeriesTupleKey|\.seen\b' --glob '*.go' .
rg -n 'metricSeriesTracker|StartExpiration|stopPrometheusExpiration|max_llm_series' --glob '*.go' --glob '*.md' --glob '*.yaml' .
```

Expected: no obsolete production or test call sites; every new symbol has an intended production owner or focused test. Remove no compatibility wrapper unless this feature introduced it.

- [ ] **Step 3: Review the complete diff for correctness and scope**

Inspect:

```bash
git status --short --branch
git diff --stat origin/master
git diff origin/master -- pkg/observability/metrics pkg/server conf/config-default.yaml docs/configuration.md docs/superpowers
```

Check lock ordering, callback lifetime, ticker shutdown, false `DeleteLabelValues` handling, error joining, exact label counts, hard bounds, and absence of exporter/cache changes.

- [ ] **Step 4: Run focused tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -count=1'
bash -lc 'source .envrc && go test ./pkg/server -count=1'
```

Expected: PASS.

- [ ] **Step 5: Run concurrency verification**

Run:

```bash
bash -lc 'source .envrc && go test -race ./pkg/observability/metrics ./pkg/server -count=1'
```

Expected: PASS with no race report.

- [ ] **Step 6: Run repository lint and build gates**

Run:

```bash
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build'
```

Expected: both commands exit zero.

- [ ] **Step 7: Commit any review-only corrections**

If review or verification required feature-owned corrections, stage only changed paths from this exact feature-owned set and commit:

```bash
git add pkg/observability/metrics pkg/server/server.go pkg/server/server_test.go conf/config-default.yaml docs/configuration.md
git commit -m "fix(metrics): harden series expiration lifecycle"
```

If no correction was required, do not create an empty commit.

- [ ] **Step 8: Confirm final branch scope**

Run:

```bash
git status --short --branch
git log --oneline origin/master..HEAD
git diff --check origin/master...HEAD
```

Expected: clean worktree; only the design, plan, implementation, tests, and documentation for Prometheus series expiration differ from `origin/master`.

### Task 7: Push and Open the Pull Request

**Files:**
- Inspect: committed branch and GitHub PR state only.
- Modify: no product files unless fresh CI exposes a feature-owned defect.

**Interfaces:**
- Consumes: the clean, verified `codex/prometheus-metrics-expire` branch.
- Produces: a GitHub pull request targeting `master` with exact local verification evidence.

- [ ] **Step 1: Recheck the remote base before publication**

Run:

```bash
git fetch origin --prune
git log --oneline --decorate -5 origin/master
git rev-list --left-right --count origin/master...HEAD
```

If `origin/master` advanced, rebase the feature branch onto it, inspect conflicts one at a time, and rerun Tasks 6 steps 4-6 because the tested base changed.

- [ ] **Step 2: Push the feature branch**

Run:

```bash
git push -u origin codex/prometheus-metrics-expire
```

Expected: the remote branch points at the verified local HEAD.

- [ ] **Step 3: Create the pull request**

Create a PR targeting `master` with title:

```text
feat(metrics): add bounded series expiration
```

The body must summarize the APISIX-compatible seven-family TTL contract, hard HTTP/LLM limits and canonical overflow, coordinated write/delete protocol, pinned active LLM gauges, and lifecycle-owned cleanup goroutine. Include the exact focused test, race, lint, and build commands from Task 6.

Run `gh pr create --base master --head codex/prometheus-metrics-expire` with that prepared title and body.

- [ ] **Step 4: Monitor required checks**

Run:

```bash
gh pr checks --watch --interval 20
```

If a check fails, inspect its primary log, reproduce the exact failure locally where possible, make only a feature-owned correction, rerun the impacted local gate, commit, push, and watch the new check run. Do not merge the PR unless separately requested.
