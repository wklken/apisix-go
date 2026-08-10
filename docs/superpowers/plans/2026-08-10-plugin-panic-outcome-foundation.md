# Plugin Panic and Request Outcome Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a request-owned lifecycle and response-outcome boundary so every HTTP request runs registered finalizers exactly once and panics produce a stable pre-commit 500 or an aborted committed connection without leaking panic details.

**Architecture:** `routeHandler` creates one `ctx.RequestLifecycle` before entering a route generation and wraps the original writer with `httpsnoop.Wrap`, which preserves the exact optional interfaces of the underlying writer while recording final status, bytes, flush and successful hijack. Inner plugins register closures that capture their current request value; the outer handler runs them in reverse registration order with per-hook recovery on success, early return, or panic. This PR deliberately does not introduce phase execution or migrate response/log plugins; it supplies the fixed lifecycle and outcome contract consumed by the later phase plans.

**Tech Stack:** Go 1.26 `net/http`, `github.com/felixge/httpsnoop`, existing `pkg/apisix/ctx`, Prometheus metrics facade, server route generations.

## Global Constraints

- Base the implementation on `origin/master` after PR #90; record the exact merge commit before editing.
- Do not change plugin priority, route matching, consumer resolution, transform ordering, or upstream behavior in this PR.
- Preserve the exact Flusher, FlushError, CloseNotifier, Hijacker, ReaderFrom, Pusher, StringWriter, deadline, and full-duplex capability set exposed by the original writer. `httpsnoop.Wrap` intentionally adds `Unwrap`; assert `httpsnoop.Unwrap(wrapped) == original` rather than requiring original/wrapped Unwrap parity.
- Treat final `101`, `204`, and `304` as committed bodyless responses; informational `100`–`199` except `101` do not mark the final response committed.
- Any `Flush` or `FlushError` attempt marks the response committed and flushed before calling the underlying writer, including an error or panic. A successful `Hijack` marks it committed and hijacked; a failed/unsupported hijack does not.
- Before commit, a recovered application panic clears uncommitted headers and writes exactly status `500`, `Content-Type: application/json; charset=UTF-8`, and body `{"message":"Internal Server Error"}`. Do not run status/body transforms over this response and do not expose the panic value.
- After final commit, flush, or hijack, recovery must not write another status or body. Run every finalizer, record a bounded stage, then panic with `http.ErrAbortHandler` so `net/http` aborts the connection or stream.
- If the recovered value is already `http.ErrAbortHandler`, run finalizers and re-panic it without counting it as a new application panic.
- Finalizers run in last-registered-first-run order, once only. A finalizer panic is logged and counted, but cannot skip later finalizers or replace the selected response.
- Metrics labels are bounded enums only. Never label with URI, route ID, request ID, panic text, or arbitrary plugin configuration.
- Write regression tests before production changes and record the exact red failure. Use impact-scoped tests, focused race checks, scoped lint, `make build`, `make clean`, and `git diff --check`.

---

### Task 1: Define the request lifecycle and response outcome contract

**Files:**
- Create: `pkg/apisix/ctx/lifecycle.go`
- Create: `pkg/apisix/ctx/lifecycle_test.go`

**Interfaces:**
- Produces:

```go
type ResponseOutcome struct {
    Kind      RequestOutcomeKind
    Status    int
    Bytes     int64
    Committed bool
    Flushed   bool
    Hijacked  bool
}

type RequestOutcomeKind string

const (
    RequestOutcomeCompleted      RequestOutcomeKind = "completed"
    RequestOutcomeRecoveredPanic RequestOutcomeKind = "recovered_panic"
    RequestOutcomeAbortedPanic   RequestOutcomeKind = "aborted_panic"
    RequestOutcomeHandlerAbort   RequestOutcomeKind = "handler_abort"
)

type RequestFinalizer func() error

type RequestLifecycle struct { /* mutex, sync.Once, finalizers, outcome, startedAt */ }

func NewRequestLifecycle(startedAt time.Time) *RequestLifecycle
func WithRequestLifecycle(r *http.Request, lifecycle *RequestLifecycle) *http.Request
func GetRequestLifecycle(r *http.Request) *RequestLifecycle
func EnsureRequestLifecycle(r *http.Request, startedAt time.Time) (*http.Request, *RequestLifecycle)
func (l *RequestLifecycle) AddFinalizer(owner string, finalizer RequestFinalizer) bool
func (l *RequestLifecycle) SetOutcome(outcome ResponseOutcome)
func (l *RequestLifecycle) Outcome() ResponseOutcome
func (l *RequestLifecycle) StartedAt() time.Time
func (l *RequestLifecycle) Finalize() []FinalizerFailure

type FinalizerFailure struct {
    Owner      string
    Err        error
    PanicValue any
    Stack      []byte
}
```

