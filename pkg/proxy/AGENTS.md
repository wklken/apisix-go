# Proxy Resource Instructions

This file inherits the repository root `AGENTS.md` and applies to `pkg/proxy`.

## Cluster identity and ownership

- `ClusterConfig.Key()` is a stable SHA-256 identity of the complete effective
  cluster config. Preserve deterministic target/priority ordering and include
  TLS, timeout, retry, connection cap, checks, name, and H2C.
- Compiler-owned clusters are shared through `runtime.ResourceRegistry` by full
  digest. Each generation owns a lease; active-health work belongs to a
  resource-local registry, not the first generation that acquired it.
- The exact active-health owner is
  `core/proxy-cluster/<full-config-digest>/active-health`. Standalone
  `NewCluster` must establish the same ownership contract.
- Use the dependency-free `pkg/proxy/observer` leaf interfaces to avoid
  proxy/metrics import cycles.

## Teardown and admission

- Do not introduce raw goroutines or `sync.WaitGroup.Go`.
- `CloseContext` stops and joins active health before releasing observers,
  health transports, or idle connections. A residual retains all authority for
  a later retry.
- Admission lifetime extends through response-body close/EOF and upgraded
  duplex lifetime, not merely through receipt of response headers.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/proxy -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/proxy -run "^(TestOwnedCluster|TestClusterCloseContext|TestNewClusterCloseContext|TestActiveHealth|TestClusterUpgrade)" -count=1'
```
