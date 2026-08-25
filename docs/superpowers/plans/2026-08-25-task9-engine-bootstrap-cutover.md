# Task 9 Generation Engine, Provider, Bootstrap, and Legacy Cutover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the mutable Store/Event/reload production path with one recoverable, acknowledgement-driven `Coordinator -> GenerationEngine -> WorkerCompilerFactory` path, publish HTTP/TLS/stream as an atomic per-domain bundle, and delete every legacy runtime owner in the same cutover.

**Architecture:** `cmd` loads one manifest/effective configuration, creates the data-encryption service and generation secret resolver, opens and recovers the journal, then transfers those owners to `Server`. `Server` constructs one materializer, compiler factory, generation engine, coordinator, listener set, and provider. The engine atomically publishes an immutable `activeBundle`, retains each generation until its domain slots and all request/hijack/stream leases drain, and retires owners on a dedicated nonblocking queue. Providers submit canonical `DesiredBatch` values through `DesiredApplier` and mutate local cursor/decision/readiness state only after the returned acknowledgement. Recovery compiles only `RecoveryState.Published`; shutdown reverses the ownership graph.

**Tech Stack:** Go 1.26, `sync.Mutex`, `sync.Once`, `atomic.Pointer`, bbolt-backed `generation.Journal`, immutable compiler snapshots, `net/http`, etcd v3, fsnotify, focused Go tests and the race detector.

**Spec:** [`2026-08-25-immutable-task9-joint-cutover.md`](./2026-08-25-immutable-task9-joint-cutover.md), [`2026-08-24-journal-immutable-cutover-reorder.md`](./2026-08-24-journal-immutable-cutover-reorder.md), Task 9 in [`2026-08-23-immutable-compiler-plugin-runtime.md`](./2026-08-23-immutable-compiler-plugin-runtime.md), and Task 9 in [`2026-08-23-durable-generation-journal.md`](./2026-08-23-durable-generation-journal.md).

## Global Constraints

- The frozen planning base is `4a1c9c99528cd69f09d5025980490f90e637f948`. The earlier `21eb10f2` reference is obsolete; its only correction was the standalone provider package path (`pkg/config`, not `pkg/standalone`).
- This is the permanent engine cutover. Do not implement or land the durable plan's temporary engine, temporary server retirement loop, dual-write adapter, Store-backed provider bridge, or a second publication path.
- Execute only after T9-1 through T9-5 are merged and reviewed on one integration base. This plan consumes, but does not redesign, `GenerationSecretResolver`, route pure helpers, HTTP/hijack/TLS lease surfaces, stream `RouterSource`, and `WorkerRuntimeObservers`.
- Keep the accepted public engine boundary exactly `NewGenerationEngine`, `InstallRecovery`, and `Close`, plus the methods required by `generation.PublicationEngine`. Do not expose bundle pointers, owner handles, runtime leases, mutable fences, or cleanup controls.
- Keep the accepted provider boundary exactly:

  ```go
  type DesiredApplier interface {
      Apply(context.Context, DesiredBatch) (Acknowledgement, error)
  }
  ```

  Put the interface in `pkg/generation/provider.go` so both `pkg/etcd` and `pkg/config` depend inward on the same contract.
- One prepared generation may own HTTP and stream simultaneously. HTTP-only or stream-only publication changes only that slot; the untouched slot keeps the exact same owner pointer and fence.
- `FinalizeActivation` is infallible and nonblocking. It may update mutex-protected state, append to an in-memory queue, and issue a nonblocking wakeup only. It must not call `Close`, wait for a task/lease/connection, perform journal I/O, or log through a blocking sink.
- `RollbackActivation` restores the complete predecessor bundle before waiting for tentative candidate leases. It never closes or decrements an untouched predecessor owner.
- Recovery serves only verified `RecoveryState.Published`. `RecoveryState.Desired` is never compiled, materialized, indexed for secrets, or installed into an HTTP/TLS/stream source.
- `InstallRecovery` is a mandatory one-time startup barrier. `Prepare` and protocol acquisition reject work until it succeeds; an exact empty journal opens the barrier without claiming a committed initialized fence.
- Zero-domain batches are durable acknowledgement operations: no compiler call, no bundle swap, no lease change, no retirement. Successful finalize sets only the initialized fence.
- Provider cursor, known-key set, acknowledged decisions, quarantine, apply metrics, and readiness advance together and only after a successful acknowledgement whose cursor exactly matches the submitted batch.
- Stop and join the provider before closing listeners or the coordinator's dependencies. Stop listeners and reject new leases before draining. Never release a generation, resolver, or journal while a request, hijack, stream connection, or provider `Apply` can still use it.
- Preserve errors with `errors.Join`; do not let a cleanup error replace the primary failure. Close-once owners replay their first terminal cleanup result. A context timeout while drain is still incomplete is retryable and must not mark later ownership phases complete.
- Remove compatibility fallbacks instead of forwarding them. A symbol used only by tests is deleted unless a documented production compatibility boundary remains.
- Use `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && ...'` for every Go command. Do not run unsourced `go`, `make`, or `golangci-lint` commands.
- Lane workers do not commit, push, merge, or rebase. The integration owner reviews each lane, reruns its gate, then creates the integration checkpoint.

## Accepted Interfaces and Ownership Ledger

The plan must compile against these already accepted interfaces; do not add another adapter layer:

```go
// pkg/generation
type PublicationEngine interface {
    Prepare(context.Context, ApplyTicket, Snapshot, map[Domain]PublishedGeneration) (PublicationSet, error)
    DiscardPrepared(context.Context, PublicationSet) error
    Activate(context.Context, PublicationToken, PublicationSet) error
    RollbackActivation(context.Context, PublicationToken, PublicationSet) error
    FinalizeActivation(context.Context, PublicationToken, PublicationSet)
    ConfirmActive(context.Context, PublicationSet) error
}

type Journal interface {
    ApplyDesired(context.Context, DesiredBatch) (ApplyTicket, error)
    LoadDesired(context.Context, uint64) (Snapshot, error)
    LoadPublished(context.Context, Domain) (PublishedGeneration, error)
    LoadAcknowledgement(context.Context, ProviderCursor) (Acknowledgement, error)
    Stage(context.Context, ApplyTicket, PublicationSet) (PublicationToken, error)
    Commit(context.Context, PublicationToken) (Acknowledgement, error)
    Abort(context.Context, PublicationToken, string) error
    Revisions(context.Context) (RevisionSet, error)
    Recover(context.Context) (RecoveryState, error)
    Close() error
}
```

```go
// pkg/compiler, after T9-5
func NewWorkerCompilerFactory(
    *capability.Manifest,
    *config.EffectiveConfig,
    secret.Materializer,
    WorkerRuntimeObservers,
) (*WorkerCompilerFactory, error)

func (*WorkerCompilerFactory) PrepareGeneration(
    context.Context,
    generation.ApplyTicket,
    generation.Snapshot,
    map[generation.Domain]generation.PublishedGeneration,
    func(runtime.TaskFailure),
) (*PreparedGeneration, error)

func (*WorkerCompilerFactory) PrepareRecovery(
    context.Context,
    generation.RevisionSet,
    map[generation.Domain]generation.PublishedGeneration,
    func(runtime.TaskFailure),
) (*PreparedGeneration, error)

func (*WorkerCompilerFactory) Close(context.Context) error
```

The engine may use `PreparedGeneration.PublicationSet`, `HTTP`, `Stream`, exact `DiscardPrepared`, and `Close`; it must not reach into its fields. `HTTPSnapshot.Handler/TLSConfig` and `StreamSnapshot.Router` are immutable, revocable views whose owner remains the `PreparedGeneration`.

The post-T9-3/T9-4 runtime boundaries are:

```go
// package server
type httpGenerationLease struct {
    Snapshot *compiler.HTTPSnapshot
    Release  func()
}

type streamGenerationLease struct {
    Router  *stream.Router
    Release func()
}

// package stream
type RouterLease struct {
    Router  *stream.Router
    Release func()
}

type RouterSource func() (RouterLease, bool)

func NewRuntime(context.Context, []config.TcpListen, RouterSource) (*Runtime, error)
```

## State Machine and Invariants

Use one immutable bundle pointer for all protocols:

```go
type activeBundle struct {
    http   *generationOwner
    stream *generationOwner
}

type preparedKey struct {
    Desired uint64
    HTTP    [32]byte
    Stream  [32]byte
}

type pendingRecord struct {
    key       preparedKey
    set       generation.PublicationSet
    owner     *generationOwner // nil only for zero-domain
    synthetic bool
    discarding bool
    discardDone chan struct{}
    discardErr  error
    discardWaiters int
}

type activationRecord struct {
    token     generation.PublicationToken
    key       preparedKey
    set       generation.PublicationSet
    previous  *activeBundle
    candidate *activeBundle
    owner     *generationOwner // nil only for zero-domain
    restored  bool             // post-swap Activate failure already restored predecessor
}

type retirementRecord struct {
    owner *generationOwner
    ctx   context.Context // context.WithoutCancel of the ownership transition
}
```

`generationOwner` is delivered by the runtime-leases lane in `pkg/server/generation_owner.go`. It owns one `*compiler.PreparedGeneration`, its defensive publication identity, an `ownerDomain` bit mask, acquired lease count, retirement/queue state, a close-once result, and a close completion channel. Slot transitions are serialized by the engine mutex; domain/lease eligibility is checked under the owner lifecycle mutex. Acquisition loads one bundle pointer, locks the selected owner, and increments a lease only while that exact domain bit remains active. An owner whose HTTP bit was removed must reject a stale HTTP acquisition even if its stream bit remains active. When deactivation removes the final active domain, the engine queues the owner exactly once even if leases remain; release only closes the one-way `drained` signal after both the active-domain mask and lease count reach zero.

| State | Entry | Allowed exit | Required effect |
| --- | --- | --- | --- |
| absent | no pending/token | `Prepare` | validate exact ticket/set |
| pending real | factory returned one prepared owner | discard or activate | no runtime visibility |
| pending synthetic | zero required domains | discard or activate | no factory/owner work |
| tentative | token bound and bundle swapped | rollback or finalize | predecessor retained for rollback |
| active | durable commit finalized | later activation or close | exact per-domain fence installed |
| retiring | no active slot, leases may remain | retired | reject new leases; natural drain |
| retired | zero slots and zero leases | closed | one noncancelled `PreparedGeneration.Close` |
| closed | engine terminal | none | factory closes after every owner |

Activation under the engine mutex performs these operations in order:

1. Remove the exact pending record by `preparedKey` and deep publication identity.
2. Clone the current bundle and replace only domains present in `set.Domains`.
3. Activate one candidate domain bit for each replaced domain. Do not change untouched slot references.
4. Store the complete activation record before publishing `active.Store(candidate)` so every partial/error path can restore the predecessor.
5. Keep every replaced predecessor domain bit active during the tentative window. This is the rollback reference: do not deactivate the predecessor and do not close its `drained` signal before durable commit.
6. Validate everything possible before the swap. If a post-swap checkpoint fails, restore `previous`, deactivate only candidate domain bits, set `activationRecord.restored`, keep the record rollback-safe, and return the error.

Finalize keeps candidate domain bits, deactivates only the predecessor domains that were replaced, installs exact candidate fences for those domains, removes the activation, sets `initialized`, queues every predecessor that now owns no active domain, and returns even when leases remain. Rollback first stores `previous`, removes the activation, deactivates only candidate domains, queues the rejected owner, and waits for that owner's close completion; predecessor domains were never deactivated and tentative candidate leases drain naturally. Do not add a separate activation-hold counter.

## File and Responsibility Map

- Create: `pkg/generation/provider.go`.
- Create: `pkg/server/generation_engine.go`, `pkg/server/generation_engine_test.go`.
- Consume: `pkg/server/generation_owner.go`, `pkg/server/generation_owner_test.go` from the runtime-leases lane; this engine lane does not create a second owner implementation.
- Modify: `pkg/etcd/watcher.go`, `pkg/etcd/watcher_test.go`.
- Modify: `pkg/config/standalone.go`, `pkg/config/standalone_test.go`.
- Modify: `cmd/config.go`, `cmd/config_test.go`, `cmd/root.go`, `cmd/root_test.go`.
- Modify: `pkg/server/server.go`, `pkg/server/server_test.go`, `pkg/server/tls.go`, `pkg/server/route_handler.go`, and focused server lease/TLS/stream tests delivered by T9-3/T9-4.
- Delete: `pkg/server/reload.go`, `pkg/server/reload_test.go` after provider cutover is green.
- Delete: `pkg/route/builder.go` after T9-2 pure-helper extraction is merged; migrate every remaining Builder test call site.
- Delete: `pkg/proxy/registry.go`, `pkg/proxy/registry_test.go`, `pkg/proxy/registry_metrics_test.go` after T9-5 observer injection is merged.
- Delete the legacy Store/Event implementation and tests listed in Task 9 below while retaining `OpenJournal`, the journal-only `Store` database owner, and journal schema/apply/publish/recovery files.
- Modify the plugin compatibility files named by the Task 9 `rg` inventory to remove global consumer/proto/upstream/secret fallbacks; do not replace them with another package global.

---

## Task 1: Freeze the Integration Preconditions and Add Engine RED Fixtures

**Files:**

- Create: `pkg/server/generation_engine_test.go`
- Test support only: existing compiler/generation fixtures in `pkg/server` tests

- [ ] Verify that T9-1 through T9-5 have landed before touching engine code:

  ```bash
  git status --short
  rg -n 'type GenerationSecretResolver|func NewGenerationSecretResolver' pkg/secret
  rg -n 'type WorkerRuntimeObservers|func NewWorkerCompilerFactory' pkg/compiler/worker_factory.go
  rg -n 'type RouterSource|func NewRuntime' pkg/stream/runtime.go
  rg -n 'type httpGenerationLease|type streamGenerationLease' pkg/server
  rg -n 'type ownerDomain|activeDomains|func \(.*\*generationOwner\) (activateDomains|deactivateDomains|acquireHTTP|acquireStream|closePrepared)' pkg/server/generation_owner.go
  ! rg -n 'NewBuilder\(' pkg/compiler --glob '*.go'
  ```

  Expected: the worktree is clean at the integration checkpoint and every consumed symbol exists. If any check fails, stop; do not copy another lane's implementation into this lane.

- [ ] Add a real `newGenerationEngineFixture` that constructs the current manifest, effective config, explicit no-op `WorkerRuntimeObservers`, T9-1 resolver/materializer, and `WorkerCompilerFactory`. Use journal snapshots/publication candidates rather than a fake method set on the concrete factory.

- [ ] Add these failing tests before creating `generation_engine.go`:

  - `TestGenerationEnginePrepareRetainsExactCompositeIdentity`
  - `TestGenerationEnginePrepareZeroDomainDoesNotCallFactory`
  - `TestGenerationEngineDiscardRequiresExactIdentityAndClosesOnce`
  - `TestGenerationEnginePublicSurfaceIsFrozen`
  - `TestGenerationEngineHTTPOnlyActivationPreservesStreamOwner`
  - `TestGenerationEngineStreamOnlyActivationPreservesHTTPOwner`
  - `TestGenerationEngineCompositeActivationPublishesOneBundle`
  - `TestGenerationEngineConcurrentPrepareRejectsDuplicateIdentity`

  Each behavior test must assert candidate set equality, factory live-owner count, active owner pointer identity, domain slot count, and exact close/detach count. The mismatch cases mutate desired revision, domain membership, artifact digest, snapshot ID, and decisions separately and prove the original pending owner remains live. The public-surface guard uses AST plus reflection to reject exported fields/embedding and permits only `Prepare`, `DiscardPrepared`, `Activate`, `RollbackActivation`, `FinalizeActivation`, `ConfirmActive`, `InstallRecovery`, and `Close`; it also asserts the constructor is exactly `NewGenerationEngine(*Server, *compiler.WorkerCompilerFactory) (*GenerationEngine, error)`.

