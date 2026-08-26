# Immutable Task10 Plugin Panic and Finalization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Recover only panics raised by explicitly identified runtime plugin callbacks, finish every request cleanup exactly once, and allow unknown core-invariant panics to escape the route boundary unchanged after cleanup.

**Architecture:** A `plugin.PanicError` captures the canonical factory, bounded runtime phase, original value and boundary stack at each plugin-owned callback. Middleware, protocol continuations and returned streaming writers use a private downstream-panic carrier so an outer plugin cannot relabel a panic raised below it. `RequestLifecycle` keeps its reverse-order `sync.Once` execution but records plugin finalizer failures separately from delayed core-invariant panics; the server applies response policy only to typed plugin panics and re-panics core values after finalization, request-state recycling and lease release.

**Tech Stack:** Go 1.26, `net/http`, `runtime/debug`, `sync`, Prometheus client, repository immutable route/compiler runtime, impact-scoped Go tests and race detector.

**Spec:** `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md` Task 10; implementation begins from Task9 handoff `master@5a502d64`.

## Global Constraints

- Source `.envrc` and set `GOFLAGS=-mod=readonly` before every Go, lint or build command.
- Preserve the immutable Task9 boundary: runtime handlers consume published generation snapshots and leases; do not restore `route.Builder`, mutable store reads, cluster registry, or runtime plugin re-resolution.
- Guard only request-time plugin callbacks. Task10 adds no recover boundary around compiler, registry, generation, snapshot publication, descriptor discovery, `Init`, `PostInit`, configuration or route materialization; their pre-existing contracts are not broadened.
- Factory identity comes only from descriptor-bearing bindings, response binding factory keys or protocol candidate identity. Never call plugin-controlled `GetName()` while handling a panic.
- `PanicError.Error()` and metric labels must not contain the raw panic value. Prometheus labels are bounded owner/stage enums; factory names belong only in controlled logs.
- Existing `*plugin.PanicError`, `http.ErrAbortHandler` and downstream raw panic values pass through unchanged; outer plugins must not relabel them.
- Plugin panic before response commit produces the stable JSON 500. Plugin panic after commit, flush or hijack writes no second response and ends with `http.ErrAbortHandler`; only the failed request's hijacked connection is closed.
- Unknown core panic and core-finalizer panic run all finalizers, recycle request state, close a failed hijack, release the generation lease and then re-panic the original value. A request's primary core panic takes precedence over a later core-finalizer panic.
- Finalizers remain reverse ordered and exactly once under concurrent `Finalize` calls. A panic in one plugin finalizer, streaming closer or streaming finalizer must not skip later cleanup.
- Current `net/http.Server` catches handler panics at its connection-goroutine boundary. Task10 proves escape from `routeHandler`; process-level worker termination remains a dependency of the later supervisor/worker program and must not be claimed here.
- Task11 owns goroutine lifecycle migration. Task10 changes `batch_requests` only enough to stop converting unknown core panic into a normal batch response.
- Use failing behavior tests before implementation, keep each code wave independently reviewable, and use only impact-scoped tests listed below.

## File Responsibility Map

| File | Responsibility in Task10 |
| --- | --- |
| `pkg/plugin/panic.go` | Typed plugin panic, bounded callback phase and downstream pass-through guards. |
| `pkg/plugin/executor.go` | Request/auth/middleware entry and unwind boundaries plus before-proxy invocation. |
| `pkg/plugin/response_executor.go` | Buffered mode, eligibility, header/body and final-store callback boundaries. |
| `pkg/plugin/streaming_executor.go` | Streaming/compression/protocol callback and returned-writer boundaries; isolated cached finish result. |
| `pkg/plugin/log_executor.go` | Canonical log binding identity and recoverable log/snapshot callback failures. |
| `pkg/apisix/ctx/lifecycle.go` | Plugin versus core finalizer trust, reverse exactly-once finalization result. |
| `pkg/apisix/ctx/context.go` | Owner-bearing before-proxy hook registration without importing `pkg/plugin`. |
| `pkg/server/route_handler.go` | Type-directed response policy and delayed core re-panic after cleanup. |
| `pkg/observability/metrics/request_panic.go` | Bounded panic-owner and response-stage counters. |
| `pkg/plugin/batch_requests/plugin.go` | Preserve `http.ErrAbortHandler` batch behavior without swallowing unknown core panic. |
| Existing dynamic-finalizer plugin files | Continue using `AddFinalizer`, whose Task10 compatibility meaning is explicitly plugin-owned. |

## Dependency and Integration Order

1. Wave 1 runs Task 1 and Task 2 from the same frozen `5a502d64` base because their write paths do not overlap. Review each diff independently, then integrate Task 1 before Task 2.
2. Wave 2 starts only from the reviewed Wave 1 integration head. Tasks 3, 4 and 5 have exclusive production/test files and may run in parallel; integrate in numeric order so failures have one deterministic history.
3. Task 6 starts from a head containing reviewed Task3 because it consumes the frozen binding log policy. It can run while Task4/Task5 review finishes because its log-executor files remain exclusive, but its commit integrates after Tasks 4 and 5.
4. Wave 3 runs Task 7 after all callback and lifecycle contracts exist, then Task 8 after route policy is green.
5. Wave 4 runs Task 9 against the single integrated Task10 head. Only this reviewed head may fast-forward local `master`.
6. Every worker starts from the declared frozen base, owns only its task's files, does not rebase onto another in-flight worker and does not push or open a PR. The phase owner inspects every diff and test result before integration.

---

### Task 1: Define Typed Plugin Panic and Attribution-Safe Guards

**Files:**
- Create: `pkg/plugin/panic.go`
- Create: `pkg/plugin/panic_test.go`

**Interfaces:**
- Consumes: `plugin.Phase`, canonical non-empty descriptor factory strings and callback closures.
- Produces: `PanicError`, `guardCall`, `guardValue`, `guardMiddleware` and a package-private downstream carrier used by later executor tasks.

- [ ] **Step 1: Write failing primitive and middleware tests**

