# Journal and Immutable Runtime Cutover Dependency Correction

**Goal:** Preserve the completed durable-journal foundation while removing the circular dependency and duplicate production cutover between the durable journal and immutable compiler/runtime plans.

**Authority:** This addendum is the canonical execution-order source for Programs 03 and 04. Where it conflicts with `2026-08-23-durable-generation-journal.md`, `2026-08-23-immutable-compiler-plugin-runtime.md`, or the linear Program 03 → Program 04 wording in the total plan, this addendum wins. The child plans remain authoritative for interfaces and task details not amended here.

**Current checkpoint:** Durable Journal Tasks 1–8 are complete at `20afa677`. Durable Task 9 has no implementation diff. Do not rebuild or replace the accepted Task 1–8 journal, coordinator, recovery, acknowledgement, or provider-translation work.

## Why the original order is unsafe

The original total plan requires the durable journal plan to finish before the immutable runtime plan. Durable Task 9 nevertheless creates a temporary `GenerationEngine` over `store.PublishedView`, mutable HTTP ownership and mutable stream reload, then says Immutable Task 9 will replace those internals. Immutable Task 9 owns the same coordinator transaction, recovery installation, HTTP/stream activation and runtime deletion. Executing both creates two production cutovers and a temporary global read owner.

That temporary owner cannot preserve generation isolation:

- the current route handler retires a predecessor immediately and cannot restore it;
- the current stream runtime reloads a mutable router in place;
- route compilation, consumer authentication, plugin metadata, proto and secret reads still reach package-global Store APIs;
- TLS certificate selection reaches the global Store during handshake;
- an old draining request or hijacked connection would therefore observe new-generation global resources.

The correction is to prepare immutable, generation-bound owners first and switch providers, journal, HTTP, TLS and stream exactly once.

## Corrected dependency graph

```text
01 Governance / Manifest / Profiles                         COMPLETE
  -> 02 Static Config / Effective Config                    COMPLETE
  -> 03A Durable Journal Core, Tasks 1-8                    COMPLETE
  -> 04A Immutable Foundations and Detached Compilation
       Wave A1 secret materializer --------------------\
       Wave A2 task ownership --------------------------+-- parallel
       Wave A3 resource registry core ------------------+
       Wave A4 capability descriptor ------------------/
       Wave B  RuntimeDependencies assembly
       Wave C  normalize / validate / closure
       Wave D  scoped plugin instances / prepared generation
       Wave E  detached immutable HTTP and stream compilation
  -> 03/04 Joint Production Cutover
       one GenerationEngine + provider/startup/recovery cutover
       one HTTP/TLS/stream owner swap
       one legacy Store/Builder/mutable-reload deletion
  -> 03B Durable documentation and gate
     + 04B panic/finalization and goroutine ownership
  -> 04C Immutable final audit and gate
  -> 05 Supervisor / Worker / Platform Lifecycle
```

## Wave A: Buildable parallel foundations

Use separate worktrees and independent review. Each branch must compile and pass its focused tests before merge. These work units have disjoint primary files. Every command runs from the worktree root after `source .envrc`; `make build` is the independent-merge smoke gate for each branch.

### A1: Scoped secret materializer

Execute Immutable Task 1 only for:

- `pkg/secret/materializer.go`
- `pkg/secret/materializer_test.go`

Do not create `pkg/runtime/dependencies.go` yet. This branch depends only on the accepted immutable data-encryption service.

Gate and commit:

```bash
bash -lc 'source .envrc && go test ./pkg/secret -run "^(TestMaterializer|TestGenerationCapability|TestScopedMaterializer)" -count=1 && make build'
git add pkg/secret/materializer.go pkg/secret/materializer_test.go
git commit -m "feat(runtime): add scoped secret materialization"
```

### A2: Named task ownership

Execute Immutable Task 2 unchanged. It owns only the task registry and request task group files under `pkg/runtime`.

Gate and commit:

```bash
bash -lc 'source .envrc && go test -race ./pkg/runtime -run "^(TestTaskRegistry|TestRequestTaskGroup)" -count=1 && make build'
git add pkg/runtime/task_registry.go pkg/runtime/task_registry_test.go pkg/runtime/request_tasks.go pkg/runtime/request_tasks_test.go
git commit -m "feat(runtime): own generation and request tasks"
```

### A3: Generic resource registry core

