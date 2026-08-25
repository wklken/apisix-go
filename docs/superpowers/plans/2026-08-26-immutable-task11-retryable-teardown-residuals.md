# Immutable Task 11 Retryable Teardown and Residual Propagation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Use `superpowers:test-driven-development` for every behavior change and `superpowers:verification-before-completion` before each commit.

**Goal:** Make generation, factory, engine, server, and shared-resource teardown retryable so a bounded task residual never releases dependent resources, while terminal cleanup still runs exactly once and retains every unrelated cleanup error.

**Architecture:** `runtime.TaskRegistry.Stop` keeps its existing `([]TaskResidual, error)` signature and wraps deadline or cancellation causes in one structured residual error that remains discoverable through `errors.As` and preserves `errors.Is`. Compiler cleanup becomes a strict two-phase state machine: retry pending quiescers until all succeed, then execute every release exactly once. Prepared generations, factories, generation owners, the generation engine, server shutdown, and the shared resource registry advance only across terminal phase boundaries; an incomplete attempt retains ownership for a later caller-driven retry and never spins.

**Tech Stack:** Go 1.26 standard library (`context`, `errors`, `slices`, `sync`), existing `runtime.TaskRegistry`, compiler `cleanupStack` and `PreparedGeneration`, `WorkerCompilerFactory`, server `generationOwner` and `GenerationEngine`, `runtime.ResourceRegistry`, focused Go tests, race detector, golangci-lint, and repository build targets.

**Spec:** Task 11 in `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`; total execution plan `docs/superpowers/plans/2026-08-26-immutable-task11-total-execution.md`; generation ownership plan `docs/superpowers/plans/2026-08-26-immutable-task11-generation-background-ownership.md`.

## Global Constraints

- Stable dependency label: **`Task11-0 / retryable teardown and residual propagation`**. Generation-owned and shared-resource Task 11 implementation starts only after this complete plan and Contract C Task 2 are reviewed and integrated.
- The request/connection plan may proceed after Contract C Task 1. It does not depend on Task11-0 except where its AI streaming task is already serialized behind the generation AI-health task by the request plan.
- Start from reviewed Task 11 planning head `167c11c451573df7e521f73c7d7863990bb4c3ea`, whose product-code base is `b0220dcebd64a1d2d687be84d1f14ab501dfffd0`. Re-establish the exact integration SHA before implementation; do not treat this planning worktree as an implementation base after other Task 11 commits land.
- Preserve `TaskRegistry.Stop(ctx) ([]TaskResidual, error)`. Do not add `Done`, `Wait`, `Complete`, `Retry`, status, callback, or channel methods to the residual error or task registry.
- The structured residual error exposes only a stable `Error()`, `Unwrap()`, and defensive `Residuals()`. Its string never includes owner names, raw resource IDs, plugin configuration, or callback errors.
- A quiesce failure is incomplete, even when its cause is not a context error. No release callback may run until every quiescer in the relevant cleanup set has succeeded.
- A release callback is attempted exactly once. Its error is terminal, retained, joined with other terminal release errors, and replayed on later calls without invoking the callback again.
- Preserve existing safe-marker/redaction contracts: `PreparedGeneration` and `WorkerCompilerFactory` may add the structured residual carrier to their error chain, but must not expose raw provider, registration, plugin, resource, or resolver cleanup errors that current tests require them to redact.
- Add one safe compiler sentinel, `compiler.ErrPreparedGenerationCleanupIncomplete`, because `cleanupStack` treats every quiesce error as retryable even when it is not a task/context error. Cross-package owners use `errors.Is` on this sentinel; no completion/status method is added to `PreparedGeneration`.
- Preserve unrelated terminal cleanup errors. An incomplete residual returned by one attempt must not overwrite terminal errors already collected from independent owners; after retry reaches terminal completion, those errors remain discoverable with `errors.Is` where the existing API already promises that behavior.
- No retry loop may immediately re-invoke a failed close. Retries occur only on a later public close/discard/shutdown call or after an existing internal completion barrier changes state. No ticker, sleep, backoff, or polling goroutine is added.
- `ResourceRegistry` admits no replacement for a key while its previous entry is closing. An acquisition waits on the entry's internal terminal-close barrier or the registry-close barrier, honors its own context, and never increments the closing entry's references or calls a replacement factory early.
- Run Go commands from the repository root as `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && ...'`. Do not run `go test ./...`, `go test ./pkg/...`, `make test`, or a broad integration suite.
- No dependency changes, push, PR, release, or unrelated cleanup is authorized. Every implementation task below ends at a local commit boundary.

---

## Current-Source Failure Map

| Boundary | Current source behavior | Required behavior |
| --- | --- | --- |
| `runtime.TaskRegistry.Stop` | Returns structured residuals separately, but the error is only `ctx.Err`; callers that convert the error to a safe marker lose residual identity. | Return the same residual slice and a stable typed carrier wrapping the context cause; `errors.Is` and `errors.As` both survive wrapping and joins. |
| `compiler.cleanupStack.Close` | `sync.Once` makes the first deadline terminal and runs release callbacks even after quiesce fails. | Seal once, retry only unfinished quiescers, prohibit release until all quiescers succeed, then run every release once and cache terminal release errors. |
| `compiler.cleanupStack.Rollback` | Removes checkpoint-owned steps before running them and releases after a quiesce error. | Retain a failed suffix, retry it or let final generation close own it, and truncate only after its quiesce barrier and releases finish. |
| `PreparedGeneration.Close` | Sets terminal, caches the first result, clears fields, revokes snapshots, and detaches even when task stop times out. | Hide observations immediately, but retain snapshots/resources/cleanup/factory membership across incomplete attempts; clear and detach only after cleanup becomes terminal. |
| `WorkerCompilerFactory.Close` | Caches the first result and closes the shared registry after any generation close result. | Reject new preparation once, retry live generations in stable order, and close the shared registry only after all live generations are terminal. |
| `generationOwner.closePrepared` | `closeOnce` caches the first prepared close result and closes `closeDone` even when cleanup is incomplete. | Retry after drain; close `closeDone` and clear ownership only on terminal prepared close. |
| `GenerationEngine.DiscardPrepared` | Deletes the pending record after the first cleanup attempt, including a residual timeout. | Replay one in-flight attempt to current waiters, retain the pending record after incomplete cleanup, and allow a later exact-set retry. |
| `GenerationEngine` retirement | Removes owners and stream metrics after any close result; `Close` uses `sync.Once` and unbounded waits. | Retain incomplete owners and metrics, wait with the caller context, retry only on a later attempt or existing barrier progress, and terminally close factory last. |
| `Server.shutdownAttempt` | Marks `engineClosed` and advances the phase after any engine error. | Keep the engine phase current on a structured residual/context-incomplete error; do not close resolver, journal, or observability until an engine retry is terminal. |
| `runtime.ResourceRegistry` | Detaches entries before close; entry, lease, and registry `sync.Once` cache the first timeout forever. | Keep a key in closing state, make the same final lease and registry close retryable, block replacement acquisition, and retain the resource until final task quiescence. |

## Frozen Error and State Contracts

### Structured task residual carrier

Add exactly this exported type in `pkg/runtime/task_registry.go`:

```go
type TaskResidualError struct {
	residuals []TaskResidual
	cause     error
}

func (e *TaskResidualError) Error() string {
	return "task registry stop has residual tasks"
}

func (e *TaskResidualError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *TaskResidualError) Residuals() []TaskResidual {
	if e == nil {
		return nil
	}
	return slices.Clone(e.residuals)
}
```

When `Stop` reaches `ctx.Done()` and `Active()` is non-empty, it returns the existing sorted, deduplicated residual slice and `&TaskResidualError{residuals: slices.Clone(residuals), cause: ctx.Err()}`. A completed registry still wins over an already-canceled context and returns `(nil, nil)`. No owner names appear in `Error()`.

Callers classify an incomplete teardown without a new helper API:

```go
func taskResidualError(err error) (*runtime.TaskResidualError, bool) {
	var residual *runtime.TaskResidualError
	if !errors.As(err, &residual) {
		return nil, false
	}
	return residual, true
}
```

The actual implementation should inline `errors.As` at the small number of state-machine boundaries rather than add this illustrative helper to production.

### Terminal versus incomplete cleanup