Add table-driven tests that assert exact factory/phase/stack capture, no raw-value disclosure from `Error`, normal return preservation, typed panic identity preservation and the existing nil-handler validation behavior. Add nested middleware tests for entry panic, unwind panic, inner `*PanicError`, raw downstream core panic and `http.ErrAbortHandler`.

```go
func TestGuardMiddlewareDoesNotRelabelDownstreamPanic(t *testing.T) {
	want := &struct{ message string }{message: "core invariant"}
	handler := guardMiddleware("outer", PhaseRewrite,
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) })
		},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(want) }),
	)
	defer func() {
		if got := recover(); got != want { t.Fatalf("panic = %#v, want original %#v", got, want) }
	}()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestPanicErrorDoesNotDiscloseValue(t *testing.T) {
	secret := "secret-panic-value"
	err := capturePluginPanicForTest("request-id", PhaseRewrite, secret)
	if strings.Contains(err.Error(), secret) { t.Fatalf("Error() disclosed panic value: %q", err.Error()) }
	if err.Factory != "request-id" || err.Phase != PhaseRewrite || len(err.Stack) == 0 {
		t.Fatalf("panic metadata = %#v", err)
	}
}
```

- [ ] **Step 2: Run the primitive tests and confirm RED**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin -run "^(TestGuard|TestPanicError)" -count=1'`

Expected: FAIL because `PanicError` and the guard functions do not exist.

- [ ] **Step 3: Implement `PanicError` and callback guards**

Use these exact package-private contracts; `guardValue` is generic so selector and response callbacks retain their existing return shapes.

```go
type PanicError struct {
	Factory string
	Phase   Phase
	Value   any
	Stack   []byte
}

func (e *PanicError) Error() string
func guardCall(factory string, phase Phase, call func() error) error
func guardValue[T any](factory string, phase Phase, call func() (T, error)) (T, error)
func guardMiddleware(factory string, phase Phase,
	build func(http.Handler) http.Handler, next http.Handler) http.Handler
```

Implement private `downstreamPanic{value any}` and `guardContinuation(next http.Handler) http.Handler`. `guardMiddleware` first validates `build` and `next`, then passes the protected continuation to `build` outside the request-time recover and validates the returned handler before request execution; nil follows the current construction-error path and is never synthesized as a plugin panic. The continuation converts a downstream panic to the carrier, and the outer guard unwraps the carrier without changing its value. If a recovered value is already `*PanicError`, return or panic that exact pointer. Capture `debug.Stack()` only when converting a new raw plugin panic.

- [ ] **Step 4: Run primitive tests and verify GREEN**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin -run "^(TestGuard|TestPanicError)" -count=1'`

Expected: PASS; nested downstream values compare by identity and the boundary stack is non-empty.

- [ ] **Step 5: Commit the panic foundation**

```bash
git add pkg/plugin/panic.go pkg/plugin/panic_test.go
git commit -m "feat(runtime): define attributed plugin panics"
```

---

### Task 2: Classify Finalizers and Return One Exactly-Once Finalization Result

**Files:**
- Modify: `pkg/apisix/ctx/lifecycle.go`
- Modify: `pkg/apisix/ctx/lifecycle_test.go`

**Interfaces:**
- Consumes: existing `RequestFinalizer`, reverse registration order, `sync.Once` and dynamic plugin cleanup sites.
- Produces: `FinalizerOwnerKind`, cached `FinalizationResult`, compatibility `Finalize() []FinalizerFailure`, `FinalizeResult() FinalizationResult`, compatibility `AddFinalizer` as plugin-owned registration and explicit `AddCoreInvariantFinalizer`.

- [ ] **Step 1: Write failing trust, ordering, concurrency and recycle tests**

Cover a plugin panic followed by successful cleanup, a core panic followed by all remaining cleanup, multiple core panics selecting the first failure in execution order, repeat/concurrent `Finalize` returning detached equivalent results, and accepted-before-finalize versus rejected-after-finalize registration.

```go
func TestRequestLifecycleCoreFinalizerPanicRunsRemainingFinalizers(t *testing.T) {
	lifecycle := NewRequestLifecycle(time.Now())
	order := make([]string, 0, 3)
	lifecycle.AddFinalizer("plugin-last", func() error { order = append(order, "plugin-last"); return nil })
	lifecycle.AddCoreInvariantFinalizer("core", func() error { order = append(order, "core"); panic("core-finalizer") })
	lifecycle.AddFinalizer("plugin-first", func() error { order = append(order, "plugin-first"); panic("plugin-finalizer") })
	result := lifecycle.FinalizeResult()
	if diff := cmp.Diff([]string{"plugin-first", "core", "plugin-last"}, order); diff != "" { t.Fatal(diff) }
	if result.FatalPanic == nil || result.FatalPanic.PanicValue != "core-finalizer" { t.Fatalf("result = %#v", result) }
	if len(result.Failures) != 2 { t.Fatalf("failures = %#v", result.Failures) }
}
```

- [ ] **Step 2: Run lifecycle tests and confirm RED**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/apisix/ctx -run "^TestRequestLifecycle" -count=1'`

Expected: FAIL because the core registration API and finalization result do not exist.

- [ ] **Step 3: Implement the finalizer trust/result contract**

Use exact types and preserve the existing short spelling for plugin callers:

```go
type FinalizerOwnerKind uint8

const (
	FinalizerOwnerPlugin FinalizerOwnerKind = iota + 1
	FinalizerOwnerCoreInvariant
)

type FinalizationResult struct {
	Failures   []FinalizerFailure
	FatalPanic *FinalizerFailure
}

func (l *RequestLifecycle) AddFinalizer(owner string, fn RequestFinalizer) bool {
	return l.addFinalizer(FinalizerOwnerPlugin, owner, fn)
}

func (l *RequestLifecycle) AddCoreInvariantFinalizer(owner string, fn RequestFinalizer) bool {
	return l.addFinalizer(FinalizerOwnerCoreInvariant, owner, fn)
}

func (l *RequestLifecycle) FinalizeResult() FinalizationResult
func (l *RequestLifecycle) Finalize() []FinalizerFailure {
	return l.FinalizeResult().Failures
}
```