Execute the independent core of Immutable Task 3:

- create and test `pkg/runtime/resource_registry.go`;
- export and test `proxy.NewCluster`;
- modify `pkg/proxy/registry.go` mechanically to call `NewCluster`, with its direct tests, so the live registry remains buildable without a forwarding wrapper;
- keep the current `ClusterRegistry` unchanged and reachable only from the still-live Builder path.

Postpone the `acquireCluster(runtime.RuntimeDependencies, ...)` compiler integration and any new compiler/route acquisition call until Wave C/E. Do not create a stub `RuntimeDependencies`, placeholder registry, alias, adapter or selection flag.

Gate and commit:

```bash
bash -lc 'source .envrc && go test -race ./pkg/runtime ./pkg/proxy -run "^(TestResourceRegistry|TestCluster|TestClusterRegistry)" -count=1 && make build'
git add pkg/runtime/resource_registry.go pkg/runtime/resource_registry_test.go pkg/proxy/cluster.go pkg/proxy/cluster_test.go pkg/proxy/registry.go pkg/proxy/registry_test.go pkg/proxy/registry_metrics_test.go
git commit -m "refactor(runtime): add generic resource ownership"
```

### A4: Capability manifest descriptor

Execute Immutable Task 4. The still-live Builder must consume the descriptor facts directly so the manifest becomes the one phase/priority/scope source without introducing a selectable runtime path.

The branch owns `pkg/plugin` descriptor/instance/executor changes, duplicate-registry deletions, the capability manifest, `pkg/route/builder.go`, and every route test it modifies. The Builder migration is part of the commit because deleting the handwritten registries without it does not compile.

