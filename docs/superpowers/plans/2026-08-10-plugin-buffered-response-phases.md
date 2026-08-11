# Plugin Buffered Response Phases Implementation Plan v4

> Implement only after explicit authorization. If the user explicitly invokes `fast-plan-impl`, dispatch at most the three mutually exclusive work units below; otherwise execute the same units inline and sequentially. Workers may implement and run focused checks only. The phase owner retains integration, review, delivery, and CI authority.

**Goal:** Replace reverse middleware unwind for the exact ten Plan 15 factory identities with a bounded, fail-closed response pipeline that uses post-consumer winners, preserves legacy behavior when no bounded plan is selected, transports cache hits once, and stores only the final canonical representation.

**Architecture:** Extend the merged Plan 14 request pipeline with a checked config-aware binding constructor and one post-resolution/pre-terminal hook. `RequestPipeline.Then` owns a request-local switching writer that buffers only while the plan is undecided, becomes a bounded capture when the static or resolved plan requires Plan 15, or replays and becomes a transparent optional-interface-preserving pass-through before an ordinary terminal. Plan 15 runs source-eligible transforms, best-effort final stores, and a continuation-based final committer whose guarded `baseCommit` is the only operation allowed to replay the original capture and its private informational-response history.

**Tech stack:** Go 1.26, `net/http`, `github.com/felixge/httpsnoop`, merged Plan 12/13 lifecycle APIs, merged Plan 14 bindings/consumer pipeline, existing `pkg/plugin/base.BufferedResponseWriter`, bbolt-backed caches, and focused package/route tests.

## Frozen implementation base and manifest synchronization

- Implement on merged Plan 14 commit `b7d17c54138ae530f7f81a4908153615fae21fae` or a direct descendant whose Plan 14 APIs are unchanged.
- Before editing production code, fetch in a clean isolated worktree and prove:

```bash
git merge-base --is-ancestor b7d17c54138ae530f7f81a4908153615fae21fae HEAD
git diff --exit-code b7d17c54138ae530f7f81a4908153615fae21fae -- pkg/plugin/executor.go pkg/plugin/request_stage_registry.go pkg/plugin/base/writer.go pkg/route/builder.go
```

  The second command may show later changes; if it does, revalidate every referenced signature and revise this plan before implementation.
- The repository plan path is `docs/superpowers/plans/2026-08-10-plugin-buffered-response-phases.md`.
- The same PR updates `docs/superpowers/plans/2026-08-10-plugin-capability-manifest.md` as follows:
  1. replace its Plan 13 baseline line with `Implementation baseline through Plan 14: origin/master@b7d17c54138ae530f7f81a4908153615fae21fae`;
  2. add `final_response_store` to the capability vocabulary;
  3. make `proxy-cache` and `graphql-proxy-cache` own `request_access, conditional_terminal, final_response_store`, not header/body mutation;
  4. make `body-transformer` own `request_rewrite, buffered_body_filter`, selected independently by its request/response config;
  5. make `error-page`, `exit-transformer`, and `response-rewrite` own `buffered_body_filter` only because each atomically updates status/header/body in that callback;
  6. replace the existing serverless row's request-only restriction with the exact config-aware mapping frozen below: `rewrite` -> `request_rewrite`, default/`access` -> `request_access`, `before_proxy` -> `before_proxy`, `header_filter` -> `header_filter`, `body_filter` -> `buffered_body_filter`, and `log` -> deferred legacy log; the two exact serverless factory rows must use the same mapping;
  7. retain the ten-row Plan 15 identity set.
- A manifest test compares the Plan 15 production registry to those ten literal manifest rows. The plan document and manifest must contain the same pinned SHA before implementation review.
- Source `.envrc` before every Go/lint/build command. Do not run `go test ./...`, `go test ./pkg/...`, `make test`, `make test-integration`, or the whole `t/plugin` package.

## Exact Plan 15 identity boundary

Plan 15 owns exactly these ten canonical factory identities:

```text
api-breaker
body-transformer
echo
error-page
exit-transformer
graphql-proxy-cache
proxy-cache
response-rewrite
serverless-pre-function
serverless-post-function
```

Only the private factory/config key installed by the checked binding constructor selects registry behavior. An alias, `GetName()`, directory name, priority, config type, wrapper type, or callback assertion cannot grant an identity or phase.

Plan 15 does not implement compression, universal commit-header mutation, streaming body wrappers, SSE, gRPC framing, WebSocket/hijack terminals, protocol terminals, log snapshots, or generation lifecycle. Those owners are either transparent on requests with no Plan 15 selection or rejected by the exact conflict registry before their terminal runs.

## Frozen request order and response outcomes

### Resolved request

```text
outer EnableTerminal
  -> Plan 15 switching writer + request-local execution state + one cache-hit holder
  -> Plan 14 system/global rewrite and authentication
  -> resolve consumer/group exactly once
  -> merge global plus route/group/consumer winners
  -> checked post-resolution hook materializes exact request-local response plan
  -> reject bounded-owner conflicts before any post-resolution stage or terminal
  -> switch writer to bounded or transparent mode
  -> deferred legacy, route rewrite, consumer rewrite, access, before-proxy
  -> existing ai_runtime.TerminalHandler around the existing route terminal
  -> normal return
  -> if bounded: validate final request/source/holder/capture
  -> global header, merged header, global body, merged body
  -> recheck 4 MiB and terminal completeness
  -> final stores with frozen best-effort error policy
  -> final committer invokes guarded baseCommit exactly once
<- outer server classifies outcome, Finalize, then RecycleVars
```

### Authentication or resolver stop

The hook was not called. On return, the executor materializes the static global plus route/service/plugin-config plan, excludes consumer/group bindings, validates its conflicts, and then either executes the bounded plan or replays the undecided capture unchanged. Authentication Stop retains its declared source; resolver/materializer/internal failures use `ResponseSourceAPISIX` and replace still-uncommitted capture with the stable 500. No dynamic winner is invented.