- `TaskResidualError` in an error chain means ownership is incomplete and retryable.
- A direct `context.Canceled` or `context.DeadlineExceeded` from a wait boundary also means incomplete, even when no task carrier is present.
- `cleanupStack` itself treats every quiesce callback error as incomplete. Its owner does not need to infer phase from the error.
- `PreparedGeneration.Close` joins the safe exported sentinel `compiler.ErrPreparedGenerationCleanupIncomplete` whenever `cleanup.terminallyClosed()` is false. `generationOwner`, `GenerationEngine`, and `Server` preserve and classify it with `errors.Is`.
- A release callback error is terminal because the release callback has already consumed its one allowed invocation.
- Safe marker errors remain stable. They may be joined with `TaskResidualError` and context causes, but raw sensitive cleanup errors remain redacted at the existing compiler boundary.

### Resource acquisition while a key is closing

`ResourceRegistry` keeps two distinct internal states per key:

```text
entries[key] -> accepting references or running its factory
closing[key] -> zero accepting references, resource still owned, terminal close barrier open
```

`Acquire` that finds `closing[key]` does not acquire that resource and does not start another factory. It waits for either the closing entry's terminal barrier, the registry's terminal-close barrier, or its own context. If the entry becomes terminal while the registry remains open, acquisition loops through normal reserve and may create exactly one replacement. If the registry closes, it returns `ErrResourceRegistryClosed`. If the caller context wins, it returns that context error. A retryable close attempt does not close the terminal barrier, so no replacement overlaps the still-owned resource.

---

## File Responsibility and Dependency Order

| Task | Production files | Test files | Depends on |
| --- | --- | --- | --- |
| 1. Structured residual carrier | `pkg/runtime/task_registry.go` | `pkg/runtime/task_registry_test.go` | reviewed Contract C Task 1 head; both touch these files, so this task starts afterward and never from a sibling base |
| 2. Retryable two-phase cleanup | `pkg/compiler/cleanup.go` | `pkg/compiler/cleanup_test.go` plus affected focused compiler tests | Task 1 |
| 3. Prepared generation and factory retry | `pkg/compiler/prepared_generation.go`, `pkg/compiler/worker_factory.go` | `pkg/compiler/worker_factory_close_test.go`, affected `pkg/compiler/worker_factory_test.go` and `worker_factory_recovery_test.go` | Tasks 1-2 |
| 4. Retryable generation owner | `pkg/server/generation_owner.go` | `pkg/server/generation_owner_test.go` | Task 3 |
| 5. Engine discard, retirement, and close | `pkg/server/generation_engine.go` | `pkg/server/generation_engine_test.go` | Task 4 |
| 6. Server phase retry | `pkg/server/server.go` | `pkg/server/server_test.go` | Task 5 |
| 7. Retryable shared-resource final close | `pkg/runtime/resource_registry.go` | `pkg/runtime/resource_registry_test.go` | Task 1; integrate before final Task 8 and before generation shared-resource work |
| 8. Integrated gate and handoff | no new product files | all tests above | Tasks 1-7 and reviewed Contract C Task 1 |

Dependency graph:

```text
Task 1 residual carrier
├── Task 2 cleanup stack -> Task 3 prepared/factory -> Task 4 owner -> Task 5 engine -> Task 6 server
└── Task 7 resource registry

Tasks 1-7 -> Task 8 integrated Task11-0 gate

Contract C Task 1 -> Task11-0 Task 1 -> Tasks 2-7 -> Task 8
Contract C Task 1 -> Contract C Task 2 may proceed beside Task11-0 after file ownership is checked
Contract C Task 2 + integrated Task11-0 -> generation/background/shared-resource plan
Contract C Task 1 -> request/connection plan may proceed independently
```

Tasks 2-6 are one strict semantic chain and must integrate in order. Task 7 may be developed from the reviewed Task 1 head in an isolated worktree, but its verification must be regenerated after Tasks 2-6 integrate because compiler factories own a `ResourceRegistry`. Do not run two workers against overlapping files.

---

### Task 1: Carry Residual Owners Through the Error Chain

**Files:**

- Modify: `pkg/runtime/task_registry.go`
- Modify: `pkg/runtime/task_registry_test.go`

**Interfaces:**

- Consumes: existing `TaskResidual`, sorted `Active`, and `Stop(ctx) ([]TaskResidual, error)`.
- Produces: `*runtime.TaskResidualError` with stable `Error`, `Unwrap`, and defensive `Residuals`; unchanged `Stop` signature.

- [ ] **Step 1: Add RED tests for structured, defensive, retryable residual reporting**

Add these focused tests beside the existing stop tests:

```go
func TestTaskRegistryStopReturnsStructuredResidualError(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	for _, owner := range []string{"plugin/zeta/r1", "plugin/alpha/r1"} {
		if err := registry.Go(TaskSpec{Owner: owner, Criticality: TaskPlugin}, func(context.Context) error {
			started <- struct{}{}
			<-release
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	<-started
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	residuals, err := registry.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop error = %v, want deadline cause", err)
	}
	var residualErr *TaskResidualError
	if !errors.As(err, &residualErr) {
		t.Fatalf("Stop error type = %T, want *TaskResidualError", err)
	}
	want := []TaskResidual{{Owner: "plugin/alpha/r1"}, {Owner: "plugin/zeta/r1"}}
	if !reflect.DeepEqual(residuals, want) || !reflect.DeepEqual(residualErr.Residuals(), want) {
		t.Fatalf("residuals = %v / %v, want %v", residuals, residualErr.Residuals(), want)
	}
	if residualErr.Error() != "task registry stop has residual tasks" ||
		strings.Contains(residualErr.Error(), "plugin/") {
		t.Fatalf("unsafe residual error = %q", residualErr.Error())
	}
	copy := residualErr.Residuals()
	copy[0].Owner = "mutated"
	if !reflect.DeepEqual(residualErr.Residuals(), want) {
		t.Fatalf("Residuals aliases caller mutation: %v", residualErr.Residuals())
	}

	close(release)
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("retry Stop = (%v, %v)", residuals, err)
	}
}

func TestTaskResidualErrorNilMethodsAreSafe(t *testing.T) {
	var err *TaskResidualError
	if err.Unwrap() != nil || err.Residuals() != nil {
		t.Fatalf("nil TaskResidualError = (%v, %v)", err.Unwrap(), err.Residuals())
	}
}
```

Keep and strengthen the existing canceled-context completion tests: a registry whose `waitDone` is already closed returns no carrier even when the supplied context is canceled.

- [ ] **Step 2: Run RED**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime -run "^(TestTaskRegistryStopReturnsStructuredResidualError|TestTaskResidualErrorNilMethodsAreSafe)$" -count=1'
```

Expected: FAIL because `TaskResidualError` does not exist and `Stop` returns the bare context error.

- [ ] **Step 3: Implement the minimal carrier and wrap only real residuals**

Import `slices`. Add the exact type from the frozen contract. In the `ctx.Done()` branch, retain the existing post-deadline completion check and empty-owner check; only the non-empty branch changes:

```go
		residuals := make([]TaskResidual, len(owners))
		for i, owner := range owners {
			residuals[i] = TaskResidual{Owner: owner}
		}
		return residuals, &TaskResidualError{
			residuals: slices.Clone(residuals),
			cause:     ctx.Err(),
		}
```

Do not change `Active`, task failure behavior, admission, criticality, or the `Stop` method signature.

- [ ] **Step 4: Run GREEN and race coverage**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime -run "^(TestTaskRegistry|TestTaskResidualError)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/runtime -run "^(TestTaskRegistryStopReturnsStructuredResidualError|TestTaskRegistryConcurrentStopsHonorIndependentDeadlines|TestTaskRegistryConcurrentAdmissionAndStop)$" -count=1'
```

Expected: PASS; the error chain preserves the context cause, the owner list stays sorted/deduplicated, and a later Stop succeeds after task exit.

- [ ] **Step 5: Review and commit**

Review `git diff -- pkg/runtime/task_registry.go pkg/runtime/task_registry_test.go`, then:

```bash
git add pkg/runtime/task_registry.go pkg/runtime/task_registry_test.go
git commit -m "fix(runtime): preserve task residual identity"
```

---

### Task 2: Make Cleanup Strictly Quiesce, Then Release, Then Cache

**Files:**

- Modify: `pkg/compiler/cleanup.go`
- Modify: `pkg/compiler/cleanup_test.go`

