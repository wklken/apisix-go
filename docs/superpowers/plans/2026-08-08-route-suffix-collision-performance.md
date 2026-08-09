# Route Suffix Collision Performance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the O(n) exact-host scan for many embedded-wildcard routes sharing one suffix, without changing registration-order, host-rank, method, 404, or 405 semantics.

**Architecture:** First lock semantics and add a reproducible collision corpus. Keep the existing bucket slices as the registration-order source of truth, then add a derived index keyed by exact method, normalized exact host, and suffix; wildcard-host matching stays on the existing ordered scan because glob patterns cannot be resolved by an exact-value map.

**Tech Stack:** Go 1.26, `go-chi/chi`, repository benchmark runner, pinned `benchstat`.

## File Structure

- Modify `pkg/route/uri_test.go`: lock exact-host, wildcard-host, method, and registration-order behavior.
- Modify `pkg/route/benchmark_test.go` and `Makefile`: add a stable suffix-collision benchmark corpus.
- Modify `pkg/route/builder.go`: add an exact-host value index to `wildcardDispatcher` while retaining ordered wildcard-host scans.
- Create `docs/performance/route-suffix-collision.md`: record the benchmark contract, accepted evidence, or rejection result.

## Global Constraints

- Run every Go command as `bash -lc 'source .envrc && ...'` from the repository root.
- Benchmark evidence must use `scripts/benchmark.sh` through Makefile targets; direct `go test -bench` is exploratory only.
- Baseline and current must use the identical committed `pkg/route/benchmark_test.go` corpus and settings.
- Primary metric: `ns/op` for 1000-route exact-host `match-first` and `host-mismatch`; allocation metrics are guardrails.
- Acceptance threshold: both primary rows improve by at least 70%; no affected row has a statistically significant regression greater than 5%; B/op and allocs/op do not increase.
- If the threshold is not met, keep the semantic tests/benchmark but do not claim or commit the production optimization.
- Bucket slices remain authoritative for latest-registration selection.
- Use focused route tests, race test, lint, and build; do not run `go test ./...` or `make test`.
- Subagents require explicit user authorization under `AGENTS.md`; inline execution is the default.

---

### Task 1: Lock suffix/host/method semantics

**Files:**
- Modify: `pkg/route/uri_test.go`

**Interfaces:**
- Consumes: `routeRegistrar.registerRouteWithHosts(methods, pattern, hosts, handler)`.
- Produces: table-driven coverage for same suffix across exact hosts, wildcard hosts, exact/all methods, and registration order.

- [ ] **Step 1: Add an exact-host collision test**

Register `/articles/*/comments` three times for `a.example.test`, `b.example.test`, and `a.example.test` again, with distinct status codes. Assert:

```go
tests := []struct {
	method string
	host   string
	want   int
}{
	{method: http.MethodGet, host: "a.example.test", want: http.StatusAccepted}, // latest a registration
	{method: http.MethodGet, host: "b.example.test", want: http.StatusCreated},
	{method: http.MethodGet, host: "missing.example.test", want: http.StatusNotFound},
	{method: http.MethodPost, host: "a.example.test", want: http.StatusMethodNotAllowed},
}
```

Build each request as `httptest.NewRequest(method, "http://"+host+"/articles/42/comments", nil)`.

- [ ] **Step 2: Add a wildcard-host collision test**

Register the same suffix for `*.example.test` and `api.*.test`, including a later duplicate pattern. Assert the latest matching wildcard registration wins, non-matching host is 404, and wrong method is 405. This test is a semantic guard only; the production optimization will not promise O(1) glob lookup.

- [ ] **Step 3: Add mixed exact/all-method coverage**

For one exact host, register GET and all-method handlers for the same suffix in both orders. Assert GET resolves according to current bucket precedence and POST reaches the all-method handler. Reuse the status-code handler pattern already used by `TestWildcardDispatcherKeepsMethodScopesSeparate`.

- [ ] **Step 4: Run the focused semantic tests**

Run: `bash -lc 'source .envrc && go test ./pkg/route -run "SameSuffix|WildcardDispatcherKeepsMethodScopesSeparate" -count=1'`

Expected: PASS before production changes; these tests characterize and freeze current behavior.

- [ ] **Step 5: Commit the semantic tests**

```bash
git add pkg/route/uri_test.go
git commit -m "test(route): lock suffix collision semantics"
```

