# Immutable Task 11 Generation Background Ownership Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Use `superpowers:test-driven-development` for every behavior change and `superpowers:verification-before-completion` before each commit.

**Goal:** Assign every generation/plugin background loop and logger shutdown helper an explicit lifecycle owner, preserve existing drain and cleanup semantics, and make cancellation-ignoring work visible without binding cross-generation resources to the wrong generation.

**Architecture:** Ordinary plugin-instance loops use the compiler-injected concrete `*runtime.TaskOwner`; plugins supply only a fixed component to `owner.Go`. Resources reused across generations do not use the creating generation owner: HTTP clusters and the process-global file-writer registry create lifecycle-local registries plus their own `TaskCore` owners. The persistent stream runtime similarly owns one runtime-local core registry across RouterSource generation changes. Logger delivery workers are generation tasks; generation cancellation seals and drains their queue, and a stuck callback remains an observable task residual. Request-owned concurrency is deliberately excluded and handed to its own Task 11 subplan.

**Tech Stack:** Go 1.26, `runtime.TaskRegistry`, compiler `PreparedGeneration`, plugin `base.Dependencies`, `runtime.ResourceRegistry`, `sync`, `context`, Go AST tests, existing package-focused Go tests and race detector.

**Spec:** `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`, Task 11; `docs/superpowers/plans/2026-08-23-runtime-safety-observability.md`, Task 5; `docs/superpowers/plans/2026-08-26-immutable-task11-retryable-teardown-residuals.md`, stable dependency label `Task11-0 / retryable teardown and residual propagation`.

## Global Constraints

- Run every Go command as `bash -lc 'source .envrc && ...'` from the worktree root.
- Preserve APISIX behavior, current bounded queues, retries, coalescing, final flushes, last-good OAS validator publication, active/passive health transitions, and file reopen semantics.
- Do not add an AST allowlist inside Contract C's scan roots: `pkg/plugin`, `pkg/proxy`, `pkg/route`, and `pkg/stream`. After this plan and the request plan integrate, those roots contain no production raw `go` statement or `sync.WaitGroup.Go`. The canonical runtime primitives live under `pkg/runtime`, outside the gate's scan roots; that directory boundary is not an allowlist entry.
- Do not bind a shared resource to `PreparedGeneration.tasks`. A task registry must live at least as long as the state it mutates.
- Every `runtime.TaskSpec.Owner` is stable and bounded. Do not include request data, URLs, target addresses, log paths, raw resource IDs, or secret/config plaintext. Plugin owners consume Contract C's canonical bounded hashed prefix; shared-resource owners use the exact bounded digests frozen below. Never use raw `InstanceKey.String()`.
- Plugins never construct `runtime.TaskSpec`, concatenate a prefix, or select criticality. They call the injected `TaskOwner.Go` with a fixed validated component. Shared cluster/file-writer resources create their own `TaskCore` owners as frozen by Contract C.
- `TaskPlugin` and `TaskCore` have intentionally different panic contracts. `TaskPlugin` panics are recovered by `TaskRegistry`, reported with the exact full owner, and fail only that owner. `TaskCore` panics are not recovered: active-health, file-writer signal, and persistent stream runtime panic tests must use subprocesses and prove worker-fatal termination rather than claiming report-and-continue isolation.
- A task function returns `nil` after expected cancellation. It returns an error only when the owner should be reported failed. Never format panic values or secrets into an owner or log.
- No dependency changes. No broad `go test ./...`, `go test ./pkg/...`, or `make test`.
- Each behavior implementation task is one independently reviewable commit. Before every behavior commit, run its focused normal/race tests, scoped lint for touched packages, `make build`, and `git diff --check`.

## Required Contract Dependencies

Every implementation task in this plan starts only after both the reviewed Contract C Task 2 head and `Task11-0 / retryable teardown and residual propagation` are integrated. Contract C owns `runtime.TaskOwner`, `base.Dependencies`, compiler injection, the error-log observer seam, and the final syntax-plus-type goroutine gate. Task11-0 owns retryable `ResourceRegistry` final-close state, residual propagation, and the compiler cleanup phases `cleanupQuiesce -> cleanupResourceFinalize -> cleanupRelease`. A retryable resource finalizer remains pending and blocks every one-shot release. This plan consumes both contracts and must not reimplement them. The current compiler has no `materialize.go`; its real injection point is `pkg/compiler/effective_binding_materializer.go`, where `selected.instance` is already validated.

```go
// Supplied by Contract C Task 2; consumed unchanged here.
type Dependencies struct {
	// existing fields unchanged
	Tasks *runtime.TaskOwner
}

func (p *BasePlugin) TaskOwner() *runtime.TaskOwner
```

Contract C computes `pluginTaskOwnerPrefix(selected.instance)` as `plugin/<1-48-byte-sanitized-factory>/<64-lowercase-hex-sha256>`, constructs the `TaskOwner` before factory construction, and proves every canonical identity field participates without exposing raw provenance. Direct package tests create a registry, then a `TaskOwner` with a constant bounded `plugin/test/<case>` prefix. This plan must not invent `TaskRegistry()`, `InstanceKey()`, prefix accessors, child owners, or stop methods on `TaskOwner`.

Digest-reused resources require a second, separate rule, not another field on `base.Dependencies`:

- `proxy.Cluster` is acquired from the factory-wide `runtime.ResourceRegistry`. Its factory calls `proxy.NewOwnedCluster(config, observer, onFailure)`, which computes `ClusterKey`, creates a resource-local `TaskRegistry`, then `runtime.NewTaskOwner(registry, "core/proxy-cluster/"+hexKey, runtime.TaskCore)`. Every target task uses the same component `active-health`, so residuals contain exactly one deduplicated full owner. `(*Cluster).CloseContext(ctx) error` stops that registry before closing transports. The prepared generation registers its HTTP cluster lease with `cleanup.Own(cleanupResourceFinalize, "http-cluster/"+owned.Name, slot.release)`, never `cleanupRelease`: generation tasks quiesce first, the retryable final lease runs second, and registration/consumer/plugin one-shot releases run only after every resource finalizer is terminal. Under Task11-0, a deadline/residual keeps the resource entry in closing state and makes the same final lease retryable; cluster, observer lease, and transports remain owned until a later successful retry. Generation N retirement must not cancel a cluster still leased by N+1.
- `sharedFileWriters` is process-global. Each non-empty watcher epoch creates a registry-local `TaskRegistry`, then `runtime.NewTaskOwner(registry, "core/file-writer-registry", runtime.TaskCore)`, and uses component `signal-watch`; the last lease stops and joins that epoch before closing the last writer. It never receives the plugin generation owner.

If the runtime-contract subplan does not expose the generation preparation `TaskFailure` sink to `PreparedGeneration.acquireHTTPCluster`, add exactly one stored package-private callback on `PreparedGeneration`, populated by `transferRegisteredGeneration`; do not add a second public task API.

## Current Production Inventory and Routing

The inventory command is:

```bash
rg -n '\bgo[[:space:]]+|\.Go\(' pkg/plugin pkg/proxy pkg/route pkg/stream --glob '*.go' --glob '!*_test.go'
```

| Current production owner | Current sites | Classification and destination |
| --- | --- | --- |
| Plugin/generation background | `proxy_cache/disk.go`, `limit_count/delayed_sync.go`, `ai_proxy_multi/health.go`, `oas_validator/plugin.go`, `graphql_proxy_cache/plugin.go`, `log_rotate/plugin.go`, `logger_batch/processor.go`, `file_logger/processor.go`, `rocketmq_logger/plugin.go` | In this plan; compiler-injected `TaskPlugin` owner and fixed components |
| Cross-generation resource | `proxy/active_health.go` through `compiler/http_cluster.go` | In this plan; resource-local `TaskCore` owner, never generation owner |
| Process-global registry | `file_logger/writer_registry.go` | In this plan; registry-local `TaskCore` watcher epoch |
| Logger cancellation helper | `error_log_logger`, `tcp_logger`, `syslog`, `udp_logger`, `datadog`, `sls_logger`, `loggly` | In this plan; replace local `WaitGroup.Go` with joined `context.AfterFunc` cancellation callback |
| Request-owned periodic flush | `ai_stream/flush_writer.go` | Not modified here; the request Task 11 plan explicitly owns its flush loop and Close/join behavior |
| Other request/connection owned | `batch_requests`, `kafka_proxy`, `proxy_mirror`, `mqtt_proxy`, `mcp_bridge`, `stream/bridge` | Not modified here; the request Task 11 plan owns them |
| Persistent server runtime | `stream/runtime.go` | In this plan; runtime-local `TaskCore` owner because `stream.Runtime` persists while RouterSource generations change |
| Canonical ownership primitive | `runtime/task_registry.go`, `runtime/request_tasks.go` | Outside the four AST scan roots; not an allowlist entry and not modified here |

The implementer must rerun the command immediately before Task 12. Any new production result must be assigned to this plan or the request plan before Contract C's AST gate can pass.

## File Responsibility and Dependency Order

| Task | Exclusive production responsibility | Consumes | Produces | Depends on |
| --- | --- | --- | --- | --- |
| 1 | `pkg/proxy/active_health.go`, `cluster.go`; `pkg/compiler/http_cluster.go`; `pkg/server/generation_engine_test.go` | resource registry, ClusterKey, failure sink, `cleanupResourceFinalize` | cross-generation resource-owned health and real engine/factory/Prepared final-lease retry evidence | Contract C Task 2 + Task11-0 teardown |
| 2 | `pkg/plugin/proxy_cache/disk.go`, `plugin.go` | plugin task owner | owned disk sweep | Contract C Task 2 + Task11-0 teardown |
| 3 | `pkg/plugin/graphql_proxy_cache/plugin.go` | plugin task owner | owned GraphQL disk sweep | Contract C Task 2 + Task11-0 teardown |
| 4 | `pkg/plugin/limit_count/delayed_sync.go`, `plugin.go` | plugin task owner | owned delayed sync and final flush | Contract C Task 2 + Task11-0 teardown |
| 5 | `pkg/plugin/ai_proxy_multi/health.go`, `plugin.go`, `plugin_test.go` | plugin task owner | owned health coordinator and probes | Contract C Task 2 + Task11-0 teardown |
| 6 | `pkg/plugin/oas_validator/plugin.go` | plugin task owner | owned lazy refresh loop | Contract C Task 2 + Task11-0 teardown |
| 7 | `pkg/plugin/log_rotate/plugin.go` | plugin task owner | owned coalescing rotation worker | Contract C Task 2 + Task11-0 teardown |
| 8 | `pkg/plugin/logger_batch`, all production logger constructors, error-log delegation tests, `pkg/server/task_residual_chain_test.go` | plugin task owner, frozen Task11-0 deferred compatibility checkpoint | generation-owned delivery/scheduler/shutdown plus exact real-chain residual evidence | Contract C Task 2 + complete Task11-0 hand-off |
| 9 | `pkg/plugin/file_logger` | plugin owner plus registry-local owner | owned processor and SIGUSR1 watcher | Contract C Task 2 + Task11-0 teardown + 8 |
| 10 | seven logger cancellation helpers and `rocketmq_logger` | owned logger delivery lifecycle | joined cancellation and sender shutdown | Contract C Task 2 + Task11-0 teardown + 8; C owns error-log observer seam first |
| 11 | `pkg/stream/runtime.go` | runtime-local core task owner | owned listener/connection lifetime across router generations | Contract C Task 2 + Task11-0 teardown |
| 12 | no new behavior | all earlier tasks, request plan, and Contract C Task 5 | final inventory/gates | Contract C Task 2 + Task11-0 teardown + 1-11 + request plan + Contract C Task 5 |