- [ ] Run the exact RED gate:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^TestGenerationEngine(Prepare|Discard|HTTPOnly|StreamOnly|Composite|ConcurrentPrepare)" -count=1'
  ```

  Expected: FAIL because `GenerationEngine` does not exist. Record the first compile/test failure in the implementation report; do not weaken the tests to obtain RED.

## Task 2: Implement Pending Records and Composite Activation

**Files:**

- Create: `pkg/server/generation_engine.go`
- Modify: `pkg/server/generation_engine_test.go`

- [ ] Implement `preparedKeyFromSet` as the sole identity constructor. Reject zero desired revision, unknown domains, inconsistent candidate domain/revision/snapshot identity, or more than HTTP and stream. Preserve domain presence through the exact stored `PublicationSet`; the zero digest fields in `preparedKey` are not sufficient to infer presence.

- [ ] Implement the private engine state:

  Add `var _ generation.PublicationEngine = (*GenerationEngine)(nil)` in the production file. Do not satisfy the interface through embedding.

  ```go
  type GenerationEngine struct {
      server  *Server
      factory *compiler.WorkerCompilerFactory

      mu          sync.Mutex
      closed      bool
      recoveryInstalled bool
      initialized bool
      pending     map[preparedKey]*pendingRecord
      activations map[generation.PublicationToken]*activationRecord
      fences      map[generation.Domain]generation.PublicationCandidate
      active      atomic.Pointer[activeBundle]

      retireMu    sync.Mutex
      retireQueue []retirementRecord
      retireWake  chan struct{}
      retireStop  chan struct{}
      retireDone  chan struct{}

      checkpoint func(string) error // nil in production; package-private deterministic test barrier

      closeOnce sync.Once
      closeErr  error
  }
  ```

  `NewGenerationEngine` rejects nil server/factory, initializes maps/channels and one empty bundle, starts exactly one retirement loop, and transfers factory ownership only on success. Use the runtime lane's `generationOwner` and a private one-time server binder; do not add a second owner type or an exported setter.

- [ ] Implement `Prepare`:

  1. Check non-nil context and cancellation before locking, then require `recoveryInstalled` and reject a closed engine.
  2. Validate ticket/desired/previous consistency using existing generation validators.
  3. For zero domains, store one synthetic pending record without calling the factory.
  4. Otherwise call `factory.PrepareGeneration(ctx, ticket, desired, previous, engine.onTaskFailure)`, read its defensive `PublicationSet`, validate it against the ticket, and store one real pending owner.
  5. On duplicate key or engine close, call exact `prepared.DiscardPrepared(context.WithoutCancel(ctx), set)` outside the engine mutex and join cleanup errors.
  6. Never publish a snapshot from `Prepare`.

- [ ] Implement `DiscardPrepared` with the per-record `discarding/discardDone/discardErr/discardWaiters` state: the first exact caller marks the deep-equal pending record discarding under the engine mutex and closes outside it; concurrent exact callers increment the waiter count, wait for, and replay that cleanup result. Each waiter decrements under the mutex and the last observer removes the terminal record. A later unknown/mismatched set returns `compiler.ErrPreparedSetMismatch` and does not touch another record. Synthetic discard uses the same coordination without owner work.

- [ ] Implement `Activate` using the state-machine order above. It must validate `PreparedGeneration.HTTP/Stream` before the atomic store, bind the exact journal token once, and leave a rollback record even if the package-private `candidate-bundle-published` checkpoint fails after the bundle store. Production leaves `checkpoint` nil; do not export the seam. A zero-domain activation only moves the synthetic record from pending to activations.

- [ ] Implement private `GenerationEngine.acquireHTTP` and `acquireStream` against `engine.active`, then pass those methods to the server protocol surfaces:

  - HTTP and TLS load `bundle.http`.
  - Stream loads `bundle.stream`.
  - Missing recovery installation, a missing slot, closed engine, revoked snapshot, or stale owner returns `(zero, false)` without falling back to Store or a previous global.
  - Every `Release` is protected by its own `sync.Once`.

- [ ] Run GREEN and focused race:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^TestGenerationEngine(Prepare|Discard|HTTPOnly|StreamOnly|Composite|ConcurrentPrepare)" -count=1'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^TestGenerationEngine(Prepare|Discard|HTTPOnly|StreamOnly|Composite|ConcurrentPrepare)" -count=1'
  ```

  Expected: PASS. Inspect `git diff -- pkg/server/generation_engine.go pkg/server/generation_engine_test.go` before continuing.

## Task 3: Prove Rollback, Nonblocking Finalize, Lease Drain, and Replay

**Files:**

- Modify: `pkg/server/generation_engine.go`
- Modify: `pkg/server/generation_engine_test.go`
- Modify only if a regression is exposed: `pkg/generation/coordinator_test.go`

- [ ] Add RED tests:

  - `TestGenerationEngineRollbackRestoresCompletePredecessorBundle`
  - `TestGenerationEngineRollbackWaitsForTentativeLeaseButNotPredecessor`
  - `TestGenerationEngineFinalizeReturnsBeforePredecessorLeaseDrains`
  - `TestGenerationEngineFinalizeRetiresReplacedOwnerExactlyOnce`
  - `TestGenerationEngineConfirmActiveChecksRequestedSubsetOnly`
  - `TestGenerationEngineConfirmActiveZeroDomainRequiresInitializedFence`
  - `TestGenerationEngineCommittedReplayDoesNotCompileOrMutate`
  - `TestGenerationEngineConcurrentCloseAndReleaseReplaysFirstCleanupError`

  Use barriers, not sleeps: hold a candidate request/stream lease, invoke rollback/finalize in another goroutine, signal when the bundle/fence transition happens, and release the lease explicitly. Block the retirement worker at a test-only checkpoint and prove `FinalizeActivation` returns without calling or waiting for `PreparedGeneration.Close`; then release the checkpoint and prove cleanup completes.

- [ ] Extend the existing coordinator commit-failure test to use the real engine:

  ```go
  func TestGenerationEngineRollsBackActivatedGenerationWhenJournalCommitFails(t *testing.T)
  ```

  Seed an active predecessor, make `Journal.Commit` return `errCommit`, and prove the predecessor HTTP and stream slots are restored, candidate leases close once, `Abort` receives the staged token, and `errors.Is` retains commit/rollback/abort sentinels. Add the equivalent activation-checkpoint failure and assert finalize was never called.

