# Rate Limit State Correctness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close P1 5.1 with atomic admission, bounded local state, stable recovery, and reusable Redis clients across limit-req, limit-conn, limit-count, and AI rate limiting.

**Architecture:** Give each limiter one reservation operation. Use bounded TTL maps locally and Lua transactions remotely. Acquire Redis clients during plugin initialization keyed by backend identity; requests never append release closures.

**Tech Stack:** Go 1.26 synchronization, bounded TTL map, go-redis Lua scripts, miniredis/fakes and race tests.

## Global Constraints

- Rejected limit-req requests do not update `last` or prolong punishment.
- Local state has both expiry and capacity; default maximum is 100,000 keys per plugin instance.
- limit-conn Redis uses unique request members in a ZSET, removes expired members before admission, and deletes its member during log/finalize.
- Check and charge for AI quotas is one atomic operation; response-token reconciliation is a second bounded delta operation.
- Dynamic limit-count configurations reuse clients/stores by backend identity and release them once at Stop.

### Task 1: Correct limit-req and limit-conn

- [x] Add fake-clock tests proving active buckets do not expire, rejects do not move `last`, and stale connection members are removed.
- [x] Change local admission to compute candidate state and commit only on allow; refresh expiry on accepted traffic.
- [x] Replace Redis limit-conn counter script with `ZREMRANGEBYSCORE` + `ZADD NX` + `PEXPIRE` atomic script.

### Task 2: Bound limit-count and AI state

- [x] Replace request-created Redis limiters with initialization/cache entries keyed by count/window/backend; cap dynamic local limiter and sliding-window maps.
- [x] Combine AI allowed/check increment into one locked/Lua reservation and add TTL/capacity eviction.
- [x] Add 100-goroutine admission tests asserting accepted count never exceeds quota and race detector is clean.

### Task 3: Verify and commit

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/limit_req ./pkg/plugin/limit_conn ./pkg/plugin/limit_count ./pkg/plugin/ai_rate_limiting -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/limit_req/... ./pkg/plugin/limit_conn/... ./pkg/plugin/limit_count/... ./pkg/plugin/ai_rate_limiting/...'
bash -lc 'source .envrc && make build'
```

- [ ] Commit with `git commit -m "fix(limits): bound and atomically update quota state"`.