### Panic, cancellation, and commit failure

- Panic, including `http.ErrAbortHandler`, escapes. No later transform/store/commit runs.
- After a normal return, nil `lifecycle.FinalRequest()` or a non-nil final request context error discards buffered state, skips stores/commit, and panics with `http.ErrAbortHandler`.
- Plan 15 never calls `SetOutcome`, `Finalize`, or `RecycleVars`.
- A client write failure after a successful store is not rollbackable. Do not claim rollback, transactional cache publication, or stable fallback after commitment.

## Import-cycle-safe checked binding and materialization contract

The root `pkg/plugin` package already imports `pkg/plugin/base`, while plugin implementations import `base`; therefore config descriptors live in `base` and contain no root-package type.

Add to `pkg/plugin/base/phase_descriptor.go`:

```go
type BindingPhaseDescriptor struct {
    RequestStage string
    Header       bool
    BufferedBody bool
}

type BindingPhaseDescriber interface {
    DescribeBindingPhases() (BindingPhaseDescriptor, error)
}
```

Allowed `RequestStage` strings are exactly `""`, `"none"`, `"rewrite"`, `"access"`, `"before_proxy"`, and `"legacy"`. Empty means the exact root registry supplies the stage; it never means “infer from interfaces.” `none` is a real response-only stage and must not enter Plan 14's legacy remainder.

Add `RequestStageNone` to the root enum and freeze:

```go
func ResolveRequestStage(
    factoryKey string,
    config any,
) (RequestStageSpec, bool, error)

func BindPluginChecked(
    factoryKey string,
    p Plugin,
    scope Scope,
    provenance ResourceProvenance,
) (Binding, error)
```

Checked-construction rules:

1. Reject nil plugins and empty factory keys with provenance-bearing internal errors.
2. Read the exact factory registry first. A descriptor cannot register an unknown identity or add a capability not allowed for that factory.
3. If the exact factory is config-sensitive, require `p.Config()` to implement `base.BindingPhaseDescriber`; validate its descriptor, resolve one request stage, and write it into `Binding.Stage` before returning.
4. `BindPluginChecked` is the sole production constructor in strict route, service, global, and consumer initialization. `BindPlugin` remains only as a compatibility helper for unchanged tests/legacy callers and is not used by production materialization.
5. All pipeline decisions consume the already checked `Binding.Stage`; do not call `RequestStageFor` again to override it. Static authentication flags still come from the exact registry.
6. Config-resolution failure fails route build for static bindings and fails consumer template initialization for dynamic bindings. No invalid binding enters a request pipeline or cache.

Config descriptors are exact:

| Exact config | Request stage | Response phase bits |
| --- | --- | --- |
| body transformer: request only | rewrite | none |
| body transformer: response only | none | buffered body |
| body transformer: request + response | rewrite | buffered body |
| echo: headers only | none | header |
| echo: any body field only | none | buffered body |
| echo: headers + body fields | none | header + buffered body |
| either serverless factory: default/access | access | none |
| either serverless factory: rewrite | rewrite | none |
| either serverless factory: before_proxy | before_proxy | none |
| either serverless factory: header_filter | none | header |
| either serverless factory: body_filter | none | buffered body |
| either serverless factory: log | legacy | none; conflicts with a bounded selection until Plan 17 |

The exact root request-stage registry entries for this plan are: API breaker, proxy cache, and GraphQL proxy cache are `access`; body transformer and both serverless factories are descriptor-controlled; echo, error page, exit transformer, and response rewrite are `none`. The response-only `none` value is checked and intentionally excluded from authentication, post-resolution request stages, and the legacy remainder.

`body_transformer.Config`, `echo.Config`, and `serverless.Config` implement the descriptor method as value-receiver, read-only logic over initialized config. The root registry cross-checks exact factory key and descriptor; the same serverless config is legal only for either exact serverless factory. Wrong config types, unsupported strings, or descriptor bits outside the factory mask return errors.

Metadata behavior is preserved in two layers:

- `metadataPlugin` and `metadataRequestPlugin` continue returning the underlying initialized config from `Config()`, so checked construction sees the descriptor through either wrapper.
- Both wrappers explicitly retain and forward only the callbacks selected by the root phase mask: `RunRequestPhase`, `RunHeaderFilter`, `RunBufferedBodyFilter`, `RunFinalResponseStore`, and `AppliesToResponseSource`. Wrapper assertions never grant a phase.
- `_meta.filter` is evaluated immediately before each selected callback; false is a no-op. `_meta.error_response` applies only to a request/local stop before downstream continuation, never to response callback errors, overflow, cancellation, or contract errors. `_meta.priority` remains the effective priority used for sorting.

## Effective winners and post-resolution hook

Add in `pkg/plugin/executor.go`:

```go
type EffectiveBindingSet struct {
    global []Binding
    merged []Binding
}

type PostResolutionHook func(
    *http.Request,
    EffectiveBindingSet,
) (*http.Request, error)

func (p RequestPipeline) ThenWithPostResolutionHook(
    terminal http.Handler,
    hook PostResolutionHook,
) http.Handler

func (p RequestPipeline) WithBufferedResponseExecutor(
    *BufferedResponseExecutor,
) RequestPipeline
```

Contract:

- Without an attached response executor, `Then` delegates to `ThenWithPostResolutionHook(terminal, nil)` with identical Plan 14 behavior.
- `WithBufferedResponseExecutor` returns a pipeline value with one immutable executor reference. Its `Then` owns switching-writer creation before authentication, calls the executor hook at the same post-resolution boundary, and calls executor completion only after a normal pipeline return. It does not use `defer` for transforms/commit, so terminal panic reaches the outer server with the real writer uncommitted.
- The set owns cloned slices and retains plugin, private factory key, checked stage, effective priority through the wrapped plugin, winning scope, and original provenance.
- The hook runs at most once after resolver success and merge, before legacy construction, route/consumer rewrite, access, before-proxy, or terminal.
- A nil hook result retains input; every replacement updates lifecycle final request.
- Hook failure sets APISIX source and stable uncommitted 500 and runs no post-resolution handler.
- Static authentication/resolver stops intentionally skip the hook.
- The executor retains request-local state directly; it never reconstructs winners from context after terminal return.

## Response state, callbacks, and exact materializer

Create `pkg/plugin/base/response_phase.go`:

```go
type BufferedResponseConfig struct {
    MaxBytes int64
}

type ResponseState struct {
    Status int
    Header http.Header
    Body   []byte
}

func CloneResponseState(ResponseState) ResponseState

type HeaderFilterPlugin interface {
    RunHeaderFilter(*http.Request, *ResponseState) error
}

type BufferedBodyFilterPlugin interface {
    RunBufferedBodyFilter(*http.Request, *ResponseState) error
}

type FinalResponseStorePlugin interface {
    RunFinalResponseStore(*http.Request, ResponseState) error
}

type ResponseEligibility interface {
    AppliesToResponseSource(ctx.ResponseSource) bool
}
```

`ResponseState` has exactly three fields. It has no request, source, writer, plugin, provenance, cache metadata, commit state, informational history, trailer, compression, or outcome.

Create `pkg/plugin/response_capability.go`:

```go
type ResponsePhaseMask uint8

const (
    ResponsePhaseHeader ResponsePhaseMask = 1 << iota
    ResponsePhaseBufferedBody
    ResponsePhaseFinalStore
)

type ResponseBinding struct {
    Plugin     Plugin
    Scope      Scope
    Provenance ResourceProvenance
    Phases     ResponsePhaseMask
    factoryKey string
}

func MaterializeResponseBindings(EffectiveBindingSet) ([]ResponseBinding, error)
```

Materialization reads only `Binding.factoryName`, checked `Binding.Stage`, initialized config descriptor, and the exact literal registry. It rejects descriptor/Binding disagreement, missing required callback, undeclared callback use, invalid config, and conflict entries with both provenances. Same-name consumer replacement is already complete; never re-merge. Keep global/merged partitions and sort descending effective priority within each partition/phase. Execute global then merged. Config selection happens once before terminal, never per callback.

Exact response manifest:

| Exact identity/config | Plan 15 phase | Eligible source |
| --- | --- | --- |
| api breaker | none | none |
| body transformer with response config | buffered body | Upstream, APISIX, EarlyStop |
| body transformer without response config | none | none |
| echo headers | header | Upstream, APISIX, EarlyStop |
| echo body fields | buffered body | Upstream, APISIX, EarlyStop |
| error page | one atomic buffered-body callback | APISIX, EarlyStop |
| exit transformer | one atomic buffered-body callback | APISIX, EarlyStop |
| response rewrite | one atomic buffered-body callback | Upstream, APISIX, EarlyStop |
| proxy cache | final store | Upstream |
| GraphQL proxy cache | final store | Upstream |
| serverless header_filter | header | Upstream, APISIX, EarlyStop |
| serverless body_filter | buffered body | Upstream, APISIX, EarlyStop |
| other serverless phase | none | none |

A declared callback without `ResponseEligibility` defaults to Upstream only. CacheHit is never eligible for Plan 15 transforms/stores.

## Request-local switching writer and exact route assembly

`RequestPipeline.Then`, when configured with `WithBufferedResponseExecutor`, creates one private execution state and returns a writer wrapper built with the repository's existing `httpsnoop.Wrap` dependency so the wrapper exposes exactly the same optional interfaces as the underlying writer. Route every supported hook through the shared state: `Header`, `WriteHeader`, `Write`, `WriteString`, `ReadFrom`, `Flush`, `FlushError`, `CloseNotify`, `Hijack`, `Push`, `SetReadDeadline`, `SetWriteDeadline`, and `EnableFullDuplex`. Do not use a concrete wrapper that falsely advertises an optional interface.

Freeze the route-terminal input and constructor:

```go
type TerminalOwner uint8

const (
    TerminalOwnerOrdinaryProxy TerminalOwner = iota
    TerminalOwnerGlobalNotFound
    TerminalOwnerAIRuntime
    TerminalOwnerKafka
    TerminalOwnerDubbo
    TerminalOwnerHTTPDubbo
)

type TerminalDescriptor struct {
    Owner      TerminalOwner
    Provenance ResourceProvenance
}

func NewBufferedResponseExecutor(
    static []Binding,
    terminal TerminalDescriptor,
    config base.BufferedResponseConfig,
) (*BufferedResponseExecutor, error)

func (e *BufferedResponseExecutor) PostResolutionHook(
    *http.Request,
    EffectiveBindingSet,
) (*http.Request, error)
```

The constructor clones and checked-materializes the static candidate but does not prematurely choose it for a resolved request. The hook replaces that candidate with the exact effective winner plan. A terminal descriptor is derived from existing route/upstream facts only; the always-installed `ai_runtime.TerminalHandler` wrapper does not by itself classify every route as an AI terminal.

The state machine is exact:

| State | Writes | Transition |
| --- | --- | --- |
| undecided | capture headers, private 1xx, final status, and body in one bounded recorder | initial when static candidate has no bounded phase/store |
| provisional-bounded | same capture, and optional operations already fail closed | initial when the static early-stop candidate has a bounded phase/store |
| transparent | replay captured calls once if any, then forward directly | hook selects no bounded plan, or early stop returns with no static bounded plan |
| bounded | retain capture; optional terminal operations are forbidden | hook selects a resolved bounded plan, or early stop retains provisional static plan |
| aborted | consume no further final work | panic, cancellation, hijack/flush conflict, or post-commit impossibility |