**Interfaces:**

- Consumes: Task 1 residual carrier; existing phase order, reverse order, checkpoints, and ownership sealing.
- Produces: retryable `cleanupStack.Close`; a failed checkpoint rollback remains owned; releases execute exactly once only after all relevant quiescers succeed.

- [ ] **Step 1: Replace replay-first-error tests with RED retry/barrier oracles**

Add tests with exact call traces:

```go
func TestCleanupStackQuiesceFailureDefersEveryReleaseAndRetriesOnlyPendingQuiescer(t *testing.T) {
	var stack cleanupStack
	var calls []string
	var quiesceAttempts int
	transient := errors.New("tasks still running")
	if err := stack.Own(cleanupRelease, "registration", func(context.Context) error {
		calls = append(calls, "release-registration")
		return nil
	}); err != nil { t.Fatal(err) }
	if err := stack.Own(cleanupQuiesce, "already-done", func(context.Context) error {
		calls = append(calls, "quiesce-already-done")
		return nil
	}); err != nil { t.Fatal(err) }
	if err := stack.Own(cleanupQuiesce, "tasks", func(context.Context) error {
		quiesceAttempts++
		calls = append(calls, fmt.Sprintf("quiesce-tasks-%d", quiesceAttempts))
		if quiesceAttempts == 1 { return transient }
		return nil
	}); err != nil { t.Fatal(err) }

	if err := stack.Close(context.Background()); !errors.Is(err, transient) {
		t.Fatalf("first Close error = %v, want %v", err, transient)
	}
	if slices.Contains(calls, "release-registration") {
		t.Fatalf("release crossed failed quiesce: %v", calls)
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("retry Close error = %v", err)
	}
	want := []string{"quiesce-tasks-1", "quiesce-already-done", "quiesce-tasks-2", "release-registration"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if err := stack.Close(context.Background()); err != nil || quiesceAttempts != 2 {
		t.Fatalf("terminal replay = %v, attempts = %d", err, quiesceAttempts)
	}
}

func TestCleanupStackTerminalReleaseErrorsJoinAndNeverRetry(t *testing.T) {
	first := errors.New("first release")
	second := errors.New("second release")
	var stack cleanupStack
	var calls atomic.Int32
	for _, item := range []struct{name string; err error}{
		{name: "first", err: first}, {name: "second", err: second},
	} {
		item := item
		if err := stack.Own(cleanupRelease, item.name, func(context.Context) error {
			calls.Add(1)
			return item.err
		}); err != nil { t.Fatal(err) }
	}
	closeErr := stack.Close(context.Background())
	if !errors.Is(closeErr, first) || !errors.Is(closeErr, second) {
		t.Fatalf("Close error = %v, want both releases", closeErr)
	}
	if replay := stack.Close(context.Background()); replay != closeErr || calls.Load() != 2 {
		t.Fatalf("terminal replay = %v, calls = %d", replay, calls.Load())
	}
}
```

Add `TestCleanupStackRollbackRetainsSuffixWhenQuiesceFails`: create a base release, checkpoint, suffix quiescer that fails once, and suffix release. First `Rollback` returns the quiesce error, runs no suffix release, and preserves the suffix. Second `Rollback` succeeds, releases the suffix once, and subsequent `Close` runs only the base release.

Update `TestCleanupStackConcurrentCloseRunsEachStepOnce` so one leader performs the callback and every waiter sees the same attempt result; a later call starts a retry only after the first incomplete attempt has returned.

- [ ] **Step 2: Run RED**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^TestCleanupStack" -count=1'
```

Expected: FAIL because current `Close` releases after failed quiescence and `sync.Once` prevents retry; current `Rollback` truncates before cleanup.

- [ ] **Step 3: Implement a serialized attempt state machine**

Replace `cleanupStack.closeOnce` with the exact private attempt and terminal state below. Extend `cleanupStep` with `done bool`. Preserve reverse ordering and callback execution outside `mu`.

```go
type cleanupAttempt struct {
	done chan struct{}
	err  error
}

type cleanupStack struct {
	mu        sync.Mutex
	quiescers []cleanupStep
	releases  []cleanupStep
	sealed    bool
	active    *cleanupAttempt
	terminal  bool
	closeErr  error
}
```

`Close` must follow this algorithm:

1. Normalize nil context and seal ownership.
2. If terminal, return the cached terminal error.
3. If another attempt is active, capture its pointer, unlock, and wait for `attempt.done` or the caller context. After the attempt finishes, return `attempt.err`; do not silently launch another retry from the same call.
4. Otherwise install `active = &cleanupAttempt{done: make(chan struct{})}` and run every unfinished quiescer in reverse order. Mark only successful quiescers done.
5. If any quiescer failed, publish the joined attempt error, close the attempt barrier, and return without running a release.
6. After every quiescer is done, run every unfinished release in reverse order. Mark each release done regardless of its return value. Join all release errors.
7. Set terminal, cache the joined release result, publish it to concurrent waiters, and replay it forever.

Before closing an attempt's `done`, store its joined error, clear `active` only if it is still that exact attempt, and publish terminal state when appropriate. This makes concurrent waiters receive the attempt they joined while a call arriving afterward may become the next retry leader.

`Rollback` uses the same phase executor for only the checkpoint suffix. It does not seal the whole stack. On quiesce failure it leaves that suffix in place with successful suffix quiescers marked done. On terminal suffix completion it truncates both slices to the checkpoint. Invalid/foreign/sealed checkpoint behavior remains unchanged.

Keep `Close(ctx context.Context) error` and `Rollback(ctx context.Context, checkpoint cleanupCheckpoint) error` unchanged. Add exactly one package-private observation for the owning `PreparedGeneration` state machine:

```go
func (s *cleanupStack) terminallyClosed() bool
```

It returns true only after all releases were attempted and their errors cached. No other caller uses it. Do not export cleanup state or add a public PreparedGeneration status method.

- [ ] **Step 4: Run GREEN, call-site tests, and race coverage**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^(TestCleanupStack|TestWorkerCompilerFactoryPrepareGenerationCleansFailedDependenciesInOrder|TestWorkerCompilerFactoryPrepareGenerationOwnsPartialConsumersBeforeError|TestPreparedGenerationAttachHTTPSerializesWithClose|TestPreparedGenerationAttachStreamSerializesWithClose)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler -run "^(TestCleanupStackConcurrentCloseRunsEachStepOnce|TestCleanupStackQuiesceFailureDefersEveryReleaseAndRetriesOnlyPendingQuiescer)$" -count=1'
```

Expected: PASS; no release trace appears before every quiescer succeeds, checkpoint rollback retains failed ownership, and all release callbacks remain exact-once.

- [ ] **Step 5: Review and commit**

Inspect the diff to confirm only `cleanup.go` and its tests changed.

```bash
git add pkg/compiler/cleanup.go pkg/compiler/cleanup_test.go
git commit -m "fix(compiler): make cleanup quiescence retryable"
```

---

### Task 3: Retry Prepared Generation and Factory Close Without Detaching Ownership

**Files:**

- Modify: `pkg/compiler/prepared_generation.go`
- Modify: `pkg/compiler/worker_factory.go`
- Modify: `pkg/compiler/prepared_generation_test.go`
- Modify: `pkg/compiler/worker_factory_close_test.go`
- Modify only for affected expectations: `pkg/compiler/worker_factory_test.go`
- Modify only for affected expectations: `pkg/compiler/worker_factory_recovery_test.go`

**Interfaces:**

- Consumes: Task 2 terminal/incomplete cleanup result; Task 1 residual carrier; existing compiler safe markers.
- Produces: safe `ErrPreparedGenerationCleanupIncomplete`; retryable `PreparedGeneration.Close`/`DiscardPrepared`; retryable stable-order factory close; shared registry close only after every prepared generation becomes terminal.

- [ ] **Step 1: Add a PreparedGeneration RED oracle**

Use the existing worker factory fixture and replace the generation-task quiescer with a real `TaskRegistry` callback that ignores cancellation until released. The test must prove all retained state through the retry boundary:

```go
func TestPreparedGenerationCloseResidualRetainsOwnersUntilRetry(t *testing.T) {
	prepared, cleanupCalls, detachCalls := preparedGenerationFixture(t)
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	prepared.tasks = tasks
	if err := prepared.cleanup.Own(cleanupQuiesce, "generation-tasks", func(ctx context.Context) error {
		residuals, stopErr := tasks.Stop(ctx)
		if stopErr != nil || len(residuals) != 0 { return stopErr }
		return nil
	}); err != nil { t.Fatal(err) }
	set := prepared.PublicationSet()
	release := make(chan struct{})
	started := make(chan struct{})
	if err := tasks.Go(runtime.TaskSpec{
		Owner: "plugin/test/attempt/blocking", Criticality: runtime.TaskPlugin,
	}, func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil { t.Fatal(err) }
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	first := prepared.DiscardPrepared(ctx, set)
	var residual *runtime.TaskResidualError
	if !errors.As(first, &residual) || !errors.Is(first, context.DeadlineExceeded) ||
		!errors.Is(first, ErrPreparedGenerationCleanupIncomplete) ||
		!errors.Is(first, errPreparedGenerationCleanupFailed) {
		t.Fatalf("first discard = %v, residual = %#v", first, residual)
	}
	if prepared.cleanup == nil || prepared.tasks == nil || prepared.detach == nil ||
		cleanupCalls.Load() != 0 || detachCalls.Load() != 0 {
		t.Fatal("incomplete close dropped or released cleanup, task, or detach ownership")
	}
	if prepared.PublicationSet().DesiredRevision != 0 {
		t.Fatal("terminal observation remained visible after close began")
	}

	close(release)
	if err := prepared.DiscardPrepared(context.Background(), set); err != nil {
		t.Fatalf("retry discard = %v", err)
	}
	if prepared.cleanup != nil || prepared.tasks != nil || prepared.detach != nil ||
		cleanupCalls.Load() != 1 || detachCalls.Load() != 1 {
		t.Fatal("terminal retry retained generation authority")
	}
}
```

Preserve the original `set` captured before closing because public observations intentionally become empty after close starts. The separate factory RED test below owns exact `live`-map retention; do not add a production constructor solely for either test.

- [ ] **Step 2: Add factory RED oracles**

Add:

- `TestWorkerCompilerFactoryCloseResidualRetainsGenerationAndDefersRegistry`: two live generations in stable AttemptID order; one task blocks. First short close reports the structured residual and safe factory marker, closes any independently terminal generation, retains the blocked generation in `live`, and leaves `factory.registry` unclosed. After release, retry closes the retained generation then registry.
- `TestWorkerCompilerFactoryCloseRetryPreservesIndependentTerminalErrors`: a terminal safe error from one prepared owner remains in the final factory error after a different owner first returns a residual and later succeeds.
- `TestWorkerCompilerFactoryConcurrentRetryHasOneAttemptLeader`: concurrent callers join one close attempt; no prepared release or registry close executes twice.

- [ ] **Step 3: Run RED**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^(TestPreparedGenerationCloseResidualRetainsOwnersUntilRetry|TestWorkerCompilerFactoryCloseResidualRetainsGenerationAndDefersRegistry|TestWorkerCompilerFactoryCloseRetryPreservesIndependentTerminalErrors|TestWorkerCompilerFactoryConcurrentRetryHasOneAttemptLeader)$" -count=1'
```

Expected: FAIL because `closeOnce` clears/detaches the prepared generation and the factory closes its registry after the first incomplete attempt.

- [ ] **Step 4: Implement retryable PreparedGeneration close**

Replace `closeOnce` with a serialized-attempt state under the existing materialization/close locks. Keep these invariants:

```go
type preparedCloseAttempt struct {
	done chan struct{}
	err  error
}

// Replace closeOnce; retain terminal as the existing observation-sealed bit.
closeMu         sync.Mutex
closeAttempt    *preparedCloseAttempt
cleanupTerminal bool
closeErr        error
```

- Export exactly `var ErrPreparedGenerationCleanupIncomplete = errors.New("prepared generation cleanup is incomplete")`; it is a safe classifier, not a raw cleanup wrapper or status method.
- `closeStarted` runs once and `terminal = true` is set before the first cleanup attempt, so no observation or attachment is admitted after close starts.
- An incomplete cleanup result returns `errors.Join(errPreparedGenerationCleanupFailed, ErrPreparedGenerationCleanupIncomplete, safeResidualCause)` and retains `cleanup`, snapshots, attempts, tasks, resources, binding operations, and `detach` internally.
- `safeResidualCause` contains only `TaskResidualError`, its context cause, and existing constant safe markers. Raw registration/provider/plugin cleanup text is not exposed.
- Use `cleanup.terminallyClosed()` after each cleanup attempt. When it is false, retain ownership and return the retryable safe chain; when true, perform the terminal field clearing below.
- On terminal cleanup, revoke snapshots, clear every authority field, call `detach` once, and cache the terminal safe result. A terminal release error returns `errPreparedGenerationCleanupFailed` exactly as the existing redaction tests require.
- Concurrent callers wait for the active attempt with their own context. They do not launch a second cleanup callback concurrently.
- Pass the normalized caller `ctx` to `cleanup.Close`; delete the current `cleanupCtx := context.WithoutCancel(ctx)` from this public close path. The caller deadline is the bound that produces the structured residual. Preparation-failure rollback remains a separate internal ownership path and is not changed to discard resources early.

Keep `DiscardPrepared` exact-set validation before delegating to `Close`. It may use the private stored publication after terminal observation is hidden; a caller with the exact original set can retry, while any mismatch still returns `ErrPreparedSetMismatch` without advancing cleanup.

- [ ] **Step 5: Implement retryable WorkerCompilerFactory close**

Replace `closeOnce` with an attempt barrier and terminal cache. On the first call, take `gate`, set `closed = true`, and never reopen preparation. Each attempt:

```go
type workerFactoryCloseAttempt struct {
	done chan struct{}
	err  error
}

// Replace closeOnce.
closeMu       sync.Mutex
closeAttempt  *workerFactoryCloseAttempt
closeTerminal bool
closeErrors   []error
closeErr      error
```

1. Snapshots `live` by AttemptID and sorts it as today.
2. Calls every live generation so independent owners can make progress; records terminal safe errors once.
3. If any generation error contains `*runtime.TaskResidualError` or a context-incomplete wait, returns the accumulated terminal safe errors joined with the current retryable error and does not call `registry.Close`.
4. Only after the live map is empty calls the retryable resource registry close.
5. Caches a terminal `errWorkerCompilerFactoryCleanupFailed` only after the registry close is terminal. Repeated terminal calls do no work.

Do not delete `live` directly from the factory attempt; only the generation's terminal `detach` removes its exact AttemptID mapping.
Pass the current caller `ctx` to every prepared-generation and registry close attempt; remove `cleanupCtx := context.WithoutCancel(ctx)` from `WorkerCompilerFactory.Close`. A factory retry is useful only when each public attempt has an independent bound.

- [ ] **Step 6: Run GREEN, existing redaction/order tests, and race**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^(TestPreparedGenerationCloseResidualRetainsOwnersUntilRetry|TestWorkerCompilerFactoryClose|TestWorkerCompilerFactoryConcurrentCloseDiscardAndFactoryCloseRunOnce|TestWorkerCompilerFactoryPrepareGenerationRedactsProviderAndCleanupErrors|TestWorkerCompilerFactoryPrepareRecoveryRedactsRegistrationAndPartialCloseErrors)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler -run "^(TestPreparedGenerationCloseResidualRetainsOwnersUntilRetry|TestWorkerCompilerFactoryCloseResidualRetainsGenerationAndDefersRegistry|TestWorkerCompilerFactoryConcurrentRetryHasOneAttemptLeader|TestWorkerCompilerFactoryConcurrentCloseDiscardAndFactoryCloseRunOnce)$" -count=1'
```

Expected: PASS; safe-marker equality/redaction stays intact where terminal, residual/context causes remain discoverable when incomplete, and registry close stays last.

- [ ] **Step 7: Review and commit**

```bash
git add pkg/compiler/prepared_generation.go pkg/compiler/worker_factory.go \
  pkg/compiler/prepared_generation_test.go \
  pkg/compiler/worker_factory_close_test.go pkg/compiler/worker_factory_test.go \
  pkg/compiler/worker_factory_recovery_test.go
git commit -m "fix(compiler): retry generation and factory teardown"
```

---

### Task 4: Retry generationOwner Close Until Prepared Cleanup Is Terminal

**Files:**

