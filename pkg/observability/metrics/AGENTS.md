# Runtime Metrics Instructions

This file inherits the repository root `AGENTS.md` and applies to
`pkg/observability/metrics`.

## Readiness

- Readiness is internal bounded state; never infer it by scraping Prometheus.
- `config_apply_ready` requires provider observed+healthy and successful HTTP
  stage, plus successful stream stage only when stream is configured. Any
  quarantine blocks readiness.
- `/livez` is independent. `/readyz` also requires provider reachability in etcd
  mode; a recovered generation may keep serving while readiness is false.
- A failed provider apply attempt increments failure evidence but must not erase
  the last acknowledged publication state.

## Cardinality and generation overlap

- Never use untrusted provider, stage, route, resource, owner, panic, or backend
  values directly as labels. Use bounded enums and canonical overflow labels.
- Dynamic HTTP/LLM/upstream series use hard-cap trackers and `__overflow__`.
  Eviction is bounded; in-flight series cannot be deleted before release.
- Stream route metric references survive until the final overlapping generation
  that can emit a terminal observation retires.
- Keep observer interfaces in dependency-free leaf packages where required to
  avoid proxy/plugin import cycles.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/observability/metrics -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/observability/metrics -count=1'
```
