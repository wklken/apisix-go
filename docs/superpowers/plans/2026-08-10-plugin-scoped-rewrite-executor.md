# Plugin Scoped Rewrite Executor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute audited system and global rewrite plugins before the existing route middleware chain while retaining exact route/service/plugin-config provenance for the later auth/consumer migration.

**Architecture:** Extend the mixed executor with immutable `Binding` records carrying source scope and a bounded request stage. A registry-owned adapter may convert a legacy Handler only when a static audit proves it performs request-side work before `next` and has no post-`next`, buffering, logging, tracing, cleanup, streaming, or hijack behavior. System and global rewrite bindings are removed from the legacy remainder and run scope-by-scope. Route/service/plugin-config bindings retain their stage and provenance but remain in the legacy priority/unwind recursion until authentication and consumer resolution are migrated together in the next PR.

**Tech Stack:** Go 1.26, request-phase bridge, route builder resource provenance, pinned APISIX 3.17 phase contract.

## Global Constraints

- Depends on the merged request-phase bridge.
- The upstream contract for this PR is global-rule `rewrite` before route/service/plugin-config `rewrite`; within each scope priority is descending.
- Do not move authentication or consumer execution. Auth plugins remain in the legacy remainder until the consumer/access plan.
- Route rewrite also remains inside that legacy auth/consumer envelope. Moving it
  ahead independently would break auth-produced variables such as
  `$jwt_auth_payload` and would execute same-name consumer overrides twice.
- Do not move access guards, response transforms, compression, streaming, logger, tracer, or finalizer plugins.
- The adapter allowlist is exact and static. A plugin absent from it is legacy; unknown registry names fail the completeness test rather than defaulting to rewrite.
- `_meta.priority`, `_meta.filter`, `_meta.error_response`, route consumer override, system `request-context`, and plugin enablement checks remain effective.
- Orphan services/plugin-configs remain inert exactly as in the strict builder contract; scope bindings are created only for materialized route generations.
- A scoped system/global rewrite plugin may replace the request and may stop. If
  it stops, no lower rewrite, route middleware, before-proxy hook, or upstream
  executes; outer panic/finalizer handling still runs. Route rewrite keeps its
  previous priority-relative stop boundary in this PR.
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
func BindPlugin(
    factoryName string,
    p Plugin,
    scope Scope,
    provenance ResourceProvenance,
) Binding
```

`NewExecutor(plugins...)` remains and creates legacy bindings for compatibility.
`factoryName` is the exact registry/config key and is never reconstructed from
`Plugin.GetName()`. This is required because the `request-context` factory
returns the implementation name `request_context`. The builder always supplies
the exact materialized source provenance; system request-context uses
`{Kind: ResourceSystem, ID: "request-context"}`.

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

Clone the binding slice including value-type provenance; plugin instances and
their configs are not deep-cloned. Stable-sort the system and global rewrite
scopes independently by descending effective priority and execute them through
`base.AdaptRequestPhase`. Keep every `ScopeRoute` binding, including audited
rewrite identities, in the prior mixed recursion so auth-produced state,
consumer override timing, legacy priority, enter/unwind, and transform-count
behavior remain compatible. Equal-priority order is stable only relative to
the supplied binding order; map iteration is not a cross-build ordering
guarantee. Every later build/runtime compatibility error reports
`Provenance.Kind` and `Provenance.ID`; never reconstruct provenance from a
merged plugin map.

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
    Stage              RequestStage
    AdaptLegacyHandler bool
}

func RequestStageFor(name string) (RequestStageSpec, bool)
```

The audited Plan 13 set is exact and matches the Plan 13 section of `2026-08-10-plugin-capability-manifest.md`:

```text
request-context, request-id, real-ip, proxy-rewrite, proxy-control,
proxy-mirror, traffic-label, traffic-split, ai-prompt-decorator,
ai-prompt-template, ai-rag, ai-request-rewrite, data-mask, degraphql,
example-plugin, jwe-decrypt
```

The registry is keyed only by the exact factory name: `request-context`;
`request_context` is accepted only by the registry/constructor consistency test
for that implementation's `GetName`. `request-context` and `request-id` already
implement `base.RequestPhasePlugin`. The remaining names use a private adapter
that calls the legacy Handler with a sentinel next, captures the request passed
to that next, returns Continue, and returns Stop when next was not called. The
adapter must reject double-next with a generic 500 early stop; the internal log
diagnostic includes the binding resource kind and ID, but the client response
must not expose control-plane identifiers. `proxy-mirror` retains its existing
before-proxy hook seam; it is not a finalizer.

- [ ] **Step 1: Add registry and adapter red tests**

Cover exact membership, no response/logger/stream plugin membership, request replacement, stop, double-next fail-closed, and registry-name drift.

- [ ] **Step 2: Run focused registry tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "(RequestStageRegistry|RewriteOnlyAdapter)" -count=1'
```

Expected: compile-red for missing registry and adapter.

- [ ] **Step 3: Implement the exact registry**

Use a map keyed by exact factory name. Do not infer the factory name from
`GetName`, stage from priority, or stage from package name. The only name drift
accepted by the constructor consistency test is factory `request-context` to
implementation `request_context`. Add comments naming the audited property: no
work after `next`, no deferred cleanup, no response writer wrapper.

- [ ] **Step 4: Run registry plus every adapted package test**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/plugin/{request_context,request_id,real_ip,proxy_rewrite,proxy_control,proxy_mirror,traffic_label,traffic_split,ai_prompt_decorator,ai_prompt_template,ai_rag,ai_request_rewrite,data_mask,degraphql,example_plugin,jwe_decrypt} -count=1'
```