- Modify: `pkg/server/generation_owner.go`
- Modify: `pkg/server/generation_owner_test.go`

**Interfaces:**

- Consumes: retryable `PreparedGeneration.Close`; existing lease drain barrier and `closeDone` signal.
- Produces: one serialized prepared-close attempt at a time; `closeDone` means terminal close, never merely first attempt.

- [ ] **Step 1: Add RED owner tests**

Add `TestGenerationOwnerPreparedResidualKeepsCloseDoneOpenUntilRetry`. Build the existing owner fixture, inject a blocking generation task, deactivate the final domain, and call `closePrepared` with a short context. Assert:

```go
first := owner.closePrepared(shortCtx)
var residual *runtime.TaskResidualError
if !errors.As(first, &residual) || !errors.Is(first, context.DeadlineExceeded) {
	t.Fatalf("first close = %v", first)
}
select {
case <-owner.closeDone:
	t.Fatal("incomplete prepared close signaled terminal closeDone")
default:
}
if owner.prepared == nil {
	t.Fatal("incomplete close dropped prepared ownership")
}
close(taskRelease)
if err := owner.closePrepared(context.Background()); err != nil {
	t.Fatalf("retry close = %v", err)
}
select {
case <-owner.closeDone:
default:
	t.Fatal("terminal retry did not close closeDone")
}
if owner.prepared != nil {
	t.Fatal("terminal close retained prepared owner")
}
```

Add `TestGenerationOwnerConcurrentCloseAttemptsSerializeAndHonorCallerContext`: one caller owns a blocked attempt; a short waiter returns its context error; after the leader attempt returns incomplete, a later caller performs the retry. Preserve the existing pre-drain timeout tests.

- [ ] **Step 2: Run RED**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestGenerationOwnerPreparedResidualKeepsCloseDoneOpenUntilRetry|TestGenerationOwnerConcurrentCloseAttemptsSerializeAndHonorCallerContext)$" -count=1'
```

Expected: FAIL because `closeOnce` closes `closeDone` and caches the first result.

- [ ] **Step 3: Replace closeOnce with a close-attempt barrier**

Keep drain waiting first. After `drained` closes, serialize calls with these exact additional fields and helper type:

```go
type generationCloseAttempt struct {
	done chan struct{}
	err  error
}

closeMu       sync.Mutex
closeAttempt  *generationCloseAttempt
closeTerminal bool
closeDone     chan struct{}
closeErr      error
```

An active-attempt waiter selects its own context versus `closeAttemptDone`. A leader calls `prepared.Close(ctx)` outside locks. If `errors.Is(err, compiler.ErrPreparedGenerationCleanupIncomplete)`, retain `prepared`, publish only the attempt result, and leave `closeDone` open. Otherwise clear `prepared`, cache the terminal result, close `closeDone` once, and replay forever. Do not infer retryability from `errPreparedGenerationCleanupFailed` alone; that marker also represents terminal redacted release errors.

- [ ] **Step 4: Run GREEN and race coverage**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^TestGenerationOwner" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^(TestGenerationOwnerPreparedResidualKeepsCloseDoneOpenUntilRetry|TestGenerationOwnerConcurrentCloseAttemptsSerializeAndHonorCallerContext|TestGenerationOwnerCloseWaitsForFinalLease)$" -count=1'
```

Expected: PASS; lease drain remains a prerequisite, concurrent close is serialized, and only terminal close signals `closeDone`.

- [ ] **Step 5: Review and commit**

```bash
git add pkg/server/generation_owner.go pkg/server/generation_owner_test.go
git commit -m "fix(server): retry generation owner teardown"
```

---

### Task 5: Retry Engine Discard, Retirement, and Close Without Polling

**Files:**

- Modify: `pkg/server/generation_engine.go`
- Modify: `pkg/server/generation_engine_test.go`

**Interfaces:**

- Consumes: Task 4 owner terminal signal; Task 3 retryable factory close; existing pending/retirement maps and stream metric ownership.
- Produces: exact-set discard retry, retained retirement owners/metrics after incomplete close, context-bounded engine Close, factory-last retry, and no busy loop.

- [ ] **Step 1: Add discard RED tests**

Add `TestGenerationEngineDiscardResidualRetainsExactSetForRetry`:

1. Prepare one non-synthetic pending generation and admit a blocking generation task through the fixture seam.
2. Call `DiscardPrepared` with a 10 ms context.
3. Assert `errors.As(err, *runtime.TaskResidualError)`, `errors.Is(err, context.DeadlineExceeded)`, the pending record remains under the same `preparedKey`, `discarding` is false after the attempt publishes, and activation remains rejected while the record is terminally closing.
4. Release the task and call `DiscardPrepared` again with the exact set; assert success and pending deletion.
5. Assert a mismatched set never joins or advances the retained cleanup.

Extend `TestGenerationEngineDiscardConcurrentWaitersReplayThenForget`: all callers that joined one attempt receive that attempt's result. A residual result resets the record for a later explicit retry instead of deleting it; a terminal result deletes only after all joined waiters have read it.

- [ ] **Step 2: Add retirement and engine-close RED tests**

Add:

- `TestGenerationEngineRetirementResidualRetainsOwnerAndStreamMetrics`: an owner close returns a structured residual. Assert it remains in `retireKnown`, its metric routes remain published, and it is not immediately requeued or called repeatedly. After task release, a later `Close` attempt retries once and unregisters metrics only after terminal close.
- `TestGenerationEngineCloseCancelsActiveRetirementAttemptBeforeRetry`: begin ordinary asynchronous retirement with an engine-owned attempt context and a task that ignores cancellation. Assert public `Close(shortCtx)` cancels that active attempt, joins `retireWG` without an unbounded wait, receives the structured residual, retains the owner/metrics, and does not start the retry until the active attempt has exited. After task release, a later `Close` retries to terminal completion.
- `TestGenerationEngineCloseDeadlineReturnsWithoutWaitingForever`: hold one owner/task beyond the caller deadline. `Close` returns a residual/context error, leaves `closed = true`, keeps owner/factory state, and rejects Prepare/Activate. It does not block on `retireWG.Wait` after the deadline.
- `TestGenerationEngineCloseRetryClosesPendingRetirementsThenFactory`: first close times out; release the task; second close finishes owners, clears metrics, then closes factory. Trace order must be `owner-terminal`, `metrics-unregister`, `factory-close`.
- `TestGenerationEngineClosePreservesIndependentCleanupErrorsAcrossRetry`: a terminal error from one pending/retired owner is still discoverable in the final engine error after another owner first returns a residual.

- [ ] **Step 3: Run RED**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestGenerationEngineDiscardResidualRetainsExactSetForRetry|TestGenerationEngineDiscardConcurrentWaitersReplayThenForget|TestGenerationEngineRetirementResidualRetainsOwnerAndStreamMetrics|TestGenerationEngineCloseCancelsActiveRetirementAttemptBeforeRetry|TestGenerationEngineCloseDeadlineReturnsWithoutWaitingForever|TestGenerationEngineCloseRetryClosesPendingRetirementsThenFactory|TestGenerationEngineClosePreservesIndependentCleanupErrorsAcrossRetry)$" -count=1'
```

Expected: FAIL because current discard forgets the record, retirement forgets any failed owner, and engine `closeOnce` advances to factory and waits without a retryable phase boundary.

- [ ] **Step 4: Make discard attempts replayable, not terminal by default**

Keep `pendingRecord`, but treat `discardDone` as one attempt barrier rather than lifetime completion:

```go
type pendingRecord struct {
	key            preparedKey
	set            generation.PublicationSet
	owner          *generationOwner
	synthetic      bool
	discarding     bool
	discardDone    chan struct{}
	discardErr     error
	discardTerminal bool
	discardWaiters int
}
```

The leader invokes discard once with the caller `ctx`, not `context.WithoutCancel(ctx)`. On incomplete error it publishes the attempt, sets `discarding = false`, creates a fresh barrier only when a later leader claims the next attempt, and retains the record. Joined waiters read the completed attempt result before counters permit terminal deletion. On terminal completion, mark terminal and delete only after current waiters drain. Synthetic records remain terminal without cleanup.

- [ ] **Step 5: Make retirement retain incomplete owners**

Each retirement record owns `attemptCtx, cancel := context.WithCancel(context.WithoutCancel(enqueueCtx))`; it is detached from a short-lived activation caller but remains explicitly cancellable by terminal engine shutdown. Store that cancel function with the active retirement attempt. `retireOwner` passes `attemptCtx` to `owner.closePrepared`, unregisters metrics, and removes `retireKnown` only after prepared cleanup is terminal. On incomplete result:

- remove it from `retireActive` because this attempt ended;
- keep it in `retireKnown` and retain its stream metrics;
- store the latest structured attempt error for the current engine-close result, not as a terminal accumulated error;
- do not append it permanently to `retireErrors`;
- do not enqueue it again from the same call and do not loop.

A later engine close attempt snapshots retained known owners and invokes one new close attempt for each. At the start of terminal `GenerationEngine.Close`, cancel every active engine-owned retirement attempt, then join those attempts with the caller context before starting a retry. This cancellation is what makes an otherwise unbounded `TaskRegistry.Stop` return its structured residual; never race a second `PreparedGeneration.Close` leader against the still-active retirement attempt. Ordinary asynchronous retirement remains detached from activation cancellation but is not detached from engine shutdown. Use existing `closeDone`, `retireWake`, and attempt barriers only. Do not add a ticker, residual `Done` API, or sleep.

- [ ] **Step 6: Replace engine closeOnce with ordered retry phases**

Use a serialized close-attempt state. The first attempt atomically marks `engine.closed`, swaps out the active bundle, rejects new work, converts activation/active owners to retirement, and retains pending records in a private closing collection rather than discarding the map. Later attempts reuse that captured ownership and never repeat domain deactivation.

```go
type generationEngineClosePhase uint8

