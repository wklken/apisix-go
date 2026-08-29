# GraphQL Proxy Cache Instructions

This file inherits `pkg/plugin/AGENTS.md`. Also follow the shared cache-zone,
storage, isolation, and lifecycle rules in
`pkg/plugin/proxy_cache/AGENTS.md`.

- Preserve GraphQL operation parsing and mutation bypass before caching.
- Keep GraphQL cache keys isolated by route, service, consumer, operation, and
  variables.
- Reuse the shared memory/disk zone contract without adding a second registry
  or cleanup owner.

Run both cache packages for focused verification:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache -count=1'
```
