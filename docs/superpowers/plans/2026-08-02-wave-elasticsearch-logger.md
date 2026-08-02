# Child Plan: elasticsearch-logger Missing Corpus Sources

> Owner row of `docs/superpowers/plans/2026-08-02-full-test-nginx-corpus-coverage.md` Task 4.
> Source: `t/plugin/elasticsearch-logger2.t` (10 blocks).

## Source Contract Extraction

Upstream harness: `log_level('debug')`, request default `GET /t`. Test-only `extra_init_by_lua` hook wraps `http.request_uri` to log `uri:` / `body:` — observability reproduced in Go via the ES fixture assertion instead.

| TEST | Title | Contract |
|---|---|---|
| 1 | should drop entries when max_pending_entries is exceeded | Plugin metadata `log_format {host=$host, @timestamp=$time_iso8601, client_ip=$remote_addr}`, `max_pending_entries=1`. Route /hello with `elasticsearch-logger {endpoint_addr http://127.0.0.1:1234, field.index=services, batch_max_size=1, timeout=1, max_retry_count=10}`. Three GET requests then sleep 2. Error log `max pending entries limit exceeded. discarding entry`. |
| 2 | set route with header auth | Route /hello with `endpoint_addr http://127.0.0.1:9201`, `field.index=services`, `headers.Authorization=Basic ZWxhc3RpYzoxMjM0NTY=`, `batch_max_size=1`, `inactive_timeout=1`. Response `passed`. |
| 3 | test route (auth success) | GET /hello; wait 2; response body `hello world`; log `Batch Processor[elasticsearch-logger] successfully processed the entries`. |
| 4 | resolve_index_vars unit test | Lua unit test of internal `_resolve_index_vars` with `%Y/%m/%d/%Y.%m.%d` formats. **Lua-internal; not a real-process surface.** |
| 5 | test date variable in index | Route with `field.index=services-{%Y.%m.%d}`, `auth{elastic/123456}`, `batch_max_size=1`, `inactive_timeout=1`. Request; ES payload index must match `services-\d{4}\.\d{2}\.\d{2}`. |
| 6 | test APISIX variable in index | Same route with `field.index=services-$host`. ES payload index must equal `services-127.0.0.1`. |
| 7 | test both APISIX variable and date variable in index | `field.index=services-$host-{%Y.%m.%d}`. Index must match `services-127.0.0.1-\d{4}\.\d{2}\.\d{2}`. |
| 8 | dynamic index template should not be mutated across requests | `field.index=services-$arg_id-{%Y.%m.%d}`. Two requests with `?id=first` and `?id=second`; two ES payloads with `services-first-...` and `services-second-...`. Response `done`. |
| 9 | ${xx} variable syntax should not trigger time replacement | `field.index=services-${arg_id}-{%Y.%m.%d}`; request `?id=myservice`; index `services-myservice-\d{4}...`; `no_error_log: failed to parse time format`. |
| 10 | non-string time format (os.date "*t" returns a table) falls back | Lua unit test of `_resolve_index_vars` for `*t`/`!*t` formats; expects fallback to `prefixsuffix` and error log `failed to parse time format`. **Lua-internal; not a real-process surface.** |

## Disposition Plan

- `converted` after evidence: tests 1, 2, 3, 5, 6, 7, 8, 9 (8 blocks).
- `blocked_runtime` (native Lua unit tests, no real-process boundary): tests 4, 10.
  Go behavior note: `replaceIndexTimeVars` uses `strftimeToGo`; `*t`/`!*t` are not Go layouts, so the fallback in Go is a string echo rather than Lua's table rejection — a native semantic, not convertible without changing behavior.

## Steps

1. Add cases to `t/plugin/elasticsearch-logger.yaml`:
   - `max-pending-entries-drop` (test [1]) — metadata with `max_pending_entries: 1`; three requests; ES fixture with `expect_requests: 0` (or 1) proving the drop; assert via startup log `max pending entries limit exceeded` if the Go plugin logs it, else fixture count.
   - `header-auth-sent` (tests [2, 3]) — ES fixture `expect` header `Authorization: Basic ZWxhc3RpYzoxMjM0NTY=`; request returns `hello world`.
   - `date-variable-index` (test [5])
   - `apisix-variable-index` (test [6])
   - `combined-variable-index` (test [7])
   - `template-not-mutated-across-requests` (test [8]) — two steps with different `?id=` values, fixture asserts both indexes.
   - `dollar-brace-variable-no-time-replacement` (test [9])
2. ES fixture kind: check existing `elasticsearch-logger.yaml` fixture shape (likely `http` kind with `expect` on path/body) and reuse; the ES bulk payload contains `{"index":{"_index":"..."}}`.
3. Run focused integration RED:
   ```bash
   source .envrc
   go test ./t/plugin -run 'TestPluginIntegration/elasticsearch-logger/(max-pending-entries-drop|header-auth-sent|date-variable-index|apisix-variable-index|combined-variable-index|template-not-mutated-across-requests|dollar-brace-variable-no-time-replacement)$' -count=1 -v
   ```
4. Focused package RED only for confirmed defects; fix minimally.
5. Run `go test ./pkg/plugin/elasticsearch_logger -count=1` + integration GREEN.
6. Update ledger: tests 1-3, 5-9 `converted`; tests 4, 10 `blocked_runtime` with reason; record evidence.
