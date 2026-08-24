# Immutable Task 6 CP5 PreparedGeneration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task by task. Only the integration owner may commit.

**Goal:** Complete Task 6's atomic worker-local generation owner. Candidate and recovery preparation must acquire the exact registered attempt, generation task registry, consumer bindings, metadata view, resource registry relationship, and cleanup authority, then transfer those owners to one closeable `PreparedGeneration` or release every acquired owner exactly once.

**Architecture:** `WorkerCompilerFactory` wraps the existing pure `Compiler` and attempt-registration boundary. It creates one generation-local task registry, prepares consumers before metadata, installs a compiler-private effective-binding materializer bound to the exact attempt and cleanup owner, and returns a defensive `PreparedGeneration`. CP5 does **not** decide effective HTTP or stream bindings and does **not** publish a binding lookup API. Immutable Task 7 and Task 8 compute route/service/plugin-config/global/404/consumer/stream effective binding specs and pass those exact specs to the CP5 private materializer, which constructs and leases the instances under the already prepared generation.

**Accepted X1 input:** consume composite-child ownership from exact checkpoint
`b31d2a6d59c3e4f39b375b4def5706d0867a36d2`; do not reconstruct its contract
from the earlier shared-seam parent or either leaf worktree commit.

**Tech Stack:** Go 1.26, `pkg/compiler`, `generation.PublicationSet`, `secret.Materializer`, `secret.AttemptRegistration`, `runtime.TaskRegistry`, `runtime.ResourceRegistry`, `runtime.ConsumerBindings`, `runtime.MetadataView`, `plugin.FactoryInstance`, `plugin.BindAttemptResolvedPlugin`, focused unit/race tests, golangci-lint, and build smoke verification.

**Spec:** `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md` Task 6, amended by `docs/superpowers/plans/2026-08-24-immutable-task6-execution-brief.md` C6.5, `docs/superpowers/plans/2026-08-24-immutable-task6-c6.4-plugin-runtime.md` Task 10, `docs/superpowers/plans/2026-08-24-immutable-task6-total-plan.md` Task 8, and the accepted current-code audit recorded in this plan.

## Global Constraints

- Execute only from the accepted Task 6 integration HEAD containing every reviewed S1, S2, S3, A1, M1, M2, and X1 checkpoint. This is a dynamic baseline; do not branch from `40c04a26`, `e5b6a73e`, `9ebcd2b5`, or any earlier fixed commit.
- Run every Go command from the active worktree with `source .envrc && export GOFLAGS=-mod=readonly`.
- CP5 owns only `pkg/compiler/**`. Do not add `plugin.CloneBinding`, a public binding view, or a raw binding accessor. Modify `pkg/runtime/**` only if one focused RED test proves the accepted registry contract is insufficient.
- The integration owner is the only actor allowed to commit. Implementation workers may edit and run focused verification, but may not commit, push, open a PR, merge, or mutate `master`.
- Candidate preparation performs pure publication refinement exactly once and registers that exact refined set. Recovery accepts only `generation.RevisionSet` plus verified committed publications; it never accepts `RecoveryState.Desired` and never re-runs disposition.
- The CP5 construction order is exact: final publication set; attempt registration and generation capability; task registry; consumer bindings; metadata view; generation-bound private materializer; `PreparedGeneration` transfer.
- Plugin instances and resource leases are acquired only when Task 7 or Task 8 supplies effective binding specs. Each acquisition becomes generation-owned immediately. CP5 acceptance must not claim that all runtime bindings already exist when `PrepareGeneration` returns.
- Every closeable owner enters one cleanup ledger immediately. Generation tasks quiesce before any plugin/resource release; plugin/resource owners unwind in reverse acquisition order; consumer bindings and attempt registration close afterward.
- `Close` and exact `DiscardPrepared` are concurrent, idempotent, and replay the first cleanup result. A mismatched discard returns `ErrPreparedSetMismatch` and leaves the candidate live.
- Errors, cleanup traces, and accessors must not expose raw secret references, decrypted plaintext, consumer credential keys, resolver/broker handles, Store handles, raw keyring material, mutable plugin configuration, `plugin.Plugin`, `plugin.Binding`, `plugin.FactoryInstance`, resource leases, task registries, or closeable consumer bindings.
- Task 7 HTTP snapshots and Task 8 stream snapshots stay deferred. CP5 adds no `HTTPSnapshot`, `StreamSnapshot`, route merge, global/404 construction, consumer composition, `_meta` wrapper, stream merge, cluster acquisition, router, listener, or activation code.
- Do not add a second production activation path. CP5 defines legal candidate/recovery preparation but does not fabricate or source a production `generation.ApplyTicket`. Task 9 remains the production provider/journal/coordinator/activation owner.

## Current-Code Audit Decision

The earlier CP5 draft assumed that `(domain, resource, factory)` identified one exact prepared binding. Current source disproves that assumption:

- HTTP winner selection is route > plugin-config > service, while global and 404 owners are separate partitions.
- consumer and consumer-group composition changes the effective request binding set;
- route/resource-derived context can change an instance even when source config is equal;
- `_meta.priority`, `_meta.filter`, and `_meta.error_response` affect binding execution and identity;
- stream route merging has a different owner and selection model;
- X1 composite children remain owned by their outer preparer and cannot be independently relabeled.

