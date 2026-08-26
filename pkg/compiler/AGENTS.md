# Immutable Compiler Instructions

This file inherits the repository root `AGENTS.md` and applies to `pkg/compiler`.

## Planning and authority

- Pure normalize/validate/dependency/domain/disposition planning completes and
  validates the final publication set before any registration, secret, client,
  resource, task, or plugin side effect.
- Plan exact winning occurrences first. Disabled/losing occurrences must not be
  materialized or acquire resources.
- Candidate and recovery compilation use the same ownership-transfer path.
- Inject immutable dependencies and the exact plugin `TaskOwner` before
  `PostInit` or any other plugin side effect.
- Do not reintroduce Store reads, mutable route builders, or a second production
  compilation path.

## Prepared generation and cleanup

- `PreparedGeneration` completely owns one attempt: publication, metadata,
  consumers, secret authority, tasks, resources, HTTP/TLS and stream snapshots,
  and cleanup ledger. Do not export lifecycle-capable handles.
- Register cleanup immediately after acquisition. Preserve reverse-order phases:
  quiesce -> resource finalize -> authority release.
- Residual, cancellation, deadline, or incomplete finalization retains the
  generation and retries from the pending phase. Only terminal cleanup revokes
  views and detaches the live generation.
- Plugin owner prefixes are compiler-derived from the full canonical instance
  key: `plugin/<sanitized-factory>/<sha256(instance-key)>`.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/compiler -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/compiler -run "^(TestCleanupStack|TestPreparedGeneration|TestWorkerCompilerFactory|TestEffectiveBindingMaterializer)" -count=1'
```
