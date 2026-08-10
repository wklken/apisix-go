# Store Snapshot and Etcd Acknowledgement Implementation Plan

> **Execution owner:** `$fast-plan-impl` with bounded implementation workers. WU-01 fixes the shared Store contract first; WU-02 and WU-03 may run in parallel only after WU-01 is accepted.

**Goal:** Close PR-010 and PR-018. Snapshot rebuilds cannot lose concurrent updates, Store apply errors are returned to producers, consumer/index side effects remain last-good on failed persistence, and etcd revisions advance only after a complete batch is durably applied. Export a bounded config-apply failure/readiness metric.

**Architecture:** Replace the snapshot dirty boolean with a monotonic generation. Acknowledged pooled events use a buffered result plus a waiter-completion handshake so reuse cannot race `Wait`. The Store event loop returns transaction/validation failures, publishes bbolt before dependent indexes/hooks/generations, and lets `Sync() error` report errors from earlier unacknowledged events. Etcd applies each snapshot/watch batch against staged key/revision state, commits watcher state only after all acknowledgements, and enters snapshot recovery on apply failure. Dynamic standalone reloads use an acknowledged callback that restores the previous file snapshot after Store apply failure, while the existing callback API remains source-compatible.

**Tech Stack:** Go 1.26 atomics/channels, `sync.Pool`, bbolt, etcd watcher fakes, Prometheus, deterministic race tests.

## Acceptance contract

- `NewAcknowledgedEvent` plus `Wait(ctx)` delivers one apply result. The processor does not reset/reuse an acknowledged Event until the waiter has received the result or returned on context cancellation.
- An event that was never enqueued is returned to the pool by its producer. An enqueued event is returned only by Store.
- `Sync() error` is an in-order barrier. It returns and clears the joined errors of prior unacknowledged events. Errors already delivered through acknowledged events are not repeated by a later `Sync`.
- Consumer PUT validation happens before bbolt; bbolt commit happens before publishing the new consumer index. Failed PUT/DELETE preserves the prior bbolt value and every in-memory identity/reference index.
- Failed mutations do not publish SSL/proto/config generation state or invoke route/stream update hooks.
- Every successful mutation of `routes`, `global_rules`, or `plugin_metadata` increments `configGeneration` after durable commit.
- A `ConfigSnapshot` is published only when the generation captured before its bucket reads still equals the generation after the reads. Otherwise the build repeats under the existing mutex. Fast-path cache hits also compare generation.
- Etcd snapshot/watch batches mutate cloned `knownKeys` and candidate revision only. Original watcher state changes once, after all events acknowledge success.
- Watch apply failure does not terminate silently: context cancellation exits; otherwise the current watch stream is abandoned, snapshot recovery runs from the last durable revision, and replay resumes at recovered revision + 1.
- Initial etcd and standalone sync errors fail server startup. Dynamic standalone sync failure does not publish route/stream reload.
- A failed dynamic standalone apply retains the previous watcher snapshot, so the next file event replays every uncommitted route/stream diff. The legacy exported reload callback keeps its original signature and may reenter `ReloadSnapshot` without deadlock.
- Prometheus exports `config_apply_failures_total` and `config_apply_ready` with no unbounded labels. Any etcd apply failure increments the counter and sets readiness to 0; a successful acknowledged snapshot/watch sets readiness to 1.
- This PR provides configuration-apply readiness through metrics. A general HTTP readiness endpoint remains owned by the later observability/runtime health plan.
- No dependency changes.

## Fixed Store interfaces

```go
func NewAcknowledgedEvent() *Event
func (e *Event) Wait(ctx context.Context) error
func (s *Store) Sync() error
```

Acknowledged Event internals use `result chan error` buffered to one and `waitDone chan struct{}`. Store sends the result, waits for `waitDone`, then calls `PutBack`. `Wait` copies both channel references to locals before selecting and always closes `waitDone`, including cancellation, so pool reset cannot race later Event field reads.

The Store event loop tracks `pendingUnacknowledged error`. A normal unacknowledged event joins its apply error into this accumulator. An acknowledged normal event delivers its own result without joining. A Sync barrier receives the accumulator and clears it.

## Work units

### WU-01: Store acknowledgement and durable side-effect ordering

**Exclusive files:**

- `pkg/store/event.go`
- `pkg/store/store.go`
- `pkg/store/store_test.go`
- `pkg/store/consumer_snapshot_test.go`
- `pkg/store/event_ack_test.go` (new)
- `pkg/store/durable_apply_test.go` (new)

**Required shared fields for WU-02:**

- Replace `configSnapshotDirty` with `configGeneration atomic.Uint64`.
- Add test-only `afterConfigSnapshotBucketRead func(string)` to `Store`; production leaves it nil.

**Steps:**

- [ ] Add acknowledged Event tests first: success/error exactly once, cancellation, delayed waiter, repeated pool reuse under `-race`, and enqueue-failure ownership.
- [ ] Implement the result/waitDone handshake and reset every acknowledgement/barrier field in `PutBack` only after ownership is safe.
- [ ] Make `processEvent` return its validation/transaction error. Implement event-loop pending-unacknowledged error accumulation and `Sync() error` barrier semantics.
- [ ] Reorder consumer PUT to `prepare -> bbolt commit -> publish snapshot`; DELETE to `bbolt commit -> delete index`. Remove the lossy rollback of a newly published index.
- [ ] Increment config/proto generations and publish SSL/hooks only after successful persistence.
- [ ] Seed a writable database, reopen it read-only, and use acknowledged events to prove failed route/SSL/proto/consumer mutations preserve last-good durable and in-memory state and do not fire hooks/generations.
- [ ] Prove invalid consumer configuration is returned by both acknowledged `Wait` and an unacknowledged `Sync`, without repeating an acknowledged error at the next Sync.