- `AddFinalizer` returns `false` after finalization has begun and must not run a late hook inline.
- `Finalize` snapshots the registered hooks under the mutex, releases the mutex, invokes each hook through a private recover boundary, and returns every callback error or recovered panic after running the entire snapshot. An ordinary error has `Err` only; a panic has `PanicValue` plus a non-empty stack.
- The outer owner initializes one shared `RequestState`, APISIX-vars map, and request-vars map before entering the route. Every derived request therefore refers to the same pooled state, and the outer request can recycle it after finalization.
- LIFO applies only to defer-style infrastructure hooks. A later log-phase plan registers one composite hook and orders loggers by APISIX priority inside that composite; it must not rely on raw lifecycle registration order.

- [ ] **Step 1: Write lifecycle regression tests**

Add tests named:

```go
func TestRequestLifecycleFinalizesInReverseOrderExactlyOnce(t *testing.T)
func TestRequestLifecycleCollectsErrorsAndPanicsAndContinues(t *testing.T)
func TestRequestLifecycleRejectsLateFinalizer(t *testing.T)
func TestRequestLifecycleSharesOutcomeAcrossRequestCopies(t *testing.T)
func TestRequestLifecycleInitializesSharedRequestState(t *testing.T)
```

The second test registers `first`, an `error`, `panic("boom")`, and `last`; assert reverse execution continues through all four and returned failures distinguish the error from the owner/panic/non-empty stack without re-panicking.

- [ ] **Step 2: Run the lifecycle tests and record the expected red failure**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/apisix/ctx -run "^TestRequestLifecycle" -count=1'
```

Expected: compile failure for missing `RequestLifecycle` APIs.

- [ ] **Step 3: Implement the minimal lifecycle**

Use a private context key and copy the finalizer slice before invocation. Do not hold the lifecycle mutex while invoking callbacks. The implementation must guard finalization with `sync.Once` and return a cloned outcome value rather than exposing mutable internal state.

- [ ] **Step 4: Run focused lifecycle tests and race tests**

```bash
bash -lc 'source .envrc && go test ./pkg/apisix/ctx -run "^TestRequestLifecycle" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/apisix/ctx -run "^TestRequestLifecycle" -count=3'
```

Expected: PASS with no race report.

### Task 2: Capture response commitment without changing writer capabilities

**Files:**
- Create: `pkg/plugin/base/outcome_writer.go`
- Create: `pkg/plugin/base/outcome_writer_test.go`

**Interfaces:**
- Consumes: `ctx.ResponseOutcome` from Task 1.
- Produces:

```go
func CaptureResponseOutcome(w http.ResponseWriter) (
    wrapped http.ResponseWriter,
    snapshot func() ctx.ResponseOutcome,
    closeHijacked func() error,
)
```

The returned writer is `httpsnoop.Wrap(w, hooks)`. The snapshot closure is concurrency-safe and returns the current final status and flags.

- [ ] **Step 1: Write outcome-writer regression tests**

Add tests named:

```go
func TestCaptureResponseOutcomePreservesOptionalInterfaces(t *testing.T)
func TestCaptureResponseOutcomeTracksInformationalAndFinalStatus(t *testing.T)
func TestCaptureResponseOutcomeDefaultsCompletedNoWriteToOK(t *testing.T)
func TestCaptureResponseOutcomeTracksImplicitOKAndBytes(t *testing.T)
func TestCaptureResponseOutcomeTracksFlushCommit(t *testing.T)
func TestCaptureResponseOutcomeTracksOnlySuccessfulHijack(t *testing.T)
func TestCaptureResponseOutcomeTracksReadFromBytes(t *testing.T)
func TestCaptureResponseOutcomeTracksWriteStringBytes(t *testing.T)
func TestCaptureResponseOutcomeTracksFlushErrorCommit(t *testing.T)
```

Use a minimal writer plus focused fakes that cover Flusher, FlushError, CloseNotifier, Hijacker, ReaderFrom, Pusher, StringWriter, response deadlines, and full-duplex. Assert exact parity for those nine capabilities and separately assert `httpsnoop.Unwrap(wrapped) == original`. For `103 -> 200`, assert final status is 200 and committed becomes true only at 200. For failed hijack, assert `Hijacked=false`.

- [ ] **Step 2: Run the writer tests and record the expected red failure**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base -run "^TestCaptureResponseOutcome" -count=1'
```

