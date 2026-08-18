# File Logger End-to-End Performance Implementation Plan

> **For Codex:** REQUIRED SUB-SKILL: Use `fast-plan-impl` to execute this plan with test-first, bounded implementation workers. Workers may edit only their assigned files and may not commit, push, or open/update the PR.

**Goal:** Remove file-logger field construction and reflection-based `zap.Any` encoding from the request-finalizer path, eliminate avoidable snapshot copies, and implement real multi-entry batching while preserving the existing JSON line format, non-blocking admission, rotation, shutdown, and callback-isolation contracts.

**Architecture:** Keep the generic log executor responsible for producing one detached canonical snapshot and one private policy-bounded snapshot per callback. Transfer ownership of the file-logger callback snapshot into a file-logger-specific bounded channel without building fields. A single worker builds fields, encodes each zap-compatible JSON line with typed object/array marshalers, and coalesces encoded lines by entry count, encoded byte size, or first-entry age before one sink write. The existing shared per-path `BufferedWriteSyncer` remains the lower-level low-QPS/syscall buffer.

**Tech Stack:** Go 1.26, existing zap/zapcore v1.28.0, existing APISIX-Go snapshot and metrics packages, repository benchmark harness and benchstat.

## Scope and invariants

- Preserve the public plugin schema and all emitted access-log fields, including zap production envelope fields (`level`, `ts`, and `msg`).
- Preserve FIFO ordering for one plugin instance and newline-delimited JSON: one JSON object per accepted entry, never a JSON array.
- `RunLogPhase` must do only match evaluation plus a non-blocking channel send; it must not build the default/custom field map, convert bodies to strings, create `[]zap.Field`, call `zap.Any`, encode JSON, or perform file I/O.
- The channel is the entry queue and is bounded to 10,000 accepted records. A full or stopped queue returns the existing `base.ErrLogQueueFull` / `base.ErrLogQueueUnavailable` behavior through a narrow file-logger helper.
- The worker flushes a true batch when any boundary is reached: 256 entries, 64 KiB of encoded bytes, or 10 ms since the first entry. An entry larger than 64 KiB is written alone.
- A batch is one call to the shared sink `Write`, containing multiple complete JSON lines when available. The 64 KiB/1 s `zapcore.BufferedWriteSyncer` below it remains unchanged.
- Legacy `Handler` behavior remains synchronously observable after `ServeHTTP`: enqueue a prebuilt-field record followed by an in-order barrier; the barrier acknowledges only after all preceding records have been written and the sink has been synced.
- `Stop` closes admission, drains every already accepted record in FIFO order, syncs, then releases the shared writer lease. It is idempotent and must not race with producers or send on a closed channel.
- `FlushAndReopen` keeps its existing shared-writer contract. The plugin processor must never retain bytes outside the sink after acknowledging a legacy barrier or finishing `Stop`.
- Delivery/write errors must be observable to the processor and metrics; do not use `logger.Info`, which discards core write errors. Accepted records that cannot be delivered may be counted as failed drops, but must not be counted delivered.
- Preserve the existing per-callback deep-isolation contract for headers, query values, nested APISIX/request variables, and bodies. Optimizations may remove duplicate copies only where ownership is already detached and exclusive.
- Do not add a dependency or configuration option.

## Benchmark contract

Before any production edit, add and record an immutable baseline for an end-to-end benchmark that invokes a real `LogExecutor` and real file-logger callback. The timed region must include final snapshot construction, sanitizer/callback clone work, `RunLogPhase`, and queue admission; plugin `Stop` and file verification occur after timing.

Rows:

1. `BenchmarkFileLoggerLogExecutor/default` — representative default fields, headers, query, APISIX/request vars, no body. This is the primary request-finalizer row.
2. `BenchmarkFileLoggerLogExecutor/custom` — nested custom format expressions, no body.
3. `BenchmarkFileLoggerLogExecutor/bodies` — bounded request and response bodies plus representative headers/vars.

Use `BENCH_TIME=1000x`, `BENCH_COUNT=10`, `BENCH_CPU=1`, `BENCH_P=1`. Every row must stop the plugin and verify exactly `b.N` valid newline-delimited JSON objects so queue drops or incomplete shutdown invalidate the benchmark.

