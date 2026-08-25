# Task 9 Wave 2 Legacy Runtime Deletion Plan

**Goal:** Delete the Store/Event/Builder/ClusterRegistry/mutable Reload serving path without removing helpers still used by the immutable compiler, preserve stream metrics for active and draining generations, and make the final production-boundary contracts pass.

**Base:** Create the implementation worktree only after the accepted plugin and route-test commits are cherry-picked into `codex/apisix-go-immutable-task9`. Cherry-pick contract commit `73684b9f9c79d4e9a5225bdcd894aeaa6d41909b` before production edits and record its intended RED failures.

**Authority:** Local implementation, focused verification, and local commit only. No push, PR, or merge to `master` until the combined Task 9 review passes.

## Success contract

1. Production has one journal/coordinator/compiler/generation serving path and no Store/Event/Builder/ClusterRegistry/mutable Reload fallback.
2. `pkg/route/builder.go`, `pkg/server/reload.go`, and legacy Store files are deleted only after every live immutable dependency is moved.
3. Stream metrics admit route IDs from active and draining stream generations; activation rollback, recovery, overlap, repeated IDs, and terminal close cannot prematurely drop or retain IDs.
4. `routeHandler` is generation-only while retaining request, batch, hijack, panic, finalizer, body-limit, reject-new, drain, and terminal-close behavior.
5. `stream.Runtime` is listener/connection ownership only and `stream.Router` is immutable-only.
6. C6 continues to forbid `ApplyTicket` construction outside the journal owner while allowing Task 9 server imports and ticket transport.
7. The Task 9 contract tests, absence guards, focused normal/race tests, lint, build, diff audit, and independent review pass.

## Dependency order

```text
accepted plugin + route migrations
  -> contract RED checkpoint
  -> Builder live-symbol extraction
  -> stream metrics generation ownership
  -> immutable-only stream Router/Runtime
  -> generation-only routeHandler
  -> server legacy reload/TLS/stream deletion
  -> Builder + ClusterRegistry deletion
  -> journal-only Store reduction
  -> C6 + Task9 absence guards
  -> combined verification and review
```

## Step 1: Establish the RED boundary

**Files:**

- `pkg/server/generation_isolation_test.go`
- `pkg/server/production_boundary_test.go`
- `pkg/compiler/c6_production_boundary_test.go`

1. Cherry-pick `73684b9f9c79d4e9a5225bdcd894aeaa6d41909b`.
2. Run each Task 9 contract independently and record which production legacy owners it detects.
3. Run `TestC6ProductionBoundary` and record the obsolete pre-Task-9 import/ticket-transport diagnostics separately.
4. Do not weaken a guard to make this checkpoint green.

## Step 2: Extract immutable route dependencies from Builder

**Files:**

- `pkg/route/builder.go`
- `pkg/route/compiler.go`
- `pkg/route/plugin_compile.go`
- `pkg/route/upstream_compile.go`
- `pkg/route/upstream_options.go`
- `pkg/route/prepared_handler.go`
- focused route tests beside the destination code

Move, without forwarding wrappers:

- route semantics and clone helpers: `validateRouteSemantics`, `clonePluginConfigs`, `isConsumerCredentialOnly`;
- metadata construction and its complete dependency closure, including all metadata phase plugins, filters, masks, error writers, `buildRequestContextConfig`, `parsePluginPriority`, and `newMetadataRequestAndBufferedPluginWithDescriptor`;
- compiled upstream helpers: `inlineUpstreamConfigured`, `upstreamNodeHost`, `compiledUpstreamTarget`, parse/resolve helpers, director error key;
- Dubbo/http-Dubbo terminal helper closure;
- prepared-handler constants/helpers for client-close status, user agent, latency, status headers, and failure logging;
- traffic-split target application.

Delete the pure legacy receiver `(*Builder).buildTransportOption`; keep `buildTransportOptionWithSSLResolver`.

