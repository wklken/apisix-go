# GC Hot-Path P1 Static Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove binding merge, log materialization, handler sorting/wrapping, and streaming-continuation construction from the common unresolved static request path.

**Architecture:** `ThenWithPostResolutionHook` prepares one immutable static binding set and post-resolution handler at route-generation build time. `runResolved` selects it only when the resolver reports `Resolved=false` with no dynamic bindings; resolved consumer/group requests retain the existing dynamic path.

**Tech Stack:** Go 1.26, `net/http`, existing immutable `RequestPipeline`, focused behavior tests, repository benchmarks.

**Spec:** `docs/superpowers/specs/2026-08-18-gc-hotpath-p0-p1.md`

## Implementation Outcome

The unresolved static handler is prepared once and selected with the exact
`Resolved=false`, zero-dynamic-bindings, no-buffered-execution predicate. The
post-resolution hook still runs for every request.

The proposed `buildPostResolutionHandler` invariant hoisting and broad
partition-only streaming traversal were implemented and measured, then
rejected: the consumer-resolved benchmark regressed by more than the declared
10% CPU threshold and briefly added one allocation per request. Those broad
production changes were removed. A narrower change avoids `effective.all()`
only while selecting dynamic header bindings; the post-resolution validation
path retains its previous traversal and error semantics. In the final
production-shaped benchmark, the consumer-resolved row kept baseline CPU while
reducing `B/op` by 1.29% and `allocs/op` from 128 to 127.
Against the refreshed `master` baseline, the static unresolved row reduced
`time/op` by 9.14% to 12.35%, `B/op` by 10.39%, and `allocs/op` from 122 to 108.

## Global Constraints

- Run every Go command as `bash -lc 'source .envrc && ...'`.
- Do not change plugin ordering, response modes, authentication, consumer overrides, CORS, logging, streaming, or panic behavior.
- Do not add a dynamic cache, pool, dependency, configuration field, or goroutine.
- Keep the dynamic path for every `ConsumerResolution{Resolved:true}`, even when `Bindings` is empty.
- The current fast-plan-impl run has local-mutation authority only; commit commands are handoff commands.

---

## File Map

- Modify `pkg/plugin/executor.go`: prepare and select the static plan; prebuild streaming continuation and dynamic-header selection.
- Modify `pkg/plugin/executor_test.go`: prove one-time handler construction, hook execution, resolved fallback, and ordering.
- Modify `pkg/plugin/streaming_executor.go`: inspect effective partitions without materializing `effective.all()` when selecting dynamic header bindings; leave post-resolution validation unchanged.
- Modify `pkg/plugin/streaming_executor_test.go`: preserve dynamic-header validation and request replacement behavior.

### Task 1: Prove the static/dynamic selection contract

**Files:**
- Modify: `pkg/plugin/executor_test.go`

**Interfaces:**
- Produces behavioral tests for the unchanged `RequestPipeline.Then` API.
- Consumes the P0 `BenchmarkRequestPipelineHotPath` row for performance acceptance.

- [ ] **Step 1: Add a handler-construction counter plugin**

```go
type constructionCountingPlugin struct {
	base.BasePlugin
	constructed atomic.Int64
	served      atomic.Int64
}

func (p *constructionCountingPlugin) Handler(next http.Handler) http.Handler {
	p.constructed.Add(1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.served.Add(1)
		next.ServeHTTP(w, r)
	})
}
```

Bind it as a route-scoped legacy plugin with a stable test factory name and serve two unresolved requests. Assert `constructed == 1`, `served == 2`, and terminal calls equal two.

- [ ] **Step 2: Add hook and resolved-empty regressions**

Add one test whose resolver returns unresolved/no bindings and whose hook increments a counter and replaces the request; assert the hook runs for every request and the terminal receives the replacement.

Add one test whose resolver returns `Resolved:true`, no bindings, and a replacement request; assert it takes the dynamic path and preserves the replacement. The test must fail if selection checks only `len(Bindings)==0`.

