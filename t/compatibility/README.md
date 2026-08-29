# APISIX Compatibility Qualification

This directory contains the generic, opt-in APISIX compatibility runner. It is
separate from plugin unit tests and the normal standalone integration suite in
`t/plugin`.

The APISIX 3.17 input is entirely versioned under
`validation/compatibility/apisix-3.17/`:

- `cases.yaml` contains case identity, obligation, standalone config, requests,
  fixtures, steps, plugin binding, and comparison-policy selection;
- `oracle.yaml` pins the APISIX source and immutable image identity;
- `normalization.yaml` defines the narrow platform-owned normalization rules.

Go code in this package is shared runner logic, protocol drivers, and
comparison policies. New ordinary cases belong in YAML, not in per-case Go
builders. A case must bind its declared plugin to the target route, or declare
the narrow non-route `plugin_binding` used to activate it. Exact-parity cases
must declare a semantic `expect` assertion so both sides cannot pass by sharing
the same no-op. If a differential run finds a product defect, add the regression
to the owning plugin unit tests or to `t/plugin` when it truly needs a real
process/dependency.

The suite is not part of `make test` or `make test-integration`. Run its local
logic tests with:

```bash
source .envrc
GOFLAGS=-mod=readonly scripts/go_cache.sh run -- go test ./t/compatibility -count=1
```

Run the pinned oracle only through `scripts/validation/plugin_differential.sh`
or the release behavior gate so source, image, catalog, and candidate identities
remain bound into the artifact.