Add `Kind FinalizerOwnerKind` to `FinalizerFailure`. Run every snapshotted finalizer in reverse. Plugin error or panic is appended to `Failures`; core error remains a normal reported failure, while the first core panic in execution order is assigned to `FatalPanic` and also appears in `Failures`. Cache the full result under the existing `sync.Once`, and deep-clone both stacks on return. Keep `Finalize` as the source-compatible failure-slice wrapper used by existing packages; only the server recovery owner consumes `FinalizeResult`. Neither method panics internally.

- [ ] **Step 4: Lock the compatibility registration to plugin trust**

Keep the five existing production dynamic cleanup sites source-compatible through `AddFinalizer`; no plugin implementation file changes are required. Add a focused AST contract test in `pkg/apisix/ctx/lifecycle_test.go` allowing `AddCoreInvariantFinalizer` under `pkg/plugin` only at the exact `log_executor.go` composite-registration function introduced by Task6. The composite owns core snapshot construction/orchestration while its individual plugin callbacks are guarded; every other plugin package is forbidden from selecting fatal trust.

```go
sites := findSelectorCalls(t, pluginSources, "AddCoreInvariantFinalizer")
allowed := callSite{File: "pkg/plugin/log_executor.go", Function: "RegisterComposite"}
for _, site := range sites {
	if site != allowed { t.Fatalf("unauthorized core finalizer registration: %#v", site) }
}
```

- [ ] **Step 5: Run lifecycle, plugin cleanup and race tests**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/apisix/ctx ./pkg/plugin/api_breaker ./pkg/plugin/limit_conn ./pkg/plugin/otel ./pkg/plugin/skywalking ./pkg/plugin/zipkin -run "^(TestRequestLifecycle|Test.*Finalizer|Test.*Release|Test.*Span)" -count=1 && go test -race ./pkg/apisix/ctx -run "^TestRequestLifecycle" -count=1'`

Expected: PASS; every accepted finalizer runs once, reverse order is deterministic and no plugin package can claim core trust.

- [ ] **Step 6: Commit lifecycle classification**

```bash
git add pkg/apisix/ctx/lifecycle.go pkg/apisix/ctx/lifecycle_test.go
git commit -m "refactor(runtime): classify request finalizers"
```

---

### Task 3: Guard Request Middleware, Request Phases and Before-Proxy Hooks

**Files:**
- Modify: `pkg/plugin/executor.go`
- Modify: `pkg/plugin/executor_test.go`
- Modify: `pkg/plugin/descriptor_binding_test.go`
- Modify: `pkg/apisix/ctx/context.go`
- Modify: `pkg/apisix/ctx/context_test.go`
- Modify: `pkg/plugin/proxy_mirror/plugin.go`
- Modify: `pkg/plugin/proxy_mirror/plugin_test.go`

**Interfaces:**
- Consumes: Task1 guards, `Binding.Descriptor.Factory`, descriptor request stage, current ordered hook state.
- Produces: attributed runtime request/auth/static-CORS/legacy middleware guards, frozen log-capture policy on strict bindings and `BeforeProxyHookRegistration` carrying canonical owner and phase.

- [ ] **Step 1: Write failing request boundary tests**

Add exact tests for rewrite/access/auth request-phase panic, legacy middleware entry and unwind panic, static CORS callback panic, inner plugin panic and downstream core panic. Verify each handler is still constructed once. Add before-proxy tests for ordered execution, cached first error, typed owner/phase panic and no repeat execution. Add a binding test proving `LogCapturePolicy()` runs exactly once in `bindResolvedPlugin`, is validated there, and the immutable binding holds a value copy.

```go
func TestRequestMiddlewareDoesNotRelabelTerminalPanic(t *testing.T) {
	want := &struct{ owner string }{owner: "core"}
	binding := requestBinding("request-id", PhaseRewrite, panicBeforeOrAfterNextPlugin{})
	handler := requestPipeline(binding).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(want) }))
	assertRecoveredIdentity(t, want, func() { handler.ServeHTTP(newRecorder(), newRequest()) })
}
```

- [ ] **Step 2: Run request and hook tests and confirm RED**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin ./pkg/apisix/ctx ./pkg/plugin/proxy_mirror -run "^(TestRequest.*Panic|TestLegacy.*Panic|TestCompositeChild.*Panic|TestBind.*LogCapturePolicy|TestBeforeProxy.*Panic|TestProxyMirror.*Hook)" -count=1'`

Expected: FAIL because callbacks currently emit raw panic and hooks carry no owner identity.

- [ ] **Step 3: Guard request callback execution without guarding compilation**

At `requestStageHandler`, wrap only the returned handler's runtime `ServeHTTP`; do not recover around `Plugin.Handler(next)` construction, descriptor lookup, expression compilation or binding construction. Derive phase from the binding request stage using one bounded mapping. Apply the same rule to auth wrappers, static CORS and the legacy path. Every `next` supplied to plugin middleware must be a Task1 downstream continuation.

Treat every composite child that is not independently registered in the immutable binding set as an implementation detail of its outer binding. This applies to `multi-auth` authentication children and `workflow` limit-req/limit-conn/limit-count actions: their raw callback panic is attributed to factory `multi-auth` or `workflow`, respectively. Add a synthetic composite-child panic test in `executor_test.go` that locks outer-binding `PanicError.Factory`. Verify the real `multi-auth` child calls remain inside the guarded outer `RunRequestPhase`, while the real `workflow` child calls remain inside the guarded outer legacy `Handler.ServeHTTP`. Task7's owner-only metric labels ensure neither outer factory nor child-config name becomes a label. Do not add a parent-package import to composite plugin packages.

- [ ] **Step 4: Freeze log capture policy on strict bindings**

Add private value fields to `Binding`:

```go
logPolicy    base.LogCapturePolicy
logPolicySet bool
```

