# APISIX Compatibility Qualification Instructions

This file inherits the repository root `AGENTS.md` and applies to
`t/compatibility`.

## Scope

- This package is an opt-in qualification suite, not a unit-test or normal
  integration-test owner.
- `validation/compatibility/<target>/cases.yaml` owns executable differential
  cases. Do not add one Go builder or one Go test file per case.
- Every default exact-parity case must declare an independent semantic `expect`;
  a shared candidate/oracle no-op is not a pass. Protocol-aware cases may put
  the equivalent fail-closed assertion in a registered comparison policy.
- Keep Go code generic: catalog loading, selection, process orchestration,
  protocol drivers, normalization, comparison policies, and artifact binding.
- A behavior discovered here must move to the owning plugin unit tests or, when
  a real process/dependency is required, to `t/plugin/*.yaml` before the
  temporary investigation is closed.
- Preserve exact oracle source commit, image digest, catalog digest, and
  candidate binary identity. A differential artifact is append-only per
  attempt and never substitutes for durable regression coverage.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./t/compatibility -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && bash scripts/validation/plugin_differential_test.sh'
```