Therefore CP5 must not expose `PreparedBindingView`, `BindingView`, or `PluginBinding(domain, resource, factory)`. It also must not enumerate `PreparationAttempt.Occurrences(capability.SecretPluginConfig)` and call that enumeration “all runtime instances.” Those occurrences are the current C6.4 scoped-secret authority for admitted plugin-config sources; they are not the effective HTTP/stream binding inventory.

The reconciliation is deliberate:

```text
CP5
  owns attempt + tasks + consumers + metadata + resource/cleanup authority
  provides one compiler-private effective-binding materializer

Task 7 / Task 8
  compute exact effective specs from final candidates and prepared dependencies
  call the CP5 private materializer
  own route/global/404/consumer/_meta/stream composition and snapshot construction

Task 9
  owns legal production activation and retirement
```

The older “plugin/resource leases before `PreparedGeneration` return” line in the total plan and C6.4 Task 10 is superseded for CP5 by this audited split: CP5 creates the lease authority before return, while Task 7/8 cause concrete lease acquisition after they know the effective specs. The Task 6 acceptance ledger must carry this correction; it must not silently preserve the old completeness claim.

## Accepted Baseline Contracts

CP5 reuses these current contracts exactly:

```go
func New(*capability.Manifest) (*Compiler, error)

type PreparationAttempt struct { /* private authority and capability */ }
func (a PreparationAttempt) Generation() uint64
func (a PreparationAttempt) AttemptID() secret.AttemptID
func (a PreparationAttempt) Candidate(generation.Domain) (generation.PublicationCandidate, bool)
func (a PreparationAttempt) Occurrences(capability.SecretDeclarationSource) []FactoryOccurrence
func (a PreparationAttempt) PrepareScopedPluginSecrets(
	context.Context,
	FactoryOccurrence,
	plugin.FactoryInstance,
) error

type MetadataPreparer interface {
	PrepareMetadata(context.Context, PreparationAttempt) (runtime.MetadataView, error)
}

func runtime.NewTaskRegistry(context.Context, func(runtime.TaskFailure)) *runtime.TaskRegistry
func (*runtime.TaskRegistry) Stop(context.Context) ([]runtime.TaskResidual, error)
func runtime.NewResourceRegistry() *runtime.ResourceRegistry
func runtime.Acquire[T any](
	context.Context,
	*runtime.ResourceRegistry,
	runtime.ResourceKey,
	runtime.ResourceFactory[T],
) (*runtime.ResourceLease[T], error)
func (*runtime.ResourceLease[T]) Release(context.Context) error

func plugin.NewFactoryInstance(string, base.Dependencies) (plugin.FactoryInstance, error)
func plugin.DescriptorForFactory(*capability.Manifest, string) (plugin.Descriptor, error)
func plugin.ResolveDescriptor(plugin.Descriptor, plugin.Plugin) (plugin.Descriptor, error)
func plugin.BindAttemptResolvedPlugin(
	secret.AttemptID,
	plugin.Descriptor,
	plugin.Plugin,
	plugin.Scope,
	plugin.ResourceProvenance,
	plugin.InstanceIdentityInput,
) (plugin.Binding, error)
```

The accepted A1 consumer preparer currently accepts a `runtime.MetadataView` but ignores it. That stale parameter contradicts the frozen order, so CP5 removes it rather than passing a fabricated empty metadata value:

```go
type ConsumerPreparer interface {
	PrepareConsumers(
		context.Context,
		PreparationAttempt,
	) (*runtime.ConsumerBindings, error)
}
```

Delete the current `PluginPreparer` / `PreparedPlugins` hook. It asks CP5 to prepare plugins before Task 7/8 have computed the effective specs. Replace it with the package-private primitive below; do not export this type or any method using it.

## Exact CP5 Interfaces

The public CP5 surface is:

```go
var ErrWorkerCompilerFactoryClosed = errors.New("worker compiler factory is closed")
var ErrPreparedSetMismatch = errors.New("prepared publication set mismatch")

func NewWorkerCompilerFactory(
	manifest *capability.Manifest,
	effective *config.EffectiveConfig,
	materializer secret.Materializer,
) (*WorkerCompilerFactory, error)

func (f *WorkerCompilerFactory) PrepareGeneration(
	ctx context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	previous map[generation.Domain]generation.PublishedGeneration,
	onFailure func(runtime.TaskFailure),
) (*PreparedGeneration, error)

func (f *WorkerCompilerFactory) PrepareRecovery(
	ctx context.Context,
	revisions generation.RevisionSet,
	committed map[generation.Domain]generation.PublishedGeneration,
	onFailure func(runtime.TaskFailure),
) (*PreparedGeneration, error)

func (f *WorkerCompilerFactory) Close(context.Context) error

func (p *PreparedGeneration) PublicationSet() generation.PublicationSet
func (p *PreparedGeneration) MetadataView() runtime.MetadataView
func (p *PreparedGeneration) ConsumerLookup() base.ConsumerLookup
func (p *PreparedGeneration) DiscardPrepared(
	context.Context,
	generation.PublicationSet,
) error
func (p *PreparedGeneration) Close(context.Context) error
```