**Focused verification:**

```bash
bash -lc 'source .envrc && go test -race ./pkg/store -run "(Acknowledg|ApplyFailure|Consumer.*(Failure|Rollback)|Sync.*Error)" -count=1'
```

### WU-02: Snapshot generation and deterministic lost-update regression

**Depends on:** accepted WU-01 `Store.configGeneration` and `afterConfigSnapshotBucketRead` fields.

**Exclusive files:**

- `pkg/store/getter.go`
- `pkg/store/config_snapshot_test.go` (new)

**Steps:**

- [ ] Add a deterministic regression first: after the routes bucket read, the hook enqueues and waits for a second acknowledged route event. The first `getConfigSnapshot` call must return both routes.
- [ ] Add an unexported generation to `ConfigSnapshot`.
- [ ] Fast path returns a cached snapshot only when `snapshot.generation == configGeneration.Load()`.
- [ ] Under `configSnapshotMu`, loop: capture generation, build all three buckets, recheck generation, and publish only on equality.
- [ ] Invoke the test hook immediately after each snapshot-owned bucket read; do not expose it through a production API.
- [ ] Cover route, global-rule, and plugin-metadata generation changes and concurrent callers under race.

**Focused verification:**

```bash
bash -lc 'source .envrc && go test -race ./pkg/store -run "^TestConfigSnapshot" -count=1'
```

### WU-03: Etcd durable batches, server sync propagation, and apply metrics

**Depends on:** accepted WU-01 acknowledged Event and `Sync() error` APIs.

**Exclusive files:**

- `pkg/etcd/watcher.go`
- `pkg/etcd/watcher_test.go`
- `pkg/server/server.go`
- `pkg/server/reload_test.go`
- `pkg/server/server_test.go` only for existing config-provider startup fixtures
- `pkg/observability/metrics/config_apply.go` (new)
- `pkg/observability/metrics/config_apply_test.go` (new)
- `pkg/observability/metrics/prometheus.go`
- `pkg/observability/metrics/prometheus_test.go` only when lifecycle registration coverage must change
- `pkg/config/standalone.go` and `pkg/config/standalone_test.go` for acknowledged dynamic apply/rollback while preserving the legacy callback API

**Steps:**

- [ ] Add metrics tests first for nil-before-init behavior, private registry counter/gauge values, and fixed no-label cardinality. Initialize metrics before config fetch in server startup so initial state is not lost.
- [ ] Change `sendEvent` to return `error`: create an acknowledged event, clone fields, enqueue with context, then wait. On enqueue cancellation the producer calls `PutBack`; after enqueue only Store owns pooling.
- [ ] Rewrite existing watcher tests to use a real started Store instead of reading pooled events directly. Inspect durable buckets and watcher state after acknowledgements.
- [ ] Stage `knownKeys` and revision for `applySnapshot`/`applyWatchResponse`; commit them only after every event succeeds. Return errors instead of booleans.
- [ ] On non-context watch apply error, mark config apply unhealthy, break the stream, recover a snapshot, and reopen from recovered revision + 1. Keep original state unchanged until recovery succeeds.
- [ ] Add batch tests: second event failure leaves revision/knownKeys unchanged; recovery snapshot succeeds and replay resumes; context cancellation exits without retry.
- [ ] Change `fetchAndSyncInitialEtcdConfig` and standalone initial sync to propagate `Sync() error`. Make dynamic `applyStandaloneSnapshot` return an error and skip route/stream reload on sync failure.
- [ ] Record config apply failure/success at acknowledged etcd batch boundaries. Do not use raw errors, keys, or bucket names as labels.
- [ ] Make the server use an acknowledged standalone callback. On apply failure, restore the prior file snapshot so a later event replays the complete diff; invoke legacy callbacks outside the reload serialization lock.

**Focused verification:**

```bash
bash -lc 'source .envrc && go test -race ./pkg/etcd ./pkg/server ./pkg/observability/metrics -run "(Apply|Snapshot|Watch|InitialEtcd|Standalone.*Sync|ConfigApply)" -count=1'
```

## Dependency order and dispatch

1. Dispatch WU-01 alone and accept its fixed API/fields.
2. Dispatch WU-02 and WU-03 in parallel. Their production/test paths are disjoint.
3. Each worker gets at most three implementation/verification cycles or 20 minutes, one optional follow-up, local mutation/focused verification only, and no commit/push/PR authority.
4. Parent inspects reports and combined diff, then requests an independent merge-level review.

## Combined verification

The `Sync() error` signature also requires explicit result handling in existing tests and benchmarks under `pkg/config`, `pkg/route`, `pkg/plugin/basic_auth`, `pkg/plugin/grpc_transcode`, and `pkg/server`; these are mechanical call-site adaptations, not behavior changes.

```bash
bash -lc 'source .envrc && go test -race ./pkg/config ./pkg/store ./pkg/etcd ./pkg/server ./pkg/observability/metrics -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/config/... ./pkg/store/... ./pkg/etcd/... ./pkg/server/... ./pkg/observability/metrics/... ./pkg/route/... ./pkg/plugin/basic_auth/... ./pkg/plugin/grpc_transcode/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

No broad repository suite or real-process `t/plugin` run is required; no integration manifest changes.

## Delivery

- [ ] Independent reviewer reports APPROVE on the frozen diff.
- [ ] Commit only the plan and accepted implementation paths with `git commit -m "fix(store): acknowledge durable control-plane updates"`.
- [ ] Push `codex/prod-ready-store-etcd-ack`, open one ready PR against `master`, and merge only after remote CI is green and the PR head matches the reviewed commit.