Tasks 1-7 and 11 can be developed as a frozen-base parallel wave only after the reviewed Contract C Task 2 and Task11-0 teardown heads are integrated. Tasks 8-10 are sequential because the concrete logger processor lifecycle is their shared interface. Generation Task 8 starts from the complete Task11-0 implementation/handoff head: Task11-0 Tasks 1-7 and its prerequisite integration gates are terminal, while Task11-0 Task 8 Step 3 is an explicitly deferred compatibility obligation implemented inside Generation Task 8. That deferred test is not a prerequisite for starting Generation Task 8, so there is no dependency cycle. Generation Task 5 owns `pkg/plugin/ai_proxy_multi/plugin.go` and `plugin_test.go`; request-plan AI flush Task 7 must start from Generation Task 5's reviewed integrated head, never in parallel. Task 12 runs only after this plan, the request plan, and Contract C Task 5 are integrated.

---

### Task 1: Give Active Health the Digest-Keyed Cluster Resource Lifetime

**Files:**

- Modify: `pkg/proxy/active_health.go`
- Modify: `pkg/proxy/active_health_test.go`
- Modify: `pkg/proxy/cluster.go`
- Modify: `pkg/proxy/cluster_test.go`
- Modify: `pkg/compiler/http_cluster.go`
- Modify: `pkg/compiler/http_cluster_test.go`
- Modify: `pkg/server/generation_engine_test.go`

**Consumes:** Factory-wide cluster `ResourceRegistry`, canonical `ClusterKey`, task failure sink, and Task11-0's exact `cleanupResourceFinalize` plus retryable final-release/closing-entry contract.

**Produces:** `core/proxy-cluster/<hex-digest>/active-health` `TaskCore` workers stopped only by the last resource lease. Multiple target workers intentionally share this exact full owner and are deduplicated in residuals.

- [ ] **Step 1: Write cross-generation RED tests**

```go
func TestHTTPClusterHealthOutlivesCreatingGenerationWhileReused(t *testing.T) {
	old, next, resourceTasks, wantOwner := prepareTwoGenerationsWithSameActiveHealthCluster(t)
	oldCluster := acquireFixtureCluster(t, old)
	nextCluster := acquireFixtureCluster(t, next)
	if oldCluster != nextCluster { t.Fatal("equal cluster config was not reused") }
	closePreparedGeneration(t, old)
	awaitProbe(t, nextCluster)
	if got := resourceTasks.Active(); !reflect.DeepEqual(got, []string{wantOwner}) { t.Fatalf("owners = %v", got) }
	closePreparedGeneration(t, next)
	assertClusterHealthStopped(t, nextCluster)
}
```

Also add this retry regression. Task11-0's own runtime test proves the registry retains the closing entry; this compiler/proxy test proves the cluster-specific resources remain attached to that entry.

```go
func TestHTTPClusterFinalReleaseRetriesAfterHealthResidual(t *testing.T) {
	lease, cluster, releaseProbe, observerReleases, transportCloses, wantOwner :=
		newBlockingOwnedClusterLease(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := lease.Release(ctx)
	assertTaskResidualError(t, err, []runtime.TaskResidual{{Owner: wantOwner}})
	if wantOwner != "core/proxy-cluster/"+clusterKeyHex(t, cluster)+"/active-health" {
		t.Fatalf("owner = %q", wantOwner)
	}
	if observerReleases.Load() != 0 || transportCloses.Load() != 0 {
		t.Fatalf("deadline teardown released observer=%d transport=%d",
			observerReleases.Load(), transportCloses.Load())
	}
	close(releaseProbe)
	if err := lease.Release(context.Background()); err != nil { t.Fatal(err) }
	if observerReleases.Load() != 1 || transportCloses.Load() != 1 {
		t.Fatalf("successful retry releases = observer:%d transport:%d",
			observerReleases.Load(), transportCloses.Load())
	}
}
```

Add this end-to-end regression in `pkg/server/generation_engine_test.go`. Its test-local fixture builds an actual `GenerationEngine` and `WorkerCompilerFactory`, prepares a real HTTP route with an inline upstream and active check, and installs a `proxy.ClusterObserver` whose `SetHealth` callback has two independently releasable gates. `activeHealthGenerationInput` returns the exact expected owner by hashing the same effective `proxy.ClusterConfig`; it does not inspect a private registry or add a production API.

```go
func TestGenerationEngineDiscardRetriesHTTPClusterResourceFinalization(t *testing.T) {
	fixture, observer, target := newActiveHealthGenerationEngineFixture(t)
	firstGate := observer.NextGate(t)
	firstTicket, firstDesired, wantOwner := activeHealthGenerationInput(t, 1, target)
	firstSet, err := fixture.engine.Prepare(context.Background(), firstTicket, firstDesired, nil)
	if err != nil { t.Fatal(err) }
	firstGate.AwaitEntered(t)

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	first := fixture.engine.DiscardPrepared(short, firstSet)
	var residual *runtime.TaskResidualError
	if !errors.As(first, &residual) || !errors.Is(first, context.DeadlineExceeded) ||
		!reflect.DeepEqual(residual.Residuals(), []runtime.TaskResidual{{Owner: wantOwner}}) {
		t.Fatalf("first discard = %v, residuals = %v", first, residual)
	}
	key := mustEnginePreparedKey(t, firstSet)
	if fixture.engine.pending[key] == nil {
		t.Fatal("resource-finalization residual detached the Prepared/factory owner")
	}

	firstGate.Release()
	if err := fixture.engine.DiscardPrepared(context.Background(), firstSet); err != nil {
		t.Fatalf("retry discard = %v", err)
	}
	if fixture.engine.pending[key] != nil {
		t.Fatal("terminal retry retained pending ownership")
	}

	secondGate := observer.NextGate(t)
	secondTicket, secondDesired, secondOwner := activeHealthGenerationInput(t, 2, target)
	if secondOwner != wantOwner { t.Fatalf("same config owner changed: %q / %q", wantOwner, secondOwner) }
	secondSet, err := fixture.engine.Prepare(context.Background(), secondTicket, secondDesired, nil)
	if err != nil { t.Fatalf("same-key Prepare after retry = %v", err) }
	secondGate.AwaitEntered(t) // a replacement was acquired only after the closing entry became terminal
	secondGate.Release()
	if err := fixture.engine.DiscardPrepared(context.Background(), secondSet); err != nil { t.Fatal(err) }
}
```

This test must use `GenerationEngine.Prepare` and `DiscardPrepared`; it must not call `runtime.Acquire`, `PreparedGeneration.Close`, or the cluster finalizer directly. Therefore the residual crosses the real engine record, factory-owned `PreparedGeneration`, cleanup phase machine, final lease, and cluster-local task registry. The first failed attempt must retain the pending record and closing resource; the explicit exact-set retry completes retirement; a later `Prepare` of the same cluster key must acquire a replacement rather than hang behind or overlap the old entry.

Add `TestActiveHealthTaskPanicUsesCoreFatalPolicy` in `pkg/proxy/active_health_test.go`. Its subprocess uses an observer that panics with a stable marker from `SetHealth`. Assert non-zero exit and the marker in combined output; do not install an expectation that `TaskFailure` is reported or that the process continues.

```go
func TestActiveHealthTaskPanicUsesCoreFatalPolicy(t *testing.T) {
	if os.Getenv("APISIX_GO_TEST_ACTIVE_HEALTH_CORE_PANIC") == "1" {
		runActiveHealthCorePanicFixture(t, "active-health-core-fatal")
		fmt.Fprintln(os.Stderr, "active-health-returned-after-core-panic")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestActiveHealthTaskPanicUsesCoreFatalPolicy$")
	cmd.Env = append(os.Environ(), "APISIX_GO_TEST_ACTIVE_HEALTH_CORE_PANIC=1")
	output, err := cmd.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("active-health-core-fatal")) ||
		bytes.Contains(output, []byte("active-health-returned-after-core-panic")) {
		t.Fatalf("core panic subprocess = %v, output = %s", err, output)
	}
}
```

`runActiveHealthCorePanicFixture` is test-local: construct an owned cluster with one immediately failing health target, make the observer panic with the marker on its first transition, and block the helper caller until the task either panics or incorrectly returns. Do not recover inside the helper.

Generation N close alone must not report a cluster residual while N+1 retains the lease. On the final release, every target shares the exact `wantOwner`; even several blocked probes produce one deduplicated residual entry independent of target identity.