In `bindResolvedPlugin`, call `LogCapturePolicy()` once during binding/materialization when implemented, validate it with `base.ValidateLogCapturePolicy`, copy it into `logPolicy`, and set `logPolicySet` even for the zero policy. A resolved production binding without `logPolicySet` is invalid; Task6 consumes only this frozen value. This is a materialization callback and is intentionally not recovered by Task10.

- [ ] **Step 5: Add owner-bearing before-proxy hook registrations**

Keep `ctx` independent of `pkg/plugin` with string metadata:

```go
type BeforeProxyHookRegistration struct {
	Owner string
	Phase string
	Hook  BeforeProxyHook
}

func WithBeforeProxyHookRegistration(r *http.Request, registration BeforeProxyHookRegistration) *http.Request
func RunBeforeProxyHookRegistrations(
	r *http.Request,
	invoke func(BeforeProxyHookRegistration) error,
) error
```

Change the hook state to store registrations while retaining its `sync.Once`, registration order and cached first error. `RunBeforeProxyHookRegistrations` invokes each accepted registration through the supplied callback. Its `once.Do` closure captures any raw panic into a cached `panicValue`; after `once.Do`, every caller re-panics that exact cached value, so `sync.Once` never turns a first raw panic into a later nil result.

Keep `WithBeforeProxyHook` and `RunBeforeProxyHooks` as core/test compatibility wrappers using an empty owner; raw compatibility-hook panic remains core. Make `proxy_mirror` use owner `proxy-mirror` and phase `before_proxy`. In `executor.go`, pass an invoker that executes empty-owner hooks raw, validates owner-bearing phase to the existing bounded `Phase`, applies Task1 `guardCall`, and re-panics a returned `*PanicError` after `RunBeforeProxyHookRegistrations` caches it. Invalid owner/phase returns a normal internal error and never becomes a plugin panic.

- [ ] **Step 6: Run request boundary tests and affected package tests**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin ./pkg/apisix/ctx ./pkg/plugin/proxy_mirror -run "^(TestRequest|TestExecutor|TestLegacy|TestCompositeChild|TestBind.*LogCapturePolicy|TestBeforeProxy|TestProxyMirror)" -count=1'`

Expected: PASS; downstream values remain identical and before-proxy hooks preserve ordered once-only execution.

- [ ] **Step 7: Commit request callback boundaries**

```bash
git add pkg/plugin/executor.go pkg/plugin/executor_test.go pkg/plugin/descriptor_binding_test.go pkg/apisix/ctx/context.go pkg/apisix/ctx/context_test.go pkg/plugin/proxy_mirror/plugin.go pkg/plugin/proxy_mirror/plugin_test.go
git commit -m "refactor(runtime): guard request plugin callbacks"
```

---

### Task 4: Guard Buffered Response Callbacks

**Files:**
- Modify: `pkg/plugin/response_executor.go`
- Modify: `pkg/plugin/response_executor_test.go`
- Modify: `pkg/plugin/response_capability_test.go`

**Interfaces:**
- Consumes: Task1 `guardCall`/`guardValue`, `ResponseBinding.factoryKey`, response source and current response-plan ordering.
- Produces: attributed mode-selector, eligibility, header, body and final-store panics; core committer panic remains raw.

- [ ] **Step 1: Write failing table-driven buffered callback tests**

Cover `SelectResponseMode`, `AppliesToResponseSource`, `RunHeaderFilter`, `RunBufferedBodyFilter` and `RunFinalResponseStore`. Assert factory and `header_filter`/`body_filter` phase. Update the existing store-panic test to expect `*PanicError`, while retaining fail-closed ordering: previous side effects remain, later stores do not run and no response is committed. Add a raw panic test for `FinalResponseCommitter.CommitFinalResponse`.

```go
func TestBufferedPluginPanicCarriesBindingIdentity(t *testing.T) {
	panicValue := &struct{ callback string }{callback: "header"}
	plan := responsePlanWithHeaderPanic("response-rewrite", panicValue)
	recovered := capturePanic(func() { plan.Then(coreTerminal()).ServeHTTP(newRecorder(), newRequest()) })
	panicErr, ok := recovered.(*PanicError)
	if !ok || panicErr.Factory != "response-rewrite" || panicErr.Phase != PhaseHeaderFilter || panicErr.Value != panicValue {
		t.Fatalf("panic = %#v", recovered)
	}
}
```

- [ ] **Step 2: Run buffered callback tests and confirm RED**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin -run "^(TestBufferedPluginPanic|TestFinalStorePanic|TestFinalResponseCommitterPanic)" -count=1'`

Expected: FAIL because current callbacks panic raw.

- [ ] **Step 3: Apply guards at exact target invocations**

Guard each target callback using the immutable response binding's canonical factory. Re-panic any `*PanicError` returned through an error-shaped guard before existing ordinary-error policy runs. Do not place the guard around response-plan orchestration or `CommitFinalResponse`.

- [ ] **Step 4: Run buffered response package tests**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin -run "^(TestBuffered|TestResponse|TestFinalStore|TestFinalResponse|TestPlan.*Response)" -count=1'`

Expected: PASS; ordinary callback errors retain their existing response policy.

- [ ] **Step 5: Commit buffered response boundaries**

```bash
git add pkg/plugin/response_executor.go pkg/plugin/response_executor_test.go pkg/plugin/response_capability_test.go
git commit -m "refactor(runtime): guard buffered response callbacks"
```

---

### Task 5: Guard Streaming, Compression and Protocol Callbacks

**Files:**
- Modify: `pkg/plugin/streaming_executor.go`
- Modify: `pkg/plugin/streaming_executor_test.go`

**Interfaces:**
- Consumes: Task1 guards, response binding factory identity, protocol candidate identity and current streaming response contracts.
- Produces: owner-bearing streaming entries, guarded returned writer operations, continuation-safe protocol terminals and cached reverse-order finish results.

- [ ] **Step 1: Write failing callback, writer and finish tests**

