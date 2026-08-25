# Immutable Task 11 Total Execution Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` for the assigned child task, `superpowers:test-driven-development` for each behavior change, `superpowers:requesting-code-review` before integration, and `superpowers:verification-before-completion` before every commit or merge-ready claim.

**Goal:** Remove every unowned production goroutine under `pkg/plugin`, `pkg/proxy`, `pkg/route`, and `pkg/stream`; bind each task to the exact generation, request/connection, or shared-resource lifetime; preserve Task 10 panic identity and cleanup semantics; and make cancellation-ignoring work visible as a bounded residual.

**Architecture:** Contract C first freezes the only two task APIs: generation/shared work enters through an immutable `runtime.TaskOwner`, and request/connection work enters through the existing `runtime.RequestTaskGroup`. The compiler derives plugin owners from `selected.instance`. Generation work, request work, and shared-resource work then migrate in reviewed frozen-base waves with exclusive file ownership. Contract C finally adds one repository AST gate and runs the integrated race/lint/build review.

**Tech Stack:** Go 1.26, `runtime.TaskRegistry`, `runtime.TaskOwner`, `runtime.RequestTaskGroup`, compiler `PreparedGeneration`, plugin `base.Dependencies`, `runtime.ResourceRegistry`, Go AST, focused tests, race detector, golangci-lint, and worktree-based local integration.

**Frozen base:** `b0220dcebd64a1d2d687be84d1f14ab501dfffd0`

## Child Plans

1. [Contract C: runtime task contract and final integration](2026-08-26-immutable-task11-runtime-task-contract-integration.md)
2. [Generation, shared-resource, logger, and stream-runtime ownership](2026-08-26-immutable-task11-generation-background-ownership.md)
3. [Request and connection concurrency ownership](2026-08-26-immutable-task11-request-concurrency-ownership.md)

The child plans contain the exact RED tests, production edits, commands, commit subjects, and review questions. This document owns only sequencing, worktree boundaries, integration dependencies, and total completion criteria. When a child-plan detail conflicts with this document, stop and repair the plans before implementation; do not average the contracts.

## Frozen Contracts

### Generation and shared-resource work

```go
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

- Compiler-owned plugin prefix: `plugin/<selected.instance.String()>`, `TaskPlugin`.
- Plugins provide only a fixed component; they do not construct `TaskSpec`, prefixes, or criticality.
- Shared clusters use `core/proxy-cluster/<digest>/active-health`, `TaskCore`, and a resource-local registry.
- The process-global file-writer epoch uses `core/file-writer-registry/signal-watch`, `TaskCore`, and an epoch-local registry.
- Persistent `stream.Runtime` uses `core/stream-runtime/{listener|connection}`, `TaskCore`, and a runtime-local registry.

### Request and connection work

The exported method set remains exactly:

```go
func NewRequestTaskGroup(parent context.Context, owner string) *RequestTaskGroup
func (g *RequestTaskGroup) Go(run func(context.Context) error) error
func (g *RequestTaskGroup) Wait() error
```

`Wait` joins every accepted child before re-panicking the first cached raw value by identity. It adds no cancellation, timeout, residual, panic-channel, or status API. Ordinary errors retain the existing `errors.Join` behavior.

## Current Inventory and Coverage

The frozen base contains:

- 32 raw production `go` statements in 20 files under the four AST roots;
- nine production `sync.WaitGroup.Go` calls: `stream/runtime.go`, `mqtt_proxy/stream.go`, and seven logger cancellation helpers;
- one additional raw file, `pkg/plugin/rocketmq_logger/plugin.go`, omitted by the stale parent Task 11 file list;
- no `pkg/compiler/materialize.go`; the real compiler injection point is `pkg/compiler/effective_binding_materializer.go`.

Coverage is exclusive:

| Owner plan | Scope |
| --- | --- |
| Contract C | `runtime.TaskOwner`, `RequestTaskGroup` join-before-repanic, compiler/base injection, error-log observer seam, final AST gate, total integration |
| Generation plan | active health, cache cleanup, delayed sync, AI health, OAS refresh, log rotation, logger batch/file/RocketMQ shutdown, seven logger cancellation helpers, persistent stream runtime |
| Request plan | stream bridge, batch requests, Kafka, proxy mirror, MQTT, MCP, and request-scoped AI periodic flush |

No bootstrap/server sibling plan exists or is required for this Task 11 gate. `pkg/runtime` contains the two canonical spawn primitives and is outside the four AST scan roots.

## Dependency Graph

```text
Contract C Task 1: TaskOwner + RequestTaskGroup semantics
├── Contract C Task 2: compiler/base/error-log owner injection
│   ├── Generation tasks 1-7 and 11 (independent wave)
│   ├── Generation tasks 8 -> 9 -> 10 (logger sequence)
│   └── Request AI flush after generation AI-health call-site edit
└── Request tasks 1,2,3,4,6 (independent wave)
    ├── Request task 5 MQTT after request task 1 bridge
    └── Request AI flush task, serialized with generation AI-health

All generation + request outputs
└── Contract C Task 5 AST gate
    └── Contract C Task 6 integrated verification and review
