# Plugin Scoped Rewrite Executor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute audited rewrite-only plugins in APISIX scope order—system setup, global rewrite, then route rewrite—while leaving every unclassified legacy middleware in its current priority/unwind chain.

**Architecture:** Extend the mixed executor with immutable `Binding` records carrying source scope and a bounded request stage. A registry-owned adapter may convert a legacy Handler only when a static audit proves it performs request-side work before `next` and has no post-`next`, buffering, logging, tracing, cleanup, streaming, or hijack behavior. Explicit rewrite bindings are removed from the legacy remainder and run scope-by-scope; unclassified plugins keep the exact mixed-chain behavior from the previous PR.

**Tech Stack:** Go 1.26, request-phase bridge, route builder resource provenance, pinned APISIX 3.17 phase contract.

## Global Constraints

- Depends on the merged request-phase bridge.
- The upstream contract for this PR is global-rule `rewrite` before route/service/plugin-config `rewrite`; within each scope priority is descending.
- Do not move authentication or consumer execution. Auth plugins remain in the legacy remainder until the consumer/access plan.
- Do not move access guards, response transforms, compression, streaming, logger, tracer, or finalizer plugins.
- The adapter allowlist is exact and static. A plugin absent from it is legacy; unknown registry names fail the completeness test rather than defaulting to rewrite.
- `_meta.priority`, `_meta.filter`, `_meta.error_response`, route consumer override, system `request-context`, and plugin enablement checks remain effective.
- Orphan services/plugin-configs remain inert exactly as in the strict builder contract; scope bindings are created only for materialized route generations.
- A migrated rewrite plugin may replace the request and may stop. If it stops, no lower rewrite, legacy middleware, before-proxy hook, or upstream executes; outer panic/finalizer handling still runs.
- This PR is a partial PR-014 migration. Do not claim access, consumer, response, or log parity.

---

### Task 1: Add immutable scope and stage bindings

**Files:**
- Modify: `pkg/plugin/executor.go`
- Modify: `pkg/plugin/executor_test.go`

**Interfaces:**

```go
type Scope uint8

const (
    ScopeSystem Scope = iota
    ScopeGlobal
    ScopeRoute
    ScopeConsumer
)

type RequestStage uint8

const (
    RequestStageLegacy RequestStage = iota
    RequestStageRewrite
    RequestStageAccess
)

type Binding struct {
    Plugin     Plugin
    Scope      Scope
    Stage      RequestStage
    Provenance ResourceProvenance
}

type ResourceKind string

const (
    ResourceSystem        ResourceKind = "system"
    ResourceGlobalRule    ResourceKind = "global_rule"
    ResourceRoute         ResourceKind = "route"
    ResourceService       ResourceKind = "service"
    ResourcePluginConfig  ResourceKind = "plugin_config"
    ResourceConsumer      ResourceKind = "consumer"
    ResourceConsumerGroup ResourceKind = "consumer_group"
)

type ResourceProvenance struct {
    Kind ResourceKind
    ID   string
}

func NewScopedExecutor(bindings ...Binding) Executor
```

`NewExecutor(plugins...)` remains and creates legacy bindings for compatibility.

- [ ] **Step 1: Write binding immutability and ordering tests**

Add:

```go
func TestScopedExecutorClonesBindings(t *testing.T)
func TestScopedExecutorPreservesResourceProvenance(t *testing.T)
func TestScopedExecutorRunsGlobalRewriteBeforeHigherPriorityRouteRewrite(t *testing.T)
func TestScopedExecutorStopsRewriteBeforeLegacyRemainder(t *testing.T)
func TestScopedExecutorPropagatesRequestAcrossScopes(t *testing.T)
func TestScopedExecutorLeavesLegacyPriorityAndUnwindUnchanged(t *testing.T)
```

Use a route rewrite at priority 10000 and a global rewrite at priority 1; require global first because scope precedes priority.