const (
	engineCloseInitial generationEngineClosePhase = iota
	engineCloseOwnersCaptured
	engineClosePendingDone
	engineCloseRetirementDone
	engineCloseFactoryDone
)

type generationEngineCloseAttempt struct {
	done chan struct{}
	err  error
}

closeMu      sync.Mutex
closeAttempt *generationEngineCloseAttempt
closePhase   generationEngineClosePhase
closePending map[preparedKey]*pendingRecord
closeErrors  []error
closeErr     error
```

Each close attempt performs exactly these barriers:

1. Retry pending discard owners; if any are incomplete, return without shutting down retirement or factory.
2. Cancel active engine-owned retirement attempts, wake/drain retirement, and join the canceled attempts before any retained-owner retry. Replace direct `<-retireDone` and `retireWG.Wait()` with context-selectable completion barriers. If the caller context wins, return incomplete without clearing metrics or closing factory. A joined incomplete attempt contributes its structured residual to the current result.
3. Once every retirement is terminal, stop the retirement loop exactly once and clear metrics.
4. Call retryable `factory.Close(ctx)` last. If it is incomplete, retain the engine phase for the next Close.
5. After factory terminal completion, cache `errors.Join` of independently retained terminal cleanup errors and replay it.

Use the caller context for every engine-close wait and retryable child close. Ordinary asynchronous retirement uses the engine-owned cancelable attempt context from Step 5: it is detached from activation cancellation but must be canceled and joined by engine shutdown before any retry. Delete `cleanupCtx := context.WithoutCancel(ctx)` from `GenerationEngine.close`; do not transform a deadline-bearing Close into an unbounded wait. Preserve the public method set frozen by `TestGenerationEnginePublicSurfaceIsFrozen`.

- [ ] **Step 7: Run GREEN, focused regressions, and race**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestGenerationEngineDiscard|TestGenerationEngineRollback|TestGenerationEngineFinalize|TestGenerationEngineClose|TestGenerationEngineRetirement|TestGenerationEngineStreamMetrics|TestGenerationEnginePublicSurfaceIsFrozen)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^(TestGenerationEngineDiscardConcurrentWaitersReplayThenForget|TestGenerationEngineRetirementResidualRetainsOwnerAndStreamMetrics|TestGenerationEngineCloseCancelsActiveRetirementAttemptBeforeRetry|TestGenerationEngineCloseDeadlineReturnsWithoutWaitingForever|TestGenerationEngineCloseRetryClosesPendingRetirementsThenFactory|TestGenerationEngineConcurrentCloseAndReleaseReplaysFirstCleanupError)$" -count=1'
```

Expected: PASS; no retained owner is forgotten, metrics clear only after terminal retirement, and a close deadline returns without CPU polling or unbounded waits.

- [ ] **Step 8: Review and commit**

Review every deletion from `pending`, `retireKnown`, `retireActive`, and `streamMetricOwners`; each must be guarded by terminal ownership evidence.

```bash
git add pkg/server/generation_engine.go pkg/server/generation_engine_test.go
git commit -m "fix(server): retry generation engine teardown"
```

---

### Task 6: Hold Server Shutdown at the Engine Phase Until Retry Completes

**Files:**

- Modify: `pkg/server/server.go`
- Modify: `pkg/server/server_test.go`

**Interfaces:**

- Consumes: Task 5 retryable `GenerationEngine.Close`; existing `shutdownAttempt(ctx) (error, bool)` phase state machine.
- Produces: engine-phase retry that preserves earlier terminal shutdown errors and never reaches resolver/journal/observability while generation ownership is incomplete.

- [ ] **Step 1: Add RED shutdown phase tests**

Add `TestServerShutdownEngineResidualRetriesBeforeLaterOwners` using `newShutdownLifecycleServer` and a fake engine close function:

```go
tasks := runtime.NewTaskRegistry(context.Background(), nil)
release := make(chan struct{})
started := make(chan struct{})
if err := tasks.Go(runtime.TaskSpec{
	Owner: "plugin/test/server-shutdown", Criticality: runtime.TaskPlugin,
}, func(context.Context) error {
	close(started)
	<-release
	return nil
}); err != nil { t.Fatal(err) }
<-started
short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
defer cancel()
_, residualErr := tasks.Stop(short)
var residual *runtime.TaskResidualError
if !errors.As(residualErr, &residual) {
	t.Fatalf("fixture Stop error = %v, want structured residual", residualErr)
}
close(release)
```

Have the fake engine return the captured `residualErr` on its first call, then nil on its second. Do not add a test-only production constructor. Assert:

- first `Shutdown` preserves `errors.Is(context.DeadlineExceeded)` and `errors.As(*runtime.TaskResidualError)`;
- `shutdownPhase` remains `shutdownPhaseDrained`, `engineClosed` remains false, and resolver/journal/observability calls remain zero;
- second `Shutdown` calls engine again, then resolver, journal, and observability in order;
- the retryable deadline is not cached into the terminal replay result.

Add `TestServerShutdownEngineResidualPreservesEarlierTerminalDrainError`: make HTTP drain return a terminal marker while route drain times out once, then on the next attempt make engine return a residual once. Final successful shutdown must still satisfy `errors.Is(final, terminalDrainErr)` and must not contain the transient context deadline.

- [ ] **Step 2: Run RED**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestServerShutdownEngineResidualRetriesBeforeLaterOwners|TestServerShutdownEngineResidualPreservesEarlierTerminalDrainError)$" -count=1'
```

Expected: FAIL because current code sets `engineClosed = true` and advances to the resolver phase after any engine error.

- [ ] **Step 3: Gate engine phase advancement on terminal close**

In `shutdownAttempt`, wrap the engine error once, then classify before mutation:

```go
	if s.shutdownPhase < shutdownPhaseEngineClosed {
		if s.engine != nil && !s.engineClosed {
			engineErr := wrapCleanupError("close generation engine", s.engine.Close(ctx))
			var residual *runtime.TaskResidualError
			if errors.As(engineErr, &residual) ||
				errors.Is(engineErr, compiler.ErrPreparedGenerationCleanupIncomplete) ||
				contextError(engineErr) {
				return errors.Join(errors.Join(s.shutdownErrors...), engineErr), false
			}
			s.appendShutdownError(engineErr)
			s.engineClosed = true
		}
		s.shutdownPhase = shutdownPhaseEngineClosed
	}
```

The actual code must not classify a nil error as a context error. Keep resolver, journal, and observability behavior unchanged except that they cannot run before terminal engine close. Preserve concurrent `Shutdown` attempt joining and terminal replay semantics.

- [ ] **Step 4: Run GREEN and shutdown regression/race gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^TestServer(Shutdown|RepeatedShutdown)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^(TestServerShutdownEngineResidualRetriesBeforeLaterOwners|TestServerShutdownEngineResidualPreservesEarlierTerminalDrainError|TestServerRepeatedShutdownReplaysFirstTerminalCleanupError|TestServerShutdownTimeoutDoesNotReleaseEngineResolverOrJournal)$" -count=1'
```

