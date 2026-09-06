# Encrypted Field Catalog Instructions

This file inherits the repository root `AGENTS.md` and applies to
`pkg/capability`.

## Authority boundary

- `declarations.go` is the runtime source for plugin secret fields and the built-in SSL
  resource certificate/key fields.
- It is not a plugin inventory, behavior, evidence, readiness, divergence,
  platform, approval, or release-status ledger.
- Plugin constructors and execution placement belong to `pkg/plugin/registry.go`.

## Change rules

- Add a declaration only when production code materializes that exact
  factory, source, and field. SSL declarations use the dedicated SSL source and
  resource owner; they do not register a plugin.
- Preserve canonical wildcard-path, duplicate, overlap, lookup, and digest
  validation.
- Keep plugin behavior and regression evidence in plugin source and focused
  tests; keep unavoidable compatibility differences in concise ADRs.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/capability ./pkg/data_encryption ./pkg/secret -count=1'
```