- [ ] **Step 2: Run tests and capture compile-red**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "^TestScopedExecutor" -count=1'
```

- [ ] **Step 3: Implement rewrite extraction plus legacy remainder**

Clone bindings including provenance. For each rewrite scope, stable-sort only that scope by descending effective priority and execute through `base.AdaptRequestPhase`. Build the remainder with the prior mixed recursion so legacy enter/unwind behavior is byte-for-byte equivalent. Do not reorder one scope by another scope's priority. Every later build/runtime compatibility error reports `Provenance.Kind` and `Provenance.ID`; never reconstruct provenance from a merged plugin map.

- [ ] **Step 4: Run executor tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "(ScopedExecutor|ExecutorMixed|BuildPluginChain)" -count=1'
```

### Task 2: Define the audited rewrite-only adapter registry

**Files:**
- Create: `pkg/plugin/request_stage_registry.go`
- Create: `pkg/plugin/request_stage_registry_test.go`
- Modify: `pkg/plugin/init_test.go`

**Interfaces:**

```go
type RequestStageSpec struct {
    Stage       RequestStage
    LegacyAudit bool
}

func RequestStageFor(name string) (RequestStageSpec, bool)
func BindPlugin(p Plugin, scope Scope) Binding
```

The audited Plan 13 set is exact and matches the Plan 13 section of `2026-08-10-plugin-capability-manifest.md`:

```text
request-context, request-id, real-ip, proxy-rewrite, proxy-control,
proxy-mirror, traffic-label, traffic-split, ai-prompt-decorator,
ai-prompt-template, ai-rag, ai-request-rewrite, data-mask, degraphql,
example-plugin, jwe-decrypt
```

The registry is keyed by factory name: `request-context`; `request_context` is accepted only when asserting that implementation's `GetName`. `request-context` and `request-id` already implement `base.RequestPhasePlugin`. The remaining names use a private adapter that calls the legacy Handler with a sentinel next, captures the request passed to that next, returns Continue, and returns Stop when next was not called. The adapter must reject double-next. `proxy-mirror` retains its existing before-proxy hook seam; it is not a finalizer.

- [ ] **Step 1: Add registry and adapter red tests**

Cover exact membership, no response/logger/stream plugin membership, request replacement, stop, double-next fail-closed, and registry-name drift.

- [ ] **Step 2: Run focused registry tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "(RequestStageRegistry|RewriteOnlyAdapter)" -count=1'
```

Expected: compile-red for missing registry and adapter.

- [ ] **Step 3: Implement the exact registry**

Use a map keyed by canonical implementation name plus the existing alias handling for `request-context`. Do not infer stage from priority or package name. Add comments naming the audited property: no work after `next`, no deferred cleanup, no response writer wrapper.

- [ ] **Step 4: Run registry plus every adapted package test**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/plugin/{request_context,request_id,real_ip,proxy_rewrite,proxy_control,proxy_mirror,traffic_label,traffic_split,ai_prompt_decorator,ai_prompt_template,ai_rag,ai_request_rewrite,data_mask,degraphql,example_plugin,jwe_decrypt} -count=1'
```

### Task 3: Preserve route provenance as executor bindings

**Files:**
- Modify: `pkg/route/builder.go`
- Create: `pkg/route/scoped_rewrite_test.go`
- Modify: `pkg/route/builder_lifecycle_test.go`

**Interfaces:**
- Route, service, plugin-config, and system instances bind as `ScopeRoute`.
- Global-rule instances bind as `ScopeGlobal`.
- System request-context binds as `ScopeSystem` and executes before global rewrite.
- Consumer remains legacy in this PR.

- [ ] **Step 1: Add route-level red tests**

Add:

```go
func TestScopedRewriteRunsSystemThenGlobalThenRoute(t *testing.T)
func TestScopedRewriteUsesPriorityOnlyWithinScope(t *testing.T)
func TestScopedRewritePreservesServiceAndPluginConfigProvenance(t *testing.T)
func TestScopedRewriteFilterAndErrorResponse(t *testing.T)
func TestScopedRewriteEarlyStopSkipsLegacyAndUpstream(t *testing.T)
func TestScopedRewriteGlobalNotFoundRunsSystemAndGlobalOnly(t *testing.T)
```

The provenance test uses the same plugin name in service/plugin-config/route and proves the materialized winner has `ScopeRoute` without losing the original strict allowlist error context.

