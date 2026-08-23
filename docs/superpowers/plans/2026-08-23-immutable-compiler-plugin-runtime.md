# Immutable Compiler and Owned Plugin Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace mutable `route.Builder` construction with an explicit immutable generation compiler whose plugin instances, shared resources, goroutines, panic boundaries, HTTP handler and stream router have deterministic identities and owned lifecycles.

**Architecture:** `pkg/compiler` consumes one durable desired snapshot plus the previously published domain snapshots and executes normalize, validate, dependency-resolution, secret-materialization, resource-preparation, HTTP/stream compilation and probe phases without touching live runtime state. `pkg/runtime` owns digest-keyed resource leases and named task registries, while `pkg/plugin` resolves phase, priority, scope and instance identity from the central capability manifest. `server.GenerationEngine` retains prepared generations until the journal transaction activates them, then atomically swaps immutable snapshots; the old `route.Builder`, mutable stream reload path and proxy-only lifecycle facades are deleted in the same production cutover.

**Corrected execution boundary:** This plan may start after Durable Journal Tasks 1–8, not after the whole journal plan. Task 1 is split, Task 3's compiler integration is postponed, Task 8's destructive reload removal is postponed, and Task 9 is merged with Durable Task 9 exactly as specified by `2026-08-24-journal-immutable-cutover-reorder.md`.

**Task 5 execution amendment:** Use `2026-08-24-immutable-task5-execution-brief.md`. It adds the canonical resource-taxonomy prerequisite, covers all managed kinds, keeps Task 5 side-effect free, and moves `WorkerCompilerFactory` to Task 6 and cluster acquisition to Task 7.

**Tech Stack:** Go 1.26, standard-library `context`, `crypto/sha256`, `encoding/json`, `net/http`, `sync`, existing `go-chi/chi`, `pkg/capability`, `pkg/config`, `pkg/data_encryption`, `pkg/generation`, `pkg/plugin`, `pkg/proxy`, `pkg/resource`, `pkg/stream` and bbolt-backed `generation.Journal`.

**Spec:** `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`

## Global Constraints

- Compatibility target is Apache APISIX 3.17.0 at commit `9ef2ecab67f652d38365049613610ef649bb4ad0`.
- Preserve the APISIX namespace; version Go-native extensions separately.
- Source `.envrc` before every Go or Make command.
- Use impact-scoped tests; do not run `go test ./...`, `go test ./pkg/...`, or `make test` for this plan.
- Run focused race tests for resource, task, publication and request-lifecycle changes and `source .envrc && make build` after the production cutover.
- Add no external dependency; the standard library and existing project packages provide every primitive needed here.
- Consume the exact `capability.Manifest`, `config.EffectiveConfig`, `generation.Journal`, `generation.PublicationEngine`, `generation.PublishedGeneration`, `generation.PublicationSet` and `generation.GenerationArtifact` contracts produced by plans 01–03.
- `generation.Snapshot` is the only compiler input for dynamic resources. Compiler and request paths must never read desired state, bbolt buckets, package-level store getters or mutable provider state.
- Normalize, validate and resolve the entire required domain before secret materialization, client creation, file opening or goroutine start.
- Plugin phase, priority, legal scope and instance scope come from `pkg/capability/manifest.yaml`; runtime code must not maintain a second handwritten phase or priority database.
- Equal effective configuration plus the same APISIX scope reuses state through a digest-keyed lease; different effective identity never shares mutable state.
- Every production goroutine belongs to a request, plugin instance, generation or supervisor. This plan covers request/plugin/generation owners; the supervisor plan supplies the final owner category.
- Recover plugin panics only at explicit plugin callback boundaries. Unknown compiler, generation, registry or snapshot invariant panics remain fatal to the worker after request finalizers run exactly once.
- Do not retain a temporary legacy adapter, `route.Builder` wrapper, mutable stream `Reload`, `ClusterRegistry` facade, old plugin phase registry or proxy-only method after its production cutover.
- Keep the four existing untracked files under `docs/reviews/` outside every implementation commit.

---

## File and Responsibility Map

**Create:**

- `pkg/secret/materializer.go` — scoped, redacted dynamic-secret materialization backed by the immutable encryption service and a managed-reference resolver.
- `pkg/secret/materializer_test.go` — scope, digest, redaction and cancellation tests.
- `pkg/runtime/dependencies.go` — exact `RuntimeDependencies` contract shared by compiler, plugins and later workers.
- `pkg/runtime/resource_registry.go` — concurrent digest-keyed resource acquisition, reference counting, retry and shutdown.
- `pkg/runtime/resource_registry_test.go` — equal/different identity, failed factory, release and race tests.
- `pkg/runtime/task_registry.go` — named plugin/generation tasks, cancellation, panic policy, join and residual reporting.
- `pkg/runtime/task_registry_test.go` — ownership, plugin-panic, core-panic, cancellation and bounded join tests.
- `pkg/runtime/request_tasks.go` — request-owned task group used by subrequests, mirroring and bidirectional bridges.
- `pkg/runtime/request_tasks_test.go` — request cancellation and join tests.
- `pkg/plugin/descriptor.go` — manifest-derived phase, priority, legal scope and lifecycle descriptor.
- `pkg/plugin/instance.go` — canonical effective-config digest and APISIX-scope instance key.
- `pkg/plugin/descriptor_test.go` and `pkg/plugin/instance_test.go` — manifest agreement, scope rejection, priority and identity tests.
- `pkg/compiler/types.go` — immutable compiler inputs, phase values, domain snapshots and `PreparedGeneration`.
- `pkg/compiler/compiler.go` — `Compiler` construction and the ordered phase pipeline.
- `pkg/compiler/normalize.go` — strict raw-resource decoding and canonical normalized model.
- `pkg/compiler/validate.go` — side-effect-free resource and plugin admission.
- `pkg/compiler/closure.go` — HTTP/stream dependency graph, dispositions and published candidate construction.
- `pkg/compiler/materialize.go` — scoped plugin instance acquisition after all pure phases pass.
- `pkg/compiler/http.go` — immutable HTTP snapshot assembly from route compiler output.
- `pkg/compiler/stream.go` — immutable stream router snapshot assembly without listeners.
- `pkg/compiler/probe.go` — bounded side-effect-free/self-contained readiness probes.
- `pkg/compiler/compiler_test.go`, `normalize_test.go`, `closure_test.go`, `materialize_test.go`, `probe_test.go` — ordered-phase, closure, last-good, no-side-effect and cleanup tests.
- `pkg/route/compiler.go` — focused `CompileHTTP` entrypoint and immutable route input/output values.
- `pkg/route/router.go` — URI/host/method registration and dispatch extracted from `builder.go`.
- `pkg/route/plugin_compile.go` — plugin source merge, precompiled consumer bindings and response-plan assembly.
- `pkg/route/upstream_compile.go` — upstream resolution, cluster acquisition and proxy handler compilation.
- `pkg/route/compiler_test.go` — compiler equivalence, immutable consumer binding and resource lease tests.
- `pkg/stream/snapshot.go` — immutable compiled `Router`; no public `Reload` method.
- `pkg/stream/snapshot_test.go` — equal input identity, route isolation and immutable concurrent serving tests.
- `docs/architecture/immutable-compiler-runtime.md` — phase, ownership, identity, panic and cutover contract.

**Modify:**

- `pkg/plugin/base/types.go` — replace raw encryption dependency with scoped `secret.Materializer`; retain configuration and phase callback implementations.
- `pkg/plugin/base/secrets.go` — accept a scope and materializer; reject unowned references before `PostInit`.
- `pkg/plugin/types.go` — reduce the universal plugin contract and declare optional lifecycle/handler interfaces.
- `pkg/plugin/init.go` — construct plugins with immutable dependencies while consuming generated factory facts.
- `pkg/plugin/executor.go`, `request_stage_registry.go`, `capability_registry.go`, `response_executor.go`, `streaming_executor.go`, `log_executor.go` — carry immutable manifest descriptors on bindings and guard every plugin callback.
- Every production plugin file currently calling `DataEncryption()`, `MaterializeSecrets`, `go`, `WaitGroup.Go`, `Init`, `PostInit` or `Stop` — receive scoped dependencies and task/resource ownership through the new contracts.
- `pkg/proxy/cluster.go` — export construction only through `NewCluster`; retain immutable `ClusterConfig.Key` semantics.
- `pkg/proxy/registry.go` — delete `ClusterRegistry` and `ClusterLease` after route compilation uses `runtime.ResourceRegistry`.
- `pkg/route/builder.go`, `upstream_options.go`, `extra.go`, `production_policy.go` — split pure mechanics into the focused files above, then delete `builder.go` during the atomic cutover.
- `pkg/server/generation_engine.go` — retain prepared generations, activate/rollback/finalize journal transactions and install immutable HTTP/stream snapshots.
- `pkg/server/route_handler.go` — replace `routeSet` with an owned prepared-generation HTTP lease; ordinary dynamic updates no longer close hijacked connections.
- `pkg/server/server.go`, `reload.go`, `stream_test.go`, `route_handler_test.go`, `generation_engine_test.go` — construct a fresh compiler/task registry for each prepared generation, install recovered artifacts, and remove builder/mutable-stream lifecycle paths.
- `pkg/stream/runtime.go`, `router.go` — listeners serve an immutable router snapshot; listener ownership remains ready for supervisor handoff.
- `pkg/apisix/ctx/lifecycle.go` and tests — retain exactly-once finalization while recording plugin panic owner and stage.
- `pkg/plugin/secret_materialization_guard_test.go` — enforce scoped materialization and reject direct store/encryption access.
- `pkg/plugin/*`, `pkg/proxy/*`, `pkg/route/*`, `pkg/stream/*` AST ownership tests — reject unowned production goroutines and duplicate lifecycle tables.

**Delete during the production cutover:**

- `pkg/route/builder.go`
- `pkg/proxy/registry.go`
- `pkg/plugin/request_stage_registry.go`
- `pkg/plugin/capability_registry.go`

Delete the old `Builder`, `NewBuilder`, `NewBuilderWithServerAddr`, `NewBuilderWithClusterRegistry`, `Build`, `BuildStrict`, `BuildWithRouteQuarantine`, `initPlugins`, `initPluginsStrict`, `initPluginBindingsStrict`, `ClusterRegistry`, `ClusterLease`, `RequestStageFor`, `ResolveRequestStage`, `CapabilitySpecForFactory`, `CapabilitySpecForIdentity`, `ResolveResponsePhases`, `ResolveBeforeProxyOwner`, `pluginStopper`, `pluginObserverStarter`, `streamRuntimeOwner.Reload`, `Router.Reload`, `routeHandler.Replace(handler, stop)` and all callers. None may remain as a wrapper for tests.

## Stable Interfaces

These names and signatures are consumed by plans 05–09. If implementation evidence requires changing one, amend this plan, the total program plan and every consuming child plan in one documentation change before code is written.

```go
// package secret
type Scope struct {
	Generation uint64
	Plugin     string
	Resource   generation.ResourceKey
	Field      string
}

type Value struct {
	plaintext string
	digest    [32]byte
}

func (v Value) Use(func(string) error) error
func (v Value) Digest() [32]byte

type ReferenceResolver interface {
	Resolve(context.Context, string) (string, error)
}

type ScopedResolver interface {
	ResolveScoped(context.Context, Scope, string) (string, error)
}

type Materializer interface {
	Materialize(context.Context, Scope, string) (Value, error)
}

type materializer struct {
	encryption data_encryption.Service
	references ReferenceResolver
}

type scopedMaterializer struct {
	resolver ScopedResolver
}

func NewMaterializer(data_encryption.Service, ReferenceResolver) Materializer
func NewScopedMaterializer(ScopedResolver) Materializer

type GenerationCapability struct {
	generation   uint64
	materializer Materializer
}

func NewGenerationCapability(Materializer, uint64) (GenerationCapability, error)
func (c GenerationCapability) Materialize(context.Context, Scope, string) (Value, error)
```

`Value` deliberately exposes plaintext only inside `Use`; errors, digests, resource identities and logs never contain it. Both materializers reject an empty plugin/resource/field scope before resolving. `NewMaterializer` is the in-process plan-02 adapter; `NewScopedMaterializer` wraps plan 05's scoped IPC client and computes `Value.digest` worker-side from the returned plaintext. `GenerationCapability.Materialize` additionally rejects a `Scope.Generation` different from its fixed generation before delegating; workers create it locally and never receive a supervisor pointer or raw keyring.