Rules:

1. Compute the static early-stop candidate before authentication. A selected static phase/store starts `provisional-bounded`; otherwise start `undecided`. This makes auth/resolver stops transformable without guessing a consumer.
2. Before the hook, an optional operation in `undecided` (`Flush`, `Hijack`, `Push`, or `ReadFrom` requiring streaming) switches to transparent, replays pending calls, and delegates. The same operation in `provisional-bounded` latches fail-closed unsupported state. If a later dynamic winner selects a bounded plan after transparent commitment, the hook rejects before terminal.
3. After the hook, a resolved plan replaces—not appends to—the static candidate. No bounded selection replays captured calls and switches to transparent before any post-resolution stage/terminal, even if the initial state was provisional. A bounded selection switches to final `bounded`. This is byte/status/header identical to existing behavior when no Plan 15 phase remains.
4. A bounded plan validates the exact identity/synthetic-terminal conflict registry in the hook before switching to bounded and before terminal execution. Unexpected bounded-mode Flush/Hijack/upgrade/trailer latches unsupported state; it never calls the underlying optional operation.
5. Auth/resolver stops are decided after pipeline return while still uncommitted. Apply the static plan if selected; otherwise replay exactly once. An empty undecided capture emits nothing.
6. Dynamic consumer/group winners replace route losers before response materialization. No static-plan response callback may leak into a resolved request.
7. No-plan requests never invoke Plan 15 transforms, stores, final committer, or body cap logic after switching transparent.

Both HTTP route construction sites preserve the existing AI context and terminal order exactly:

```go
executor, err := plugin.NewBufferedResponseExecutor(
    staticBindings,
    terminalOwner,
    base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
)
if err != nil {
    return nil, err
}

pipeline = pipeline.WithBufferedResponseExecutor(executor)
return ai_runtime.EnableTerminal(
    pipeline.Then(
        ai_runtime.TerminalHandler(handler),
    ),
), nil
```

For the global-not-found builder, `handler` is the existing not-found terminal and `terminalOwner` is the literal global-not-found owner. For ordinary routes, `handler` is the existing `buildReverseHandler` result and `terminalOwner` is derived from the existing route/upstream assembly, including AI runtime, Kafka, Dubbo/http-Dubbo, and ordinary proxy classifications. Do not remove, duplicate, move inside the pipeline, or bypass `ai_runtime.EnableTerminal` or `ai_runtime.TerminalHandler`.

## Response source ownership

Add `ResponseSourceAPISIX = "apisix"`. Lifecycle is authoritative; `$response_source` may only be a mirror written by the same setter and is never read by Plan 15.

| Outcome | Sole setter and timing |
| --- | --- |
| explicit request stop, including breaker/serverless | `base.AdaptRequestPhase`, after callback and before unwind; invalid source becomes EarlyStop |
| cache hit | cache request phase after holder publication, before Stop(CacheHit) |
| PURGE/local cache-only response | cache request phase before Stop(EarlyStop) |
| accepted proxy response | `newModifyResponse` sets Upstream before copy into capture |
| director/transport/timeout/overload/capacity failure | `newErrorHandler` sets APISIX before local JSON |
| request-body terminal 400/413 | terminal branch sets APISIX before JSON |
| global not found | global handler sets EarlyStop before write |
| resolver/hook/materializer/executor failure | failing owner sets APISIX before stable 500 |
| terminal returns with Unknown | nobody; executor replaces uncommitted bounded capture with stable 500 |

Delete Unknown-to-Upstream fallback. Never pre-arm Upstream and never set CacheHit again in the outer executor.

Stable 500 is status 500, `Content-Type: application/json; charset=UTF-8`, body `{"message":"Internal Server Error"}`. Stable 502 uses the same content type and body `{"message":"Bad Gateway"}`.

## Immutable cache hit and per-instance store intent

Add a private-state `CachedResponseState` and mutex-protected `CacheHitResponseHolder` with `Publish` and exactly-once `Consume`. Construction, publication, consumption, and callback delivery deep-copy headers/value slices/body. Missing publication marks consumed; a second consume always returns `ErrCacheHitResponseAlreadyConsumed`.

- CacheHit requires one published consume; every other source requires the not-published result. A mismatch becomes stable 500.
- A hit publishes before Stop(CacheHit), performs zero writer calls, and skips Plan 15 transforms/stores.
- A miss publishes nothing.
- Cache hits remain subject to final 4 MiB validation and go to the final-commit seam only when a bounded plan exists.

Each cache package owns a private request holder keyed by concrete `*Plugin`:

```go
type storeIntentHolder struct {
    mu       sync.Mutex
    intents  map[*Plugin]storeIntent
    consumed map[*Plugin]bool
}
```

A miss publishes one immutable key/Vary/TTL/policy/storage intent for that instance. Hit, bypass, PURGE, and cache-only miss publish none. `RunFinalResponseStore` consumes only its own intent once; missing is no-op; duplicate is an error. The executor supplies a deep-cloned final state. Persist only status/header/body and remove `Age`, `Apisix-Cache-Status`, and `APISIX-Cache-Key` case-insensitively. Preserve existing cache key, Vary, TTL, Cache-Control, Set-Cookie, PURGE, memory/disk ownership, and eviction semantics. No request pointer, writer, lifecycle, trailer, or body is stored in the intent.

## One 4 MiB capture, trailers, upgrade, and private 1xx

Freeze `DefaultBufferedResponseMaxBytes int64 = 4 << 20`; constructor rejects non-positive values. Exactly 4 MiB succeeds. The first byte beyond the cap latches overflow, discards final buffered representation, returns original input length with nil error, and consumes later writes. Recheck transformed output and cache-hit size. Overflow invokes no phase/store and commits only stable 502 through the direct final path.

