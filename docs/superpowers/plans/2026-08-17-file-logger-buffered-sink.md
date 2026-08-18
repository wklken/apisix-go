# File Logger Buffered Sink Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the per-log-entry file syscall bottleneck from `file-logger` by adding a shared, rotation-safe zap buffer while preserving asynchronous request handling, bounded queue behavior, legacy Handler visibility, and shutdown durability.

**Architecture:** Keep `logger_batch.Processor` as the asynchronous request-to-worker boundary. Replace each registry-owned raw file sink with one `zapcore.BufferedWriteSyncer` shared by all plugin instances that resolve to the same canonical path. The registry remains the sole owner of reopen and final-close lifecycle. A one-second periodic flush bounds the application-buffer loss window; an explicit sync preserves the compatibility Handler contract.

**Tech Stack:** Go 1.26, zap v1.28.0, `zapcore.BufferedWriteSyncer`, the existing `logger_batch` processor and Prometheus metrics, repository benchmark runner and benchstat.

**Spec:** This plan is the implementation specification. No separate design document is required.

## Global Constraints

- Execute from `/Users/wklken/workspace/tx/wklken/apisix-go/.worktrees/file-logger-buffered` on branch `codex/file-logger-buffered`, based on `origin/master` revision `b0e12508393ca7863363990515bf0332a27ae5ab`.
- Source `.envrc` before every Go, Make, lint, or benchmark command.
- Preserve the production async boundary: `RunLogPhase` may serialize and enqueue, but it must not wait for disk I/O.
- Preserve the existing queue capacity, `BatchMaxSize: 1`, and mutex/condition-variable processor. Converting the shared batch processor to a channel is out of scope.
- Do not add public file-logger configuration in this PR. Use a fixed 64 KiB buffer and one-second flush interval per canonical output path.
- Do not change request/response snapshot construction, body limits, queue accounting, or failure semantics in `sendBatch`. Entry-count versus byte-count admission needs a separate cross-logger design.
- Preserve shared-path leases, `SIGUSR1` reopen, external rotation behavior, `0600` creation mode, metadata precedence, match semantics, and body redaction.
- A final registry lease release must stop the zap buffer ticker, flush buffered bytes, close the underlying file, remove the registry entry, and stop the signal watcher.
- Workers may make local implementation and focused-test changes only. They must not commit, push, create a PR, or delegate recursively.
- Use strict TDD: add the focused behavior or benchmark first, observe the required red state, then make the minimum production change.

---

## Task 1: Add a Reproducible File Sink Benchmark and Buffered Writer Tests

**Files:**

- Create: `pkg/plugin/file_logger/benchmark_test.go`
- Modify: `pkg/plugin/file_logger/plugin_test.go`

- [ ] Add `BenchmarkFileLoggerWrite` using a plugin initialized with a temporary output path and a stable representative log map. Invoke `sendBatch` directly so the benchmark covers zap JSON encoding plus the file sink without measuring queue scheduling. Stop the plugin in cleanup so the final buffer is flushed.
- [ ] Add a registry-level test that acquires a writer lease, writes a short byte slice, and verifies the data is not visible before `Sync` but is visible after `Sync`. The test must compile against the unmodified implementation and initially fail because the current raw writer writes immediately.
- [ ] Add or extend a rotation test to prove that `FlushAndReopen` flushes pre-reopen buffered bytes to the old inode, then routes later bytes to the recreated current path.
- [ ] Extend final-lease cleanup coverage to prove buffered data is present after the last lease is released.
- [ ] Run the new behavior test before production changes and record the expected failure.
- [ ] Install the pinned benchstat tool without editing dependencies:

```bash
source .envrc && make init-bench
```

- [ ] Record an immutable pre-change baseline after the benchmark compiles but before production changes:

```bash
source .envrc && \
  BENCH_DIR=.cache/bench/file-logger-buffered \
  BENCH_PACKAGES=./pkg/plugin/file_logger \
  BENCH_CORPUS_FILES=pkg/plugin/file_logger/benchmark_test.go \
  BENCH_REGEX='^BenchmarkFileLoggerWrite$' \
  BENCH_TIME=500ms BENCH_COUNT=10 BENCH_CPU=1 BENCH_P=1 \
  bash scripts/benchmark.sh run file-logger-raw
```

Expected: a published baseline containing `ns/op`, `B/op`, and `allocs/op`. Primary metric is `ns/op`; the practical target is at least a 20% reduction with no material allocation regression.

## Task 2: Introduce the Shared Buffered zap Sink

**Files:**

- Modify: `pkg/plugin/file_logger/plugin.go`
- Modify: `pkg/plugin/file_logger/writer_registry.go`
- Test: `pkg/plugin/file_logger/plugin_test.go`

- [ ] Define internal constants for a 64 KiB buffer and one-second flush interval.
- [ ] Add a small registry-owned wrapper around the existing `appendFileWriteSyncer` and `zapcore.BufferedWriteSyncer`. The wrapper must expose the `zapcore.WriteSyncer` methods needed by the core plus a rotation-safe reopen and a final stop/close operation.
- [ ] Serialize explicit writes and reopen boundaries. `FlushAndReopen` must not allow a new write to enter the old file after the buffer has been flushed and the underlying descriptor has reopened.
- [ ] Construct the buffered writer once in `fileWriterRegistry.acquire` for each canonical path. All leases for the same path must point to the same wrapper and therefore the same buffer and ticker.
- [ ] Change final lease release from raw `Close` to the wrapper's stop/close path. The zap buffer ticker must not survive the last lease.
- [ ] Keep `Plugin.Stop` ordering: stop/drain the batch processor, sync zap, then release the registry lease.
- [ ] Preserve immediate file visibility only for the legacy direct `Handler`: after `enqueueHandler` observes zero pending/processing/buffered entries, call `p.logger.Sync()` before returning. Do not add this sync to `RunLogPhase` or `sendBatch`.
- [ ] Run focused tests:

```bash
source .envrc && go test ./pkg/plugin/file_logger -count=1
source .envrc && go test -race ./pkg/plugin/file_logger -count=1
```

Expected: buffered visibility, rotation, final release, existing shared writer, Handler, metadata, match, body, and redaction behavior all pass; race detector reports no race.

## Task 3: Add file-logger to Bounded Batch Metrics

**Files:**

- Modify: `pkg/observability/metrics/logger_batch.go`
- Modify: `pkg/observability/metrics/logger_batch_test.go`

- [ ] Add a test that acquires a `file-logger` observer, changes pending state, emits a supported event, and proves the pending gauge and event counter use the `file-logger` label. Run it first and observe failure because the allowlist rejects this plugin.
- [ ] Add `file-logger` to `loggerBatchPluginIDs`; do not weaken label validation or admit arbitrary plugin names.
- [ ] Keep the current behavior that file-logger's empty route/server labels suppress only the legacy `BatchProcessEntries` series. The plugin-specific pending and event series remain valid and useful.
- [ ] Run the package test:

```bash
source .envrc && go test ./pkg/observability/metrics -count=1
```

Expected: the new file-logger observer test and all existing lifecycle/label tests pass.

## Task 4: Measure the Buffered Implementation

**Files:**

- Evidence only: `.cache/bench/file-logger-buffered/*` (ignored; do not commit)

- [ ] Run the current benchmark with settings identical to the baseline:

```bash
source .envrc && \
  BENCH_DIR=.cache/bench/file-logger-buffered \
  BENCH_PACKAGES=./pkg/plugin/file_logger \
  BENCH_CORPUS_FILES=pkg/plugin/file_logger/benchmark_test.go \
  BENCH_REGEX='^BenchmarkFileLoggerWrite$' \
  BENCH_TIME=500ms BENCH_COUNT=10 BENCH_CPU=1 BENCH_P=1 \
  bash scripts/benchmark.sh run file-logger-buffered
```

- [ ] Compare only if metadata and corpus fingerprints match:

```bash
source .envrc && \
  BENCH_DIR=.cache/bench/file-logger-buffered \
  BENCH_PACKAGES=./pkg/plugin/file_logger \
  BENCH_CORPUS_FILES=pkg/plugin/file_logger/benchmark_test.go \
  BENCH_REGEX='^BenchmarkFileLoggerWrite$' \
  BENCH_TIME=500ms BENCH_COUNT=10 BENCH_CPU=1 BENCH_P=1 \
  bash scripts/benchmark.sh compare file-logger-raw file-logger-buffered
```

- [ ] Report every benchmark row, `ns/op`, `B/op`, `allocs/op`, confidence intervals, and whether the predeclared 20% latency target was met. A mismatch or noisy result is not a verified optimization.

## Task 5: Document the Actual Runtime Contract

**Files:**

- Modify: `docs/plugins.md`
- Modify: `docs/design.md`

- [ ] Update only the `file-logger` status row to mention shared per-path zap buffering, one-second periodic flush, rotation-safe explicit flush/reopen, and bounded asynchronous queue metrics.
- [ ] Extend the logger batch resource-ownership design text to include the file sink and make the two buffers explicit: entries are queued in `logger_batch`; encoded bytes are then buffered by the shared per-path zap sink.
- [ ] State the durability boundary: process crash can lose bytes still inside the one-second application buffer; orderly `Stop` and reopen flush them.
- [ ] Keep deferred items explicit: APISIX/OpenResty lrucache expiry semantics, queue byte admission, and durable retry/error propagation remain out of scope.

## Task 6: Integration Acceptance

**Files:**

- Review all changed files and ignored benchmark evidence.

- [ ] Inspect `git diff --check`, `git diff --stat`, and the complete diff. Reject unrelated cleanup or dependency changes.
- [ ] Run the combined focused packages:

```bash
source .envrc && go test ./pkg/plugin/file_logger ./pkg/plugin/logger_batch ./pkg/observability/metrics -count=1
source .envrc && go test -race ./pkg/plugin/file_logger -count=1
```

- [ ] Run the repository lint gate and build smoke check required for this code change:

```bash
source .envrc && make lint
source .envrc && make build
```

- [ ] Perform an independent read-only code review focused on lock ordering, ticker ownership, shutdown/reopen races, Handler blocking behavior, benchmark validity, and metric label cardinality.
- [ ] Fix only confirmed findings and rerun the smallest affected verification plus the final integration gates.
- [ ] Commit only intended files with a Conventional Commit message, push `codex/file-logger-buffered`, and create a PR against `master`. Do not merge without separate authorization.

## Stop Conditions

- Stop if the buffer cannot be shared per canonical path without breaking lease or rotation semantics.
- Stop if the only correct implementation requires changing public plugin schema, logger_batch behavior for all loggers, or dependency versions.
- Stop if the benchmark corpus cannot remain identical between raw and buffered runs; report measurements as invalid rather than claiming a win.
- Stop if focused race tests expose a pre-existing or new lifecycle race that cannot be repaired inside `pkg/plugin/file_logger` without broad refactoring.
