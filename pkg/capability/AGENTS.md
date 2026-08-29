# Capability Manifest Instructions

This file inherits the repository root `AGENTS.md` and applies to
`pkg/capability`.

## Authority boundary

- `manifest.yaml` is the only editable capability source: pinned APISIX target,
  factories/aliases, phases/priorities/scopes, behavior, evidence,
  validation evidence, platforms, gaps/divergences, and secret declarations.
- `pkg/plugin/registry_gen.go`, `docs/plugins.md`, and generated README summaries
  are projections. Never hand-edit them to repair drift.
- Behavior support and evidence state are independent. Do not infer production
  readiness from a registered factory count.
- Accepted divergence requires a manifest entry and its controlled ADR. Secret
  source/field/strictness/target is runtime authority, not descriptive metadata.

## Change rules

- Keep strict YAML `KnownFields` parsing and duplicate/evidence/catalog
  validation fail closed.
- Add/rename a plugin or alias through the manifest factory, then regenerate all
  projections in the same change.
- Never weaken a missing-evidence validation failure into an inventory pass.
- Upstream source-block accounting belongs to `t/plugin/corpus_scope.yaml`,
  executable converted behavior to `t/plugin/*.yaml`, and differential
  obligations to `validation/differential-cases.yaml`. Do not create a second
  profile-specific corpus or hand-maintained status ledger.
- A qualification result is current only when its source commit, oracle
  identity, manifest/catalog digests, and exact candidate binary or image
  identity agree and the required first-attempt evidence passes. A historical
  report, older binary result, plan checkbox, or inventory total cannot promote
  capability evidence.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/capability ./cmd/capability-gen -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root . -check'
```