Add cases for streaming header, mode selector, response wrapper construction, compression offer registration/wrap, returned `Header`/`WriteHeader`/`Write`/`WriteString`/`ReadFrom`/`Flush`/`FlushError`/`Hijack`/`CloseNotify`/`Push`/read-deadline/write-deadline/full-duplex methods, exclusive protocol before-next/after-next and raw downstream panic. For every optional interface, test both supported and unsupported downstream writers so the guard neither loses nor invents capability. Add closer/finalizer tests proving a panic does not skip remaining entries, original terminal panic wins over finish failures, ordinary finish error keeps the existing response behavior, and concurrent `finish` calls wait for and receive the same cloned result.

```go
func TestStreamingFinishPanicDoesNotSkipRemainingCleanup(t *testing.T) {
	finish := newStreamingFinish([]streamingFinalizerEntry{
		{factory: "first", finalizer: recordingFinalizer("first", nil)},
		{factory: "panic", finalizer: recordingFinalizer("panic", panicValue)},
		{factory: "last", finalizer: recordingFinalizer("last", nil)},
	})
	result := finish.finish(nil)
	if diff := cmp.Diff([]string{"last", "panic", "first"}, recordedOrder()); diff != "" { t.Fatal(diff) }
	if len(result.Panics) != 1 || result.Panics[0].Factory != "panic" { t.Fatalf("result = %#v", result) }
}

func TestStreamingFinishPreservesFirstOrdinaryError(t *testing.T) {
	want := errors.New("close failed")
	order := make([]string, 0, 2)
	finish := newStreamingFinishWithClosers(
		recordingErrorCloser("registered-first", errors.New("later failure"), &order),
		recordingErrorCloser("registered-last", want, &order),
	)
	result := finish.finish(nil)
	if !errors.Is(result.Err, want) || len(result.Panics) != 0 { t.Fatalf("result = %#v", result) }
	if diff := cmp.Diff([]string{"registered-last", "registered-first"}, order); diff != "" { t.Fatal(diff) }
}
```

- [ ] **Step 2: Run streaming tests and confirm RED**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin -run "^(TestStreaming.*Panic|TestStreamingFinish|TestCompression.*Panic|TestProtocol.*Panic)" -count=1'`

Expected: FAIL because owner identity is discarded and the first finish panic exits the loop.

- [ ] **Step 3: Store factory identity with every deferred streaming operation**

Replace bare closer/finalizer slices with private entries carrying `factory`, `phase` and callback. Extend internal compression offer entries and protocol candidates only as needed to retain an already canonical identity; do not call `GetName()` during request execution.

```go
type streamingFinalizerEntry struct {
	factory   string
	phase     Phase
	finalizer base.StreamingResponseFinalizer
}

type streamingFinishResult struct {
	Err      error
	Panics   []*PanicError
}
```

- [ ] **Step 4: Guard construction and returned writer operations**

Before calling each plugin wrapper constructor, pass it a protected downstream writer whose methods convert panics from the current writer into the Task1 downstream carrier. Wrap the plugin-produced writer with a private owner writer that catches only raw panics from that plugin and unwraps the carrier unchanged. Build both wrappers with `httpsnoop.Wrap` hooks so the library preserves the wrapped writer's exact optional-interface set. Guard every supported hook with `PhaseBodyFilter`, including close notification, HTTP/2 push, deadlines and full duplex. Apply the same pair to response wrappers and compression wrappers so an outer writer cannot relabel an inner-plugin or core writer panic.

- [ ] **Step 5: Make streaming finish reverse, complete, concurrent and cached**

Replace `atomic.Bool` with `sync.Once` plus a cached `streamingFinishResult`. Execute closers in reverse registration order, then streaming finalizers in reverse registration order, guarding each entry separately and continuing after failure. Preserve the first ordinary `Close`/`FinishStreamingResponse` error in `Err`; append every guarded plugin panic to `Panics`. Callers arriving concurrently wait for `sync.Once` completion and receive detached panic stacks. When a terminal/commit panic is the finish cause, pass a stable private `errStreamingPanic` sentinel rather than formatting the raw panic value into an error visible to plugin finalizers. At each existing finish call site apply this precedence: an already captured terminal/commit panic is re-panicked unchanged after finish; otherwise the first plugin panic is re-panicked as `*PanicError`; otherwise `Err` follows the existing pre-commit stable-error or post-commit `http.ErrAbortHandler` policy. Additional finish panics are logged with bounded factory/phase data and never replace the primary value.

- [ ] **Step 6: Guard exclusive protocol continuations**

Run the protocol plugin with its candidate identity and `PhaseProtocol`; pass it a Task1 protected continuation. A panic before or after a successful `next` is attributed to the protocol plugin, while a panic raised by `next` escapes unchanged.

- [ ] **Step 7: Run streaming normal and race tests**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin ./pkg/plugin/gzip ./pkg/plugin/brotli ./pkg/plugin/ai_rate_limiting ./pkg/plugin/ai_aliyun_content_moderation -run "^(TestStreaming|TestCompression|TestProtocol|TestExclusive|Test.*StreamingResponse|Test.*Compression)" -count=1 && go test -race ./pkg/plugin -run "^(TestStreamingFinish|TestStreaming.*Panic|TestProtocol.*Panic)" -count=1'`

Expected: PASS; streaming finish side effects occur once and every concurrent caller observes the same failures.

- [ ] **Step 8: Commit streaming boundaries**

```bash
git add pkg/plugin/streaming_executor.go pkg/plugin/streaming_executor_test.go
git commit -m "refactor(runtime): isolate streaming plugin panics"
```

---

### Task 6: Guard Log and Snapshot Finalizer Callbacks

**Files:**
- Modify: `pkg/plugin/log_executor.go`
- Modify: `pkg/plugin/log_executor_test.go`

**Interfaces:**
- Consumes: Task1 guards, Task2 core-finalizer API, Task3 frozen log policy and descriptor factory from each source `Binding`.
- Produces: `LogBinding.Factory`, attributed recoverable log/sanitizer/snapshot failures and continued callback execution.

- [ ] **Step 1: Write failing log callback identity tests**

