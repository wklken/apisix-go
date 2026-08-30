# Detached HTTP Route Instructions

This file inherits the repository root `AGENTS.md` and applies to `pkg/route`.

## Boundary

- This package compiles/assembles detached HTTP snapshots. It must not read
  provider state, instantiate plugin factories, resolve
  secrets, acquire shared resources, start background tasks, or activate a
  generation.
- Clone externally supplied route/service/upstream/plugin data before retaining
  it in a snapshot.
- Planning selects the exact final occurrence before compiler materialization.
  Disabled or losing occurrences must have no side effects.
- Preserve stable priority ordering and the established equal-priority winner
  semantics. Route-scoped failures quarantine the complete route atomically.

## Request lifecycle

- Do not introduce raw goroutines or `sync.WaitGroup.Go`; use the owned runtime
  APIs and join every accepted child before the handler returns.
- Preserve plugin/core panic attribution and the exact abort sentinel. Run every
  finalizer; one panic must not skip remaining cleanup.
- Response/body capture, commit/flush/hijack state, vars reuse, and generation
  lease release are one lifecycle contract. Do not release any of them early.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/route -run "^(TestCompileHTTP|TestPlanHTTPPlugins|TestPreparedGeneration|TestBufferedRouteFinalizer)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/route -count=1'
```
