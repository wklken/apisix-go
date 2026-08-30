# Generation Contract Instructions

This file inherits the repository root `AGENTS.md` and applies to
`pkg/generation`.

## Authority boundary

- This package owns immutable values, canonical snapshot identity, structural
  validation, in-memory desired/published state, publication policy, and the
  coordinator. It must not import compiler, server, route, or plugins.
- Providers submit `DesiredBatch`; only `Coordinator` serializes desired state
  into runtime publication.

## Transaction invariants

- Preserve `apply desired in memory -> compile/prepare -> atomic active-bundle
  swap -> commit coordinator state -> return acknowledgement to provider`.
- Publication failure leaves coordinator state and the active bundle unchanged.
- Exact same-cursor replay returns the stored in-memory acknowledgement without
  applying or compiling again.
- `last-good` requires exact same-domain predecessor bytes. First generation
  without a predecessor fails closed; tombstone always means delete.
- Preserve canonical sorting/digests, defensive copies, nil-versus-empty bytes,
  normalized required domains, and one exact decision per resource key.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/generation -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/generation -count=1'
```
