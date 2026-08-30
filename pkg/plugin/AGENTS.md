# Plugin Runtime Instructions

This file inherits the repository root `AGENTS.md` and applies to the complete
`pkg/plugin` tree. A closer nested file may add stricter rules.

## Registration and dependencies

- Plugin factories, aliases, phases, priorities, evidence, and secret fields
  come from `pkg/capability/manifest.yaml`. Do not hand-edit generated registry
  or plugin-status projections.
- `base.Dependencies` is immutable compiler injection. Background plugins
  receive `*runtime.TaskOwner`, provide only a fixed component, and must not
  build `TaskSpec`, choose criticality/prefix, or regain a raw TaskRegistry
  accessor.
- Start generation background work during `PostInit` admission. On failure,
  roll back every client, lease, observer, or earlier admission. A plugin may
  privately quiesce admission, but registry stop owns the task join.
- Do not introduce raw goroutines or `sync.WaitGroup.Go`. Request children use
  `RequestTaskGroup` and join before request/connection return.

## Panic, secrets, and cleanup

- Guard plugin callbacks with bounded factory/phase attribution. Preserve exact
  `http.ErrAbortHandler`; never relabel downstream/core panic as plugin failure.
- Do not place raw panic values, route/resource IDs, plaintext, ciphertext, or
  secret references in errors, metrics, status, or task owner names.
- Secret access must use manifest-declared, generation-scoped dependencies before
  `PostInit`; never read a global Store or resolve an undeclared field.
- Residual/deadline means plugin authority is still live. Do not free a sink,
  client, sender, lease, or observer until admitted callbacks actually exit.
- `file-logger` is distinct from generic logger batch: one `file-log-writer`
  task, entry+byte bounds, canonical-path writer leases, and a process-local
  `core/file-writer-registry/signal-watch` epoch.

## Focused verification

Always run the package for each modified plugin, for example
`scripts/go_cache.sh run -- go test ./pkg/plugin/<name> -count=1`. The root package command below covers
only shared executor/panic contracts; it does not cover plugin subpackages.

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/plugin -run "^(TestGuard|TestStreamingFinish|TestLogComposite|TestPlugin)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/runtime -run "^TestProductionGoroutinesUseOwnedRuntime$" -count=1'
```