```go
// package runtime
type RuntimeDependencies struct {
	Config    *config.EffectiveConfig
	Secrets   secret.Materializer
	Resources *ResourceRegistry
	Tasks     *TaskRegistry
}

type ResourceKey struct {
	Kind   string
	Scope  string
	Digest [32]byte
}

type ResourceFactory[T any] func(context.Context) (T, func(context.Context) error, error)

type resourceEntry struct {
	ready         chan struct{}
	value         any
	closeResource func(context.Context) error
	createErr     error
	references    int
	closeOnce     sync.Once
	closeErr      error
}

type ResourceRegistry struct {
	mu      sync.Mutex
	entries map[ResourceKey]*resourceEntry
	closed  bool
}

type ResourceLease[T any] struct {
	value       T
	release     func(context.Context) error
	releaseOnce sync.Once
	releaseErr  error
}
func Acquire[T any](context.Context, *ResourceRegistry, ResourceKey, ResourceFactory[T]) (*ResourceLease[T], error)
func (l *ResourceLease[T]) Value() T
func (l *ResourceLease[T]) Release(context.Context) error
func NewResourceRegistry() *ResourceRegistry
func (r *ResourceRegistry) Len() int
func (r *ResourceRegistry) Close(context.Context) error

type TaskCriticality string
const (
	TaskPlugin TaskCriticality = "plugin"
	TaskCore   TaskCriticality = "core"
)

type TaskSpec struct {
	Owner       string
	Criticality TaskCriticality
}

type TaskFailure struct {
	Owner      string
	Err        error
	PanicValue any
	Stack      []byte
}

type TaskResidual struct { Owner string }

type TaskRegistry struct {
	ctx       context.Context
	cancel    context.CancelFunc
	onFailure func(TaskFailure)
	mu        sync.Mutex
	stopped   bool
	active    map[string]int
	failed    map[string]struct{}
	wg        sync.WaitGroup
	cancelOnce sync.Once
}

func NewTaskRegistry(context.Context, func(TaskFailure)) *TaskRegistry
func (r *TaskRegistry) Go(TaskSpec, func(context.Context) error) error
func (r *TaskRegistry) Stop(context.Context) ([]TaskResidual, error)
func (r *TaskRegistry) Active() []string

type RequestTaskGroup struct {
	ctx     context.Context
	owner   string
	mu      sync.Mutex
	waiting bool
	errs    []error
	wg      sync.WaitGroup
}

func NewRequestTaskGroup(context.Context, string) *RequestTaskGroup
func (g *RequestTaskGroup) Go(func(context.Context) error) error
func (g *RequestTaskGroup) Wait() error
```

`TaskPlugin` recovers and reports a panic, marks the owner failed and stops accepting new tasks for that owner. `TaskCore` does not recover a panic, so an invariant failure terminates the worker. `Stop` cancels once, waits to the caller deadline, and returns sorted residual owner names.

```go
// package plugin
type Plugin interface {
	Config() any
	GetSchema() string
	GetMetadataSchema() string
	GetName() string
}

type Initializer interface { Init() error }
type PostInitializer interface { PostInit() error }
type Middleware interface { Handler(http.Handler) http.Handler }
type Stopper interface { Stop() }
type ObserverStarter interface { StartObserving() }

type Phase string
const (
	PhaseRewrite         Phase = "rewrite"
	PhaseConsumerRewrite Phase = "consumer_rewrite"
	PhaseAccess          Phase = "access"
	PhaseBeforeProxy     Phase = "before_proxy"
	PhaseHeaderFilter    Phase = "header_filter"
	PhaseBodyFilter      Phase = "body_filter"
	PhaseLog             Phase = "log"
	PhaseFinalizer       Phase = "finalizer"
	PhaseProtocol        Phase = "protocol"
)

type InstanceScope string
const (
	InstancePerRoute        InstanceScope = "route"
	InstancePerService      InstanceScope = "service"
	InstancePerConsumer     InstanceScope = "consumer"
	InstancePerGlobalRule   InstanceScope = "global_rule"
	InstanceEffectiveConfig InstanceScope = "effective-config"
)

type Descriptor struct {
	Factory       string
	Implementation string
	Phases        []Phase
	Priority      int
	Scopes        []Scope
	InstanceScope InstanceScope
}

type InstanceKey struct {
	Factory      string
	Scope        Scope
	Owner        ResourceProvenance
	ConfigDigest [32]byte
}

func DescriptorForFactory(*capability.Manifest, string) (Descriptor, error)
func ResolveDescriptor(Descriptor, Plugin) (Descriptor, error)
func NewInstanceKey(Descriptor, Scope, ResourceProvenance, any) (InstanceKey, error)
```

`Binding` stores a resolved `Descriptor`, effective priority and `InstanceKey`; executors never query mutable priority or a handwritten registry at request time.

```go
// package compiler
type compileHTTPFunc func(context.Context, generation.GenerationArtifact, route.CompileInput) (*HTTPSnapshot, error)
type compileStreamFunc func(context.Context, generation.GenerationArtifact, []resource.StreamRoute, []string, func(stream.Result)) (*StreamSnapshot, error)

type Compiler struct {
	manifest     *capability.Manifest
	dependencies runtime.RuntimeDependencies
	compileHTTP  compileHTTPFunc
	compileStream compileStreamFunc
}

type WorkerCompilerFactory struct {
	manifest     *capability.Manifest
	config       *config.EffectiveConfig
	materializer secret.Materializer
	resources    *runtime.ResourceRegistry
	mu           sync.Mutex
	closed       bool
}

func New(*capability.Manifest, runtime.RuntimeDependencies) (*Compiler, error)
func NewWorkerCompilerFactory(*capability.Manifest, *config.EffectiveConfig, secret.Materializer) (*WorkerCompilerFactory, error)
func (f *WorkerCompilerFactory) PrepareGeneration(context.Context, generation.ApplyTicket, generation.Snapshot,
	map[generation.Domain]generation.PublishedGeneration, func(runtime.TaskFailure)) (*PreparedGeneration, error)
func (f *WorkerCompilerFactory) Close(context.Context) error
func (c *Compiler) Prepare(
	context.Context,
	generation.ApplyTicket,
	generation.Snapshot,
	map[generation.Domain]generation.PublishedGeneration,
) (*PreparedGeneration, error)

type HTTPSnapshot struct {
	artifact generation.GenerationArtifact
	handler  http.Handler
	tlsConfig *tls.Config
}
func (s *HTTPSnapshot) Revision() uint64
func (s *HTTPSnapshot) Handler() http.Handler
func (s *HTTPSnapshot) TLSConfig() *tls.Config

type StreamSnapshot struct {
	artifact generation.GenerationArtifact
	router   *stream.Router
}
func (s *StreamSnapshot) Revision() uint64
func (s *StreamSnapshot) Router() *stream.Router

type preparedLease interface {
	Release(context.Context) error
}

type PreparedGeneration struct {
	publication generation.PublicationSet
	http        *HTTPSnapshot
	stream      *StreamSnapshot
	leases      []preparedLease
	tasks       *runtime.TaskRegistry
	probes      []func(context.Context) error
	closeOnce   sync.Once
	closeErr    error
}
func (p *PreparedGeneration) PublicationSet() generation.PublicationSet
func (p *PreparedGeneration) HTTP() (*HTTPSnapshot, bool)
func (p *PreparedGeneration) Stream() (*StreamSnapshot, bool)
func (p *PreparedGeneration) Probe(context.Context) error
func (p *PreparedGeneration) DiscardPrepared(context.Context, generation.PublicationSet) error
func (p *PreparedGeneration) Close(context.Context) error
```

All returned slices, maps, resource bytes and publication values are defensive copies. `HTTPSnapshot.TLSConfig` returns nil or `s.tlsConfig.Clone()` so plan 06 can select certificates without mutating generation state. `PreparedGeneration.Close` is idempotent, stops generation tasks before releasing resource leases in reverse acquisition order, and never closes a shared resource while another generation holds a lease. `DiscardPrepared` first compares canonical publication identity and returns `ErrPreparedSetMismatch` without closing on mismatch; on a match it delegates to the same idempotent `Close` path.

`generation.Journal` remains the only durable writer. `Compiler.Prepare` consumes `generation.PublishedGeneration` values supplied by `generation.Coordinator`; it never calls `Journal.Stage`, `Commit` or `Abort`. The `generation.PublicationEngine` contract gains `DiscardPrepared(context.Context, generation.PublicationSet) error`; the coordinator calls it when staging fails after a successful prepare. `server.GenerationEngine` implements that method by removing the matching pending prepared generation and calling its `DiscardPrepared`, which verifies the defensive-copy publication set and then closes candidate tasks and leases.

An exec-created worker reconstructs `WorkerCompilerFactory` from its local effective config, manifest and the plan 05 scoped IPC client implementing `secret.Materializer`. The constructor creates its own `ResourceRegistry`; it accepts no supervisor object, `data_encryption.Service`, reference resolver or raw keyring. Before the exec cutover, the in-process owner adapts plan 02 with `secret.NewMaterializer(data_encryption.Service, resolver)` and passes only that interface to the same factory. `PrepareGeneration` atomically wraps the supplied materializer in a `GenerationCapability`, creates a fresh generation-owned `TaskRegistry`, constructs the compiler, prepares the candidate and transfers task ownership to the successful `PreparedGeneration`; every constructor or prepare failure stops the task registry before returning. `RuntimeDependencies.Resources` is worker-owned and shared; `RuntimeDependencies.Tasks` is never reused across prepared generations. `WorkerCompilerFactory.Close` first prevents new generations, then closes the worker registry; the plan 05 cutover deletes every worker-side raw encryption-service/keyring path.

---

### Task 1: Create Scoped Secret Materialization and Runtime Dependencies

> **Execution amendment:** Land the `pkg/secret` materializer half in parallel with Tasks 2, the resource-registry core of Task 3, and Task 4. Create `pkg/runtime/dependencies.go` only after the real `TaskRegistry` and `ResourceRegistry` types are merged. Do not use placeholder types. See `2026-08-24-journal-immutable-cutover-reorder.md`.

**Files:**

- Create: `pkg/secret/materializer.go`
- Create: `pkg/secret/materializer_test.go`
- Create: `pkg/runtime/dependencies.go`
- Test: `pkg/runtime/dependencies_test.go`
- Consume: `pkg/data_encryption/service.go` (`Service`, `Resolver`)

**Interfaces:**

- Consumes: immutable `data_encryption.Service` plus `ReferenceResolver` for the in-process phase, and plan 05 `secret.ScopedResolver` for the exec-worker phase.
- Produces: exact `secret.Materializer`, `secret.ScopedResolver`, `NewScopedMaterializer`, `secret.GenerationCapability`, `secret.Scope`, `secret.Value` and `runtime.RuntimeDependencies` interfaces above.

- [ ] **Step 1: Write the failing materializer scope and redaction tests**

```go
func TestMaterializerRequiresOwnedScopeAndNeverLeaksPlaintext(t *testing.T) {
	want := "credential-value"
	refs := referenceResolverFunc(func(context.Context, string) (string, error) { return want, nil })
	materializer := NewMaterializer(data_encryption.NewService(false, nil), refs)
	_, err := materializer.Materialize(context.Background(), Scope{}, "$ENV://TOKEN")
	if err == nil || strings.Contains(err.Error(), want) {
		t.Fatalf("Materialize() error = %v, want redacted scope error", err)
	}
	value, err := materializer.Materialize(context.Background(), Scope{
		Generation: 9,
		Plugin: "http-logger",
		Resource: generation.ResourceKey{Kind: "routes", ID: "r1"},
		Field: "token",
	}, "$ENV://TOKEN")
	if err != nil { t.Fatal(err) }
	var got string
	if err := value.Use(func(plaintext string) error { got = plaintext; return nil }); err != nil { t.Fatal(err) }
	if got != want || value.Digest() != sha256.Sum256([]byte(want)) {
		t.Fatalf("materialized value/digest mismatch")
	}
}

func TestGenerationCapabilityRejectsCrossGenerationScope(t *testing.T) {
	base := NewMaterializer(data_encryption.NewService(false, nil), passthroughResolver{})
	capability, err := NewGenerationCapability(base, 9)
	if err != nil { t.Fatal(err) }
	_, err = capability.Materialize(context.Background(), Scope{Generation: 10, Plugin: "http-logger",
		Resource: generation.ResourceKey{Kind: "routes", ID: "r1"}, Field: "token"}, "value")
	if !errors.Is(err, ErrCapabilityScopeMismatch) { t.Fatalf("Materialize() error = %v", err) }
}

type captureScopedResolver struct {
	scope     Scope
	raw       string
	plaintext string
	err       error
}

func (r *captureScopedResolver) ResolveScoped(_ context.Context, scope Scope, raw string) (string, error) {
	r.scope, r.raw = scope, raw
	return r.plaintext, r.err
}

func TestScopedMaterializerPassesOwnedScopeAndRedactsResolverError(t *testing.T) {
	wantScope := Scope{Generation: 9, Plugin: "http-logger",
		Resource: generation.ResourceKey{Kind: "routes", ID: "r1"}, Field: "token"}
	resolver := &captureScopedResolver{plaintext: "credential-value"}
	materializer := NewScopedMaterializer(resolver)
	value, err := materializer.Materialize(context.Background(), wantScope, "$secret://logger/token")
	if err != nil { t.Fatal(err) }
	if resolver.scope != wantScope || resolver.raw != "$secret://logger/token" { t.Fatalf("resolver input = %#v/%q", resolver.scope, resolver.raw) }
	if value.Digest() != sha256.Sum256([]byte("credential-value")) { t.Fatal("digest mismatch") }
	resolver.err = errors.New("backend included credential-value")
	_, err = materializer.Materialize(context.Background(), wantScope, "$secret://logger/token")
	if !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), "credential-value") {
		t.Fatalf("Materialize() error = %v", err)
	}
}
```

