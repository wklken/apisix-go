# Standalone Watcher and Store Lifecycle Implementation Plan

> **Execution owner:** `$fast-plan-impl` with bounded implementation workers. WU-01 and WU-02 have disjoint files and may run in parallel; WU-03 starts only after both fixed interfaces are accepted.

**Goal:** Close PR-011. Standalone event production is cancellable and durably acknowledged, the persisted Store is reconciled as the authoritative file baseline after restart, config producers stop before Store, every startup/serve error cleans acquired resources, and the process can construct a fresh Server after shutdown.

**Architecture:** Give `StandaloneFileWatcher` an owned context, idempotent `Start`/`Stop`, ordered reload serialization, and acknowledged Store events emitted without holding the state mutex. Add one consistent Store bucket-snapshot API, persisted consumer-index reconstruction, stopped-state synchronization, and singleton clearing after close. Server owns either standalone or etcd behind `configProducer.Stop() error`; etcd ownership includes child-context cancel, Watch completion, and client close. Server shutdown first reaches retryable HTTP quiescence, then completes the ordered teardown `producer -> reload scheduler -> stream -> routes -> clusters -> Store -> observability` exactly once.

**Tech Stack:** Go 1.26 contexts/channels/`sync.Once`, fsnotify, bbolt read transactions, Plan-7 acknowledged Store events, real Store lifecycle tests, and race detection.

## Acceptance contract

- `StandaloneFileWatcher.Start() error` and `Stop() error` are idempotent. `Stop` cancels and waits for the watch goroutine, never closes the shared Store event channel, and is safe before/after Start.
- Reload ordering uses a dedicated mutex. Snapshot diff/state mutexes are not held across event send, `Event.Wait`, callback, or filesystem I/O.
- Production standalone events use `NewAcknowledgedEvent`. Before enqueue, producer owns pooling; after enqueue, Store owns pooling. Cancellation unblocks both a blocked send and an acknowledged wait.
- A standalone baseline advances only after every event in its ordered diff acknowledges success. Failure/cancellation keeps the last-good baseline so the next reload deterministically replays the full diff.
- Existing exported `SetReloadCallback` and `Watch` compatibility remains. Server uses the acknowledged/durable path and propagates fsnotify setup failure.
- Startup seeds the watcher from one consistent bbolt read of every standalone-owned bucket, preserving exact consumer and nested secret keys. The first file diff deletes resources that remain in Store but were removed from the file.
- `Store.Stop()` closes bbolt and clears the package singleton only when it still points to that receiver. A later `GetStore` opens a new DB owner with the new event channel; stopping an older non-global Store cannot clear the replacement.
- `Store.Open()` rebuilds every consumer-derived authentication index from persisted data before returning. Invalid persisted consumers fail open with resource context; an unchanged standalone consumer remains usable after restart.
- `Store.Sync()` returns promptly when the Store is not started, stopping, or stopped. `GetStore` never returns a stopping singleton; callers may retry after Stop completes.
- Server owns exactly one config producer. Standalone Stop waits for fsnotify exit; etcd Stop cancels Watch, waits for exit, then closes the client.
- Server owns cancellation and completion for startup and the reload scheduler. Shutdown stops the config producer, cancels and joins Store users, then closes Store.
- An HTTP shutdown timeout retains active route dependencies and Store so a later call can finish quiescence. Startup binds every listener before serving; error cleanup is bounded, force-closes listeners/connections, and finishes dependency teardown only after active handlers exit.
- Any `Server.Start` return after resources are acquired invokes the same idempotent cleanup path. Completed Shutdown joins underlying errors and is stable on repeated calls.
- Route/stream generations are not published for a failed standalone batch. Successful batches retain the Plan-7 config-apply metric behavior.
- No dependency changes; no new general readiness endpoint.

## Fixed interfaces

```go
// pkg/config
func (w *StandaloneFileWatcher) Start() error
func (w *StandaloneFileWatcher) Stop() error
func (w *StandaloneFileWatcher) SeedCurrentSnapshot(snapshot map[string]map[string][]byte)
func StandaloneBuckets() []string

// pkg/store
func (s *Store) SnapshotBuckets(bucketNames []string) (map[string]map[string][]byte, error)

// pkg/server, private
type configProducer interface { Stop() error }
```

`SeedCurrentSnapshot` clones all keys/values. `SnapshotBuckets` reads every requested bucket inside one `db.View` transaction and returns cloned key/value bytes as strings/byte slices. Missing buckets are errors.

## Work units

### WU-01: Standalone watcher durable lifecycle

**Exclusive files:**

- `pkg/config/standalone.go`
- `pkg/config/standalone_test.go`

**Steps:**

