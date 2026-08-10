# Plugin Request-Phase Bridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Introduce an explicit request-phase contract that can coexist with legacy middleware in one priority-ordered chain, then migrate the system request context, request ID, and connection-limit cleanup without changing remaining plugin behavior.

**Architecture:** Phase interfaces live in `pkg/plugin/base`, not the parent registry package, so child plugin packages can implement them without an import cycle. `pkg/plugin` builds a recursive mixed executor: explicit request nodes run once and continue by result, while legacy nodes still receive the recursively built `next` handler and retain their current enter/unwind semantics. This is a compatibility bridge only; global/route/consumer scope separation and response/log phases remain later plans.

**Tech Stack:** Go 1.26 `net/http`, existing plugin registry and priorities, request lifecycle from `2026-08-10-plugin-panic-outcome-foundation.md`.

## Global Constraints

- Depends on the merged panic/outcome foundation and consumes its exact `ctx.RequestLifecycle` LIFO finalizer contract.
- Do not import `pkg/plugin` from any `pkg/plugin/<child>` package. All child-implementable phase types belong in `pkg/plugin/base`.
- Preserve the current total priority order across explicit and legacy plugins; do not group scopes or phases yet.
- A legacy middleware must receive the recursive remainder of the mixed chain as `next`; never execute all explicit nodes before all legacy nodes.
- Explicit request plugins must not call downstream code. They return `Continue` or `Stop`; a stopped plugin owns any response it wrote.
- Request replacement must be explicit in the result so context/header mutations reach later nodes.
- `_meta.filter`, `_meta.error_response`, and `_meta.priority` must behave identically for migrated and legacy plugins. A false filter returns `Continue` without invoking the underlying phase.
- Keep `Plugin.Handler` during migration. Direct unit tests and unconverted callers remain source compatible.
- `request-context` registers first and its lifecycle finalizer runs last. `limit-conn` registers release hooks after admission, so they run before request variables are recycled on success, rejection, panic, and `http.ErrAbortHandler`.
- This PR does not claim APISIX global/route/consumer phase parity and does not close PR-014.

---

### Task 1: Define request-phase result types in the child-safe base package

**Files:**
- Create: `pkg/plugin/base/request_phase.go`
- Create: `pkg/plugin/base/request_phase_test.go`
- Modify: `pkg/apisix/ctx/lifecycle.go`
- Modify: `pkg/apisix/ctx/lifecycle_test.go`

**Interfaces:**
- Produces:

```go
type RequestDecision uint8

const (
    RequestContinue RequestDecision = iota
    RequestStop
)

type RequestPhaseResult struct {
    Request  *http.Request
    Decision RequestDecision
    Source   apisixctx.ResponseSource
}

func ContinueRequest(r *http.Request) RequestPhaseResult
func StopRequest(r *http.Request) RequestPhaseResult
func StopRequestWithSource(r *http.Request, source apisixctx.ResponseSource) RequestPhaseResult

type RequestPhasePlugin interface {
    RunRequestPhase(http.ResponseWriter, *http.Request) RequestPhaseResult
}

func AdaptRequestPhase(plugin RequestPhasePlugin, next http.Handler) http.Handler

// The following declarations live in pkg/apisix/ctx.
type ResponseSource string

const (
    ResponseSourceUnknown   ResponseSource = "unknown"
    ResponseSourceUpstream  ResponseSource = "upstream"
    ResponseSourceEarlyStop ResponseSource = "early_stop"
    ResponseSourceCacheHit  ResponseSource = "cache_hit"
)

func (*RequestLifecycle) SetFinalRequest(*http.Request)
func (*RequestLifecycle) FinalRequest() *http.Request
func (*RequestLifecycle) SetResponseSource(ResponseSource)
func (*RequestLifecycle) ResponseSource() ResponseSource
```

`ResponseSource` and the final-request holder live in `pkg/apisix/ctx/lifecycle.go`; the snippet is grouped here because later response/log plans consume them. If an implementation returns `Request=nil`, the adapter retains its input request. Every replacement updates `FinalRequest`. `StopRequest` defaults to `early_stop`; cache lookup later uses `StopRequestWithSource(..., cache_hit)`. The terminal wrapper marks `upstream` only when no earlier owner selected a source. Unknown decisions fail closed as `RequestStop`. The adapter owns continuation only; it does not create/finalize a lifecycle or synthesize cleanup.