Expected: compile failure for missing `CaptureResponseOutcome`.

- [ ] **Step 3: Implement capture hooks using `httpsnoop.Wrap`**

Use a small mutex-protected state owned by the snapshot closure. Hook `WriteHeader`, `Write`, `WriteString`, `ReadFrom`, `Flush`, `FlushError`, and `Hijack`. Mark implicit/final commitment before calling an underlying write/flush so an underlying panic cannot be misclassified as pre-commit; add only the returned byte count. Record hijack only when the underlying call returns no error, retain that connection privately, and make `closeHijacked` close it at most once. `httpsnoop.Wrap` must also preserve deadline and full-duplex optional interfaces without custom forwarding code.

- [ ] **Step 4: Run focused writer tests and race tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base -run "^TestCaptureResponseOutcome" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/base -run "^TestCaptureResponseOutcome" -count=3'
```

Expected: PASS with exact optional-interface parity.

### Task 3: Add bounded panic metrics

**Files:**
- Create: `pkg/observability/metrics/request_panic.go`
- Create: `pkg/observability/metrics/request_panic_test.go`
- Modify: `pkg/observability/metrics/prometheus.go`
- Test: `pkg/observability/metrics/prometheus_test.go`

**Interfaces:**
- Produces:

```go
type RequestPanicStage string

const (
    RequestPanicPreCommit   RequestPanicStage = "pre_commit"
    RequestPanicPostCommit  RequestPanicStage = "post_commit"
    RequestPanicPostFlush   RequestPanicStage = "post_flush"
    RequestPanicPostHijack  RequestPanicStage = "post_hijack"
    RequestPanicFinalizer   RequestPanicStage = "finalizer"
)

func RecordRequestPanic(stage RequestPanicStage)
```

Register `apisix_http_request_panics_total{stage}` in the existing one-time Prometheus lifecycle. Invalid stages are ignored and never create a series. Calls before `metrics.Init` are nil-safe.

- [ ] **Step 1: Write metric lifecycle tests**

Cover nil-before-init, every allowed stage, invalid stage rejection, registry initialization, and no dynamic labels beyond `stage`.

- [ ] **Step 2: Run the metric tests and record the expected red failure**

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "^TestRecordRequestPanic|^TestRequestPanic" -count=1'
```

Expected: compile failure for the missing metric facade.

- [ ] **Step 3: Implement the bounded counter and registration**

Follow the private-registry observer constructor pattern in `pkg/observability/metrics/proxy_runtime.go`. Keep validation in the facade so callers cannot create unbounded label values.

- [ ] **Step 4: Run focused metrics tests**

```bash
bash -lc 'source .envrc && go test -race ./pkg/observability/metrics -run "RequestPanic" -count=3'
```

Expected: PASS.

### Task 4: Install the outer panic boundary in route generations

**Files:**
- Modify: `pkg/server/route_handler.go`
- Modify: `pkg/server/route_handler_test.go`

**Interfaces:**
- Consumes: `ctx.NewRequestLifecycle`, `ctx.WithRequestLifecycle`, `base.CaptureResponseOutcome`, `metrics.RecordRequestPanic`.
- Produces a private helper:

```go
func serveRouteRequest(
    w http.ResponseWriter,
    r *http.Request,
    handler http.Handler,
)
```

- [ ] **Step 1: Add panic-boundary regression tests before editing production code**

Add tests named:

```go
func TestRouteHandlerPanicBeforeCommitReturnsStableJSON(t *testing.T)
func TestRouteHandlerPanicResponseWriteFailureStillFinalizesAndAborts(t *testing.T)
func TestRouteHandlerPanicAfterWriteAbortsWithoutSecondResponse(t *testing.T)
func TestRouteHandlerPanicAfterFlushAbortsWithoutSecondResponse(t *testing.T)
func TestRouteHandlerPanicAfterHijackAbortsWithoutSecondResponse(t *testing.T)
func TestRouteHandlerAbortHandlerRunsFinalizersWithoutNewMetric(t *testing.T)
func TestRouteHandlerFinalizerPanicDoesNotSkipOtherFinalizers(t *testing.T)
func TestRouteHandlerPanicStillReleasesRouteGeneration(t *testing.T)
```

The post-commit tests call the private helper directly and recover the final `http.ErrAbortHandler`; use a real `net.Pipe`/HTTP server test for at least the write or flush case to assert the client sees EOF/connection abort rather than a normal complete second response. The hijack test must use a real Hijacker-capable writer, not `httptest.ResponseRecorder`.

- [ ] **Step 2: Run the server regression tests and record the red behavior**

```bash
bash -lc 'source .envrc && go test ./pkg/server -run "^TestRouteHandler(Panic|Abort|Finalizer)" -count=1'
```

Expected before the fix: panic escapes to the test or `net/http` closes without the stable pre-commit JSON, and registered finalizers do not exist.

- [ ] **Step 3: Implement the route-owned lifecycle boundary**

In `routeHandler.ServeHTTP`, keep `defer h.finishRequest(current)` outside `serveRouteRequest` so generation accounting is always released. `serveRouteRequest` creates the lifecycle, wraps the writer, and installs one defer that performs this exact order:

1. recover the downstream value, if any, and snapshot commitment/flush/hijack;
2. select the final outcome kind; for an uncommitted application panic, clear headers and write the stable JSON 500 before the final snapshot; if that recovery write panics, short-writes, or returns an error, retain abort semantics while still completing steps 3–5;
3. store the final status/bytes/outcome in the lifecycle (`200` for a completed handler that wrote nothing);
4. run every lifecycle finalizer and record/log each isolated callback error or panic;
5. recycle shared request state exactly once after all finalizers;
6. for committed/flushed/hijacked panic, close a successfully hijacked connection and panic `http.ErrAbortHandler`; for an original `http.ErrAbortHandler`, re-panic it unchanged.

Clear every existing header before writing the stable pre-commit 500. Log the original panic and `debug.Stack()` internally, but never write either to the response.

- [ ] **Step 4: Run focused server tests and race tests**

```bash
bash -lc 'source .envrc && go test ./pkg/server -run "^TestRouteHandler" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/server -run "^TestRouteHandler(Panic|Abort|Finalizer|Replacement|Close)" -count=3'
```

Expected: PASS; existing route replacement/drain behavior remains unchanged.

### Task 5: Move request metrics and variable recycling onto the lifecycle

**Files:**
- Modify: `pkg/plugin/request_context/plugin.go`
- Modify: `pkg/plugin/request_context/plugin_test.go`

**Interfaces:**
- Consumes: `ctx.GetRequestLifecycle(r)`, lifecycle start time/outcome, `AddFinalizer`.
- Keeps the current `Plugin` and `Handler` public contracts unchanged.

- [ ] **Step 1: Add regression tests for early return and panic cleanup**

Add tests named:

```go
func TestRequestContextRegistersMetricsAndRecycleFinalizer(t *testing.T)
func TestRequestContextFinalizerRecordsEarlyReturnOutcome(t *testing.T)
func TestRequestContextFinalizerRunsAfterDownstreamPanic(t *testing.T)
func TestRequestContextLegacyDirectHandlerStillFinalizes(t *testing.T)
```

Use a lifecycle in the first three tests and assert `Finalize` records metrics but does not recycle state; then simulate the outer owner calling `RecycleVars` and assert recycling happens afterward. The legacy test calls the plugin handler without an outer lifecycle and asserts the direct-call path creates/finalizes a local lifecycle and recycles after its finalizers.

- [ ] **Step 2: Run the request-context tests and record the red behavior**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/request_context -run "^TestRequestContext.*Finalizer" -count=1'
```

Expected: new tests fail because metrics and recycling still execute only after `next` returns normally.

- [ ] **Step 3: Register one captured-request finalizer**

After creating APISIX/request vars, obtain the lifecycle. Register a closure that reads the lifecycle outcome and records the existing HTTP request metrics; the lifecycle owner recycles request state after every finalizer, not inside this closure. If no outer lifecycle exists, preserve direct-call compatibility with the current capture path but move `ctx.RecycleVars(r)` into a defer so even a direct downstream panic cannot leak pooled maps. Remove the normal-path-only metric/recycle block.

- [ ] **Step 4: Run request-context and combined focused tests**

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/request_context ./pkg/apisix/ctx ./pkg/plugin/base ./pkg/server -run "(RequestContext|RequestLifecycle|CaptureResponseOutcome|RouteHandlerPanic)" -count=1'
```

