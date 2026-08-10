# Plugin Consumer Resolution and Access Pipeline Implementation Plan v2

> Execute with the explicitly authorized `fast-plan-impl` workflow. Workers make local changes and focused verification only; delivery remains with the phase owner.

**Goal:** Replace callback-driven consumer execution with one explicit request pipeline that authenticates first, resolves consumer/group configuration once, selects effective route/group/consumer winners before any route rewrite or deferred legacy handler executes, and then runs rewrite, consumer rewrite, access, before-proxy, and upstream work exactly once.

**Architecture:** Plan 13 system and global rewrite remain the outer explicit stages. Plan 14 then runs explicit consumer authentication, resolves and attaches the consumer once, and computes one immutable effective binding set. Only after those winners are known does it build one per-request deferred legacy chain containing effective response/log/stream/multi-capability bindings. That chain preserves its own legacy priority, pre-next, post-next unwind, and transform behavior while wrapping the explicit route-rewrite, consumer-rewrite, access, before-proxy, and upstream stages. This intentionally changes deferred legacy pre-next observation relative to authentication: auth and resolver failures do not enter the deferred chain. Plans 15 and 17 later give early failures explicit response/log ownership.

**Tech Stack:** Go 1.26, `net/http`, Plan 12 request-phase results and lifecycle, Plan 13 immutable bindings/provenance, `pkg/apisix/ctx`, `pkg/route.Builder`, bbolt-backed consumer/group stores, and deterministic registry tests.

## Prerequisites and fixed dependency

- Start only from the merged Plan 13 SHA. Refresh `origin/master`, prove the merge is an ancestor, and record the exact SHA in the Plan 14 worktree and capability manifest.
- Use an isolated worktree. Do not dispatch from or modify the dirty main checkout.
- Plans 15–17 are not prerequisites and no API from those plans may be introduced.
- Source `.envrc` before every Go, lint, or make command.
- Verification is impact-scoped. Do not run `go test ./...`, `make test`, the whole `t/plugin`, or `make test-integration`.

## Fixed production order

For a normal route request, production order is exactly:

```text
system rewrite
-> global rewrite
-> explicit pre-consumer authentication
-> resolve consumer/group once and attach the winner once
-> materialize effective global plus route/group/consumer winners
-> enter one effective deferred legacy chain
   -> merged route rewrite
   -> global consumer rewrite
   -> merged consumer rewrite
   -> global access
   -> merged route/group/consumer access
   -> finalize proxy rewrite once
   -> run before-proxy hooks once
   -> existing AI terminal or normal upstream
<- unwind the same deferred legacy chain once
```

Rules:

1. System/global rewrite complete before authentication. Route/service/plugin-config rewrite does not execute before authentication or consumer resolution.
2. Authentication uses only explicit bindings marked `AuthenticatesConsumer=true`; a Stop skips resolution, deferred legacy work, route rewrite, access, before-proxy, and upstream.
3. The resolver is invoked exactly once. Missing authentication returns an unresolved empty result without store reads or attachment.
4. A successful resolver loads and validates the referenced group, overlays consumer config on group config, filters credential-only consumer/group configs, attaches the consumer once, and returns clone-safe bindings plus the final request.
5. Before route rewrite executes, consumer/group bindings override same-name materialized route/service/plugin-config bindings. Global bindings stay a separate scope and are never removed by consumer config.
6. The selected route/group/consumer `proxy-rewrite` winner executes once after authentication, so auth-produced variables such as `$jwt_auth_payload` are available.
7. `attach-consumer-label` is the sole Plan 14 consumer-rewrite identity. Global runs before the one merged route/group/consumer winner.
8. Global access runs before merged route/group/consumer access. Priority descends only within one scope and stage.
9. Deferred legacy bindings are selected after resolution from the effective winners. The chain preserves its internal legacy priority, pre-next behavior, reverse unwind, and transform count; it is never entered before auth/resolution.
10. The changed legacy boundary is explicit: deferred handlers do not observe auth or resolver failures in Plan 14. Do not claim otherwise.
11. The pipeline is the only production caller that finalizes proxy rewrite and invokes before-proxy hooks. A prior Stop skips both and skips terminal/upstream.
12. Global-not-found uses system request-context plus global bindings, an empty resolver, no route/group/consumer bindings, and no `client-control` system injection.

