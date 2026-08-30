# Durable Journal Instructions

This file inherits the repository root `AGENTS.md` and applies to `pkg/store`.

## Authority boundary

- Store is the bbolt implementation of the generation journal. Do not restore
  runtime getters, secret lookup, route reload, plugin storage, Event/Builder,
  or other mutable serving responsibilities.
- Persisted artifacts, cursors, decisions, stages, and published heads retain
  complete identity and integrity metadata.

## Transaction and recovery rules

- Only the exact current schema is accepted. Older, future, partial, and
  unrecognized formats fail closed without mutation; do not add migrations.
- `Commit` atomically updates all required domain heads, revision/decision
  state, acknowledgement, and stage removal.
- Recovery validates global desired integrity, clears abandoned stages, and
  isolates a damaged published domain without treating desired as published.
- Preserve nil/empty wire distinction, exact cursor replay, defensive copies,
  and content-addressed artifact identity.
- The journal path is `<EffectiveConfig.Paths.DataDir>/apisix-go-store.db`; do
  not silently fall back to the working directory.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/store -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/store -count=1'
```