- [ ] **Step 2: Capture RED**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler ./pkg/proxy ./pkg/server -run "^(TestHTTPClusterHealthOutlivesCreatingGenerationWhileReused|TestHTTPClusterFinalReleaseRetriesAfterHealthResidual|TestGenerationEngineDiscardRetriesHTTPClusterResourceFinalization|TestActiveHealthTaskPanicUsesCoreFatalPolicy)$" -count=1'
```

Expected: FAIL because, even after Task11-0 has installed the correct cleanup phase, `activeHealthChecker.Start` still creates raw goroutines and `Cluster.Close` has no context-bounded retryable resource owner.

- [ ] **Step 3: Implement a resource-local registry**

Change the internal cluster constructor to require an owned task registry and digest owner. `Start` registers one task per target; `probeTarget(ctx, index, target)` selects on the supplied context. Do not put target text in the owner.

```go
cluster, err := proxy.NewOwnedCluster(owned, observerLease.Observer(), prepared.taskFailure)
```

`NewOwnedCluster` computes the prefix `"core/proxy-cluster/" + hex.EncodeToString(digest[:])`, constructs a `TaskCore` owner, and calls `owner.Go("active-health", ...)` for every target. All targets therefore share the exact full owner and residuals deduplicate to one entry.

Task11-0 is already integrated before this task and has registered the HTTP lease in its middle phase:

```go
if err := prepared.cleanup.Own(
	cleanupResourceFinalize,
	"http-cluster/"+owned.Name,
	slot.release,
); err != nil {
	return nil, err
}
```

Preserve this registration while changing the cluster factory to `NewOwnedCluster`; do not move it back to `cleanupRelease` or wrap the lease in a new one-shot callback. `cleanupQuiesce` must finish every generation-owned plugin task before this callback runs. A retryable `slot.release` result remains pending in `cleanupResourceFinalize` and prohibits `cleanupRelease`; a terminal resource-finalization error is recorded once while the phase continues, and only after every resource finalizer is terminal may one-shot registration, consumer, plugin, observer, and secret releases run.

The resource finalizer enforces this order:

```go
if err := cluster.CloseContext(ctx); err != nil {
	return err // Task11-0 retains the closing entry and retries this same finalizer.
}
observerLease.Release()
return nil
```

`Cluster.CloseContext` first retries its resource-local registry `Stop(ctx)`. If Stop returns an error or residual, it returns immediately without closing the active-health transports or changing the transport/observer release state. Only a successful Stop closes transports through their own exactly-once guard. A later final-lease `Release` retry re-enters this finalizer; after probes exit it closes transports once, releases the observer once, and lets Task11-0 delete the closing entry. Do not cache the first deadline as a terminal cluster/resource close result.

- [ ] **Step 4: GREEN and race**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler ./pkg/proxy ./pkg/server -run "^(TestHTTPCluster|TestCluster|TestActiveHealth|TestGenerationEngineDiscardRetriesHTTPClusterResourceFinalization)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler ./pkg/proxy ./pkg/server -run "^(TestHTTPClusterHealthOutlivesCreatingGenerationWhileReused|TestHTTPClusterFinalReleaseRetriesAfterHealthResidual|TestGenerationEngineDiscardRetriesHTTPClusterResourceFinalization|TestActiveHealthTaskPanicUsesCoreFatalPolicy)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/compiler/... ./pkg/proxy/... ./pkg/server/... && make build'
git diff --check
```

- [ ] **Step 5: Commit**

```bash
git add pkg/proxy/active_health.go pkg/proxy/active_health_test.go pkg/proxy/cluster.go pkg/proxy/cluster_test.go pkg/compiler/http_cluster.go pkg/compiler/http_cluster_test.go pkg/server/generation_engine_test.go
git commit -m "refactor(proxy): own active health by cluster resource"
```

---

### Task 2: Own Proxy Cache Disk Cleanup by Plugin Instance

**Files:** `pkg/plugin/proxy_cache/disk.go`, `plugin.go`, `plugin_test.go`

**Consumes:** Contract C's `BasePlugin.TaskOwner() *runtime.TaskOwner`.

**Produces:** `<instance-owner>/disk-cleanup` `TaskPlugin` loop; generation quiescence joins it before `Stop` releases memory-zone resources.

- [ ] Add `TestDiskBackgroundExpirySweepUsesGenerationTaskOwner` and `TestDiskCleanupPanicFailsOnlyProxyCacheOwner`. Use a test registry failure channel, deterministic cleanup interval, and a cleanup seam that panics. Assert exact owner and that a second plugin owner remains admissible.

```go
func TestDiskBackgroundExpirySweepUsesGenerationTaskOwner(t *testing.T) {
	tasks, failures := newProxyCacheTestTasks(t)
	p := newDiskPluginFixture(t, time.Millisecond)
	owner := newPluginTaskOwnerForTest(t, tasks, "plugin/test/proxy-cache/attempt-1")
	p.SetDependencies(base.Dependencies{Tasks: owner})
	if err := p.PostInit(); err != nil { t.Fatal(err) }
	awaitDiskSweep(t, p)
	if got := tasks.Active(); !slices.Contains(got, "plugin/test/proxy-cache/attempt-1/disk-cleanup") {
		t.Fatalf("active owners = %v", got)
	}
	stopTestRegistry(t, tasks)
	p.Stop()
	assertNoTaskFailure(t, failures)
}
```
- [ ] Run RED:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/proxy_cache -run "^(TestDiskBackgroundExpirySweepUsesGenerationTaskOwner|TestDiskCleanupPanicFailsOnlyProxyCacheOwner)$" -count=1'
```

Expected: FAIL because `startDiskCleanup` does not consult dependencies.

- [ ] Replace `cleanupStop/cleanupDone` goroutine ownership with:

```go
err := p.TaskOwner().Go("disk-cleanup", func(ctx context.Context) error {
	return p.diskCleanupLoop(ctx)
})
```

`PostInit` returns the admission error. `diskCleanupLoop` owns its ticker and exits only from the callback context. Delete `cleanupStop/cleanupDone`; production `Stop` runs after `PreparedGeneration.tasks.Stop` and only releases the memory-zone lease. Update direct package tests to stop their test registry before calling plugin `Stop`; plugin code must not wait a second time for registry-owned work.

- [ ] GREEN/race/gates:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/proxy_cache -run "^(TestDisk|TestPostInit|TestMemoryZone)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/proxy_cache -run "^(TestDisk|TestPostInit|TestMemoryZone)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/proxy_cache/... && make build'
git diff --check
```

- [ ] Commit: `git commit -m "refactor(proxy-cache): own disk cleanup task"`.

---

### Task 3: Own GraphQL Cache Cleanup by Plugin Instance

**Files:** `pkg/plugin/graphql_proxy_cache/plugin.go`, `plugin_test.go`

**Consumes:** plugin task owner and existing disk-store cleanup interval.

**Produces:** `<instance-owner>/disk-cleanup` `TaskPlugin` loop that is joined before memory store and route-cache publication are released.

- [ ] Add `TestGraphQLDiskCleanupUsesGenerationTaskOwner` and `TestGraphQLStopDoesNotRemoveReplacementRouteCache`. The first captures task admission/exit; the second overlaps old/new route instances, retires old, and proves the new `routeCaches.plugins[routeID]` remains published.

```go
func TestGraphQLStopDoesNotRemoveReplacementRouteCache(t *testing.T) {
	old := newOwnedGraphQLCacheFixture(t, "plugin/test/graphql/old", "r1")
	next := newOwnedGraphQLCacheFixture(t, "plugin/test/graphql/new", "r1")
	publishRouteCacheForTest(t, old)
	publishRouteCacheForTest(t, next)
	stopPluginRegistryForTest(t, old)
	old.Stop()
	if got := routeCachePluginForTest("r1"); got != next { t.Fatalf("published plugin = %p, want %p", got, next) }
	stopPluginRegistryForTest(t, next)
	next.Stop()
}
```
- [ ] RED:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/graphql_proxy_cache -run "^(TestGraphQLDiskCleanupUsesGenerationTaskOwner|TestGraphQLStopDoesNotRemoveReplacementRouteCache)$" -count=1'
```

- [ ] Make `startDiskCleanup() error`, call `p.TaskOwner().Go("disk-cleanup", loop)`, and delete the local stop/done pair. Preserve the guarded `routeCaches.plugins[p.routeID] == p` deletion after generation task quiescence. `PostInit` rolls back acquired memory/disk state if task admission fails; direct tests stop their registry before plugin `Stop`.
- [ ] GREEN/race/gates:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/graphql_proxy_cache -run "^(TestGraphQL|TestPostInit|TestHandlerRefreshesExpired)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/graphql_proxy_cache -run "^(TestGraphQL|TestPostInit|TestHandlerRefreshesExpired)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/graphql_proxy_cache/... && make build'
git diff --check
```

- [ ] Commit: `git commit -m "refactor(graphql-cache): own disk cleanup task"`.

---

### Task 4: Own Delayed Limit Synchronization and Preserve Final Flush

**Files:** `pkg/plugin/limit_count/delayed_sync.go`, `delayed_sync_test.go`, `plugin.go`, `plugin_test.go`

**Consumes:** plugin task owner; existing delayed-sync backend and retry state.

**Produces:** one or more tasks sharing the exact full owner `<instance-owner>/delayed-sync`; the registry count tracks concurrent cached syncers and residual output deduplicates the name. Cancellation performs the same complete dirty-state flush as `Stop`.

- [ ] Add tests using the existing fake backend:

```go
func TestDelayedSyncerCancellationFlushesAllDirtyStatesUnderOwnedTask(t *testing.T) {
	registry, failures := testTaskRegistry(t)
	owner := newPluginTaskOwnerForTest(t, registry, "plugin/test/limit-count/attempt-1")
	syncer, err := newDelayedSyncer(owner, backend, 7, 10*time.Second, time.Hour, 2)
	if err != nil { t.Fatal(err) }
	dirtyThreeKeys(t, syncer)
	stopRegistry(t, registry)
	assertAllDeltasFlushedOnce(t, backend)
	assertNoFailure(t, failures)
}
```

Add a cancellation-ignoring backend case that makes `TaskRegistry.Stop(shortCtx)` return the exact owner residual, then releases and proves the next Stop is clean. Preserve `TestDelayedSyncStopFlushesAllDirtyStatesIncludingDroppedQueueEntries`.

- [ ] RED:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/limit_count -run "^(TestDelayedSyncerCancellationFlushesAllDirtyStatesUnderOwnedTask|TestDelayedSyncerBlockingFinalFlushIsVisibleResidual)$" -count=1'
```

- [ ] Change `newDelayedSyncer` to return `(*delayedSyncer, error)` and accept `*runtime.TaskOwner`. Call `owner.Go("delayed-sync", s.run)` before publishing it in `delayedByKey`. `run(ctx)` treats owner cancellation as a request to drain its queue and flush every dirty/retry state once. Delete the syncer's independent stop/join goroutine lifecycle; plugin `Stop` clears backend/state only after generation task quiescence. Roll back backend/secret acquisition if task admission fails.
- [ ] GREEN/race/gates:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/limit_count -run "^(TestDelayedSync|TestLimitCountStop|TestScopedSecretsLimitCountStop)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/limit_count -run "^(TestDelayedSync|TestLimitCountStop|TestScopedSecretsLimitCountStop)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/limit_count/... && make build'
git diff --check
```