`PublicationSet` returns a deep defensive clone. `MetadataView` is the immutable decode-only value. `ConsumerLookup` returns `consumerLookupView`, never `*runtime.ConsumerBindings`. After close, `MetadataView` is zero and `ConsumerLookup` is inert; a previously returned consumer view becomes inert because its owned `ConsumerBindings` is closed. No public method returns attempt capability, registration, tasks, resources, leases, plugin instances, or bindings.

The compiler-private Task 7/8 bridge is:

```go
type effectiveBindingSourceKind uint8

const (
	effectiveBindingPluginConfig effectiveBindingSourceKind = iota
	effectiveBindingPreparedConsumer
	effectiveBindingSystem
)

type effectiveBindingSource struct {
	kind       effectiveBindingSourceKind
	resource   generation.ResourceKey
	source     capability.SecretDeclarationSource
	occurrence FactoryOccurrence
}

type effectiveBindingContextKind uint8

const (
	effectiveBindingContextNone effectiveBindingContextKind = iota
	effectiveBindingContextHTTP
	effectiveBindingContextStream
)

type effectiveBindingResourceContext struct {
	kind        effectiveBindingContextKind
	route       resource.Route
	service     resource.Service
	streamRoute resource.StreamRoute
}

type effectiveBindingSpec struct {
	domain          generation.Domain
	executionOwner  generation.ResourceKey
	source          effectiveBindingSource
	factory         string
	config          resource.PluginConfig
	scope           plugin.Scope
	provenance      plugin.ResourceProvenance
	resourceContext effectiveBindingResourceContext
	filterIdentity  any
	errorIdentity   any
}

func (p *PreparedGeneration) materializeEffectiveBindings(
	ctx context.Context,
	specs []effectiveBindingSpec,
) ([]plugin.Binding, error)
```

All names are package-private. The return value is compiler-internal input to Task 7/8 snapshot compilation, not an accessor and not a view retained by external packages. `effectiveBindingSpec` is a post-selection contract, not a raw-resource occurrence: Task 7/8 must already have chosen precedence, owner, context, consumer composition, and stream merge. Task 7/8 also own `_meta` parsing and wrappers; they pass filter/error identity so CP5 constructs the correct `plugin.InstanceKey`, then apply their exact wrappers to the returned internal binding.

The source kinds are intentionally distinct:

- `effectiveBindingPluginConfig` must resolve an exact attempt-owned `FactoryOccurrence` with matching domain, source resource, `SecretPluginConfig`, and factory before scoped secret preparation.
- `effectiveBindingPreparedConsumer` must be derived from the generation's defensive A1 consumer/group data. Its secrets were already materialized by A1; CP5 must not resolve them again.
- `effectiveBindingSystem` is allowed only for compiler-derived system factories that the accepted manifest proves have no secret declaration. It cannot be used to bypass an ordinary occurrence.

`effectiveBindingSource.occurrence` is mandatory only for plugin-config sources;
the private attempt authority prevents a foreign/relabelled occurrence from
being accepted merely because its visible fields match. Resource context is a
caller-supplied, defensively owned discriminated value: HTTP context is valid
only for the HTTP domain, stream context only for the stream domain, and none
only for bindings that require no route/service/stream context. It participates
in canonical instance identity without pointer or function identity. Task 4
calls only the existing HTTP `SetResourceContext(resource.Route,
resource.Service)` capability; it must not invent a stream setter or callback.

The primitive validates nonzero attempt identity, valid domain and execution owner, exact factory identity, allowed scope, provenance/source agreement, defensive config ownership, source authority, descriptor compatibility, and resource-key identity before constructing anything. It rejects duplicate effective specs within one call and never infers missing route/global/consumer/stream semantics.

## File Structure

- Modify `pkg/compiler/hooks.go` — remove the stale consumer metadata argument and delete `PluginPreparer` / `PreparedPlugins`.
- Modify `pkg/compiler/factory.go` — make `attemptFactory` registration-only and retain a defensive publication set plus exact `PreparationAttempt`; remove metadata/consumer/plugin ownership from `registeredAttempt`.
- Modify `pkg/compiler/factory_test.go` — preserve candidate/recovery registration identity, typed-nil, partial-registration cleanup, and exact final-set tests.
- Modify `pkg/compiler/consumer_preparer.go` and `pkg/compiler/consumer_preparer_test.go` — remove the unused metadata parameter and prove behavior remains unchanged.
- Create `pkg/compiler/cleanup.go` and `pkg/compiler/cleanup_test.go` — immediate-ownership cleanup ledger with task-quiesce and reverse-release phases.
- Create `pkg/compiler/prepared_generation.go` and `pkg/compiler/prepared_generation_test.go` — defensive public owner, materialization gate, exact discard, close, and factory-detach callback.
- Create `pkg/compiler/effective_binding_materializer.go` and `pkg/compiler/effective_binding_materializer_test.go` — compiler-private spec validation, plugin lifecycle, scoped-secret use, attempt binding, resource lease acquisition, and third-plugin failure cleanup.
- Create `pkg/compiler/worker_factory.go` and `pkg/compiler/worker_factory_test.go` — public constructor, candidate/recovery transactions, shared resource registry, live generation tracking, and close races.
- Modify `pkg/compiler/types.go` — only stable CP5 sentinel errors shared by these files.

