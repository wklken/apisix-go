# Static Configuration Instructions

This file inherits the repository root `AGENTS.md` and applies to `pkg/config`.

## Contract

- Preserve precedence: builtins -> `conf/config-default.yaml` -> optional
  `-c/--config` file -> APISIX 3.17 reserved environment overrides.
- Expand APISIX file templates inside each file layer before merge. Maps merge
  recursively; sequences replace as a whole.
- Preserve the distinction among absent, null, false, zero, and empty string.
- The runtime has one APISIX-compatible behavior. Removed selectors must not
  reappear under another name or alter route, plugin, or rendering behavior.
- `EffectiveConfig` is immutable input to compiler construction. Do not add a
  process-global mutable config or Viper-backed production path.

## Change rules

- New fields need presence, merge, and validation coverage as applicable. Do not
  add generic environment or CLI override layers that APISIX 3.17 does not expose.
- Diagnostics must remain bounded and exclude endpoints, credentials, keys,
  certificates, tokens, and secret references.
- Static provider/listener shape and selection validation belongs
  here. Binding, stream protocol/listen conflict checks, and desired-state
  publication do not.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/config ./cmd -run "^(TestLoadEffective|TestConfig|TestRoot|TestStartup)" -count=1'
```