- [ ] **Step 2: Run the materializer test and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/secret -run "^TestMaterializer" -count=1'`

Expected: FAIL because `pkg/secret` and `NewMaterializer` do not exist.

- [ ] **Step 3: Implement the exact scoped materializer**

```go
func (m materializer) Materialize(ctx context.Context, scope Scope, raw string) (Value, error) {
	if err := validateScope(scope); err != nil { return Value{}, err }
	if err := ctx.Err(); err != nil { return Value{}, err }
	resolved := raw
	if strings.HasPrefix(raw, "$secret://") || strings.HasPrefix(strings.ToUpper(raw), "$ENV://") {
		value, err := m.references.Resolve(ctx, raw)
		if err != nil { return Value{}, ErrCredentialUnavailable }
		resolved = value
	}
	plaintext, err := m.encryption.Resolver().ResolveForContext(resolved, materializationContext(scope))
	if err != nil { return Value{}, ErrCredentialUnavailable }
	return Value{plaintext: plaintext, digest: sha256.Sum256([]byte(plaintext))}, nil
}

func (v Value) Use(use func(string) error) error {
	if use == nil { return errors.New("secret use callback is required") }
	return use(v.plaintext)
}

func NewGenerationCapability(materializer Materializer, generation uint64) (GenerationCapability, error) {
	if materializer == nil || generation == 0 { return GenerationCapability{}, ErrInvalidCapability }
	return GenerationCapability{generation: generation, materializer: materializer}, nil
}

func (c GenerationCapability) Materialize(ctx context.Context, scope Scope, raw string) (Value, error) {
	if scope.Generation != c.generation { return Value{}, ErrCapabilityScopeMismatch }
	return c.materializer.Materialize(ctx, scope, raw)
}

func NewScopedMaterializer(resolver ScopedResolver) Materializer {
	return scopedMaterializer{resolver: resolver}
}

func (m scopedMaterializer) Materialize(ctx context.Context, scope Scope, raw string) (Value, error) {
	if err := validateScope(scope); err != nil { return Value{}, err }
	if err := ctx.Err(); err != nil { return Value{}, err }
	if m.resolver == nil { return Value{}, ErrCredentialUnavailable }
	plaintext, err := m.resolver.ResolveScoped(ctx, scope, raw)
	if err != nil { return Value{}, ErrCredentialUnavailable }
	return Value{plaintext: plaintext, digest: sha256.Sum256([]byte(plaintext))}, nil
}
```

Define `ErrCredentialUnavailable`, `ErrInvalidCapability` and `ErrCapabilityScopeMismatch` with constant redacted messages. `materializationContext` contains only generation, plugin, resource kind/ID and field; it never includes `raw` or plaintext.

- [ ] **Step 4: Add runtime dependency validation**

```go
func (d RuntimeDependencies) Validate() error {
	if d.Config == nil { return errors.New("runtime dependencies: effective config is required") }
	if d.Secrets == nil { return errors.New("runtime dependencies: secret materializer is required") }
	if d.Resources == nil { return errors.New("runtime dependencies: resource registry is required") }
	if d.Tasks == nil { return errors.New("runtime dependencies: task registry is required") }
	return nil
}
```

- [ ] **Step 5: Run focused tests**

Run: `bash -lc 'source .envrc && go test ./pkg/secret ./pkg/runtime -run "^(TestMaterializer|TestRuntimeDependencies)" -count=1'`

Expected: PASS; cancellation and resolver errors contain no plaintext or raw reference.

- [ ] **Step 6: Commit the dependency foundation**

```bash
git add pkg/secret pkg/runtime/dependencies.go pkg/runtime/dependencies_test.go
git commit -m "feat(runtime): define scoped generation dependencies"
```

---

### Task 2: Implement Named Task Ownership and Request Task Groups

**Files:**

- Create: `pkg/runtime/task_registry.go`
- Create: `pkg/runtime/task_registry_test.go`
- Create: `pkg/runtime/request_tasks.go`
- Create: `pkg/runtime/request_tasks_test.go`

**Interfaces:**

- Consumes: one generation or request parent context and a bounded failure callback.
- Produces: exact `TaskRegistry`, `TaskSpec`, `TaskFailure`, `TaskResidual` and `RequestTaskGroup` interfaces above.

- [ ] **Step 1: Write failing plugin-panic and join tests**

```go
func TestTaskRegistryReportsPluginPanicAndJoinsOwner(t *testing.T) {
	failures := make(chan TaskFailure, 1)
	registry := NewTaskRegistry(context.Background(), func(f TaskFailure) { failures <- f })
	if err := registry.Go(TaskSpec{Owner: "plugin/http-logger/r1", Criticality: TaskPlugin},
		func(context.Context) error { panic("boom") }); err != nil { t.Fatal(err) }
	failure := <-failures
	if failure.Owner != "plugin/http-logger/r1" || failure.PanicValue != "boom" || len(failure.Stack) == 0 {
		t.Fatalf("failure = %#v", failure)
	}
	residuals, err := registry.Stop(context.Background())
	if err != nil || len(residuals) != 0 { t.Fatalf("Stop() = (%v, %v)", residuals, err) }
}

func TestTaskRegistryStopReportsCancellationIgnoringOwner(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	release := make(chan struct{})
	if err := registry.Go(TaskSpec{Owner: "plugin/stuck/r1", Criticality: TaskPlugin},
		func(context.Context) error { <-release; return nil }); err != nil { t.Fatal(err) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	residuals, err := registry.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(residuals,
		[]TaskResidual{{Owner: "plugin/stuck/r1"}}) { t.Fatalf("Stop() = (%v, %v)", residuals, err) }
	close(release)
}
```

- [ ] **Step 2: Run task tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/runtime -run "^(TestTaskRegistry|TestRequestTaskGroup)" -count=1'`

Expected: FAIL because registries and groups do not exist.

- [ ] **Step 3: Implement plugin and core panic policy**

```go
func (r *TaskRegistry) Go(spec TaskSpec, run func(context.Context) error) error {
	if err := validateTaskSpec(spec, run); err != nil { return err }
	if !r.begin(spec.Owner) { return ErrTaskRegistryStopped }
	go func() {
		defer r.finish(spec.Owner)
		if spec.Criticality == TaskCore {
			if err := run(r.ctx); err != nil { r.report(TaskFailure{Owner: spec.Owner, Err: err}) }
			return
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				r.failOwner(spec.Owner)
				r.report(TaskFailure{Owner: spec.Owner, PanicValue: recovered, Stack: debug.Stack()})
			}
		}()
		if err := run(r.ctx); err != nil { r.failOwner(spec.Owner); r.report(TaskFailure{Owner: spec.Owner, Err: err}) }
	}()
	return nil
}
```

`NewTaskRegistry` derives `ctx/cancel` from the supplied parent and initializes `active` and `failed`. `begin`, `finish`, `Active` and `Stop` guard `stopped`, both maps and wait-group admission with `mu`; `cancelOnce` cancels exactly once, and each accepted task contributes one `wg` count. Failure callbacks execute outside the mutex. A `TaskCore` panic is deliberately not recovered.

- [ ] **Step 4: Implement request task groups with no detached completion**

```go
func (g *RequestTaskGroup) Go(run func(context.Context) error) error {
	if run == nil { return errors.New("request task callback is required") }
	g.mu.Lock()
	if g.waiting { g.mu.Unlock(); return ErrTaskGroupWaiting }
	g.wg.Add(1)
	g.mu.Unlock()
	go func() { defer g.wg.Done(); g.record(run(g.ctx)) }()
	return nil
}

func (g *RequestTaskGroup) Wait() error {
	g.mu.Lock(); g.waiting = true; g.mu.Unlock()
	g.wg.Wait()
	return errors.Join(g.errors()...)
}
```

`NewRequestTaskGroup` stores the parent context and bounded owner and initializes an empty `errs` slice; `Go` returns a stable validation error before admission if either constructor input is invalid. `record` appends under `mu`; `Wait` sets `waiting` before waiting, clones `errs`, and returns `errors.Join` without permitting another `Go` admission.

- [ ] **Step 5: Run task race tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/runtime -run "^(TestTaskRegistry|TestRequestTaskGroup)" -count=1'`

Expected: PASS; `Active()` and residuals are sorted and contain owner names only.

- [ ] **Step 6: Commit task ownership primitives**

```bash
git add pkg/runtime/task_registry.go pkg/runtime/task_registry_test.go pkg/runtime/request_tasks.go pkg/runtime/request_tasks_test.go
git commit -m "feat(runtime): own generation and request tasks"
```

---

### Task 3: Replace the Cluster Registry With a Generic Resource Registry

> **Execution amendment:** The parallel foundation branch contains only the generic registry, `proxy.NewCluster` and their tests. Postpone `RuntimeDependencies`-based cluster/compiler acquisition to Task 7, where it is first used. Keep the current `ClusterRegistry` unchanged until the joint cutover.

**Files:**

- Create: `pkg/runtime/resource_registry.go`
- Create: `pkg/runtime/resource_registry_test.go`
- Modify: `pkg/proxy/cluster.go` (`newCluster` → `NewCluster`)
- Modify: `pkg/proxy/cluster_test.go`
- Modify: `pkg/route/upstream_options.go` (`ClusterConfig.Key` consumption)
- Modify temporarily: `pkg/proxy/registry.go`, `pkg/proxy/registry_test.go`, `pkg/proxy/registry_metrics_test.go` (keep the production owner buildable until Task 9; final deletion happens only with the production cutover)

**Interfaces:**

- Consumes: canonical `proxy.ClusterConfig.Key()` digests and generation/plugin scope strings.
- Produces: exact generic `runtime.Acquire`, `ResourceRegistry`, `ResourceKey` and `ResourceLease[T]`; equal reload state reuse no longer depends on a proxy-specific registry.

- [ ] **Step 1: Write failing concurrent acquire and release tests**

```go
func TestResourceRegistrySharesEqualIdentityUntilFinalRelease(t *testing.T) {
	registry := NewResourceRegistry()
	key := ResourceKey{Kind: "upstream-cluster", Scope: "upstream/u1", Digest: sha256.Sum256([]byte("same"))}
	var creates atomic.Int32
	factory := func(context.Context) (*testResource, func(context.Context) error, error) {
		creates.Add(1)
		resource := &testResource{}
		return resource, func(context.Context) error { resource.closed.Store(true); return nil }, nil
	}
	first, err := Acquire(context.Background(), registry, key, factory); if err != nil { t.Fatal(err) }
	second, err := Acquire(context.Background(), registry, key, factory); if err != nil { t.Fatal(err) }
	if first.Value() != second.Value() || creates.Load() != 1 { t.Fatal("equal identity did not share") }
	if err := first.Release(context.Background()); err != nil { t.Fatal(err) }
	if first.Value().closed.Load() { t.Fatal("resource closed before final release") }
	if err := second.Release(context.Background()); err != nil { t.Fatal(err) }
	if !second.Value().closed.Load() { t.Fatal("resource remained open after final release") }
}
```

Add cases for different scope, different digest, concurrent first acquire, failed factory not cached, canceled factory, idempotent release and terminal registry close.

```go
func TestResourceLeaseConcurrentReleaseRunsCloseOnce(t *testing.T) {
	registry := NewResourceRegistry()
	var closes atomic.Int32
	lease, err := Acquire(context.Background(), registry, testResourceKey(),
		func(context.Context) (*testResource, func(context.Context) error, error) {
			return &testResource{}, func(context.Context) error { closes.Add(1); return errCloseFixture }, nil
		})
	if err != nil { t.Fatal(err) }
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { errs <- lease.Release(context.Background()) })
	}
	wg.Wait()
	close(errs)
	for err := range errs { if !errors.Is(err, errCloseFixture) { t.Fatalf("Release() error = %v", err) } }
	if got := closes.Load(); got != 1 { t.Fatalf("close calls = %d", got) }
}
```

- [ ] **Step 2: Run resource tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/runtime -run "^TestResourceRegistry" -count=1'`

Expected: FAIL because `ResourceRegistry` and `Acquire` do not exist.

- [ ] **Step 3: Implement single-creator acquisition and typed leases**

```go
func Acquire[T any](ctx context.Context, registry *ResourceRegistry, key ResourceKey,
	factory ResourceFactory[T]) (*ResourceLease[T], error) {
	if registry == nil || key.Kind == "" || key.Scope == "" || key.Digest == ([32]byte{}) {
		return nil, ErrInvalidResourceIdentity
	}
	entry, creator, err := registry.reserve(ctx, key)
	if err != nil { return nil, err }
	if creator {
		value, closeResource, createErr := factory(ctx)
		registry.complete(key, entry, value, closeResource, createErr)
	}
	value, release, err := registry.await(ctx, key, entry)
	if err != nil { return nil, err }
	typed, ok := value.(T)
	if !ok { release(context.Background()); return nil, ErrResourceTypeMismatch }
	return &ResourceLease[T]{value: typed, release: release}, nil
}

func (l *ResourceLease[T]) Value() T { return l.value }

func (l *ResourceLease[T]) Release(ctx context.Context) error {
	l.releaseOnce.Do(func() { l.releaseErr = l.release(ctx) })
	return l.releaseErr
}
```

