# Proxy Cache Instructions

This file inherits `pkg/plugin/AGENTS.md` and applies to `proxy-cache` and
the shared cache-zone implementation used by `graphql-proxy-cache`.

## Configuration and ownership

- Cache zones come from the immutable effective configuration injected into
  each generation. Validate duplicate names, sizes, absolute disk paths,
  `cache_levels`, unknown zones, and strategy mismatches before use.
- Plugin construction must inject the generation-local zone snapshot before
  `PostInit`; do not add a process-global configuration fallback.
- Memory and disk zones are shared by declared zone identity and released by
  generation/plugin lifecycle. Background cleanup must use the injected task
  owner and must be joined during `Stop`.

## Stored behavior

- Keep memory capacity and disk size bounded. Disk writes are atomic; corrupt
  or expired entries are misses and are removed.
- Preserve route, service, consumer, cache-key, and `Vary` isolation across
  `proxy-cache` and `graphql-proxy-cache`.
- Never return an expired body merely because an upstream request failed.
  `PURGE`, `only-if-cached`, response cache controls, and `Set-Cookie`
  handling must remain explicit and tested.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache -count=1'
```
