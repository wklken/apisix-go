# Runtime Metrics Instructions

This file inherits the repository root `AGENTS.md` and applies to
`pkg/observability/metrics`.

## Readiness

- Readiness is internal bounded state; never infer it by scraping Prometheus.
- `config_apply_ready` requires provider observed+healthy and successful HTTP
  stage, plus successful stream stage only when stream is configured.
  Quarantine is diagnostic only. A first acknowledgement containing only
  rejected dispositions does not establish stage readiness; later quarantine
  does not erase an already serviceable acknowledgement.
- The separate status listener exposes `/status` for process liveness and
  `/status/ready` for committed serviceable configuration. Provider
  reachability remains separate: last-good stays ready during an etcd outage.
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