Construct `ResourceRegistry` with a non-nil `entries` map. Each `resourceEntry.ready` closes exactly once after its factory stores `value`, `closeResource` and `createErr`; `references`, `closed` and the map are protected by `ResourceRegistry.mu`. Entry and lease `sync.Once` fields make registry-close/final-release and concurrent repeated lease release replay one stored close error. The final release removes the entry before invoking its close function outside the registry mutex. `Close` prevents new acquisitions, waits for in-progress factories, closes each remaining resource once and returns `errors.Join`.

- [ ] **Step 4: Export cluster construction and keep the live registry buildable**

Rename the implementation constructor to `proxy.NewCluster`, update `pkg/proxy/registry.go` mechanically to call it, and keep `ClusterRegistry` behavior and ownership unchanged. Do not retain a private forwarding `newCluster` wrapper.

#### Deferred integration: acquire clusters from the compiler in Task 7

In Task 7, move observer deletion assertions to resource-registry-backed route/compiler tests and make newly extracted HTTP compiler code acquire clusters through `runtime.ResourceRegistry`. Keep the existing `ClusterRegistry` reachable only from the still-live `route.Builder` path until Task 9. Do not add an alias, wrapper or selection flag between the registries; Task 9 deletes Builder and the proxy-specific registry together after the new path becomes the only production owner.

- [ ] **Step 5: Run registry and live-cluster race tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/runtime ./pkg/proxy -run "^(TestResourceRegistry|TestCluster|TestClusterRegistry)" -count=1 && make build'`

Expected: PASS; registry-core tests prove equal identities share one resource until final release, and the mechanically updated live `ClusterRegistry` remains green and buildable. Compiler acquisition is intentionally not claimed yet.

- [ ] **Step 6: Commit the resource-registry foundation**

```bash
git add pkg/runtime/resource_registry.go pkg/runtime/resource_registry_test.go pkg/proxy/cluster.go pkg/proxy/cluster_test.go pkg/proxy/registry.go pkg/proxy/registry_test.go pkg/proxy/registry_metrics_test.go
git commit -m "refactor(runtime): add generic resource ownership"
```

---

### Task 4: Make the Capability Manifest the Runtime Plugin Descriptor

**Files:**

- Create: `pkg/plugin/descriptor.go`
- Create: `pkg/plugin/descriptor_test.go`
- Create: `pkg/plugin/instance.go`
- Create: `pkg/plugin/instance_test.go`
- Modify: `pkg/plugin/executor.go` (`Binding`, `BindPluginChecked`, ordering)
- Modify: `pkg/plugin/response_executor.go`, `streaming_executor.go`, `log_executor.go`
- Modify: `pkg/plugin/manifest_contract_test.go`
- Modify temporarily: `pkg/route/builder.go` (`initPluginBindingsStrict` and plugin-source classification consume `Descriptor`; the file remains only until Task 9)
- Delete at task end: `pkg/plugin/request_stage_registry.go`
- Delete at task end: `pkg/plugin/capability_registry.go`
- Delete/replace: `pkg/plugin/capability_registry_test.go` and request-stage duplicate-table tests in `pkg/plugin/init_test.go`

**Interfaces:**

- Consumes: `capability.Manifest.Plugin(factory)` generated in plan 01 and optional config-aware `base.BindingPhaseDescriber` callbacks.
- Produces: exact `plugin.Descriptor`, `ResolveDescriptor`, `InstanceKey` and immutable descriptor-bearing `Binding`.

- [ ] **Step 1: Write the failing manifest descriptor contract**

```go
func TestDescriptorForFactoryUsesManifestPhasePriorityScope(t *testing.T) {
	manifest, err := capability.Load(); if err != nil { t.Fatal(err) }
	descriptor, err := DescriptorForFactory(manifest, "request-id"); if err != nil { t.Fatal(err) }
	entry, _ := manifest.Plugin("request-id")
	if descriptor.Priority != entry.Priority || descriptor.Factory != "request-id" ||
		!slices.Equal(descriptor.Phases, []Phase{PhaseRewrite}) {
		t.Fatalf("descriptor = %#v, manifest = %#v", descriptor, entry)
	}
}

func TestNewInstanceKeySeparatesScopeButIgnoresMapOrder(t *testing.T) {
	d := Descriptor{Factory: "limit-count", InstanceScope: InstancePerRoute}
	a, err := NewInstanceKey(d, ScopeRoute, ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
		map[string]any{"count": 10, "time_window": 60}); if err != nil { t.Fatal(err) }
	b, err := NewInstanceKey(d, ScopeRoute, ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
		map[string]any{"time_window": 60, "count": 10}); if err != nil { t.Fatal(err) }
	if a != b { t.Fatalf("canonical identities differ: %#v %#v", a, b) }
}
```

- [ ] **Step 2: Run descriptor tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin -run "^(TestDescriptor|TestNewInstanceKey)" -count=1'`

Expected: FAIL because the descriptor and identity APIs do not exist.

- [ ] **Step 3: Implement strict manifest parsing**

```go
func DescriptorForFactory(manifest *capability.Manifest, factory string) (Descriptor, error) {
	if manifest == nil { return Descriptor{}, errors.New("plugin descriptor: manifest is required") }
	entry, ok := manifest.Plugin(factory)
	if !ok { return Descriptor{}, fmt.Errorf("plugin descriptor: unknown factory %q", factory) }
	phases, err := parseManifestPhases(entry.Phases); if err != nil { return Descriptor{}, err }
	scopes, err := parseManifestScopes(entry.Scopes); if err != nil { return Descriptor{}, err }
	instanceScope, err := parseInstanceScope(entry.InstanceScope); if err != nil { return Descriptor{}, err }
	return Descriptor{Factory: factory, Implementation: entry.Implementation, Phases: phases,
		Priority: entry.Priority, Scopes: scopes, InstanceScope: instanceScope}, nil
}
```

Reject unknown phase/scope/instance strings and a factory key not present in the entry's `Factories`. `ResolveDescriptor` may narrow config-aware phases only through `BindingPhaseDescriber`; it cannot add a phase absent from the manifest's declared allowed set.

- [ ] **Step 4: Put descriptor and effective priority on `Binding`**

```go
type Binding struct {
	Plugin      Plugin
	Descriptor  Descriptor
	Priority    int
	Scope       Scope
	Provenance  ResourceProvenance
	InstanceKey InstanceKey
}

func compareBindings(a, b Binding) int {
	if a.Scope != b.Scope { return cmp.Compare(a.Scope, b.Scope) }
	if phase := compareDescriptorPhase(a.Descriptor, b.Descriptor); phase != 0 { return phase }
	if priority := cmp.Compare(b.Priority, a.Priority); priority != 0 { return priority }
	return cmp.Compare(a.Descriptor.Factory, b.Descriptor.Factory)
}
```

`_meta.priority` sets `Binding.Priority`; it never mutates the plugin instance. Equal phase/priority order is stable by canonical factory and provenance identity, not map iteration.

- [ ] **Step 5: Switch every executor and the still-live Builder to descriptor facts**

Replace `RequestStageFor`, `CapabilitySpecForFactory`, `ResolveResponsePhases`, `ResolveBeforeProxyOwner`, `finalizerForIdentity` and `generationOwnerForIdentity` calls with `Binding.Descriptor` predicates. Update `route.Builder` in the same task so its remaining production call sites resolve the manifest descriptor and pass it into `Binding`; preserve config-aware callbacks by resolving them once during construction. This is a direct source-of-truth replacement inside the old owner, not an adapter or selectable runtime path.

- [ ] **Step 6: Delete both handwritten runtime registries**

```bash
test ! -e pkg/plugin/request_stage_registry.go
test ! -e pkg/plugin/capability_registry.go
! rg -n 'requestStageRegistry|capabilityRegistry|RequestStageFor|CapabilitySpecFor|ResolveResponsePhases|ResolveBeforeProxyOwner' pkg/plugin pkg/route --glob '*.go'
```

Expected: PASS after deletion; `pkg/capability/manifest.yaml` plus generated factory registration is the only phase/priority/scope database.

- [ ] **Step 7: Run plugin executor and manifest tests**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin -run "^(TestDescriptor|TestNewInstanceKey|TestManifest|TestRequestPipeline|TestResponse|TestStreaming|TestLog)" -count=1'`

Expected: PASS with exact APISIX scope/phase/priority ordering.

- [ ] **Step 8: Commit descriptor ownership**

```bash
git add pkg/plugin pkg/capability/manifest.yaml
git add pkg/route/builder.go pkg/route/*_test.go
git commit -m "refactor(plugin): derive runtime descriptors from manifest"
```

---

### Task 5: Normalize Resources and Resolve Atomic Domain Closures

> **Execution amendment:** The detailed steps below are superseded where they conflict with `2026-08-24-immutable-task5-execution-brief.md`. In particular, execute C0 first; do not create `worker_factory.go` in this task, do not acquire clusters, cover all managed resource kinds, and preserve raw/exact-generic/typed representations.

Execute only the linked brief. Task 5 produces `PreparePublication`; Task 6 composes it behind the stable `Prepare(...)(*PreparedGeneration, error)` and atomic worker-factory lifecycle.

<!-- Superseded Task 5 reference retained for historical context only. Do not execute.

**Files:**

- Create: `pkg/compiler/types.go`
- Create: `pkg/compiler/compiler.go`
- Create: `pkg/compiler/worker_factory.go`
- Create: `pkg/compiler/worker_factory_test.go`
- Create: `pkg/compiler/normalize.go`
- Create: `pkg/compiler/normalize_test.go`
- Create: `pkg/compiler/validate.go`
- Create: `pkg/compiler/closure.go`
- Create: `pkg/compiler/closure_test.go`
- Modify: `pkg/resource/*` only where a decoder currently requires a live store getter

**Interfaces:**

- Consumes: `generation.ApplyTicket`, immutable desired `generation.Snapshot`, previous `map[generation.Domain]generation.PublishedGeneration`, `capability.Manifest` and `runtime.RuntimeDependencies`.
- Produces: `compiler.New`, `NewWorkerCompilerFactory`, worker-local generation compiler construction, the pure normalize/validate/resolve phases, complete `generation.PublicationCandidate` values and no external side effect.

- [ ] **Step 1: Write a failing phase-order and no-side-effect test**

```go
func TestCompilerRejectsInvalidDependencyBeforeMaterialization(t *testing.T) {
	deps, calls := runtimeDependenciesWithSpies(t)
	compiler, err := New(testManifest(t), deps); if err != nil { t.Fatal(err) }
	desired := snapshotWithRouteReferencingMissingUpstream(t, 11, "r1", "missing")
	prepared, err := compiler.Prepare(context.Background(), ticketFor(desired, generation.DomainHTTP), desired, nil)
	if err != nil { t.Fatal(err) }
	decision := findDecision(prepared.PublicationSet(), generation.DomainHTTP,
		generation.ResourceKey{Kind: "routes", ID: "r1"})
	if decision.Disposition != generation.DispositionQuarantined || decision.Code != "dependency-missing" {
		t.Fatalf("decision = %#v", decision)
	}
	if calls.Secrets.Load() != 0 || calls.Resources.Load() != 0 || calls.Tasks.Load() != 0 {
		t.Fatalf("pure-phase failure created side effects: %#v", calls)
	}
}

type panicMaterializer struct{}

func (panicMaterializer) Materialize(context.Context, secret.Scope, string) (secret.Value, error) {
	panic("generation capability must reject before delegation")
}

func TestWorkerCompilerFactoryCreatesScopedGenerationOwnersLocally(t *testing.T) {
	factory, err := NewWorkerCompilerFactory(testManifest(t), testEffectiveConfig(t), panicMaterializer{})
	if err != nil { t.Fatal(err) }
	defer factory.Close(context.Background())
	compiler, err := factory.NewGeneration(context.Background(), generation.ApplyTicket{DesiredRevision: 11}, nil)
	if err != nil { t.Fatal(err) }
	if compiler.dependencies.Resources != factory.resources || compiler.dependencies.Tasks == nil {
		t.Fatal("worker/generation ownership was not constructed locally")
	}
	if _, err := compiler.dependencies.Secrets.Materialize(context.Background(), secret.Scope{Generation: 12,
		Plugin: "request-id", Resource: generation.ResourceKey{Kind: "routes", ID: "r1"}, Field: "value"}, "x");
		!errors.Is(err, secret.ErrCapabilityScopeMismatch) { t.Fatalf("cross-generation secret error = %v", err) }
}
```