No `pkg/plugin` production change is required because bindings never cross a public CP5 boundary. No `pkg/runtime` production change is expected; `TaskRegistry.Stop`, `ResourceLease.Release`, and `ResourceRegistry.Close` already provide the required idempotent primitives.

## Ownership and Teardown Model

Use one cleanup ledger with two phases:

```go
type cleanupPhase uint8

const (
	cleanupQuiesce cleanupPhase = iota
	cleanupRelease
)

type cleanupStep struct {
	name string
	run  func(context.Context) error
}

type cleanupStack struct {
	mu        sync.Mutex
	quiescers []cleanupStep
	releases  []cleanupStep
	closeOnce sync.Once
	closeErr  error
}

func (s *cleanupStack) Own(
	phase cleanupPhase,
	name string,
	run func(context.Context) error,
) error
func (s *cleanupStack) Close(context.Context) error
```

`Own` rejects a blank name, nil callback, or ownership after close. `Close` snapshots and seals the ledger, runs quiescers in reverse registration order, then releases in reverse registration order, joins every error, and caches the joined result. Registration is the first release owner; task registry is the first quiescer; consumer bindings follow registration; future materialized plugin leases append as release owners.

`PreparedGeneration.materializeEffectiveBindings` and `Close` share one private gate. Materialization is serialized per generation. A call admitted before close either attaches each acquired lease immediately and returns all bindings, or marks the generation failed and triggers the same terminal cleanup after releasing the gate. Close waits for an admitted materialization call; a call beginning after close fails without construction. This prevents a lease from being appended after cleanup has passed it.

Observable terminal order after any materialization is:

```text
task registry Stop and wait
-> plugin/resource lease N Release
-> ...
-> plugin/resource lease 1 Release
-> consumer bindings Close
-> attempt registration Close/revoke
-> clear private attempt/materializer/metadata/consumer references
-> detach from WorkerCompilerFactory live set
```

The factory-level `ResourceRegistry` outlives individual leases. `WorkerCompilerFactory.Close` first closes every tracked generation, then closes the registry.

---

### Task 1: Freeze Hook Dependencies and Registration-Only Attempt Factory

**Files:** `pkg/compiler/hooks.go`, `pkg/compiler/factory.go`, `pkg/compiler/factory_test.go`, `pkg/compiler/consumer_preparer.go`, `pkg/compiler/consumer_preparer_test.go`

**Produces:** exact `ConsumerPreparer` signature; no `PluginPreparer`; a registration-only `registeredAttempt` containing the exact attempt, defensive publication set, and registration owner.

- [x] **Step 1: Write RED compile and ownership tests**

Add compile assertions that call consumers without metadata and assert no `PluginPreparer`/`PreparedPlugins` declaration remains. Update recording hooks to trace only registration and consumer calls. Add tests proving `registeredAttempt.Close` closes only the registration.

```go
var _ ConsumerPreparer = (*consumerBindingPreparer)(nil)

func TestConsumerPreparerHasNoMetadataDependency(t *testing.T) {
	preparer, err := newConsumerBindingPreparer(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = preparer.PrepareConsumers(context.Background(), PreparationAttempt{})
}
```

- [x] **Step 2: Run focused RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run 'TestConsumerPreparerHasNoMetadataDependency|TestRegisteredAttempt' -count=1
```

Expected: compile failure while the old method and ownership shape remain.

- [x] **Step 3: Refactor the hook and attempt owner**

Change `ConsumerPreparer` and the A1 implementation to the two-argument form. Delete `PluginPreparer` and `PreparedPlugins`. Reduce the stable attempt types to:

```go
type attemptFactory struct {
	compiler     *Compiler
	materializer secret.Materializer
}

type registeredAttempt struct {
	attempt      PreparationAttempt
	publication  generation.PublicationSet
	registration secret.AttemptRegistration
	closeOnce    sync.Once
	closeErr     error
}
```

Candidate still performs pure preparation, final occurrence enumeration, final-set validation, exact registration, registration identity checking, generation capability creation, and `PreparationAttempt` construction in that order. Recovery still validates committed publications, constructs candidates only from verified values, registers them with `RegisterRecovery`, and never calls candidate disposition. Store `clonePublicationSetForPreparation(set)` before return and clone candidate maps before placing them in both owners.

- [x] **Step 4: Preserve all failure semantics**

- close a nonnil registration returned with error and join the errors;
- reject typed-nil registration;
- reject candidate/recovery attempt-ID mismatch and close the forged registration;
- close registration if generation capability or attempt construction fails;
- make concurrent `registeredAttempt.Close` execute once and replay its first result.

- [x] **Step 5: Run GREEN**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run 'TestAttemptFactory|TestRegisteredAttempt|TestConsumerBindingPreparer|TestPrepareRecoveryAttempt' -count=1
go test -race ./pkg/compiler -run 'TestRegisteredAttemptClose|TestConsumerBindingPreparerKeepsOverlappingGenerationsIndependent' -count=1
```

Expected: PASS with exact registration identity and no plugin preparation during attempt registration.

---

### Task 2: Implement the Immediate-Ownership Cleanup Ledger

**Files:** create `pkg/compiler/cleanup.go`, `pkg/compiler/cleanup_test.go`

- [x] **Step 1: Write RED tests**

