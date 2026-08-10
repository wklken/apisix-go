# Plugin Buffered Response Phases Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace legacy reverse middleware unwind for bounded HTTP responses with explicit APISIX-ordered header-filter and body-filter phases while preserving representation safety and early-response behavior.

**Architecture:** The request pipeline selects a bounded buffered response mode only when its materialized plugin bindings require buffered header/body phases and no streaming-terminal capability is present. One shared recorder captures the terminal response, then global bindings followed by merged route/consumer bindings run header and body phases in descending priority. Plugins can implement more than one phase; request behavior remains in the request executor and response behavior is not duplicated in legacy middleware. Stable panic responses from the outer boundary bypass configurable transforms.

**Tech Stack:** Go 1.26, `pkg/plugin/base.BufferedResponseWriter`, representation helpers from PR #84, explicit request pipeline, route builder binding provenance.

## Global Constraints

- Depends on the merged consumer/access executor.
- Pinned order after upstream or an early request stop: global header-filter → merged route header-filter → global body-filter → merged route body-filter → log.
- Within each scope/phase priority is descending. Do not preserve legacy reverse-unwind order when it contradicts this phase contract.
- Authentication, rate-limit, validation, and fault responses enter the same response phases when the configured response plugin is eligible for rejection responses.
- The outer stable pre-commit panic JSON is not transformed, cached, compressed, or status-rewritten.
- Use one bounded recorder per request, not one recorder per plugin. This PR owns `DefaultBufferedResponseLimit = 8 << 20` bytes as an internal safety default and adds no operator config surface. A write crossing the cap stores no bytes beyond it and ultimately replaces the still-uncommitted response with stable `502` JSON; it never passes through and is never cached.
- Preserve `103` informational responses, final `101/204/304`, HEAD semantics, trailers, repeated headers, `Vary`, and the body-derived header invalidation contract from the HTTP representation PR.
- `proxy-cache`/`graphql-proxy-cache` lookup remains request-side; only cache-store/final response work moves to response phases. A cache hit is a terminal response that still runs later eligible response phases exactly once.
- Compression, grpc-web, websocket/hijack, SSE/flush, and other streaming writers are excluded and remain legacy until the next plan.
- This PR is partial PR-014 migration; it does not close streaming or log phase gaps.
- The exact ten primary identities and their multi-capabilities are the Plan 15 section of `2026-08-10-plugin-capability-manifest.md`; registry tests reject drift.

---

### Task 1: Define child-safe header and buffered-body contracts

**Files:**
- Create: `pkg/plugin/base/response_phase.go`
- Create: `pkg/plugin/base/response_phase_test.go`
- Modify: `pkg/plugin/base/writer.go`
- Modify: `pkg/plugin/base/writer_test.go`

**Interfaces:**

```go
type BufferedResponse struct {
    Status  int
    Header  http.Header
    Body    []byte
    Trailer http.Header
    Source  ctx.ResponseSource
}

type HeaderFilterPlugin interface {
    RunHeaderFilter(*http.Request, *BufferedResponse) error
}

type BufferedBodyFilterPlugin interface {
    RunBufferedBodyFilter(*http.Request, *BufferedResponse) error
}

type ResponseEligibility interface {
    AppliesToResponseSource(ctx.ResponseSource) bool
}

const DefaultBufferedResponseLimit int64 = 8 << 20
var ErrBufferedResponseLimit = errors.New("buffered response exceeds limit")
```

The executor owns the response value. Plugins may mutate it but must not retain the body or header maps after returning. A plugin without `ResponseEligibility` applies to upstream and cache-hit responses but not early stops. Response-rewrite, error-page, body-transformer, exit-transformer, echo, and configured serverless response phases explicitly opt into early-stop responses; proxy-cache/graphql-proxy-cache storage explicitly do not. Stable panic output is always ineligible. Errors fail closed before commit with the stable internal-error response.

- [ ] **Step 1: Write invariants and bodyless regressions**

Cover deep-cloned headers, repeated values, informational-to-final status, HEAD, `101/204/304`, trailers, and `ReplaceBody` invalidating `Content-Length`, `Content-Encoding`, ranges, digests, and strong validators.

- [ ] **Step 2: Run tests and record compile-red**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base -run "(ResponsePhase|BufferedResponse|ReplaceBody)" -count=1'
```

- [ ] **Step 3: Implement minimal immutable ownership helpers**

Reuse existing writer/representation functions. Do not create a second header invalidation list. `ResponseAllowsBody` remains the single source for bodyless decisions.

- [ ] **Step 4: Run base package and race tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/base -run "(ResponsePhase|BufferedResponse)" -count=3'
```