- [ ] Commit: `git commit -m "refactor(limit-count): own delayed synchronization"`.

---

### Task 5: Own AI Multi Health Coordination and Probe Fan-Out

**Files:** `pkg/plugin/ai_proxy_multi/health.go`, `plugin.go`, `plugin_test.go`

**Cross-plan file fence:** This task exclusively owns `pkg/plugin/ai_proxy_multi/plugin.go` and `plugin_test.go` first. Request-plan AI flush Task 7 also needs those two files, so it must create/rebase its worktree from this task's reviewed integrated head. The two tasks must not run in parallel or merge sibling-base edits.

**Consumes:** plugin task owner, current immutable health snapshot and per-instance clients.

**Produces:** one `<instance-owner>/health-refresh` coordinator plus bounded tasks sharing `<instance-owner>/health-probe`, all under the already-frozen `TaskPlugin` owner.

- [ ] Add `TestAIHealthUsesAttemptQualifiedTaskOwners`, `TestAIHealthGenerationStopJoinsOwnedInFlightProbes`, and `TestAIHealthProbePanicCompletesPassWithoutPartialPublication`. Do not put provider name or endpoint in expected owners. Preserve coalesced wakes and one refresh pass.

```go
func TestAIHealthGenerationStopJoinsOwnedInFlightProbes(t *testing.T) {
	tasks, _ := newAIHealthTestTasks(t)
	p, probeStarted, release := newBlockingHealthPlugin(t, tasks, "plugin/test/ai-multi/attempt-1")
	p.wakeHealthRefresh()
	<-probeStarted
	stopped := make(chan struct{})
	go func() { stopTestRegistry(t, tasks); p.Stop(); close(stopped) }() // test orchestration only
	assertNotClosed(t, stopped)
	close(release)
	awaitClosed(t, stopped)
}

func TestAIHealthProbePanicCompletesPassWithoutPartialPublication(t *testing.T) {
	tasks, failures := newAIHealthTestTasks(t)
	wantPanic := &struct{ marker string }{marker: "probe-panic"}
	p := newTwoProbeHealthPlugin(t, tasks, "plugin/test/ai-multi/attempt-1")
	before := p.snapshot.Load()
	p.probeForTest = func(_ context.Context, index int) healthProbeResult {
		if index == 0 { panic(wantPanic) }
		return healthyProbeResult(index)
	}
	p.wakeHealthRefresh()
	failure := awaitTaskFailure(t, failures)
	if failure.Owner != "plugin/test/ai-multi/attempt-1/health-probe" || failure.PanicValue != wantPanic {
		t.Fatalf("failure = %#v", failure)
	}
	awaitOwnerExit(t, tasks, "plugin/test/ai-multi/attempt-1/health-refresh")
	if residuals, err := tasks.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop = (%v, %v)", residuals, err)
	}
	if got := p.snapshot.Load(); got != before { t.Fatal("incomplete pass published a partial snapshot") }
}
```
- [ ] RED:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_proxy_multi -run "^(TestAIHealthUsesAttemptQualifiedTaskOwners|TestAIHealthGenerationStopJoinsOwnedInFlightProbes|TestAIHealthProbePanicCompletesPassWithoutPartialPublication)$" -count=1'
```

- [ ] `startHealthLoop() error` calls `p.TaskOwner().Go("health-refresh", p.healthLoop)` before PostInit publishes success. For every successfully admitted `health-probe`, allocate a distinct capacity-one `chan healthProbeCompletion`. Also allocate one pass-local `ready` channel with capacity `len(due)` so completion can be observed in actual finish order without another goroutine. The callback installs its completion defer before any probe code:

```go
type healthProbeCompletion struct {
	index     int
	result    healthProbeResult
	completed bool
}

completion := make(chan healthProbeCompletion, 1)
err := p.TaskOwner().Go("health-probe", func(context.Context) error {
	marker := healthProbeCompletion{index: index, completed: false}
	defer func() {
		completion <- marker
		ready <- completion
	}()
	marker.result = p.probeInstance(passCtx, index)
	if passCtx.Err() != nil { return nil }
	marker.completed = true
	return nil
})
```

Both buffered sends are mandatory and unconditional for every admitted callback: normal completion sends `completed=true`; cancellation, early return, or panic unwinding sends `completed=false` before TaskRegistry handles the panic. `ready` is sized for every due probe, so the defer cannot block while the coordinator is between admission and receive. Admission failure creates no awaited completion, immediately cancels the pass, and marks it incomplete.

- [ ] The coordinator receives exactly the admitted count from `ready`, reads each corresponding capacity-one completion, cancels the pass on the first `completed=false`, and continues draining all admitted completions before returning. It publishes a new immutable snapshot only when admission succeeded for every due probe and all completions are true. Any incomplete marker makes `refreshHealthPass` return `false`; `healthLoop` then exits cleanly without a second task failure. Thus a probe panic reports only `<instance-owner>/health-probe`, the `<instance-owner>/health-refresh` task exits, registry Stop completes, and no partial health result is published. Delete the independent health stop/cancel/done lifecycle; after generation quiescence, `Stop` only closes idle clients and clears state.
- [ ] GREEN/race/gates:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_proxy_multi -run "^(TestHealth|TestPostInitOwnsHealth|TestStopHealth)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/ai_proxy_multi -run "^(TestHealth|TestPostInitOwnsHealth|TestStopHealth)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/ai_proxy_multi/... && make build'
git diff --check
```

- [ ] Commit: `git commit -m "refactor(ai-proxy-multi): own health tasks"`.

After review/integration, publish this commit SHA as the mandatory base for request-plan AI flush Task 7 before that task begins.

---

### Task 6: Own Lazy OAS Refresh Without Losing Last-Good State

**Files:** `pkg/plugin/oas_validator/plugin.go`, `plugin_test.go`, `scoped_secrets_test.go`

**Consumes:** plugin task owner, scoped-secret work retirement, atomic compiled validator.

**Produces:** one lazily admitted `<instance-owner>/spec-refresh` task.

- [ ] Add `TestOASSpecRefreshUsesGenerationTaskOwner`, `TestOASRefreshAdmissionFailureLeavesCurrentValidator`, and `TestOASStopCancelsOwnedFetchBeforeDroppingSecrets`. The last test blocks header-secret use and remote fetch independently and asserts ordering: cancel fetch, join refresh, join secret/work leases, clear secrets/compiled.

```go
func TestOASRefreshAdmissionFailureLeavesCurrentValidator(t *testing.T) {
	registry := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newPluginTaskOwnerForTest(t, registry, "plugin/test/oas/attempt-1")
	stopTestRegistry(t, registry)
	p := newOASFixtureWithCompiledValidator(t, owner)
	want := p.compiled.Load()
	p.wakeSpecRefresh()
	if got := p.compiled.Load(); got != want { t.Fatal("task admission replaced last-good validator") }
}
```
- [ ] RED:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/oas_validator -run "^(TestOASSpecRefreshUsesGenerationTaskOwner|TestOASRefreshAdmissionFailureLeavesCurrentValidator|TestOASStopCancelsOwnedFetchBeforeDroppingSecrets)$" -count=1'
```

- [ ] In `wakeSpecRefresh`, use `sync.Once` only to call `p.TaskOwner().Go("spec-refresh", p.specRefreshLoop)`. Store the admission error so requests can retain the current validator and avoid repeated admission. `specRefreshLoop(ctx)` uses only its owner callback context; failed refresh preserves last-good. Delete the independent refresh cancel/done lifecycle. Generation quiescence cancels/joins the remote fetch before plugin `Stop` retires secret/work leases and clears private state; direct tests stop their registry first.
- [ ] GREEN/race/gates:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/oas_validator -run "^(TestHandlerSpecRefresh|TestHandlerFailedSpecRefresh|TestOASStop|TestScopedSecretsOASStop)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/oas_validator -run "^(TestHandlerSpecRefresh|TestHandlerFailedSpecRefresh|TestOASStop|TestScopedSecretsOASStop)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/oas_validator/... && make build'
git diff --check
```

- [ ] Commit: `git commit -m "refactor(oas-validator): own spec refresh task"`.

---

### Task 7: Own Log Rotation and Keep Requests Non-Blocking

**Files:** `pkg/plugin/log_rotate/plugin.go`, `plugin_test.go`

**Consumes:** plugin task owner, bounded trigger channel, file-logger reopen callback.

**Produces:** `<instance-owner>/rotation` worker under the compiler-frozen `TaskPlugin` owner.

- [ ] Add `TestRotationWorkerUsesGenerationTaskOwner`, `TestRotationTaskCancellationWaitsForInFlightRotate`, and `TestRotationPanicReportsPluginOwner`. Assert request phase still only coalesces a trigger and returns before a blocked rotate finishes.

```go
func TestRotationTaskCancellationWaitsForInFlightRotate(t *testing.T) {
	p, registry, started, release := newBlockingRotationPlugin(t, "plugin/test/log-rotate/attempt-1")
	p.requestRotation()
	<-started
	stopped := make(chan struct{})
	go func() { stopTestRegistry(t, registry); p.Stop(); close(stopped) }() // test orchestration only
	assertNotClosed(t, stopped)
	close(release)
	awaitClosed(t, stopped)
}
```
- [ ] RED:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/log_rotate -run "^(TestRotationWorkerUsesGenerationTaskOwner|TestRotationTaskCancellationWaitsForInFlightRotate|TestRotationPanicReportsPluginOwner)$" -count=1'
```

- [ ] Make PostInit call `p.TaskOwner().Go("rotation", p.rotationWorker)` and return admission failure. `rotationWorker(ctx)` selects on the callback context and trigger; a normal rotate error remains a sanitized plugin log and does not terminate the worker. A panic is left for TaskRegistry recovery. Delete the independent stop/done lifecycle; direct tests stop their registry before plugin `Stop`.
- [ ] GREEN/race/gates:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/log_rotate -run "^(TestRotate|TestRotation|TestPostInit)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/log_rotate -run "^(TestRotate|TestRotation|TestPostInit)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/log_rotate/... && make build'
git diff --check
```

- [ ] Commit: `git commit -m "refactor(log-rotate): own rotation worker"`.

---

### Task 8: Make Logger Batch Workers Generation-Owned

**Files:**

- Modify: `pkg/plugin/logger_batch/processor.go`, `processor_test.go`
- Modify: `pkg/plugin/base/types.go`, `types_test.go`
- Modify: `pkg/plugin/error_log_logger/plugin.go`, `plugin_test.go`
- Modify: `pkg/compiler/effective_binding_materializer.go`, `effective_binding_materializer_test.go`
- Modify: `pkg/server/generation_engine.go`, `generation_engine_test.go`
- Create: `pkg/server/task_residual_chain_test.go`
- Modify every production result of:

```bash
rg -l 'base\.NewBatchProcessor|logger_batch\.New(WithContext)?' pkg/plugin --glob '*.go' --glob '!*_test.go' | sort
```

At the frozen base this is `clickhouse_logger`, `datadog`, `elasticsearch_logger`, `error_log_logger`, `google_cloud_logging`, `http_logger`, `kafka_logger`, `lago`, `loggly`, `loki_logger`, `rocketmq_logger`, `skywalking_logger`, `sls_logger`, `splunk_hec_logging`, `syslog`, `tcp_logger`, `tencent_cloud_cls`, `udp_logger`, `zipkin`, plus `base/types.go`.

**Consumes:** Reviewed Contract C Task 2, the complete Task11-0 implementation/handoff head, the compiler-injected plugin `TaskOwner`, and current bounded delivery/shutdown settings. Task11-0 Task 8 Step 3 is consumed here as a deferred compatibility checkpoint, not as a prerequisite test result.

**Produces:** `<owner>/batch-scheduler`, `<owner>/batch-worker`, and `<owner>/batch-shutdown` tasks under one compiler-frozen `TaskPlugin` owner; multiple workers share/deduplicate `batch-worker`. The private `schedulerDone` barrier makes the deterministic and real-chain residual set exactly sorted `<owner>/batch-shutdown`, `<owner>/batch-worker`; `<owner>/observer` and `<owner>/batch-scheduler` are forbidden from that snapshot. There is no `context.Background` delivery root or detached cleanup waiter.

- [ ] **Step 1: Add primitive ownership and shutdown RED tests**

```go
func TestProcessorRegistersOwnedWorkersAndDrainsOnRegistryStop(t *testing.T) {
	registry, failures := testTaskRegistry(t)
	owner := newLoggerTaskOwnerForTest(t, registry, "plugin/test/http")
	p, err := NewWithContext(Config{Tasks: owner, BatchMaxSize: 2}, deliver)
	if err != nil { t.Fatal(err) }
	p.Push(entry1); p.Push(entry2)
	stopRegistry(t, registry)
	assertDelivered(t, entry1, entry2)
	assertOwnersExited(t, registry)
	assertNoFailure(t, failures)
}
```

Add `TestProcessorBatchShutdownRunsCleanupExactlyOnceAfterLastWorker`, `TestProcessorTaskAdmissionRollbackOwnsNoObserverOrEntries`, and `TestProcessorDirectShutdownAndRegistryStopRace`. Retain the existing timeout accounting, retry cancellation, partial suffix accounting, timer generation, and deferred cleanup tests.

- [ ] **Step 2: Add the frozen deterministic residual-set RED**

Add `TestProcessorBlockingDeliveryResidualSetIsWorkerAndShutdown` in `pkg/plugin/logger_batch/processor_test.go`. Use one real registry and `TaskOwner` with this independent exact prefix:

```go
ownerPrefix := "plugin/error-log-logger/" + strings.Repeat("a", 64)
want := []runtime.TaskResidual{
	{Owner: ownerPrefix + "/batch-shutdown"},
	{Owner: ownerPrefix + "/batch-worker"},
}
```

Configure `MaxConcurrentDeliveries: 1`; the delivery callback closes `deliveryStarted`, ignores its callback context, and waits on `releaseDelivery`. Push exactly one entry and wait for `deliveryStarted`. Call `StopWithCleanup` with an atomic cleanup counter and require it to return while delivery remains blocked. Then call the real registry `Stop(shortCtx)` and require `errors.As(err, *runtime.TaskResidualError)`, `errors.Is(err, context.DeadlineExceeded)`, and exact equality between both returned/resident residual slices and `want`. Explicitly reject `ownerPrefix+"/batch-scheduler"` and `ownerPrefix+"/observer"`; suffix-only matching is forbidden. Close `releaseDelivery`, retry `registry.Stop(context.Background())`, and assert no residual and cleanup exactly once.

This is the deterministic seam: it injects the delivery callback and does not depend on network timeout. `StopWithCleanup` returning proves only that the scheduler has sealed admission and published the terminal drain; the blocked delivery intentionally keeps `/batch-worker` and the pre-admitted `/batch-shutdown` task active.

- [ ] **Step 3: Add error-log observer delegation RED**

Add `TestErrorLogObserverDelegatesBlockingBatchShutdownWithoutRemainingResidual` in `pkg/plugin/error_log_logger/plugin_test.go`. Build the plugin with the same real registry/owner prefix and Task-8-owned batch processor using the blocking delivery callback. Call the Contract C signature `StartObservingWithTasks(*runtime.TaskOwner)`, emit one entry, and wait for delivery entry. Run `p.Stop()` in a goroutine and require it to return while delivery remains blocked. Then stop the registry with a deadline and require exact equality with the same two-element `want` above.

The test must prove `/observer` is absent because `Plugin.Stop` has completed observer delegation, and `/batch-scheduler` is absent because `StopWithCleanup` waited for `schedulerDone`. Release delivery, retry registry Stop, and assert no residual and cleanup exactly once. Do not add an observer hook, exported task-state accessor, sleep, or an expected set learned from the observed residual.

- [ ] **Step 4: Add the real compiler-to-server batch chain RED**

Create `pkg/server/task_residual_chain_test.go` with `TestServerShutdownPreservesExactGenerationOwnersThroughRealChain`:

1. Construct a real `WorkerCompilerFactory`, real `GenerationEngine`, and `Server`. Prepare and activate a snapshot with one route-scoped `clickhouse-logger` binding. The Task 8 batch constructor must receive the compiler-derived owner. The test must not call `TaskRegistry.Go`, `PreparedGeneration.Close`, `factory.Close`, or a fake engine. The lower `error-log-logger` test from Step 3 separately proves that one supplied owner governs both observer delegation and batch tasks.
2. Use an inline blocking ClickHouse `httptest.Server`, batch size one, one worker, and a delivery timeout longer than the bounded shutdown attempt. Its handler closes `deliveryStarted` and waits on `releaseDelivery` even after request-context cancellation. Emit one application log and wait for handler entry before shutdown.
3. Independently construct `ownerPrefix` from the known activated `clickhouse-logger` `plugin.InstanceKey`, using Contract C's frozen canonical field order and SHA-256. Include the returned attempt, route scope/provenance, and canonical config digest; do not call the compiler's private owner helper and do not read the expected prefix from actual residuals. The exact sorted expectation is `/batch-shutdown` followed by `/batch-worker`; `/batch-scheduler` is forbidden. Step 3 remains the authority that `/observer` has exited before the error-log residual snapshot.
4. Call `server.Shutdown(shortCtx)`. Require `errors.As(err, *runtime.TaskResidualError)`, exact equality with the independently constructed two-element set, `errors.Is(err, context.DeadlineExceeded)`, and `errors.Is(err, compiler.ErrPreparedGenerationCleanupIncomplete)`. Assert the prepared/factory/engine owner and the later resolver, journal, and observability owners remain retained.
5. Close `releaseDelivery`, call `server.Shutdown(context.Background())`, and assert terminal completion, no residual, and exactly-once plugin/resource/processor cleanup.

The server test is the batch residual-propagation authority. The lower processor and error-log tests make the batch component set and observer delegation deterministic; the server test proves the exact batch set survives transfer into `PreparedGeneration`, factory close, engine close, and server phase retry without redaction, suffix inference, or an accidental scheduler residual.

The split is intentional. At the frozen implementation, `PlanHTTPPlugins` derives only `request-context` as a system binding, while the effective-binding materializer rejects secret-declaring factories without an authoritative ordinary source. Therefore no real production path can materialize `error-log-logger` as the Step 4 system binding. Repairing system/plugin-metadata materialization is a separate correctness task shared with `log-rotate`, not an implicit Task 8 expansion. This plan must not claim that one end-to-end test proves the compiler-derived owner reaches both the error-log observer and its batch constructor; the Step 3 plus Step 4 combination is the explicit evidence boundary.

- [ ] **Step 5: Capture both RED layers**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/logger_batch ./pkg/plugin/base -run "^(TestProcessorRegistersOwnedWorkersAndDrainsOnRegistryStop|TestProcessorBatchShutdownRunsCleanupExactlyOnceAfterLastWorker|TestProcessorTaskAdmissionRollbackOwnsNoObserverOrEntries|TestProcessorDirectShutdownAndRegistryStopRace)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/logger_batch ./pkg/plugin/error_log_logger ./pkg/server -run "^(TestProcessorBlockingDeliveryResidualSetIsWorkerAndShutdown|TestErrorLogObserverDelegatesBlockingBatchShutdownWithoutRemainingResidual|TestServerShutdownPreservesExactGenerationOwnersThroughRealChain)$" -count=1'
```

Expected: FAIL because the frozen implementation has raw workers/detached cleanup, cannot preserve the exact two-owner set, and cannot carry it through real server shutdown.

- [ ] **Step 6: Extend the exact constructor contract**

```go
type Config struct {
	// existing fields
	Tasks *runtime.TaskOwner
}