- [ ] Run RED:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server ./pkg/generation -run "(GenerationEngineRollback|GenerationEngineFinalize|GenerationEngineConfirmActive|GenerationEngineCommittedReplay|RollsBackActivatedGeneration)" -count=1'
  ```

  Expected: FAIL on missing rollback/finalize/retirement behavior.

- [ ] Consume the runtime lane's owner lifecycle helpers. During tentative activation the predecessor's replaced domain bits remain active until finalize, so a goroutine that loaded the predecessor bundle before the atomic swap may still complete its old-domain acquisition; that lease is valid and pins the predecessor. Finalize removes the predecessor domain bit, after which a stale acquisition rejects and retries the current bundle. Do not add `activationHolds` or deactivate/reactivate the predecessor around rollback.

- [ ] Implement `RollbackActivation`:

  1. Verify exact token, key, and deep set identity under the engine mutex; impossible token/set mismatches panic because the coordinator owns both values.
  2. If `record.restored` is false, store the complete predecessor bundle and deactivate candidate domains; predecessor domain bits are still active and need no reactivation. If `record.restored` is true, do not deactivate the candidate twice.
  3. Delete the activation and make the candidate ineligible for new leases.
  4. Queue the candidate once and wait only for its close completion. Use `context.WithoutCancel` for the actual cleanup while still returning a caller wait-context error if the wait is abandoned; background retirement must continue.
  5. Never decrement or close predecessor slots restored by rollback.

- [ ] Implement `FinalizeActivation`:

  1. Verify token/set/record identity; panic on an impossible invariant after journal commit.
  2. For real activation, install exact defensive per-domain candidate fences, keep candidate domains active, deactivate only replaced predecessor domains for the first time, delete the activation, and enqueue each predecessor whose active-domain mask is now zero, regardless of remaining leases.
  3. For synthetic activation, delete the record and set `initialized` only.
  4. Signal the retirement loop with `select { case retireWake <- struct{}{}: default: }` and return while still no owner cleanup has begun on the caller goroutine.

- [ ] Implement `ConfirmActive` as a read-only exact fence check. Check context before locking and immediately after acquiring the mutex. Zero-domain replay requires `initialized`; non-empty replay compares every requested candidate identity and permits an unrelated active domain at another revision. Return `generation.ErrActiveGenerationMismatch` for a missing/mismatched fence and never compile, swap, close, enqueue, or update metrics.

- [ ] Implement one retirement loop that pops queued owners outside engine/owner locks and calls the runtime lane's `owner.closePrepared(record.ctx)`. `closePrepared` waits for the owner's one-way `drained` signal before closing. Queue `context.WithoutCancel(ctx)` from finalize, rollback, or close so cleanup cannot be cancelled but still preserves caller values. Store the owner's first close result and record cleanup errors for final engine close. `closePrepared` owns its completion channel and exact-once close. Duplicate queue attempts are harmless and do not duplicate cleanup.

- [ ] Run GREEN and race repeatedly enough to expose ordering errors:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server ./pkg/generation -run "(GenerationEngineRollback|GenerationEngineFinalize|GenerationEngineConfirmActive|GenerationEngineCommittedReplay|RollsBackActivatedGeneration)" -count=20'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server ./pkg/generation -run "(GenerationEngineRollback|GenerationEngineFinalize|GenerationEngineConfirmActive|GenerationEngineCommittedReplay|RollsBackActivatedGeneration)" -count=10'
  ```

  Expected: PASS without sleep-based flakes or leaked goroutines.

## Task 4: Install Recovery from Published Artifacts and Close the Engine

**Files:**

- Modify: `pkg/server/generation_engine.go`
- Modify: `pkg/server/generation_engine_test.go`
- Use existing: `pkg/compiler/worker_factory_recovery_test.go`

- [ ] Add RED tests:

  - `TestGenerationEngineInstallRecoveryUsesPublishedNotDesired`
  - `TestGenerationEngineInstallRecoveryPreservesIndependentDomainRevisions`
  - `TestGenerationEngineInstallRecoveryLeavesMissingDomainUnavailable`
  - `TestGenerationEngineInstallEmptyJournalLeavesReplayUninitialized`
  - `TestGenerationEngineInstallDesiredOnlyRecoveryInitializesZeroDomainReplay`
  - `TestGenerationEngineRejectsSecondRecoveryInstall`
  - `TestGenerationEngineCloseRejectsNewWorkAndClosesOwnersBeforeFactory`
  - `TestGenerationEngineCloseReplaysJoinedOwnerAndFactoryErrors`

  Put marker A only in `RecoveryState.Desired` and marker B in committed `Published`; HTTP/TLS/stream observations and the resolver spy must prove only B is materialized. For independent revisions use desired 42, HTTP 40, stream 41. For missing stream include the journal recovery failure metadata but no stream published head; HTTP remains available and stream acquisition fails closed.

- [ ] Run RED:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server ./pkg/compiler -run "(GenerationEngineInstall|GenerationEngineClose|WorkerCompilerFactoryPrepareRecovery)" -count=1'
  ```

- [ ] Implement `InstallRecovery`:

  1. Require a non-nil context and an engine with no previous recovery installation, pending record, activation, initialized fence, or active slot.
  2. Special-case only the exact empty-journal recovery state: zero `RevisionSet`, zero `Desired`, empty/nil `Published`, and no failures. Mark `recoveryInstalled=true`, leave `initialized=false`, keep an empty bundle, and do not call `ValidateRecoverySet` or the factory. This allows first startup while ensuring a zero-domain committed replay cannot be falsely confirmed.
  3. For every nonempty recovery state, validate `RecoveryState.Revisions` against the complete `RecoveryState.Published` map using `generation.ValidateRecoverySet`.
  4. Ignore `RecoveryState.Desired` after validation of the envelope; do not pass it to any compiler/resolver/materializer API.
  5. If `Revisions.Desired > 0` and `Published` is empty, mark `recoveryInstalled=true` and `initialized=true` with an empty bundle; this is a recovered durable zero-domain commit and does not call `PrepareRecovery`.
  6. Otherwise call `factory.PrepareRecovery(ctx, state.Revisions, state.Published, onFailure)` exactly once; validate that returned HTTP/stream snapshots correspond to the published domains, create one owner, add one slot per published domain, atomically store the bundle, clone exact per-domain fences, then set both `recoveryInstalled` and `initialized`.
  7. On any pre-install error, close the prepared recovery owner outside locks and leave both fences false so startup fails closed.

- [ ] Implement engine `Close` as a terminal ownership barrier:

  1. Normalize nil context, enter the close-once path, set `closed`, atomically publish an empty bundle, and reject all new prepare/acquisition calls.
  2. Remove pending records, activations, retiring records, and active slot references under locks while deduplicating owners by pointer.
  3. Close synthetic records without owner work; queue/await every real owner exactly once.
  4. Stop and join the retirement loop only after its queue is empty and all owner close results are recorded.
  5. Close `WorkerCompilerFactory` last with `context.WithoutCancel(ctx)`.
  6. Store `errors.Join(ownerErrors..., factoryErr)` and replay that exact result on later calls. Preserve context values while ignoring cancellation for already-owned cleanup.

- [ ] Run GREEN and race:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server ./pkg/compiler -run "(GenerationEngineInstall|GenerationEngineClose|WorkerCompilerFactoryPrepareRecovery)" -count=20'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server ./pkg/compiler -run "(GenerationEngineInstall|GenerationEngineClose|WorkerCompilerFactoryPrepareRecovery)" -count=10'
  ```

---

## Task 5: Cut etcd to `DesiredApplier` and Ack-Only State

**Files:**

- Create: `pkg/generation/provider.go`
- Modify: `pkg/etcd/watcher.go`
- Modify: `pkg/etcd/watcher_test.go`

- [ ] Add `generation.DesiredApplier` and a compile-time assertion in tests that `*generation.Coordinator` satisfies it. Do not add `Start`, `Stop`, readiness, or provider-specific methods to this interface.

- [ ] Add RED provider tests using a recording/failing applier:

  - `TestConfigClientSnapshotAppliesCanonicalDesiredBatch`
  - `TestConfigClientWatchAdvancesOnlyAfterAcknowledgement`
  - `TestConfigClientFailedApplyRetainsCursorKnownKeysDecisionsAndQuarantine`
  - `TestConfigClientSameCursorReplayUsesCommittedAcknowledgement`
  - `TestConfigClientAcknowledgementCursorMismatchFailsClosed`
  - `TestConfigClientCompilerOrJournalFailureRetriesSameProviderPosition`
  - `TestConfigClientShutdownCancellationDoesNotCommitProviderState`

  The failure test must snapshot all local fields and metric gauges before `Apply`, force an error, and deep-compare after. The retry test must assert the next watch revision remains `lastAcknowledgedRevision + 1`, not the rejected response header revision.

