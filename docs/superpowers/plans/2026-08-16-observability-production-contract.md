# Observability Production Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task in an isolated worktree. This plan does not authorize subagents.

**Goal:** Make every observability setting in the qualified profile truthful, prevent empty logger events, and bound HTTP status/latency/bandwidth metric cardinality for a long-running multi-replica deployment.

**Architecture:** Reject NGINX process access-log settings because the Go server has no process-level formatter/writer owner. Require an effective route-or-metadata `log_format` for Elasticsearch, ClickHouse, and Tencent CLS rather than emitting near-empty records. Put a concurrency-safe series budget in front of the three HTTP metric families: preserve bounded protocol labels, admit a fixed number of full label tuples, and aggregate excess dynamic tuples into a fixed overflow identity instead of allocating unbounded Prometheus children.

**Tech Stack:** Go 1.26, Prometheus client_golang, existing detached log snapshots and logger batch processors.

## Frozen contracts

- `nginx_config.http.enable_access_log`, HTTP access-log path/buffer/format/escape, and stream access-log settings are unsupported and fail configuration load when explicitly non-zero/non-empty. Error logging remains owned by the existing logger package.
- Elasticsearch, ClickHouse, and Tencent CLS require a non-empty effective `log_format` from route config or plugin metadata. Initialization fails before creating clients/processors when both are empty.
- Custom log formats remain explicit operator choices and may include sensitive fields; default credential-header redaction is unchanged.
- HTTP metric series limits apply separately to `http_status`, `http_latency`, and `bandwidth`; default 10,000 admitted full label tuples per family, configurable only from `plugin_attr.prometheus.max_http_series` in range 100–100,000.
- Once a family reaches its budget, unseen dynamic label tuples map to fixed `__overflow__` values. Existing series continue monotonically; do not delete/reset counters or histograms during route reload.
- `code`, latency/bandwidth `type`, and `response_source` stay bounded and are not replaced by overflow. Route/service/consumer/node/matched host/URI/request-model/extra labels are overflow candidates.
- Stream metrics and SkyWalking's multi-span parity remain excluded because `http-data-plane-v1` rejects stream and does not allowlist SkyWalking. Do not claim those registered capabilities are production qualified.

### Task 1: Reject parsed but unimplemented process access logs

**Files:**
- Modify: `pkg/config/init.go`
- Modify: `pkg/config/init_test.go`
- Modify: `docs/configuration.md`

- [ ] **Step 1: Add one row for every parsed access-log field**

Start from a valid compatibility config and independently set HTTP enable/path/buffer/format/escape and stream enable/path/format/escape. Each must fail with the exact `nginx_config.http.*` or `.stream.*` field. Zero values continue to load.

- [ ] **Step 2: Run the focused red test**

```bash
bash -lc 'source .envrc && go test ./pkg/config -run "ProcessAccessLog" -count=1'
```

Expected: every row is currently accepted.

- [ ] **Step 3: Add explicit validation**

Use a fixed field/value table; do not use reflection so future fields cannot silently enter the accepted set:

```go
if cfg.NginxConfig.HTTP.EnableAccessLog {
	return errors.New("nginx_config.http.enable_access_log is unsupported by the Go data plane")
}
if cfg.NginxConfig.HTTP.AccessLog != "" {
	return errors.New("nginx_config.http.access_log is unsupported by the Go data plane")
}
```

Apply the same rule to all listed HTTP/stream fields. Document route/plugin loggers as the supported logging mechanism.

### Task 2: Require meaningful logger formats

**Files:**
- Create: `pkg/plugin/base/required_log_format.go`
- Create: `pkg/plugin/base/required_log_format_test.go`
- Modify: `pkg/plugin/elasticsearch_logger/plugin.go`
- Modify: `pkg/plugin/elasticsearch_logger/plugin_test.go`
- Modify: `pkg/plugin/clickhouse_logger/plugin.go`
- Modify: `pkg/plugin/clickhouse_logger/plugin_test.go`
- Modify: `pkg/plugin/tencent_cloud_cls/plugin.go`
- Modify: `pkg/plugin/tencent_cloud_cls/plugin_test.go`
- Modify: `docs/plugins.md`

- [ ] **Step 1: Add route/metadata/empty precedence tests**

For each plugin: route format wins, metadata supplies the format when route is absent, and both empty return an initialization error containing the plugin name and `log_format`. Assert no client, shared lease, or batch processor exists after failure.

