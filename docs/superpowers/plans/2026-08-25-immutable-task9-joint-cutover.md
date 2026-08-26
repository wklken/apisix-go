# Task 9 Joint Journal and Immutable Runtime Cutover Execution Plan

**Goal:** Replace the legacy Store/Event/Builder/mutable-reload production path with the single durable journal and immutable generation path, preserve independently published HTTP and stream revisions, and delete every legacy owner in the same reviewed integration unit.

**Authority:** This plan decomposes Task 9 from `2026-08-23-immutable-compiler-plugin-runtime.md` under the ordering correction in `2026-08-24-journal-immutable-cutover-reorder.md`. Those documents remain authoritative for scope. This plan is authoritative for Task 9 execution order, frozen cross-lane interfaces, worktree ownership, and merge gates.

**Base:** local `master` at `16b28bb54162da6e94560d772c46640c719cdde6`. Durable Tasks 1-8 and Immutable Tasks 1-8 are accepted. Do not rebuild the journal, coordinator, compiler, detached HTTP/TLS snapshot, or detached stream router.

**Delivery boundary:** one reviewed Task 9 integration branch, then local fast-forward merge to `master`. No push or PR is authorized. No production activation commit may leave the legacy and immutable paths selectable at the same time.

## Success contract

Task 9 is complete only when all of the following are true:

1. Startup opens and recovers the journal before starting a provider.
2. Recovery serves only verified `RecoveryState.Published` artifacts through `WorkerCompilerFactory.PrepareRecovery`; `RecoveryState.Desired` is never a serving input.
3. The only update path is:

   ```text
   provider
     -> Coordinator.Apply
     -> GenerationEngine.Prepare
     -> WorkerCompilerFactory.PrepareGeneration
     -> Journal.Stage
     -> GenerationEngine.Activate
     -> Journal.Commit
     -> GenerationEngine.FinalizeActivation
     -> acknowledgement
   ```

4. Provider cursor, quarantine, and decision state advance only after the returned acknowledgement.
5. HTTP routing, TLS selection, consumers, metadata, proto, secrets, plugins, clusters, and stream routing are generation-bound.
6. Activation publishes HTTP/TLS/stream through one atomic bundle pointer, while preserving independently active domain revisions.
7. Rollback restores the exact predecessor bundle. Finalize only transfers ownership and queues retirement; it performs no blocking close or drain.
8. Old requests and naturally closing hijacked/stream connections retain their predecessor generation until their lease closes.
9. Production contains no Store/Event/Builder/ClusterRegistry/mutable Reload/last-good dual path.
10. Secret candidate and recovery attempts are exact, independent views and never read Store.
11. Startup failure, normal shutdown, and repeated close preserve exact reverse ownership and replay the first cleanup error.
12. Focused tests, race gates, lint, build, absence guards, diff audit, and independent review pass on the final combined branch.

## Non-goals

- Do not change the accepted `generation.PublicationEngine`, `generation.Journal`, or coordinator transaction signatures unless a real integration test proves a defect.
- Do not implement the durable plan's temporary `prepareDomain(store.PublishedView)` or `domainActivationLease`.
- Do not introduce a runtime flag or compatibility switch between legacy and immutable production paths.
- Do not start Program 05 supervisor/worker process separation, Task 10 panic boundaries, or Task 11 goroutine ownership here.
- Do not force-close hijacked connections during ordinary generation retirement. Terminal server shutdown remains allowed to force-close after its drain policy.
- Do not silently remove cluster or stream result observability while deleting legacy owners.

## Frozen cross-lane contracts

These contracts must be committed before parallel production edits. A lane may request a contract correction with a failing test; it must not silently invent a competing owner.

### 1. Composite active bundle

HTTP and stream may be published or recovered at different revisions. The engine therefore owns one atomically installed composite bundle, not one global active generation:

```go
type activeBundle struct {
	http   *generationOwner
	stream *generationOwner
}

type generationOwner struct {
	prepared *compiler.PreparedGeneration
	// active domain-slot references and acquired request/connection leases
	// are protected by the owner lifecycle implementation.
}
```