### Task 2: Add the formal suffix-collision benchmark corpus

**Files:**
- Modify: `pkg/route/benchmark_test.go`

**Interfaces:**
- Consumes: same route registrar and router used in Task 1.
- Produces: `BenchmarkRouteDispatchSuffixCollision` rows for 10/100/1000 routes and exact/wildcard host cases.

- [ ] **Step 1: Add exact-host benchmark rows**

Create one router per sub-benchmark with every route using `/articles/*/comments`, method GET, and host `host-%04d.example.test`. Add results:

```go
for _, result := range []string{"match-first", "match-last", "host-mismatch", "method-mismatch", "path-miss"} {
	for _, routeCount := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("host=exact/result=%s/routes=%d", result, routeCount), func(b *testing.B) {
			benchmarkRouteDispatchSuffixCollision(b, "exact", result, routeCount)
		})
	}
}
```

Use host zero for `match-first`, host `routeCount-1` for `match-last`, an absent host for `host-mismatch`, POST for `method-mismatch`, and `/articles/slug/likes` for `path-miss`.

- [ ] **Step 2: Add wildcard-host guardrail rows**

Use patterns `tenant-%04d.*` with requests such as `tenant-0000.example.test`. Add 10/100/1000 `match-first`, `match-last`, and `host-mismatch` rows under `host=wildcard`. These rows are reported and must not regress; they are not primary optimization targets.

- [ ] **Step 3: Assert status after every benchmark loop**

Expected statuses are 204 for matches, 405 for method mismatch, and 404 for host/path misses. Keep `b.ReportAllocs()` and `runtime.KeepAlive(sink)` consistent with `BenchmarkRouteDispatch`.

- [ ] **Step 4: Run the benchmark runner self-test and compile the corpus**

Run:

```bash
bash -lc 'source .envrc && make benchmark-runner-test'
bash -lc 'source .envrc && go test ./pkg/route -run "^$" -count=1'
```

Expected: PASS.

- [ ] **Step 5: Commit the immutable benchmark corpus**

```bash
git add pkg/route/benchmark_test.go
git commit -m "bench(route): add suffix collision corpus"
```

### Task 3: Record the baseline

**Files:**
- Create ignored evidence only: `.cache/bench/route-suffix-collision/baseline.txt`
- Create ignored evidence only: `.cache/bench/route-suffix-collision/baseline.meta`

**Interfaces:**
- Consumes: the committed Task 2 corpus.
- Produces: immutable baseline with route-only packages and CPU=1.

- [ ] **Step 1: Install/verify pinned benchstat**

Run: `bash -lc 'source .envrc && make init-bench'`

Expected: PASS and version output for the pinned benchstat binary.

- [ ] **Step 2: Record the baseline with isolated labels**

Run:

```bash
bash -lc 'source .envrc && BENCH_DIR=.cache/bench/route-suffix-collision BENCH_PACKAGES=./pkg/route BENCH_CORPUS_FILES=pkg/route/benchmark_test.go BENCH_REGEX="^BenchmarkRouteDispatchSuffixCollision$" BENCH_CPU=1 BENCH_TIME=1s BENCH_COUNT=10 make benchmark-baseline'
```

Expected: published `baseline` result and metadata. Do not rerun the same label; the runner must reject overwrite.

- [ ] **Step 3: Inspect all primary and guardrail rows**

Record baseline ns/op, B/op, and allocs/op for exact-host 1000-route match-first/host-mismatch plus every wildcard row. Do not rank an optimization before the current run exists.

### Task 4: Add exact method/host/suffix candidate indexing

**Files:**
- Modify: `pkg/route/builder.go`
- Test: `pkg/route/uri_test.go`

**Interfaces:**
- Consumes: existing `buckets [2][3][2][]wildcardRoute` and `embeddedSuffixes [3][2]map[string][]int`.
- Produces: `embeddedExactHostSuffixes map[string]map[string]map[string][]int`, keyed `method -> normalized host -> suffix -> bucket indexes`.

- [ ] **Step 1: Add one request-host normalization helper**

Extract the current request-host parsing from `routeHostRank`:

```go
func normalizeRequestHost(requestHost string) string {
	host := requestHost
	if parsedHost, _, err := net.SplitHostPort(requestHost); err == nil {
		host = parsedHost
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}
```