- [ ] Run RED:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/etcd -run "^TestConfigClient(Snapshot|Watch|FailedApply|SameCursor|Acknowledgement|Compiler|Shutdown)" -count=1'
  ```

- [ ] Replace the `events chan *store.Event` field and constructor arguments with `applier generation.DesiredApplier`:

  ```go
  func NewConfigClient(
      endpoints []string,
      username, password, prefix string,
      applier generation.DesiredApplier,
  ) (*ConfigClient, error)

  func NewConfigClientWithOptions(
      endpoints []string,
      username, password, prefix string,
      applier generation.DesiredApplier,
      options ClientOptions,
  ) (*ConfigClient, error)
  ```

  Reject a nil/typed-nil applier before opening the etcd client. Keep `desiredBatchFromEtcdSnapshot` and `desiredBatchFromEtcdWatch` byte-for-byte canonical unless a focused translator test proves a bug.

- [ ] Delete `sendBatch`, `sendEvent`, `applyMutationBatch`, `store.Mutation`, `store.BatchOptions`, and `store.BatchValidationError` paths. Both snapshot and watch call `ack, err := c.applier.Apply(ctx, batch)` exactly once. Do not split a rejected batch into pruned retries; compiler/journal decisions are already represented in the acknowledgement.

- [ ] Add a private acknowledgement commit helper that first validates `ack.Cursor == batch.Cursor`, revisions are nonzero/monotonic, and each decision key belongs to the submitted/acknowledged managed state. Derive provider-local quarantine from acknowledged decisions: `LastGood`, `Quarantined`, or `FailClosed` keeps the corresponding full etcd key quarantined at its acknowledged revision; `Published` or `Deleted` clears it only when every affected domain acknowledges a non-rejected disposition.

- [ ] Commit `knownKeys`, `lastRevision`, cloned decisions, quarantine, `RecordEtcdModifyIndex`, `RecordEtcdAppliedRevision`, provider success, and readiness only after the helper succeeds. On any translator/apply/ack-validation error record only the bounded failure metric; leave all acknowledged state untouched.

- [ ] Preserve existing health/watch/backoff semantics. `recoverSnapshot` resubmits a full authoritative snapshot through the same applier. Same-cursor committed replay is success because `Coordinator.Apply` confirms the installed fence and returns the stored acknowledgement.

- [ ] Keep `server-info` reporting inside the etcd provider lifetime. Start it only after the client/provider is retained, and make provider stop cancel/join the reporter before closing the etcd client; it must not outlive the provider or read engine/journal state after shutdown begins.

- [ ] Run translator, provider, and race gates:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/etcd -run "(DesiredBatchFromEtcd|ConfigClient|ApplyWatchResponse|ApplySnapshot|Watch)" -count=1'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/etcd -run "(ConfigClient|ApplyWatchResponse|ApplySnapshot|Watch)" -count=10'
  ```

## Task 6: Cut Standalone File Provider to `DesiredApplier`

**Files:**

- Modify: `pkg/config/standalone.go`
- Modify: `pkg/config/standalone_test.go`

- [ ] Add RED tests:

  - `TestStandaloneWatcherAppliesCanonicalFullSnapshotThroughDesiredApplier`
  - `TestStandaloneWatcherFailedApplyRetainsAcknowledgedCursorAndDecisions`
  - `TestStandaloneWatcherSameContentReplaysCommittedCursor`
  - `TestStandaloneWatcherParseFailureDoesNotCallApplier`
  - `TestStandaloneWatcherDoesNotRetainInMemoryLastGoodSnapshot`
  - `TestStandaloneWatcherStopWaitsForApplyAndDoesNotAdvanceAfterCancellation`

  The last-good test writes valid A, applies it, writes an invalid replacement plus valid sibling B, and proves no local A bytes are copied into the next batch. Structurally untranslatable input fails the file apply as a whole and leaves durable acknowledged A active; semantically valid bytes are submitted and compiler decisions determine `Published/LastGood/Quarantined/FailClosed`.

- [ ] Run RED:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/config -run "^TestStandaloneWatcher" -count=1'
  ```

- [ ] Change `NewStandaloneFileWatcher(path, provider, applier, encryption)` to require `generation.DesiredApplier`. Delete the Store event channel, `SeedCurrentSnapshot`, `StandaloneReloadResult`, acknowledged reload callbacks, Store `ResourceKey` conversions, Store mutation construction, and `enqueueAndWait`.

- [ ] Keep `desiredBatchFromStandalone` as the one canonical full-snapshot translator. Continue deterministic kind/ID sorting and the SHA-256 content cursor. Do not use file mtime, callback sequence, or a process-local counter as the cursor.

- [ ] Remove `current standaloneSnapshot`, `retainStandaloneLastGood`, previous/next diffing, and route/stream changed-bucket state. `reconcile` becomes:

  ```go
  snapshot, parseErr := readStandaloneSnapshot(...)
  if parseErr != nil { return parseErr }
  batch := desiredBatchFromStandalone(snapshot)
  ack, applyErr := w.applier.Apply(w.ctx, batch)
  if applyErr != nil { return applyErr }
  return w.commitAcknowledgement(batch, ack)
  ```

  If normalization cannot produce an exact key/value (missing ID, invalid JSON shape, encryption failure), reject the whole file without applying valid siblings; this keeps the prior durable publication and avoids a provider-local last-good shadow. Schema/plugin/resource validity after canonical bytes exist belongs to compiler decisions.

- [ ] Retain only `Start`, `StartAndReconcile`, `Reload`, `Watch`, and `Stop` methods used by production. Delete exported `ReloadSnapshot` and callback setters if the call-site scan confirms tests are their only remaining users. `Stop` cancels, joins fsnotify and any in-flight apply, never closes coordinator/journal, and replays its first stop result.

- [ ] Derive standalone quarantine/readiness solely from the returned acknowledgement using the same disposition rule as etcd. A failed apply retains the last acknowledged cursor/decisions/quarantine metrics. Do not store raw resource bytes after submission.

- [ ] Run focused and race gates:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/config -run "(DesiredBatchFromStandalone|StandaloneWatcher|StandaloneFileWatcher)" -count=1'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/config -run "(StandaloneWatcher|StandaloneFileWatcher)" -count=10'
  ```

---

## Task 7: Wire `cmd` Bootstrap and Transfer Ownership Once

**Files:**

- Modify: `cmd/config.go`
- Modify: `cmd/config_test.go`
- Modify: `cmd/root.go`
- Modify: `cmd/root_test.go`
- Modify: `pkg/server/server.go`
- Modify: `pkg/server/server_test.go`

- [ ] Add RED tests for exact construction order and cleanup:

  - `TestStartupLoadsOneManifestForEffectiveConfigAndCompiler`
  - `TestStartupOpensAndRecoversJournalBeforeServerConstruction`
  - `TestStartupImportsLegacyBucketsAsDesiredOnlyBeforeProvider`
  - `TestStartupJournalRecoveryFailureClosesJournalOnly`
  - `TestNewServerRecoveryInstallFailureClosesEngineResolverJournalAndObservabilityInReverse`
  - `TestNewServerFactoryFailureClosesResolverAndJournalWithoutDoubleClose`
  - `TestNewServerRejectsNilDependencyBeforeTakingOwnership`

  Use an unexported `startupFactories` test seam in `cmd`, passed explicitly to `startWithOptionsWithFactories`; do not use mutable package globals. Record calls in a synchronized slice and compare the full sequence.

  The legacy-import test starts from a pre-cutover database with one legacy resource bucket and no generation journal marker. It must prove `OpenJournal` imports that resource into `RecoveryState.Desired`, deletes the legacy bucket transactionally, and that startup does not pass those Desired bytes to `PrepareRecovery`; the first provider reconciliation is the only path that may compile and publish them.