Expected: PASS; later owners remain untouched until engine terminal completion, earlier terminal errors survive, and transient residual deadlines do not poison terminal replay.

- [ ] **Step 5: Review and commit**

```bash
git add pkg/server/server.go pkg/server/server_test.go
git commit -m "fix(server): retry incomplete shutdown phases"
```

---

### Task 7: Keep Resource Entries Closing Until Final Task Quiescence

**Files:**

- Modify: `pkg/runtime/resource_registry.go`
- Modify: `pkg/runtime/resource_registry_test.go`

**Interfaces:**

- Consumes: Task 1 structured residual carrier; existing key identity, single creator, reference count, typed lease, and terminal registry close contracts.
- Produces: retryable final lease and registry close, internal closing-by-key barrier, no overlapping replacement, exact-once terminal release, and retained resources across residuals.

- [ ] **Step 1: Add RED final-lease retry test**

```go
func TestResourceRegistryFinalReleaseResidualRetainsResourceForRetry(t *testing.T) {
	registry := NewResourceRegistry()
	tasks := NewTaskRegistry(context.Background(), nil)
	releaseTask := make(chan struct{})
	started := make(chan struct{})
	if err := tasks.Go(TaskSpec{Owner: "core/resource/test", Criticality: TaskCore}, func(context.Context) error {
		close(started)
		<-releaseTask
		return nil
	}); err != nil { t.Fatal(err) }
	<-started
	var resourceReleased atomic.Bool
	lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(ctx context.Context) error {
			residuals, stopErr := tasks.Stop(ctx)
			if stopErr != nil || len(residuals) != 0 { return stopErr }
			resourceReleased.Store(true)
			return nil
		}, nil
	})
	if err != nil { t.Fatal(err) }
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	first := lease.Release(short)
	var residual *TaskResidualError
	if !errors.As(first, &residual) || resourceReleased.Load() {
		t.Fatalf("first release = %v, resourceReleased = %t", first, resourceReleased.Load())
	}
	if registry.Len() != 0 {
		t.Fatalf("accepting Len = %d, want 0 while closing", registry.Len())
	}
	close(releaseTask)
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("retry release = %v", err)
	}
	if !resourceReleased.Load() {
		t.Fatal("terminal retry did not release resource")
	}
}
```

The finalizer shown above returns `stopErr`, which is already the structured carrier. Generation shared-resource finalizers may wrap it with stable safe text but must preserve `errors.As` and `errors.Is`.

- [ ] **Step 2: Add acquisition-during-close RED tests**

Add `TestResourceRegistryAcquireWaitsForClosingIdentityBeforeReplacement`:

1. Acquire one lease and make its finalizer return a residual until released.
2. Start final `Release(shortCtx)` and confirm the key moves out of accepting `Len` but remains in private `closing`.
3. Start a second `Acquire` for the same key with a factory-call counter. Assert the factory counter remains one and acquisition has not returned.
4. Release the task and retry the same final lease.
5. Assert the waiter creates exactly one replacement only after terminal close, receives it, and can release it.

Add `TestResourceRegistryAcquireClosingIdentityHonorsContext`: while the key stays closing, a short acquisition returns `context.DeadlineExceeded`, never increments references, and never calls a replacement factory.

Add `TestResourceRegistryAcquireClosingIdentityReturnsRegistryClosed`: while an acquisition waits on a closing key, begin registry Close; the waiter returns `ErrResourceRegistryClosed` rather than starting a factory.

- [ ] **Step 3: Add registry-close and exact-once RED tests**

Add:

- `TestResourceRegistryCloseResidualRetainsEntriesAndRetries`: first Close reports the structured residual; second Close after task release succeeds; acquisitions are rejected from the first Close onward.
- `TestResourceRegistryCloseRetriesResidualButNotTerminalReleaseError`: one entry has a retryable residual and another returns `errCloseFixture` terminally. The terminal callback runs once; after retry, final registry error still satisfies `errors.Is(err, errCloseFixture)`.
- `TestResourceRegistryConcurrentFinalReleaseAndCloseSerializeAttempts`: release and registry Close race; there is one active finalizer call, one reference decrement, and one terminal resource release.
- Update `TestResourceRegistryLeaseConcurrentReleaseRunsCloseOnce`: terminal errors replay; retryable residual attempts may run again only on a later call after the first attempt ends.

- [ ] **Step 4: Run RED**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime -run "^(TestResourceRegistryFinalReleaseResidualRetainsResourceForRetry|TestResourceRegistryAcquireWaitsForClosingIdentityBeforeReplacement|TestResourceRegistryAcquireClosingIdentityHonorsContext|TestResourceRegistryAcquireClosingIdentityReturnsRegistryClosed|TestResourceRegistryCloseResidualRetainsEntriesAndRetries|TestResourceRegistryCloseRetriesResidualButNotTerminalReleaseError|TestResourceRegistryConcurrentFinalReleaseAndCloseSerializeAttempts)$" -count=1'
```

Expected: FAIL because entry/lease/registry `sync.Once` values cache the first timeout and the key is detached before terminal close.

- [ ] **Step 5: Implement explicit accepting, closing, and terminal entry state**

Replace entry `closeOnce` with serialized close-attempt fields and a terminal barrier:

```go
type resourceEntry struct {
	ready         chan struct{}
	value         any
	closeResource func(context.Context) error
	createErr     error
	creatorCanceled bool
	references    int

	closeMu          sync.Mutex
	closeInProgress  bool
	closeAttemptDone chan struct{}
	terminal         bool
	terminalErr      error
	terminalDone     chan struct{}
}
```

Initialize `terminalDone` with every entry. A close attempt waits for `ready`, serializes with another attempt, and calls `closeResource(ctx)` outside all registry/entry locks. If the returned error contains `*TaskResidualError` or is a context error, publish it as an incomplete attempt and leave `terminalDone` open. Otherwise cache it as terminal, close `terminalDone`, and never call the finalizer again.

Replace `closing map[*resourceEntry]struct{}` with `closing map[ResourceKey]*resourceEntry`; add one registry terminal-close channel initialized by `NewResourceRegistry`. The final reference moves the exact entry from `entries[key]` to `closing[key]` before invoking close. It removes the closing mapping only after entry terminal completion.

Replace registry `closeOnce` with these exact fields:

```go
type resourceRegistryCloseAttempt struct {
	done chan struct{}
	err  error
}

closedDone    chan struct{}
closeAttempt  *resourceRegistryCloseAttempt
closeRecorded map[*resourceEntry]struct{}
closeErrors   []error
closeTerminal bool
closeErr      error
```

All are protected by `ResourceRegistry.mu`. `closeRecorded` prevents a terminal entry error from being appended again on a later registry retry.

- [ ] **Step 6: Make the same final lease retry without decrementing twice**

Replace lease `sync.Once` with a mutex and explicit state:

```go
type ResourceLease[T any] struct {
	value    T
	registry *ResourceRegistry
	key      ResourceKey
	entry    *resourceEntry

	mu                sync.Mutex
	referenceReleased bool
	finalReference    bool
	terminal          bool
	terminalErr       error
}
```

On the first `Release`, decrement this lease's reference once and record whether it became the final reference. A non-final lease replays its completed release result. The final lease invokes or joins the entry close attempt; if incomplete, it retains retry authority. A later `Release` calls close again without decrementing. After entry terminal completion it caches/replays the terminal error. Preserve type-mismatch and canceled-acquisition joining of a terminal final-close error.

- [ ] **Step 7: Make Acquire wait behind closing and make registry Close retry**

In `Acquire`, reserve returns one of three internal outcomes: creator/accepted entry, closing terminal barrier, or registry closed. For closing:

```go
select {
case <-entry.terminalDone:
	continue
case <-registry.closedDone:
	return nil, ErrResourceRegistryClosed
case <-ctx.Done():
	return nil, ctx.Err()
}
```

After `entry.terminalDone`, loop; normal reserve creates the replacement only if registry remains open.

Registry `Close` sets `closed = true` and closes `closedDone` once, preventing and waking acquisitions. Every attempt snapshots both accepting and closing entries, attempts each terminal close once, retains incomplete entries, and accumulates independent terminal errors once. It becomes terminal only after every entry is terminal, then caches `errors.Join(terminalErrors...)`. A later Close retries only incomplete entries. `Len` continues to count accepting entries only.

- [ ] **Step 8: Run GREEN, existing registry suite, and race**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime -run "^TestResourceRegistry" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/runtime -run "^(TestResourceRegistryConcurrentFirstAcquireHasSingleCreator|TestResourceRegistryFinalReleaseResidualRetainsResourceForRetry|TestResourceRegistryAcquireWaitsForClosingIdentityBeforeReplacement|TestResourceRegistryConcurrentFinalReleaseAndCloseSerializeAttempts|TestResourceRegistryCloseResidualRetainsEntriesAndRetries|TestResourceRegistryLeaseConcurrentReleaseRunsCloseOnce)$" -count=1'
```

