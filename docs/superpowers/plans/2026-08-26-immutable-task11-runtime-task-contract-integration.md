# Immutable Task11 Runtime Task Contract Integration Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add one immutable named-task binding contract, inject exact plugin task owners during effective binding materialization, integrate the generation/request ownership migrations, and make a repository AST gate reject every remaining unowned production goroutine.

**Architecture:** `runtime.TaskRegistry` remains the only generation task registry and `RequestTaskGroup` remains the only request/connection join primitive. A new concrete `runtime.TaskOwner` binds one registry, immutable owner prefix, and immutable criticality; the compiler derives plugin prefixes from the already-validated `selected.instance`, while shared proxy/file-writer resources and the persistent stream runtime use their own lifecycle-local core registries rather than borrowing a generation registry. The final AST gate scans production syntax without package allowlists and rejects both raw `go` statements and syntactic `sync.WaitGroup.Go` admissions outside the two canonical runtime primitives.

**Tech Stack:** Go 1.26 standard library (`context`, `go/ast`, `go/parser`, `go/token`, `io/fs`, `path/filepath`, `sync`), existing `runtime.TaskRegistry`, `runtime.RequestTaskGroup`, compiler effective binding materialization, focused/race Go tests, golangci-lint, and the repository build.

**Spec:** Task 11, lines 1961-2038 of `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`; generation migration plan `docs/superpowers/plans/2026-08-26-immutable-task11-generation-background-ownership.md`; request migration plan `docs/superpowers/plans/2026-08-26-immutable-task11-request-concurrency-ownership.md`.

## Global Constraints

- Execute from Task11 base `b0220dcebd64a1d2d687be84d1f14ab501dfffd0` or a reviewed descendant containing no partial Task11 migration.
- Source `.envrc` and set `GOFLAGS=-mod=readonly` before every Go or Make command.
- Use impact-scoped tests. Do not run `go test ./...`, `go test ./pkg/...`, `make test`, or the full integration suite as routine verification.
- Add no dependency and no feature flag, package allowlist, legacy goroutine path, hidden background context, process-global panic channel, or runtime owner resolver.
- `runtime.RuntimeDependencies.Tasks` remains `*runtime.TaskRegistry`; it is the private generation registry owned by `PreparedGeneration`. Only plugin-facing `base.Dependencies.Tasks` changes to `*runtime.TaskOwner`.
- `RequestTaskGroup` keeps exactly its current constructor and methods. Internally it joins accepted children before re-panicking the first raw panic value from `Wait`; do not add `Cancel`, `WaitContext`, `Active`, `Residuals`, `Owner`, a public panic hook, or a bridge into `TaskRegistry`.
- A `TaskPlugin` panic/error fails only its exact full owner name; a `TaskCore` panic remains unrecovered and worker-fatal.
- `PreparedGeneration.Close` stops its task registry in the existing `cleanupQuiesce` phase before any plugin/resource release. A deadline returns sorted, deduplicated full owner names from `TaskRegistry.Stop`; a later `Stop` may complete after the tasks exit.
- Shared resources must follow their actual lifecycle. A factory-wide shared `proxy.Cluster`, the process-shared file-writer registry, and the persistent stream runtime must not attach work to the first generation that happens to construct or use them.
- Do not edit or stage the four user-owned untracked review documents under `docs/reviews/`.
- This plan owns only the shared runtime/compiler contract, the error-log observer seam, cross-plan integration, and the final AST gate. Generation/background production migrations belong to the generation plan; request/connection migrations belong to the request plan.
- No push or PR is authorized. Each implementation task may create a local reviewed commit for integration.

---

## Current-Source Findings at the Frozen Base

1. There is no `pkg/compiler/materialize.go`. The exact injection point is `pkg/compiler/effective_binding_materializer.go` in `acquireEffectiveBinding`, where `selected.instance` has already been validated before `base.Dependencies` is constructed.
2. `base.Dependencies.Tasks` and `BasePlugin.TaskRegistry()` currently expose the raw generation registry. No production plugin except `error_log_logger.StartObservingWithTasks` currently registers through it.
3. `TaskRegistry.Stop` already cancels once, waits to the caller deadline, reports sorted/deduplicated active owners, rejects new admission after stop, and permits a later successful stop. These semantics must not be reimplemented in plugins.
4. `RequestTaskGroup` already closes admission when `Wait` begins and joins ordinary accepted task completion, but a raw child panic currently escapes its wrapper immediately and can kill the process before sibling join/lease cleanup. Task11 must cache the first raw panic identity, finish all accepted children, and re-panic it from `Wait`. Its `owner` remains validation-only and it deliberately has no deadline-return path.
5. The four target directories currently contain 32 raw production `go` statements in 20 files and nine production `sync.WaitGroup.Go` calls. The raw inventory additionally reveals `pkg/plugin/rocketmq_logger/plugin.go`, which the stale top-level Files list omitted.
6. `proxy.Cluster` is interned in a factory-wide `runtime.ResourceRegistry` by configuration digest and can be shared by N and N+1. Binding active-health work to N's `PreparedGeneration.tasks` would cancel a cluster still leased by N+1.
7. `file_logger.sharedFileWriters` is process-shared and starts/stops its signal watcher as the path registry transitions between zero/non-zero leases. `stream.Runtime` also outlives individual prepared generations and switches routers through a lease source. Both require lifecycle-local core registries.

---

## Frozen Runtime Contract

### Exact concrete API

Add this concrete type; do not add an interface with one implementation:

```go
// package runtime
var (
	ErrTaskRegistryRequired = errors.New("task registry is required")
	ErrTaskComponentInvalid = errors.New("task component is invalid")
)

type TaskOwner struct {
	registry    *TaskRegistry
	prefix      string
	criticality TaskCriticality
}

func NewTaskOwner(
	registry *TaskRegistry,
	prefix string,
	criticality TaskCriticality,
) (*TaskOwner, error)

func (owner *TaskOwner) Go(
	component string,
	run func(context.Context) error,
) error
```

`NewTaskOwner` rejects a nil registry with `ErrTaskRegistryRequired`, a blank/outer-whitespace prefix with `ErrTaskOwnerRequired`, and a criticality other than `TaskPlugin` or `TaskCore` with `ErrTaskCriticalityInvalid`. It stores the exact prefix without trimming or later mutation. It does not inspect registry stop state; actual admission remains atomic inside `TaskRegistry.Go`.