Cover sanitizer selector, sanitizer, log callback and snapshot finalizer panics. Assert canonical factory, `PhaseLog` or `PhaseFinalizer` and non-empty stack. Preserve the existing security policy: selector or sanitizer error/panic fails closed and no log or snapshot-finalizer callback receives the possibly unsanitized snapshot; log callback and snapshot-finalizer panic remains bounded and does not skip later callbacks. Remove assertions tied to the old `"callback panic: ..."` string.

```go
func TestLogCallbackPanicUsesCanonicalFactory(t *testing.T) {
	binding := LogBinding{Factory: "http-logger", Plugin: panicLogger{}}
	failures := runLogBindings(requestWithLifecycle(), []LogBinding{binding, recordingBinding()})
	panicErr, ok := failures[0].Err.(*PanicError)
	if !ok || panicErr.Factory != "http-logger" || panicErr.Phase != PhaseLog {
		t.Fatalf("failure = %#v", failures[0])
	}
	if !recordingBindingRan() { t.Fatal("panic skipped later log callback") }
}

func TestSanitizerPanicFailsClosed(t *testing.T) {
	failures := runLogBindings(requestWithLifecycle(), []LogBinding{panicSanitizerBinding(), recordingBinding()})
	if len(failures) != 1 || recordingBindingRan() { t.Fatalf("failures/logger = %#v/%v", failures, recordingBindingRan()) }
}
```

- [ ] **Step 2: Run log tests and confirm RED**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin -run "^(TestLog.*Panic|TestSnapshot.*Panic|TestSanitizer.*Panic)" -count=1'`

Expected: FAIL because `LogBinding` lacks canonical factory and the current recover returns a generic string error.

- [ ] **Step 3: Carry canonical identity into log bindings**

Add `Factory string` to `LogBinding` and populate it from `Binding.Descriptor.Factory` in `NewLogExecutorFromBindings`. Copy the frozen private `binding.logPolicy` produced by Task3 and require `binding.logPolicySet` for every resolved binding; never call `LogCapturePolicy()` for that production path. For unresolved legacy/test bindings, resolve the provider once in constructor setup and store the value before callbacks can run. For direct `NewLogExecutor` callers with an empty factory, resolve `Plugin.GetName()` once during constructor validation and store the result; reject an empty result. Request-time failure handling must never call plugin `GetName()` or `LogCapturePolicy()`.

- [ ] **Step 4: Replace generic recover with Task1 guards**

Use `PhaseLog` for selector, sanitizer and log callback, and `PhaseFinalizer` for snapshot finalizer. Return `*PanicError` as the failure error. Selector/sanitizer failure returns immediately before any logger sees a snapshot; log and snapshot-finalizer loops retain their current continue-after-failure behavior.

Register the composite through `AddCoreInvariantFinalizer("log-executor", ...)`, because snapshot creation, sorting and clone orchestration are core-owned. Raw panic from that orchestration therefore becomes delayed fatal. A guarded callback panic returns `*PanicError` as the composite's ordinary error; Task7 recognizes it with `errors.As`, records owner `plugin_finalizer`, logs canonical factory/phase and does not re-panic it. Add a test proving raw composite orchestration panic is fatal while guarded callback panic stays bounded.

- [ ] **Step 5: Run log executor tests**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin -run "^(TestLog|TestSnapshot|TestSanitizer|TestPluginPhaseClosure)" -count=1'`

Expected: PASS; failure strings are stable and raw panic values remain available only through typed internal fields.

- [ ] **Step 6: Commit log boundaries**

```bash
git add pkg/plugin/log_executor.go pkg/plugin/log_executor_test.go
git commit -m "refactor(runtime): attribute log callback panics"
```

---

### Task 7: Make Route Recovery Type-Directed and Metrics Bounded

**Files:**
- Modify: `pkg/server/route_handler.go`
- Modify: `pkg/server/route_handler_test.go`
- Modify: `pkg/server/request_body_limit.go`
- Modify: `pkg/server/request_body_limit_test.go`
- Modify: `pkg/observability/metrics/request_panic.go`
- Modify: `pkg/observability/metrics/request_panic_test.go`

**Interfaces:**
- Consumes: `*plugin.PanicError`, `ctx.FinalizationResult`, `ctx.ResponseOutcome`, response capture and generation lease cleanup.
- Produces: request-local plugin recovery, delayed unchanged core re-panic and `RecordRequestPanic(owner, stage)` with validated enums.

- [ ] **Step 1: Replace old raw-panic expectations with failing typed-policy tests**

Change the old pre-commit 500 and post-write/flush/hijack tests to raise `*plugin.PanicError`. Add raw core tests before commit and after write/flush/hijack, body-limit 413 followed by core panic, stable-500 writer panic, canonical-413 writer panic, generation lease balance, finalizer order, request-state recycle and primary core panic precedence. Writer-panic cases prove finalizers and lease release complete before the writer's original panic escapes.

```go
func TestUnknownRouteInvariantPanicEscapesAfterCleanup(t *testing.T) {
	want := &struct{ message string }{message: "core invariant"}
	var finalized atomic.Int32
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(want) })
	defer func() {
		if got := recover(); got != want { t.Fatalf("panic = %#v, want original", got) }
		if finalized.Load() != 1 { t.Fatalf("finalizers = %d", finalized.Load()) }
		assertGenerationLeaseBalanced(t)
		assertRequestStateRecycled(t)
	}()
	serveRouteRequestWithCoreObserver(t, handler, &finalized)
}
```

- [ ] **Step 2: Write failing bounded metric tests**

Define owner values `plugin`, `core`, `plugin_finalizer`, `core_finalizer` and retain response stages `pre_commit`, `post_commit`, `post_flush`, `post_hijack`, `finalizer`. Prove invalid owner or stage produces no series, and factory/raw panic text never appears as a label.

- [ ] **Step 3: Run route and metric tests and confirm RED**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server ./pkg/observability/metrics -run "^(TestGuardedPluginPanic|TestUnknownRouteInvariantPanic|TestRouteHandlerPanic|TestRequestBodyLimit.*Panic|TestRequestPanicMetric)" -count=1'`