- [ ] **Step 3: Run the tests and observe the current failure**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "^TestRequestPipeline(PrebuildsStaticHandler|StaticHookPerRequest|ResolvedEmptyUsesDynamicPath)$" -count=1'
```

Expected before implementation: the construction-count assertion reports more than one handler construction.

### Task 2: Prebuild and select the static post-resolution handler

**Files:**
- Modify: `pkg/plugin/executor.go:160-527`
- Test: `pkg/plugin/executor_test.go`

**Interfaces:**
- Produces private `preparedStaticPipeline{effective EffectiveBindingSet, handler http.Handler}`.
- Changes private `runResolved` arguments to accept the prepared static pipeline.
- Public `RequestPipeline` APIs remain unchanged.

- [ ] **Step 1: Prepare static state once in `ThenWithPostResolutionHook`**

After normalizing `terminal`, construct:

```go
type preparedStaticPipeline struct {
	effective EffectiveBindingSet
	handler   http.Handler
}

staticEffective := mergeEffectiveBindingSet(p.bindings, nil)
preparedStatic := preparedStaticPipeline{
	effective: staticEffective,
	handler:   p.buildPostResolutionHandler(staticEffective, terminal, nil),
}
```

Pass it into both plain and buffered resolver boundaries. The buffered path may use only `effective`; its request-local `responseExecution` still requires a request-local handler.

- [ ] **Step 2: Add the exact safe fast-path predicate**

Inside `runResolved`, after resolving the request:

```go
usePreparedStatic := !resolution.Resolved && len(resolution.Bindings) == 0 && execution == nil
```

For this branch, use `prepared.effective`, do not call `mergeEffectiveBindingSet`, do not reconstruct `LogExecutor`, and serve `prepared.handler` after the hook. The outer static `LogExecutor.Prepare` has already installed the static log state. All errors and hook request replacement behavior remain identical.

For every other branch, keep the existing merge, dynamic log materialization, hook, and handler construction.

- [ ] **Step 3: Precompute closure invariants in `buildPostResolutionHandler`**

**Measured outcome:** rejected and reverted because the resolved consumer row
exceeded the 10% CPU regression threshold and added one allocation per request.

Before creating `boundary`, compute:

```go
bindings := effective.all()
dynamicHeaders := dynamicHeaderBindingsForEffective(effective)
var streamingContinuation http.Handler
if p.streamingExecutor != nil {
	streamingContinuation = p.streamingExecutor.Then(terminalHandler(terminal))
}
```

The request closure reuses `dynamicHeaders` and `streamingContinuation`. `RunExclusiveProtocol` remains request-local because it owns request state.

- [ ] **Step 4: Run focused behavior tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "^TestRequestPipeline|^TestStreamingResponseExecutor" -count=1'
```

Expected: new tests and all existing request-pipeline/streaming tests pass.

### Task 3: Remove dynamic-header binding materialization

**Measured outcome:** broad partition traversal in the post-resolution hook was
rejected and reverted because the combined resolved path failed the performance
acceptance threshold. The narrower dynamic-header selection change was retained:
it preserved CPU and reduced the consumer-resolved benchmark by 1.29% `B/op`
and one allocation per request.

**Files:**
- Modify: `pkg/plugin/streaming_executor.go:200-250`
- Modify: `pkg/plugin/streaming_executor_test.go`

**Interfaces:**
- Consumes private `EffectiveBindingSet.global` and `.merged` partitions.
- Produces no public API change.

- [x] **Step 1: Iterate both partitions without `effective.all()` for dynamic-header selection**

Use an index loop that does not allocate a combined slice:

```go
total := len(effective.global) + len(effective.merged)
for index := 0; index < total; index++ {
	binding := Binding{}
	if index < len(effective.global) {
		binding = effective.global[index]
	} else {
		binding = effective.merged[index-len(effective.global)]
	}
	// preserve the existing consumer/group validation and header selection
}
```

Allocate `dynamicHeaders` lazily only after a consumer/group header filter is found.
Keep `PostResolutionHook` on `effective.all()` because its full validation path
was part of the rejected broad change.

- [x] **Step 2: Add partition coverage**

Add a test with global, route, consumer, and consumer-group bindings proving only supported consumer/group header filters are attached and unsupported dynamic response capabilities still fail with the same error.

- [ ] **Step 3: Run focused tests and race gate**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "^(TestRequestPipeline|TestStreamingResponseExecutor)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin -run "^(TestRequestPipeline|TestStreamingResponseExecutor)" -count=1'
```

Expected: PASS with no race report.

- [ ] **Step 4: Prepare the commit command**

Do not execute without commit authority:

```bash
git add pkg/plugin/executor.go pkg/plugin/executor_test.go pkg/plugin/streaming_executor.go pkg/plugin/streaming_executor_test.go
git commit -m "perf(plugin): prebuild static request pipeline"
```