Expected: PASS with no double metric or double recycle.

### Task 6: Final verification, review, and delivery

**Files:**
- Create in the implementation PR: `docs/superpowers/plans/2026-08-10-production-readiness-index.md`
- Create in the implementation PR: `docs/superpowers/plans/2026-08-10-plugin-phase-and-panic-boundary.md`
- Create in the implementation PR: `docs/superpowers/plans/2026-08-10-plugin-capability-manifest.md`
- Create in the implementation PR: all seven detailed Plan 11–17 documents. This PR is their sole coordination-document publication owner; later PRs update only their own plan if implementation drift requires it.

- [ ] **Step 1: Format only touched Go files and inspect the diff**

```bash
bash -lc 'source .envrc && golangci-lint fmt pkg/apisix/ctx pkg/plugin/base pkg/observability/metrics pkg/server pkg/plugin/request_context'
git diff --check
```

Discard unrelated formatter edits.

- [ ] **Step 2: Run affected package tests and the focused race gate**

```bash
bash -lc 'source .envrc && go test ./pkg/apisix/ctx ./pkg/plugin/base ./pkg/observability/metrics ./pkg/plugin/request_context ./pkg/server -count=1'
bash -lc 'source .envrc && go test -race ./pkg/apisix/ctx ./pkg/plugin/base ./pkg/plugin/request_context ./pkg/server -run "(RequestLifecycle|CaptureResponseOutcome|RouteHandlerPanic|RequestContext.*Finalizer)" -count=3'
```

- [ ] **Step 3: Run scoped lint and build smoke**

```bash
bash -lc 'source .envrc && golangci-lint run ./pkg/apisix/ctx/... ./pkg/plugin/base/... ./pkg/observability/metrics/... ./pkg/plugin/request_context/... ./pkg/server/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 4: Perform independent read-only review before delivery**

The reviewer must verify optional-interface parity, 1xx/final status accounting, pre/post-commit panic behavior, `http.ErrAbortHandler`, finalizer isolation/order, generation release, and exact-once request metrics/recycling. Any High/Medium finding returns to the owning task with one bounded regression-first follow-up.

- [ ] **Step 5: Deliver one independent PR**

Stage only the ten coordination plan documents above and accepted implementation paths. Commit with:

```bash
git commit -m "feat(server): add panic-safe request lifecycle"
```

Push a `codex/` branch, open a ready PR targeting current `master`, wait for required CI, and merge only after CI and independent review are green.

## Fast-plan-impl Dispatch Ownership

1. **WU-01 lifecycle and outcome contract** owns `pkg/apisix/ctx/lifecycle*` and `pkg/plugin/base/outcome_writer*`. It lands both fixed interfaces first.
2. **WU-02 metric and server boundary** owns `pkg/observability/metrics/request_panic*`, `pkg/observability/metrics/prometheus.go`, `pkg/observability/metrics/prometheus_test.go`, and `pkg/server/route_handler*`. It starts after WU-01 is accepted.
3. **WU-03 request-context integration** owns only `pkg/plugin/request_context/plugin.go` and its test and may run in parallel with WU-02 after WU-01. Workers do not edit the plan, commit, push, or open a PR.

## Explicit Deferrals

- Global/route/consumer rewrite and access ordering belongs to the request-phase plans.
- Buffered response-transform ordering belongs to the buffered-response phase plan.
- SSE, compression, gRPC streaming, WebSocket, and hijack compatibility belongs to the streaming/terminal phase plan; this PR only guarantees the outer outcome wrapper preserves capabilities.
- Logger/tracer migration onto lifecycle hooks belongs to the log/finalizer phase plan. Only the system `request-context` cleanup is migrated here because it owns pooled request state required for panic safety.
- This PR alone does not close PR-014 or all of P1 5.5. It establishes the merge-safe foundation those closures require.