`TaskOwner.Go` accepts a component only when it is 1-64 ASCII bytes, begins and ends with `[a-z0-9]`, and every interior byte is `[a-z0-9-]`. It rejects uppercase, whitespace, `/`, empty components, leading/trailing `-`, and values longer than 64 bytes with `ErrTaskComponentInvalid`. A valid call delegates exactly once:

```go
return owner.registry.Go(TaskSpec{
	Owner:       owner.prefix + "/" + component,
	Criticality: owner.criticality,
}, run)
```

The wrapper has no `Stop`, `Context`, `Active`, `Prefix`, criticality setter, rebind, or child-owner method. The registry remains the sole cancellation/join authority.

### Why not inject `TaskRegistry + ownerPrefix`

Two raw dependency fields would force every plugin to reconstruct `TaskSpec`, repeat criticality, and concatenate owner names. That permits a plugin to relabel itself as `TaskCore`, omit the compiler-derived instance, or drift component spelling. `TaskOwner` adds only three immutable fields and one delegating method, but removes those invalid states. It is a concrete value, not speculative polymorphism.

### Exact naming and criticality

| Lifecycle owner | Prefix | Component examples | Criticality | Stop owner |
| --- | --- | --- | --- | --- |
| Materialized outer plugin | `plugin/` + `selected.instance.String()` | `observer`, `health-refresh`, `disk-cleanup`, `delayed-sync`, `spec-refresh`, `rotation`, `batch-worker`, `file-log-writer` | `TaskPlugin` | `PreparedGeneration.tasks.Stop` |
| Composite child | inherits the outer plugin `TaskOwner` | the child's fixed function component | `TaskPlugin` | outer plugin resource lease / prepared generation |
| Shared HTTP cluster | `core/proxy-cluster/` + lower-case hex `proxy.ClusterKey` | `active-health` | `TaskCore` | final cluster resource close |
| Process-shared file writer registry | `core/file-writer-registry` | `signal-watch` | `TaskCore` | zero-writer transition; a later first lease creates a fresh registry |
| Persistent stream listener runtime | `core/stream-runtime` | `listener`, `connection` | `TaskCore` | `stream.Runtime.Close(ctx)` |

The plugin prefix names the resource/lifecycle owner, not whichever nested Go value executes the callback. Composite children have no independent resource-registry lease, so inheriting the outer immutable owner is intentional; do not derive a late child owner after `PostInit` and do not mutate an already-injected owner.

### Stop, drain, and residual semantics

- Each accepted task increments the existing registry count for its full name. Multiple workers with the same prefix/component are counted but appear once in `Active` and deadline residuals.
- Generation stop cancels all generation task callbacks, then waits. Plugin `Stop` closes non-task resources only; it must not launch a waiter, add a task after registry stop, or wait a second time for registry-owned goroutines.
- A callback that ignores cancellation remains in `Active` and appears as its exact full `TaskResidual.Owner`. Resource release does not run before generation task quiescence.
- A shared resource's local registry stops only at that resource's real final close. Its residual is returned through that close as a cleanup failure; it is not falsely reported as a residual of an earlier generation that released a non-final lease.
- A `TaskPlugin` failure marks only the full prefix/component failed. A sibling component under the same plugin prefix may still start; this preserves the current registry's exact-owner failure semantics.

### RequestTaskGroup boundary

The production method set does not change, but its internal panic timing must change:

```go
type RequestTaskGroup struct {
	ctx   context.Context
	owner string

	mu         sync.Mutex
	waiting    bool
	errs       []error
	panicked   bool
	panicValue any
	wg         sync.WaitGroup

	validationErr error
}
```

Each task wrapper recovers a raw panic only long enough to record the first recovered value under `mu` and call `wg.Done`. `Wait` waits for all accepted callbacks, snapshots `panicked`, `panicValue`, and `errs` under `mu`, then re-panics the cached value before considering `errors.Join`. Repeated and concurrent `Wait` calls all re-panic the same cached identity. With no panic, existing error joining is unchanged. The group must not convert a panic to an error, log/serialize it, or select a later panic over the first value that acquired the record lock.

Call sites must additionally obey all of these rules:

1. Construct it with the exact request/connection parent context and a fixed bounded owner string such as `request/batch-requests`, `request/proxy-mirror`, `connection/stream-bridge`, `connection/kafka-proxy`, `connection/mqtt-proxy`, or `connection/mcp-bridge`.
2. Admit all children before `Wait`; an `ErrTaskGroupWaiting` result is a call-site lifecycle bug, not a retry signal.
3. On externally visible timeout, cancel the parent/derived context, decide the response/connection result, and still call `Wait` before the handler/connection owner returns and before its generation/dispatch lease is released.
4. Do not recover unknown panics at the call site. `RequestTaskGroup` performs only join-before-repanic bookkeeping. The Task10 nested route boundary has already converted recoverable plugin failures; an unknown core panic in a child is re-thrown unchanged from `Wait` and remains process-fatal after sibling cleanup.
5. Do not depend on error ordering from `errors.Join`: concurrent completion order is not an observable contract.

---

## File Responsibility and Cross-Plan Dependencies

| Owner | Files | Output | Dependency |
| --- | --- | --- | --- |
| Contract C, Task 1 | `pkg/runtime/task_registry.go`, `task_registry_test.go`, `request_tasks.go`, `request_tasks_test.go` | additive `TaskOwner` API, exact residual/failure tests, and request join-before-repanic semantics with unchanged method set | none |
| Contract C, Task 2 | `pkg/plugin/base/types.go`, `types_test.go`; `pkg/compiler/effective_binding_materializer.go`, `effective_binding_materializer_test.go`; observer-only seam in `pkg/plugin/error_log_logger/plugin.go`, `plugin_test.go`; `pkg/plugin/composite_preparer_test.go` | raw registry removed from plugin dependencies; compiler-bound exact owner; error-log observer uses component `observer` | Task 1 |
| Generation plan | files named in `2026-08-26-immutable-task11-generation-background-ownership.md` | all generation/shared/core background loops and shutdown helpers migrated | must start from reviewed C Task 2 head; it may edit the remaining error-log shutdown helper only after C is integrated |
| Request plan | files named in `2026-08-26-immutable-task11-request-concurrency-ownership.md` | all request/connection concurrency migrated with unchanged `RequestTaskGroup` API | may run from C Task 1 or later; must not edit C/A files |
| Contract C, Task 5 | create `pkg/runtime/goroutine_contract_test.go` | syntax gate for raw `go` and `sync.WaitGroup.Go` | generation and request outputs integrated |

