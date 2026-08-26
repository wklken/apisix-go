# Generation Contract Instructions

This file inherits the repository root `AGENTS.md` and applies to
`pkg/generation`.

## Authority boundary

- This package owns immutable values, canonical snapshot identity, structural
  validation, journal interfaces, publication policy, and the coordinator. It
  must not import Store implementations, compiler, server, route, or plugins.
- Providers submit `DesiredBatch`; only `Coordinator` serializes desired state
  into runtime publication.

## Transaction invariants

- Preserve `ApplyDesired -> LoadDesired -> Prepare -> Stage -> Activate ->
  Commit (persist acknowledgement) -> FinalizeActivation -> return
  acknowledgement to provider`.
- Stage failure discards the prepared generation. Activation/commit failure
  restores the exact predecessor before abort. Commit precedes retirement.
- Committed replay confirms the active fence and must not apply or compile
  again. Cleanup after failure must not inherit caller cancellation.
- Recovery serves only committed published state. Desired is never serving
  input; an empty journal does not synthesize a generation.
- `last-good` requires exact same-domain predecessor bytes. First generation
  without a predecessor fails closed; tombstone always means delete.
- Preserve canonical sorting/digests, defensive copies, nil-versus-empty bytes,
  normalized required domains, and one exact decision per resource key.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/generation -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/generation -count=1'
```