func NewWithContext(Config, ContextDeliveryFunc) (*Processor, error)
```

Production constructors require the owner field. `base.NewBatchProcessor` also returns `(*logger_batch.Processor, error)` and receives `tasks *runtime.TaskOwner`; every plugin PostInit passes `p.TaskOwner()` and propagates the error before publishing the processor. Direct unit tests create a local registry and `TaskPlugin` owner.

- [ ] **Step 7: Implement the private scheduler and shutdown barriers**

Replace the timer callback and raw workers with pre-admitted `Tasks.Go("batch-scheduler", ...)`, repeated `Tasks.Go("batch-worker", ...)`, and one `Tasks.Go("batch-shutdown", ...)`. Add private `schedulerDone`, `workersDone`, and terminal shutdown state; expose none of them.

The scheduler owns timer reset/coalescing. On owner cancellation or explicit stop it atomically seals `Push`, flushes the final buffered batch, queues the terminal drain, wakes workers, closes `schedulerDone`, and returns. Only after `schedulerDone` closes may `/batch-scheduler` disappear from a residual snapshot. Workers ignore cancellation only to finish already admitted batches with their existing bounded delivery contexts; the final worker closes `workersDone` but does not directly free sink resources.

The pre-admitted `/batch-shutdown` callback waits for `schedulerDone`, then `workersDone`, runs registered final cleanup exactly once, closes terminal shutdown state, and returns. It is the only task that waits for workers and invokes final cleanup. This ordering keeps `/batch-shutdown` active beside `/batch-worker` for a blocked delivery and removes the detached cleanup waiter.

`StopWithCleanup(cleanup)` must register cleanup before initiating shutdown, synchronously initiate scheduler sealing, wait for `schedulerDone`, and return without waiting for `workersDone`. This is required so `error_log_logger.Plugin.Stop` can finish `/observer` before generation registry Stop snapshots residuals. `Shutdown(ctx)` initiates the same state machine and waits for terminal shutdown with the caller context; a deadline returns without releasing sink resources or hiding the still-owned worker/shutdown tasks. Concurrent/repeated Stop, Shutdown, registry cancellation, and final-worker exit must share the same exact-once state.

Admission is atomic with publication: if scheduler, any worker, or shutdown task admission fails, seal the un-published processor, wake every admitted callback, wait for its private barriers, close the observer, and return the admission error. No constructor failure may publish a processor, retain entries, or leave an owner active.

The real compiler chain also needs an opt-in pre-quiesce seam. The effective-binding creator registers a `cleanupQuiesce` step after `generation-tasks`; reverse cleanup order invokes a structural `QuiesceGenerationTasks()` hook before `TaskRegistry.Stop`. Only the creator instance may receive the hook: follower/shared leases must not borrow or retire it, and rollback must not quiesce an earlier shared owner. Every Task 8 batch logger implements the hook by calling its concrete idempotent `Stop`, so scheduler admission is sealed while delivery-owned sink cleanup remains deferred to `/batch-shutdown`. Generic plugin `Stop`, lease release, and all non-opt-in plugins keep the existing `cleanupRelease` ordering.

When engine shutdown joins an active background retirement, its internal cancellation is transient evidence rather than the final residual snapshot. `closeRetirementOwners` must synchronously retry every retained owner with the public caller context in the same attempt. If that retry reaches the caller deadline, return the latest exact `TaskResidualError`, `context.DeadlineExceeded`, and the joined internal cancellation marker without retaining either transient in terminal replay. If the retry succeeds, discard the old cancellation. Do not return the old residual before the caller-bounded retry, and do not manufacture a deadline marker without performing that retry.

- [ ] **Step 8: Run exact compatibility GREEN and race gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/logger_batch ./pkg/plugin/error_log_logger ./pkg/server -run "^(TestProcessorBlockingDeliveryResidualSetIsWorkerAndShutdown|TestErrorLogObserverDelegatesBlockingBatchShutdownWithoutRemainingResidual|TestServerShutdownPreservesExactGenerationOwnersThroughRealChain)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/logger_batch ./pkg/plugin/error_log_logger ./pkg/server -run "^(TestProcessorBlockingDeliveryResidualSetIsWorkerAndShutdown|TestErrorLogObserverDelegatesBlockingBatchShutdownWithoutRemainingResidual|TestServerShutdownPreservesExactGenerationOwnersThroughRealChain)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^(TestEffectiveBindingCreatorQuiescesGenerationTasksBeforeRegistryRelease|TestEffectiveBindingGenerationTaskQuiescerIsCreatorOnlyAndRollbackIsolated)$" -count=10'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler -run "^(TestEffectiveBindingCreatorQuiescesGenerationTasksBeforeRegistryRelease|TestEffectiveBindingGenerationTaskQuiescerIsCreatorOnlyAndRollbackIsolated)$" -count=3'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^TestGenerationEngineCloseCancelsActiveRetirementAttemptBeforeRetry$" -count=10'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^TestGenerationEngineCloseCancelsActiveRetirementAttemptBeforeRetry$" -count=3'
```

Expected: PASS. Both lower-level tests and the real `clickhouse-logger` compiler -> `PreparedGeneration` -> factory -> engine -> server chain return exactly the independently derived sorted worker/shutdown set; the retry releases each owner and cleanup once. The lower error-log test separately proves observer delegation has exited before its batch residual snapshot.

- [ ] **Step 9: Run primitive, constructor-caller, lint, and build gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/logger_batch ./pkg/plugin/base -run "^(TestProcessor|TestBaseLogger|TestNewBatchProcessor)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/logger_batch ./pkg/plugin/base -run "^(TestProcessor|TestBaseLogger|TestNewBatchProcessor)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/clickhouse_logger ./pkg/plugin/datadog ./pkg/plugin/elasticsearch_logger ./pkg/plugin/error_log_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/http_logger ./pkg/plugin/kafka_logger ./pkg/plugin/lago ./pkg/plugin/loggly ./pkg/plugin/loki_logger ./pkg/plugin/rocketmq_logger ./pkg/plugin/skywalking_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/syslog ./pkg/plugin/tcp_logger ./pkg/plugin/tencent_cloud_cls ./pkg/plugin/udp_logger ./pkg/plugin/zipkin -run "^(TestPostInit|TestStop|TestBatch|TestProcessor|TestMaterialize)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/logger_batch/... ./pkg/plugin/base/... ./pkg/plugin/clickhouse_logger/... ./pkg/plugin/datadog/... ./pkg/plugin/elasticsearch_logger/... ./pkg/plugin/error_log_logger/... ./pkg/plugin/google_cloud_logging/... ./pkg/plugin/http_logger/... ./pkg/plugin/kafka_logger/... ./pkg/plugin/lago/... ./pkg/plugin/loggly/... ./pkg/plugin/loki_logger/... ./pkg/plugin/rocketmq_logger/... ./pkg/plugin/skywalking_logger/... ./pkg/plugin/sls_logger/... ./pkg/plugin/splunk_hec_logging/... ./pkg/plugin/syslog/... ./pkg/plugin/tcp_logger/... ./pkg/plugin/tencent_cloud_cls/... ./pkg/plugin/udp_logger/... ./pkg/plugin/zipkin/... && make build'
git diff --check
```

- [ ] **Step 10: Review and commit**

Review `error_log_logger.StartObservingWithTasks`, `Plugin.Stop`, `logger_batch.Processor.StopWithCleanup`, scheduler/worker/shutdown callbacks, and the server shutdown phase together. Confirm the lower error-log test proves observer and scheduler termination precede its residual snapshot; confirm the real `clickhouse-logger` server chain leaves worker and shutdown for the same blocked delivery under the exact compiler prefix; confirm no cleanup runs before worker exit and Task11-0 retains the incomplete generation/server phase. If the reviewed state machine legitimately changes the complete component set, update Task11-0's frozen mapping and all three exact-equality tests in the same reviewed correction; never weaken to suffix matching or copy observed residuals into `want`.

```bash
git add pkg/plugin/logger_batch/processor.go pkg/plugin/logger_batch/processor_test.go \
  pkg/compiler/effective_binding_materializer.go pkg/compiler/effective_binding_materializer_test.go \
  pkg/plugin/base/types.go pkg/plugin/base/types_test.go \
  pkg/plugin/clickhouse_logger pkg/plugin/datadog pkg/plugin/elasticsearch_logger \
  pkg/plugin/error_log_logger pkg/plugin/google_cloud_logging pkg/plugin/http_logger \
  pkg/plugin/kafka_logger pkg/plugin/lago pkg/plugin/loggly pkg/plugin/loki_logger \
  pkg/plugin/rocketmq_logger pkg/plugin/skywalking_logger pkg/plugin/sls_logger \
  pkg/plugin/splunk_hec_logging pkg/plugin/syslog pkg/plugin/tcp_logger \
  pkg/plugin/tencent_cloud_cls pkg/plugin/udp_logger pkg/plugin/zipkin \
  pkg/server/generation_engine.go pkg/server/generation_engine_test.go \
  pkg/server/task_residual_chain_test.go
git commit -m "refactor(logger): own batch delivery tasks"
```

Before committing, rerun the production constructor inventory and inspect `git diff --cached --name-only`. The staged set must contain every and only Task 8 constructor caller, the explicitly named processor/base/compiler/engine files, `error_log_logger/plugin_test.go`, and `pkg/server/task_residual_chain_test.go`; do not stage unrelated plugin files.

---

### Task 9: Own File Logger Processor and Shared Reopen Watcher

**Files:** `pkg/plugin/file_logger/processor.go`, `processor_test.go`, `writer_registry.go`, `plugin.go`, `plugin_test.go`

**Consumes:** plugin owner for processor; registry-local owner for process-global writers.

**Produces:** `<instance-owner>/file-log-writer` under the injected `TaskPlugin` owner and `core/file-writer-registry/signal-watch` under a registry-local `TaskCore` owner.

- [ ] Add `TestFileLoggerProcessorUsesPluginTaskOwner`, `TestFileLoggerBlockingWriteIsNamedResidualAndDefersLeaseRelease`, `TestSharedWriterWatcherSurvivesOldGenerationRelease`, `TestSharedWriterWatcherStopsAndRestartsByRegistryEpoch`, and subprocess `TestFileWriterSignalTaskPanicUsesCoreFatalPolicy`. The overlap test acquires the same canonical path from old/new plugin instances, releases old, sends SIGUSR1 through the injected channel seam, and proves the new lease reopens. The subprocess injects a watcher callback that panics with a stable marker, asserts non-zero exit plus that marker, and asserts no post-panic continuation marker; it must not expect a `TaskFailure` report because `signal-watch` is `TaskCore`.

```go
func TestSharedWriterWatcherSurvivesOldGenerationRelease(t *testing.T) {
	registry := newFileWriterRegistryForTest(t)
	old := acquireWriterLeaseForTest(t, registry, testLogPath(t))
	next := acquireWriterLeaseForTest(t, registry, old.path)
	old.release()
	registry.signalForTest(syscall.SIGUSR1)
	awaitReopen(t, next.writer)
	if !slices.Contains(registry.activeTaskOwnersForTest(), "core/file-writer-registry/signal-watch") {
		t.Fatal("shared watcher stopped while a lease remained")
	}
	next.release()
}
```

Use the same subprocess structure for the core watcher; the injected signal source and reopen callback are existing Task 9 test seams, not production exports:

```go
func TestFileWriterSignalTaskPanicUsesCoreFatalPolicy(t *testing.T) {
	if os.Getenv("APISIX_GO_TEST_FILE_WRITER_CORE_PANIC") == "1" {
		runFileWriterSignalCorePanicFixture(t, "file-writer-core-fatal")
		fmt.Fprintln(os.Stderr, "file-writer-returned-after-core-panic")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFileWriterSignalTaskPanicUsesCoreFatalPolicy$")
	cmd.Env = append(os.Environ(), "APISIX_GO_TEST_FILE_WRITER_CORE_PANIC=1")
	output, err := cmd.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("file-writer-core-fatal")) ||
		bytes.Contains(output, []byte("file-writer-returned-after-core-panic")) {
		t.Fatalf("core panic subprocess = %v, output = %s", err, output)
	}
}
```
- [ ] RED:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/file_logger -run "^(TestFileLoggerProcessorUsesPluginTaskOwner|TestFileLoggerBlockingWriteIsNamedResidualAndDefersLeaseRelease|TestSharedWriterWatcherSurvivesOldGenerationRelease|TestSharedWriterWatcherStopsAndRestartsByRegistryEpoch|TestFileWriterSignalTaskPanicUsesCoreFatalPolicy)$" -count=1'
```