### Task 2: Add one bounded response executor

**Files:**
- Create: `pkg/plugin/response_executor.go`
- Create: `pkg/plugin/response_executor_test.go`
- Modify: `pkg/plugin/executor.go`
- Modify: `pkg/plugin/request_stage_registry.go`

**Interfaces:**

```go
type ResponseBinding struct {
    Plugin     Plugin
    Scope      Scope
    Provenance ResourceProvenance
}

type BufferedResponseExecutor struct { /* cloned phase slices and byte cap */ }

func NewBufferedResponseExecutor(bindings []ResponseBinding) BufferedResponseExecutor
func (e BufferedResponseExecutor) Wrap(terminal http.Handler) http.Handler
```

- [ ] **Step 1: Add exact order and early-response tests**

Add:

```go
func TestBufferedResponseExecutorRunsGlobalThenMergedByPhase(t *testing.T)
func TestBufferedResponseExecutorRunsOnEarlyAuthenticationResponse(t *testing.T)
func TestBufferedResponseExecutorRunsEachMultiPhasePluginOncePerPhase(t *testing.T)
func TestBufferedResponseExecutorBypassesStablePanicResponse(t *testing.T)
func TestBufferedResponseExecutorDoesNotCommitPartialTransformError(t *testing.T)
func TestBufferedResponseExecutorEnforcesBodyLimit(t *testing.T)
```

Use priorities that conflict across scopes and phases. Assert header phase completes for every binding before any body phase begins.

- [ ] **Step 2: Run focused tests and record red behavior**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "^TestBufferedResponseExecutor" -count=1'
```

- [ ] **Step 3: Implement a single recorder and explicit phase loops**

Build global and merged route/consumer slices separately, stable-sort each by effective priority, and remove migrated response work from the legacy remainder. Buffer only final response bytes; forward informational headers immediately without merging them into the final header. On cap overflow remember `ErrBufferedResponseLimit`, discard later bytes, let terminal execution return, suppress all transforms/cache store, and commit one stable 502 response. A private constructor accepts a smaller cap for deterministic tests; production always uses the constant. Builder validation separately rejects known streaming/hijack combinations because runtime body length cannot be known at build time.

- [ ] **Step 4: Run executor regression and race tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "(BufferedResponseExecutor|RequestPipeline|LegacyRemainder)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin -run "BufferedResponseExecutor" -count=3'
```

### Task 3: Migrate bounded transforms and api-breaker outcome observation

**Files:**
- Modify: `pkg/plugin/response_rewrite/plugin.go`
- Modify: `pkg/plugin/response_rewrite/plugin_test.go`
- Modify: `pkg/plugin/error_page/plugin.go`
- Modify: `pkg/plugin/error_page/plugin_test.go`
- Modify: `pkg/plugin/body_transformer/plugin.go`
- Modify: `pkg/plugin/body_transformer/plugin_test.go`
- Modify: `pkg/plugin/exit_transformer/plugin.go`
- Modify: `pkg/plugin/exit_transformer/plugin_test.go`
- Modify: `pkg/plugin/echo/plugin.go`
- Modify: `pkg/plugin/echo/plugin_test.go`
- Modify: `pkg/plugin/serverless/plugin.go`
- Modify: `pkg/plugin/serverless/plugin_test.go`
- Modify: `pkg/plugin/api_breaker/plugin.go`
- Modify: `pkg/plugin/api_breaker/plugin_test.go`

- [ ] **Step 1: Add per-plugin phase regressions before implementation**

For each transform plugin, prove the migrated Handler no longer performs post-`next` response work while the explicit response interface produces the same isolated result. Add combined tests for response-rewrite + error-page + body-transformer with conflicting priorities, early key-auth rejection, and bodyless statuses. `api-breaker` is not a header/body filter: migrate its request gate plus one lifecycle finalizer that observes the final outcome status exactly once.

- [ ] **Step 2: Run the plugin matrix and record current reverse order**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{response_rewrite,error_page,body_transformer,exit_transformer,echo,serverless,api_breaker} -run "(ResponsePhase|HeaderFilter|BodyFilter|EarlyResponse)" -count=1'
```

- [ ] **Step 3: Implement explicit interfaces with no duplicate legacy work**

Request-capable plugins keep their request interface. Response logic moves into header/body methods. A direct legacy Handler test may use `base.AdaptBufferedResponsePlugin` only as a package-local compatibility boundary; production route assembly must not install both forms. Serverless `phase=log` is declared but remains inert in the buffered executor until Plan 17 supplies `LogPhasePlugin`.

- [ ] **Step 4: Run complete affected plugin packages**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{response_rewrite,error_page,body_transformer,exit_transformer,echo,serverless,api_breaker} -count=1'
```

