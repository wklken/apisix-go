# Etcd Store Reliability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make etcd snapshots/watch responses atomic and authoritative, publish one internally consistent HTTP/stream generation, and make disconnect, compaction, restart, invalid-resource, lock-contention, and cancellation behavior fail-safe.

**Architecture:** Keep the existing Store event loop and last-good publication model, but add an acknowledged batch event that validates first and commits one bbolt transaction. The etcd watcher filters the exact managed namespace, retries a validation-pruned batch once, and advances revision state only after durable Store plus required stream publication succeeds. Route builds consume a complete immutable Store snapshot. Readiness gains an optional required stream stage.

**Tech Stack:** Go 1.26, etcd client v3, bbolt, Prometheus client, existing Store event pool and server reload scheduler.

**Spec:** `docs/superpowers/specs/2026-08-19-etcd-store-reliability.md`

## Task 1: Add the atomic Store batch contract

**Files:**
- Modify: `pkg/store/event.go`
- Modify: `pkg/store/store.go`
- Modify: `pkg/store/standalone_snapshot.go`
- Test: `pkg/store/event_ack_test.go`
- Test: `pkg/store/durable_apply_test.go`
- Test: `pkg/store/store_test.go`
- Test: `pkg/store/standalone_snapshot_test.go`

- [ ] Write failing tests proving a two-mutation batch does not persist its first mutation when the second is invalid.
- [ ] Write a failing restart test that seeds stale routes/services/consumers, applies an authoritative replacement, and proves absent rows and derived consumer indexes are removed.
- [ ] Write failing tests proving quarantine preservation keeps an existing last-good row while unrelated valid rows commit in the same replacement.
- [ ] Write failing tests proving a committed batch increments each generation once and invokes each affected bucket hook once.
- [ ] Add the cloned `Mutation`, `ResourceKey`, `BatchOptions`, `RejectedMutation`, and `BatchValidationError` value types plus `NewAcknowledgedBatch`.
- [ ] Validate every mutation before the transaction. Validate route/global-rule/SSL/consumer behavior already covered by single events and add strict plugin-metadata JSON-object decoding.
- [ ] In one `db.Update`, optionally clear all managed buckets except preserved bucket/id rows, then apply every candidate mutation.
- [ ] Publish consumer/SSL/cache/generation side effects only after commit. Preserve the existing single-event API by routing it through the same mutation machinery.
- [ ] Add `AddAcknowledgedEventUpdateHook(func(*Event) error)` without changing the legacy notification-hook signature. Return acknowledged hook failures after durable commit so providers can retry publication.
- [ ] Open bbolt with a finite timeout and change persisted-consumer index rebuild to log/skip invalid rows while still returning actual database errors.
- [ ] Run focused RED/GREEN verification:

```bash
bash -lc 'source .envrc && go test -race ./pkg/store -run "(Batch|Authoritative|PersistedConsumer|StoreLock|Acknowledged)" -count=1'
```

## Task 2: Add an optional required stream readiness stage

**Files:**
- Modify: `pkg/observability/metrics/config_apply.go`
- Test: `pkg/observability/metrics/config_apply_test.go`

- [ ] Write failing tests showing HTTP-only readiness remains provider+HTTP, while stream-required readiness stays false until provider, HTTP, and stream all succeed.
- [ ] Write a failing test showing a later stream failure blocks readiness and increments the bounded failure counter, then stream success recovers it.
- [ ] Add `ConfigApplyStageStreams` and `SetConfigApplyStreamRequired(bool)`.
- [ ] Refactor readiness calculation into one locked helper used by collector updates and `GetReadiness`; do not add labels or a new metric family.
- [ ] Ensure collector replacement resets all stream-required/observed/healthy state deterministically.
- [ ] Run:

```bash
bash -lc 'source .envrc && go test -race ./pkg/observability/metrics -run "ConfigApply" -count=1'
```

## Task 3: Make the etcd namespace and response boundary exact

**Depends on:** Task 1 accepted Store batch API.

**Files:**
- Modify: `pkg/etcd/watcher.go`
- Test: `pkg/etcd/watcher_test.go`
- Test: `pkg/etcd/watcher_metrics_test.go`

- [ ] Add failing constructor tests proving `/apisix/` is used for Get/Watch, `/apisix2/...`, `/apisix/data_plane/server_info/...`, unknown buckets, and malformed collection keys never reach Store.
- [ ] Add a failing startup test that seeds a persisted stale Store row while `knownKeys` is empty, applies an etcd snapshot without that row, and proves deletion.
- [ ] Replace per-key `sendEvent` calls with one acknowledged batch per snapshot/watch response.
- [ ] On `BatchValidationError`, quarantine rejected keys, preserve their last-good rows for replacements, remove them from the batch, and retry once. All non-validation errors abort response application.
- [ ] Commit `knownKeys`, quarantine, metrics, and `lastRevision` only after the batch acknowledgement succeeds.
- [ ] Add watch-response tests proving a second invalid event cannot leave the first event durably visible before the pruned atomic retry, and a non-validation hook failure enters snapshot recovery without revision advancement.
- [ ] Add `FetchAllContext(ctx)` and keep `FetchAll()` as a background-context compatibility wrapper. Make startup retry waits cancellation-aware.
- [ ] Wire `WatchTimeout` and `ResyncDelay` through `ClientOptions`. Idle watch timeout reopens from `lastRevision+1` without a full snapshot or false unreachable transition; recovery delay uses configured base plus bounded 0-50 percent jitter.
- [ ] Run:

```bash
bash -lc 'source .envrc && go test -race ./pkg/etcd -run "(Namespace|Managed|Authoritative|Batch|Watch|Snapshot|FetchAll|Retry|Timeout)" -count=1'
```

## Task 4: Build HTTP routes from one complete Store generation

**Depends on:** Task 1 generation semantics.

**Files:**
- Modify: `pkg/store/getter.go`
- Test: `pkg/store/config_snapshot_test.go`
- Modify: `pkg/route/builder.go`
- Modify: `pkg/route/upstream_options.go`
- Modify: `pkg/plugin/traffic_split/plugin.go`
- Test: `pkg/route/builder_lifecycle_test.go`
- Test: `pkg/route/upstream_options_test.go`

- [ ] Write a failing Store test that mutates a service/upstream between logical reads and proves one snapshot cannot mix generations.
- [ ] Build `ConfigSnapshot` from routes, global rules, plugin metadata, services, upstreams, plugin configs, and SSLs inside one bbolt read transaction. Keep request-time consumer and consumer-group resolution live.
- [ ] Add snapshot lookup methods returning parsed values or `ErrNotFound`; callers must not receive mutable internal maps/slices.
- [ ] Fail snapshot creation with plugin-metadata bucket/id context on decode error. Retain legacy route/global-rule quarantine behavior.
- [ ] Make `(*Store).GetConfigSnapshot()` the owning API and keep the package-level wrapper for compatibility.
- [ ] Store the constructor's `*store.Store` in `Builder`. Replace direct BuildStrict-time package-global reads for plugin config, service, upstream, SSL, global rule, and metadata with snapshot methods.
- [ ] Inject the same snapshot-aware upstream resolver into traffic-split before `PostInit` so nested `upstream_id` references never fall back to the package-global Store.
- [ ] Write a regression using two Store instances: set the package global to conflicting data, pass the intended Store to Builder, and prove the built handler uses only the passed Store generation.
- [ ] Run:

```bash
bash -lc 'source .envrc && go test -race ./pkg/store ./pkg/route -run "(ConfigSnapshot|Builder.*Snapshot|PassedStore|Upstream.*SSL)" -count=1'
```

## Task 5: Acknowledge stream publication and wire runtime settings

**Depends on:** Tasks 1-4 accepted.

**Files:**
- Modify: `pkg/server/server.go`
- Modify: `pkg/server/reload.go`
- Test: `pkg/server/server_test.go`
- Test: `pkg/server/reload_test.go`
- Test: `pkg/server/stream_test.go`

- [ ] Add a failing test where a dynamic stream reload returns an error: last-good stream routing remains installed, readiness becomes false, and the acknowledged Store update returns that error.
- [ ] Add a success-after-failure test proving the next recovery apply retries stream publication and restores readiness.
- [ ] Register the Server through `AddAcknowledgedEventUpdateHook`. Queue HTTP reload as before; synchronously reload enabled streams and return errors to Store.
- [ ] Mark stream publication required before provider startup only when stream proxy mode is enabled. Record initial stream success/failure around `startStreamProxy`.
- [ ] Serialize initial stream publication, acknowledged reload, failed-start cleanup, and shutdown close under one runtime ownership lock.
- [ ] Change standalone dynamic stream reload callback to return/propagate errors instead of logging and acknowledging them.
- [ ] Pass configured watch timeout/resync delay into `etcd.ClientOptions` and call `FetchAllContext(ctx)` during startup.
- [ ] Keep provider, HTTP, and stream stages separately observable; a stream error must not replace the installed HTTP or stream last-good generation.
- [ ] Run:

```bash
bash -lc 'source .envrc && go test -race ./pkg/server -run "(Stream.*Reload|ConfigApply|InitialEtcd|Etcd.*Options|Standalone.*Stream)" -count=1'
```

## Task 6: Integrated verification and delivery

- [ ] Format touched Go files with the repository formatter and inspect that no unrelated file changed.
- [ ] Run the impact-scoped race gate:

```bash
bash -lc 'source .envrc && go test -race ./pkg/etcd ./pkg/store ./pkg/server ./pkg/route ./pkg/observability/metrics -count=1'
```

- [ ] Run scoped lint, build, and whitespace checks:

```bash
bash -lc 'source .envrc && golangci-lint run ./pkg/etcd/... ./pkg/store/... ./pkg/server/... ./pkg/route/... ./pkg/observability/metrics/...'
bash -lc 'source .envrc && make build'
git diff --check
```

- [ ] Inspect every moved/renamed/deleted symbol with `rg`; remove no compatibility API unless all production and test callers are migrated.
- [ ] Request one independent merge-level review on the frozen diff and remediate only confirmed findings.
- [ ] Commit exactly the plan, spec, source, and tests; push `codex/etcd-store-reliability`; open one ready PR against `master` without merging it.

## Plan self-review

- Every approved P0/P1 finding maps to a task and a failing regression test.
- Store, etcd, snapshot/route, metrics, and server workers have disjoint exclusive files within each parallel phase.
- Public compatibility wrappers are retained for single events, `FetchAll`, package-level snapshot access, and notification hooks.
- No placeholder functions, dependency additions, broad `go test ./...`, real-process integration cases, or benchmark claims are required.
