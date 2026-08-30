# Plugin Registry Manifest Instructions

This file inherits the repository root `AGENTS.md` and applies to
`pkg/capability`.

## Authority boundary

- `manifest.yaml` is a temporary runtime source for the pinned APISIX 3.17
  target, factories/aliases, phases/priorities/scopes, and secret declarations.
- It is not a behavior, evidence, readiness, divergence, platform, approval, or
  release-status ledger.
- `pkg/plugin/registry_gen.go` is generated from manifest factory facts. Never
  hand-edit it to repair drift.
- Secret source/field/target remains runtime input until the secret resolver is
  aligned with APISIX 3.17 in a later simplification phase.

## Change rules

- Keep strict YAML `KnownFields` parsing and duplicate/factory/catalog
  validation fail closed.
- Add or rename a plugin or alias through the manifest factory, then regenerate
  the plugin registry in the same change.
- Keep plugin behavior and regression evidence in plugin source and focused
  tests. Keep unavoidable compatibility differences in concise ADRs.
- Do not add behavior status, evidence claims, known-gap ledgers, divergence
  approval state, supported-platform claims, or candidate identities here.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/capability ./cmd/capability-gen -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root . -check'
```