## Frozen request and resolver interfaces

WU-01 freezes these declarations before WU-02 and WU-03 start:

```go
type RequestStage uint8

const (
    RequestStageLegacy RequestStage = iota
    RequestStageRewrite
    RequestStageConsumerRewrite
    RequestStageAccess
    RequestStageBeforeProxy
)

type RequestStageSpec struct {
    Stage                 RequestStage
    AuthenticatesConsumer bool
    ConsumerConfigOnly    bool
    AdaptLegacyHandler    bool
}

func RequestStageFor(factoryKey string) (RequestStageSpec, bool)

type AuthenticationState struct {
    Source   string
    consumer resource.Consumer
}

func NewAuthenticationState(source string, consumer resource.Consumer) AuthenticationState
func (s AuthenticationState) Consumer() resource.Consumer
func WithAuthenticationState(r *http.Request, state AuthenticationState) *http.Request
func AuthenticationStateFrom(r *http.Request) (AuthenticationState, bool)
func NewAuthenticationProbeRequest(r *http.Request) *http.Request

type ConsumerIdentity struct {
    Username   string
    GroupID    string
    AuthSource string
}

type ConsumerCacheKey struct {
    ConsumerID     string
    ConsumerDigest [32]byte
    GroupID        string
    GroupDigest    [32]byte
    RouteID        string
    ServiceID      string
}

type ConsumerResolution struct {
    Bindings []Binding
    Request  *http.Request
    CacheKey ConsumerCacheKey
    Identity ConsumerIdentity
    Resolved bool
}

type ConsumerBindingResolver func(*http.Request) (ConsumerResolution, error)

func NewRequestPipeline(bindings []Binding, resolve ConsumerBindingResolver) RequestPipeline
func (p RequestPipeline) Then(terminal http.Handler) http.Handler
```

Interface invariants:

- Registry keys are exact factory/config keys. Never infer from `GetName`, priority, Handler shape, directory name, or suffix.
- Authentication state clones nested consumer plugin configs and labels at every constructor/context/accessor boundary.
- Probe requests isolate headers, URL state, diagnostics, and authentication state. `multi-auth` retains body replay/close ownership.
- The resolver returns initialized immutable binding metadata; request pointers, authentication state, attached vars, and override maps are never cached.
- A nil resolution request means the current request. Returned and cached binding slices are cloned; plugin instances are not deep-cloned.
- Only the resolver calls `ctx.AttachConsumer`, after group lookup, validation, initialization, and cache selection all succeed.
- Resolver errors contain internal consumer/group/store context. The client receives only status 500 with `Internal Server Error\n`, and lower stages do not run.
- Explicit request-phase implementations win over an audited legacy adapter. The adapter treats no-next as Stop and double-next as a generic fail-closed error with internal provenance only.
- `NewExecutor` and `NewScopedExecutor` remain compatibility surfaces for existing callers/tests; route production uses `NewRequestPipeline`.

## Exact Plan 14 classification

The Plan 14 registry contains exactly 35 identities, plus the already explicit `limit-conn` row.

### Seven explicit authentication identities

```text
basic-auth, hmac-auth, jwt-auth, key-auth, ldap-auth, multi-auth, wolf-rbac
```

Each has:

```text
Stage=Access, AuthenticatesConsumer=true,
ConsumerConfigOnly=true, AdaptLegacyHandler=false
```

### One consumer rewrite identity

```text
attach-consumer-label
```

It has:

```text
Stage=ConsumerRewrite, AuthenticatesConsumer=false,
ConsumerConfigOnly=false, AdaptLegacyHandler=true
```

### Twenty-seven audited legacy access adapters

```text
acl, ai-aws-content-moderation, ai-prompt-guard, authz-casbin,
authz-casdoor, authz-keycloak, cas-auth, chaitin-waf, client-control,
consumer-restriction, csrf, dingtalk-auth, feishu-auth, forward-auth,
graphql-limit-count, ip-restriction, limit-count, limit-req, oas-validator,
opa, openid-connect, referer-restriction, request-validation, saml-auth,
ua-restriction, uri-blocker, workflow
```

