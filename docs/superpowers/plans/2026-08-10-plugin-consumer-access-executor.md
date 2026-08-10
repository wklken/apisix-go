# Plugin Consumer and Access Executor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move authentication, consumer merge/rewrite, global access, route access, and before-proxy execution into one explicit request pipeline that matches the pinned APISIX 3.17 order and invokes consumer resolution exactly once.

**Architecture:** Extend the scoped request executor with explicit route-rewrite, consumer-rewrite, and access stages. Authentication plugins only authenticate and attach a consumer; they no longer invoke a request-context callback. After route rewrite completes, the route executor loads consumer and group plugins, applies group then consumer precedence, runs only newly introduced non-auth consumer rewrite plugins, then global access, merged route access, and the existing before-proxy terminal. Legacy plugins are adapted only from an audited static stage registry; response, stream, and log owners remain in the legacy remainder for later plans.

**Tech Stack:** Go 1.26, `pkg/plugin` scoped executor, `pkg/route` builder, `pkg/apisix/ctx`, bbolt-backed consumer getters, pinned APISIX 3.17 phase order.

## Global Constraints

- Depends on the merged scoped rewrite executor.
- Fixed order: system setup → global rewrite → route rewrite including auth → consumer/group merge → newly introduced non-auth consumer rewrite → global access → merged route access → before-proxy → upstream.
- Within a scope and stage, effective plugin priority is descending. Scope/stage order always wins over priority.
- Authentication must resolve at most one consumer and must not execute consumer plugins. Anonymous fallback attaches its selected consumer through the same result path.
- `multi-auth` probe attempts must never run consumer plugins, write a successful child response, or publish partial auth state. Only the winning child result is committed to the real request.
- Consumer group plugins merge first, then consumer plugins override them. Consumer plugins override same-name route/service/plugin-config plugins. Auth plugins in consumer/group config authenticate nobody and are not re-run.
- Missing or invalid consumer/group configuration fails closed before access or upstream. Existing HTTP status and anonymous-consumer compatibility remain unchanged.
- Preserve `_meta.filter`, `_meta.error_response`, `_meta.priority`, plugin allowlist checks, route/service/plugin-config provenance, and current lazy consumer materialization.
- Do not migrate header/body/log/stream behavior or claim full PR-014 closure in this PR.

---

### Task 1: Extend the request executor with consumer and access stages

**Files:**
- Modify: `pkg/plugin/executor.go`
- Modify: `pkg/plugin/executor_test.go`
- Modify: `pkg/plugin/request_stage_registry.go`
- Modify: `pkg/plugin/request_stage_registry_test.go`
- Modify: `pkg/apisix/ctx/context.go`
- Modify: `pkg/apisix/ctx/context_test.go`

**Interfaces:**

```go
const (
    RequestStageLegacy RequestStage = iota
    RequestStageRewrite
    RequestStageConsumerRewrite
    RequestStageAccess
    RequestStageBeforeProxy
)

type ConsumerBindingResolver func(*http.Request) ([]Binding, *http.Request, error)

type RequestPipeline struct { /* immutable stage slices and resolver */ }

func NewRequestPipeline(bindings []Binding, resolve ConsumerBindingResolver) RequestPipeline
func (p RequestPipeline) Then(beforeProxy http.Handler) http.Handler

type AuthenticationState struct {
    Consumer *resource.Consumer
    Source   string
}

func WithAuthenticationState(r *http.Request, state AuthenticationState) *http.Request
func AuthenticationStateFrom(r *http.Request) (AuthenticationState, bool)
```

The resolver is called once after route rewrite. It returns consumer-scoped bindings plus the request carrying consumer/group variables. It must not invoke handlers itself. Existing before-proxy hooks execute exactly once in `RequestStageBeforeProxy`; they are not finalizers and do not enter the response pipeline.

- [ ] **Step 1: Write executor ordering regressions**

Add:

```go
func TestRequestPipelineMatchesPinnedScopeAndStageOrder(t *testing.T)
func TestRequestPipelineResolvesConsumerExactlyOnce(t *testing.T)
func TestRequestPipelineSkipsConsumerAuthPlugins(t *testing.T)
func TestRequestPipelineConsumerOverridesRoutePlugin(t *testing.T)
func TestRequestPipelineStopsBeforeAccessAndUpstream(t *testing.T)
func TestRequestPipelinePreservesRequestReplacement(t *testing.T)
```