Add `TestCleanupStackQuiescesThenReleasesInReverseOrder`, `TestCleanupStackConcurrentCloseRunsEachStepOnce`, `TestCleanupStackReplaysJoinedErrors`, and `TestCleanupStackRejectsLateOwnership`. Own registration, tasks, consumers, plugin-1, plugin-2 and expect `tasks, plugin-2, plugin-1, consumers, registration`.

- [x] **Step 2: Run RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestCleanupStack' -count=1
```

- [x] **Step 3: Implement minimally**

Validate inputs under lock, append immediately, seal before execution, normalize nil context, run both phases despite errors, join errors, and cache the first result. Do not add removal, reordering, retry, or public introspection APIs.

- [x] **Step 4: Run GREEN and race**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestCleanupStack' -count=1
go test -race ./pkg/compiler -run '^TestCleanupStack' -count=1
```

---

### Task 3: Define Defensive `PreparedGeneration` Without a Binding View

**Files:** create `pkg/compiler/prepared_generation.go`, `pkg/compiler/prepared_generation_test.go`; modify `pkg/compiler/types.go`

- [x] **Step 1: Write RED defensive and mismatch tests**

Add:

```go
func TestPreparedGenerationPublicationSetIsDefensive(t *testing.T)
func TestPreparedGenerationMetadataViewIsDefensive(t *testing.T)
func TestPreparedGenerationConsumerLookupExposesNoClose(t *testing.T)
func TestPreparedGenerationAccessorsAreInertAfterClose(t *testing.T)
func TestPreparedGenerationExactDiscardClosesOnce(t *testing.T)
func TestPreparedGenerationMismatchedDiscardLeavesOwnersLive(t *testing.T)
func TestPreparedGenerationConcurrentExactDiscardAndCloseRunsCleanupOnce(t *testing.T)
func TestPreparedGenerationPublicAPIExposesNoRuntimeHandles(t *testing.T)
```

Mutate returned snapshot bytes, closure, decisions, and map, then read again. Change every publication identity field one at a time for mismatch coverage.

- [x] **Step 2: Add the public API AST guard**

Parse `prepared_generation.go`. Reject exported fields/methods using registration, generation capability, materializer, task/resource registry, lease, closeable consumer bindings, plugin instance/binding/factory instance, Store, or encryption resolver. Reject `PreparedBindingView`, `BindingView`, `PluginBinding`, `Bindings`, `Plugins`, `Leases`, `Resources`, and `Tasks`. Allow only `PublicationSet`, `MetadataView`, `ConsumerLookup`, `DiscardPrepared`, and `Close`.

- [x] **Step 3: Run RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestPreparedGeneration' -count=1
```

- [x] **Step 4: Implement private state and defensive access**

Keep publication, attempt, metadata, consumers, safe lookup, tasks, effective config, manifest, shared registry, cleanup ledger, materialization gate/state, and detach callback private. Accessors return defensive/inert values after close. `DiscardPrepared` compares the complete publication identity; mismatch preserves ownership, exact match delegates to `Close`. Close marks materialization terminal, runs cleanup, clears private references, and detaches once.

- [x] **Step 5: Run GREEN and race**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestPreparedGeneration' -count=1
go test -race ./pkg/compiler -run 'TestPreparedGenerationConcurrent|TestPreparedGenerationAccessorsAreInertAfterClose' -count=1
```

---

### Task 4: Implement the Compiler-Private Effective-Binding Materializer

**Files:** create `pkg/compiler/effective_binding_materializer.go`, `pkg/compiler/effective_binding_materializer_test.go`; modify prepared-generation files

**Produces:** compiler-internal bindings for future exact Task 7/8 specs; no inventory and no public lookup.

- [ ] **Step 1: Write RED API and validation tests**

Add:

```go
func TestEffectiveBindingMaterializerRequiresTask7OrTask8Specs(t *testing.T)
func TestEffectiveBindingMaterializerRejectsRawOccurrenceEnumeration(t *testing.T)
func TestEffectiveBindingMaterializerRejectsSourceAuthorityMismatch(t *testing.T)
func TestEffectiveBindingMaterializerRejectsCrossAttemptOrRelabeledSpec(t *testing.T)
func TestEffectiveBindingMaterializerRejectsDuplicateEffectiveSpec(t *testing.T)
func TestEffectiveBindingMaterializerPreparedConsumerDoesNotRematerializeSecrets(t *testing.T)
func TestEffectiveBindingMaterializerSystemSourceRequiresNoSecretDeclaration(t *testing.T)
func TestEffectiveBindingMaterializerRejectsAfterGenerationClose(t *testing.T)
func TestEffectiveBindingMaterializerCloseRaceIsLinearized(t *testing.T)
```

The raw-occurrence test creates a candidate containing route, service, plugin-config, global, consumer, and stream resources, then supplies zero specs. Assert zero plugin construction and leases. Enumerating `attempt.Occurrences(SecretPluginConfig)` alone must never trigger construction.

- [ ] **Step 2: Write RED lifecycle and third-plugin tests**

Record this order:

```text
validate exact effective spec/source authority
-> create per-effective-binding CompositeChildPreparer from exact attempt/scope/provenance
-> inject CompositeChildren into outer Dependencies
-> NewFactoryInstance
-> Init
-> decode caller-owned config
-> scoped secret preparation only for exact plugin-config occurrence
-> apply caller-supplied route/service or stream resource context
-> PostInit
-> StartObservingWithTasks when implemented
-> descriptor resolve
-> BindAttemptResolvedPlugin
-> Acquire lease
-> own release immediately
-> return internal binding
```

Add exact dependency, third-plugin failure, reverse release,
same-config/different-context, candidate-vs-recovery attempt, composite-child
injection-before-outer-construction, and observer-start failure tests. The third
plugin fails after construction but before lease transfer. Expect tasks to
stop, partial plugin 3 and any child owners to stop, leases 2 and 1 to release
in reverse, consumers to close, and registration to close, all exactly once.
An observer-start failure must use the same cleanup path and leave no task or
binding. Return no partial bindings.

- [ ] **Step 3: Run RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run 'TestEffectiveBindingMaterializer|TestPrepareGenerationThirdPluginFailure' -count=1
```

- [ ] **Step 4: Validate supplied specs without inference**

Do not scan candidates to invent instances. For plugin config, index `attempt.Occurrences(SecretPluginConfig)` only to authorize the supplied exact domain/resource/factory. For prepared consumer, validate exact defensive A1 consumer/group config and never resolve its secrets again. For system, require `ScopeSystem`, compiler-recognized derived owner, and manifest proof of no secret declaration.

Require valid domain, execution owner, source, factory, scope, provenance, config, manifest compatibility, and no duplicate canonical spec. Do not implement route precedence, global/404, consumer composition, request-context derivation, `_meta`, or stream merge.

- [ ] **Step 5: Construct and own exact instances**

Build the base dependencies with effective config, attempt capability,
metadata, safe consumers, and tasks. Never populate `DataEncryption`. Before
constructing the outer plugin, create the X1
`plugin.NewCompositeChildPreparer` from those dependencies plus the exact
attempt, effective scope, and effective provenance; inject it as
`CompositeChildren`. Use `plugin.NewFactoryInstance`, then `Init`, defensive
decode, source-specific secret handling, caller-supplied resource context,
`PostInit`, optional `StartObservingWithTasks(tasks)`, descriptor resolution,
and `plugin.BindAttemptResolvedPlugin` with config/filter/error identity. A
failure after child or observer acquisition stops the partial outer owner and
lets the generation ledger quiesce tasks before releasing resources.

Derive `runtime.ResourceKey` from the complete attempt-owned instance key plus domain and execution owner, not `(domain, source resource, factory)`. Use a private acquisition slot: it owns constructed plugin `Stop()` until a lease is adopted, then owns `lease.Release`. Append each lease to generation cleanup before the next spec.

If any spec fails, mark the generation terminal under the materialization gate, release the gate, and invoke generation cleanup so tasks quiesce before resource shutdown. Join redacted primary and cleanup errors; return no bindings.

- [ ] **Step 6: Keep effective wrappers deferred**

Returned internal bindings contain base callbacks, resolved descriptor, attempt identity, scope, provenance, and manifest priority. Task 7/8 owns `_meta` wrapper and partition. Filter/error values enter instance identity only. X1 children remain under the outer instance; do not create top-level child bindings.

- [ ] **Step 7: Run GREEN and race**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run 'TestEffectiveBindingMaterializer|TestPrepareGenerationThirdPluginFailure' -count=1
go test -race ./pkg/compiler -run 'TestEffectiveBindingMaterializer|TestPrepareGenerationThirdPluginFailure' -count=1
```

---

### Task 5: Implement Atomic Candidate Preparation and Materializer Transfer

**Files:** create `pkg/compiler/worker_factory.go`, `pkg/compiler/worker_factory_test.go`; modify `pkg/compiler/prepared_generation.go`

- [ ] **Step 1: Write RED transaction tests**

Add tests for frozen order, base owner transfer, zero plugins without specs,
registration/consumer/metadata failure cleanup, and catalog digest mismatch.
Exercise the real production construction path and assert its returned
`base.ConsumerLookup` cannot expose `Close` or `*runtime.ConsumerBindings`, then
becomes inert after generation close; do not rely only on the Task 4 fixture.
The successful trace is:

```text
prepare-final-publication-set
register-attempt-and-capability
create-task-registry
prepare-consumers
prepare-metadata
bind-private-materializer-authority
transfer-prepared-generation
```

- [ ] **Step 2: Run RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestWorkerCompilerFactoryPrepareGeneration' -count=1
```

- [ ] **Step 3: Implement factory and transaction**

The factory privately owns compiler, effective config, attempt factory, metadata/consumer preparers, shared resource registry, close gate, live set, and cached close result. The public constructor validates inputs, compiler/manifest/catalog digest, creates A1/M2 preparers and registry, and accepts no Store/resolver/broker/keyring or hook override.

Hold `gate.RLock` through live insertion. Immediately own registered attempt, task quiescer, and consumer close in sequence; prepare metadata; bind private materializer state without construction; build defensive generation; insert/live-detach; disarm local cleanup. Every failure runs the same ledger with `context.WithoutCancel(ctx)`.

- [ ] **Step 4: Prove transfer and run GREEN/race**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestWorkerCompilerFactoryPrepareGeneration' -count=1
go test -race ./pkg/compiler -run 'TestWorkerCompilerFactoryPrepareGenerationTransfersBaseOwners|TestWorkerCompilerFactoryPrepareGenerationConstructsNoPluginsWithoutSpecs' -count=1
```

