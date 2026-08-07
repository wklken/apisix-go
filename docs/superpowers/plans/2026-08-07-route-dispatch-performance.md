# Route Dispatcher Performance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. This plan does not authorize subagents. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the measured quadratic static-route registration cost and route-count-dependent embedded-wildcard lookup while preserving APISIX route precedence, host/method selection, and 404/405 behavior.

**Architecture:** Introduce a build-local `routeRegistrar` that owns one chi mux and a `map[convertedPattern]*wildcardDispatcher`, so registration never reconstructs the chi route tree. Keep the existing precedence buckets, but add a per-bucket embedded-suffix index that enumerates suffixes from the request path and selects the latest matching route by its existing bucket position. This changes lookup complexity without changing externally observable routing rules.

**Tech Stack:** Go 1.26.4, `go-chi/chi/v5`, Go `testing` benchmarks, repository `scripts/benchmark.sh`, pinned `benchstat`, `pprof`.

## Global Constraints

- Base branch: `origin/master` at `bea4ea8bc6cd5b7e928033e3e3ad1459670d3229` or a descendant fetched before execution.
- Feature branch: `codex/optimize-route-dispatch`.
- Worktree: `.worktrees/optimize-route-dispatch-codex-20260807`.
- Preserve latest-registration precedence within each existing embedded/host/method bucket.
- Preserve embedded routes before catch-all routes, exact host before wildcard host before hostless, exact method before all-method, and existing 404/405 decisions.
- Add no dependencies and do not modify `go.mod`, `go.sum`, or vendor.
- Use `bash -lc 'source .envrc && ...'` for every Go, benchmark, lint, and build command.
- Keep the plan and corrected benchmark report in the PR because the user explicitly requested both planning and report remediation.
- Keep all production and test changes uncommitted until staged-diff review is ready to commit.

---

## Files Map

- Modify `pkg/route/builder.go`: own dispatcher registration state and indexed embedded-suffix lookup.
- Modify `pkg/route/uri_test.go`: lock overlapping suffix precedence, host/method behavior, and 404/405 outcomes through real HTTP dispatch.
- Modify `pkg/route/benchmark_test.go`: use the production registrar and measure `match-first`, `match-last`, and `miss` separately.
- Add `docs/superpowers/benchmark-analysis-report-20260807.md`: carry in and correct the reviewed local report with verified current measurements and bounded claims.
- Add `docs/superpowers/plans/2026-08-07-route-dispatch-performance.md`: this executable plan.

### Task 1: Lock Route Semantics And Establish A Comparable Corpus

**Files:**

- Modify: `pkg/route/uri_test.go`
- Modify: `pkg/route/benchmark_test.go`
- Modify: `pkg/route/builder.go:119-254`

**Interfaces:**

- Produces: `newRouteRegistrar(mux *chi.Mux) *routeRegistrar`.
- Produces: `(*routeRegistrar).registerRouteWithHosts(methods []string, uri string, hosts []string, handler http.Handler) error`.
- Baseline implementation must still call `existingWildcardDispatcher`; the dispatcher map optimization belongs to Task 2.

- [x] **Step 1: Add a failing real-dispatch test for the desired registrar API**

Add `TestEmbeddedWildcardOverlappingSuffixUsesLatestRegistration` with two registration orders for `/articles/*/comments` and `/articles/*/v1/comments`. Dispatch `GET /articles/tenant/v1/comments` and assert the handler registered later wins in both orders. Construct one registrar per chi mux:

```go
router := chi.NewRouter()
registrar := newRouteRegistrar(router)
if err := registrar.registerRouteWithHosts([]string{http.MethodGet}, pattern, nil, handler); err != nil {
	t.Fatalf("register %s: %v", pattern, err)
}
```

The production mutation this catches is choosing suffix specificity instead of the existing latest-registration rule.

- [x] **Step 2: Verify RED**

Run:

