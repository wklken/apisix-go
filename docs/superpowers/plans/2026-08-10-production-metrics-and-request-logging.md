# Production Metrics and Request Logging Implementation Plan

> **Execution:** Follow `superpowers:executing-plans` in one isolated worktree. Do not use subagents for this plan.

**Goal:** Close production-readiness findings P1 5.8 and P1 5.15 by exporting only lifecycle-owned metrics and by enriching the existing Plan 17 detached log snapshot with bounded request/outcome correlation.

**Base:** Plan 09 owns proxy runtime metrics and config-apply readiness. Plan 17 already owns the single detached `LogSnapshot`, response capture, logger dispatch, tracer finalization, and request-context metrics finalizer. This plan extends those contracts; it must not introduce a second snapshot or phase executor.

## Frozen invariants

- A metric is registered only when a production lifecycle owner updates it.
- Connection states and retry results use finite labels; raw addresses, errors, route IDs, or etcd keys never become new lifecycle labels here.
- Etcd reachability reports transport/provider reachability, while the existing config-apply metric separately reports whether fetched configuration was accepted and published.
- Etcd revision advances only after the corresponding snapshot/watch batch has been successfully applied.
- Cluster metric series are deleted only after the final cluster with the same exported owner name retires.
- The existing immutable `pkg/apisix/log.LogSnapshot` remains the only logger snapshot. Request ID, node ID, upstream status, retry count, response source, and terminal outcome must be detached values on it or deterministically derived from it.
- Authentication/limit early stops, upstream retry success, recovered failures, and streaming terminal outcomes produce one final snapshot and one log callback.

## Task 1: Make core metrics truthful

- [ ] Add a bounded HTTP connection-state observer and install it through `http.Server.ConnState`; prove transitions and terminal cleanup without negative gauges.
- [ ] Replace the unused per-key etcd modify-index vector with one applied-revision gauge. Record reachability on initial fetch, watch delivery/error, recovery, and shutdown-safe cancellation; advance revision only after apply succeeds.
- [ ] Remove the unowned `upstream_status` metric family and its dead route FIXME. Keep the real Plan 09 `upstream_health` owner.
- [ ] Remove `reading`, `writing`, and `waiting` from node-status until a real HTTP parser/response state owner exists; preserve active/accepted/handled/total.
- [ ] Add a cluster-retirement callback. Delete in-flight/rejected/retry/health series only after the final registry entry with the same exported cluster name closes.

## Task 2: Complete one production log snapshot

- [ ] Extend the canonical detached request snapshot with request ID and node ID. Preserve safe cloning and body limits.
- [ ] Record final upstream status and actual retry count in bounded request variables owned by the proxy path.
- [ ] Extend `BuildAccessLogFromSnapshot` with request/node IDs, response source, terminal outcome, upstream status, and retry count while preserving the existing request/response/server, route/service/consumer, bytes, latency, and upstream address fields.
- [ ] Make the HTTP logger default detached payload use that canonical access-log builder. Custom formats and body-expression policy remain unchanged.
- [ ] Add focused lifecycle tests proving one correlated snapshot for an early rejection and an upstream retry; retain the existing Plan 17 stream/panic/cache-hit coverage rather than duplicating it.

## Task 3: Documentation, verification, and delivery

- [ ] Update the Prometheus and node-status support notes in `docs/plugins.md` to match the exported runtime behavior.
- [ ] Run impact-scoped package tests and focused race tests for metrics, etcd, proxy, server, node-status, snapshot/base, HTTP logger, and route integration.
- [ ] Run scoped lint, `make build`, `make clean`, and `git diff --check`.
- [ ] Perform an independent local review, repair confirmed findings with regression-first evidence, then commit, push, open a PR, wait for required CI, squash-merge to `master`, and verify the remote merge.

## Verification commands

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics ./pkg/etcd ./pkg/proxy ./pkg/server ./pkg/plugin/node_status ./pkg/apisix/log ./pkg/plugin/base ./pkg/plugin/http_logger ./pkg/route -count=1'
bash -lc 'source .envrc && go test -race ./pkg/observability/metrics ./pkg/etcd ./pkg/proxy ./pkg/server ./pkg/plugin/node_status ./pkg/plugin/http_logger ./pkg/route -run "(Connection|Etcd|Revision|Cluster|Snapshot|LogPhase|Retry|EarlyStop|NodeStatus)" -count=3'
bash -lc 'source .envrc && golangci-lint run ./pkg/observability/metrics/... ./pkg/etcd/... ./pkg/proxy/... ./pkg/server/... ./pkg/plugin/node_status/... ./pkg/apisix/log/... ./pkg/plugin/base/... ./pkg/plugin/http_logger/... ./pkg/route/...'
bash -lc 'source .envrc && make build'
bash -lc 'source .envrc && make clean'
git diff --check
```

**Commit:** `feat(observability): publish truthful request outcomes`