The first test must use priorities that conflict with phase order and require `global-rewrite, route-auth, route-rewrite, consumer-rewrite, global-access, route-access, before-proxy`.

- [ ] **Step 2: Run tests and record compile-red**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "^TestRequestPipeline" -count=1'
```

- [ ] **Step 3: Implement immutable stage slices and one resolver call**

Clone all binding inputs. Reuse the bridge adapter for explicit request plugins. Reject resolver errors with the existing internal-error response path; do not continue into access. Consumer auth filtering must use a fixed registry classification, not a name suffix or priority heuristic.

- [ ] **Step 4: Run plugin focused and race tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "(RequestPipeline|ScopedExecutor|RequestStageRegistry)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin -run "(RequestPipeline|ScopedExecutor)" -count=3'
```

### Task 2: Replace the request-context consumer runner in route resolution

**Files:**
- Modify: `pkg/route/builder.go`
- Create: `pkg/route/consumer_access_test.go`

Delete production use of `WithConsumerPluginRunner` and `RunConsumerPlugins` after every auth caller is migrated. Keep no proxy-only compatibility wrapper unless an external production caller remains after an `rg` scan.

- [ ] **Step 1: Add route integration regressions**

Cover group-before-consumer precedence, consumer-over-route replacement, disabled consumer plugin fail-closed, missing group, consumer resolver exactly once, global access before route access, and before-proxy once. Add a route with key-auth followed by an early rejecting access plugin and prove upstream is not called.

- [ ] **Step 2: Run focused route tests and record current drift**

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "^(TestConsumerAccess|TestAuthenticationState)" -count=1'
```

- [ ] **Step 3: Build consumer bindings after authentication**

The builder-owned resolver reads the authenticated consumer, loads its group, validates allowlist membership before construction, merges group then consumer maps, and creates `ScopeConsumer` bindings. Compare names against route bindings so only newly introduced non-auth rewrite plugins run in consumer rewrite; same-name consumer entries replace the route binding for later access/response plans.

- [ ] **Step 4: Remove the old callback seam after call-site migration**

```bash
rg -n 'WithConsumerPluginRunner|RunConsumerPlugins|ConsumerPluginRunner' pkg cmd t
```

Expected after Task 3: no production matches. Delete obsolete context tests and helpers only after the scan proves no compatibility owner remains.

### Task 3: Migrate authentication plugins to attach state and return

**Files:**
- Modify: `pkg/plugin/basic_auth/plugin.go`
- Modify: `pkg/plugin/basic_auth/plugin_test.go`
- Modify: `pkg/plugin/hmac_auth/plugin.go`
- Modify: `pkg/plugin/hmac_auth/plugin_test.go`
- Modify: `pkg/plugin/jwt_auth/plugin.go`
- Modify: `pkg/plugin/jwt_auth/plugin_test.go`
- Modify: `pkg/plugin/key_auth/plugin.go`
- Modify: `pkg/plugin/key_auth/plugin_test.go`
- Modify: `pkg/plugin/ldap_auth/plugin.go`
- Modify: `pkg/plugin/ldap_auth/plugin_test.go`
- Modify: `pkg/plugin/multi_auth/plugin.go`
- Modify: `pkg/plugin/multi_auth/plugin_test.go`
- Modify: `pkg/plugin/wolf_rbac/plugin.go`
- Modify: `pkg/plugin/wolf_rbac/plugin_test.go`

- [ ] **Step 1: Add auth-specific red tests**

For every package, assert `RunRequestPhase` attaches `AuthenticationState` and does not call downstream. Separately assert the legacy `Handler(next)` compatibility adapter calls `next` exactly once on success without a consumer runner. Preserve invalid credentials, hide-credentials, anonymous fallback, and protected identity-header behavior.

For `multi-auth`, add:

```go
func TestMultiAuthProbesDoNotResolveConsumerPlugins(t *testing.T)
func TestMultiAuthPublishesOnlyWinningAuthenticationState(t *testing.T)
```

- [ ] **Step 2: Run the auth matrix before production edits**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{basic_auth,hmac_auth,jwt_auth,key_auth,ldap_auth,multi_auth,wolf_rbac} -run "(AuthenticationState|ConsumerPlugins|MultiAuthProbes)" -count=1'
```