Do not run C Task 2 and the generation plan from the same base: both must touch `error_log_logger/plugin.go`. C owns `StartObservingWithTasks` and its owner seam first; the generation plan then owns only the remaining shutdown cancellation watcher from C's reviewed head.

---

### Task 1: Add the Immutable TaskOwner Contract and Join-Before-Repanic Semantics

**Files:**
- Modify: `pkg/runtime/task_registry.go`
- Modify: `pkg/runtime/task_registry_test.go`
- Modify: `pkg/runtime/request_tasks.go`
- Modify: `pkg/runtime/request_tasks_test.go`

**Interfaces:**
- Consumes: existing `TaskRegistry.Go(TaskSpec, func(context.Context) error) error`, `TaskPlugin`, `TaskCore`, and validation errors.
- Produces: exact `TaskOwner`, `NewTaskOwner`, `ErrTaskRegistryRequired`, and `ErrTaskComponentInvalid` contract above; unchanged `RequestTaskGroup` method set with first-panic join/replay semantics.

- [ ] **Step 1: Write failing owner validation, failure-isolation, and residual tests**

Add these tests before production code:

```go
func TestTaskOwnerUsesExactPrefixComponentAndPluginFailureIsolation(t *testing.T) {
	failures := make(chan TaskFailure, 1)
	registry := NewTaskRegistry(context.Background(), func(f TaskFailure) { failures <- f })
	owner, err := NewTaskOwner(registry, "plugin/request-id/attempt/scope/route/digest", TaskPlugin)
	if err != nil { t.Fatal(err) }

	wantErr := errors.New("health failed")
	if err := owner.Go("health-refresh", func(context.Context) error { return wantErr }); err != nil {
		t.Fatal(err)
	}
	failure := <-failures
	if failure.Owner != "plugin/request-id/attempt/scope/route/digest/health-refresh" ||
		!errors.Is(failure.Err, wantErr) {
		t.Fatalf("failure = %#v", failure)
	}
	if err := owner.Go("health-refresh", func(context.Context) error { return nil });
		!errors.Is(err, ErrTaskOwnerFailed) {
		t.Fatalf("failed component admission = %v", err)
	}
	done := make(chan struct{})
	if err := owner.Go("disk-cleanup", func(context.Context) error { close(done); return nil }); err != nil {
		t.Fatalf("sibling component admission = %v", err)
	}
	<-done
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskOwnerStopReportsExactDeduplicatedResidual(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	owner, err := NewTaskOwner(registry, "plugin/logger/key", TaskPlugin)
	if err != nil { t.Fatal(err) }
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	for range 2 {
		if err := owner.Go("batch-worker", func(context.Context) error {
			started <- struct{}{}
			<-release
			return nil
		}); err != nil { t.Fatal(err) }
	}
	<-started; <-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	residuals, stopErr := registry.Stop(ctx)
	if !errors.Is(stopErr, context.DeadlineExceeded) || !reflect.DeepEqual(
		residuals, []TaskResidual{{Owner: "plugin/logger/key/batch-worker"}},
	) { t.Fatalf("Stop() = (%v, %v)", residuals, stopErr) }
	close(release)
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("retry Stop() = (%v, %v)", residuals, err)
	}
}
```

Add a table test for nil registry; blank, padded, and empty prefix; invalid criticality; empty/uppercase/slashed/whitespace/leading-hyphen/trailing-hyphen/65-byte component; and nil callback. Assert the exact stable error listed in the frozen contract and assert `registry.Active()` remains empty after every validation failure.

- [ ] **Step 2: Write failing request panic join/replay tests**

Add these tests without adding an exported method:

```go
func TestRequestTaskGroupWaitJoinsSiblingsBeforeRepanickingExactValue(t *testing.T) {
	group := NewRequestTaskGroup(context.Background(), "request/batch-requests")
	wantPanic := &struct{ marker string }{marker: "core-invariant"}
	releaseSibling := make(chan struct{})
	siblingDone := make(chan struct{})
	if err := group.Go(func(context.Context) error { panic(wantPanic) }); err != nil {
		t.Fatal(err)
	}
	if err := group.Go(func(context.Context) error {
		<-releaseSibling
		close(siblingDone)
		return nil
	}); err != nil { t.Fatal(err) }

	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		_ = group.Wait()
	}()
	select {
	case value := <-recovered:
		t.Fatalf("Wait() repanicked before sibling join: %#v", value)
	default:
	}
	close(releaseSibling)
	<-siblingDone
	if got := <-recovered; got != wantPanic {
		t.Fatalf("recovered panic = %#v, want exact %#v", got, wantPanic)
	}
}

func TestRequestTaskGroupRepeatedAndConcurrentWaitReplaySamePanic(t *testing.T) {
	group := NewRequestTaskGroup(context.Background(), "connection/stream-bridge")
	wantPanic := &struct{ marker string }{marker: "bridge-invariant"}
	if err := group.Go(func(context.Context) error { panic(wantPanic) }); err != nil { t.Fatal(err) }

	const waiters = 4
	results := make(chan any, waiters)
	for range waiters {
		go func() {
			defer func() { results <- recover() }()
			_ = group.Wait()
		}()
	}
	for range waiters {
		if got := <-results; got != wantPanic {
			t.Fatalf("concurrent Wait recovered %#v, want exact %#v", got, wantPanic)
		}
	}
}
```

Retain `TestRequestTaskGroupJoinsAcceptedTaskErrors` unchanged to prove the no-panic path still returns `errors.Join`. Retain constructor/callback validation, admission closure, repeatable wait, and no-detached-completion tests.