- [ ] **Step 1: Write result invariant tests**

Add `TestRequestPhaseResultConstructors`, `TestAdaptRequestPhasePropagatesReplacement`, `TestAdaptRequestPhaseRecordsEarlyStopSource`, `TestAdaptRequestPhaseStops`, `TestAdaptRequestPhaseUnknownDecisionStops`, plus lifecycle tests proving final-request/source updates are race-safe.

- [ ] **Step 2: Run the focused test and record the expected red failure**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base -run "^TestRequestPhase" -count=1'
```

- [ ] **Step 3: Implement the exact types and constructors**

Keep this file independent of registry, route, metrics, and resource packages.

- [ ] **Step 4: Run base tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base -run "^TestRequestPhase" -count=1'
```

### Task 2: Build a priority-preserving mixed executor

**Files:**
- Create: `pkg/plugin/executor.go`
- Create: `pkg/plugin/executor_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`

**Interfaces:**
- Consumes: `base.RequestPhasePlugin` and the existing `Plugin` interface.
- Produces:

```go
type Executor struct { /* immutable sorted plugin slice and transform count */ }

func NewExecutor(plugins ...Plugin) Executor
func (e Executor) Then(terminal http.Handler) http.Handler
```

Keep `BuildPluginChain` as the current legacy compatibility API. Route/global/consumer production assembly moves explicitly to `NewExecutor`; direct parity tests and external callers that intentionally exercise a legacy Alice chain do not silently acquire the new lifecycle requirement.

- [ ] **Step 1: Write mixed-order regressions first**

Create synthetic plugins that append markers to a request-owned slice. Cover:

```go
func TestExecutorMixedRequestAndLegacyPreservesPriorityAndUnwind(t *testing.T)
func TestExecutorStopsWithoutCallingRemainder(t *testing.T)
func TestExecutorPropagatesReplacementRequest(t *testing.T)
func TestExecutorTreatsUnknownDecisionAsStop(t *testing.T)
func TestExecutorDoesNotMutateCallerSlice(t *testing.T)
```

For priorities `explicit-high=300`, `legacy=200`, `explicit-low=100`, require entry order `explicit-high, legacy-enter, explicit-low, terminal, legacy-exit`. This exact assertion prevents a two-bucket implementation.

- [ ] **Step 2: Run the executor tests and record the red failure**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "^TestExecutor" -count=1'
```

Expected: compile failure for missing `NewExecutor`.

- [ ] **Step 3: Implement the recursive executor**

After sorting a cloned slice by descending priority, build a private recursive function equivalent to:

```go
handler := terminal
for index := len(sorted) - 1; index >= 0; index-- {
    current := sorted[index]
    if phase, ok := current.(base.RequestPhasePlugin); ok {
        handler = base.AdaptRequestPhase(phase, handler)
        continue
    }
    handler = current.Handler(handler)
}
return base.WithTransformPipeline(transformCount)(handler)
```

The production code must handle unknown decisions as stop. `NewExecutor` clones and sorts the caller slice; `Then` retains `base.WithTransformPipeline(transformCount)` until response plugins migrate.

- [ ] **Step 4: Run plugin package tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "(Executor|BuildPluginChain|NewConstructs)" -count=1'
```

### Task 3: Preserve `_meta` behavior for explicit request plugins

**Files:**
- Modify: `pkg/route/builder.go`
- Modify: `pkg/route/builder_lifecycle_test.go`
- Modify: `pkg/route/consumer_plugin_test.go`
- Create: `pkg/route/request_phase_metadata_test.go`

**Interfaces:**
- `consumerPluginChains` stores `plugin.Executor`, not `alice.Chain`.
- `assembleRoutePluginChain` and `withAIExecutionTerminal` accept/return the executor boundary without separating scopes yet.
- Add four concrete wrappers; do not make one wrapper conditionally satisfy a Go interface:

```go
type metadataPlugin struct { Plugin; /* legacy metadata */ }
type metadataRequestPlugin struct { Plugin; phase base.RequestPhasePlugin; /* explicit metadata */ }
type routeConsumerOverridePlugin struct { Plugin }
type routeConsumerOverrideRequestPlugin struct { Plugin; phase base.RequestPhasePlugin }
```

Go interface implementation is static. If `metadataPlugin` or `routeConsumerOverridePlugin` unconditionally implements `RequestPhasePlugin`, a wrapped legacy plugin would be misclassified as explicit and its downstream behavior would be lost.

- [ ] **Step 1: Add metadata regressions**

Add table cases for a migrated synthetic/request-id plugin:

- `_meta.filter` false skips the plugin and continues;
- `_meta.filter` true executes it;
- `_meta.priority` reorders it across a legacy plugin;
- `_meta.error_response` replaces a migrated plugin's 4xx/5xx response exactly once;
- a stopped migrated plugin never calls upstream.
- a consumer explicit plugin overrides the same route plugin exactly once;
- stacked auth and `multi-auth` still trigger the cached consumer executor only after real authentication succeeds.

Name the table `TestRequestPhaseMetadataContract`.

- [ ] **Step 2: Run the metadata test and record the red behavior**

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "^TestRequestPhaseMetadataContract$" -count=1'
```

Expected before adaptation: wrappers hide `base.RequestPhasePlugin`, consumer cache still stores `alice.Chain`, and metadata behavior cannot be applied at an explicit stop boundary.

- [ ] **Step 3: Implement phase-aware metadata delegation**

Select the concrete legacy or request wrapper at initialization time. Factor filter evaluation and terminal error-response capture so both wrapper families use one behavior owner. `metadataRequestPlugin` may capture only the wrapped phase's own response; it must not capture downstream. A consumer override returns `Continue` without calling the route phase when the consumer owns the same name. Replace route/global/consumer production assembly with `plugin.NewExecutor` while keeping their current combined priority ordering.

- [ ] **Step 4: Run focused route tests**

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "(RequestPhaseMetadata|BuilderLifecycle|PluginMeta)" -count=1'
```

### Task 4: Migrate request-context initialization and request-id

**Files:**
- Modify: `pkg/plugin/request_context/plugin.go`
- Modify: `pkg/plugin/request_context/plugin_test.go`
- Modify: `pkg/plugin/request_id/plugin.go`
- Modify: `pkg/plugin/request_id/plugin_test.go`

**Interfaces:**
- Both plugins implement `base.RequestPhasePlugin`. `request-id` retains `Handler` as:

```go
func (p *Plugin) Handler(next http.Handler) http.Handler {
    return base.AdaptRequestPhase(p, next)
}
```

The adapter executes one phase call and invokes `next` only for `RequestContinue`. `request-context.Handler` preserves the foundation PR's direct-call fallback: when no outer lifecycle exists it creates/finalizes a local lifecycle and recycles state; production route execution already has an outer lifecycle and uses only `RunRequestPhase`.

- [ ] **Step 1: Add dual-entry compatibility tests**

For each plugin, run the same behavior table once through `RunRequestPhase` and once through `Handler(next)`. Assert identical request headers/context, response headers, terminal status, and lifecycle registration.

- [ ] **Step 2: Run tests and record the red compile failure**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/request_context ./pkg/plugin/request_id -run "(RequestPhase|HandlerCompatibility)" -count=1'
```

- [ ] **Step 3: Extract pre-next logic into `RunRequestPhase`**

`request-context` initializes vars and registers the already-defined lifecycle finalizer, then returns the updated request. `request-id` generates/propagates the ID, response header, and context value, then returns the updated request. Error generation writes the existing 500 and returns stop.

- [ ] **Step 4: Run full affected plugin package tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/request_context ./pkg/plugin/request_id -count=1'
```

### Task 5: Move limit-conn release to the request lifecycle

**Files:**
- Modify: `pkg/plugin/limit_conn/plugin.go`
- Modify: `pkg/plugin/limit_conn/plugin_test.go`

**Interfaces:**
- `limit_conn.Plugin` implements `base.RequestPhasePlugin`.
- Successful admission registers one lifecycle finalizer that releases the exact admitted key/rule set and calculates latency from the captured admission time.

- [ ] **Step 1: Write cleanup regressions first**

Add:

```go
func TestRequestPhaseLimitConnReleasesAfterNormalCompletion(t *testing.T)
func TestRequestPhaseLimitConnReleasesAfterDownstreamPanic(t *testing.T)
func TestRequestPhaseLimitConnReleasesOnceAfterAbortHandler(t *testing.T)
func TestRequestPhaseLimitConnFinalizesBeforeRequestStateRecycle(t *testing.T)
func TestRequestPhaseLimitConnDegradationDoesNotRegisterRelease(t *testing.T)
```

The ordering test registers a request-context recycle marker first, then admits limit-conn, finalizes, and asserts release observed request vars before recycle.

- [ ] **Step 2: Run the focused tests and record the red behavior**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/limit_conn -run "^TestRequestPhaseLimitConn" -count=1'
```

Expected: compile failure before the interface exists or missing release after simulated panic.

- [ ] **Step 3: Implement admission as a request phase**

Preserve all current reject/degradation messages and delay semantics. Replace the production-path `defer decrease...` blocks with lifecycle finalizer registration after successful admission. `AdaptRequestPhase` remains lifecycle-neutral. When `limit_conn.Handler` is called directly without an outer lifecycle, it retains a package-owned local `defer release` compatibility path; this is required by workflow/direct unit callers and must have its own panic regression.

- [ ] **Step 4: Run limit-conn tests and focused race**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/limit_conn -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/limit_conn -run "(RequestPhaseLimitConn|HandlerRejectsConcurrent|Decrease)" -count=3'
```

### Task 6: Combined verification and independent PR delivery

**Files:**
- Include: `docs/superpowers/plans/2026-08-10-plugin-request-phase-bridge.md`

- [ ] **Step 1: Scan mixed-chain call sites and compatibility boundaries**

```bash
rg -n 'BuildPluginChain|NewExecutor|assembleRoutePluginChain|consumerPluginChains|consumerPluginChainForIdentity' pkg cmd t
```

Confirm legacy callers intentionally remain on `BuildPluginChain`, production route/global/consumer caches use `plugin.Executor`, no stale `alice.Chain` cache remains, and no proxy-only adapter was introduced.

- [ ] **Step 2: Run affected tests and race tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/plugin/base ./pkg/plugin/request_context ./pkg/plugin/request_id ./pkg/plugin/limit_conn ./pkg/route -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin ./pkg/plugin/request_context ./pkg/plugin/limit_conn ./pkg/route -run "(MixedPluginChain|RequestPhase|MetadataContract|LimitConn)" -count=3'
```

- [ ] **Step 3: Run scoped lint/build/diff gates**

```bash
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/... ./pkg/route/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 4: Independent review**

The reviewer must verify recursive priority ordering, legacy unwind, stop behavior, request replacement, metadata parity, lifecycle LIFO ordering, and exact-once limit release. Any High/Medium finding requires one bounded regression-first follow-up.

- [ ] **Step 5: Deliver one PR**

Commit only the plan and accepted implementation with:

```bash
git commit -m "refactor(plugin): add explicit request-phase bridge"
```

Open a ready PR against current `master`, wait for required CI, and merge before beginning the scoped phase executor plan.

## Fast-plan-impl Dispatch Ownership

1. **WU-01 lifecycle extension, base, and mixed executor** owns `pkg/apisix/ctx/lifecycle*`, `pkg/plugin/base/request_phase*`, `pkg/plugin/executor*`, and `pkg/plugin/init*`; its interface is accepted first.
2. **WU-02 route wrappers and caches** owns only `pkg/route/**` files named in Task 3 and starts after WU-01.
3. **WU-03 migrated request plugins** owns `request_context`, `request_id`, and `limit_conn` production/tests; it starts after WU-01 and may run parallel with WU-02. No worker commits, pushes, opens a PR, or edits another unit's paths.

## Explicit Deferrals

- Global, route, consumer, rewrite, and access grouping remains unchanged in this compatibility bridge.
- Authentication/consumer resolution remains on `ctx.RunConsumerPlugins` until the access/consumer plan.
- Response, streaming, terminal, log, and tracer plugins remain legacy middleware.
- The explicit registry completeness gate belongs to the final migration plan, after every non-request capability has a stable interface.