- [ ] **Step 2: Run route tests and record the red order**

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "^TestScopedRewrite" -count=1'
```

Expected before the change: combined priority order lets a high-priority route plugin execute before a low-priority global plugin.

- [ ] **Step 3: Construct scoped bindings in the builder**

Retain source scope until after plugin initialization; do not recover it from merged maps. `assembleRoutePluginChain` becomes `assembleRouteExecutor(routeBindings, globalBindings)`. Preserve consumer override wrapper selection from the bridge plan and do not modify consumer invocation.

- [ ] **Step 4: Run focused and full route package tests**

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "(ScopedRewrite|HTTPPluginAllowlist|ConsumerPlugin|BuilderLifecycle|PluginMeta)" -count=1'
bash -lc 'source .envrc && go test ./pkg/route -count=1'
```

### Task 4: Add compatibility characterization for the legacy remainder

**Files:**
- Modify: `pkg/plugin/executor_test.go`
- Modify: `pkg/route/plugin_order_test.go` if present; otherwise create `pkg/route/legacy_remainder_test.go`

- [ ] **Step 1: Characterize response and logger unwind**

Build a chain with migrated rewrite nodes around synthetic legacy response/logger nodes. Assert rewrite executes in scoped order, while legacy enter/exit and transform pipeline count remain the same as the pre-PR `BuildPluginChain` result.

- [ ] **Step 2: Characterize auth/consumer deferral**

Use key-auth plus a consumer plugin and assert the current `ctx.RunConsumerPlugins` timing remains unchanged in this PR. Mark the assertion as a compatibility boundary removed by the next access/consumer plan.

- [ ] **Step 3: Run characterization tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/route -run "(LegacyRemainder|ScopedRewrite.*Compatibility|Consumer.*Deferred)" -count=1'
```

### Task 5: Verification and independent PR delivery

**Files:**
- Include: `docs/superpowers/plans/2026-08-10-plugin-scoped-rewrite-executor.md`

- [ ] **Step 1: Run changed-package and race gates**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/route ./pkg/plugin/{request_context,request_id,real_ip,proxy_rewrite,proxy_control,proxy_mirror,traffic_label,traffic_split,ai_prompt_decorator,ai_prompt_template,ai_rag,ai_request_rewrite,data_mask,degraphql,example_plugin,jwe_decrypt} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin ./pkg/route -run "(ScopedExecutor|ScopedRewrite|LegacyRemainder)" -count=3'
```

- [ ] **Step 2: Scan registry and call sites**

```bash
rg -n 'NewExecutor|NewScopedExecutor|RequestStageFor|assembleRouteExecutor|BuildPluginChain' pkg cmd t
```

Verify no audited rewrite plugin remains duplicated in the legacy remainder.

- [ ] **Step 3: Run lint/build/diff gates**

```bash
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/... ./pkg/route/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 4: Independent review and delivery**

Review must verify scope-before-priority, exact adapter allowlist, no double execution, request replacement, stop behavior, metadata/allowlist parity, and unchanged legacy unwind. After approval, commit:

```bash
git commit -m "refactor(plugin): execute scoped rewrite plugins"
```

Open one ready PR, wait for CI, and merge before the access/consumer plan.

## Fast-plan-impl Dispatch Ownership

1. **WU-01 scoped executor and audited registry** owns `pkg/plugin/executor*`, `request_stage_registry*`, and `pkg/plugin/init_test.go` and freezes the binding interface first.
2. **WU-02 route provenance integration** owns only `pkg/route/**` files named in Tasks 3–4 and starts after WU-01.
3. **WU-03 adapter package characterization** owns only tests in the explicitly audited plugin package directories when package-level coverage is needed; it cannot modify executor or route files and may run parallel with WU-02. No worker performs delivery.

## Explicit Deferrals

- Auth plugins and consumer merge/rewrite remain at their legacy call site.
- Global and route access remain in the legacy remainder.
- Header/body/log phases and streaming/hijack behavior remain unchanged.
- The registry completeness gate only checks the explicit rewrite set in this PR; full registered-plugin phase coverage belongs to the final log/finalizer plan.