---

### Task 6: Implement Recovery Without Desired-State or Disposition Access

**Files:** modify worker factory/tests and `pkg/compiler/factory_test.go`

- [ ] **Step 1: Write RED tests**

Cover committed-only input, independent domain revisions, mismatch before registration, zero plugins without specs, and base owner transfer. Any candidate/disposition call must panic.

- [ ] **Step 2: Implement exact recovery**

Call registration-only `prepareRecoveryAttempt`, then the same task/consumer/metadata/private-materializer helper as candidate. Accept no desired snapshot, apply ticket, recovery state, journal, disposition callback, or Store. Preserve independent artifact revisions/snapshots/closures/decisions.

- [ ] **Step 3: Run GREEN and race**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestWorkerCompilerFactoryPrepareRecovery|^TestPrepareRecoveryAttempt' -count=1
go test -race ./pkg/compiler -run 'TestWorkerCompilerFactoryPrepareRecoveryTransfersBaseOwners|TestEffectiveBindingMaterializerCandidateAndRecoveryAttemptsDoNotAlias' -count=1
```

---

### Task 7: Linearize Factory Close, Generation Close, and Materialization

**Files:** modify worker factory, prepared generation, materializer tests

- [ ] **Step 1: Write RED race tests**

Cover reject-after-close, close all live generations before registry, blocked preparation, blocked second-lease materialization, generation close race, and cached concurrent factory close. In every allowed ordering, no task/plugin/lease/consumer/registration/registry entry leaks.

- [ ] **Step 2: Implement close linearization**

Factory close marks closed under `gate.Lock`, snapshots live generations, closes them in deterministic private identity order without holding factory locks, then closes shared registry, joins and caches errors. New preparation returns `ErrWorkerCompilerFactoryClosed` without side effects.

Generation close and materialization use one private serialized gate. Materialization owns the final acquired lease before releasing the gate; close seals cleanup only after admitted materialization exits. On materialization failure, mark terminal, unlock, then run close to avoid deadlock.

- [ ] **Step 3: Run GREEN and race**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestWorkerCompilerFactoryClose' -count=1
go test -race ./pkg/compiler -run 'TestWorkerCompilerFactoryCloseRaces|TestWorkerCompilerFactoryConcurrentClose|TestEffectiveBindingMaterializerCloseRace' -count=1
```

---

### Task 8: Run CP5 Integration, Contract Audit, and Independent Review

**Files:** review all changed `pkg/compiler/**`; verify accepted S1/S2/S3/A1/M1/M2/X1; cross-check Task 6 total plan, C6.4 Task 10, original Task 6, and CP6 handoff

- [ ] **Step 1: Run focused unit and race gates**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler ./pkg/runtime ./pkg/plugin \
  -run 'PreparedGeneration|EffectiveBindingMaterializer|PrepareGeneration|PrepareRecovery|Cleanup|Discard|FactoryClose|ScopedSecretMaterialization|FactoryInstance' -count=1
go test -race ./pkg/compiler ./pkg/runtime ./pkg/plugin \
  -run 'PreparedGeneration|EffectiveBindingMaterializer|PrepareGeneration|PrepareRecovery|Cleanup|Discard|FactoryClose|ScopedSecretMaterialization|FactoryInstance' -count=1
```

Record any unrelated baseline failure exactly; never call a narrowed rerun full-package passing.

- [ ] **Step 2: Run lint, generator, build, and diff checks**

```bash
source .envrc
export GOFLAGS=-mod=readonly
golangci-lint run ./pkg/compiler/... ./pkg/runtime/... ./pkg/plugin/...
go run ./cmd/capability-gen -check
make build
git diff --check
```

- [ ] **Step 3: Run public API and stale-assumption scans**

```bash
rg -n --glob '*.go' \
  'PrepareConsumers\([^\n]*MetadataView|type PluginPreparer|type PreparedPlugins|registeredAttempt\.(metadata|consumers|plugins)' pkg cmd
rg -n --glob '*.go' --glob '!**/*_test.go' \
  'PreparedBindingView|BindingView\(|PluginBinding\(|\[\]plugin\.Binding|map\[.*\]plugin\.Binding' pkg/compiler
rg -n --glob '*.go' --glob '!**/*_test.go' \
  'func \(.*PreparedGeneration.*\).*(AttemptRegistration|GenerationCapability|Materializer|TaskRegistry|ResourceRegistry|ConsumerBindings|ResourceLease|plugin\.Plugin|plugin\.Binding|FactoryInstance|store\.Store|data_encryption)' pkg/compiler
rg -n --glob '*.go' \
  'Occurrences\(capability\.SecretPluginConfig\).*all|all.*Occurrences\(capability\.SecretPluginConfig\)|inventory-all-runtime-plugins' \
  pkg/compiler docs/superpowers/plans/2026-08-24-immutable-task6-cp5-prepared-generation.md