- [ ] **Step 2: Run compiler tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/compiler -run "^(TestCompiler|TestWorkerCompilerFactory|TestNormalize|TestDependencyClosure)" -count=1'`

Expected: FAIL because `pkg/compiler` is absent.

- [ ] **Step 3: Construct worker-local compiler dependencies**

```go
func NewWorkerCompilerFactory(manifest *capability.Manifest, effective *config.EffectiveConfig,
	materializer secret.Materializer) (*WorkerCompilerFactory, error) {
	if manifest == nil || effective == nil || materializer == nil { return nil, ErrInvalidWorkerCompilerInput }
	return &WorkerCompilerFactory{manifest: manifest, config: effective,
		materializer: materializer, resources: runtime.NewResourceRegistry()}, nil
}

func (f *WorkerCompilerFactory) NewGeneration(ctx context.Context, ticket generation.ApplyTicket,
	onFailure func(runtime.TaskFailure)) (*Compiler, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed { return nil, ErrWorkerCompilerFactoryClosed }
	secrets, err := secret.NewGenerationCapability(f.materializer, ticket.DesiredRevision)
	if err != nil { return nil, err }
	tasks := runtime.NewTaskRegistry(ctx, onFailure)
	compiler, err := New(f.manifest, runtime.RuntimeDependencies{
		Config: f.config, Secrets: secrets, Resources: f.resources, Tasks: tasks,
	})
	if err != nil {
		_, stopErr := tasks.Stop(context.WithoutCancel(ctx))
		return nil, errors.Join(err, stopErr)
	}
	return compiler, nil
}
```

`panicMaterializer` is a compiler-test implementation whose `Materialize` panics if called; the generation-scope rejection happens before delegation. `Close` locks, marks `closed`, unlocks and calls `resources.Close(ctx)`; it never closes a generation task registry because each successful `PreparedGeneration` owns that registry. The exec worker passes the plan 05 scoped IPC materializer, not an IPC pointer to the supervisor, local service, resolver callback or raw keyring. `New` validates dependencies and stores the exact manifest/dependency values plus non-nil `compileHTTP` and `compileStream` function fields.

- [ ] **Step 4: Implement strict normalization from snapshot bytes**

```go
func normalize(snapshot generation.Snapshot) (normalizedInput, []resourceIssue, error) {
	input := newNormalizedInput(snapshot.Revision())
	for _, raw := range snapshot.Resources() {
		if err := input.decode(raw.Key, bytes.Clone(raw.Value)); err != nil {
			input.issues = append(input.issues, resourceIssue{Key: raw.Key, Code: "decode-invalid", Err: err})
		}
	}
	for _, tombstone := range snapshot.Tombstones() { input.tombstones[tombstone.Key] = tombstone }
	input.sortAll()
	return input, append([]resourceIssue(nil), input.issues...), nil
}
```

Use exact-number decoding and reject duplicate typed IDs, kind/embedded-ID mismatches and unsupported kinds for the required domain. No decoder may consult `store.Get*`.

- [ ] **Step 5: Build the dependency graph explicitly**

```go
type dependencyGraph struct {
	edges map[generation.ResourceKey][]generation.ResourceKey
}

func (g *dependencyGraph) add(from generation.ResourceKey, to ...generation.ResourceKey) {
	cloned := append([]generation.ResourceKey(nil), to...)
	slices.SortFunc(cloned, compareResourceKey)
	g.edges[from] = slices.Compact(cloned)
}
```

Add edges for route→service/upstream/plugin_config/SSL, service→upstream, plugin config→metadata/secrets where declared, consumer→consumer_group and every protocol-specific referenced resource. Detect cycles with a three-color DFS and return stable `dependency-cycle` decisions.

- [ ] **Step 6: Apply tombstone, quarantine, last-good and fail-closed decisions**

```go
func decideResource(key generation.ResourceKey, issue *resourceIssue,
	desired generation.Snapshot, previous generation.PublishedGeneration,
	securitySensitive bool) resourceDecision {
	if desired.Deleted(key) { return resourceDecision{Key: key, Disposition: generation.DispositionDeleted, Code: "explicit-delete"} }
	if issue == nil { return resourceDecision{Key: key, Disposition: generation.DispositionPublished, Code: "validated"} }
	if securitySensitive {
		if value, ok := previous.Snapshot.Lookup(key); ok {
			return resourceDecision{Key: key, Value: value, Disposition: generation.DispositionLastGood, Code: issue.Code}
		}
		return resourceDecision{Key: key, Disposition: generation.DispositionFailClosed, Code: issue.Code}
	}
	return resourceDecision{Key: key, Disposition: generation.DispositionQuarantined, Code: issue.Code}
}
```

After decisions, recompute each enabled route/stream route closure. If any required dependency is absent, quarantine that owner with `dependency-unavailable`; do not leave a candidate referring to a missing resource.

- [ ] **Step 7: Produce canonical publication candidates**

Build one sorted candidate per ticket-required domain. Candidate closure includes every domain-relevant published, last-good, quarantined, fail-closed and deleted key exactly once, with one matching decision. Candidate snapshots contain only published/last-good values plus tombstones for authoritative deletes.

- [ ] **Step 8: Run pure compiler tests**

Run: `bash -lc 'source .envrc && go test ./pkg/compiler -run "^(TestNormalize|TestValidate|TestDependencyClosure|TestDisposition|TestCompilerRejects|TestWorkerCompilerFactory)" -count=1'`

Expected: PASS for missing dependency, cycle, explicit delete, security first-invalid, security predecessor last-good and HTTP/stream closure isolation.

- [ ] **Step 9: Commit the pure compiler phases**

```bash
git add pkg/compiler pkg/resource
git commit -m "feat(compiler): resolve immutable domain closures"
```

-->

---

### Task 6: Materialize Scoped Plugin Instances and Prepared Generations

> **Execution amendment:** Task 6 also owns `WorkerCompilerFactory`. Expose only atomic `PrepareGeneration`, which stops its new generation task registry on every failure and transfers ownership to `PreparedGeneration` only on success. Do not expose `NewGeneration(...)(*Compiler, error)`.

> **Execution amendment:** The implementation file scope includes every still-live Builder, server and plugin caller affected by the new explicit plugin/secret dependencies, including direct `store.MaterializeSecret` and runtime `store.Get*` consumers. The task must keep the current production owner buildable without a global-secret fallback.

**Files:**

- Create: `pkg/compiler/materialize.go`
- Create: `pkg/compiler/materialize_test.go`
- Modify: `pkg/compiler/types.go`, `compiler.go`
- Modify: `pkg/plugin/base/types.go`, `base/secrets.go`, `types.go`, `init.go`
- Modify: all production plugin files returned by `rg -l 'DataEncryption\(\)|MaterializeSecrets\(\)' pkg/plugin --glob '*.go' --glob '!*_test.go'`
- Modify: `pkg/plugin/secret_materialization_guard_test.go`

**Interfaces:**

- Consumes: validated plugin sources, manifest `Descriptor`, `runtime.RuntimeDependencies`, `secret.Materializer` and `runtime.ResourceRegistry`.
- Produces: exact reduced plugin interfaces, scoped secret injection, digest-keyed plugin leases and `PreparedGeneration.Close` ownership.

- [ ] **Step 1: Write the failing admission-order test**

```go
func TestMaterializePluginRunsAdmissionBeforeSecretsAndPostInit(t *testing.T) {
	trace := []string{}
	plugin := &phaseSpyPlugin{trace: &trace}
	prepared, err := materializePlugin(context.Background(), materializeRequest{
		Descriptor: testDescriptor("http-logger", pluginpkg.PhaseLog),
		Source: normalizedPluginSource{Key: generation.ResourceKey{Kind: "routes", ID: "r1"},
			Factory: "http-logger", Config: map[string]any{"token": "$ENV://TOKEN"}},
		Dependencies: testRuntimeDependencies(t, &trace),
		Factory: func(pluginpkg.Dependencies) pluginpkg.Plugin { return plugin },
	})
	if err != nil { t.Fatal(err) }
	defer prepared.Release(context.Background())
	want := []string{"init", "schema", "decode", "pre-materialization", "secret", "post-init", "bind"}
	if !slices.Equal(trace, want) { t.Fatalf("trace = %v, want %v", trace, want) }
}
```

- [ ] **Step 2: Run materialization tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/compiler -run "^TestMaterialize" -count=1'`

Expected: FAIL because `materializePlugin` and the prepared lease do not exist.

- [ ] **Step 3: Reduce the universal plugin contract**

Use the exact `Plugin`, `Initializer`, `PostInitializer`, `Middleware`, `Stopper` and `ObserverStarter` interfaces from Stable Interfaces. `plugin.New` accepts explicit dependencies and does not call lifecycle hooks:

```go
type Dependencies struct {
	Config  *config.EffectiveConfig
	Secrets secret.Materializer
	Tasks   *runtime.TaskRegistry
}

func New(name string, deps Dependencies) Plugin {
	factory, ok := pluginRegistry[name]; if !ok { return nil }
	p := factory()
	if receiver, ok := p.(dependencyReceiver); ok { receiver.SetDependencies(deps) }
	return p
}
```

This task deletes `base.Dependencies.DataEncryption` and `BasePlugin.DataEncryption()`. It consumes plan 02's `data_encryption.Service` only through `secret.NewMaterializer`; no plugin may retain the raw service/resolver.

- [ ] **Step 4: Implement exact plugin admission order**

```go
func initializePlugin(ctx context.Context, request materializeRequest, p plugin.Plugin) error {
	if initializer, ok := p.(plugin.Initializer); ok { if err := initializer.Init(); err != nil { return stageError("init", err) } }
	if err := request.Schema.Validate(request.Source.Config); err != nil { return stageError("schema", err) }
	if err := util.Parse(request.Source.Config, p.Config()); err != nil { return stageError("decode", err) }
	if validator, ok := p.(plugin.PreMaterializationValidator); ok {
		if err := validator.ValidatePreMaterialization(); err != nil { return stageError("pre-materialization", err) }
	}
	if err := plugin.MaterializePluginSecrets(ctx, request.Scope, request.Dependencies.Secrets, p); err != nil {
		return stageError("secret", err)
	}
	if post, ok := p.(plugin.PostInitializer); ok { if err := post.PostInit(); err != nil { return stageError("post-init", err) } }
	return nil
}
```

- [ ] **Step 5: Acquire plugin instances by effective identity**

Use `plugin.NewInstanceKey` to derive a canonical digest and translate it to `runtime.ResourceKey{Kind: "plugin-instance", Scope: ..., Digest: ...}`. The resource factory constructs, admits and starts the instance once; its close function stops its task owner before invoking `Stopper.Stop`.

- [ ] **Step 6: Precompile consumer and consumer-group bindings**

Build an immutable map keyed by `plugin.ConsumerCacheKey` from the candidate snapshot. Request-time authentication selects from that map; it never reads `store.GetConsumerGroup`, initializes a plugin or mutates a generation cache.

- [ ] **Step 7: Make partial prepare cleanup deterministic**

Append every acquired lease to `PreparedGeneration.leases` immediately and append every bounded probe to `probes`. On any later error, cancel the generation task registry, wait, and release leases in reverse order. Implement matching discard and the single close path exactly:

```go
func (p *PreparedGeneration) DiscardPrepared(ctx context.Context, set generation.PublicationSet) error {
	if !equalPublicationSet(p.publication, set) { return ErrPreparedSetMismatch }
	return p.Close(ctx)
}

func (p *PreparedGeneration) Close(ctx context.Context) error {
	p.closeOnce.Do(func() {
		var errs []error
		if p.tasks != nil {
			residuals, err := p.tasks.Stop(ctx)
			if err != nil { errs = append(errs, err) }
			if len(residuals) != 0 { errs = append(errs, fmt.Errorf("prepared generation residual tasks: %v", residuals)) }
		}
		for i := len(p.leases) - 1; i >= 0; i-- {
			if err := p.leases[i].Release(ctx); err != nil { errs = append(errs, err) }
		}
		p.closeErr = errors.Join(errs...)
	})
	return p.closeErr
}
```

Add a test where the third plugin fails `PostInit` and the first two resource close callbacks each execute exactly once. Add `TestPreparedGenerationDiscardPreparedMatchesSetAndClosesOnce`: eight concurrent calls with the exact set return the stored close result and stop tasks/release each lease once; a mismatched desired revision or domain artifact returns `ErrPreparedSetMismatch` and leaves the candidate open. Also assert `PublicationSet`, `HTTP`, `Stream` and TLS accessors return defensive copies/clones and `Probe` runs the immutable `probes` slice without changing ownership.

- [ ] **Step 8: Enforce the secret boundary with an AST test**

Extend `pkg/plugin/secret_materialization_guard_test.go` to reject imports of `pkg/data_encryption` and calls to `store.ResolveSecretReference` from production plugin packages. Allow only `pkg/secret` types supplied through `base.Dependencies`.

- [ ] **Step 9: Run materialization and affected plugin tests**