Compute it once at the start of `wildcardDispatcher.ServeHTTP` and pass the normalized value to matching helpers. Normalize registered exact patterns with lowercase plus trailing-dot trim; do not strip a configured port because current exact matching does not accept one.

- [ ] **Step 2: Add the derived exact-host index**

Add:

```go
embeddedExactHostSuffixes map[string]map[string]map[string][]int
```

When `addToBucket` receives an embedded route at host rank 2, append its bucket index once for every non-glob host under `route.method`, normalized host, and suffix. Routes with both exact and wildcard hosts remain present in both existing rank buckets; only their exact values enter this map.

- [ ] **Step 3: Use exact candidates without changing path/method status tracking**

In `matchEmbeddedRoute`, continue consulting `embeddedSuffixes[hostRank][methodIndex]` to set `pathMatched`. For host rank 2, replace the general suffix candidate list with:

```go
method := request.Method
if methodIndex == 1 {
	method = "*"
}
indexes := d.embeddedExactHostSuffixes[method][requestHost][suffix]
```

When indexes are non-empty, set `hostMatched = true`; select the greatest index above `bestIndex`. Do not call `routeHostRank` for these candidates because the index already proves exact host and method identity. Keep the existing reverse scan for host ranks 1 and 0.

- [ ] **Step 4: Run semantic and race tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "SameSuffix|EmbeddedWildcard|WildcardDispatcher|RouteDispatcher" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/route -run "SameSuffix|EmbeddedWildcard|WildcardDispatcher|RouteDispatcher" -count=1'
```

Expected: PASS.

### Task 5: Measure and accept or reject the optimization

**Files:**
- Create ignored evidence: `.cache/bench/route-suffix-collision/current.txt`
- Create ignored evidence: `.cache/bench/route-suffix-collision/current.meta`
- Create ignored evidence: `.cache/bench/route-suffix-collision/compare.txt`

**Interfaces:**
- Consumes: unchanged benchmark corpus/settings from Task 3 and production change from Task 4.
- Produces: benchstat decision against the declared 70%/5%/allocation contract.

- [ ] **Step 1: Record current results with identical settings**

Run:

```bash
bash -lc 'source .envrc && BENCH_DIR=.cache/bench/route-suffix-collision BENCH_PACKAGES=./pkg/route BENCH_CORPUS_FILES=pkg/route/benchmark_test.go BENCH_REGEX="^BenchmarkRouteDispatchSuffixCollision$" BENCH_CPU=1 BENCH_TIME=1s BENCH_COUNT=10 make benchmark-current'
```

Expected: published current result and metadata.

- [ ] **Step 2: Compare with pinned benchstat**

Run:

```bash
bash -lc 'source .envrc && BENCH_DIR=.cache/bench/route-suffix-collision BENCH_PACKAGES=./pkg/route BENCH_CORPUS_FILES=pkg/route/benchmark_test.go BENCH_REGEX="^BenchmarkRouteDispatchSuffixCollision$" BENCH_CPU=1 BENCH_TIME=1s BENCH_COUNT=10 make benchmark-compare'
```

Expected: metadata match, then benchstat output.

- [ ] **Step 3: Apply the acceptance contract**

Accept only if both exact-host 1000-route primary rows improve at least 70%, no affected row regresses significantly by more than 5%, and allocations do not increase. Report every row, including wildcard guardrails. If rejected, do not commit `pkg/route/builder.go`; retain Tasks 1-3 as useful test/benchmark coverage and report the failed threshold.

- [ ] **Step 4: Commit accepted production code**

Only after acceptance:

```bash
git add pkg/route/builder.go pkg/route/uri_test.go
git commit -m "perf(route): index exact-host suffix collisions"
```

### Task 6: Final verification

**Files:**
- Verify only; no planned file changes.

**Interfaces:**
- Consumes: accepted Task 4 change or test/benchmark-only result if rejected.
- Produces: correctness and repository-gate evidence.

- [ ] **Step 1: Run route tests and race gate**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/route -count=1'
bash -lc 'source .envrc && go test -race ./pkg/route -count=1'
```

Expected: PASS.

- [ ] **Step 2: Run lint, build, and diff checks**

Run:

```bash
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build'
git diff --check
```

Expected: all PASS. Run `bash -lc 'source .envrc && make clean'` afterward when the binary is no longer needed.