```

The package-private `materializeEffectiveBindings` return is the only allowed production `[]plugin.Binding` in CP5. Confirm no CP5 code selects effective HTTP/stream sets.

- [ ] **Step 4: Synchronize downstream contracts**

Record: no public binding view; CP5 return does not mean all bindings exist; Task 7/8 compute exact specs and may not directly call plugin construction/lifecycle/resource acquisition; CP6 consumes only the base bundle and cannot forge an ApplyTicket; `SecretPluginConfig` occurrence is authority, not inventory. If another plan retains the rejected contract, update it before implementation or record explicit accepted supersession. Never implement both.

- [ ] **Step 5: Request independent review**

Ask one read-only reviewer to verify exact candidate/recovery identity, zero speculative bindings, immediate ownership, task-before-resource reverse teardown, third-plugin failure, source authority, no public binding/raw handles, discard preservation, close/materialize/factory races, redaction, Task 7/8 deferral, and Task 9 activation ownership. Independently validate every finding and rerun affected gates.

- [ ] **Step 6: Commit as integration owner only**

```bash
git status --short
git diff --stat
git diff -- pkg/compiler pkg/runtime
git diff --check
git add pkg/compiler
git add pkg/runtime  # only if independently justified
git commit -m "feat(compiler): own prepared plugin generations"
```

Do not stage unrelated plans/files. Do not push, open a PR, merge to `master`, or start Task 7/8 from a pre-commit worktree.

## Acceptance Ledger

| Boundary | Required evidence |
| --- | --- |
| Dynamic baseline | HEAD contains reviewed S1/S2/S3/A1/M1/M2/X1 integration; no stale fixed SHA |
| Candidate identity | Pure final set runs once, validates, registers exactly, and becomes the defensive publication identity |
| Recovery identity | Only verified committed publications plus `RevisionSet`; no desired snapshot or disposition |
| Base construction order | final set → registration/capability → tasks → consumers → metadata → private materializer authority → transfer |
| No speculative inventory | zero specs means zero plugin construction/leases; `SecretPluginConfig` occurrence count is not binding count |
| Effective ownership | only Task 7/8 computes exact route/global/404/consumer/stream specs; CP5 only validates/materializes supplied specs |
| Source authority | plugin config matches exact attempt occurrence; consumer comes from A1 view; system is manifest-proven no-secret |
| Instance identity | attempt, domain, execution owner, scope, provenance, config, filter, and error identity prevent false sharing |
| Immediate ownership | registration, tasks, consumers, partial plugin, and each lease enter cleanup before next fallible stage |
| Partial failure | third failure stops tasks, partial plugin 3, leases 2 then 1, consumers, registration exactly once |
| Public access | defensive publication/metadata/consumer views plus discard/close only; no binding view or raw handles |
| Closed behavior | new access/materialization inert or rejected; prior consumer lookup becomes inert |
| Discard | exact closes once; changed identity returns mismatch and preserves owners |
| Factory close | no generation/lease escapes races; generations close before shared registry |
| Attempt isolation | equal specs in different candidate/recovery attempts never share attempt-owned bindings |
| Redaction | no raw reference, plaintext, credential key, provider secret, or mutable config in errors/traces/accessors |
| Activation gap | CP5/CP6 do not fabricate `ApplyTicket`; Task 9 owns provider/journal/coordinator |
| Deferred scope | no HTTP/stream snapshot, merge, `_meta` wrapper, cluster/router/listener, or activation in CP5 |
| Verification | focused unit/race, lint, generator, build, diff, AST/rg, independent review recorded honestly |
| Delivery | one integration-owner local checkpoint commit; no worker commit/push/PR/release/master mutation |

## Explicit Deferrals

- Task 7 owns HTTP precedence, global/404 partitions, route/resource context, consumer/group composition, `_meta` wrappers, cluster leases, handlers, and `HTTPSnapshot`. It passes final internal specs to CP5 materialization and retains the generation for snapshot lifetime.
- Task 8 owns stream merge, protocol selection, stream context, router/handler construction, and `StreamSnapshot`. It passes final internal specs to the same materializer and does not construct plugins directly.
- Task 7/8 must coordinate snapshot construction/attachment with the generation materialization gate so close cannot retire bindings mid-build. Concrete snapshot fields and attachment API are intentionally not invented in CP5.
- Task 9 owns journal stage/activate/rollback/finalize and snapshot swaps.
- CP6 only audits compatibility and removes proven zero-caller leaves. It cannot use a rejected binding lookup, retain a base bundle in production, rebuild effective inventory, or create a parallel activation engine.

## Self-Review Record

- **Spec coverage:** candidate/recovery, frozen base order, cleanup, private materializer, third-plugin failure, close/discard/materialization races, mismatch preservation, factory close, defensive views, verification, review, and Task 7/8/9 deferrals each have an owner.
- **Audit correction:** all public `PreparedBindingView`/`PluginBinding` and CP5 “complete runtime inventory” claims are removed. `SecretPluginConfig` is only scoped-secret source authority.
- **Type consistency:** public prepare methods return `*PreparedGeneration`; public access is limited to publication, metadata, consumer lookup, discard, and close; only package-private materialization accepts specs and returns bindings.
- **Ownership consistency:** CP5 transfers base owners first; Task 7/8 materialization serializes with close and appends each lease immediately to the same ledger.
- **Scope consistency:** precedence, global/404, consumer composition, context, `_meta`, stream merge, snapshots, legal tickets, and activation remain outside CP5.