- [ ] **Step 3: Implement phase adapters without changing credential semantics**

Each successful explicit auth phase returns a request containing the chosen consumer. Anonymous success uses the same helper. `multi-auth` clones probe state, suppresses child continuation, and copies only the winning state to the real request. Legacy Handler adapters alone invoke supplied `next`.

- [ ] **Step 4: Run complete affected auth packages**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{basic_auth,hmac_auth,jwt_auth,key_auth,ldap_auth,multi_auth,wolf_rbac} -count=1'
```

### Task 4: Characterize access and response deferral

**Files:**
- Modify: `pkg/plugin/request_stage_registry_test.go`
- Modify: `pkg/route/consumer_access_test.go`

- [ ] **Step 1: Audit and pin the access-only adapter set**

For every Plan 14 identity in the exact manifest, inspect its Handler and record that the migrated request capability executes before downstream. The static Plan 14 registry contains exactly the 35 identities in `2026-08-10-plugin-capability-manifest.md` plus pre-migrated `limit-conn`; identities assigned to Plans 15/16 retain their legacy access half until their multi-capability owner PR. Tests reject missing or extra names and verify no adapter hides post-next, response-writer, logging, streaming, hijack, or cleanup behavior.

- [ ] **Step 2: Prove response plugins remain deferred**

Combine key-auth, one migrated access plugin, and synthetic legacy response/logger nodes. Assert request ordering changes as planned while legacy response unwind remains unchanged. Mark this as the boundary removed by the buffered-response plan.

- [ ] **Step 3: Run compatibility tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/route -run "(AccessOnlyRegistry|ConsumerAccess|ResponseDeferral)" -count=1'
```

### Task 5: Verification, review, and independent PR delivery

- [ ] **Step 1: Run changed-package and race gates**

```bash
bash -lc 'source .envrc && go test ./pkg/apisix/ctx ./pkg/plugin ./pkg/route ./pkg/plugin/{basic_auth,hmac_auth,jwt_auth,key_auth,ldap_auth,multi_auth,wolf_rbac} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin ./pkg/route ./pkg/plugin/multi_auth -run "(RequestPipeline|ConsumerAccess|AuthenticationState|MultiAuth)" -count=3'
```

- [ ] **Step 2: Run call-site and duplicate-execution scans**

```bash
rg -n 'RunConsumerPlugins|WithConsumerPluginRunner|RequestStageAccess|ScopeConsumer|AuthenticationState' pkg cmd t
```

- [ ] **Step 3: Run scoped lint/build/diff gates**

```bash
bash -lc 'source .envrc && golangci-lint run ./pkg/apisix/ctx/... ./pkg/plugin/... ./pkg/route/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 4: Independent review and delivery**

Review must verify pinned phase order, exactly-once consumer resolution, group/consumer precedence, auth not re-run, multi-auth probe isolation, allowlist failure, and unchanged response unwind. After approval, commit:

```bash
git commit -m "refactor(plugin): execute consumer and access phases"
```

Open one ready PR, wait for CI, and merge before buffered response phases.

## Fast-plan-impl Dispatch Ownership

1. **WU-01 pipeline stages, authentication-state interface, and registry** owns `pkg/plugin/executor*`, `request_stage_registry*`, and `pkg/apisix/ctx/context*`; accept this interface first.
2. **WU-02 route resolution** owns only the `pkg/route/**` files named in Task 2.
3. **WU-03 authentication family** owns the seven auth plugin directories named in Task 3 and may run parallel with WU-02 after WU-01. Ownership is exclusive; workers do not deliver the branch.

## Explicit Deferrals

- Header/body transforms still use the characterized legacy response remainder. Multi-capability request owners assigned to Plans 15/16 remain explicit deferrals, so this PR does not claim all registered access capabilities are migrated.
- Streaming, compression, hijack, websocket, and gRPC response behavior remain unchanged.
- Loggers and tracers remain legacy finalizers.
- Consumer/group updates remain lazily materialized under the existing store/reload architecture.