- [ ] Run RED:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./cmd ./pkg/server -run "(StartupLoadsOneManifest|StartupOpensAndRecovers|StartupJournalRecoveryFailure|NewServerRecoveryInstallFailure|NewServerFactoryFailure|NewServerRejectsNil)" -count=1'
  ```

- [ ] Change `loadEffectiveForStartup` to return the exact manifest used by `loadEffectiveForManifest`, plus the effective config and declaration catalog. Do not call `capability.Load` again in `server` or compiler bootstrap.

- [ ] In `cmd.startWithOptions` perform this exact order:

  1. Load manifest, effective config, and catalog once.
  2. Build the configured data-encryption service and logger.
  3. Call the T9-1 public constructor `secret.NewGenerationSecretResolver(encryption)` exactly once. HTTP client injection remains a package-private resolver test seam; bootstrap must not depend on or expose an options type.
  4. Validate `config.JournalPath(effective)`, create only its parent data directory with mode `0700`, and call `store.OpenJournal(path, store.JournalOptions{LegacyResourceBuckets: generation.ManagedResourceKinds()})`. Keep this option in production so an existing pre-cutover database is imported transactionally on its first post-upgrade open; `OpenJournal` deletes imported legacy buckets and ignores the list for an already initialized journal.
  5. Call `journal.Recover(ctx)` before provider construction, listener binding, factory construction, or runtime serving.
  6. Transfer manifest/effective/encryption/resolver/journal/recovery into `server.NewServer` exactly once.
  7. Call `runServer` only after `NewServer` has installed recovery.

  If resolver creation fails, no journal exists. If open fails, close resolver. If recover fails, close journal then resolver. Once `NewServer` accepts all non-nil inputs, it owns and cleans resolver/journal even when a later constructor fails.

- [ ] Replace the server constructor with the explicit accepted ownership transfer:

  ```go
  func NewServer(
      effective *config.EffectiveConfig,
      manifest *capability.Manifest,
      encryption data_encryption.Service,
      resolver *secret.GenerationSecretResolver,
      journal generation.Journal,
      recovery generation.RecoveryState,
  ) (*Server, error)
  ```

  Validate all dependencies before transfer. Construct the server shell/observers, `secret.NewMaterializer(encryption, resolver)`, `WorkerCompilerFactory`, `GenerationEngine`, and `Coordinator` once. Call `engine.InstallRecovery` before returning. `Server` owns coordinator, engine, resolver, journal, HTTP server, stream runtime, provider, and observability; factory ownership belongs exclusively to engine.

- [ ] Build `WorkerRuntimeObservers` from `newClusterObserver(cfg)` and `logStreamResult`. Require the stream callback only when stream mode is enabled. Do not recreate `ClusterRegistry`.

- [ ] Ensure `runServerWithSignals` creates shutdown context from `context.Background()`, not from the already cancelled serving context, so the 30-second drain window is usable. Preserve SIGHUP unsupported behavior after cleanup.

- [ ] Run bootstrap GREEN and focused race:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./cmd ./pkg/server -run "(Startup|NewServer|RunServer|RecoveryInstall)" -count=1'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./cmd ./pkg/server -run "(Startup|NewServer|RunServer|RecoveryInstall)" -count=10'
  ```

## Task 8: Start Recovery/Listeners Before Provider and Reverse Shutdown

**Files:**

- Modify: `pkg/server/server.go`
- Modify: `pkg/server/server_test.go`
- Modify: `pkg/server/route_handler.go` and T9-3 tests only where needed for drain/reject-new semantics
- Modify: `pkg/server/tls.go`
- Modify: `pkg/server/stream_test.go`

- [ ] Add RED lifecycle tests:

  - `TestServerStartsRecoveredEngineThenStreamHTTPAndProvider`
  - `TestServerOfflineRecoveryServesWhileProviderReadinessIsDegraded`
  - `TestServerProviderStartFailureClosesStartedListenersBeforeEngine`
  - `TestServerShutdownStopsProviderBeforeRejectingNewLeases`
  - `TestServerShutdownDrainsHTTPHijackAndStreamBeforeEngineClose`
  - `TestServerShutdownTimeoutDoesNotReleaseEngineResolverOrJournal`
  - `TestServerShutdownClosesEngineResolverJournalObservabilityInOrder`
  - `TestServerRepeatedShutdownReplaysFirstTerminalCleanupError`

  Use explicit channels at provider stop, listener close, request/hijack/stream release, engine close, resolver close, journal close, and observability close. No timing sleeps. The timeout test performs a second shutdown after releasing the lease and proves cleanup resumes rather than double-closing earlier owners.

- [ ] Run RED:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^TestServer(StartsRecovered|OfflineRecovery|ProviderStartFailure|Shutdown)" -count=1'
  ```

- [ ] Refactor `Server.Start` into explicit phases:

  Use one unexported lifecycle boundary for both provider implementations:

  ```go
  type configProducer interface {
      Start(context.Context) error
      Stop() error
  }
  ```

  `Stop` must cancel and join every goroutine/in-flight `Apply` before returning, even when the underlying client close reports an error. `Server` invokes it through one private `producerStopOnce/producerStopDone/producerStopErr` barrier: start terminal stop once, wait on `producerStopDone` or the shutdown context, and resume the same wait on a later `Shutdown`. Never start duplicate stop goroutines and never advance to listener/engine cleanup while provider stop is incomplete.

  1. Begin lifecycle and initialize bounded observability/metrics owners.
  2. Set stream-required readiness policy.
  3. Start the immutable stream listener runtime with the engine's `RouterSource`; it may reject connections until recovery provides a stream slot.
  4. Bind and start HTTP/TLS listeners with engine-backed request and TLS selectors; return a serve-error channel instead of blocking inside listener setup.
  5. Construct/start the configured etcd or standalone provider with the one coordinator.
  6. Wait for root cancellation or a listener/provider terminal error.

  Recovery is already installed by `NewServer`. A transient first provider fetch/apply error keeps provider readiness false and enters current retry/backoff; it does not tear down recovered listeners. A constructor/static configuration error remains fatal and triggers reverse cleanup.

- [ ] Make the production HTTP handler acquire an engine HTTP lease per request. TLS callbacks briefly acquire the same HTTP slot, clone/select from `HTTPSnapshot.TLSConfig`, and release before returning. Remove every `store.GetSSLCertificate*` selector and never keep a TLS config beyond its owner lease.

- [ ] Start providers only through `generation.DesiredApplier`:

  - etcd constructor receives `s.coordinator`, performs an initial full snapshot attempt, then starts watch/recovery even when the remote is temporarily unavailable.
  - standalone constructor receives `s.coordinator`, registers fsnotify, then reconciles the full file. A transient read/apply failure is logged and retried on the next event without mutating acknowledged state.
  - provider `Stop` cancels and joins all in-flight `Apply` calls before returning.

- [ ] Implement shutdown phases with ownership safety:

  1. Mark shutdown requested, initiate provider stop once, and join it first, rejecting new applies. If the wait context expires, return an incomplete shutdown without touching listeners or later owners; a later call resumes the same provider-stop barrier.
  2. Close owned HTTP/TLS/stream listeners and reject new engine leases.
  3. Drain normal HTTP requests, batch leases, natural hijacks under server policy, and terminally close stream connections through `stream.Runtime.Close`.
  4. If the drain context expires, return the context error without touching engine/resolver/journal; a later `Shutdown` resumes at this phase.
  5. Close `GenerationEngine` (which closes factory) once drain is complete.
  6. Close `GenerationSecretResolver`.
  7. Close journal.
  8. Stop metric expiration/export and tracing last.

  Attempt independent cleanup in a phase even after one owner errors and join results. Do not progress past an incomplete drain. When all phases complete, store the joined first terminal errors and replay them on every later shutdown.

- [ ] Run lifecycle GREEN and race:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^TestServer(StartsRecovered|OfflineRecovery|ProviderStartFailure|Shutdown)" -count=20'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^TestServer(StartsRecovered|OfflineRecovery|ProviderStartFailure|Shutdown)" -count=10'
  ```