```bash
bash -lc 'source .envrc && go test -race ./pkg/compiler ./pkg/plugin -run "^(TestMaterialize|TestPreparedGeneration|TestSecret|TestDescriptor|TestInstance)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/ai_rate_limiting ./pkg/plugin/clickhouse_logger ./pkg/plugin/csrf ./pkg/plugin/elasticsearch_logger ./pkg/plugin/error_log_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/http_logger ./pkg/plugin/kafka_logger ./pkg/plugin/kafka_proxy ./pkg/plugin/lago ./pkg/plugin/loggly ./pkg/plugin/response_rewrite ./pkg/plugin/rocketmq_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/tencent_cloud_cls -run "(Secret|Resolve|PostInit|Materialize|Config)" -count=1'
```

Expected: PASS; no plugin imports or retains a raw global/static encryption resolver.

- [ ] **Step 10: Commit scoped plugin materialization**

```bash
git add pkg/compiler pkg/plugin pkg/secret pkg/runtime
git commit -m "refactor(plugin): own scoped materialization and instances"
```

---

### Task 7: Compile an Immutable HTTP Snapshot

**Files:**

- Create: `pkg/route/compiler.go`
- Create: `pkg/route/router.go`
- Create: `pkg/route/plugin_compile.go`
- Create: `pkg/route/upstream_compile.go`
- Create: `pkg/route/compiler_test.go`
- Create: `pkg/compiler/http.go`
- Modify: `pkg/compiler/compiler.go`, `types.go`
- Move from: `pkg/route/builder.go` (`routeRegistrar` through `matchesWildcardRoute`, plugin source merge, handler assembly, upstream handler assembly)
- Modify tests: `pkg/route/uri_test.go`, `route_parity_test.go`, `scoped_rewrite_test.go`, `consumer_access_test.go`, `builder_lifecycle_test.go`, `public_api_test.go`

**Interfaces:**

- Consumes: normalized dependency-closed HTTP resources, pre-materialized bindings and leased upstream clusters.
- Produces: `route.CompileHTTP(context.Context, CompileInput) (*Snapshot, error)` and immutable `compiler.HTTPSnapshot`.

- [ ] **Step 1: Write a failing immutable input/equivalence test**

```go
func TestCompileHTTPDoesNotObserveInputMutation(t *testing.T) {
	input := compileInputWithRoute(t, "r1", "/before")
	snapshot, err := CompileHTTP(context.Background(), input); if err != nil { t.Fatal(err) }
	input.Routes[0].Uri = "/after"
	assertStatus(t, snapshot.Handler(), http.MethodGet, "/before", http.StatusNoContent)
	assertStatus(t, snapshot.Handler(), http.MethodGet, "/after", http.StatusNotFound)
}

func TestHTTPSnapshotTLSConfigReturnsClone(t *testing.T) {
	snapshot := &HTTPSnapshot{artifact: generation.GenerationArtifact{Domain: generation.DomainHTTP,
		Revision: 7, Digest: sha256.Sum256([]byte("http-7"))}, tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	first := snapshot.TLSConfig()
	first.MinVersion = tls.VersionTLS13
	if got := snapshot.TLSConfig().MinVersion; got != tls.VersionTLS12 { t.Fatalf("MinVersion = %x", got) }
}
```

- [ ] **Step 2: Run HTTP compiler tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/route -run "^TestCompileHTTP" -count=1'`

Expected: FAIL because `CompileHTTP`, `CompileInput` and `Snapshot` do not exist.

- [ ] **Step 3: Define the focused route compiler contract**

```go
type CompileInput struct {
	Revision         uint64
	Routes           []resource.Route
	Services         map[string]resource.Service
	Upstreams        map[string]resource.Upstream
	SSLs             map[string]resource.SSL
	GlobalBindings   []plugin.Binding
	RouteBindings    map[string][]plugin.Binding
	ConsumerBindings map[plugin.ConsumerCacheKey][]plugin.Binding
	PublicAPI        *public_api.Registry
	Dependencies     runtime.RuntimeDependencies
}

type Snapshot struct { revision uint64; handler http.Handler }
func (s *Snapshot) Revision() uint64 { return s.revision }
func (s *Snapshot) Handler() http.Handler { return s.handler }
```

The constructor deep-clones maps/slices and rejects a nil public API registry or runtime dependency.

- [ ] **Step 4: Move router matching without semantic edits**

Move `routeRegistrar`, wildcard dispatcher, URI conversion, host matching and method selection to `router.go`. Preserve existing tests and add a deterministic equal-priority tie test based on the normalized route order.

- [ ] **Step 5: Replace builder store lookups with immutable maps**

`CompileHTTP` resolves services/upstreams/SSLs only from `CompileInput`. Replace `resolveConsumerBindings` with a lookup over the immutable precompiled binding map. A missing precompiled consumer identity is a request-time authorization/config error, never a lazy initialization path.

- [ ] **Step 6: Acquire every upstream resource during prepare**

Move reverse-proxy construction to `upstream_compile.go`; it receives an already-acquired cluster lease and never creates a cluster or starts health tasks itself. Traffic-split uses the same `ResourceRegistry` identity rules and appends its leases to the prepared generation.

- [ ] **Step 7: Preserve request/response phase equivalence**

Run the existing scoped rewrite, auth/CORS, response, logger, transparent upgrade, public API and protocol-terminal tests against `CompileHTTP`. Compile the complete SNI/certificate selector from immutable SSL inputs into one owned `*tls.Config`; never retain mutable input maps or certificate slices. Replace test-only Builder calls with `compileHTTPForTest`; do not keep a Builder constructor for tests.

- [ ] **Step 8: Wrap the route snapshot in `compiler.HTTPSnapshot`**

```go
func compileHTTP(ctx context.Context, artifact generation.GenerationArtifact,
	input route.CompileInput) (*HTTPSnapshot, error) {
	routes, err := route.CompileHTTP(ctx, input)
	if err != nil { return nil, err }
	tlsConfig, err := compileTLSConfig(input.SSLs)
	if err != nil { return nil, err }
	return &HTTPSnapshot{artifact: artifact, handler: routes.Handler(), tlsConfig: tlsConfig}, nil
}

func (s *HTTPSnapshot) Revision() uint64 { return s.artifact.Revision }
func (s *HTTPSnapshot) Handler() http.Handler { return s.handler }
func (s *HTTPSnapshot) TLSConfig() *tls.Config {
	if s.tlsConfig == nil { return nil }
	return s.tlsConfig.Clone()
}
```

- [ ] **Step 9: Run focused HTTP compilation tests**

Run: `bash -lc 'source .envrc && go test ./pkg/compiler ./pkg/route ./pkg/plugin -run "^(TestCompileHTTP|TestCompilerHTTP|TestRoute|TestScoped|TestConsumer|TestPublicAPI|TestResponsePlan|TestRequestPipeline)" -count=1'`

Expected: PASS; no request handler uses a mutable compiler or store getter.

- [ ] **Step 10: Commit immutable HTTP compilation**

```bash
git add pkg/compiler pkg/route pkg/plugin
git commit -m "refactor(route): compile immutable HTTP snapshots"
```

---

### Task 8: Compile an Immutable Stream Router Snapshot

> **Execution amendment:** Before the joint cutover, land only additive detached router/snapshot/compiler work. Do not remove `Router.Reload`, change listener/runtime installation ownership, or route the legacy event path through the new compiler. Those destructive steps execute with Task 9 in the single joint cutover.

**Files:**

- Create: `pkg/stream/snapshot.go`
- Create: `pkg/stream/snapshot_test.go`
- Create: `pkg/compiler/stream.go`
- Modify before the joint cutover: `pkg/stream/router.go` (add detached compilation without removing the live `Router.Reload` path)
- Modify during the joint cutover: `pkg/stream/runtime.go`, `router.go` (install immutable ownership and remove `Router.Reload`)
- Modify: `pkg/stream/router_test.go`, `runtime_test.go`
- Modify: `pkg/compiler/compiler.go`, `types.go`, `compiler_test.go`

**Interfaces:**

- Consumes: normalized dependency-closed stream routes, services/upstreams and enabled stream plugin descriptors.
- Produces: immutable `stream.Router`, `compiler.StreamSnapshot`; no listener is opened during compilation.

- [ ] **Step 1: Write the failing immutable stream test**

```go
func TestCompileRouterDoesNotObserveInputMutation(t *testing.T) {
	routes := []resource.StreamRoute{streamRoute("r1", "127.0.0.1:19001")}
	router, err := CompileRouter(routes, []string{"mqtt-proxy"}, nil); if err != nil { t.Fatal(err) }
	routes[0].ID = "mutated"
	if got := router.RouteIDs(); !slices.Equal(got, []string{"r1"}) { t.Fatalf("RouteIDs() = %v", got) }
}

func TestStreamSnapshotRetainsArtifactIdentity(t *testing.T) {
	artifact := generation.GenerationArtifact{Domain: generation.DomainStream, Revision: 8,
		Digest: sha256.Sum256([]byte("stream-8"))}
	snapshot, err := compileStream(context.Background(), artifact, nil, nil, nil)
	if err != nil { t.Fatal(err) }
	if snapshot.artifact != artifact || snapshot.Revision() != 8 { t.Fatalf("snapshot = %#v", snapshot) }
}
```

- [ ] **Step 2: Run stream snapshot tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/stream -run "^TestCompileRouter" -count=1'`

Expected: FAIL because `CompileRouter` and `RouteIDs` do not exist.

- [ ] **Step 3: Build complete route entries before publishing a router**

```go
func CompileRouter(routes []resource.StreamRoute, enabled []string, onResult func(Result)) (*Router, error) {
	entries, err := compileRouteEntries(cloneStreamRoutes(routes), enabled)
	if err != nil { return nil, err }
	return &Router{routes: entries, enabledPlugins: setOf(enabled), onResult: onResult}, nil
}
```

Before the joint cutover, `CompileRouter` constructs a detached router whose owned inputs cannot change; the still-live legacy runtime may retain `Router.mu` and `Router.Reload`. During the joint cutover, remove both and make `Serve` read immutable entries without locks. The legacy runtime must never receive a compiler-produced detached router before that cutover.

- [ ] **Step 4: Separate listener runtime from the compiled snapshot**

Defer this step to the joint cutover. There, change stream runtime construction to receive listeners plus an immutable router. It may install a new router only through the generation activation boundary; it does not mutate an existing router.

- [ ] **Step 5: Produce `compiler.StreamSnapshot` without binding**

```go
func compileStream(ctx context.Context, artifact generation.GenerationArtifact,
	routes []resource.StreamRoute, enabled []string, onResult func(stream.Result)) (*StreamSnapshot, error) {
	if err := ctx.Err(); err != nil { return nil, err }
	router, err := stream.CompileRouter(routes, enabled, onResult)
	if err != nil { return nil, err }
	return &StreamSnapshot{artifact: artifact, router: router}, nil
}

func (s *StreamSnapshot) Revision() uint64 { return s.artifact.Revision }
func (s *StreamSnapshot) Router() *stream.Router { return s.router }
```

- [ ] **Step 6: Run stream compiler and race tests**

Run before the joint cutover: `bash -lc 'source .envrc && go test -race ./pkg/compiler ./pkg/stream -run "^(TestCompilerStream|TestCompileRouter|TestRouter)" -count=1'`

Expected before the joint cutover: PASS for detached compilation and input isolation. The runtime-install and absence-of-`Reload` cases run in Task 9, where concurrent connections must observe exactly one immutable router each.

- [ ] **Step 7: Commit immutable stream compilation**

```bash
git add pkg/compiler pkg/stream
git commit -m "refactor(stream): compile immutable router snapshots"
```

---

### Task 9: Jointly Cut Journal and Immutable Runtime Over and Delete Legacy Owners

> **Execution amendment:** This is the permanent and only `GenerationEngine` implementation owner. Execute it in one worktree with Durable Task 9's provider, startup/recovery, acknowledgement and legacy-store deletion obligations. Do not first land the durable plan's temporary engine. The authoritative combined scope and gate are in `2026-08-24-journal-immutable-cutover-reorder.md`.

**Files:**

- Modify: `pkg/server/generation_engine.go`
- Modify: `pkg/server/generation_engine_test.go`
- Modify: `pkg/server/server.go` (`Server` construction, recovery and runtime ownership)
- Modify: `pkg/server/route_handler.go`, `route_handler_test.go`
- Modify: `pkg/server/reload.go`, `reload_test.go`
- Modify: `pkg/server/stream_test.go`
- Modify: `pkg/generation/coordinator.go`, `coordinator_test.go` only for the accepted activation rollback/finalize extension
- Delete: `pkg/route/builder.go`
- Delete: `pkg/proxy/registry.go`, `pkg/proxy/registry_test.go`, `pkg/proxy/registry_metrics_test.go`
- Delete obsolete Builder-only tests/helpers after equivalent compiler tests exist

**Interfaces:**

- Consumes: `compiler.Compiler`, `compiler.PreparedGeneration`, `generation.PublicationEngine` including read-only `ConfirmActive`, journal publication token/set and recovered `generation.PublishedGeneration` artifacts.
- Produces: the only production path `Coordinator → Compiler.Prepare → Journal.Stage → GenerationEngine.Activate → Journal.Commit → activation finalize`; no builder/runtime dual path.