After each extraction cluster:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/route -run '<exact affected tests>' -count=1
rg -n '\b<symbol>\b' pkg/route
```

The step is complete only when production outside `builder.go` has no reference to a symbol still defined there.

## Step 3: Preserve stream metrics across generation leases

**Files:**

- `pkg/server/generation_owner.go`
- `pkg/server/generation_engine.go`
- `pkg/server/generation_engine_test.go`
- `pkg/observability/metrics/official_runtime.go` only if a minimal lifecycle API is required

Write failing tests first for:

1. active A -> finalize B: A and B route IDs remain recordable while A has a stream lease;
2. tentative B -> rollback A: a B connection started before rollback remains recordable until B drains;
3. recovery publishes its stream route IDs;
4. the same route ID in predecessor and successor is reference-counted and not deleted early;
5. engine terminal close clears the index.

Implement a private generation-owner route-ID registration lifecycle. The published metric index is the union of stream owners that are active or still have stream leases. Update `metrics.SetStreamRoutes` only from that lifecycle. Do not publish only the current bundle's IDs because draining predecessor results would be lost.

## Step 4: Make stream Router and Runtime immutable-only

**Files:**

- `pkg/stream/router.go`
- `pkg/stream/router_test.go`
- `pkg/stream/runtime.go`
- `pkg/stream/runtime_test.go`

Delete:

- `Router.Reload`, `Runtime.Reload`, `NewRouter`, `newLegacyRuntime`;
- mutable router fields and `ErrFrozenRouter`;
- raw-config runtime construction and reload validation.

Retain and test:

- `CompileRouter`, `RouterSource`, one lease per connection;
- unavailable/error release behavior;
- accept retry/listener rollback;
- source rollback affecting only new connections;
- terminal close canceling connections;
- concurrent immutable `Serve` and `RouteIDs`.

Run focused normal and race tests for `pkg/stream` before continuing.

## Step 5: Make routeHandler generation-only

**Files:**

- `pkg/server/route_handler.go`
- `pkg/server/route_handler_test.go`

Delete `routeSet`, legacy atomics/draining, `newRouteHandler`, `Replace`, and the legacy `ServeHTTP` branch. Preserve the generation lease source and all request/hijack/drain behavior.

Migrate generic panic/finalizer/body-limit tests to a generation lease fixture before deleting their legacy setup. Delete tests that only assert `Replace` or route-set stopper behavior.

Run exact route-handler tests and focused race tests.

## Step 6: Delete server legacy serving owners

**Files:**

- delete `pkg/server/reload.go`
- delete or migrate `pkg/server/reload_test.go`
- `pkg/server/server.go`
- `pkg/server/tls.go`
- relevant server tests and benchmarks

Delete:

- reload scheduler/hooks, Store event classification and publication caches;
- Builder startup/install helpers and engine-nil compatibility shutdown;
- legacy stream load/resolve/reload/last-good functions;
- Store-backed TLS selectors and duplicate depth validation;
- legacy fields: clusters, storage, reload channels/mutexes, HTTP publication state, stream route cache, compatibility close state, scheduler state.

Shrink `streamRuntimeOwner` to `Close(context.Context) error`. Keep `streamRuntime` pointer synchronization, but move it under a single lifecycle lock and never hold that lock across `Runtime.Close`.

Retain immutable listener startup, provider shutdown ordering, generation TLS selection, `logStreamResult`, and terminal stream close.

## Step 7: Delete Builder and ClusterRegistry

**Files:**

- delete `pkg/route/builder.go`
- delete `pkg/proxy/registry.go`
- delete/migrate `pkg/proxy/registry_test.go`
- delete/migrate `pkg/proxy/registry_metrics_test.go`
- affected server/route tests

Before deletion, require zero production and test call sites for `Builder`, `NewBuilder*`, and `ClusterRegistry`. Preserve `proxy.Cluster`, `ClusterConfig`, and `ClusterObserver`; runtime ownership remains `runtime.ResourceRegistry` plus compiler cluster observers.

## Step 8: Reduce Store to journal ownership

**Files:**

- add `pkg/store/journal_store.go`
- `pkg/store/journal_schema.go`
- delete legacy Store/Event/getter/consumer/secret/published-view files and tests

Move only the bbolt handle, open timeout, close-once state, and idempotent `Close` needed by journal files into `journal_store.go`. Keep journal schema/apply/publish/recovery and raw legacy-bucket import support.

Delete legacy Event/getters/caches/standalone snapshot/`ResolvedSecret`/secret broker/`PublishedView` only after production plugin, route, server, and stream imports of `pkg/store` are zero. Remove `stopProducers` schema initialization.

Run journal/apply/publish/recovery/policy tests before deleting any additional Store code.

## Step 9: Update authority and absence guards

**Files:**

- `pkg/compiler/c6_production_boundary_test.go`
- `pkg/server/production_boundary_test.go`
- `pkg/server/generation_isolation_test.go`

Remove only C6's obsolete compiler/runtime import ban. Keep the exact journal `ApplyTicket` construction allowlist. Allow ticket parameters, fields, and return-value transport; continue rejecting composite/new/zero declaration/named-result/conversion/aggregate construction outside the journal owner.

Make Task 9 guards reject production Store/Event/Builder/ClusterRegistry/Replace/Reload fallback symbols, including aliases, dot imports, method values, and cross-package calls.

## Step 10: Final verification and review

```bash
source .envrc
export GOFLAGS=-mod=readonly

go test ./pkg/route -count=1 -timeout=300s
go test ./pkg/stream -count=1 -timeout=300s
go test ./pkg/server -run '(GenerationEngine|RouteHandler|Hijack|FrontendTLS|ProductionBoundary|GenerationIsolation)' -count=1 -timeout=300s
go test ./pkg/store -run '(Journal|Recovery|Apply|Publish|Policy)' -count=1 -timeout=300s
go test ./pkg/compiler -run '(C6ProductionBoundary|ResourceRegistry|AcquireHTTPCluster|CompileAndAttachStream)' -count=1 -timeout=300s
go test -race ./pkg/server ./pkg/stream -run '(GenerationEngine|RouteHandler|Hijack|TLS|Runtime|Stream)' -count=1 -timeout=300s
make build
git diff --check
```

Run the plan's absence scans and targeted lint for every changed package. Then request one independent merge-level review. Do not merge to the Task 9 integration branch until every Critical and Important finding is fixed and re-reviewed.