```bash
bash -lc "source .envrc && go test ./pkg/route -run '^TestEmbeddedWildcardOverlappingSuffixUsesLatestRegistration$' -count=1"
```

Expected: compile failure naming undefined `newRouteRegistrar`.

- [x] **Step 3: Introduce the registrar without optimizing lookup**

Add:

```go
type routeRegistrar struct {
	mux *chi.Mux
}

func newRouteRegistrar(mux *chi.Mux) *routeRegistrar {
	return &routeRegistrar{mux: mux}
}
```

Move `registerRouteWithHosts` and `registerWildcardRoute` onto `routeRegistrar`, but keep `registerWildcardRoute` using `existingWildcardDispatcher(r.mux, converted)`. Create one registrar in `Builder.Build`, in each route test mux setup, and in each benchmark mux setup.

- [x] **Step 4: Verify GREEN and existing routing semantics**

Run:

```bash
bash -lc "source .envrc && go test ./pkg/route -run '^(TestConvertURI|TestRegisterRoute|TestEmbeddedWildcard|TestWildcardDispatcher|TestRouteDispatcher)' -count=1"
```

Expected: PASS, including both overlapping-suffix registration orders.

- [x] **Step 5: Expand dispatch benchmark positions without changing production behavior**

Change `BenchmarkRouteDispatch` results from `match`/`miss` to `match-first`/`match-last`/`miss`. For both route kinds, use route index `0` for `match-first` and `routeCount-1` for `match-last`. Keep the existing literal miss path.

- [x] **Step 6: Install the pinned comparison tool and record the immutable baseline**

Run:

```bash
bash -lc 'source .envrc && make init-bench'
bash -lc 'source .envrc && BENCH_DIR=.cache/bench/route-dispatch-formatted-corpus BENCH_PACKAGES=./pkg/route BENCH_CORPUS_FILES=pkg/route/benchmark_test.go BENCH_REGEX="^Benchmark(RegisterRoutes|RouteDispatch)/" BENCH_TIME=250ms BENCH_COUNT=6 BENCH_CPU=1 BENCH_P=1 bash scripts/benchmark.sh run baseline-warm'
```

Expected baseline failures against the practical thresholds: static registration at 1000 routes remains approximately 1 second and 755 MiB/op; embedded `match-first` and `miss` remain approximately 14 microseconds at 1000 routes.

### Task 2: Eliminate Quadratic Dispatcher Discovery

**Files:**

- Modify: `pkg/route/builder.go:119-254`
- Test: `pkg/route/uri_test.go`
- Benchmark: `pkg/route/benchmark_test.go`

**Interfaces:**

- Extends: `routeRegistrar` with `dispatchers map[string]*wildcardDispatcher`.
- Removes: `existingWildcardDispatcher` after all production and test call sites use one registrar per mux.

- [x] **Step 1: Make the baseline performance failure explicit**

Inspect the baseline row `BenchmarkRegisterRoutes/kind=static/routes=1000` and record its `ns/op`, `B/op`, and `allocs/op`. The declared practical threshold is at most 10 ms/op and 5 MiB/op at 1000 static routes, with identical route behavior.

- [x] **Step 2: Implement build-local dispatcher ownership**

Change the constructor to initialize the map:

```go
func newRouteRegistrar(mux *chi.Mux) *routeRegistrar {
	return &routeRegistrar{
		mux:         mux,
		dispatchers: make(map[string]*wildcardDispatcher),
	}
}
```

In `registerWildcardRoute`, obtain the dispatcher from `r.dispatchers[converted]`; on a miss create it, register it once with `r.mux.Handle`, and store it. Delete `existingWildcardDispatcher` and its `mux.Routes()` traversal.