- [ ] **Step 1: Write the failing journal-commit rollback test**

```go
func TestGenerationEngineRollsBackActivatedGenerationWhenJournalCommitFails(t *testing.T) {
	old := preparedHTTPGeneration(t, 20, http.StatusOK)
	next := preparedHTTPGeneration(t, 21, http.StatusCreated)
	engine := newGenerationEngineWithActive(t, old)
	journal := newFailingCommitJournal(errors.New("disk full"))
	coordinator := generation.NewCoordinator(journal, engine)
	_, err := coordinator.Apply(context.Background(), desiredBatchFor(next))
	if err == nil { t.Fatal("Apply() error = nil") }
	assertActiveHTTPStatus(t, engine, http.StatusOK)
	assertPreparedClosed(t, next)
	assertPreparedOpen(t, old)
}
```

Add `TestGenerationEngineRollsBackPartiallyActivatedGenerationWhenActivateFails`, where HTTP is switched before stream activation fails, and assert old HTTP/stream owners are restored, the new generation is closed, and the staged token is aborted. Add activation- and commit-error cases where rollback and abort also fail; assert `errors.Is` finds all three errors. In every failure case assert `FinalizeActivation` was not called. In the successful case assert it is called exactly once with a non-cancelled cleanup context, updates `active`, records the exact publication identity, deletes the activation record and enqueues the predecessor exactly once; before it returns there is no IPC, close, task wait or drain call. Add committed-replay cases where `ConfirmActive` accepts only the exact HTTP/stream artifact fence installed by finalize or startup recovery, rejects a predecessor/missing/partial fence with `generation.ErrActiveGenerationMismatch`, respects context cancellation, and causes no compiler, activation or retirement work.

Add `TestCoordinatorDiscardsPreparedGenerationWhenStageFails`: prepare a candidate with one running generation task and two counted leases, make `Journal.Stage` return `errStage`, and assert `DiscardPrepared` is called once with the exact set, the task stops, leases close in reverse order, no activation/commit/abort occurs, and `errors.Is` preserves both `errStage` and a forced discard error.

Add zero-domain normal-publication tests for first commit, commit failure plus same-cursor retry, and committed replay. `GenerationEngine.Prepare` must return a synthetic empty set without invoking `Compiler.Prepare`; Activate/Rollback/Finalize must perform no snapshot swap, close or retirement, while successful finalize sets the initialized fence and replay calls only `ConfirmActive`.

- [ ] **Step 2: Run activation tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/generation ./pkg/server -run "^(TestCoordinator|TestGenerationEngine)" -count=1'`

Expected: FAIL until `GenerationEngine` implements the plan 03 reversible lifecycle and the tests prove partial-activation rollback, staged-token abort, joined cleanup errors and context-bearing finalization.

- [ ] **Step 3: Implement the accepted activation transaction extension**

Use the exact cross-plan signatures approved in the total plan/plan 03 documentation update. Plan 03 owns the complete coordinator state machine. Before `ApplyDesired` or any comparison of incoming batch bytes, it must call `LoadAcknowledgement(ctx, batch.Cursor)`: a committed marker reconstructs the exact required-domain publication set from the acknowledgement and verified published generations, calls read-only `ConfirmActive`, and returns the stored acknowledgement without ApplyDesired/Prepare/Stage/Activate/Commit/Finalize. This intentionally supports an incremental watch batch replayed after restart as a different full-snapshot representation with the same committed provider cursor. A missing marker enters only the following normal publication branch, where `ApplyDesired` retains exact digest conflict detection; any other marker or active-fence error fails closed and never falls back to publication. The normal branch is:

```go
set, err := c.engine.Prepare(ctx, ticket, desired, previous)
if err != nil { return Acknowledgement{}, err }
token, err := c.journal.Stage(ctx, ticket, set)
if err != nil {
	cleanupCtx := context.WithoutCancel(ctx)
	discardErr := c.engine.DiscardPrepared(cleanupCtx, set)
	return Acknowledgement{}, errors.Join(err, discardErr)
}
if err := c.engine.Activate(ctx, token, set); err != nil {
	cleanupCtx := context.WithoutCancel(ctx)
	rollbackErr := c.engine.RollbackActivation(cleanupCtx, token, set)
	abortErr := c.journal.Abort(cleanupCtx, token, stableAbortCode("activation", err))
	return Acknowledgement{}, errors.Join(err, rollbackErr, abortErr)
}
ack, err := c.journal.Commit(ctx, token)
if err != nil {
	cleanupCtx := context.WithoutCancel(ctx)
	rollbackErr := c.engine.RollbackActivation(cleanupCtx, token, set)
	abortErr := c.journal.Abort(cleanupCtx, token, stableAbortCode("commit", err))
	return Acknowledgement{}, errors.Join(err, rollbackErr, abortErr)
}
c.engine.FinalizeActivation(context.WithoutCancel(ctx), token, set)
return ack, nil
```

`Activate` may have switched one domain before returning an error, so both activation-error and commit-error branches first call idempotent `RollbackActivation`, then abort the staged token, and preserve the primary, rollback and abort errors with `errors.Join`. For a non-empty publication, `FinalizeActivation` is a non-failing, non-blocking local ownership handoff after durable commit: under the engine mutex it sets `active = activation.next`, deletes the activation record and appends a non-nil predecessor to the retiring queue, then returns. For the synthetic empty publication it only deletes the activation record and initializes the active fence; it never changes `active` or the retirement queue. It performs no IPC, `Close`, task wait or drain. The temporary server main loop owns asynchronous queue drain; plan 05 atomically moves that queue and its drain ownership into the worker lifecycle during supervisor cutover. Any impossible finalize invariant panics and therefore terminates the worker.

- [ ] **Step 4: Retain prepared generations by immutable publication identity**

```go
type preparedKey struct { Desired uint64; HTTP, Stream [32]byte }

type activation struct {
	token generation.PublicationToken
	next *compiler.PreparedGeneration
	previous *compiler.PreparedGeneration
}
```

For a non-empty publication, `Prepare` stores one prepared generation keyed by revision/domain digests and returns only its defensive-copy `PublicationSet`; `Activate` removes it from pending, probes it, installs the new HTTP/stream snapshots under one engine mutex and records `activation`. For an empty publication, Prepare stores only the synthetic identity and Activate only binds its token, with no compiler or owner work. Neither path closes nor drains `previous` before commit.

- [ ] **Step 5: Finalize or rollback the activation**

Implement `DiscardPrepared(context.Context, generation.PublicationSet) error`, `RollbackActivation(context.Context, generation.PublicationToken, generation.PublicationSet) error`, `FinalizeActivation(context.Context, generation.PublicationToken, generation.PublicationSet)` and `ConfirmActive(context.Context, generation.PublicationSet) error` with the accepted publication lifecycle. `Prepare` special-cases an empty required-domain set before calling the compiler and stores a synthetic pending record with no prepared generation or leases. Its discard, activation and rollback only move/delete that record. `FinalizeActivation` for the synthetic record sets the initialized fence but does not replace the active generation or enqueue retirement. For non-empty sets, `DiscardPrepared` locks the engine, finds and removes only the exact pending `preparedKey`, unlocks, then calls `PreparedGeneration.DiscardPrepared`; repeated exact discard is idempotent, while an unknown/mismatched set returns `ErrPreparedSetMismatch` without touching another candidate. Non-empty `FinalizeActivation` sets the new prepared generation active, updates the defensive-copy identity for each domain named by the publication while retaining untouched independently active domain identities, deletes the matching activation and enqueues the predecessor under the same mutex; it does not initiate drain work. `ConfirmActive` checks the requested domain subset exactly against those active per-domain identities under the same mutex, permits additional independently active domains, and performs no compilation or owner mutation. Recovery installs every verified recovered domain identity regardless of its independent revision and sets a separate initialized fence, including for an empty set. The temporary server main loop asynchronously consumes the retiring queue, and plan 05 replaces that owner atomically with its worker drain loop. `RollbackActivation` restores both previous snapshots under the same mutex and closes the rejected new generation. Unknown activation token/set/digest combinations are core invariant panics.

- [ ] **Step 6: Recover only committed published artifacts**

At startup, compile each verified `RecoveryState.Published` domain into one prepared recovery generation before connecting to the provider. Install all successfully verified domain owners and their exact artifact identities even when their revisions differ; set the separate initialized active fence after the installation succeeds. Never compile `RecoveryState.Desired` for serving. Missing/corrupt required domains remain absent and readiness stays false as defined by plan 03.

- [ ] **Step 7: Replace `routeHandler.Replace` with prepared snapshot activation**

The route handler stores a `*compiler.PreparedGeneration`/HTTP lease. Ordinary dynamic retirement stops accepting new requests but does not close registered hijacked connections; they keep the old generation lease until natural close. Full worker drain in plan 05 applies the bounded deadline and process termination.

- [ ] **Step 8: Delete Builder and mutable stream runtime paths in the same cutover**

Delete the file and all symbols listed in File and Responsibility Map. Move any still-used pure helper before deletion; do not leave a forwarding method.

- [ ] **Step 9: Run the cutover race gate**

Run: `bash -lc 'source .envrc && go test -race ./pkg/generation ./pkg/compiler ./pkg/runtime ./pkg/route ./pkg/stream ./pkg/server -run "^(TestCoordinator|TestCompiler|TestPreparedGeneration|TestGenerationEngine|TestRouteHandler|TestStream)" -count=1'`

Expected: PASS, including prepare failure, activate failure, journal commit failure rollback, commit finalize, concurrent request handoff and natural WebSocket lifetime.

- [ ] **Step 10: Prove the old path is absent**

```bash
test ! -e pkg/route/builder.go
! rg -n 'type Builder|NewBuilder|BuildWithRouteQuarantine|initPluginBindingsStrict|ClusterRegistry|ClusterLease|\.Reload\(\[\]resource\.StreamRoute' cmd pkg --glob '*.go'
! rg -n 'store\.(Get|List|PrepareStreamRoutes|CommitStreamRouteLastGood)' pkg/compiler pkg/route pkg/stream --glob '*.go'
```

Expected: PASS.

- [ ] **Step 11: Commit the atomic production cutover**

```bash
git add pkg/generation pkg/compiler pkg/runtime pkg/route pkg/stream pkg/server pkg/plugin pkg/proxy
git commit -m "refactor(runtime): activate immutable compiled generations"
```

---

### Task 10: Enforce Plugin Panic Boundaries and Exactly-Once Finalization

**Files:**

- Create: `pkg/plugin/panic.go`
- Create: `pkg/plugin/panic_test.go`
- Modify: `pkg/plugin/executor.go`, `response_executor.go`, `streaming_executor.go`, `log_executor.go`
- Modify: `pkg/server/route_handler.go`, `route_handler_test.go`
- Modify: `pkg/apisix/ctx/lifecycle.go`, `lifecycle_test.go`
- Modify: `pkg/observability/metrics/request_panic.go` only to add bounded owner/stage values

**Interfaces:**

- Consumes: descriptor-bearing bindings, `ctx.ResponseOutcome` commit/flush/hijack state and current exactly-once finalizer behavior.
- Produces: `plugin.PanicError`, guarded callbacks, request-local plugin panic recovery and fatal unknown invariant panics.

- [ ] **Step 1: Write failing plugin-vs-core panic tests**

```go
func TestGuardedPluginPanicBeforeCommitReturnsStable500(t *testing.T) {
	binding := panicBinding("request-id", PhaseRewrite)
	handler := guardedMiddleware(binding, http.NotFoundHandler())
	recorder := httptest.NewRecorder()
	serveRouteRequest(recorder, httptest.NewRequest(http.MethodGet, "/", nil), handler)
	if recorder.Code != http.StatusInternalServerError { t.Fatalf("status = %d", recorder.Code) }
}

func TestUnknownRouteInvariantPanicEscapesAfterFinalizer(t *testing.T) {
	finalized := atomic.Int32{}
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("core invariant") })
	defer func() {
		if recovered := recover(); recovered != "core invariant" || finalized.Load() != 1 {
			t.Fatalf("panic/finalizers = %#v/%d", recovered, finalized.Load())
		}
	}()
	serveWithFinalizer(t, handler, &finalized)
}
```

- [ ] **Step 2: Run panic tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/server -run "^(TestGuardedPluginPanic|TestUnknownRouteInvariantPanic)" -count=1'`

Expected: FAIL because current route recovery converts unknown panics to 500.

- [ ] **Step 3: Guard each explicit plugin callback**

```go
type PanicError struct { Factory string; Phase Phase; Value any; Stack []byte }

func guard(factory string, phase Phase, call func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &PanicError{Factory: factory, Phase: phase, Value: recovered, Stack: debug.Stack()}
		}
	}()
	return call()
}
```

Apply it around middleware entry/unwind, request phases, header/body filters, protocol terminals, log callbacks and plugin finalizers. Do not wrap compiler/registry/generation code.