Acceptance thresholds, declared before measurement:

- Primary `default` row: at least 20% lower `ns/op` and at least 20% lower `B/op` versus the Phase 1 baseline.
- `custom` and `bodies` rows: no more than 5% `ns/op` regression; report all `ns/op`, `B/op`, and `allocs/op` deltas even when statistically insignificant.
- Existing `BenchmarkFileLoggerWrite` must not regress by more than 5% in `ns/op`; because its implementation seam changes, keep its corpus semantics equivalent and explain any harness adjustment.
- Threshold misses are stop conditions for performance claims: diagnose with the benchmark/profile instead of relabeling the change successful.

## Task 1: Freeze the end-to-end benchmark before production changes

**Files:**

- Create: `pkg/plugin/log_executor_benchmark_test.go`

- [x] Add a benchmark helper that initializes a real `file_logger.Plugin` against `b.TempDir()`, prepares a representative request/lifecycle/capture, creates a `LogExecutor` binding, completes the lifecycle, and invokes finalization once per iteration.
- [x] Add the three benchmark rows above without referring to future processor internals.
- [x] Move plugin `Stop`, file read, line count, and JSON validity checks outside the timed region.
- [x] Run the focused benchmark once as a smoke check.
- [x] Record immutable baseline label `file-logger-e2e-before` using the repository harness with the exact package, corpus file, regex, count, time, CPU, and P settings above.
- [x] Report the baseline artifact paths and metadata hash; do not edit production code in this task.

## Task 2: Remove redundant snapshot copies without weakening isolation

**Files:**

- Modify: `pkg/apisix/log/snapshot.go`
- Modify: `pkg/apisix/log/snapshot_test.go`
- Modify: `pkg/plugin/base/log_phase.go`
- Modify: `pkg/plugin/base/log_phase_test.go`
- Modify: `pkg/plugin/log_executor.go`
- Modify: `pkg/plugin/log_executor_test.go`

- [x] First add tests proving the current detachment and callback-isolation behavior for request/response headers, trailers, query values, nested safe maps, request body, and response body.
- [x] Add an owned-input snapshot builder used only when the caller already owns detached response capture and captured request-body bytes. It must still clone mutable live request headers/query/vars exactly once.
- [x] Remove `request.Clone` and the deliberate `http.NoBody` replacement from `runComposite`; build directly from the live request metadata without reading the live body again.
- [x] Transfer the already-detached response header/trailer/body returned by `ResponseCapture.Snapshot` into the canonical snapshot without cloning them a second time.
- [x] Transfer or copy the captured request body exactly once into the canonical snapshot without allowing later `LogRequestState` mutation to affect it.
- [x] Change policy cloning so each body is bounded and copied once, rather than copied in `CloneSnapshot` and copied again by `boundedBody`; all other mutable structures remain private per callback.
- [x] Allocate the full pre-sanitized snapshot only when at least one sanitizer implements `LogSnapshotSanitizerSelectorPlugin`. Sanitizers without selectors remain selected by default and do not require that clone.
- [x] Preserve sanitizer ordering, selector evaluation against the same pre-sanitized state, callback panic isolation, and snapshot-finalizer behavior.
- [x] Run exact new tests red before production edits, then green; run the affected package tests and focused race gate.

## Task 3: Replace map/zap.Any delivery with an asynchronous typed encoder and true batching

**Files:**

- Modify: `pkg/plugin/file_logger/plugin.go`
- Modify: `pkg/plugin/file_logger/plugin_test.go`
- Modify: `pkg/plugin/file_logger/benchmark_test.go`
- Create: `pkg/plugin/file_logger/processor.go`
- Create: `pkg/plugin/file_logger/processor_test.go`
- Create: `pkg/plugin/file_logger/encoder.go`
- Create: `pkg/plugin/file_logger/encoder_test.go`