---

## Task 9: Delete Store/Event/Builder/ClusterRegistry/Reload and Migrate Tests

**Files:**

- Delete: `pkg/server/reload.go`, `pkg/server/reload_test.go`
- Delete: `pkg/route/builder.go`
- Delete: `pkg/proxy/registry.go`, `pkg/proxy/registry_test.go`, `pkg/proxy/registry_metrics_test.go`
- Create: `pkg/store/journal_store.go`
- Delete legacy Store files: `pkg/store/event.go`, `pkg/store/getter.go`, `pkg/store/consumer_kv.go`, `pkg/store/consumer_secret.go`, `pkg/store/plugin_metadata_cache.go`, `pkg/store/standalone_snapshot.go`, `pkg/store/resolved_secret.go`, `pkg/store/secret_broker.go`, `pkg/store/published_view.go`, and `pkg/store/store.go`
- Delete corresponding legacy-only tests: `pkg/store/event_test.go`, `pkg/store/event_ack_test.go`, `pkg/store/durable_apply_test.go`, `pkg/store/getter_test.go`, `pkg/store/store_test.go`, `pkg/store/config_snapshot_test.go`, `pkg/store/consumer_kv_test.go`, `pkg/store/consumer_kv_jwe_decrypt_test.go`, `pkg/store/consumer_secret_reference_test.go`, `pkg/store/consumer_snapshot_test.go`, `pkg/store/encryption_service_test.go`, `pkg/store/plugin_metadata_cache_test.go`, `pkg/store/standalone_snapshot_test.go`, `pkg/store/secret_broker_test.go`, `pkg/store/published_view_test.go`, `pkg/store/ssl_test.go`, `pkg/store/ssl_benchmark_test.go`, `pkg/store/stream_route_test.go`, and `pkg/store/benchmark_test.go` where their behavior is already covered by journal/compiler/runtime tests.
- Modify or delete every Builder/global-fallback test found by the call-site scans below.

- [ ] Before deletion, write the four cross-generation RED contract tests:

  - `TestGenerationEngineOldAndNewRequestsUseOwnConsumerMetadataProtoAndSecrets`
  - `TestGenerationEngineHijackedConnectionRetainsPredecessorResources`
  - `TestGenerationEngineTLSAndHTTPActivateAndRollbackTogether`
  - `TestProductionRuntimeHasNoGlobalStoreReads`

  The first test must keep an old request blocked while publishing a new bundle and observe different consumer lookup, metadata, proto bytes, and secret values from the two request contexts. The second holds an actual hijacked connection across finalize. The third observes certificate/handler A, tentatively activates B, rolls back, and observes A again while a B TLS lease drains.

- [ ] Add an AST/type-aware guard in the existing production-boundary test package. Inspect non-test Go files under `pkg/compiler`, `pkg/route`, `pkg/plugin`, `pkg/server`, and `pkg/stream`; reject:

  - selector calls on package `store` for `GetStore`, any `Get*`/`List*`, `MaterializeSecret`, or `ResolveSecretReference`;
  - declarations or calls of `routeHandler.Replace`, `stream.Runtime.Reload`, or `stream.Router.Reload`;
  - `Builder`, `NewBuilder*`, `ClusterRegistry`, Store Event channel fields, and reload scheduler owners;
  - direct bbolt/resource-bucket reads outside `pkg/store` journal/schema migration code.

- [ ] Run RED before deleting compatibility branches:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server ./pkg/plugin ./pkg/route ./pkg/stream -run "(OldAndNewRequests|HijackedConnectionRetains|TLSAndHTTPActivate|ProductionRuntimeHasNoGlobalStoreReads)" -count=1'
  ```

  Expected: FAIL because Store getters, Builder, registry, and reload symbols remain.

- [ ] Delete server mutable publication state: `events`, `storage`, `clusters`, `streamReloadMu`, `streamRoutes`, `reloadEventChan`, reload locks/generation/error fields, acknowledged Store hooks, reload scheduler, `loadStreamRoutes`, Store route merge/commit, `applyStandaloneSnapshot`, and every `Replace/Reload` call. Delete `reload.go` rather than leaving no-op scheduling facades.

- [ ] Delete `routeHandler.Replace`, `stream.Runtime.Reload`, and `stream.Router.Reload`. The only route handler source is an engine HTTP lease; the only stream router source is an engine stream lease. Keep static listener ownership and terminal `Close`.

- [ ] Delete `proxy.ClusterRegistry` after confirming compiler owners use T9-5 `WorkerRuntimeObservers.Cluster` and resource-registry leases. Move no registry singleton into server.

- [ ] Delete `route.Builder`. For every test call site:

  - pure router/rewrite/transport helpers already extracted by T9-2 are called directly with explicit immutable inputs;
  - generation binding, consumer, metadata, proto, SSL, secret, and plugin-chain behavior uses a compiler/`PreparedGeneration` fixture and closes it;
  - full server behavior uses a journal `DesiredBatch -> Coordinator.Apply` fixture;
  - tests that only assert the deleted Builder facade delegates to helpers are removed, not rewritten around a new facade.

  Inventory and eliminate every survivor:

  ```bash
  rg -n 'type Builder|NewBuilder|NewBuilderWithServerAddr|NewBuilderWithClusterRegistry|BuildWithRouteQuarantine|BuildStrict' pkg/route pkg/server --glob '*.go'
  ```

- [ ] Remove plugin global fallbacks. Start with this exact production inventory and rerun it after every batch:

  ```bash
  rg -n 'store\.(Get[A-Z][A-Za-z0-9_]*|List[A-Z][A-Za-z0-9_]*|MaterializeSecret|ResolveSecretReference)\(' pkg/plugin --glob '*.go' --glob '!*_test.go'
  ```

  Delete legacy consumer lookup branches from auth/workflow plugins; require generation-injected `ConsumerLookup` and replace `store.ErrNotFound` with each plugin's existing/private domain error. Delete proto/upstream lookup fallbacks from `grpc-transcode` and `traffic-split`; require compiled generation inputs. Delete `MaterializeSecrets` Store compatibility branches from logger/auth/cloud/AI plugins; retain only `MaterializeScopedSecrets`/prepared descriptors. Update tests to prepare explicit generation bindings. Do not change plugin error/status semantics except that a missing mandatory generation binding fails closed instead of reading a global.

- [ ] Extract the journal database owner into `pkg/store/journal_store.go`: retain only `Store.db`, `storeOpenTimeout`, `closeOnce`, `closeErr`, and idempotent `Close`; remove obsolete `stopProducers` initialization from `OpenJournal`. Then delete legacy `store.go` with `Open`, `GetStore`, `ReplaceGlobalStoreForTest`, Start/Sync/Stop event loop, event hooks, resource buckets/caches, consumer/SSL/stream last-good globals, and Store secret broker. Delete the unused transitional `PublishedView`; current production has no caller, and equivalent immutable decoding coverage belongs to compiler snapshots. The journal package name/type may remain `store.Store`, but it has no resource getter/event/runtime authority.

- [ ] Migrate rather than lose behavior coverage:

  - Store durable apply/last-good tests move to `pkg/store/journal_apply_test.go`, `journal_publish_test.go`, and compiler closure tests.
  - Store secret broker tests move to T9-1 `pkg/secret/generation_resolver_test.go` and materializer tests.
  - Store consumer/metadata/proto/SSL/stream decode tests move to compiler prepared snapshot tests.
  - Builder lifecycle/isolation tests move to compiler + engine tests.
  - registry metrics tests move to compiler observer/resource-registry tests.
  - reload coalescing tests are deleted because there is no scheduler; provider acknowledgement ordering tests replace them.

- [ ] For every deleted or moved symbol, run production and test call-site searches. A test-only survivor must be deleted or justified as a real exported compatibility contract in the review report:

  ```bash
  rg -n 'store\.Event|type Event struct|NewAcknowledged(Event|Batch)|GetStore|ReplaceGlobalStoreForTest|type Builder|NewBuilder|ClusterRegistry|routeHandler\.Replace|Runtime\.Reload|Router\.Reload' cmd pkg --glob '*.go'
  rg -n 'pkg/store' pkg/compiler pkg/route pkg/plugin pkg/server pkg/stream --glob '*.go'
  ```

- [ ] Run the migrated focused packages and race gate:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/store ./pkg/compiler ./pkg/runtime ./pkg/route ./pkg/plugin ./pkg/stream ./pkg/server ./pkg/etcd ./pkg/config -count=1'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler ./pkg/runtime ./pkg/route ./pkg/stream ./pkg/server ./pkg/etcd ./pkg/config -run "(PreparedGeneration|GenerationEngine|RouteHandler|TLS|Stream|Provider|Shutdown|Isolation)" -count=1'
  ```