- [ ] **Step 3: Run RED tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime -run "^(TestTaskOwner|TestRequestTaskGroupWaitJoinsSiblingsBeforeRepanickingExactValue|TestRequestTaskGroupRepeatedAndConcurrentWaitReplaySamePanic)" -count=1'
```

Expected: FAIL to compile because the TaskOwner symbols do not exist; after test compilation is isolated, the first request panic test also fails because the raw child panic terminates the test process before sibling join.

- [ ] **Step 4: Implement the minimal concrete wrapper**

Add the exact type and methods from the frozen contract. Implement component validation with a byte loop; do not add a regexp or dependency:

```go
func validTaskComponent(component string) bool {
	if len(component) == 0 || len(component) > 64 {
		return false
	}
	for index := range len(component) {
		value := component[index]
		alphanumeric := value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
		if alphanumeric {
			continue
		}
		if value != '-' || index == 0 || index == len(component)-1 {
			return false
		}
	}
	return true
}
```

Do not change `TaskRegistry.Go`, `Stop`, `Active`, or generation failure handling.

- [ ] **Step 5: Implement request join-before-repanic internally**

Add the two private fields and helper, then wrap each accepted callback:

```go
func (g *RequestTaskGroup) Go(run func(context.Context) error) error {
	if g.validationErr != nil {
		return g.validationErr
	}
	if run == nil {
		return ErrTaskCallbackRequired
	}
	g.mu.Lock()
	if g.waiting {
		g.mu.Unlock()
		return ErrTaskGroupWaiting
	}
	g.wg.Add(1)
	g.mu.Unlock()
	go func() {
		defer g.wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				g.recordPanic(recovered)
			}
		}()
		g.record(run(g.ctx))
	}()
	return nil
}

func (g *RequestTaskGroup) Wait() error {
	g.mu.Lock()
	g.waiting = true
	g.mu.Unlock()
	g.wg.Wait()

	g.mu.Lock()
	panicked := g.panicked
	panicValue := g.panicValue
	errs := append([]error(nil), g.errs...)
	g.mu.Unlock()
	if panicked {
		panic(panicValue)
	}
	return errors.Join(errs...)
}

func (g *RequestTaskGroup) recordPanic(value any) {
	g.mu.Lock()
	if !g.panicked {
		g.panicked = true
		g.panicValue = value
	}
	g.mu.Unlock()
}
```

Do not turn the panic into an `error`. Do not call `record` for a panicking task. Go 1.26 converts `panic(nil)` to a non-nil runtime panic value, so the recovered identity remains replayable.

- [ ] **Step 6: Run focused normal and race tests**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime -run "^(TestTaskOwner|TestTaskRegistry|TestRequestTaskGroup)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/runtime -run "^(TestTaskOwner|TestTaskRegistry|TestRequestTaskGroup)" -count=1'
```

Expected: PASS. Panic waits for sibling completion, repeated/concurrent `Wait` replays the same identity, and no-panic error joining remains unchanged.

- [ ] **Step 7: Commit the runtime contract**

```bash
git add pkg/runtime/task_registry.go pkg/runtime/task_registry_test.go \
  pkg/runtime/request_tasks.go pkg/runtime/request_tasks_test.go
git commit -m "feat(runtime): bind and join owned tasks"
```

---

### Task 2: Atomically Inject Exact Plugin Owners and Cut Over the Observer Seam

**Files:**
- Modify: `pkg/plugin/base/types.go`
- Modify: `pkg/plugin/base/types_test.go`
- Modify: `pkg/compiler/effective_binding_materializer.go`
- Modify: `pkg/compiler/effective_binding_materializer_test.go`
- Modify: `pkg/plugin/error_log_logger/plugin.go` only for `StartObservingWithTasks` and its immediate owner registration
- Modify: `pkg/plugin/error_log_logger/plugin_test.go` only for observer task admission/lifecycle expectations
- Modify: `pkg/plugin/composite_preparer_test.go`

**Interfaces:**
- Consumes: `runtime.NewTaskOwner`, validated `selected.instance`, and existing effective-binding construction order.
- Produces: `base.Dependencies.Tasks *runtime.TaskOwner`, `BasePlugin.TaskOwner() *runtime.TaskOwner`, `effectiveBindingOps.startObserver func(plugin.Plugin, *runtime.TaskOwner) error`, and `StartObservingWithTasks(*runtime.TaskOwner) error`.
- Provides to generation plan: every outer plugin receives immutable `plugin/<InstanceKey.String()>` with `TaskPlugin` before `Init`, config decode, `PostInit`, or observer start.

- [ ] **Step 1: Write the compiler exact-owner RED test**

Replace the raw-registry assertion in `TestEffectiveBindingMaterializerInjectsExactDependenciesBeforeOuterConstruction` and add a task that proves the injected prefix from observable registry state:

```go
func TestEffectiveBindingMaterializerInjectsExactPluginTaskOwner(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-1")
	defaultNew := prepared.bindingOps.newFactoryInstance
	var captured *runtime.TaskOwner
	prepared.bindingOps.newFactoryInstance = func(
		factory string,
		dependencies base.Dependencies,
	) (plugin.FactoryInstance, error) {
		captured = dependencies.Tasks
		return defaultNew(factory, dependencies)
	}

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if err != nil || len(bindings) != 1 || captured == nil {
		t.Fatalf("materialize owner = (%#v, %v, %v)", bindings, captured, err)
	}
	started := make(chan struct{})
	if err := captured.Go("health-refresh", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return nil
	}); err != nil { t.Fatal(err) }
	<-started
	want := []string{"plugin/" + bindings[0].InstanceKey.String() + "/health-refresh"}
	if got := prepared.tasks.Active(); !reflect.DeepEqual(got, want) {
		t.Fatalf("active owners = %v, want %v", got, want)
	}
	if err := prepared.Close(context.Background()); err != nil { t.Fatal(err) }
}
```

Update base dependency tests to construct two `TaskOwner` values and assert `left.TaskOwner() == leftOwner` and `right.TaskOwner() == rightOwner`. Update the composite test to assert the child inherits the exact outer owner pointer and no raw registry accessor remains.