- One `generationOwner` may occupy both slots when one prepared generation publishes both domains.
- HTTP-only activation replaces only `bundle.http`; stream-only activation replaces only `bundle.stream`.
- The untouched domain slot remains bit-for-bit the same owner.
- HTTP handler, TLS selector, and stream acceptor load the same bundle pointer.
- A prepared generation closes only after it occupies no active domain slot and has no request, hijack, or stream lease.

### 2. Generation engine API and state

Keep the accepted public boundary:

```go
func NewGenerationEngine(server *Server, factory *compiler.WorkerCompilerFactory) (*GenerationEngine, error)
func (engine *GenerationEngine) InstallRecovery(context.Context, generation.RecoveryState) error
func (engine *GenerationEngine) Close(context.Context) error
```

`GenerationEngine` implements `generation.PublicationEngine` and internally owns:

- exact `preparedKey{Desired, HTTPDigest, StreamDigest}` identities;
- pending real and synthetic zero-domain candidates;
- token-bound activation records containing previous and candidate bundles;
- the atomic active bundle;
- exact per-domain active publication fences plus a separate initialized fence;
- a nonblocking retirement queue;
- close-once ownership of `WorkerCompilerFactory`.

`Activate` may expose a tentative candidate bundle before durable commit. A failure restores the complete predecessor bundle. Leases acquired during the tentative window remain attached to the rejected owner and drain naturally before it closes.

### 3. Runtime lease surfaces

The server exposes only generation-bound acquisition functions to protocol owners:

```go
type httpGenerationLease struct {
	Snapshot *compiler.HTTPSnapshot
	Release  func()
}

type streamGenerationLease struct {
	Router  *stream.Router
	Release func()
}
```

- Each HTTP request acquires and releases one HTTP lease.
- Batch/subrequest dispatch retains its own lease until all child work completes.
- Successful hijack acquires a distinct lease wrapped into the returned `net.Conn`; only `Close` releases it.
- TLS callbacks acquire the current HTTP owner briefly and select from its immutable `HTTPSnapshot.TLSConfig()`.
- Each accepted stream connection acquires one stream lease and retains its router for the entire connection.
- Acquisition fails closed when the requested domain is unavailable.

### 4. Stream listener boundary

`stream.Runtime` becomes a listener/accept owner. It does not compile or mutate routers:

```go
type RouterLease struct {
	Router  *Router
	Release func()
}

type RouterSource func() (RouterLease, bool)

func NewRuntime(context.Context, []config.TcpListen, RouterSource) (*Runtime, error)
```

Delete `Runtime.Reload` and `Router.Reload` after all callers and tests are migrated. `Runtime.Close` stops listeners and terminally cancels active connections; ordinary generation retirement does neither.

### 5. Compiler observer injection

Deleting `ClusterRegistry` and the legacy stream runtime must not delete observability. Freeze a concrete constructor input rather than an optional variadic:

```go
type WorkerRuntimeObservers struct {
	Cluster proxy.ClusterObserver
	Stream  func(stream.Result)
}

func NewWorkerCompilerFactory(
	manifest *capability.Manifest,
	effective *config.EffectiveConfig,
	materializer secret.Materializer,
	observers WorkerRuntimeObservers,
) (*WorkerCompilerFactory, error)
```

- Production supplies the generation-safe cluster observer and `logStreamResult` callback.
- Tests use explicit no-op observers.
- Cluster observer is required and non-nil.
- Stream observer is required when stream proxy mode is enabled.
- `compileAndAttachStream` passes the callback into `stream.CompileInput.OnResult` before the router freezes.

### 6. Secret resolver ownership

Create `secret.GenerationSecretResolver`, implementing `secret.AttemptResolverFactory` and `Close(context.Context) error`.