### Task 4: Split cache lookup from cache-store response phases

**Files:**
- Modify: `pkg/plugin/proxy_cache/plugin.go`
- Modify: `pkg/plugin/proxy_cache/plugin_test.go`
- Modify: `pkg/plugin/graphql_proxy_cache/plugin.go`
- Modify: `pkg/plugin/graphql_proxy_cache/plugin_test.go`

- [ ] **Step 1: Add cache-hit/miss phase tests**

Assert lookup runs in request stage; hit stops upstream and enters later response phases; miss stores the fully transformed representation at the declared cache phase exactly once; `Vary` variants and PURGE remain unchanged. Stable panic and aborted/oversized responses are never stored.

- [ ] **Step 2: Run focused cache tests before edits**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache -run "(ResponsePhase|CacheHit|CacheMiss|Vary|Purge)" -count=1'
```

- [ ] **Step 3: Implement request lookup plus response store contracts**

Keep disk/memory owner and signature format unchanged. Cache store is a distinct final-representation step after every eligible header/body transform, not an unordered peer body filter; pin and test this order explicitly.

- [ ] **Step 4: Run both complete cache packages**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache -count=1'
```

### Task 5: Integrate response bindings in route generations

**Files:**
- Modify: `pkg/route/builder.go`
- Create: `pkg/route/buffered_response_phase_test.go`
- Modify: `pkg/route/builder_lifecycle_test.go`

- [ ] **Step 1: Add route-combination regressions**

Cover global/route/consumer order, same-name consumer override, early auth/limit response, cache hit, transform error, panic bypass, HEAD, and `103 -> 200`.

- [ ] **Step 2: Build response bindings from retained provenance**

Remove migrated response plugins from the legacy remainder. Select one response recorder outside the terminal. Reject build configurations that combine bounded-only response plugins with known streaming/hijack owners until the next PR supplies a compatibility plan.

- [ ] **Step 3: Run route focused and full package tests**

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "(BufferedResponsePhase|ConsumerAccess|ScopedRewrite|BuilderLifecycle)" -count=1'
bash -lc 'source .envrc && go test ./pkg/route -count=1'
```

### Task 6: Verification, review, and independent PR delivery

- [ ] **Step 1: Run changed-package and race gates**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin ./pkg/route ./pkg/plugin/{response_rewrite,error_page,body_transformer,exit_transformer,echo,serverless,api_breaker,proxy_cache,graphql_proxy_cache} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin ./pkg/route ./pkg/plugin/proxy_cache -run "(BufferedResponse|ResponsePhase|Cache)" -count=3'
```

- [ ] **Step 2: Scan for duplicate post-next response work**

```bash
rg -n 'RunHeaderFilter|RunBufferedBodyFilter|BufferedResponseExecutor|WithTransformPipeline|NewBufferedResponseWriter' pkg/plugin pkg/route
```

- [ ] **Step 3: Run scoped lint/build/diff gates**

```bash
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/... ./pkg/route/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 4: Independent review and delivery**

Review must verify phase order, one recorder, early responses, panic bypass, body cap, representation invalidation, cache-store timing, no duplicate Handler work, and build-time rejection of unsupported stream combinations. After approval, commit:

```bash
git commit -m "refactor(plugin): execute buffered response phases"
```

Open one ready PR, wait for CI, and merge before streaming phases.

## Fast-plan-impl Dispatch Ownership

1. **WU-01 response contracts/executor** owns `pkg/plugin/base/response_phase*`, touched base writer tests, `pkg/plugin/response_executor*`, and all request/response registry files; freeze it first.
2. **WU-02 bounded transform plugins** owns the response-rewrite, error-page, body/exit-transformer, echo, serverless, and api-breaker directories.
3. **WU-03 cache and route integration** owns proxy-cache, graphql-proxy-cache, and the named `pkg/route/**` files; cache packages implement fixed interfaces without editing core registries. WU-02 and WU-03 start only after WU-01 and never edit core files or each other's paths.

## Explicit Deferrals

- gzip, brotli, grpc-web/transcode, SSE/flush, websocket, hijack, and unbounded streaming compatibility.
- Loggers, tracers, request metrics, and finalizer phase ordering.
- An operator-facing response-body cap remains in the separate body-resource plan; this PR uses only its fixed internal 8 MiB fail-closed bound.