Each has:

```text
Stage=Access, AuthenticatesConsumer=false,
ConsumerConfigOnly=false, AdaptLegacyHandler=true
```

### Existing rows extended or retained

- `limit-conn`: Access, not authentication, not consumer-only, no legacy adapter. Its Plan 12 lifecycle finalizer is unchanged.
- Consumer/group credential exclusion is exactly eight identities: the seven auth identities plus `jwe-decrypt`.
- `jwe-decrypt` remains a Plan 13 route/global Rewrite identity and an audited adapter. Only consumer/group provenance is excluded as credential configuration; it is not counted again in the 35 Plan 14 identities.
- All other Plan 13 rewrite rows retain their existing classification.

Literal tests must prove exact 35+1 membership, exact 27/7/1 subsets, exact eight credential exclusions, enum order, and factory/implementation alias handling.

## Effective winner and deferred legacy algorithm

At request time, after resolver success:

1. Clone static global and materialized route/service/plugin-config bindings.
2. Overlay consumer-group bindings on route-scope bindings by exact factory key.
3. Overlay consumer bindings on the resulting merged bindings by exact factory key.
4. Preserve the winning binding's original `ResourceProvenance` and effective priority.
5. Partition the effective set into route Rewrite, ConsumerRewrite, Access, BeforeProxy, and Legacy.
6. Build one deferred legacy chain from effective global legacy bindings plus effective merged legacy bindings. Use the existing legacy priority comparator and recursion; do not apply explicit scope-first ordering inside the legacy chain.
7. Run the legacy chain once around the explicit inner handler. No route legacy loser may enter the chain, and no dynamic consumer legacy winner may be installed a second time.
8. Install a fresh consumer/group winner-name set through the existing consumer override context helper only when current production handlers still consume that signal. The set is not used to rescue an already-entered route wrapper.

The old route override wrappers are removed because winner selection is complete before any route/deferred binding executes. The underlying consumer override context seam is retained while concrete production callers such as consumer-aware access implementations still use it; Plans 15/17 may reassess it after their migrations.

## Task 1 — Freeze registry and clone-safe authentication state

**Owner:** WU-01

**Files:**

- `pkg/plugin/request_stage_registry.go`
- `pkg/plugin/request_stage_registry_test.go`
- `pkg/plugin/init_test.go`
- `pkg/apisix/ctx/context.go`
- `pkg/apisix/ctx/context_test.go`
- `docs/superpowers/plans/2026-08-10-plugin-capability-manifest.md`

Steps:

- [ ] Add literal enum/table/subset tests and record the expected red result.
- [ ] Add authentication-state deep-clone and probe-isolation tests, including nested maps/slices and losing probe mutation.
- [ ] Extend the exact registry; do not weaken Plan 13's no-next/double-next behavior.
- [ ] Implement clone-safe context helpers without importing plugin packages into `pkg/apisix/ctx`.
- [ ] Keep all consumer runner and consumer override context symbols during WU-01. Cleanup happens only after WU-02/WU-03 return.
- [ ] Run focused normal/race gates and freeze interface signatures plus a diff fingerprint.

Focused gates:

```bash
bash -lc 'source .envrc && go test ./pkg/apisix/ctx ./pkg/plugin -run "(AuthenticationState|AuthenticationProbe|Plan14Registry|ConsumerConfigOnly|LegacyAccessAdapter|RequestStageEnum|LimitConnRemains)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/apisix/ctx ./pkg/plugin -run "(AuthenticationState|AuthenticationProbe|Plan14Registry|LegacyAccessAdapter)" -count=3'
```

## Task 2 — Implement effective merge and the post-resolution request pipeline

**Owner:** WU-01

**Files:**

- `pkg/plugin/executor.go`
- `pkg/plugin/executor_test.go`

Regression-first tests:

```go
func TestRequestPipelineRunsPlan14V2Order(t *testing.T)
func TestRequestPipelineAuthStopSkipsResolverAndLegacy(t *testing.T)
func TestRequestPipelineResolverErrorSkipsLegacyAndReturnsGeneric500(t *testing.T)
func TestRequestPipelineResolvesExactlyOnceWithoutAuthentication(t *testing.T)
func TestRequestPipelineConsumerOverridesRouteRewriteBeforeExecution(t *testing.T)
func TestRequestPipelineDeferredLegacyUsesEffectiveWinnersOnly(t *testing.T)
func TestRequestPipelineDeferredLegacyPreservesPriorityAndUnwind(t *testing.T)
func TestRequestPipelinePropagatesEveryReplacementRequest(t *testing.T)
func TestRequestPipelineRunsBeforeProxyOnce(t *testing.T)
```

Steps:

- [ ] Partition only system/global rewrite and explicit authentication before resolver invocation.
- [ ] Merge route/group/consumer winners before partitioning route rewrite or legacy work.
- [ ] Build the deferred legacy chain after resolution and wrap the explicit route rewrite/consumer rewrite/access/before-proxy/terminal handler.
- [ ] Preserve internal legacy priority, pre-next, post-next unwind, transform count, Stop behavior, and final request propagation.
- [ ] Make the changed auth-failure observation boundary explicit in tests: deferred legacy count remains zero on auth or resolver failure.
- [ ] Keep the existing AI terminal/upstream interface; do not import later-plan APIs.

Focused gates:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "(RequestPipeline|ScopedExecutor|LegacyRemainder|RequestStageRegistry)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin -run "(RequestPipeline|ScopedExecutor|LegacyRemainder|BeforeProxy)" -count=3'
```

## Task 3 — Resolve/cache consumer and group bindings

**Owner:** WU-02 after WU-01 interface freeze

**Files:**

- `pkg/route/builder.go`
- `pkg/route/consumer_access_test.go`
- `pkg/route/consumer_plugin_test.go`

Private route contract:

```go
type consumerResolutionTemplate struct {
    bindings []plugin.Binding
}

type consumerResolutionCache struct {
    mu      sync.Mutex
    entries map[plugin.ConsumerCacheKey]consumerResolutionTemplate
}

func (b *Builder) resolveConsumerBindings(
    routeContext pluginRouteContext,
) plugin.ConsumerBindingResolver
```

Steps:

- [ ] Test empty resolution, exact-once invocation, stable comparable cache key, zero-digest fallback, cache slice cloning, and reload digest separation.
- [ ] Test group then consumer precedence with exact group/consumer provenance.
- [ ] Test exact eight credential exclusions while preserving consumer `proxy-rewrite` as executable rewrite.
- [ ] Fail every missing/unreadable/invalid group closed; response remains generic and internal logs contain username, group ID, and wrapped store error.
- [ ] Validate raw consumer and group plugin sources against the HTTP allowlist before initialization.
- [ ] Cache initialized instances plus immutable binding metadata only. Never cache request/context/auth/override state.
- [ ] Attach the consumer exactly once after all failure points. Return `Resolved=true` even when there are zero executable bindings.

Focused gates:

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "^(TestConsumerResolution|TestConsumerPlugin)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/route -run "(ConsumerResolution|ConsumerPlugin.*Cache|ReloadDigest)" -count=3'
```

## Task 4 — Install route pipeline, effective legacy chain, and one before-proxy owner

**Owner:** WU-02

**Additional files:**

- `pkg/route/scoped_rewrite_test.go`
- `pkg/route/builder_lifecycle_test.go`
- `pkg/route/ai_runtime_test.go`
- `pkg/route/plugin_allowlist_test.go`
- `pkg/route/plugin_parity_test.go`
- `pkg/route/request_phase_metadata_test.go`

Required route tests:

```go
func TestPlan14V2JWTAuthPayloadFeedsEffectiveProxyRewrite(t *testing.T)
func TestPlan14V2ConsumerProxyRewriteOverridesRouteBeforeEitherExecutes(t *testing.T)
func TestPlan14V2DeferredLegacyEffectiveWinnerRunsOnce(t *testing.T)
func TestPlan14V2DeferredLegacyDoesNotObserveAuthFailure(t *testing.T)
func TestPlan14V2MissingGroupRunsNoLegacyAccessOrUpstream(t *testing.T)
func TestPlan14V2AttachConsumerLabelRunsAfterRouteRewriteOnce(t *testing.T)
func TestPlan14V2BeforeProxyAndFinalizeRunOnce(t *testing.T)
func TestPlan14V2GlobalNotFoundHasNoConsumerOrClientControl(t *testing.T)
```