- [x] **Step 3: Run focused correctness and exploratory performance checks**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/route -count=1'
bash -lc "source .envrc && go test ./pkg/route -run '^$' -bench '^BenchmarkRegisterRoutes/kind=static/routes=1000$' -benchmem -benchtime=250ms -count=3 -cpu=1"
```

Expected: route tests PASS; static registration is below both declared thresholds.

### Task 3: Index Embedded Wildcard Suffixes Without Changing Precedence

**Files:**

- Modify: `pkg/route/builder.go:153-328`
- Test: `pkg/route/uri_test.go`
- Benchmark: `pkg/route/benchmark_test.go`

**Interfaces:**

- Adds: `embeddedSuffixes [3][2]map[string][]int` to `wildcardDispatcher`, keyed by host-rank and method bucket.
- Adds: an internal matcher returning the highest existing bucket index whose suffix, host rank, and method match.
- Keeps: the existing route slices as the source of latest-registration order.

- [x] **Step 1: Record the request-path performance failures**

From the Task 1 baseline, record embedded-wildcard `match-first`, `match-last`, and `miss` at 1000 routes. The practical thresholds are at most 2 microseconds for `match-first` and `miss`, no more than 20% regression for `match-last`, and no increase in `B/op` or `allocs/op`.

- [x] **Step 2: Index embedded routes as they enter each existing bucket**

Keep each bucket slice unchanged. When `route.embedded` is true, derive the literal suffix after `*`, lazily initialize that bucket's map, and append the route's bucket index:

```go
suffix := route.pattern[strings.IndexByte(route.pattern, '*')+1:]
index[suffix] = append(index[suffix], len(bucket)-1)
```

Use one helper for hostless, exact-host, and wildcard-host insertion so routes added to two host buckets receive the correct local index in each bucket.

- [x] **Step 3: Match only suffixes present in the request path**

For embedded buckets, enumerate slash-starting suffixes from the request path using string slices and `strings.IndexByte`. Look up only those suffixes in the bucket map. Reject empty wildcard spans using `len(path) > len(prefix)+len(suffix)`, then scan each candidate index list backward and select the largest bucket index that also satisfies the current host rank and method bucket.

Return separate `pathMatched` and `hostMatched` flags so the surrounding dispatcher preserves existing 404 versus 405 behavior. Keep the current backward slice scan for non-embedded catch-all and static routes.

- [x] **Step 4: Verify all routing semantics**

Run:

```bash
bash -lc "source .envrc && go test ./pkg/route -run '^(TestRegisterRoute|TestEmbeddedWildcard|TestWildcardDispatcher|TestRouteDispatcher)' -count=1"
bash -lc 'source .envrc && go test ./pkg/route -count=1'
```

Expected: PASS with latest-registration, host rank, method precedence, exact sibling, catch-all, 404, and 405 behavior unchanged.

- [x] **Step 5: Record current results and compare with the identical corpus**

Run:

```bash
bash -lc 'source .envrc && BENCH_DIR=.cache/bench/route-dispatch-formatted-corpus BENCH_PACKAGES=./pkg/route BENCH_CORPUS_FILES=pkg/route/benchmark_test.go BENCH_REGEX="^Benchmark(RegisterRoutes|RouteDispatch)/" BENCH_TIME=250ms BENCH_COUNT=6 BENCH_CPU=1 BENCH_P=1 bash scripts/benchmark.sh run current-latest'
bash -lc 'source .envrc && BENCH_DIR=.cache/bench/route-dispatch-formatted-corpus BENCH_PACKAGES=./pkg/route BENCH_CORPUS_FILES=pkg/route/benchmark_test.go BENCH_REGEX="^Benchmark(RegisterRoutes|RouteDispatch)/" BENCH_TIME=250ms BENCH_COUNT=6 BENCH_CPU=1 BENCH_P=1 bash scripts/benchmark.sh compare baseline-warm current-latest'
```

Expected: metadata comparison accepted; all declared thresholds satisfied; all route rows and any regressions reported.

### Task 4: Correct The Benchmark Report With Verified Results

**Files:**

- Add: `docs/superpowers/benchmark-analysis-report-20260807.md`

**Interfaces:**

- Consumes: exact baseline/current `benchstat` output and current pprof evidence.
- Produces: a report that separates measured registration cost, conditional request-path cost, and unconfirmed JSON hypotheses.

- [x] **Step 1: Carry in the reviewed local report**

Add the user-provided report to the feature worktree without changing unrelated documents.

- [x] **Step 2: Correct the four verified review findings**

Make these exact semantic corrections:

```text
- Embedded wildcard match is not generally constant: match-last is best-case; match-first was linear before indexing.
- Decoder+UseNumber 1.255 MiB/op and UnmarshalDynamic 119 allocs/op are distinct rows and do not prove one generic data-plane bottleneck.
- The JSON profile command targets BenchmarkJSONDecoder/size=256KiB.
- 1.09 seconds is isolated registration cost, not measured end-to-end etcd reload latency; 10k latency remains an extrapolation.
```

Replace the proposed `map[suffix] -> O(1)` claim with the implemented bound: O(request path segments + matching candidates), preserving precedence.

- [x] **Step 3: Add verified before/after route results**

Report the threshold-defining 1000-route registration and dispatch rows, including exact `ns/op`, `B/op`, and `allocs/op` deltas, and whether each threshold passed. Keep the full all-row comparison in the ignored benchmark artifacts. Do not describe an optimization as verified unless the runner comparison accepted identical metadata and corpus.

- [x] **Step 4: Review the report and diff**

Run:

```bash
git diff --check
git diff -- docs/superpowers/benchmark-analysis-report-20260807.md
```

Expected: no whitespace errors; every performance statement maps to the recorded benchmark or profile evidence.

### Task 5: Refactor Audit, Staged Review, Verification, And PR Delivery

**Files:**

- Review all task-owned files listed in the Files Map.

- [x] **Step 1: Run the refactor audit**

List changed functions/types/imports, then run production and test call-site searches:

```bash
rg -n 'registerRoute\(|registerRouteWithHosts\(|existingWildcardDispatcher|newRouteRegistrar|routeRegistrar|embeddedSuffixes' pkg/route
```

Delete dead free-function wrappers, proxy-only compatibility paths, unused helpers, and stale tests unless a documented production boundary requires them.

- [x] **Step 2: Stage only task-owned files and run `fast-review-local`**

Stage exactly:

```text
pkg/route/builder.go
pkg/route/uri_test.go
pkg/route/benchmark_test.go
docs/superpowers/benchmark-analysis-report-20260807.md
docs/superpowers/plans/2026-08-07-route-dispatch-performance.md
```

Review the staged diff against `origin/master`. If findings are reported, use `fast-review-remediation`, restage only confirmed repairs, and repeat until the verdict is `ready to commit`.

- [ ] **Step 3: Run the full repository verification gate**

Run from the task worktree:

```bash
bash -lc 'source .envrc && go test ./... -count=1 -p=1'
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build'
bash -lc 'source .envrc && BENCH_DIR=.cache/bench/final-smoke make benchmark-smoke'
git diff --check
git diff --check --cached
```

Remove only the ignored `./apisix` build artifact after verification. Preserve benchmark artifacts under ignored `.cache/`.

- [ ] **Step 4: Record the pre-push tree and intended scope**

Record `git rev-parse HEAD^{tree}`, `git status --short`, the staged file set, every verification command with exit code, and the final staged diff. Any file-content change invalidates affected evidence.

- [ ] **Step 5: Commit, push, and open the PR**

Create a scoped conventional commit such as:

```text
perf(route): index dispatcher registration and matching
```

Push `codex/optimize-route-dispatch`, verify the remote head equals the local commit, and open one ready PR against `master`. The PR body must include summary, exact verification commands/results, benchmark deltas and thresholds, staged-review verdict, and residual risks.