Gate and commit:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/route -run "^(TestDescriptor|TestNewInstanceKey|TestManifest|TestRequestPipeline|TestResponse|TestStreaming|TestLog|TestBuilder|TestRoute)" -count=1 && make build'
git add pkg/plugin pkg/capability/manifest.yaml pkg/route/builder.go pkg/route/*_test.go
git commit -m "refactor(plugin): derive runtime descriptors from manifest"
```

### Wave B: RuntimeDependencies assembly

After A1–A3 are merged, complete the remaining half of Immutable Task 1:

- create `pkg/runtime/dependencies.go` and its test;
- use the real `secret.Materializer`, `ResourceRegistry` and `TaskRegistry` types;
- reject incomplete dependencies.

This split removes the original Task 1 ↔ Task 3 compile-time cycle without temporary types.

Gate and commit:

```bash
bash -lc 'source .envrc && go test ./pkg/runtime -run "^TestRuntimeDependencies" -count=1 && make build'
git add pkg/runtime/dependencies.go pkg/runtime/dependencies_test.go
git commit -m "feat(runtime): assemble explicit runtime dependencies"
```

## Waves C-E: Detached immutable runtime

Execute in dependency order:

1. Immutable Task 5, moving the postponed cluster acquisition integration into the first real compiler use.
2. Immutable Task 6, adding every still-live Builder/server/plugin caller affected by the explicit secret and plugin dependency signature to the task scope. No global secret fallback may remain.
3. Immutable Task 7, producing generation-bound HTTP, TLS, consumer, metadata, proto, secret and plugin owners while the current Builder remains the sole production entry until the joint cutover.
4. Immutable Task 8 only for additive immutable router/snapshot/compiler work.

Immutable Task 8 must not yet remove `Router.Reload`, mutate listener ownership, or install a new router from the legacy event path. Those destructive steps belong to the joint cutover. A detached compiler may exist before the cutover, but there must be no runtime flag or second selectable production path.

## Joint Production Cutover

Execute Durable Task 9 and Immutable Task 9 as one reviewed integration unit in one worktree. Immutable Task 9 owns the permanent `GenerationEngine`; Durable Task 9 contributes provider, journal, migration/deletion and acknowledgement responsibilities. Do not implement the durable plan's temporary `prepareDomain(store.PublishedView)`, temporary `domainActivationLease`, or temporary `securitySensitiveResource` classifier.

The unit must:

1. Open and recover the journal before provider startup.
2. Compile only verified committed published artifacts for serving; never rebuild serving state from desired state during recovery.
3. Make the only publication path `provider -> Coordinator.Apply -> Compiler.Prepare -> Journal.Stage -> GenerationEngine.Activate -> Journal.Commit -> FinalizeActivation -> acknowledgement`.
4. Advance etcd/standalone local cursor and decision state only from an acknowledgement.
5. Bind HTTP handler, TLS selector, consumer credentials/groups, metadata, proto, materialized secrets, plugin instances, upstream leases and stream router to the owning prepared generation.
6. Retain the predecessor through journal commit, restore it on partial activation or commit failure, and retire it only after finalize.
7. Preserve old-generation resources for draining requests and naturally closing hijacked connections.
8. Delete Event/hooks, legacy resource buckets, in-memory last-good state, package-global production Store getters, `route.Builder`, `ClusterRegistry`, mutable stream `Reload`, and proxy-only lifecycle facades in the same integration unit.
9. Move all managed resource kind/domain classification to one canonical contract in `pkg/generation`; expose at least `ManagedResourceKinds() []string` as a sorted defensive copy and the pure kind-to-domain classifier consumed by etcd, standalone, legacy import and compiler. Do not call the nonexistent `store.ManagedResourceKinds()` or retain divergent lists.
10. Migrate affected Store, provider, plugin, route, TLS, stream and server tests to journal snapshots and generation-bound fixtures. Do not replace coverage by deleting tests wholesale.

The integration review must prove:

- stage failure discards the exact prepared generation;
- activation failure, including HTTP-switched/stream-failed, restores all predecessor owners and aborts the token;
- commit failure rolls back owners, releases only candidate leases and aborts the token;
- committed replay confirms the exact active artifact fence without recompilation;
- old and new concurrent requests observe only their own generation's resources;
- TLS, HTTP and stream ownership cannot advance independently;
- offline last-good may serve while provider readiness remains degraded;
- the legacy-path absence scans in both child plans pass.

Add and pass generation-isolation tests named for the contract:

- `TestGenerationEngineOldAndNewRequestsUseOwnConsumerMetadataProtoAndSecrets`;
- `TestGenerationEngineHijackedConnectionRetainsPredecessorResources`;
- `TestGenerationEngineTLSAndHTTPActivateAndRollbackTogether`;
- `TestProductionRuntimeHasNoGlobalStoreReads`, an AST/type-aware guard over non-test Go files under `pkg/compiler`, `pkg/route`, `pkg/plugin`, `pkg/server` and `pkg/stream`. It must reject global Store getter/secret selectors, declarations of `routeHandler.Replace`, `stream.Runtime.Reload` and `stream.Router.Reload`, and selector expressions resolved to those methods.

Run this direct absence scan in addition to the AST guard:

```bash
! rg -n '\bstore\.(GetStore|Get[A-Z][A-Za-z0-9_]*|List[A-Z][A-Za-z0-9_]*|MaterializeSecret|ResolveSecretReference)\(' pkg/compiler pkg/route pkg/plugin pkg/server pkg/stream --glob '*.go' --glob '!*_test.go'
! rg -n 'type Builder|NewBuilder|ClusterRegistry' cmd pkg --glob '*.go'
! rg -n 'func \([^)]*\*(routeHandler|Runtime|Router)\) (Replace|Reload)\(|\b(routes|streamRuntime|router)\.(Replace|Reload)\(' pkg/server pkg/stream --glob '*.go'
```

Run the union of both Task 9 focused race gates, `make lint`, `make build`, and `git diff --check`. The existing unrelated Windows `pkg/plugin/file_logger/writer_registry.go` `syscall.SIGUSR1` failure may be recorded; no new journal/compiler/runtime cross-build failure is exempt.

## Completion order after cutover

1. Execute Durable Task 10 and its milestone gate.
2. Execute Immutable Tasks 10–11. They may run beside Durable Task 10 only when file ownership is disjoint.
3. Execute Immutable Task 12 after both preceding branches are merged.
4. Mark Programs 03 and 04 complete only after their respective final gates pass on the same merged `master` ancestry.
5. Start Program 05 only from that merged master.

## Progress accounting

- Programs 01–02: complete.
- Durable Journal Core: 8/10 child tasks complete.
- Durable Journal milestone: incomplete until the joint cutover and Task 10 gate.
- Immutable Compiler/Runtime: next active stage is Wave A; no child task is credited complete until merged and reviewed.
- Overall percentage remains on the prior task-count basis; dependency correction neither removes completed credit nor claims the postponed cutover.