Steps:

- [ ] Preserve exact factory key and provenance through route/service/plugin-config/global materialization.
- [ ] Construct `NewRequestPipeline` once around the existing AI terminal/normal upstream boundary.
- [ ] Remove route production use of the consumer callback, route override wrapper types, old consumer executor cache, and separate before-proxy wrapper.
- [ ] Do not remove context callback declarations; root performs that after WU-03 removes auth callers.
- [ ] Retain consumer override context calls only where the effective winner set and concrete deferred/access handlers need them.
- [ ] Preserve allowlist, `_meta.filter`, `_meta.error_response`, `_meta.priority`, disabled-source diagnostics, and last-good generation behavior.

Focused gates:

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "(Plan14V2|ConsumerResolution|ConsumerAccess|AttachConsumerLabel|RoutePipeline|ScopedRewrite|AIExecution|BuilderLifecycle|RequestPhaseMetadata)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/route -run "(Plan14V2|ConsumerResolution|BeforeProxy|AIExecution|Reload)" -count=3'
```

## Task 5 — Migrate seven authentication families

**Owner:** WU-03 after WU-01 interface freeze; runs parallel with WU-02

**Files:**

- `pkg/plugin/basic_auth/**`
- `pkg/plugin/hmac_auth/**`
- `pkg/plugin/jwt_auth/**`
- `pkg/plugin/key_auth/**`
- `pkg/plugin/ldap_auth/**`
- `pkg/plugin/multi_auth/**`
- `pkg/plugin/wolf_rbac/**`

Steps:

- [ ] Add paired explicit-phase/legacy-handler tests for success, failure, anonymous behavior, protected headers, body limits, and exactly-once downstream execution.
- [ ] On success publish only clone-safe `AuthenticationState` with the exact factory key; never attach a consumer or invoke a consumer resolver/callback.
- [ ] Preserve current credential failure status/body/header behavior.
- [ ] Probe six migrated child auth plugins through their explicit phase. The sole allowed legacy child probe is exact `jwe-decrypt`.
- [ ] Isolate every losing probe's headers, body replay, diagnostics, and auth state. Publish only the winner's request mutation and state; preserve the winning child factory key.

Focused gates:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{basic_auth,hmac_auth,jwt_auth,key_auth,ldap_auth,multi_auth,wolf_rbac} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/{basic_auth,hmac_auth,jwt_auth,key_auth,ldap_auth,multi_auth,wolf_rbac} -run "(AuthenticationState|RunRequestPhase|MultiAuth|BodyReplay|Anonymous|LegacyHandler)" -count=3'
```

## Task 6 — Root-owned integration cleanup, verification, review, and delivery

This task starts only after all three WU results are accepted.

Cleanup rules:

1. Scan all production and tests for the obsolete `ConsumerPluginRunner` callback family.
2. Delete that callback family from `pkg/apisix/ctx/context.go` and its obsolete tests only when WU-02 and WU-03 have removed every caller.
3. Scan the consumer override context family separately. Retain it when any concrete production caller remains; classify every caller. Do not delete it merely because route wrapper types were removed.
4. Confirm `ctx.AttachConsumer` has one production owner and `ctx.RunBeforeProxyHooks` has one production owner.
5. Run dead/proxy-only scans for every removed route helper and cache type.

Required scans:

```bash
rg -n 'WithConsumerPluginRunner|RunConsumerPlugins|ConsumerPluginRunner|consumerPluginRunnerKey|consumerPluginsRunKey' pkg cmd t
rg -n 'withConsumerPluginRunner|runConsumerPlugins|routeConsumerOverridePlugin|routeConsumerOverrideRequestPlugin|newRouteConsumerOverridePlugin|consumerPluginChainForIdentity|consumerPluginChains|withBeforeProxyHooks' pkg/route
rg -n 'WithConsumerPluginOverrides|ConsumerPluginOverrides' pkg --glob '*.go'
rg -n 'ctx\.AttachConsumer' pkg --glob '*.go' --glob '!**/*_test.go'
rg -n 'RunBeforeProxyHooks' pkg/route pkg/plugin pkg/server
```

Combined verification:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "(Plan14Registry|LegacyAccessAdapterSet|ConsumerConfigOnlyCredentialSet|ExplicitAuthSet|LimitConnRemains|RegistryMatchesFactories|RequestStageEnum)" -count=1'
bash -lc 'source .envrc && go test ./pkg/apisix/ctx ./pkg/plugin ./pkg/route ./pkg/plugin/{acl,ai_aws_content_moderation,ai_prompt_guard,attach_consumer_label,authz_casbin,authz_casdoor,authz_keycloak,basic_auth,cas_auth,chaitin_waf,client_control,consumer_restriction,csrf,dingtalk_auth,feishu_auth,forward_auth,graphql_limit_count,hmac_auth,ip_restriction,jwt_auth,key_auth,ldap_auth,limit_conn,limit_count,limit_req,multi_auth,oas_validator,opa,openid_connect,referer_restriction,request_validation,saml_auth,ua_restriction,uri_blocker,wolf_rbac,workflow} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/apisix/ctx ./pkg/plugin ./pkg/route ./pkg/plugin/{basic_auth,hmac_auth,jwt_auth,key_auth,ldap_auth,multi_auth,wolf_rbac} -run "(Plan14V2|RequestPipeline|ConsumerResolution|AuthenticationState|MultiAuth|BeforeProxy|Reload|LegacyRemainder)" -count=3'
bash -lc 'source .envrc && golangci-lint run ./pkg/apisix/ctx/... ./pkg/plugin/... ./pkg/route/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

Later-plan API boundary scan over changed production files:

```bash
rg -n 'ResponseState|BufferedResponseExecutor|ResponsePhase|FinalResponseStore|StreamingBodyFilter|ExclusiveProtocol|LogSnapshot|RunLogPhase' pkg/apisix/ctx pkg/plugin pkg/route
```

Every match must be pre-existing and outside the changed Plan 14 paths; otherwise reject the diff.

Independent merge-level review must verify:

- exact 35+1 registry and exact 27/7/1/eight subsets;
- system/global rewrite before auth, then resolution before effective route rewrite;
- real JWT payload expansion and same-name consumer `proxy-rewrite` winner;
- clone-safe probe state and winner-only attachment;
- missing-group generic client failure and exact internal provenance;
- one effective deferred legacy chain after resolver, internal priority/unwind preservation, and explicitly changed auth-failure observation boundary;
- one `attach-consumer-label`, one finalize/before-proxy owner, and one terminal/upstream call;
- callback cleanup without deleting a still-used consumer override context seam;
- no later-plan API or completion overclaim.

After approval and fresh verification, commit only accepted Plan 14 paths with:

```text
refactor(plugin): execute consumer and access phases
```

Push one ready PR, wait for required CI on the unchanged head, and merge only when review and CI are green. Refresh the merged SHA before Plan 15 starts.

## Fast-plan-impl ownership

| WU | Exclusive ownership | Dependency |
| --- | --- | --- |
| WU-01 | `pkg/plugin/executor.go`, `pkg/plugin/executor_test.go`, `pkg/plugin/request_stage_registry.go`, `pkg/plugin/request_stage_registry_test.go`, `pkg/plugin/init_test.go`, `pkg/apisix/ctx/context.go`, `pkg/apisix/ctx/context_test.go`, capability manifest | merged Plan 13 |
| WU-02 | only the nine named `pkg/route` files in Tasks 3–4 | frozen WU-01 |
| WU-03 | only the seven named auth directories | frozen WU-01 |

WU-02 and WU-03 run in parallel. WU-01 does not delete callback symbols. Root alone performs final callback cleanup after both return. Workers do not commit, push, open PRs, edit peer paths, or delegate recursively.

## Explicit completion boundary

- Plan 14 intentionally changes deferred legacy pre-next observation for auth/resolver failures; it preserves only the effective chain's internal priority/unwind/transform behavior.
- Plan 15 owns bounded response phases and cache-store ordering.
- Plan 16 owns streaming, compression, protocol, and terminal closure.
- Plan 17 owns log/tracer/finalizer closure and the final 115/114 completeness proof.
- Plan 14 does not close PR-014, P1 5.5, or production readiness.