- Constructor accepts the already validated `data_encryption.Service` and immutable supporting configuration only.
- `OpenCandidate` indexes only the effective candidate publication closure.
- `OpenRecovery` indexes only verified committed published closures.
- Candidate and recovery attempts with the same desired revision have distinct domain-separated IDs and may overlap.
- Each attempt owns independent immutable indexes and zeroes/revokes them on close.
- Factory close prevents new opens, closes remaining attempts, then shared clients/caches.
- No method imports or reads `pkg/store`.

### 7. Provider application boundary

Providers depend on a minimal acknowledgement-returning interface rather than Store:

```go
type DesiredApplier interface {
	Apply(context.Context, generation.DesiredBatch) (generation.Acknowledgement, error)
}
```

The existing etcd and standalone desired-batch translators remain canonical. Only successful acknowledgement updates provider-local cursor, decisions, quarantine, metrics, and readiness. A failed apply retains the previous acknowledged state.

### 8. Bootstrap and shutdown ownership

Bootstrap constructs one validated manifest/catalog/effective configuration/data-encryption service/resolver. Ownership transfers exactly once into `Server`/factory/engine.

Startup order:

```text
load manifest + effective config
-> construct data-encryption service and GenerationSecretResolver
-> OpenJournal
-> Journal.Recover
-> construct Materializer + WorkerCompilerFactory + GenerationEngine
-> GenerationEngine.InstallRecovery
-> construct Coordinator
-> start listeners
-> start provider
```

Shutdown order:

```text
stop provider / reject new Apply
-> stop HTTP and stream listeners / reject new leases
-> drain HTTP requests and natural connection leases under server policy
-> GenerationEngine.Close
-> GenerationSecretResolver.Close
-> Journal.Close
-> observability shutdown
```

Every constructor failure closes only owners already transferred, in reverse order, with `errors.Join` and close-once behavior.

## Dependency graph

```text
T9-0 frozen contracts and RED fixtures
  ├─ T9-1 generation secret resolver -------------------------┐
  ├─ T9-2 route pure-helper extraction/test migration --------┤
  ├─ T9-3 HTTP/hijack/TLS lease surfaces ---------------------┤
  ├─ T9-4 stream listener/router-source split ----------------┤
  └─ T9-5 compiler observer injection ------------------------┘
                  all merge to one integration base
                              |
                    T9-6 GenerationEngine
                              |
             ┌────────────────┴────────────────┐
             |                                 |
       T9-7 provider ack adapters       T9-8 bootstrap/recovery/shutdown
             └────────────────┬────────────────┘
                              |
                 T9-9 legacy deletion/test migration
                              |
                 T9-10 isolation, absence and gates
                              |
                   independent review and local merge
```

## T9-0: Commit contracts and failing integration fixtures

**Files:** this plan; child plans; new test fixture files under `pkg/server` only after the plan checkpoint.

1. Record the contracts above and produce complete child plans before product edits.
2. Add compile-time interface assertions for `GenerationEngine`, the resolver, and provider appliers.
3. Add deterministic RED tests for:
   - exact pending publication identity;
   - zero-domain synthetic lifecycle without compiler calls;
   - HTTP-only and stream-only active slots at different revisions;
   - partial activation and commit rollback restoring the whole bundle;
   - nonblocking finalize with predecessor queued, not closed;
   - exact committed replay fence;
   - recovery from Published while Desired differs;
   - close-once ownership.
4. Add lease-fixture RED tests without changing production selection yet.
5. Commit only contracts/tests. Confirm the failures are behavioral missing-implementation failures, not compile accidents.

## T9-1: Move the transitional Store secret broker into `pkg/secret`

**Owns:** `pkg/secret/generation_resolver.go`, its tests, and only the minimum shared secret helpers it needs. It does not modify server, providers, compiler, or Store deletion.

1. Port manager reference parsing, environment/Vault resolution, bounded cache/client ownership, redaction, closure checks, and zeroing from `pkg/store/secret_broker.go`.
2. Replace Store-backed attempt registration with independent candidate/recovery resolver objects.
3. Test exact attempt, generation, domain, plugin owner, declaration, resource, and referenced secret membership before backend access.
4. Add recovery-vs-desired, same-revision A/B overlap, missing target, cross-attempt, duplicate open, revoke, close, and cleanup-error tests.
5. Run focused `pkg/secret` tests and race tests. Keep the transitional Store broker until T9-9.