### Task 3: Preserve route provenance as executor bindings

**Files:**
- Modify: `pkg/route/builder.go`
- Create: `pkg/route/scoped_rewrite_test.go`
- Modify: `pkg/route/builder_lifecycle_test.go`
- Modify: `pkg/route/prometheus_test.go`

**Interfaces:**
- Route, service, and plugin-config winners bind as `ScopeRoute`.
- Global-rule instances bind as `ScopeGlobal`.
- System request-context binds as `ScopeSystem` and executes before global rewrite.
- `ScopeRoute` records are retained for provenance and the next plan but execute
  in the legacy priority recursion in this PR.
- Consumer remains legacy in this PR.
- Route/service/plugin-config maps are validated separately before precedence
  merging. Only the materialized winner receives a binding, with its original
  resource kind and ID. Overridden losers remain subject to strict allowlist
  validation but do not create bindings.

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

Split provenance coverage into: independently materialized service and
plugin-config sources; a same-name route/plugin-config/service precedence case
that proves only the route winner is bound; and an overridden disabled source
case that proves strict allowlist validation still reports that loser's original
resource kind and ID.

- [ ] **Step 2: Run route tests and record the red order**

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "^TestScopedRewrite" -count=1'
```

Expected before the change: combined priority order lets a high-priority route plugin execute before a low-priority global plugin.

- [ ] **Step 3: Construct scoped bindings in the builder**

Retain `{factory name, config, provenance}` through precedence selection and
plugin initialization; do not recover it from merged maps or `GetName`.
`assembleRoutePluginChain` becomes
`assembleRouteExecutor(routeBindings, globalBindings, systemBindings)`.
Preserve consumer override wrapper selection and the route legacy envelope;
do not modify consumer invocation. Service provenance uses the authoritative
route `service_id`, not an optional embedded service payload ID. Global rules
without an embedded ID fail closed instead of creating an empty provenance.
The global-not-found path constructs only a system
`request-context` binding plus global bindings; it must not reuse
`buildSystemPluginConfigs`, which can also inject access-stage
`client-control`.

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

Build a chain with system/global rewrite outside synthetic legacy nodes and a
route rewrite inside the legacy auth-like envelope. Assert global executes
first, auth-like state is visible to route rewrite, legacy enter/exit remains
balanced, and transform pipeline count remains the same as the pre-PR
`BuildPluginChain` result. Add the real JWT `$jwt_auth_payload` to
proxy-rewrite integration gate and a same-name consumer proxy-rewrite case that
executes only the consumer instance.

- [ ] **Step 2: Characterize auth/consumer deferral**

Use key-auth plus a consumer plugin and assert the current `ctx.RunConsumerPlugins` timing remains unchanged in this PR. Mark the assertion as a compatibility boundary removed by the next access/consumer plan.

- [ ] **Step 3: Run characterization tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/route -run "(LegacyRemainder|ScopedRewrite.*Compatibility|Consumer.*Deferred)" -count=1'
```

### Task 5: Verification and independent PR delivery

**Files:**
- Include: `docs/superpowers/plans/2026-08-10-plugin-scoped-rewrite-executor.md`
- Include: `docs/superpowers/plans/2026-08-10-plugin-capability-manifest.md`

- [ ] **Step 1: Run changed-package and race gates**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/route ./pkg/plugin/{request_context,request_id,real_ip,proxy_rewrite,proxy_control,proxy_mirror,traffic_label,traffic_split,ai_prompt_decorator,ai_prompt_template,ai_rag,ai_request_rewrite,data_mask,degraphql,example_plugin,jwe_decrypt} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin ./pkg/route -run "(ScopedExecutor|ScopedRewrite|LegacyRemainder)" -count=3'
```

- [ ] **Step 2: Scan registry and call sites**

```bash
rg -n 'NewExecutor|NewScopedExecutor|RequestStageFor|assembleRouteExecutor|BuildPluginChain' pkg cmd t
```

Verify system/global audited rewrite bindings do not remain duplicated. Route
rewrite bindings intentionally remain in the legacy remainder until the next
plan migrates authentication and consumer resolution with them.

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

1. **WU-01 scoped executor and audited registry** owns
   `pkg/plugin/executor.go`, `pkg/plugin/executor_test.go`,
   `pkg/plugin/request_stage_registry.go`,
   `pkg/plugin/request_stage_registry_test.go`, and `pkg/plugin/init_test.go`.
   It freezes the binding/registry/adapter interface first.
2. **WU-02 route provenance integration** owns only `pkg/route/builder.go`,
   `pkg/route/scoped_rewrite_test.go`, `pkg/route/legacy_remainder_test.go`, and
   `pkg/route/builder_lifecycle_test.go` when adaptation is required. The root
   owner may adapt the single `pkg/route/prometheus_test.go` strict-helper
   caller after the dead-code scan. It starts only after WU-01 is accepted.
3. **WU-03 adapter package characterization** owns only `plugin_test.go` files
   under the 16 explicitly audited plugin package directories. It adds only
   missing phase/legacy-equivalence, replacement, stop, hook, nested-JWE, and
   public-API coverage; it cannot modify production, executor, or route files
   and may run parallel with WU-02 after WU-01. No worker performs delivery.

## Explicit Deferrals

- Auth plugins and consumer merge/rewrite remain at their legacy call site.
- Global and route access remain in the legacy remainder.
- Header/body/log phases and streaming/hijack behavior remain unchanged.
- The registry completeness gate only checks the explicit rewrite set in this PR; full registered-plugin phase coverage belongs to the final log/finalizer plan.