Do not add trailer or informational fields/APIs. Status 101, a `Trailer` declaration, or a `Trailer:`-prefixed field fails closed with stable 502 before transform/store/commit. HEAD/204/304 suppression continues through `base.ResponseAllowsBody`; body replacement uses `base.InvalidateBodyDerivedHeaders`.

The existing capture's private informational history must survive final-state changes. Add this writer-owned primitive, which replaces only final status/header/body with deep copies, preserves request method and private informationals, and then uses the existing commit path:

```go
func (w *BufferedResponseWriter) CommitFinalResponse(
    http.ResponseWriter,
    ResponseState,
)
```

`Reset` remains inappropriate because it clears informationals. HEAD/204/304 suppression therefore remains in the existing writer commit path.

## Exactly-once continuation-based final commit

Create in `pkg/plugin/response_executor.go`:

```go
type BaseCommit func(
    http.ResponseWriter,
    *base.ResponseState,
)

type FinalResponseCommitter interface {
    CommitFinalResponse(
        http.ResponseWriter,
        *http.Request,
        *base.ResponseState,
        BaseCommit,
    )
}

func (e *BufferedResponseExecutor) WithFinalResponseCommitter(
    FinalResponseCommitter,
) *BufferedResponseExecutor
```

The executor is not a free-standing middleware wrapper; `RequestPipeline.Then` is its sole runtime owner. `WithFinalResponseCommitter` returns an immutable executor clone for focused tests/later composition. The executor supplies a guarded request-bound `baseCommit`. A committer may mutate `ResponseState` and wrap or replace the destination writer, but must call the provided continuation exactly once before returning. The continuation rejects a second call and invokes the original capture's `CommitFinalResponse` once. After the committer returns, zero calls are an internal contract panic; panic escapes and no fallback is attempted. The default direct committer immediately calls `baseCommit`. No later-plan type or registry is introduced.

Tests must prove one 103 captured before the final response is replayed once and in order through both direct and custom committers; a custom writer wrapper still receives the final response; zero and double continuation calls fail deterministically without a second final write.

## Exact bounded-owner conflict registry

If a request selects at least one Plan 15 phase/store, reject these exact effective identities before terminal: `ai-aliyun-content-moderation`, `ai-rate-limiting`, `brotli`, `gzip`, `cors`, `grpc-transcode`, `grpc-web`, `proxy-buffering`, `ai-proxy`, `ai-proxy-multi`, `mcp-bridge`, `dubbo-proxy`, `http-dubbo`, `kafka-proxy`, `aws-lambda`, `azure-functions`, `fault-injection`, `mocking`, `openfunction`, `openwhisk`, `public-api`, and `redirect`; also reject either serverless factory configured for `log`. `mqtt-proxy` is never an HTTP candidate.

Synthetic terminal owners are literal: ordinary reverse proxy and global not-found are allowed; selected AI runtime execution, Kafka upstream scheme, Dubbo, and http-Dubbo are rejected. Every error names bounded identity/provenance and conflicting identity/owner/provenance. The registry has literal-set tests; unknown owners fail closed when a bounded plan is selected. With no bounded selection these owners retain current behavior.

## Pre-store gate and honest final-store error policy

Before any store require normal return, non-nil uncancelled final request, canonical non-Unknown source, valid holder/source relation, complete non-overflow capture, final non-101 status, no trailer declaration, all transforms successful, and transformed body at most 4 MiB.

`RunFinalResponseStore` errors are non-transactional cache side-effect failures. Freeze this minimal policy:

1. Run all eligible stores in deterministic global-then-merged priority order, each with a deep clone.
2. On returned error, emit one bounded diagnostic containing exact factory/provenance, continue remaining stores, and commit the unchanged final response once.
3. Never replace the client response with stable 500 for a returned store error; never claim rollback of an earlier successful store.
4. A store panic is not converted to an error: it escapes, later stores and final commit do not run, and any earlier store side effect remains possible and explicitly non-rollbackable.
5. A commit/client-write failure after stores also has no rollback.

Tests cover first-store error then second-store success, all-store errors, unchanged one-time client commit, no fallback response, sanitized diagnostics, panic after one successful store, and absence of rollback claims/operations.

Lifecycle order remains: request finalizer registration → terminal/capture → transforms → completeness gate → stores → final commit → outer outcome classification → `SetOutcome` → `Finalize` → `RecycleVars`. API breaker open registers no observer; continue registers one idempotent finalizer that observes only completed, committed, non-hijacked HTTP outcomes.

## Work unit 1 — Core contracts, checked binding, switching executor

**Dependency:** clean worktree at the frozen base. **Exclusive ownership:**

- `pkg/apisix/ctx/lifecycle.go`, `pkg/apisix/ctx/lifecycle_test.go`
- `pkg/plugin/base/phase_descriptor.go`, `pkg/plugin/base/phase_descriptor_test.go`
- `pkg/plugin/base/request_phase.go`, `pkg/plugin/base/request_phase_test.go`
- `pkg/plugin/base/response_phase.go`, `pkg/plugin/base/response_phase_test.go`
- `pkg/plugin/base/writer.go`, `pkg/plugin/base/writer_test.go`
- `pkg/plugin/executor.go`, `pkg/plugin/executor_test.go`
- `pkg/plugin/request_stage_registry.go`, `pkg/plugin/request_stage_registry_test.go`
- `pkg/plugin/response_capability.go`, `pkg/plugin/response_capability_test.go`
- `pkg/plugin/response_executor.go`, `pkg/plugin/response_executor_test.go`
- the two exact plan/manifest documents named above

Write red tests first:

```go
func TestBindPluginCheckedWritesConfigAwareStage(t *testing.T)
func TestBindPluginCheckedRejectsDescriptorOutsideExactFactoryMask(t *testing.T)
func TestRequestPipelineUsesCheckedBindingStageWithoutReresolving(t *testing.T)
func TestPostResolutionHookRunsAfterWinnerMergeBeforeAnyLaterStage(t *testing.T)
func TestAuthAndResolverStopsDoNotInvokePostResolutionHook(t *testing.T)
func TestEffectiveBindingSetClonePreservesPrivateFactoryStageScopeProvenance(t *testing.T)
func TestMaterializeResponseBindingsUsesPrivateFactoryIdentity(t *testing.T)
func TestPlan15ManifestAndRegistryHaveExactTenIdentities(t *testing.T)
func TestSwitchingWriterTransparentModePreservesEveryUnderlyingOptionalInterfaceAndBytes(t *testing.T)
func TestSwitchingWriterDynamicBoundedWinnerDecidesBeforeTerminal(t *testing.T)
func TestCacheHitHolderDeepCopiesAndConsumesExactlyOnce(t *testing.T)
func TestBufferedResponseAcceptsExactFourMiB(t *testing.T)
func TestBufferedResponseCapPlusOneReturnsStable502WithoutCallbacks(t *testing.T)
func TestFinalCommitterBaseCommitPreservesPrivate103AndRunsOnce(t *testing.T)
func TestFinalCommitterRejectsZeroAndDoubleBaseCommit(t *testing.T)
func TestFinalStoreErrorsContinueAndCommitUnchangedResponse(t *testing.T)
func TestFinalStorePanicDoesNotClaimOrAttemptRollback(t *testing.T)
```

Red/green and race gates:

```bash
bash -lc 'source .envrc && go test ./pkg/apisix/ctx ./pkg/plugin/base ./pkg/plugin -run "(BindPluginChecked|CheckedBindingStage|PostResolutionHook|EffectiveBindingSet|MaterializeResponseBindings|Plan15Manifest|SwitchingWriter|CacheHitHolder|BufferedResponse|FinalCommitter|FinalStore)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/apisix/ctx ./pkg/plugin/base ./pkg/plugin -run "(PostResolutionHook|EffectiveBindingSet|SwitchingWriter|CacheHitHolder|BufferedResponse|FinalCommitter|FinalStore)" -count=3'
git diff --check
```

Freeze WU-01 signatures only after those pass. Tests use fake descriptor configs; production config implementations belong to WU-02.

## Work unit 2 — Migrate the exact ten implementations

**Dependency:** frozen WU-01 interfaces. **Exclusive ownership:** production/tests/benchmarks only under:

- `pkg/plugin/api_breaker/**`
- `pkg/plugin/body_transformer/**`
- `pkg/plugin/echo/**`
- `pkg/plugin/error_page/**`
- `pkg/plugin/exit_transformer/**`
- `pkg/plugin/graphql_proxy_cache/**`
- `pkg/plugin/proxy_cache/**`
- `pkg/plugin/response_rewrite/**`
- `pkg/plugin/serverless/**`

Write red tests first:

```go
func TestBodyTransformerDescriptorSeparatesRequestAndResponse(t *testing.T)
func TestEchoDescriptorSelectsHeaderAndBodyExactly(t *testing.T)
func TestServerlessDescriptorSelectsOneConfiguredStageOrPhase(t *testing.T)
func TestErrorPageRunsOneAtomicBufferedBodyCallback(t *testing.T)
func TestExitTransformerRunsOneAtomicBufferedBodyCallback(t *testing.T)
func TestResponseRewriteRunsOneAtomicBufferedBodyCallback(t *testing.T)
func TestMigratedHandlersHaveNoDuplicatePostNextResponseWork(t *testing.T)
func TestAPIBreakerFinalizerObservesOnlyCompletedCommittedNonHijacked(t *testing.T)
func TestProxyCacheHitPublishesWithoutWriterAndReturnsCacheHitStop(t *testing.T)
func TestGraphQLCacheHitPublishesWithoutWriterAndReturnsCacheHitStop(t *testing.T)
func TestProxyCacheStoreIntentIsPerPluginInstanceAndConsumedOnce(t *testing.T)
func TestGraphQLCacheStoreIntentIsPerPluginInstanceAndConsumedOnce(t *testing.T)
func TestCacheFinalStorePersistsCanonicalStateWithoutDerivedHeaders(t *testing.T)
func TestCachePolicyVaryTTLSetCookiePurgeAndStorageOwnershipRemain(t *testing.T)
```