- [ ] **Step 2: Run focused red tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/elasticsearch_logger ./pkg/plugin/clickhouse_logger ./pkg/plugin/tencent_cloud_cls -run "EffectiveLogFormat" -count=1'
```

Expected: empty format initializes successfully and would emit an empty/near-empty map.

- [ ] **Step 3: Centralize format selection before side effects**

```go
func RequireStringLogFormat(
	pluginName string,
	route map[string]string,
	metadata map[string]string,
) (map[string]string, error) {
	if len(route) != 0 {
		return maps.Clone(route), nil
	}
	if len(metadata) != 0 {
		return maps.Clone(metadata), nil
	}
	return nil, fmt.Errorf("%s requires log_format in route config or plugin metadata", pluginName)
}
```

Call it immediately after loading metadata. Move existing client acquisition and batch-processor construction below this validation; do not change sink transport semantics.

### Task 3: Bound HTTP metric label tuples

**Files:**
- Create: `pkg/observability/metrics/http_series_budget.go`
- Create: `pkg/observability/metrics/http_series_budget_test.go`
- Modify: `pkg/observability/metrics/prometheus.go`
- Modify: `pkg/observability/metrics/prometheus_test.go`
- Modify: `docs/configuration.md`
- Modify: `docs/design.md`

- [ ] **Step 1: Add configuration and concurrency red tests**

Cover default/min/max parsing, invalid types/ranges failing config construction, exact limit admission, repeated tuple reuse, first unseen tuple after the limit mapping to overflow, distinct families having independent budgets, and 100 goroutines racing at the boundary without exceeding the cap.

- [ ] **Step 2: Define the private budget**

```go
type httpSeriesBudget struct {
	mu      sync.Mutex
	limit   int
	seen    map[string]struct{}
	dropped prometheus.Counter
}

func (b *httpSeriesBudget) Admit(labels []string, dynamic []int) []string
```

Build the map key with collision-safe length prefixes, not string concatenation. When unseen and full, clone labels and replace only indexes in `dynamic` with `__overflow__`; increment a no-dynamic-label counter for that family. The overflow tuple itself is not inserted into `seen` and does not consume additional budget.

- [ ] **Step 3: Parse and validate the budget once at server startup**

Add `MaxHTTPSeries int` to `prometheusMetricConfig`, default 10,000, and parse `plugin_attr.prometheus.max_http_series`. Change `newPrometheusMetricConfig` to return `(prometheusMetricConfig, error)`, `Init` to return `error`, and the `sync.Once` path to retain that initialization error. Update both `pkg/server/server.go` callers to return `fmt.Errorf("initialize prometheus metrics: %w", err)`. Unit tests that call `metrics.Init()` must assert or explicitly require no error. Values outside 100–100,000 fail server startup rather than silently defaulting.

- [ ] **Step 4: Apply budgets before every vector lookup**

Create one budget per family during metrics initialization. Normalize invalid status codes to `0`. Pass the final label slice through the family budget after appending extra labels and before `WithLabelValues`. Include every route/service/consumer/node/request-model/matched/extra-label index in the dynamic set; keep bounded control indexes unchanged.

Add metric `apisix_http_metric_series_overflow_total{metric}` where `metric` is validated against exactly `http_status`, `http_latency`, and `bandwidth`.

- [ ] **Step 5: Run focused normal/race tests**

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "(HTTPSeriesBudget|RecordHTTPRequest|PrometheusMetricConfig)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/observability/metrics -run "HTTPSeriesBudget" -count=3'
```

### Task 4: Record exclusions and verification evidence

**Files:**
- Modify: `docs/production-profile.md`
- Modify: `docs/plugins.md`
- Modify: `docs/production-readiness-remediation-2026-08-15.md`

- [ ] **Step 1: Make exclusion status explicit**

State that no stream metrics are required for `http-data-plane-v1` because stream activation is rejected. Keep the stream plugin table honest. State that SkyWalking and OTel configurations outside the validated OTel subset are not in the production allowlist; the config-honesty PR already rejects OTel `inactive_timeout`/`set_ngx_var` claims.

- [ ] **Step 2: Run impact-scoped gates**

```bash
bash -lc 'source .envrc && go test ./pkg/config ./pkg/plugin/base ./pkg/plugin/elasticsearch_logger ./pkg/plugin/clickhouse_logger ./pkg/plugin/tencent_cloud_cls ./pkg/observability/metrics -count=1'
bash -lc 'source .envrc && go test -race ./pkg/observability/metrics ./pkg/plugin/elasticsearch_logger ./pkg/plugin/clickhouse_logger ./pkg/plugin/tencent_cloud_cls -run "(HTTPSeriesBudget|EffectiveLogFormat|Reload|Stop)" -count=3'
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 3: Commit the observability PR**

```bash
git add pkg/config pkg/plugin/base/required_log_format.go pkg/plugin/base/required_log_format_test.go \
  pkg/plugin/elasticsearch_logger pkg/plugin/clickhouse_logger pkg/plugin/tencent_cloud_cls \
  pkg/observability/metrics docs/configuration.md docs/design.md docs/plugins.md \
  docs/production-profile.md docs/production-readiness-remediation-2026-08-15.md \
  docs/superpowers/plans/2026-08-16-observability-production-contract.md
git commit -m "fix(observability): enforce bounded production telemetry"
```