- [ ] Add regression-first tests for blocked-send cancellation, cancellation during ack wait, state mutex availability while send is blocked, failed second event preserving baseline, Stop-before-Start, repeated Stop, and Watch exit waiting.
- [ ] Add owned context/cancel/done, `startOnce`, `stopOnce`, start/stop errors, and an owned fsnotify watcher lifecycle.
- [ ] Preserve `Watch()` as a logging compatibility wrapper around `Start() error`; server will call `Start` directly.
- [ ] Refactor reload into `read -> compute ordered acknowledged events under state lock -> unlock -> send/wait each -> commit current`. Return every never-enqueued remaining event to the pool on failure.
- [ ] Keep legacy callback execution outside reload serialization so reentrant `ReloadSnapshot` remains safe. Keep acknowledged callbacks serialized with commit/rollback.
- [ ] Add `StandaloneBuckets` and cloning `SeedCurrentSnapshot` without exposing mutable internal maps.
- [ ] Update existing standalone tests to use a real started Store or durable bucket assertions instead of manually consuming pooled events.

**Focused verification:**

```bash
bash -lc 'source .envrc && go test -race ./pkg/config -run "Standalone" -count=1'
```

### WU-02: Store restart and persisted baseline API

**Exclusive files:**

- `pkg/store/store.go`
- `pkg/store/store_test.go`
- `pkg/store/standalone_snapshot.go` (new)
- `pkg/store/standalone_snapshot_test.go` (new)

**Steps:**

- [ ] Add regression-first tests that `GetStore` reopens after Stop with a different event channel and that stopping a previous Store cannot clear a newer global Store.
- [ ] Clear the package singleton only after the matching Store has fully stopped and bbolt is closed; serialize the close/clear boundary against `GetStore`.
- [ ] Implement `SnapshotBuckets` as one bbolt read transaction with exact cloned bucket keys/values.
- [ ] Cover consumer IDs, nested secret IDs such as `vault/item`, missing bucket failure, and mutation of returned data not aliasing bbolt memory.
- [ ] Rebuild persisted consumer lookup/reference/value indexes during Open and reject invalid persisted consumer data with bucket/id context.
- [ ] Make Sync reject not-started/stopping/stopped Stores and make GetStore reject a stopping singleton without blocking callback reentrancy.

**Focused verification:**

```bash
bash -lc 'source .envrc && go test -race ./pkg/store -run "(GetStore.*Stop|StopClears|SnapshotBuckets|StandaloneBaseline)" -count=1'
```

### WU-03: Server producer ownership, startup cleanup, and shutdown order

**Depends on:** accepted WU-01 and WU-02 interfaces.

**Exclusive files:**

- `pkg/server/server.go`
- `pkg/server/server_test.go`
- `pkg/server/reload_test.go`
- `cmd/root.go` and `cmd/root_test.go` only if Start-owned cleanup needs a command-boundary assertion

**Steps:**

- [ ] Add private `configProducer`. Make standalone and an etcd wrapper implement Stop; the etcd wrapper owns child cancellation, Watch done, and client close.
- [ ] Before initial standalone reload, call `storage.SnapshotBuckets(config.StandaloneBuckets())`, seed the watcher, set its acknowledged callback, and retain it as the producer before any operation that can fail.
- [ ] Start fsnotify with `Start() error` after initial route/stream construction; propagate setup failures.
- [ ] Own and join both startup work and the reload scheduler; make `Server.Start` use the same cleanup path on config load, route build, stream start, Prometheus bind, HTTP bind/serve, and context-return paths.
- [ ] Bind every configured listener before starting any Serve goroutine. Bound startup-error cleanup and complete Store teardown asynchronously only after active handlers release their route generation.
- [ ] Make Shutdown retry HTTP quiescence after a timeout, then complete idempotently in order: cancel/wait producer and scheduler; close stream; drain routes; close clusters; stop Store; stop Prometheus/tracing. Join all errors.
- [ ] Add deterministic tests for persisted resource deletion on first standalone startup, producer-before-Store order, Start failure cleanup, etcd Watch wait, joined errors, repeated Shutdown, and creating a new Server/Store after shutdown.

**Focused verification:**

```bash
bash -lc 'source .envrc && go test -race ./pkg/server ./pkg/config ./pkg/store ./pkg/etcd -run "(Standalone|Shutdown|Stop|StartFailure|WatchExit|Reopen)" -count=1'
```

## Dependency order and dispatch

1. Dispatch WU-01 and WU-02 in parallel; their paths are disjoint.
2. Parent accepts both fixed interfaces, then dispatches WU-03.
3. Each worker gets at most three implementation/verification cycles or 20 minutes, one optional follow-up, local implementation/focused verification only, and no commit/push/PR authority.
4. Parent runs combined gates and requests independent merge-level review before delivery.

## Combined verification

```bash
bash -lc 'source .envrc && go test -race ./pkg/config ./pkg/server ./pkg/store ./pkg/etcd -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/config/... ./pkg/server/... ./pkg/store/... ./pkg/etcd/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

No broad repository suite or real-process `t/plugin` run is required.

## Delivery

- [ ] Independent reviewer reports APPROVE on the frozen diff.
- [ ] Commit only the plan and accepted implementation paths with `git commit -m "fix(config): close standalone and Store lifecycles"`.
- [ ] Push `codex/prod-ready-standalone-lifecycle`, open one ready PR against `master`, and merge only after remote CI is green and the PR head matches the reviewed commit.