Expected: FAIL because route recovery still treats every raw panic as recoverable and metrics accept only stage.

- [ ] **Step 4: Implement bounded owner/stage metrics**

Use exact contracts:

```go
type RequestPanicOwner string

const (
	RequestPanicPlugin          RequestPanicOwner = "plugin"
	RequestPanicCore            RequestPanicOwner = "core"
	RequestPanicPluginFinalizer RequestPanicOwner = "plugin_finalizer"
	RequestPanicCoreFinalizer   RequestPanicOwner = "core_finalizer"
)

func RecordRequestPanic(owner RequestPanicOwner, stage RequestPanicStage)
```

The private `CounterVec` labels are exactly `owner` and `stage`; both enums are validated before increment. Update every current caller mechanically.

When recording finalization failures, inspect content as well as registration kind: `errors.As(failure.Err, &panicErr)` is `plugin_finalizer` even when the core-owned log composite returned it as an ordinary error; raw `failure.PanicValue` uses `FinalizerOwnerPlugin` versus `FinalizerOwnerCoreInvariant`. This prevents guarded log/snapshot panic from being counted as a core invariant.

- [ ] **Step 5: Implement route recovery precedence**

In the one outer defer:

1. Capture the primary recovered value.
2. Classify exact `http.ErrAbortHandler`, `*plugin.PanicError`, or unknown core before the body-limit canonical-response branch.
3. Apply stable 500/abort policy only to `*plugin.PanicError`; never write a synthetic response for unknown core. Invoke canonical 413 and stable 500 writers through `captureCleanupPanic(func()) any` so a response-writer panic is retained without escaping the cleanup defer early.
4. Publish `lifecycle.Complete`, call `FinalizeResult`, record bounded failures, recycle request state and close every failed hijack. Isolate each connection `Close` panic/error so one connection cannot skip the rest; the first raw close panic is another core cleanup candidate.
5. Re-panic in this precedence: primary unknown core; first core panic raised while writing canonical 413/stable 500 or closing hijacks; core finalizer fatal; plugin post-commit `http.ErrAbortHandler`; exact original handler abort.

Change `writeStableInternalError` to return `(ok bool, panicValue any)` rather than swallowing the value. In `requestBodyLimitState.writeCanonicalResponse`, use a defer to clear `canonicalizing` even when the writer panics, then re-panic for the route cleanup helper to capture. A panic while the server writes either canonical response is core and must not be relabeled as the original plugin owner.

- [ ] **Step 6: Run route normal and race tests**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server ./pkg/observability/metrics -run "^(TestGuardedPluginPanic|TestUnknownRouteInvariantPanic|TestRouteHandlerPanic|TestRequestBodyLimit.*Panic|TestPluginPhaseClosure|TestGeneration.*Lease|TestRequestPanicMetric)" -count=1 && go test -race ./pkg/server -run "^(TestGuardedPluginPanic|TestUnknownRouteInvariantPanic|TestRouteHandlerPanic|TestRequestBodyLimit.*Panic|TestPluginPhaseClosure|TestGeneration.*Lease)" -count=1'`

Expected: PASS; raw values escape by identity only after all request cleanup, and typed post-commit plugin panic never writes twice.

- [ ] **Step 7: Commit server policy and metrics**

```bash
git add pkg/server/route_handler.go pkg/server/route_handler_test.go pkg/server/request_body_limit.go pkg/server/request_body_limit_test.go pkg/observability/metrics/request_panic.go pkg/observability/metrics/request_panic_test.go
git commit -m "refactor(runtime): separate plugin and invariant panics"
```

---

### Task 8: Stop Batch Subrequests From Swallowing Core Panics

**Files:**
- Modify: `pkg/plugin/batch_requests/plugin.go`
- Modify: `pkg/plugin/batch_requests/plugin_test.go`

**Interfaces:**
- Consumes: Task7 route behavior and exact `http.ErrAbortHandler` control flow.
- Produces: bounded batch abort for handler abort, unchanged escape for unknown core panic; goroutine ownership remains Task11.

- [ ] **Step 1: Write failing batch panic classification tests**

Assert `http.ErrAbortHandler` still becomes `abortResponse`. Use a subprocess fixture for the pointer-valued raw core panic and assert a non-zero process exit plus a marker written before the panic; this proves it is not converted into an ordinary batch item without crashing the parent test process.

```go
func TestBatchSubrequestCorePanicTerminatesSubprocess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestBatchSubrequestCorePanicHelper$")
	cmd.Env = append(os.Environ(), "APISIX_BATCH_CORE_PANIC_HELPER=1")
	output, err := cmd.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("core-panic-started")) {
		t.Fatalf("subprocess error/output = %v/%s", err, output)
	}
}
```

- [ ] **Step 2: Run batch tests and confirm RED**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/batch_requests -run "^TestBatch.*Panic" -count=1'`

Expected: FAIL because the current catch-all converts every panic to `abortResponse`.

- [ ] **Step 3: Narrow recovery to exact handler abort and preserve core fatality**

The production goroutine converts exact `http.ErrAbortHandler` to the existing bounded abort response. For any other value, re-panic the original value from that goroutine after the defer records no ordinary response. Do not classify `*plugin.PanicError` here: the nested route boundary must already have converted recoverable post-commit plugin panic to `http.ErrAbortHandler`. Task11 will replace the detached goroutine with its request-task owner without changing this fatality rule.

- [ ] **Step 4: Run batch package tests**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/batch_requests -run "^(TestBatch|TestSubrequest)" -count=1 && go test -race ./pkg/plugin/batch_requests -run "^TestBatch.*Panic" -count=1'`

Expected: PASS; ordinary batch response ordering is unchanged and unknown core panic is not serialized as a successful batch result.

- [ ] **Step 5: Commit the batch boundary**

```bash
git add pkg/plugin/batch_requests/plugin.go pkg/plugin/batch_requests/plugin_test.go
git commit -m "fix(runtime): preserve batch invariant panics"
```

---

### Task 9: Run Contract, Race, Lint, Build and Merge Review Gates