Expected: PASS; no replacement overlaps a closing resource, context-bound acquisitions return, the same final lease can retry, registry close is terminal only after all entries are terminal, and terminal callbacks remain exact-once.

- [ ] **Step 9: Review and commit**

Review reference decrement, map movement, terminal barrier close, and finalizer invocation as four distinct events. No path may perform any event twice.

```bash
git add pkg/runtime/resource_registry.go pkg/runtime/resource_registry_test.go
git commit -m "fix(runtime): retry shared resource final close"
```

---

### Task 8: Run the Integrated Task11-0 Gate and Hand Off Both Dependency Edges

**Files:**

- Verify all files from Tasks 1-7.
- Do not modify Contract C's `pkg/runtime/goroutine_contract_test.go`.
- Do not edit generation/background or request/connection production files in this task.

**Interfaces:**

- Consumes: Tasks 1-7 and reviewed Contract C Task 1.
- Produces: one reviewed retryable teardown substrate for generation/shared-resource work; request plan remains independently unblocked after Contract C Task 1.

- [ ] **Step 1: Run focused normal suites**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime -run "^(TestTaskRegistry|TestTaskResidualError|TestResourceRegistry)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^(TestCleanupStack|TestPreparedGeneration|TestWorkerCompilerFactory)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestGenerationOwner|TestGenerationEngine|TestServerShutdown|TestServerRepeatedShutdown)" -count=1'
```

Expected: PASS. If a regex reports no tests, record it and run the exact named tests from its owning task; `[no tests to run]` is not passing evidence.

- [ ] **Step 2: Run the integrated race gate**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/runtime ./pkg/compiler ./pkg/server -run "(Residual|Retry|Concurrent.*Close|Concurrent.*Release|Shutdown|Discard|Retirement|CleanupStack)" -count=1'
```

Expected: PASS with no race and with at least one selected test in each package.

- [ ] **Step 3: Run scoped lint, build, and diff checks**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/runtime/... ./pkg/compiler/... ./pkg/server/...'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
git diff --check b0220dcebd64a1d2d687be84d1f14ab501dfffd0..HEAD
```

Expected: lint reports zero issues, build succeeds, and diff check prints nothing.

- [ ] **Step 4: Run API, placeholder, and lifecycle scans**

```bash
rg -n 'type TaskResidualError|func \(e \*TaskResidualError\) (Error|Unwrap|Residuals)' pkg/runtime/task_registry.go
rg -n 'func \(e \*TaskResidualError\) (Done|Wait|Complete|Retry|Status)|func \(r \*TaskRegistry\) (Done|Wait|Complete|Retry|Status)' pkg/runtime
rg -n 'closeOnce|releaseOnce' pkg/compiler/cleanup.go pkg/compiler/prepared_generation.go \
  pkg/compiler/worker_factory.go pkg/server/generation_owner.go pkg/server/generation_engine.go \
  pkg/runtime/resource_registry.go
rg -n 'T[B]D|T[O]DO|implement la[t]er|fill in det[a]ils|similar to Ta[s]k' \
  docs/superpowers/plans/2026-08-26-immutable-task11-retryable-teardown-residuals.md
```

Expected:

- first scan shows exactly `Error`, `Unwrap`, and `Residuals` for the carrier;
- second scan returns no new status API;
- third scan returns no first-attempt cache controlling a retryable teardown boundary; an unrelated exact-once field requires a line-by-line written justification;
- red-flag scan returns no unresolved implementation text.

- [ ] **Step 5: Perform an independent merge-level review**

Review the exact Task11-0 commit range for:

1. `errors.Is` context preservation and defensive residual copies.
2. No release after any failed quiescer.
3. Prepared generation retains every resource and its factory map entry across a residual.
4. Factory registry close occurs only after every live generation is terminal.
5. `generationOwner.closeDone` means terminal close.
6. Pending discard and retirement state survive incomplete attempts.
7. Engine Close has no unbounded wait after caller cancellation and no retry spin.
8. Server cannot reach resolver/journal/observability before terminal engine close.
9. A closing resource key admits neither a reference nor a replacement factory.
10. Reference decrement, release callback, owner detach, metric unregister, and terminal barrier close are each exact-once.
11. Existing raw cleanup details remain redacted at compiler safe-marker boundaries.
12. No product file outside the Task 1-7 responsibility table changed.

Any confirmed finding returns to its owning task, repeats that task's smallest RED/GREEN/race gate, and then repeats the affected integrated checks.

- [ ] **Step 6: Create an integration correction commit only when reviewed corrections exist**

Do not create an empty commit. If the independent review requires a cross-task correction in files already owned by this plan:

```bash
git add pkg/runtime/task_registry.go pkg/runtime/task_registry_test.go \
  pkg/runtime/resource_registry.go pkg/runtime/resource_registry_test.go \
  pkg/compiler/cleanup.go pkg/compiler/cleanup_test.go \
  pkg/compiler/prepared_generation.go pkg/compiler/worker_factory.go \
  pkg/compiler/worker_factory_close_test.go pkg/server/generation_owner.go \
  pkg/server/generation_owner_test.go pkg/server/generation_engine.go \
  pkg/server/generation_engine_test.go pkg/server/server.go pkg/server/server_test.go
git commit -m "fix(runtime): complete retryable teardown integration"
```

- [ ] **Step 7: Hand off exact dependency evidence**

Return:

- integrated head SHA and ordered Task11-0 commits;
- exact normal/race/lint/build/diff commands and results;
- final `TaskResidualError` method set;
- RED evidence that first-attempt cleanup released resources on the old base and GREEN evidence that retry retained then released them once;
- closing-key acquisition behavior and its context/registry-close results;
- confirmation that generation/background work now has both prerequisites: Contract C Task 2 and Task11-0;
- confirmation that request/connection work remains eligible after Contract C Task 1, subject to its documented AI shared-file ordering;
- explicit list of broad tests not run: repository-wide unit aggregation and integration suite unless separately authorized.

---

## Self-Review Checklist

- **Spec coverage:** Tasks 1-7 cover structured residual propagation, retryable cleanup, PreparedGeneration, WorkerCompilerFactory, generationOwner, GenerationEngine discard/retirement/Close, server shutdown phase retry, and ResourceRegistry final-close retry.
- **Ordering:** Generation/background/shared-resource work waits for Contract C Task 2 plus Task11-0. Request/connection work may proceed after Contract C Task 1.
- **API restraint:** `TaskRegistry.Stop` is unchanged; `TaskResidualError` has only stable `Error`, `Unwrap`, and defensive `Residuals`; no completion/status channel is exposed.
- **Barrier correctness:** every failed quiescer blocks all release callbacks; terminal releases are attempted once and replay their errors.
- **Ownership correctness:** a residual retains prepared fields, factory membership, engine records, metrics, registry closing entries, and the actual resource.
- **Retry correctness:** retries are triggered by later close/discard/shutdown calls or existing internal barriers, never a busy loop.
- **Error correctness:** context causes survive `errors.Is`, residual data survives `errors.As`, unrelated terminal errors remain joined, and compiler raw cleanup details stay redacted.
- **Acquisition correctness:** a key in closing state blocks same-key replacement, honors acquisition context, and returns registry-closed when terminal registry shutdown wins.
- **Verification:** every behavior task has named RED/GREEN/race commands, scoped lint/build/diff gates, exact files, review checks, and a local commit subject.
- **Scope:** no dependency, product feature, public owner status API, push, PR, broad test aggregation, or unrelated refactor is included.