- [ ] Make `newFileLoggerProcessor(owner *runtime.TaskOwner, sink fileLoggerSink) (*fileLoggerProcessor, error)` and call `owner.Go("file-log-writer", processor.run)` before publishing. Registry cancellation seals admission and drains records/barriers; last completion closes observer and releases the writer lease. Remove the late-cleanup goroutine from `stopWithCleanup`.
- [ ] `fileWriterRegistry.startSignalWatcherLocked` creates its own epoch registry and `runtime.NewTaskOwner(registry, "core/file-writer-registry", runtime.TaskCore)`, then calls `owner.Go("signal-watch", watcher)`. The last registry lease stops that epoch and joins it before stopping the buffered writer. It does not receive a generation owner. Preserve canonical-path deduplication, exact last-lease writer Stop, buffered flush, missing-path reopen, and the `sync.Once` lease release.
- [ ] GREEN/race/gates:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/file_logger -run "^(TestFileLogger|TestFlushAndReopen|TestHandlerWritesToCurrentPath|TestHandlerKeepsUnlinked|TestFileWriterSignalTaskPanicUsesCoreFatalPolicy)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/file_logger -run "^(TestFileLogger|TestFlushAndReopen|TestHandlerWritesToCurrentPath|TestHandlerKeepsUnlinked|TestFileWriterSignalTaskPanicUsesCoreFatalPolicy)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/file_logger/... && make build'
git diff --check
```

- [ ] Commit: `git commit -m "refactor(file-logger): own processor and reopen watcher"`.

---

### Task 10: Remove Logger Cancellation and RocketMQ Shutdown Detachment

**Files:**

- Modify: `pkg/plugin/error_log_logger/plugin.go`, tests
- Modify: `pkg/plugin/tcp_logger/plugin.go`, tests
- Modify: `pkg/plugin/syslog/transport.go`, tests
- Modify: `pkg/plugin/udp_logger/plugin.go`, tests
- Modify: `pkg/plugin/datadog/plugin.go`, tests
- Modify: `pkg/plugin/sls_logger/plugin.go`, tests
- Modify: `pkg/plugin/loggly/plugin.go`, tests
- Modify: `pkg/plugin/rocketmq_logger/plugin.go`, `plugin_test.go`

**Consumes:** logger batch owned workers and bounded delivery contexts.

**Produces:** joined context cancellation callbacks and `<instance-owner>/rocketmq-sender-shutdown` ownership; no helper goroutine hides a blocking Close/Shutdown.

- [ ] For each connection helper add/retain a table-driven test proving: context cancellation closes the connection; normal completion prevents a later cancellation from closing a reused connection; cleanup waits if the cancellation callback is already inside Close. Replace `WaitGroup.Go` with this exact joined context-owned callback shape:

```go
func watchConnectionCancellation(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); _ = conn.Close() })
	return func() {
		if stop() { close(done) }
		<-done
	}
}
```

Keep the existing nil-context no-op. This has one owner (the delivery context), and cleanup joins a callback that already started.

- [ ] Add RocketMQ RED tests `TestRocketMQSenderShutdownIsOwnedAndResidualVisible` and `TestRocketMQGenerationCancellationFlushesBeforeSenderShutdown`. Use a sender whose `Shutdown` blocks. Assert pending delivery completes before Shutdown starts, short generation stop reports `<owner>/rocketmq-sender-shutdown`, release lets stop complete, and Shutdown is called once.
- [ ] RED:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/error_log_logger ./pkg/plugin/tcp_logger ./pkg/plugin/syslog ./pkg/plugin/udp_logger ./pkg/plugin/datadog ./pkg/plugin/sls_logger ./pkg/plugin/loggly ./pkg/plugin/rocketmq_logger -run "^(TestWatchConnectionCancellation|TestRocketMQSenderShutdownIsOwnedAndResidualVisible|TestRocketMQGenerationCancellationFlushesBeforeSenderShutdown)$" -count=1'
```

- [ ] Delete `shutdownRocketMQSender`'s raw goroutine and timeout. After sender construction but before publication, call `p.TaskOwner().Go("rocketmq-sender-shutdown", shutdownRun)`. The pre-admitted task waits for its callback cancellation, seals/drains the batch processor, waits for delivery workers, then calls `sender.Shutdown()` synchronously. Plugin `Stop` only clears already-quiesced non-task state; it never signals, waits again, or calls `TaskOwner.Go` after registry stop begins. If admission fails, synchronously destroy the unpublished sender and return the admission error; never publish a sender without a shutdown owner.
- [ ] GREEN/race/gates:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/error_log_logger ./pkg/plugin/tcp_logger ./pkg/plugin/syslog ./pkg/plugin/udp_logger ./pkg/plugin/datadog ./pkg/plugin/sls_logger ./pkg/plugin/loggly ./pkg/plugin/rocketmq_logger -run "^(Test.*Cancel|Test.*Stop|Test.*Shutdown|Test.*Batch|TestRocketMQ)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/error_log_logger ./pkg/plugin/tcp_logger ./pkg/plugin/syslog ./pkg/plugin/udp_logger ./pkg/plugin/datadog ./pkg/plugin/sls_logger ./pkg/plugin/loggly ./pkg/plugin/rocketmq_logger -run "^(Test.*Cancel|Test.*Stop|Test.*Shutdown|Test.*Batch|TestRocketMQ)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/error_log_logger/... ./pkg/plugin/tcp_logger/... ./pkg/plugin/syslog/... ./pkg/plugin/udp_logger/... ./pkg/plugin/datadog/... ./pkg/plugin/sls_logger/... ./pkg/plugin/loggly/... ./pkg/plugin/rocketmq_logger/... && make build'
git diff --check
```

- [ ] Commit: `git commit -m "refactor(logger): join cancellation and sender shutdown"`.

---

### Task 11: Own the Persistent Stream Runtime Across Router Generations

**Files:**

- Modify: `pkg/stream/runtime.go`
- Modify: `pkg/stream/runtime_test.go`

**Consumes:** Contract C `TaskOwner`, existing `RouterSource` leases, listener and connection close semantics.

**Produces:** one runtime-local `TaskRegistry`, a `TaskCore` owner with prefix `core/stream-runtime`, and deduplicated components `listener` and `connection`. RouterSource changes never replace this owner.

- [ ] Add focused RED tests:

```go
func TestRuntimeCoreOwnerSurvivesRouterSourceGenerationChange(t *testing.T) {
	source, publish := newSwitchableRouterSource(t)
	runtime := newRuntimeWithInjectedListeners(t, source)
	first := publish(routerLeaseFixture(t, "generation-1"))
	serveOneConnection(t, runtime)
	second := publish(routerLeaseFixture(t, "generation-2"))
	serveOneConnection(t, runtime)
	if got := runtime.tasks.Active(); !slices.Contains(got, "core/stream-runtime/listener") {
		t.Fatalf("active owners = %v", got)
	}
	if first.Owner == second.Owner { t.Fatal("router fixture did not change generation") }
	closeRuntime(t, runtime)
}

func TestRuntimeCloseReportsBlockingConnectionResidualAndLaterJoins(t *testing.T) {
	runtime, release, leaseReleased := newBlockingConnectionRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := runtime.Close(ctx)
	var closeErr *runtimeCloseError
	if !errors.As(err, &closeErr) || !errors.Is(err, context.DeadlineExceeded) { t.Fatalf("Close = %v", err) }
	if got := closeErr.residuals; !reflect.DeepEqual(got,
		[]runtime.TaskResidual{{Owner: "core/stream-runtime/connection"}}) { t.Fatalf("residuals = %v", got) }
	close(release)
	if err := runtime.Close(context.Background()); err != nil { t.Fatal(err) }
	awaitClosed(t, leaseReleased)
}
```

Add a subprocess `TestRuntimeConnectionTaskPanicUsesCoreFatalPolicy` whose router panics with a unique marker. Assert non-zero exit, marker in output, and absence of a post-panic continuation marker. This proves the local owner is `TaskCore`, not a recoverable plugin owner; do not expect a registry failure report or continuation.

```go
func TestRuntimeConnectionTaskPanicUsesCoreFatalPolicy(t *testing.T) {
	if os.Getenv("APISIX_GO_TEST_STREAM_CORE_PANIC") == "1" {
		runStreamConnectionCorePanicFixture(t, "stream-core-fatal")
		fmt.Fprintln(os.Stderr, "stream-returned-after-core-panic")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeConnectionTaskPanicUsesCoreFatalPolicy$")
	cmd.Env = append(os.Environ(), "APISIX_GO_TEST_STREAM_CORE_PANIC=1")
	output, err := cmd.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("stream-core-fatal")) ||
		bytes.Contains(output, []byte("stream-returned-after-core-panic")) {
		t.Fatalf("core panic subprocess = %v, output = %s", err, output)
	}
}
```

The three helpers intentionally do not install recovery. Their only bounded synchronization is enough to keep the subprocess alive until the owned callback reaches the injected panic; timeout belongs to the parent command/test harness, not to a recovery path.

- [ ] Capture RED:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/stream -run "^(TestRuntimeCoreOwnerSurvivesRouterSourceGenerationChange|TestRuntimeCloseReportsBlockingConnectionResidualAndLaterJoins|TestRuntimeConnectionTaskPanicUsesCoreFatalPolicy)$" -count=1'
```

Expected: FAIL because `Runtime` uses raw listener goroutines, `WaitGroup.Go` connections, and a detached close waiter with no named owner/residual.