**Files:**
- Verify: files changed by Tasks 1-8; this gate creates no source file.

**Interfaces:**
- Consumes: all Task10 commits.
- Produces: fresh callback-site inventory, impact-scoped verification evidence and a merge-reviewed Task10 branch.

- [ ] **Step 1: Re-inventory the runtime callback surface**

Run a production-only call-site inventory for request phase, response selector/eligibility/header/body/store, streaming/compression/protocol, log/snapshot finalizer and before-proxy hooks. Classify each result as an executor dispatch boundary, a metadata adapter already inside that boundary, a plugin-internal compatibility delegation already inside that boundary, or an invalid unguarded dispatch. Any invalid dispatch blocks completion and receives a focused regression test before repair.

```bash
rg -n 'RunRequestPhase\(|SelectResponseMode\(|AppliesToResponseSource\(|Run(Header|BufferedBody)Filter\(|RunFinalResponseStore\(|RunStreamingHeaderFilter\(|WrapStreamingResponse\(|RegisterCompressionOffers\(|WrapCompression\(|RunExclusiveProtocol\(|RunLogPhase\(|FinishStreamingResponse\(' pkg --glob '*.go' --glob '!**/*_test.go'
rg -n '\.Handler\(' pkg --glob '*.go' --glob '!**/*_test.go'
rg -n 'RunBeforeProxyHooks\(|WithBeforeProxyHook' pkg --glob '*.go' --glob '!**/*_test.go'
rg -n 'LogCapturePolicy\(' pkg --glob '*.go' --glob '!**/*_test.go'
! rg -n '\.LogCapturePolicy\(' pkg/plugin/log_executor.go
```

Expected: all top-level runtime dispatches are in the four plugin executors or the owner-bearing before-proxy path; route metadata adapters and self-delegating compatibility handlers are transitively reached only from a guarded outer binding. Unregistered `multi-auth` children remain below its guarded `RunRequestPhase`, and unregistered `workflow` children remain below its guarded legacy `Handler.ServeHTTP`. `LogCapturePolicy()` declarations and materialization adapters may appear in the broad inventory, but the targeted negative scan proves request-time log-executor construction no longer invokes it.

- [ ] **Step 2: Run the focused normal suite**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/apisix/ctx ./pkg/plugin ./pkg/plugin/batch_requests ./pkg/plugin/proxy_mirror ./pkg/server ./pkg/observability/metrics -run "^(TestRequestLifecycle|TestGuard|TestPanicError|TestRequest|TestExecutor|TestBeforeProxy|TestBuffered|TestResponse|TestFinalStore|TestStreaming|TestCompression|TestProtocol|TestExclusive|TestLog|TestSnapshot|TestSanitizer|TestBatch|TestGuardedPluginPanic|TestUnknownRouteInvariantPanic|TestRouteHandlerPanic|TestPluginPhaseClosure|TestGeneration.*Lease|TestRequestPanicMetric)" -count=1 -timeout=300s'`

Expected: PASS.

- [ ] **Step 3: Run the concurrency-sensitive race suite**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/apisix/ctx ./pkg/plugin ./pkg/plugin/batch_requests ./pkg/server -run "^(TestRequestLifecycle|TestStreamingFinish|TestStreaming.*Panic|TestBatch.*Panic|TestGuardedPluginPanic|TestUnknownRouteInvariantPanic|TestRouteHandlerPanic|TestPluginPhaseClosure|TestGeneration.*Lease)" -count=1 -timeout=300s'`

Expected: PASS.

- [ ] **Step 4: Run format, selected lint, build and diff gates**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && git diff --name-only --diff-filter=ACMR master...HEAD -- "*.go" | xargs golangci-lint fmt'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/apisix/ctx/... ./pkg/plugin/... ./pkg/server/... ./pkg/observability/metrics/...'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
git diff --check master...HEAD
```

Expected: zero lint issues, build succeeds and diff check prints nothing. Inspect formatter changes and discard any unrelated rewrite before continuing.

- [ ] **Step 5: Run invariant absence scans**

Run:

```bash
! rg -n 'recover\(\)' pkg/compiler pkg/generation pkg/runtime/resource_registry.go pkg/route/compiler.go --glob '*.go' --glob '!**/*_test.go'
test "$(rg -l '\.AddCoreInvariantFinalizer\(' pkg/plugin --glob '*.go' --glob '!**/*_test.go')" = "pkg/plugin/log_executor.go"
! rg -n 'callback panic:' pkg/plugin --glob '*.go'
rg -n 'PanicError|guard(Call|Value|Middleware)' pkg/plugin pkg/server --glob '*.go'
```

Expected: both negative scans print nothing; the exact-authority check names only `pkg/plugin/log_executor.go`; the positive scan lists only explicit Task10 boundaries and their tests.

- [ ] **Step 6: Request an independent merge-level review**

The reviewer checks the full `master...HEAD` diff for downstream misattribution, optional writer-interface inflation, second-response writes, finalizer ordering/once guarantees, original panic precedence, generation lease balance, batch catch-all recovery and bounded metric labels. Fix only confirmed Task10 findings, add a regression test first, and rerun the smallest affected gate plus Steps 2-5.

- [ ] **Step 7: Record the current process-fatal limitation in the Task10 handoff**

State exactly: an ordinary HTTP unknown core panic escapes `routeHandler` after cleanup, but the standard library `net/http` connection goroutine still recovers it; process-level fail-stop for that path remains for the supervisor/worker program. The existing detached batch subrequest goroutine differs temporarily: re-panicking an unknown core value there terminates the current process, as proved by the subprocess test, until Task11 moves it under the request-task owner. Do not add `os.Exit`, a process-global panic channel or a new detached goroutine in Task10.

- [ ] **Step 8: Fast-forward local master only after review readiness**

Verify the main checkout still contains only the four user-owned untracked review documents, fast-forward local `master` to the reviewed Task10 head, rerun `git status --short --branch`, clean only the Task10 worktree-local cache, and remove the Task10 worktree. Do not push or create a PR.