Update error-log observer tests so a supplied `TaskOwner` with prefix `plugin/<fixture-key>` produces active owner `plugin/<fixture-key>/observer`; nil owner returns the renamed stable error `errObserverTaskOwnerRequired`; stopped registry admission still returns `errObserverTaskRegistration`.

- [ ] **Step 2: Run RED tests**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime ./pkg/plugin/base ./pkg/compiler ./pkg/plugin/error_log_logger ./pkg/plugin -run "(TaskOwner|InjectsExactPluginTaskOwner|PreservesOuterAuthorityAndDependencies|StartObservingWithTasks)" -count=1'
```

Expected: FAIL because `base.Dependencies.Tasks` still accepts a raw registry and compiler/error-log observer signatures still use `*runtime.TaskRegistry`.

- [ ] **Step 3: Change the plugin dependency bundle without a dual path**

Make these exact changes:

```go
// package base
type Dependencies struct {
	Config            *config.EffectiveConfig
	DataEncryption    data_encryption.Resolver
	Secrets           secret.GenerationCapability
	Metadata          runtime.MetadataView
	Consumers         ConsumerLookup
	Tasks             *runtime.TaskOwner
	CompositeChildren CompositeChildPreparer
}

func (p *BasePlugin) TaskOwner() *runtime.TaskOwner {
	return p.dependencies.Tasks
}
```

Delete `BasePlugin.TaskRegistry()` in the same diff. Do not retain a forwarding accessor or add `TaskRegistry` beside `Tasks`. `runtime.RuntimeDependencies.Tasks` is not modified.

- [ ] **Step 4: Bind the owner from the validated InstanceKey before construction**

In `acquireEffectiveBinding`, before the `base.Dependencies` literal, add:

```go
taskOwner, err := runtime.NewTaskOwner(
	prepared.tasks,
	"plugin/"+selected.instance.String(),
	runtime.TaskPlugin,
)
if err != nil {
	return plugin.Binding{}, nil, err
}
dependencies := base.Dependencies{
	Config: prepared.effective, Secrets: prepared.attempt.capability,
	Metadata: prepared.metadata, Consumers: prepared.lookup, Tasks: taskOwner,
}
```

Keep this before `NewCompositeChildPreparer` and `newFactoryInstance`. Do not derive the prefix from `instance.GetName`, route config, current generation state, a global lookup, or the later returned binding.

- [ ] **Step 5: Cut the observer hook to the same owner atomically**

Change only the hook contract and error-log observer registration:

```go
// compiler effectiveBindingOps
startObserver func(plugin.Plugin, *runtime.TaskOwner) error

operations.startObserver = func(instance plugin.Plugin, tasks *runtime.TaskOwner) error {
	observer, ok := instance.(interface {
		StartObservingWithTasks(*runtime.TaskOwner) error
	})
	if !ok { return nil }
	return observer.StartObservingWithTasks(tasks)
}
```

Call `operations.startObserver(instance, taskOwner)` at the existing post-`PostInit` observer point. In error-log logger, accept `*runtime.TaskOwner`, call `tasks.Go("observer", callback)`, retain the existing ready barrier and stop behavior, and rename only the nil-dependency error to `errObserverTaskOwnerRequired`. Delete the old hard-coded `TaskSpec` and raw registry parameter.

- [ ] **Step 6: Run focused normal/race tests and the dependency leak scan**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime ./pkg/plugin/base ./pkg/compiler ./pkg/plugin/error_log_logger ./pkg/plugin -run "(TaskOwner|EffectiveBindingMaterializer|CompositeChildPreparer|StartObservingWithTasks|BasePlugin)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/runtime ./pkg/plugin/base ./pkg/compiler ./pkg/plugin/error_log_logger ./pkg/plugin -run "(TaskOwner|InjectsExactPluginTaskOwner|PreservesOuterAuthorityAndDependencies|StartObservingWithTasks)" -count=1'
rg -n 'TaskRegistry\(\)|StartObservingWithTasks\(\*runtime\.TaskRegistry\)|Tasks:\s*(prepared\.tasks|tasks\b)' pkg/plugin pkg/compiler --glob '*.go'
```

Expected: tests PASS. The scan has no production raw plugin dependency or old observer signature; test fixtures may mention their local registry only when constructing a `TaskOwner`.

- [ ] **Step 7: Commit the atomic compiler/observer cutover**

```bash
git add pkg/plugin/base/types.go pkg/plugin/base/types_test.go \
  pkg/compiler/effective_binding_materializer.go pkg/compiler/effective_binding_materializer_test.go \
  pkg/plugin/error_log_logger/plugin.go pkg/plugin/error_log_logger/plugin_test.go \
  pkg/plugin/composite_preparer_test.go
git commit -m "refactor(compiler): inject exact plugin task owners"
```

After review, this commit is the required base of the generation/background plan. Do not dispatch that plan from `b0220dce`.

---

### Task 3: Integrate Generation, Shared-Resource, and Shutdown Ownership

**Files:**
- Execute and integrate: `docs/superpowers/plans/2026-08-26-immutable-task11-generation-background-ownership.md`
- Verify but do not duplicate its owned production files.

**Interfaces:**
- Consumes: reviewed Task 2 `TaskOwner`, compiler-injected plugin owner, and unchanged `TaskRegistry.Stop` semantics.
- Produces: no raw generation/shared/core background `go` or `WaitGroup.Go`; every long-lived owner stops at its actual lifecycle boundary.
- Provides to Task 5: generation-side raw goroutine inventory is empty.

- [ ] **Step 1: Rebase the generation worktree boundary by recreation, not by overlapping edits**

Create its worktree from the reviewed Task 2 commit. The generation plan may now edit the remaining `error_log_logger.watchConnectionCancellation` helper but must preserve the Task 2 observer signature and component `observer`.

- [ ] **Step 2: Enforce lifecycle classification during implementation review**

Reject the generation output unless all of these are true:

- Outer plugin loops use the compiler-injected `BasePlugin.TaskOwner()` and a fixed component; no plugin reconstructs `TaskSpec` or prefix.
- `proxy.Cluster` owns a fresh resource-local core registry keyed by lower-case hex `ClusterKey`; `NewCluster`/internal constructor receives or constructs that owner before active-health start; final resource close calls `Stop(ctx)`. It never uses `PreparedGeneration.tasks`.
- `fileWriterRegistry` creates a fresh local core registry only for a zero-to-one writer transition and stops it on the one-to-zero transition. Restart creates a new registry; a stopped registry is never reused.
- `stream.Runtime` owns one local core registry for its listener/connection lifetime and `Close(ctx)` stops it. It never changes task owner when `RouterSource` switches generation.
- Logger/file/plugin `Stop` functions close resources synchronously or signal task callbacks; they do not spawn a detached cleanup waiter and do not call `TaskOwner.Go` after generation stop has begun.
- Cancellation-ignoring plugin tasks remain present in the owning registry until their callback actually returns.

- [ ] **Step 3: Run the generation plan's exact RED/GREEN tests**

Use every focused command in the generation plan. At minimum, retain fresh evidence for:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/runtime ./pkg/compiler ./pkg/proxy ./pkg/stream ./pkg/plugin/error_log_logger ./pkg/plugin/logger_batch ./pkg/plugin/file_logger ./pkg/plugin/limit_count ./pkg/plugin/ai_proxy_multi -run "(Task|Owner|Cancel|Shutdown|Drain|Health|Observer|Signal|Batch|Residual)" -count=1'
```

Expected: PASS. Tests must include N/N+1 sharing of one cluster: stopping N's generation registry does not stop the shared active-health task; final cluster release does.

- [ ] **Step 4: Review and integrate generation output**

Review its exact diff against Task 2 head. Reject raw `go`, hidden `context.Background()` ownership, component names built from request data, plugin/core relabeling, or `Stop` that returns before resource cleanup while silently detaching a waiter. Integrate reviewed commits only.

---

### Task 4: Integrate Request and Connection Task Groups Without Expanding Their API

**Files:**
- Execute and integrate: `docs/superpowers/plans/2026-08-26-immutable-task11-request-concurrency-ownership.md`
- Verify: `pkg/runtime/request_tasks.go`, `request_tasks_test.go` remain API-compatible.

**Interfaces:**
- Consumes: Task 1 `NewRequestTaskGroup(context.Context, string) *RequestTaskGroup`, `Go(func(context.Context) error) error`, and join-before-repanic `Wait() error`, with the same exported method set as the base.
- Produces: bounded request/connection concurrency that joins before owner return and lease release.
- Provides to Task 5: request-side raw goroutine inventory is empty.

- [ ] **Step 1: Freeze the public method set before integration**

Add no production method. Record the current declarations:

```bash
rg -n '^func (NewRequestTaskGroup|\(g \*RequestTaskGroup\) (Go|Wait))' pkg/runtime/request_tasks.go
```

Expected: exactly the constructor, `Go`, and `Wait`.

- [ ] **Step 2: Enforce timeout/lease/panic semantics during implementation review**

For batch requests, proxy mirror, Kafka, MQTT, MCP, stream bridge, and AI flush paths, require:

```go
group := runtime.NewRequestTaskGroup(parent, "<fixed-owner>")
if err := group.Go(func(ctx context.Context) error { /* existing child body */ }); err != nil {
	return err
}
// admit remaining children
waitErr := group.Wait()
// only now may the handler/connection owner return and release its lease
```

Timeout selects an externally visible result and cancels the derived context; it does not skip `Wait`. Existing exact `http.ErrAbortHandler` handling remains bounded. An unknown panic is cached only inside the group, siblings join, and `Wait` re-panics the exact value to preserve Task10 fatal behavior after cleanup.

- [ ] **Step 3: Run request plan RED/GREEN and race tests**

Use every focused command in the request plan. At minimum:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/runtime ./pkg/stream/bridge ./pkg/plugin/batch_requests ./pkg/plugin/kafka_proxy ./pkg/plugin/mqtt_proxy ./pkg/plugin/proxy_mirror ./pkg/plugin/mcp_bridge ./pkg/plugin/ai_stream -run "(RequestTaskGroup|Batch|Mirror|Bridge|WebSocket|Transport|Timeout|Cancel|Lease|Panic|Flush)" -count=1'
```

Expected: PASS, including timeout tests that prove the last generation/dispatch lease is released only after all admitted callbacks exit.

- [ ] **Step 4: Prove the RequestTaskGroup method set did not grow**

```bash
git diff b0220dce -- pkg/runtime/request_tasks.go pkg/runtime/request_tasks_test.go
rg -n '^func (NewRequestTaskGroup|\(g \*RequestTaskGroup\) (Go|Wait))' pkg/runtime/request_tasks.go
```

Expected: the production diff contains only private `panicked`/`panicValue` state, `recordPanic`, wrapper recovery, and `Wait` replay from C Task 1. The declaration scan still shows exactly the constructor, `Go`, and `Wait`; the request migration plan adds no further runtime diff.

- [ ] **Step 5: Review and integrate request output**

Reject any local goroutine wrapper, call-site panic suppression, `WaitContext`, timeout path that returns before `Wait`, dynamic request IDs in owner strings, reacquisition of the current generation from a child, or change that converts the cached panic to an ordinary error.

---

### Task 5: Add the No-Allowlist Production Goroutine AST Gate

**Files:**
- Create: `pkg/runtime/goroutine_contract_test.go`

**Interfaces:**
- Consumes: integrated generation and request migrations; canonical goroutine creation remains only in `pkg/runtime/task_registry.go` and `pkg/runtime/request_tasks.go`, which are outside the scanned feature directories.
- Produces: `TestProductionGoroutinesUseOwnedRuntime`, rejecting raw `*ast.GoStmt` and syntactic `sync.WaitGroup.Go` in `pkg/plugin`, `pkg/proxy`, `pkg/route`, and `pkg/stream`.

- [ ] **Step 1: Write the scanner and first run it against the frozen base for RED evidence**

The test must use the test source path, not current working directory, to locate the repository:

```go
package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestProductionGoroutinesUseOwnedRuntime(t *testing.T) {
	root := taskContractRepositoryRoot(t)
	for _, relativeDir := range []string{"pkg/plugin", "pkg/proxy", "pkg/route", "pkg/stream"} {
		dir := filepath.Join(root, relativeDir)
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil { return walkErr }
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil { return err }
			variables, fields := waitGroupSyntax(file)
			ast.Inspect(file, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.GoStmt:
					position := fset.Position(typed.Go)
					t.Errorf("unowned go statement: %s:%d", taskContractRelative(root, path), position.Line)
				case *ast.CallExpr:
					selector, ok := typed.Fun.(*ast.SelectorExpr)
					if ok && selector.Sel.Name == "Go" && isWaitGroupSyntax(selector.X, variables, fields) {
						position := fset.Position(selector.Sel.Pos())
						t.Errorf("unowned sync.WaitGroup.Go: %s:%d", taskContractRelative(root, path), position.Line)
					}
				}
				return true
			})
			return nil
		})
		if err != nil { t.Fatalf("walk %s: %v", relativeDir, err) }
	}
}

func taskContractRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := goruntime.Caller(0)
	if !ok { t.Fatal("locate goroutine contract source") }
	return filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
}

func taskContractRelative(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil { return path }
	return filepath.ToSlash(relative)
}
```

Implement `waitGroupSyntax` without `go/types`: discover the file's actual `sync` import alias; collect identifiers declared as `sync.WaitGroup`, identifiers assigned `sync.WaitGroup{}`, and named struct fields of that type. Use these exact test-private helpers:

```go
func waitGroupSyntax(file *ast.File) (map[string]struct{}, map[string]struct{}) {
	syncAliases := make(map[string]struct{})
	dotSync := false
	for _, imported := range file.Imports {
		if imported.Path.Value != `"sync"` {
			continue
		}
		alias := "sync"
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		switch alias {
		case "_":
			continue
		case ".":
			dotSync = true
		default:
			syncAliases[alias] = struct{}{}
		}
	}
	isWaitGroupType := func(expression ast.Expr) bool {
		if identifier, ok := expression.(*ast.Ident); ok {
			return dotSync && identifier.Name == "WaitGroup"
		}
		selector, ok := expression.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "WaitGroup" {
			return false
		}
		identifier, ok := selector.X.(*ast.Ident)
		if !ok {
			return false
		}
		_, ok = syncAliases[identifier.Name]
		return ok
	}

	variables := make(map[string]struct{})
	fields := make(map[string]struct{})
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.ValueSpec:
			if typed.Type != nil && isWaitGroupType(typed.Type) {
				for _, name := range typed.Names { variables[name.Name] = struct{}{} }
			}
		case *ast.AssignStmt:
			for index, right := range typed.Rhs {
				literal, ok := right.(*ast.CompositeLit)
				if !ok || !isWaitGroupType(literal.Type) || index >= len(typed.Lhs) { continue }
				if name, ok := typed.Lhs[index].(*ast.Ident); ok { variables[name.Name] = struct{}{} }
			}
		case *ast.StructType:
			for _, field := range typed.Fields.List {
				if !isWaitGroupType(field.Type) { continue }
				for _, name := range field.Names { fields[name.Name] = struct{}{} }
			}
		}
		return true
	})
	return variables, fields
}

func isWaitGroupSyntax(
	expression ast.Expr,
	variables map[string]struct{},
	fields map[string]struct{},
) bool {
	switch typed := expression.(type) {
	case *ast.Ident:
		_, ok := variables[typed.Name]
		return ok
	case *ast.SelectorExpr:
		_, ok := fields[typed.Sel.Name]
		return ok
	case *ast.ParenExpr:
		return isWaitGroupSyntax(typed.X, variables, fields)
	case *ast.StarExpr:
		return isWaitGroupSyntax(typed.X, variables, fields)
	default:
		return false
	}
}
```

Do not flag arbitrary `.Go` calls, because `TaskOwner.Go` is the required owned admission. Do not add a file/package exception when a new syntax shape appears; extend the structural detector and add a focused parser fixture inside `goroutine_contract_test.go`.

On frozen base `b0220dce`, the first run must report 32 raw statements across these exact 20 files:

```text
pkg/plugin/ai_proxy_multi/health.go
pkg/plugin/ai_stream/flush_writer.go
pkg/plugin/batch_requests/plugin.go
pkg/plugin/file_logger/processor.go
pkg/plugin/file_logger/writer_registry.go
pkg/plugin/graphql_proxy_cache/plugin.go
pkg/plugin/kafka_proxy/transport.go
pkg/plugin/kafka_proxy/websocket.go
pkg/plugin/limit_count/delayed_sync.go
pkg/plugin/log_rotate/plugin.go
pkg/plugin/logger_batch/processor.go
pkg/plugin/mcp_bridge/plugin.go
pkg/plugin/mqtt_proxy/stream.go
pkg/plugin/oas_validator/plugin.go
pkg/plugin/proxy_cache/disk.go
pkg/plugin/proxy_mirror/plugin.go
pkg/plugin/rocketmq_logger/plugin.go
pkg/proxy/active_health.go
pkg/stream/bridge/bridge.go
pkg/stream/runtime.go
```

It must additionally report nine `sync.WaitGroup.Go` calls in `stream/runtime.go`, `mqtt_proxy/stream.go`, and the seven logger cancellation helpers (`error_log_logger`, `tcp_logger`, `syslog`, `udp_logger`, `datadog`, `sls_logger`, `loggly`).

- [ ] **Step 2: Run the gate on the integrated Task11 head**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime -run "^TestProductionGoroutinesUseOwnedRuntime$" -count=1'
```

Expected: PASS with no package/file allowlist and no production feature goroutine exception.

- [ ] **Step 3: Run the scanner and task registry under race**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/runtime -run "^(TestProductionGoroutinesUseOwnedRuntime|TestTaskOwner|TestTaskRegistry|TestRequestTaskGroup)" -count=1'
```

Expected: PASS.

- [ ] **Step 4: Commit the governance gate**

```bash
git add pkg/runtime/goroutine_contract_test.go
git commit -m "test(runtime): reject unowned production goroutines"
```

---

### Task 6: Run Cross-Plan Integration, Review, and Completion Gates

**Files:**
- Verify: every file changed by Tasks 1-5 and both sibling Task11 plans
- Modify only to repair a confirmed Task11 integration defect in the owning file; do not perform cleanup outside the Task11 diff