- [ ] **Step 4: Make route recovery type-directed**

If the recovered value is `*plugin.PanicError`, apply the existing stable pre-commit 500 or post-commit/flush/hijack abort behavior. For every other panic, complete outcome, run finalizers, recycle request state and re-panic the original value so the worker dies.

- [ ] **Step 5: Preserve finalizer isolation without masking core panic**

Keep finalizers reverse-ordered and exactly once. Finalizer plugin panics become bounded `FinalizerFailure`; a finalizer marked as a core invariant owner re-panics only after remaining request finalizers complete.

- [ ] **Step 6: Run lifecycle and panic race tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/apisix/ctx ./pkg/plugin ./pkg/server -run "^(TestRequestLifecycle|TestGuardedPluginPanic|TestUnknownRouteInvariantPanic|TestRouteHandlerPanic|TestPluginPhaseClosure)" -count=1'`

Expected: PASS; post-commit plugin panic never writes a second response and a hijacked connection is closed only for that failed request.

- [ ] **Step 7: Commit explicit panic boundaries**

```bash
git add pkg/apisix/ctx pkg/plugin pkg/server pkg/observability/metrics
git commit -m "refactor(runtime): separate plugin and invariant panics"
```

---

### Task 11: Migrate Every Production Goroutine to an Owner

**Files:**

- Create: `pkg/runtime/goroutine_contract_test.go`
- Modify generation-owned tasks: `pkg/proxy/active_health.go`, `pkg/plugin/proxy_cache/disk.go`, `limit_count/delayed_sync.go`, `ai_proxy_multi/health.go`, `oas_validator/plugin.go`, `graphql_proxy_cache/plugin.go`, `log_rotate/plugin.go`, `logger_batch/processor.go`, `file_logger/processor.go`, `file_logger/writer_registry.go`, `ai_stream/flush_writer.go`, `stream/runtime.go`
- Modify request-owned tasks: `pkg/stream/bridge/bridge.go`, `pkg/plugin/batch_requests/plugin.go`, `kafka_proxy/websocket.go`, `kafka_proxy/transport.go`, `proxy_mirror/plugin.go`, `mqtt_proxy/stream.go`, `mcp_bridge/plugin.go`
- Modify shutdown helpers using `WaitGroup.Go`: `pkg/plugin/error_log_logger/plugin.go`, `tcp_logger/plugin.go`, `syslog/transport.go`, `udp_logger/plugin.go`, `datadog/plugin.go`, `sls_logger/plugin.go`, `loggly/plugin.go`
- Modify: `pkg/compiler/materialize.go` to inject exact task owner names

**Interfaces:**

- Consumes: `runtime.TaskRegistry`, `RequestTaskGroup`, plugin `InstanceKey` and request context.
- Produces: no unowned production `go` statement under plugin/proxy/route/stream; cancellation-ignoring work remains visible as a generation residual.

- [ ] **Step 1: Add the failing AST ownership gate**

```go
func TestProductionGoroutinesUseOwnedRuntime(t *testing.T) {
	root := repositoryRoot(t)
	for _, dir := range []string{"pkg/plugin", "pkg/proxy", "pkg/route", "pkg/stream"} {
		walkGoFiles(t, filepath.Join(root, dir), func(path string, file *ast.File) {
			ast.Inspect(file, func(node ast.Node) bool {
				if _, ok := node.(*ast.GoStmt); ok {
					t.Errorf("unowned go statement: %s", relative(root, path))
				}
				return true
			})
		})
	}
}
```

Exclude `_test.go` and `pkg/runtime/task_registry.go`/`request_tasks.go`; do not add package-specific allowlists.

- [ ] **Step 2: Run the gate and capture the exact current file list**

Run: `bash -lc 'source .envrc && go test ./pkg/runtime -run "^TestProductionGoroutinesUseOwnedRuntime$" -count=1'`

Expected: FAIL listing every production `go` statement enumerated in Files above.

- [ ] **Step 3: Convert generation/plugin background loops**

Replace each long-lived `go` with the injected registry:

```go
if err := p.Tasks().Go(runtime.TaskSpec{
	Owner: "plugin/" + p.InstanceKey().String() + "/health",
	Criticality: runtime.TaskPlugin,
}, func(ctx context.Context) error { return p.healthLoop(ctx) }); err != nil {
	return fmt.Errorf("start health task: %w", err)
}
```

Loops select on the supplied context and return. `Stopper.Stop` becomes cancellation/join through the owning prepared generation; plugin-specific `Stop` remains only for closing non-task resources.

- [ ] **Step 4: Convert request-owned concurrent work**

Use `NewRequestTaskGroup(request.Context(), owner)` in batch subrequests, proxy mirror, Kafka/MQTT/MCP bridges and stream copy directions. Call `Wait` before the request/connection owner returns. If the externally visible timeout returns first, keep its dispatch lease until the task group finishes so generation drain reports the owner.

- [ ] **Step 5: Convert bounded shutdown helpers**

Replace local `WaitGroup.Go` close/send helpers with a named task in the plugin registry or synchronous close under an explicit caller deadline. Do not spawn a detached goroutine solely to hide a blocking `Close`.

- [ ] **Step 6: Run the ownership gate and focused concurrency tests**

```bash
bash -lc 'source .envrc && go test -race ./pkg/runtime ./pkg/proxy ./pkg/stream ./pkg/plugin/batch_requests ./pkg/plugin/kafka_proxy ./pkg/plugin/mqtt_proxy ./pkg/plugin/proxy_mirror ./pkg/plugin/mcp_bridge ./pkg/plugin/logger_batch ./pkg/plugin/limit_count ./pkg/plugin/ai_proxy_multi -run "(Task|Ownership|Cancel|Shutdown|Drain|Bridge|Timeout|Health|Batch)" -count=1'
```

Expected: PASS; the AST gate reports no unowned production `go` statement.

- [ ] **Step 7: Commit goroutine ownership**

```bash
git add pkg/runtime pkg/proxy pkg/route pkg/stream pkg/plugin pkg/compiler
git commit -m "refactor(runtime): assign every goroutine an owner"
```

---

### Task 12: Document and Verify the Immutable Runtime Cutover

**Files:**

- Create: `docs/architecture/immutable-compiler-runtime.md`
- Modify: `docs/design.md`
- Modify: `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program.md` only if the accepted activation interface amendment requires it
- Verify: all paths changed by Tasks 1–11

**Interfaces:**

- Consumes: final compiler/runtime/plugin signatures and the durable journal state machine.
- Produces: the accepted `compiler.Compiler`, `compiler.PreparedGeneration`, `runtime.RuntimeDependencies`, `runtime.ResourceRegistry` and `runtime.TaskRegistry` interfaces required by supervisor, HTTP closure, runtime-safety and stream plans.

- [ ] **Step 1: Write the architecture document from implemented names**

Document this exact phase chain and state boundary:

```text
generation.Snapshot
  -> normalize (clone, exact decode)
  -> validate (schema and compatibility, no side effects)
  -> resolve (HTTP/stream dependency closure and decisions)
  -> materialize (scoped secrets only)
  -> prepare (digest-keyed resource leases and named tasks)
  -> compile (immutable HTTP handler and stream router)
  -> probe (bounded, not live)
  -> stage/activate/commit/finalize through generation.Coordinator
```

Include the resource identity tuple `(kind, APISIX scope, canonical effective-config digest)`, plugin instance-scope table, task criticality policy, plugin/core panic boundary, finalizer order, activation rollback and no-legacy-adapter rule.

- [ ] **Step 2: Inventory every moved or deleted symbol**

Run:

```bash
git diff --name-status $(git merge-base HEAD origin/master)..HEAD
rg -n 'type Builder|NewBuilder|BuildWithRouteQuarantine|initPluginBindingsStrict|ClusterRegistry|ClusterLease|requestStageRegistry|capabilityRegistry|Router\.Reload|streamRuntimeOwner.*Reload' cmd pkg --glob '*.go'
```

Expected: the second command prints no definition or call site.

- [ ] **Step 3: Scan production and tests for stale call sites and proxy-only facades**

```bash
rg -n 'BuildStrict|Build\(|NewBuilder|RequestStageFor|ResolveRequestStage|CapabilitySpecFor|ResolveResponsePhases|ResolveBeforeProxyOwner|GetPriority\(\).*compare|DataEncryption\(\)|store\.(Get|List)' pkg/compiler pkg/route pkg/plugin pkg/server --glob '*.go'
```

Expected: no removed runtime API remains. `GetPriority` may remain only as a concrete plugin compatibility method if a focused plugin test directly verifies its public configuration; it must not influence runtime ordering.

- [ ] **Step 4: Run the complete impact-scoped compiler gate**

```bash
bash -lc 'source .envrc && go test -race ./pkg/generation ./pkg/secret ./pkg/runtime ./pkg/compiler ./pkg/route ./pkg/plugin ./pkg/proxy ./pkg/stream ./pkg/server ./pkg/apisix/ctx -run "^(TestCoordinator|TestMaterializer|TestRuntimeDependencies|TestResourceRegistry|TestTaskRegistry|TestRequestTaskGroup|TestCompiler|TestNormalize|TestDependencyClosure|TestMaterialize|TestPreparedGeneration|TestCompileHTTP|TestCompileRouter|TestDescriptor|TestNewInstanceKey|TestGenerationEngine|TestRouteHandler|TestRequestLifecycle|TestProductionGoroutines)" -count=1'
```

Expected: PASS without retries or skipped package output.

- [ ] **Step 5: Run scoped lint and build**

```bash
bash -lc 'source .envrc && golangci-lint run ./pkg/generation/... ./pkg/secret/... ./pkg/runtime/... ./pkg/compiler/... ./pkg/route/... ./pkg/plugin/... ./pkg/proxy/... ./pkg/stream/... ./pkg/server/... ./pkg/apisix/ctx/...'
bash -lc 'source .envrc && make build'
git diff --check
```

Expected: PASS. Record any pre-existing failure with exact package, file, line and message; never describe a skipped/failing gate as passing.

- [ ] **Step 6: Verify Windows remains source-buildable at this boundary**

Run: `bash -lc 'source .envrc && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...'`

Expected: PASS, or only the already-recorded unrelated platform failure with exact package/file/line. New compiler/runtime/secret packages import no Unix-only API.

- [ ] **Step 7: Check scope and untracked-file preservation**

Run: `git status --short && git diff --check`

Expected: only plan-authorized implementation/docs paths are changed; the four pre-existing untracked review files remain byte-for-byte untouched.

- [ ] **Step 8: Commit the architecture record**

```bash
git add docs/architecture/immutable-compiler-runtime.md docs/design.md docs/superpowers/plans/2026-08-23-apisix-go-convergence-program.md
git commit -m "docs(runtime): specify immutable compiler ownership"
```

Skip the commit when Tasks 1–11 and the documentation update leave no diff; never create an empty commit.

## Self-Review Results

- **Spec coverage:** Tasks 1–2 establish explicit runtime, secret and task dependencies; Task 3 generalizes equal-config resource reuse; Task 4 makes manifest phase/priority/scope/identity authoritative; Tasks 5–8 implement normalize, validate, closure, materialize, prepare, HTTP/stream compile and probe inputs; Task 9 publishes and rolls back immutable generations while deleting Builder; Task 10 separates plugin and core panic policy while retaining exactly-once finalizers; Task 11 assigns every current production goroutine an owner; Task 12 performs moved/deleted-symbol, stale-call-site, race, lint, build and platform audits.
- **No-adapter coverage:** `route.Builder`, `ClusterRegistry`, mutable `Router.Reload`, handwritten request/capability registries and their proxy-only helpers are removed in the same task that switches the final production caller. Inert compiler foundations may land earlier, but there is never a selectable second production runtime.
- **Dependency consistency:** `RuntimeDependencies` has the exact total-plan fields. Plan 02's immutable `data_encryption.Service` is consumed only to construct `secret.Materializer`, and raw plugin access is removed atomically. Compiler receives previous state only as `generation.PublishedGeneration`; journal staging/commit remains with coordinator/engine. Prepared generation outputs are the exact inputs expected by plans 05–09.
- **Type consistency:** `ResourceKey` is used by `Acquire` and `ResourceLease`; `TaskSpec` is used by `TaskRegistry.Go`; plugin `Descriptor` and `InstanceKey` are stored on every `Binding`; `Compiler.Prepare` always returns `*PreparedGeneration`; `PublicationSet`, `HTTP`, `Stream`, `Probe` and `Close` use identical names in every later task.
- **Completeness scan:** The plan contains no deferred implementation marker, unspecified test request, generic error-handling instruction or undefined neighboring interface. Every code-producing task names concrete files, functions, red/green commands, expected outcomes and commit scope.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`. Execute its corrected waves after Durable Journal Tasks 1–8 and use its Task 9 only as the shared production cutover, following `2026-08-24-journal-immutable-cutover-reorder.md`. Use `superpowers:subagent-driven-development` (recommended, fresh implementation worker plus review between tasks) or `superpowers:executing-plans` (inline batches with checkpoints).