```

Contract C Task 2 must precede all generation tasks because they consume `BasePlugin.TaskOwner()`. Request tasks need only Contract C Task 1, but use the reviewed Task 2 head as their common frozen base to simplify deterministic integration. The AI periodic-flush task and generation AI-health task both touch `pkg/plugin/ai_proxy_multi/plugin.go`; they must not run from the same base in parallel. Integrate AI-health first, then start or replay AI flush from that reviewed head.

## Worktree and Integration Protocol

For every implementation unit:

1. Refresh local `master` identity and require the expected reviewed dependency SHA.
2. Create one `codex/immutable-task11-<unit>` branch and linked worktree from that frozen SHA.
3. Assign exclusive production/test files from the child-plan responsibility table.
4. Write the named RED oracle and capture the expected failure before product edits.
5. Implement only the bounded unit; run focused normal/race tests, scoped lint, build, and `git diff --check` from that worktree.
6. Run one independent read-only review against the exact frozen base.
7. Repair confirmed findings in the owning worktree and repeat affected gates/review.
8. Commit locally. Do not push or create a PR.
9. Before integration, prove local `master` still equals the unit's expected integration base. Fast-forward/cherry-pick only the reviewed commit sequence; if master moved, regenerate the integration result and relevant verification from the new dependency head.
10. After merge and evidence capture, remove the merged worktree and its worktree-local cache. Preserve the main shared cache and the four user-owned untracked review documents.

At most three workers run concurrently because the parent occupies one of four slots. A worker may implement only its assigned files, run focused verification, and return evidence; it may not commit, push, open a PR, or delegate recursively. The parent owns review acceptance, commits, and master integration.

## Execution Waves

### Wave 0: Freeze the runtime contract

Implement Contract C Task 1, review it, and integrate it. Then implement Contract C Task 2 from that head, review it, and integrate it. Do not start generation work before Task 2 is accepted.

Success criteria:

- `TaskOwner` has only constructor plus `Go` and validates bounded prefix/component syntax.
- `RequestTaskGroup` method set is unchanged.
- raw panic identity is replayed only after all accepted children join.
- compiler owner derives from `selected.instance`, not mutable plugin state.
- `BasePlugin.TaskRegistry()` is deleted rather than retained as a forwarding compatibility method.

### Wave 1: Independent ownership migrations

From reviewed Contract C Task 2, run up to three disjoint units concurrently, replenishing slots only after review/integration:

- generation active-health resource ownership;
- generation proxy-cache cleanup;
- request stream bridge;
- request batch/lease ownership;
- request Kafka ownership;
- request proxy-mirror lifecycle;
- request MCP session ownership;
- generation GraphQL cache, delayed sync, OAS refresh, and log rotation units;
- persistent stream runtime.

Integrate each accepted unit in the deterministic order recorded in its child plan. Re-run its focused consumer gate if a preceding integrated unit touched an imported package or shared test seam.

### Wave 2: Explicit shared-path dependencies

Execute in this order:

1. request MQTT after the reviewed bridge commit;
2. generation AI health;
3. request AI periodic flush from the AI-health integrated head;
4. generation logger batch and every constructor caller;
5. generation file logger from logger-batch head;
6. logger cancellation helpers and RocketMQ shutdown from logger-batch head, serialized after any overlapping constructor edits.

No two worktrees in this wave may modify the same production file from the same base.

### Wave 3: Governance and final integration

After every production migration is integrated:

1. implement Contract C Task 5's AST test;
2. demonstrate the frozen-base RED inventory and integrated-head PASS;
3. run Contract C Task 6's exact normal/race/lint/build gates;
4. run dead-helper, raw-spawn, owner-prefix, and `RequestTaskGroup` method-set scans;
5. run an independent merge-level review over `b0220dce..HEAD`;
6. repair findings only in their owning unit, re-integrate, and restart affected final gates;
7. fast-forward local `master` to the accepted Task 11 head and remove merged Task 11 worktrees/caches.

## Progress Checkpoints

Report at these boundaries:

1. Contract C Task 1 merged: exact SHA, tests, review findings.
2. Contract C Task 2 merged: exact SHA and frozen-base SHA for parallel waves.
3. Each wave: completed units, current unit, Task 11 percentage, total-program percentage, active worktrees, and blockers.
4. Before final merge: complete commit list, AST inventory delta, normal/race/lint/build evidence, review result, and known verification limits.
5. After master integration: master SHA, worktree/cache cleanup, no-push statement, Task 12 handoff.

Progress is computed from accepted implementation units, not agent activity. A plan draft or running worker is not counted as implementation completion.

## Completion Criteria

Task 11 is complete only when all are true:

- the authoritative AST gate finds no raw `go` or syntactic `sync.WaitGroup.Go` under `pkg/plugin`, `pkg/proxy`, `pkg/route`, or `pkg/stream`;
- every plugin owner is attempt-qualified through compiler `selected.instance`;
- no shared cluster, file-writer epoch, or persistent stream runtime borrows a generation registry;
- request timeout paths retain their lifecycle/generation leases until accepted children finish;
- unknown child panic joins siblings and re-panics the same raw value; exact `http.ErrAbortHandler` behavior from Task 10 is unchanged;
- cancellation-ignoring work remains observable under its bounded owner and resources are not released before it exits;
- focused normal/race suites, scoped lint, `make build`, and `git diff --check` pass on the final integrated head;
- independent review has no unresolved Critical, Important, or Minor findings;
- local `master` contains the reviewed commits, no push/PR occurred, merged Task 11 worktrees are removed, and user-owned untracked files remain untouched.

## Explicit Non-Goals

- No repository-wide `go test ./...`, `go test ./pkg/...`, `make test`, or full integration aggregation unless separately requested.
- No new dependency, general supervisor framework, public task interface, request status API, or package-specific AST allowlist.
- No bootstrap/server goroutine migration outside the parent Task 11's four AST roots.
- No behavior cleanup unrelated to goroutine ownership, lifetime, panic identity, or required testability seams.