**Interfaces:**
- Consumes: reviewed C contract, generation plan, request plan, and AST gate commits.
- Produces: one locally integrated Task11 head ready for the parent Task11 merge decision.

- [ ] **Step 1: Record the exact integrated identity and diff scope**

```bash
git status --short --branch
git log --oneline --decorate -12
git diff --stat b0220dce..HEAD
git diff --check b0220dce..HEAD
```

Expected: only Task11-owned files plus plan documents; diff check is clean. Preserve all user-owned untracked files.

- [ ] **Step 2: Run the ownership and lifecycle integration gate**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race \
  ./pkg/runtime ./pkg/compiler ./pkg/proxy ./pkg/stream ./pkg/stream/bridge \
  ./pkg/plugin/base ./pkg/plugin/error_log_logger ./pkg/plugin/logger_batch ./pkg/plugin/file_logger \
  ./pkg/plugin/limit_count ./pkg/plugin/ai_proxy_multi ./pkg/plugin/batch_requests \
  ./pkg/plugin/kafka_proxy ./pkg/plugin/mqtt_proxy ./pkg/plugin/proxy_mirror \
  ./pkg/plugin/mcp_bridge ./pkg/plugin/ai_stream \
  -run "(Task|Owner|Ownership|Cancel|Shutdown|Drain|Residual|Health|Observer|Signal|Batch|Mirror|Bridge|Timeout|Lease|Panic|Flush|ProductionGoroutines)" -count=1'
```

Expected: PASS with no race. If the regex selects no tests in a package, record that package and run its exact test names from the sibling plan rather than treating `[no tests to run]` as evidence.

- [ ] **Step 3: Run lint and build**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run \
  ./pkg/runtime/... ./pkg/compiler/... ./pkg/proxy/... ./pkg/stream/... \
  ./pkg/plugin/base/... ./pkg/plugin/error_log_logger/... ./pkg/plugin/logger_batch/... \
  ./pkg/plugin/file_logger/... ./pkg/plugin/limit_count/... ./pkg/plugin/ai_proxy_multi/... \
  ./pkg/plugin/batch_requests/... ./pkg/plugin/kafka_proxy/... ./pkg/plugin/mqtt_proxy/... \
  ./pkg/plugin/proxy_mirror/... ./pkg/plugin/mcp_bridge/... ./pkg/plugin/ai_stream/...'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
```

Expected: lint reports zero issues and build succeeds.

- [ ] **Step 4: Run deletion, call-site, and hidden-owner scans**

```bash
rg -n --glob '*.go' --glob '!**/*_test.go' '\bgo\s+(func\s*\(|[A-Za-z_][A-Za-z0-9_.]*\s*\()' \
  pkg/plugin pkg/proxy pkg/route pkg/stream
rg -n --glob '*.go' --glob '!**/*_test.go' 'sync\.WaitGroup|WaitGroup\.Go|\.wg\.Go\(|\bwait\.Go\(|\bactive\.Go\(' \
  pkg/plugin pkg/proxy pkg/route pkg/stream
rg -n 'TaskRegistry\(\)|StartObservingWithTasks\(\*runtime\.TaskRegistry\)|TaskSpec\{Owner:\s*"plugin/' \
  pkg/plugin pkg/compiler --glob '*.go'
rg -n 'NewRequestTaskGroup|func \(g \*RequestTaskGroup\)' pkg/runtime/request_tasks.go
```

Expected: first three commands return no production legacy ownership. The last command shows only the unchanged constructor, `Go`, and `Wait` methods.

- [ ] **Step 5: Perform independent merge-level review**

Review `b0220dce..HEAD` for:

- exact compiler prefix derived from `selected.instance`, not mutable plugin state;
- no generation registry captured by shared cluster/file-writer/stream owners;
- `TaskPlugin` versus `TaskCore` classification from the naming table;
- stop-before-release and exact residual behavior;
- request timeout still waits before generation/dispatch lease release;
- unknown child panic joins owned siblings, re-panics with exact identity, remains fatal, and leaves Task10 `ErrAbortHandler` behavior unchanged;
- no raw goroutine, `WaitGroup.Go`, proxy-only compatibility method, unused stop channel, dead wait group, or test-only production helper.

Any finding is repaired in its owning sibling plan/worktree, re-reviewed, then reintegrated. Do not patch an overlapping integration file opportunistically.

- [ ] **Step 6: Create the final local Task11 integration commit if integration itself changed files**

If cherry-picks are already discrete and Task 6 required no repair, do not create an empty commit. Otherwise:

```bash
git add <exact-reviewed-Task11-files>
git commit -m "refactor(runtime): assign every goroutine an owner"
```

- [ ] **Step 7: Hand off exact evidence to the parent**

Return:

- integrated head SHA and commit list;
- AST base failure count (32 raw `go`, nine `WaitGroup.Go`) and final PASS;
- normal/race/lint/build command results with durations;
- final `TaskOwner` API and plugin/core owner naming table;
- any residual-risk evidence, especially shared-resource close behavior;
- explicit statement that `RequestTaskGroup` exported method set did not expand and its only semantic change is join-before-repanic;
- whether any broad tests were not run (expected: broad repository and integration aggregations were not run unless separately requested).

---

## Self-Review Checklist

- **Spec coverage:** Task 1 owns the named task primitive; Task 2 injects exact plugin names; Tasks 3-4 migrate generation/request/shutdown ownership; Task 5 enforces the no-raw-goroutine rule; Task 6 runs race/lint/build and merge review.
- **Current-source accuracy:** the plan names `effective_binding_materializer.go`, not nonexistent `materialize.go`; uses `BasePlugin.TaskRegistry()` only as a deletion target; and includes the extra rocketmq raw goroutine found at the base.
- **Lifecycle accuracy:** cross-generation shared cluster, process-shared file writers, and persistent stream runtime are not assigned to a prepared generation.
- **API restraint:** no `RequestTaskGroup` method is added; its private panic state only delays fatal propagation until join. `TaskOwner` is concrete and has only constructor plus `Go`.
- **No placeholders:** every production signature, owner prefix, component rule, test command, dependency edge, and final gate is fixed above.
- **Type consistency:** compiler, base plugin, error-log observer, composite dependency propagation, and generation plan all consume the same `*runtime.TaskOwner` type.