Focused gates:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{body_transformer,echo,error_page,exit_transformer,response_rewrite,serverless} -run "(Descriptor|AtomicBufferedBody|ConfiguredStageOrPhase|DuplicatePostNext)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/{proxy_cache,graphql_proxy_cache} -run "(CacheHit|StoreIntent|FinalStore|CanonicalState|Vary|TTL|SetCookie|Purge|StorageOwnership)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/api_breaker -run "(APIBreaker|Finalizer|Outcome)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/{api_breaker,body_transformer,echo,error_page,exit_transformer,graphql_proxy_cache,proxy_cache,response_rewrite,serverless} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/{api_breaker,graphql_proxy_cache,proxy_cache,serverless} -run "(Finalizer|CacheHit|StoreIntent|FinalStore|Descriptor)" -count=3'
```

Owned-path scan:

```bash
rg -n 'next\.ServeHTTP|GetOrCreateTransformResponseWriter|WithTransformPipeline|DescribeBindingPhases|RunHeaderFilter|RunBufferedBodyFilter|RunFinalResponseStore|CacheHitResponseHolder|storeIntent' pkg/plugin/{api_breaker,body_transformer,echo,error_page,exit_transformer,graphql_proxy_cache,proxy_cache,response_rewrite,serverless}
```

Every remaining `next.ServeHTTP` is an explicit request compatibility or serverless-log path. Cache holders appear only in the two cache lookups; store intents stay private and instance-keyed.

## Work unit 3 — Route wrappers, source owners, exact assembly, closure

**Dependencies:** accepted WU-01 and WU-02. **Exclusive ownership:**

- `pkg/route/builder.go`
- `pkg/route/coverage_helpers_test.go`
- `pkg/route/proxy_control_test.go`
- `pkg/route/builder_lifecycle_test.go`
- `pkg/route/request_phase_metadata_test.go`
- `pkg/route/buffered_response_phase_test.go`
- `pkg/route/buffered_response_source_test.go`
- `pkg/route/buffered_response_cache_test.go`
- `pkg/route/buffered_response_conflict_test.go`

Write red tests first:

```go
func TestStrictRouteMaterializationUsesBindPluginCheckedForStaticAndConsumer(t *testing.T)
func TestMetadataWrappersForwardOnlyRegistryDeclaredRequestAndResponseInterfaces(t *testing.T)
func TestMetadataResponseFilterPriorityAndErrorResponseRemainExact(t *testing.T)
func TestResolvedConsumerResponseWinnerMaterializesBeforeRouteStageAndRunsOnce(t *testing.T)
func TestAuthStopUsesStaticPlanWithoutConsumerBinding(t *testing.T)
func TestNoBoundedPlanPreservesFlushHijackPushReaderFromAndAIAssembly(t *testing.T)
func TestDynamicBoundedWinnerRejectsConflictBeforeTerminal(t *testing.T)
func TestModifyResponseSetsUpstreamBeforeBoundedCapture(t *testing.T)
func TestErrorAndRequestBodyHandlersSetAPISIXBeforeJSON(t *testing.T)
func TestGlobalNotFoundSetsEarlyStopBeforeWrite(t *testing.T)
func TestTerminalHandlerDoesNotDefaultUnknownToUpstream(t *testing.T)
func TestBufferedRouteCacheHitConsumesOnceAndSkipsTransformsStores(t *testing.T)
func TestBufferedRouteCacheMissStoresAfterTransformsPerInstance(t *testing.T)
func TestBufferedRouteFinalStoreErrorCommitsUnchangedOnce(t *testing.T)
func TestBufferedRouteFinalCommitterPreserves103AndRunsBaseCommitOnce(t *testing.T)
func TestBufferedRouteFinalizerRunsAfterOutcomeBeforeRecycle(t *testing.T)
```

Focused gates; regexes intentionally contain the complete test-name stems:

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "(StrictRouteMaterialization|MetadataWrappers|MetadataResponse|ResolvedConsumerResponseWinner|AuthStopUsesStaticPlan|NoBoundedPlan|DynamicBoundedWinner|SetsUpstream|SetAPISIX|GlobalNotFound|DoesNotDefaultUnknown|BufferedRoute|FinalCommitter|Finalizer)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/route -run "(ResolvedConsumerResponseWinner|DynamicBoundedWinner|BufferedRoute|CacheHit|StoreIntent|LifecycleSource|FinalCommitter)" -count=3'
bash -lc 'source .envrc && go test ./pkg/route -count=1'
```

## Integration acceptance on one unchanged diff

Run in order. Any edit restarts affected gates:

```bash
bash -lc 'source .envrc && go test ./pkg/apisix/ctx ./pkg/plugin/base ./pkg/plugin -run "(BindPluginChecked|CheckedBindingStage|PostResolutionHook|EffectiveBindingSet|MaterializeResponseBindings|Plan15Manifest|BoundedConflict|ResponseSource|ResponseState|SwitchingWriter|CacheHitHolder|BufferedResponse|FinalCommitter|FinalStore)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/apisix/ctx ./pkg/plugin/base ./pkg/plugin -run "(PostResolutionHook|EffectiveBindingSet|SwitchingWriter|CacheHitHolder|BufferedResponse|FinalCommitter|FinalStore|ResponseSource)" -count=3'
bash -lc 'source .envrc && go test ./pkg/plugin/{api_breaker,body_transformer,echo,error_page,exit_transformer,graphql_proxy_cache,proxy_cache,response_rewrite,serverless} -count=1'
bash -lc 'source .envrc && go test ./pkg/route -run "(StrictRouteMaterialization|MetadataWrappers|MetadataResponse|ResolvedConsumerResponseWinner|AuthStopUsesStaticPlan|NoBoundedPlan|DynamicBoundedWinner|LifecycleSource|BufferedRoute|FinalCommitter|Finalizer)" -count=1'
bash -lc 'source .envrc && go test ./pkg/route -count=1'
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build'
git diff --check
git status --short
git diff --name-only
```

Default is no `t/plugin`. Add only one exact named real-process case when package/route seams cannot prove changed behavior; state why and never run concurrent real-process cases.

Duplicate-owner/dead compatibility scan:

```bash
rg -n 'BindPlugin\(|BindPluginChecked|RequestStageFor|ResolveRequestStage|RunHeaderFilter|RunBufferedBodyFilter|RunFinalResponseStore|BufferedResponseExecutor|PostResolutionHook|WithTransformPipeline|GetOrCreateTransformResponseWriter|\$response_source|ResponseSource|CacheHitResponseHolder|CachedResponseState|storeIntent|EnableTerminal|TerminalHandler' pkg/plugin pkg/route pkg/server pkg/proxy pkg/observability
```

Classify every match. Acceptance requires checked production bindings, one owner per phase, one hit holder, package-private per-instance intents, no Unknown fallback, no `$response_source` decisions, exact AI assembly, and no later-plan API beyond the Plan 15 final-commit seam. Because request stages/callbacks are moved or deleted, also list changed symbols and run `rg` production/test call-site scans; remove proxy-only/dead methods caused by this change and report the scan.

## Independent review and one-PR delivery

After all gates pass on the unchanged diff:

1. Independent merge-level review covers checked descriptors/stages, hook timing, switching optional interfaces, AI assembly, factory identity, wrapper forwarding, source owners, cache isolation, cap/private-1xx behavior, exactly-once continuation, store-error policy, and pre-store abort gate.
2. Resolve every High/Medium finding and rerun affected gates plus final lint/build/diff checks.
3. Confirm only the three ownership sets changed; classify extras before staging.
4. Stage accepted paths only; branch `codex/plugin-buffered-response-phases`; commit `refactor(plugin): execute buffered response phases`.
5. Push one branch and open one ready PR based on the pinned merged Plan 14 descendant.
6. Wait for required CI on the unchanged head; merge only after review and CI green. Record the merge SHA for Plan 16.

Workers never commit, push, open/comment on PRs, or delegate recursively.

## Work-unit ownership intersection audit

| Pair | Intersection | Result |
| --- | --- | --- |
| WU-01 and WU-02 | empty: ctx/base/root-plugin/docs versus nine plugin directories | pass |
| WU-01 and WU-03 | empty: ctx/base/root-plugin/docs versus named route files | pass |
| WU-02 and WU-03 | empty: nine plugin directories versus named route files | pass |

If a unit needs a path outside its ownership, stop and return the path/reason to the phase owner. Do not widen silently.

## Finding closure matrix

| Finding | Disposition | Executable closure |
| --- | --- | --- |
| v3 review: base not pinned/manifest unsynced | closed | exact merged SHA and five manifest edits frozen |
| v3 review: config-aware stage not written into Binding | closed | import-safe descriptor plus `BindPluginChecked` writes and pipeline trusts `Binding.Stage` |
| v3 review: body/echo/serverless and wrappers unspecified | closed | exact config table, required descriptor implementations, wrapper config/callback forwarding |
| v3 review: final committer loses private 1xx | closed | guarded request-bound `baseCommit` updates original capture then calls existing Commit |
| v3 review: dynamic winner conflicts with legacy transparency | closed | undecided/bounded/transparent writer and pre-terminal hook decision |
| v3 review: AI runtime/terminal assembly omitted | closed | exact outer `EnableTerminal(pipeline.Then(TerminalHandler))` composition retained |
| v3 review: focused regex misses identity/winner tests | closed | full `MaterializeResponseBindings` and `ResolvedConsumerResponseWinner` stems included |
| v3 review: nonstandard final lint | closed | final gate is `source .envrc && make lint` |
| v3 review: store errors ambiguous/nonrollbackable | closed | best-effort returned-error policy, fatal panic, exact tests, no rollback claim |
| cross-review H1 dynamic effective bindings | closed | clone-safe global/merged set and post-resolution/pre-terminal hook |
| cross-review H2 config-aware request-stage API | closed | exact resolver and checked constructor share one descriptor |
| cross-review H3 competing response sources | closed | sole setter table; no pre-arm or outer cache-hit reset |
| cross-review H4 CORS/auth order | protected | CORS conflicts with bounded selection; no Plan 15 CORS API |
| cross-review H5 gzip/Brotli duplicate owner | protected | exact conflicts; no Plan 15 compression/stream phase |
| cross-review H6 header versus universal commit-header | protected | bounded header only; continuation is generic and grants no phase |
| cross-review H7 late preparation after commit | protected | all Plan 15 materialization/conflict checks occur before terminal; no log prep |
| cross-review M1 Plan 14 order drift | closed | frozen order copies merged Plan 14 and inserts hook at one exact boundary |
| cross-review M2 self-referential repository completeness | bounded | literal ten-row equality only; no 115/114 closure claim |
| cross-review M3 unauthorized delegation | closed | banner requires explicit authorization; inline path valid |
| cross-review M4 later-plan delivery gap | not Plan 15-owned | this plan has its own one-PR endgame and records next merge SHA |
| brief: exact identity/wrapper/conflict boundaries | closed | private key, checked masks, wrapper forwarding, literal identity/synthetic registries |
| brief: cache/cap/abort/trailer/store closure | closed | one-consume holder, per-instance intent, 4 MiB gates, no trailer API, honest stores |

## Self-audit and acceptance boundary

Accept only if the copied repository artifact still has:

- the exact ten identities in the boundary block;
- exactly twenty-two closure rows;
- exactly three mutually exclusive work units;
- zero unresolved base tokens;
- balanced fences;
- no later-plan symbol except the generic callable final-commit seam;
- no invented trailer/informational state field;
- complete test regex coverage for every named focused regression;
- no implementation or delivery action implied merely by this plan.

Artifact audit:

```bash
test "$(awk '/^## Exact Plan 15 identity boundary/{in_section=1} in_section && /^```text$/{in_block=1;next} in_block && /^```$/{exit} in_block && NF{n++} END{print n+0}' /tmp/apisix-plan15-revised-v4.md)" = 10
test "$(awk '/^## Finding closure matrix/{in_section=1;next} in_section && /^## /{exit} in_section && /^\| [^ -]/{n++} END{print n-1}' /tmp/apisix-plan15-revised-v4.md)" = 22
test "$(rg -n '^## Work unit [123] ' /tmp/apisix-plan15-revised-v4.md | wc -l | tr -d ' ')" = 3
test "$(rg -n '^```' /tmp/apisix-plan15-revised-v4.md | wc -l | tr -d ' ')" -gt 0
test "$(( $(rg -n '^```' /tmp/apisix-plan15-revised-v4.md | wc -l | tr -d ' ') % 2 ))" = 0
! rg -n 'PLAN14_MERGE_''SHA_TO_PIN|T''BD|TO''DO' /tmp/apisix-plan15-revised-v4.md
shasum -a 256 /tmp/apisix-plan15-revised-v4.md
```

This PR closes only bounded HTTP response ownership for Plan 15 and its effective-binding/cache-store contracts. It does not close streaming/protocol/compression, CORS universal headers, logger/tracer phases, serverless log migration, repository-wide capability completeness, PR-014, or overall production readiness.