- [x] First add a failing request-path test using a blocking/test hook around field construction or encoding. Prove `RunLogPhase` returns after admission while the worker is blocked, and prove the queued snapshot remains detached after the caller mutates its original value.
- [x] Add focused processor tests for non-blocking full-queue rejection, FIFO order, count flush, byte flush, timer flush, oversized-entry-alone behavior, barrier sync, drain-on-stop, idempotent stop, concurrent push/stop race safety, and write-error accounting.
- [x] Add encoder parity tests that decode old-compatible JSON lines and compare every expected default/custom/nested field and zap envelope field by value. Cover strings, booleans, signed/unsigned integers, floats, `nil`, `time.Time`, `time.Duration`, `error`, `[]string`, `[]any`, `map[string]string`, `map[string][]string`, and `map[string]any`; uncommon values may use a reflected fallback.
- [x] Replace `BatchProcessor` with a file-logger-specific processor whose bounded channel accepts an owned `base.LogSnapshot`, an already-built legacy field map, or an in-order barrier.
- [x] Keep match evaluation in `RunLogPhase`, then enqueue the snapshot by value. Move request/response body visibility checks, default/custom field construction, body conversion, DNS/upstream field construction, and JSON encoding into the single worker.
- [x] Build one zap production JSON encoder per worker. Encode fields with typed `zapcore.ObjectMarshaler` / `ArrayMarshaler` traversal and `zap.Inline`; remove production uses of `zap.Any` and `logger.Info` from file-logger delivery.
- [x] Coalesce encoded lines into one byte slice and one sink `Write` per true batch according to 256 entries / 64 KiB / 10 ms boundaries. Treat short writes as errors.
- [x] Preserve compatibility `sendBatch` only if tests or benchmarks need it; if kept, route it through the same typed encoder and one multi-line sink write, and return actual write errors.
- [x] Implement metrics using the existing `file-logger` batch observer. Pending/buffered/dropped/delivered/failed accounting must correspond to channel admission and completed sink writes.
- [x] Change legacy `enqueueHandler` to an in-order barrier instead of polling processor stats. It must retain the existing 500 ms maximum wait and sync before returning.
- [x] Make `Stop` reject new pushes, drain accepted records, flush encoded bytes, sync, and only then release the lease. Never close a channel that concurrent producers can still send to.
- [x] Keep shared writer registry, buffering, transient-error recovery, and rotation implementation unchanged unless a failing compatibility test demonstrates a required integration change.
- [x] Run exact new tests red then green, affected package tests, and focused race gate.

## Task 4: Combined acceptance and performance evidence

**Files:**

- Modify only if acceptance exposes a defect in the files assigned above.

- [x] Inspect the complete diff and verify no schema/output-contract/config changes and no unrelated cleanup.
- [x] Run focused tests for `pkg/apisix/log`, `pkg/plugin/base`, `pkg/plugin`, and `pkg/plugin/file_logger`.
- [x] Run `go test -race` for the same four concurrency-relevant packages.
- [x] Record `file-logger-e2e-final` with exactly the baseline settings, then run benchstat comparison and report every row.
- [x] Record the existing sink benchmark current result and compare it to the Phase 1 sink baseline when corpus metadata remains compatible.
- [x] The primary threshold passed, so the conditional focused CPU/allocation profile was not required.
- [x] Run `make lint`, `make build`, and `git diff --check` after the final production mutation.
- [x] Run an independent merge-level code review; resolve every confirmed Important/Critical issue and rerun affected verification.

## Task 5: Update PR #128

- [ ] Verify the diff contains only the Phase 1 buffered sink/metrics work plus this Phase 2 plan, benchmark, snapshot, encoder, and processor work.
- [ ] Commit with a specific Conventional Commit message.
- [ ] Push `codex/file-logger-buffered` without force to the existing remote branch.
- [ ] Update PR #128 body with both architecture layers, benchmark evidence, correctness/race/lint/build gates, and explicit remaining limitations.
- [ ] Verify PR #128 head SHA, base `master`, open state, and check status. Do not merge.

## Stop conditions

- Stop and report if emitted JSON compatibility requires removing zap envelope fields or changing the public schema.
- Stop and report if preserving generic callback isolation requires an unsafe shared snapshot or live-request access after finalization.
- Stop and report if the benchmark cannot detect queue drops/incomplete shutdown or baseline/current metadata are not comparable.
- Stop and report if concurrent `RunLogPhase`/`Stop` cannot be made race-free without changing the generic logger interface outside the listed files.
- Do not expand this PR into generic logger-batch refactoring; this phase is file-logger-specific except for the bounded snapshot-copy reductions.
