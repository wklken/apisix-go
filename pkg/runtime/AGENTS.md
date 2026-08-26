# Runtime Ownership Instructions

This file inherits the repository root `AGENTS.md` and applies to `pkg/runtime`.

## Task contract

- Keep this package generic; do not add plugin, Store, compiler, or business
  policy.
- Generation/shared work uses `TaskOwner.Go(component, callback)`. Request or
  connection child work uses `RequestTaskGroup.Go` and `Wait`. Do not add a
  second task group, public panic channel, status API, or plugin-built `TaskSpec`.
- Owner names and criticality are stable diagnostics. `TaskPlugin` failure
  poisons only its exact owner; `TaskCore` error reports without poisoning;
  `TaskCore` panic remains unrecovered.
- `RequestTaskGroup.Wait` joins every admitted child before re-panicking the
  first panic with the same identity. Panic takes precedence over joined errors.
- `TaskRegistry.Stop` rejects admission, cancels once, joins accepted work, and
  returns sorted/deduplicated residual owners. Residual/deadline is retryable,
  not terminal close.

## Resource contract

- Shared identity is the full `ResourceKey{Kind, Scope, Digest}` plus Go type.
- Final-reference release and registry close are retryable. Incomplete close
  retains the resource and identity in the closing set.
- The raw-goroutine gate covers production files under `pkg/plugin`,
  `pkg/proxy`, `pkg/route`, and `pkg/stream` only. Keep this scope explicit.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/runtime -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/runtime -count=1'
```