## T9-2: Extract live pure route helpers before deleting Builder

**Owns:** `pkg/route` helper destinations and corresponding route/compiler behavior tests. It must not change production activation.

1. Move every live pure symbol enumerated by the Task9 inventory from `builder.go` to its existing consumer file: `plugin_compile.go`, `prepared_handler.go`, `upstream_compile.go`, `upstream_options.go`, `compiler.go`, or `router.go`.
2. Move code without forwarding methods and without behavior changes.
3. Migrate pure helper tests to the new seam; migrate Builder lifecycle behavior to compiler/prepared-generation fixtures.
4. Run call-site searches for every moved symbol.
5. Keep `Builder` buildable until T9-9, but shrink it to legacy ownership only.

## T9-3: Add HTTP, hijack, batch, and TLS generation leases

**Owns:** `pkg/server/route_handler.go`, `pkg/server/tls.go`, focused tests, and the server-internal lease interface fixed in T9-0.

1. Route dispatch acquires the current HTTP generation lease and retains it through finalizers.
2. Batch dispatch acquires a separate lease for child work.
3. Hijack wraps the returned connection with a close-once release; ordinary generation retirement no longer closes registered hijacks.
4. Terminal handler/server shutdown may close outstanding hijacks according to its existing bounded policy.
5. TLS handshake acquires current HTTP snapshot and performs immutable certificate/client-CA selection without Store.
6. Add N/N+1 request isolation, natural hijack retention, terminal close, batch retention, TLS switch, TLS rollback, and unavailable-domain tests.

## T9-4: Separate stream listeners from immutable routers

**Owns:** `pkg/stream/runtime.go`, `pkg/stream/router.go`, their tests, and no server bootstrap.

1. Change Runtime construction to `RouterSource`.
2. Acquire exactly one router lease per accepted connection.
3. Retain that router until the connection exits; release exactly once on all paths.
4. Preserve fixed listener ownership and stream result callback already frozen into the router.
5. Delete mutable router internals and reload methods after callers migrate in the same branch or integration checkpoint.
6. Test N connection/N+1 connection isolation, rollback source restoration, missing-router rejection, accept errors, and terminal close.

## T9-5: Inject generation-safe runtime observers

**Owns:** `pkg/compiler` factory/HTTP cluster/stream compilation and focused observer tests. It does not own server metrics implementation or proxy registry deletion.

1. Add the required `WorkerRuntimeObservers` constructor input.
2. Use the injected cluster observer when acquiring compiler-owned clusters instead of `proxy.NopClusterObserver`.
3. Pass the stream callback to `stream.CompileInput.OnResult` during detached compilation.
4. Update all factory constructors with explicit test observers.
5. Add tests proving observers are fixed per generation, not mutated after activation, and overlap/final release metrics remain correct.

## T9-6: Implement the permanent GenerationEngine

**Owns:** `pkg/server/generation_engine.go`, retirement implementation, and engine tests. It consumes the merged lease surfaces; it does not edit provider translation.

1. Implement exact real/synthetic pending records.
2. `Prepare` delegates non-empty sets to the compiler and stores the returned defensive publication identity.
3. `Activate` probes the prepared generation, constructs the candidate composite bundle by replacing only required domains, swaps it atomically, and records predecessor/candidate ownership under the token.
4. A partial activation failure restores the predecessor bundle before returning.
5. `RollbackActivation` restores predecessor, removes activation, rejects candidate owner, and waits only for candidate leases; it never closes predecessor.
6. `FinalizeActivation` verifies token/set identity, publishes exact domain fences, removes activation, drops replaced domain-slot references, and enqueues every predecessor that now owns no active domain, even if leases still drain, without blocking.
7. A dedicated engine/server retirement loop waits for each queued owner's one-way drain signal and closes it outside the activation mutex.
8. `ConfirmActive` checks only requested domain fences, permits other active domains, honors context cancellation, and performs no compilation or mutation.
9. `InstallRecovery` compiles one owner from verified Published artifacts, installs independently revised domain fences, and sets initialized state. Damaged/missing domains remain unavailable.
10. `Close` rejects new work, stops retirement intake, closes pending/activation/retiring/active owners exactly once, then closes the factory.
11. Run focused and race tests for engine, compiler, request leases, TLS, and stream sources.