---

## Task 10: Final Isolation, Absence, Formatting, Build, and Review Gates

**Files:**

- No planned product changes unless a gate finds a scoped defect
- Update tests only with a failing reproduction before repairing a confirmed defect

- [ ] Run the named isolation tests alone first:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestGenerationEngineOldAndNewRequestsUseOwnConsumerMetadataProtoAndSecrets|TestGenerationEngineHijackedConnectionRetainsPredecessorResources|TestGenerationEngineTLSAndHTTPActivateAndRollbackTogether|TestProductionRuntimeHasNoGlobalStoreReads)$" -count=20'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^(TestGenerationEngineOldAndNewRequestsUseOwnConsumerMetadataProtoAndSecrets|TestGenerationEngineHijackedConnectionRetainsPredecessorResources|TestGenerationEngineTLSAndHTTPActivateAndRollbackTogether)$" -count=10'
  ```

- [ ] Run direct absence guards; every command must produce no matches and exit successfully:

  ```bash
  ! rg -n '\bstore\.(GetStore|Get[A-Z][A-Za-z0-9_]*|List[A-Z][A-Za-z0-9_]*|MaterializeSecret|ResolveSecretReference)\(' pkg/compiler pkg/route pkg/plugin pkg/server pkg/stream --glob '*.go' --glob '!*_test.go'
  ! rg -n 'type Builder|NewBuilder|ClusterRegistry' cmd pkg --glob '*.go'
  ! rg -n 'func \([^)]*\*(routeHandler|Runtime|Router)\) (Replace|Reload)\(|\b(routes|streamRuntime|router)\.(Replace|Reload)\(' pkg/server pkg/stream --glob '*.go'
  ! rg -n 'chan \*store\.Event|NewAcknowledged(Event|Batch)|AddAcknowledgedEventUpdateHook|startReloadScheduler|listenReloadEvent' cmd pkg --glob '*.go'
  test ! -e pkg/route/builder.go
  test ! -e pkg/proxy/registry.go
  test ! -e pkg/server/reload.go
  test ! -e pkg/store/event.go
  ```

- [ ] Run the cross-cutting correctness and race gates exactly as frozen by the joint plan:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/generation ./pkg/secret ./pkg/compiler ./pkg/runtime ./pkg/route ./pkg/stream ./pkg/server ./pkg/etcd ./pkg/config -count=1'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/generation ./pkg/secret ./pkg/compiler ./pkg/runtime ./pkg/route ./pkg/stream ./pkg/server ./pkg/etcd ./pkg/config -run "(Coordinator|GenerationEngine|RecoverySecretResolver|PreparedGeneration|RouteHandler|TLS|Stream|Provider|Shutdown|Isolation)" -count=1'
  ```

- [ ] Format only after tests are green, inspect all formatter edits, and discard unrelated rewrites:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint fmt'
  git status --short
  git diff --stat
  git diff --check
  ```

- [ ] Run lint and build:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make lint'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
  ```

- [ ] Run an explicit Linux/Windows compile check for the cross-platform server packages if the normal build host is macOS:

  ```bash
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && GOOS=linux GOARCH=amd64 go test ./cmd ./pkg/server ./pkg/stream -run "^$" -count=1'
  bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && GOOS=windows GOARCH=amd64 go test ./cmd ./pkg/server ./pkg/stream -run "^$" -count=1'
  ```

  If the already documented unrelated Windows `file_logger` SIGUSR1 compile issue remains unchanged, record the exact file/line/error and baseline reproduction; do not describe the Windows gate as passing.

- [ ] Perform the AGENTS.md refactor audit:

  1. List every changed/deleted function, method, type, import, and module from the complete diff.
  2. Run `rg` for every moved/renamed/deleted symbol across production and tests.
  3. Remove proxy-only forwarding methods, dead imports/helpers/fixtures, and tests that only cover deleted facades.
  4. Recheck package/import boundaries and confirm no second provider/publication/runtime owner exists.
  5. Run `git diff --check` again.

- [ ] Request one independent read-only merge review over the complete Task 9 integration diff. The review brief must explicitly ask for composite bundle atomicity, stale acquisition races, finalize nonblocking behavior, recovery-only-Published proof, ack-only provider state, shutdown safety, global Store absence, public API drift, and missing test migrations.

- [ ] Repair each confirmed Critical/Important finding only after adding a focused regression test that fails on the reviewed diff. Rerun the finding's focused/race gate and every final gate affected by the repair.

- [ ] The integration owner, not a lane worker, records final evidence and creates the reviewed integration checkpoint. Before any commit, verify only Task 9 files are staged:

  ```bash
  git status --short
  git diff --cached --stat
  git diff --cached --check
  ```

## Parallel and Dependency Boundaries

```text
merged T9-1 resolver ----┐
merged T9-2 route -------┤
merged T9-3 HTTP leases -┤
merged T9-4 stream ------┤
merged T9-5 observers ---┘
          |
          v
T9-6 engine Tasks 1-4 (single owner; no parallel engine edits)
          |
          +----------------------+
          v                      v
T9-7 provider Tasks 5-6     T9-8 bootstrap Tasks 7-8
(etcd/config ownership)     (cmd/server ownership)
          +-----------+----------+
                      v
T9-9 deletion Task 9 (single destructive integration owner)
                      |
                      v
T9-10 gates/review Task 10
```

- Provider and bootstrap lanes may run in parallel only after the engine signatures/tests are merged. Provider owns `pkg/etcd`, `pkg/config/standalone.go`, and `pkg/generation/provider.go`; bootstrap owns `cmd` and `pkg/server/server.go`. If both require the same server constructor/provider wiring hunk, bootstrap owns the final integration edit.
- Do not start legacy deletion until engine, providers, bootstrap, listener startup, and shutdown are green together. Deletion is one reviewed unit because leaving either path creates split authority.
- Do not merge a dependent lane from an unreviewed/unmerged predecessor. If a frozen interface changes, stop, amend the joint contract and this plan, merge the contract checkpoint, and restart dependent work from that SHA.
- No lane may modify another lane's files to make its own tests compile. Record the exact dependency and wait for the integration checkpoint.

## Completion Criteria

Task 9 is complete only when all of the following are true:

- recovery is installed from verified `Published` artifacts before listeners/provider;
- zero-domain replay works without compiler/runtime mutation;
- HTTP-only and stream-only updates preserve the untouched exact owner;
- tentative rollback restores the whole predecessor and tentative leases drain safely;
- finalize performs no close/wait/drain itself, returns even when the retirement worker is blocked, and retirement closes once asynchronously;
- provider state and readiness advance only from a matching acknowledgement;
- offline recovered traffic can serve while provider readiness remains degraded;
- shutdown stops/join provider, listeners and leases before engine/resolver/journal;
- Store/Event/Builder/ClusterRegistry/Replace/Reload paths and test-only facades are absent;
- the named isolation, focused, race, lint, build, absence, diff, dead-code, and independent review gates have fresh evidence with every exception disclosed.
