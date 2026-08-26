# Capability Manifest Instructions

This file inherits the repository root `AGENTS.md` and applies to
`pkg/capability`.

## Authority boundary

- `manifest.yaml` is the only editable capability source: pinned APISIX target,
  factories/aliases, phases/priorities/scopes, behavior, evidence,
  qualification, platforms, gaps/divergences, and secret declarations.
- `pkg/plugin/registry_gen.go`, `docs/plugins.md`, and generated README summaries
  are projections. Never hand-edit them to repair drift.
- Behavior support, evidence state, and qualification are independent. Do not
  infer one from another or from a registered factory count.
- Accepted divergence requires a manifest entry and its controlled ADR. Secret
  source/field/strictness/target is runtime authority, not descriptive metadata.

## Change rules

- Keep strict YAML `KnownFields` parsing and duplicate/profile/evidence/catalog
  validation fail closed.
- Add/rename a plugin or alias through the manifest factory, then regenerate all
  projections in the same change.
- Never weaken a missing-evidence qualification failure into an inventory pass.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/capability ./cmd/capability-gen -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root . -check'
```