## T9-7: Cut providers to acknowledgement-only application

**Owns:** etcd and standalone provider application loops, focused provider tests, and no journal/coordinator rewrite.

1. Inject `DesiredApplier` instead of Store/Event mutation authority.
2. Keep current desired-batch translators and canonical managed domain contract.
3. Submit the full translated batch to `Coordinator.Apply`.
4. Advance provider cursor, decisions, quarantine, metrics, and ready state only from successful acknowledgement.
5. On apply failure, retain previous acknowledged local state and retry from the same provider position according to current backoff semantics.
6. Test first snapshot, incremental replay, same-cursor committed replay, compiler rejection, journal failure, offline recovery readiness, and shutdown cancellation.

## T9-8: Wire startup, recovery, listeners, and shutdown

**Owns:** `cmd/root.go`, `pkg/server/server.go`, focused startup/shutdown tests, and constructor signatures fixed in T9-0.

1. Return the validated manifest from startup loading rather than reloading it downstream.
2. Construct one data-encryption service and one GenerationSecretResolver.
3. Open journal and recover before provider construction/start.
4. Construct materializer, observer set, compiler factory, engine, and coordinator exactly once.
5. Install recovery before listeners accept traffic and before the provider starts.
6. Start HTTP/TLS/stream listeners using engine lease sources; allow offline published recovery to serve while provider readiness is degraded.
7. Implement exact shutdown ordering and reverse cleanup on every constructor/start failure.
8. Test normal shutdown, resolver construction failure, journal recovery failure, recovery install failure, listener failure, provider failure, repeated close, and joined cleanup errors.

## T9-9: Delete legacy owners and migrate retained coverage

**Owns:** deletions and remaining tests only after T9-1 through T9-8 are green on one integration base.

1. Delete Store/Event production ownership, event hooks, reload scheduler, in-memory last-good state, legacy resource buckets/getters, and transitional Store secret broker.
2. Delete `route.Builder` after all live helpers and behavior tests have migrated.
3. Delete `proxy.ClusterRegistry` and its proxy-only lifecycle facades after observer/resource-registry coverage replaces it.
4. Delete `routeHandler.Replace`, `stream.Runtime.Reload`, and `stream.Router.Reload`.
5. Delete Store-based TLS selection and stream route merge/loading.
6. Migrate affected Store/provider/route/TLS/stream/server tests to journal snapshots and generation fixtures; delete only tests proved exact duplicates.
7. Run symbol-by-symbol `rg` call-site scans for every moved/deleted symbol. Treat test-only survivors as suspicious and document any compatibility boundary retained.

## T9-10: Isolation proof, absence guards, and merge gate

Add and pass the named contract tests:

- `TestGenerationEngineOldAndNewRequestsUseOwnConsumerMetadataProtoAndSecrets`
- `TestGenerationEngineHijackedConnectionRetainsPredecessorResources`
- `TestGenerationEngineTLSAndHTTPActivateAndRollbackTogether`
- `TestProductionRuntimeHasNoGlobalStoreReads`

The AST/type-aware guard covers non-test Go files under `pkg/compiler`, `pkg/route`, `pkg/plugin`, `pkg/server`, and `pkg/stream`. It rejects global Store selectors and declarations/calls of the deleted Replace/Reload methods.

Run direct absence scans:

```bash
! rg -n '\bstore\.(GetStore|Get[A-Z][A-Za-z0-9_]*|List[A-Z][A-Za-z0-9_]*|MaterializeSecret|ResolveSecretReference)\(' pkg/compiler pkg/route pkg/plugin pkg/server pkg/stream --glob '*.go' --glob '!*_test.go'
! rg -n 'type Builder|NewBuilder|ClusterRegistry' cmd pkg --glob '*.go'
! rg -n 'func \([^)]*\*(routeHandler|Runtime|Router)\) (Replace|Reload)\(|\b(routes|streamRuntime|router)\.(Replace|Reload)\(' pkg/server pkg/stream --glob '*.go'
```

Run impact-scoped correctness and race gates first:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/generation ./pkg/secret ./pkg/compiler ./pkg/runtime ./pkg/route ./pkg/stream ./pkg/server ./pkg/etcd ./pkg/config -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/generation ./pkg/secret ./pkg/compiler ./pkg/runtime ./pkg/route ./pkg/stream ./pkg/server ./pkg/etcd ./pkg/config -run "(Coordinator|GenerationEngine|RecoverySecretResolver|PreparedGeneration|RouteHandler|TLS|Stream|Provider|Shutdown|Isolation)" -count=1'
```

Then run repository completion gates required for this cross-cutting cutover:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint fmt'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make lint'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
git diff --check master...HEAD
```

Inspect formatter output and retain only Task9 changes. Do not report any skipped, narrowed, or pre-existing failure as passing.

Finally:

1. Perform the moved/deleted-symbol and proxy-only/dead-code audit required by `AGENTS.md`.
2. Request one independent read-only merge review over the complete Task9 diff.
3. Repair every confirmed Critical/Important issue with a focused regression test and rerun affected gates.
4. Commit the accepted cutover, clean worktree-local caches, fast-forward local `master`, verify its exact SHA, and remove only Task9 worktrees already merged.

## Parallel worktree waves

All branches start from a frozen merged base. Workers own only the files listed for their lane and must not commit, push, merge, delete unrelated files, or revert another lane.

- **Plan wave:** child plans for secret resolver; runtime bundle/leases; engine/provider/bootstrap/legacy deletion.
- **Implementation Wave A:** T9-1 secret resolver, T9-2 route helper extraction, T9-5 observer injection. These have disjoint primary files.
- **Integration checkpoint A:** review each diff, run focused tests, merge to the Task9 integration branch, freeze new base.
- **Implementation Wave B:** T9-3 HTTP/TLS lease surfaces and T9-4 stream runtime source. Their fixed interfaces were committed at T9-0.
- **Integration checkpoint B:** review, test, merge, freeze new base.
- **Implementation Wave C:** T9-6 GenerationEngine. Keep this lane single-owner because it controls the atomic bundle and retirement invariants.
- **Implementation Wave D:** T9-7 provider adapters and T9-8 bootstrap/shutdown may run in parallel only after T9-6 signatures and tests are merged.
- **Integration checkpoint D:** production path must pass before destructive deletion.
- **Implementation Wave E:** T9-9 legacy deletion/test migration, then T9-10 gates and independent review.

No dependent worker starts from an unmerged predecessor. If a lane changes a frozen interface, stop parallel integration, update this plan, merge the contract change, and respawn dependent work from the new base.

## Checkpoints and progress accounting

- Plan and contracts committed: Task9 50%.
- Wave A merged and reviewed: Task9 60%.
- Wave B merged and reviewed: Task9 70%.
- GenerationEngine merged and reviewed: Task9 80%.
- Providers/bootstrap cut over and production tests green: Task9 90%.
- Legacy deleted, all gates and independent review accepted: Task9 100%.

Overall convergence percentage continues to use the existing task-count basis; planning progress does not count as a completed implementation task.

## Task 10 handoff

Task10 begins only from Task9's merged local `master`. It inherits:

- one active immutable bundle owner;
- generation-bound HTTP/TLS/stream leases;
- no Builder/Store/Reload production path;
- an asynchronous retirement owner that Task11/Program05 may later relocate;
- explicit plugin observers and generation-owned resources.

Task10 must not reopen Task9 activation, secret, provider, or shutdown interfaces unless a focused regression proves a defect.