- [ ] In `newRuntime`, create `runtimeCtx`, one local `TaskRegistry`, then `runtime.NewTaskOwner(tasks, "core/stream-runtime", runtime.TaskCore)`. Admit every listener with `owner.Go("listener", ...)`; after a lease is acquired and the connection is tracked, admit it with `owner.Go("connection", ...)`. Admission failure must release the router lease, untrack/close the connection, and initiate runtime close.
- [ ] Remove `wg`, `closeDone`, and the raw close waiter. `initiateClose` only cancels runtime context and synchronously closes listeners/current connections once. Every `Close(ctx)` then calls the local registry's `Stop(ctx)`. Add package-private `runtimeCloseError{residuals []runtime.TaskResidual, err error}` with a stable constant `Error()` string and `Unwrap() error`; use it only when Stop returns an error or residual, so the bounded owner evidence is retained without changing the public method signature. A later Close can finish. A terminal Accept error initiates close and returns from its own listener task; it must not call `Stop` from inside that task. Preserve listener retry backoff, lease pinning, rollback visibility for new connections only, and exactly-once lease release.
- [ ] GREEN/race/gates:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/stream -run "^(TestRuntime|TestNewRuntime|TestServeListener|TestNewGenerationRuntime)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/stream -run "^(TestRuntime|TestNewRuntime|TestServeListener|TestNewGenerationRuntime)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/stream/... && make build'
git diff --check
```

- [ ] Commit: `git commit -m "refactor(stream): own runtime listener tasks"`.

---

### Task 12: Reconcile the Global Inventory and Run Completion Gates

**Files:** Verify all files above. Contract C Task 5 exclusively owns creation and edits of `pkg/runtime/goroutine_contract_test.go`; this task must not modify it.

**Consumes:** Contract C Task 2, Task11-0 retryable teardown, this plan, the request Task 11 plan, and Contract C Task 5's AST gate.

**Produces:** zero unowned production `go`/`WaitGroup.Go` in the four AST scan roots, owner/residual evidence, and an impact-scoped buildable tree.

- [ ] Run the raw inventory and compare every result to the routing table:

```bash
rg -n '\bgo[[:space:]]+|\.Go\(' pkg/plugin pkg/proxy pkg/route pkg/stream --glob '*.go' --glob '!*_test.go'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime -run "^TestProductionGoroutinesUseOwnedRuntime$" -count=1'
```

Expected after this plan plus the request plan and Contract C Task 5: PASS; the four scanned roots contain no production raw `go` or `sync.WaitGroup.Go`. Canonical runtime primitives are outside those roots.

- [ ] Run generation-background focused normal and race gates:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime ./pkg/compiler ./pkg/proxy ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache ./pkg/plugin/limit_count ./pkg/plugin/ai_proxy_multi ./pkg/plugin/oas_validator ./pkg/plugin/log_rotate ./pkg/plugin/logger_batch ./pkg/plugin/file_logger ./pkg/plugin/rocketmq_logger -run "(Task|Owner|Residual|Stop|Shutdown|Cleanup|Health|Refresh|Rotation|Processor|DelayedSync|Cluster)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/runtime ./pkg/compiler ./pkg/proxy ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache ./pkg/plugin/limit_count ./pkg/plugin/ai_proxy_multi ./pkg/plugin/oas_validator ./pkg/plugin/log_rotate ./pkg/plugin/logger_batch ./pkg/plugin/file_logger ./pkg/plugin/rocketmq_logger -run "(Task|Owner|Residual|Stop|Shutdown|Cleanup|Health|Refresh|Rotation|Processor|DelayedSync|Cluster)" -count=1'
```

- [ ] Re-run Task 8's frozen compatibility checkpoint as an exact normal/race gate:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/logger_batch ./pkg/plugin/error_log_logger ./pkg/server -run "^(TestProcessorBlockingDeliveryResidualSetIsWorkerAndShutdown|TestErrorLogObserverDelegatesBlockingBatchShutdownWithoutRemainingResidual|TestServerShutdownPreservesExactGenerationOwnersThroughRealChain)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/logger_batch ./pkg/plugin/error_log_logger ./pkg/server -run "^(TestProcessorBlockingDeliveryResidualSetIsWorkerAndShutdown|TestErrorLogObserverDelegatesBlockingBatchShutdownWithoutRemainingResidual|TestServerShutdownPreservesExactGenerationOwnersThroughRealChain)$" -count=1'
```

Expected: every package selects a test; both commands return the independently constructed sorted `/batch-shutdown`, `/batch-worker` set with no observer/scheduler residual, then complete a clean retry.

- [ ] Run scoped logger cancellation packages normal/race, then lint/build:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/error_log_logger ./pkg/plugin/tcp_logger ./pkg/plugin/syslog ./pkg/plugin/udp_logger ./pkg/plugin/datadog ./pkg/plugin/sls_logger ./pkg/plugin/loggly -run "(Cancel|Stop|Shutdown|Delivery)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/runtime/... ./pkg/compiler/... ./pkg/proxy/... ./pkg/server/... ./pkg/plugin/proxy_cache/... ./pkg/plugin/graphql_proxy_cache/... ./pkg/plugin/limit_count/... ./pkg/plugin/ai_proxy_multi/... ./pkg/plugin/oas_validator/... ./pkg/plugin/log_rotate/... ./pkg/plugin/logger_batch/... ./pkg/plugin/file_logger/... ./pkg/plugin/rocketmq_logger/... ./pkg/plugin/error_log_logger/... ./pkg/plugin/tcp_logger/... ./pkg/plugin/syslog/... ./pkg/plugin/udp_logger/... ./pkg/plugin/datadog/... ./pkg/plugin/sls_logger/... ./pkg/plugin/loggly/...'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
git diff --check
```

- [ ] Run moved/deleted helper scans:

```bash
rg -n 'cleanupStop|cleanupDone|healthCancel|healthDone|refreshCancel|refreshDone|shutdownRocketMQSender|watchConnectionCancellation|startSignalWatcherLocked|newDelayedSyncer|newFileLoggerProcessor|NewBatchProcessor' pkg --glob '*.go'
rg -n 'context\.Background\(\)' pkg/plugin/logger_batch pkg/plugin/file_logger pkg/plugin/proxy_cache pkg/plugin/graphql_proxy_cache pkg/plugin/limit_count pkg/plugin/ai_proxy_multi pkg/plugin/oas_validator pkg/plugin/log_rotate pkg/plugin/rocketmq_logger pkg/proxy/active_health.go --glob '*.go' --glob '!*_test.go'
```

Expected: each remaining symbol is a documented compatibility/lifecycle boundary with a production caller; no proxy-only helper or detached background root remains.

- [ ] Do not create an integration commit unless this plan's owned files required a real reviewed correction. Never stage or edit Contract C's AST test from this task.

## Review Checkpoints

After each task, the reviewer must inspect:

1. Owner stability: exact InstanceKey/digest suffix, no unbounded or sensitive value.
2. Lifetime: task registry outlives every state/resource the task touches.
3. Admission: task is registered before its plugin/resource is published; failure rolls back all acquired state.
4. Stop order: reject new work, cancel/wake, drain/join, then close transports/secrets/writers and remove publications.
5. Residual visibility: an injected cancellation-ignoring callback appears under the expected owner.
6. Plugin panic policy: a `TaskPlugin` panic is recovered by the registry, reports the exact full owner and raw identity to the configured failure sink, fails only that owner, and leaves unrelated owners admissible. Review the focused plugin panic test in the task's normal and race gates.
7. Core panic policy: active-health, file-writer `signal-watch`, and persistent stream `TaskCore` panics are never recovered or reported as isolated plugin failures. Their subprocess tests prove non-zero worker termination with the stable marker and no post-panic continuation; no in-process test may deliberately trigger them.
8. Concurrency: repeated/concurrent Stop, generation overlap, task admission versus rollback, and last-lease release are race-tested.
9. Scope: request work has not been moved into a generation registry, and the persistent stream runtime keeps its own runtime-local core registry.
10. Retryable teardown: generation tasks quiesce before `cleanupResourceFinalize`; a failed cluster final close retains the Prepared/factory/engine record, closing entry, and every subordinate resource. Only a terminal retry permits one-shot release, removes the closing entry, and makes the same key acquirable again, each release exactly once.
11. Logger residual compatibility: `StopWithCleanup` returns only after private `schedulerDone`; blocked delivery leaves the exact independently derived sorted `/batch-shutdown`, `/batch-worker` set. `/observer` and `/batch-scheduler` are absent in processor, error-log delegation, and real server-chain tests; retry performs cleanup exactly once without advancing Task11-0 server phases early.

## Plan Self-Review

- **Coverage:** Every original generation/background file is assigned. `rocketmq_logger/plugin.go:242` and all seven `WaitGroup.Go` cancellation helpers are explicit. Current extra raw-go sites are classified into the request plan rather than silently ignored. AI probe panic completion, retryable cluster final close, and all three resource/runtime `TaskCore` fatal paths have dedicated regressions.
- **Dependency consistency:** Every implementation task starts from Contract C Task 2 plus `Task11-0 / retryable teardown and residual propagation`, consumes the exact compiler-injected `*runtime.TaskOwner`, and does not reference nonexistent `materialize.go`. Shared cluster and writer resources construct lifecycle-local `TaskCore` owners rather than borrowing a generation owner. HTTP leases use Task11-0's exact `cleanupResourceFinalize` phase between generation quiescence and one-shot release. Task11-0's deferred logger checkpoint is consumed inside Generation Task 8 rather than made its own prerequisite, so the hand-off is complete without a dependency cycle.
- **Testability:** Every behavior task starts with a focused RED test, names the expected failure cause, and has normal/race/lint/build commands. Cross-generation cluster and writer reuse receive explicit executable tests. The cluster-specific engine discard test crosses real engine, factory, Prepared, final-lease, and resource-registry boundaries; the three core-panic tests are subprocess-only. Task 8 freezes the complete residual set at processor, error-log delegation, and real server-chain layers and re-runs all three in the final normal/race gate.
- **No placeholders:** Owner strings, criticality, constructor changes, stop order, focused commands, and commit subjects are specified. Test helpers shown in skeletons are test-local helpers to implement in the named test file, not production APIs.
- **File fence:** Generation Task 5 integrates `ai_proxy_multi/plugin.go` and `plugin_test.go` first; request AI flush Task 7 starts from that reviewed head and is never dispatched in parallel.
- **Known risk:** Logger shutdown is the highest-risk sequence because the generation registry currently stops before plugin `Stop`. Task 8 therefore makes owner cancellation itself seal/drain the processor, and Task 10 runs RocketMQ shutdown inside a pre-admitted owned task. Do not retain the old cleanup goroutine as a fallback.
