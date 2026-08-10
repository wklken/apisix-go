# Logger Batch Resource Bounds Implementation Plan

> **Execution owner:** `$fast-plan-impl`. Implement with at most three bounded workers, exclusive file ownership, regression-first evidence, and an independent read-only review before delivery.

**Goal:** Close production-readiness item PR-012 by making every shared logger batch processor bounded by default: accepted entries have an exact capacity limit, flushes execute on a fixed worker set, delivery and retry waits are cancellable, shutdown is time-bounded and idempotent, and capacity/delivery/shutdown outcomes are observable.

**Architecture:** Replace one-goroutine-per-flush with a mutex-protected ready-batch deque consumed by fixed workers. `pending` remains the single entry-capacity source of truth and covers buffered, ready, active, and retrying entries. Introduce a context-aware delivery constructor for production sinks while retaining the old constructor as a compatibility adapter. Keep the repository-wide `Stop()` lifecycle contract unchanged; add an explicit processor shutdown API for error-aware callers and tests. Metrics are owned by a lifecycle-aware observer so route-generation overlap cannot erase a live series or leak retired gauges.

**Tech stack:** Go 1.26, `context`, `sync`, existing Resty/Elasticsearch/Kafka/RocketMQ/socket clients, Prometheus client, focused race tests.

## Scope and decisions

### Confirmed defects

- `MaxPendingEntries=0` currently means unlimited in the shared processor. Several production logger paths therefore have no default pending bound.
- Capacity is checked with `pending > max`, so a configured maximum of one accepts two entries.
- Every flush starts a new goroutine. A slow sink can create an unbounded number of goroutines waiting on delivery or a shared transport lock.
- Retry delay uses `time.Sleep`, delivery has no parent context, and `Stop` waits forever.
- The existing `batch_process_entries` gauge tracks only the in-memory buffer. It becomes zero immediately after flush even while entries remain queued, active, or retrying.
- Existing dynamic metric labels are not lifecycle-cleaned. Retired and current route generations can overlap, and a retired processor must not delete the current processor's series.

### Fixed defaults and validation

- `MaxPendingEntries = 10_000` when unset or non-positive.
- `MaxConcurrentDeliveries = 1` when unset or non-positive.
- Internal concurrency overrides are clamped to the safe range `1..8`. Plan 09 does not add a new route/schema field to every logger plugin.
- `DeliveryTimeout = 10s` when unset or non-positive.
- `ShutdownTimeout = 15s` when unset or non-positive.
- Existing `BatchMaxSize`, retry count/delay, buffer duration, inactive timeout, and one-based `firstFail` semantics remain compatible.
- `Push` remains non-blocking with respect to delivery. It accepts only while running and `pending < MaxPendingEntries`; at `pending >= max` it rejects the new entry without changing pending.
- `pending` includes buffered, ready, active, and retrying entries. It decreases exactly once for a delivered or terminally dropped entry and never underflows.

### Queue and timer contract

- Do not send batches to a possibly full channel while holding the processor mutex.
- Seal a buffer into a mutex-protected ready deque and signal a fixed worker set. The hard entry bound makes the deque bounded without a second ambiguous capacity policy.
- A worker removes one ready batch, performs delivery outside the mutex, and preserves the existing one-based partial-success contract: on `err != nil` and `2 <= firstFail <= len(entries)`, the successful prefix is terminal and only the suffix is retried.
- Each attempt derives `context.WithTimeout(processorDeliveryContext, DeliveryTimeout)`.
- Retry delay uses a timer/select and exits on processor cancellation.
- Timer scheduling uses the earlier of `firstEntry + BufferDuration` and `lastEntry + InactiveTimeout`; each accepted push reschedules. A generation token prevents an older callback from clearing or replacing the current timer.

### Shutdown compatibility contract

- Keep `Processor.Stop()` with the existing `func()` shape so `t.Cleanup`, `pluginStopper`, `Builder.Stop`, route generations, and unrelated plugins remain compatible.
- Add `Processor.Shutdown(context.Context) error`. It starts shutdown once, rejects later pushes, seals the current buffer, stops the timer, and waits for workers.
- `Stop()` calls `Shutdown` with the configured `ShutdownTimeout`. Timeout/cancellation is logged and recorded; it does not change the route stopper interface.
- On shutdown deadline, cancel delivery attempts, mark undispatched entries terminally dropped exactly once, and wake workers. `Stop()` may then return to preserve its bound, but sink cleanup is attached through `StopWithCleanup` and remains behind a separate worker barrier; clients/connections are not released until every active callback has actually returned.
- Concurrent/repeated `Stop` and `Shutdown` calls are safe. They never close a channel twice, never enqueue after stopping, and observe the same terminal processor state.

### Delivery API compatibility

Use two explicit function types:

```go
type DeliveryFunc func([]map[string]any, int) (firstFail int, err error)
type ContextDeliveryFunc func(context.Context, []map[string]any, int) (firstFail int, err error)

func New(Config, DeliveryFunc) *Processor
func NewWithContext(Config, ContextDeliveryFunc) *Processor
```

- `New` adapts the legacy callback and remains available for compatibility and focused legacy tests.
- `base.NewBatchProcessor` accepts `ContextDeliveryFunc` and calls `NewWithContext`.
- All 18 base-backed production loggers and direct Zipkin construction migrate to the context-aware path.
- Resty requests call `SetContext(ctx)`; Elasticsearch bulk uses `WithContext(ctx)`; standard HTTP uses `NewRequestWithContext`.
- Kafka, RocketMQ, and error-log delivery derive their protocol operation from the supplied context instead of `context.Background()`.
- Socket delivery uses `DialContext` where the transport owns dialing and uses the earlier of the configured I/O deadline and the context deadline. A cancellation watcher may close a retained/blocking connection only when needed to make the transport honor cancellation; it must terminate after the call.
- Google Cloud token acquisition and the subsequent POST both use the same delivery context.

### Metrics contract

- Preserve `apisix_batch_process_entries{name,route_id,server_addr}` for APISIX compatibility and keep its meaning as buffered entries.
- Add `apisix_logger_batch_pending_entries{plugin,route_id,server_addr}` for all accepted nonterminal entries. `plugin` is a stable plugin identifier supplied separately from configurable batch display name.
- Add `apisix_logger_batch_events_total{plugin,outcome}`. Validate both labels before creating a series. Allowed plugin identifiers are exactly the migrated production owners; allowed outcomes are:
  - `capacity_dropped`
  - `stopped_dropped`
  - `delivery_failed`
  - `delivery_timeout`
  - `shutdown_timeout`
- Do not add route ID, server address, configured batch name, request fields, or log payload fields to the counter.
- `metrics.AcquireLoggerBatchObserver(plugin, batchName, routeID, serverAddr)` returns a nil-safe observer with `SetBuffered`, `AddPending`, `AddEvent`, and `Close`.
- Observers aggregate gauge deltas by label set and refcount overlapping route generations. Closing an old generation cannot erase a live generation; the last close removes dynamic gauge series. Counters remain process-lifetime totals.
- Plan 21 remains the owner of broader production logging/request snapshot correlation. Plan 09 does not add payload or request labels.

### Explicit deferrals

- Do not change the repository-wide `pluginStopper`, `Builder.Stop`, route-handler stopper, or server shutdown signature.
- Do not add `max_concurrent_deliveries`, `delivery_timeout`, or `shutdown_timeout` to 18 independent plugin schemas. The safe defaults apply centrally; public tuning requires a separately reviewed compatibility surface.
- Do not normalize APISIX route-vs-metadata placement of `max_pending_entries`, configurable batch names, or unrelated logger parity differences.
- Do not change logger payload encoding, batching boundaries, partial-success indexing, retry counts, or backend-specific response parsing.

## Work-unit graph

```text
WU-01 core + metrics contract
        |
        +-------------------+
        |                   |
WU-02 HTTP/report sinks   WU-03 socket/message sinks
        |                   |
        +---------+---------+
                  |
       combined verification + review + PR
```

WU-01 lands the fixed APIs first. WU-02 and WU-03 then run in parallel in the same isolated worktree with disjoint package ownership.

## WU-01: Processor, base defaults, and metrics ownership

**Exclusive production ownership:**

- `pkg/plugin/logger_batch/processor.go`
- `pkg/plugin/base/types.go`
- `pkg/observability/metrics/logger_batch.go` (new)
- `pkg/observability/metrics/prometheus.go`

**Exclusive test ownership:**

- `pkg/plugin/logger_batch/processor_test.go`
- `pkg/plugin/base/logging_test.go`
- `pkg/observability/metrics/logger_batch_test.go` (new)
- `pkg/observability/metrics/prometheus_test.go`
- `pkg/server/route_handler_test.go` only for legacy `logger_batch.New` compile compatibility if required

### Regression-first tests

- Add `TestProcessorDefaultsResourceBounds` covering 10,000 pending, one worker, 10-second delivery, and 15-second shutdown defaults through observable behavior or package-private state.
- Replace the historical max+1 test with `TestProcessorRejectsAtExactPendingBoundary`: max=1 accepts the first entry and rejects the second while the first is active.
- Add accounting tests for buffered, ready, active, partial-success retry suffix, invalid `firstFail`, terminal delivery failure, and no underflow/double decrement after cancellation.
- Add default and configured concurrency tests using a blocking context-aware callback; assert active calls never exceed one by default and never exceed the configured value.
- Add `TestProcessorPushDoesNotWaitForWorkerCapacity`: a stalled worker and ready backlog do not block Push; only the exact pending limit rejects.
- Add timer tests for inactivity reschedule, hard buffer-duration ceiling, and stale callback generation isolation.
- Add delivery-timeout and cancellable retry-delay tests.
- Add shutdown tests: buffered flush, reject-after-start, bounded timeout, active cancellation, queued drop accounting, idempotent/concurrent calls, and late callback protection.
- Add base tests for centralized defaults and stable plugin ID forwarding. Preserve the legacy constructor test.
- Add metrics tests for pre-init no-op, label validation, pending aggregation across overlapping observers, old-generation close safety, last-owner series deletion, and persistent bounded counters.

Run the named tests before production edits and record the expected compile/behavior failures:

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/logger_batch -run "^TestProcessor(DefaultsResourceBounds|RejectsAtExactPendingBoundary|BoundsConcurrentDeliveries|PushDoesNotWaitForWorkerCapacity|ReschedulesTimer|HonorsBufferDuration|CancelsDelivery|Shutdown)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/base ./pkg/observability/metrics -run "(LoggerBatch|BatchProcess|ApplyBatchDefaults|NewBatchProcessor)" -count=1'
```

### Implementation

- Implement the context-aware function type/constructor while retaining the legacy constructor.
- Normalize the four resource defaults centrally.
- Implement the fixed worker/deque state machine and generation-safe timer.
- Implement exact pending/processing/delivery/drop accounting under the processor mutex.
- Implement `Shutdown(context.Context) error` plus compatible `Stop()`.
- Acquire one metrics observer per processor and close it only after processor termination or abort bookkeeping is complete.
- Extend `BatchDefaults` with stable `PluginID` and internal resource overrides. `NewBatchProcessor` applies defaults and uses `NewWithContext`.
- Keep `BaseLoggerPlugin.Stop()` and route stopper interfaces unchanged.

### WU-01 acceptance

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/logger_batch ./pkg/plugin/base ./pkg/observability/metrics -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/logger_batch/... ./pkg/plugin/base/... ./pkg/observability/metrics/...'
git diff --check -- pkg/plugin/logger_batch pkg/plugin/base pkg/observability/metrics pkg/server/route_handler_test.go
```

## WU-02: HTTP and reporter delivery owners

**Exclusive package ownership:**

- `pkg/plugin/clickhouse_logger`
- `pkg/plugin/elasticsearch_logger`
- `pkg/plugin/google_cloud_logging`
- `pkg/plugin/http_logger`
- `pkg/plugin/lago`
- `pkg/plugin/loggly`
- `pkg/plugin/loki_logger`
- `pkg/plugin/skywalking_logger`
- `pkg/plugin/splunk_hec_logging`
- `pkg/plugin/tencent_cloud_cls`
- `pkg/plugin/zipkin`

### Regression-first tests

- Add at least one focused cancellation test for each transport seam, not just signature compilation:
  - Resty request cancellation before the backend responds.
  - Elasticsearch bulk context cancellation.
  - Loggly standard HTTP request cancellation.
  - Google Cloud token acquisition or POST cancellation.
  - Zipkin request cancellation.
- Existing direct `SendBatch` tests pass `context.Background()` unless they are the cancellation test.
- Preserve existing payload, headers, partial-success, and response-status assertions.

Run the new named cancellation tests before production edits and record the expected compile/behavior failures.

### Implementation

- Add `context.Context` to each production delivery callback and all direct internal/test invocations.
- Supply a stable `PluginID` matching the APISIX plugin name to `base.BatchDefaults`; direct Zipkin config supplies `PluginID: "zipkin"`.
- Attach the context to every outbound request and token refresh involved in that delivery attempt.
- Keep existing client timeouts as a backend-specific upper bound; the earlier processor context deadline wins.
- Do not modify schemas, payloads, endpoint selection, retry counts, authentication, or TLS policy.

### WU-02 acceptance

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/clickhouse_logger ./pkg/plugin/elasticsearch_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/http_logger ./pkg/plugin/lago ./pkg/plugin/loggly ./pkg/plugin/loki_logger ./pkg/plugin/skywalking_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/tencent_cloud_cls ./pkg/plugin/zipkin -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/google_cloud_logging ./pkg/plugin/http_logger ./pkg/plugin/loggly ./pkg/plugin/zipkin -run "(Batch|Send|Deliver|Context|Cancel|Timeout)" -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/clickhouse_logger/... ./pkg/plugin/elasticsearch_logger/... ./pkg/plugin/google_cloud_logging/... ./pkg/plugin/http_logger/... ./pkg/plugin/lago/... ./pkg/plugin/loggly/... ./pkg/plugin/loki_logger/... ./pkg/plugin/skywalking_logger/... ./pkg/plugin/splunk_hec_logging/... ./pkg/plugin/tencent_cloud_cls/... ./pkg/plugin/zipkin/...'
git diff --check -- pkg/plugin/clickhouse_logger pkg/plugin/elasticsearch_logger pkg/plugin/google_cloud_logging pkg/plugin/http_logger pkg/plugin/lago pkg/plugin/loggly pkg/plugin/loki_logger pkg/plugin/skywalking_logger pkg/plugin/splunk_hec_logging pkg/plugin/tencent_cloud_cls pkg/plugin/zipkin
```

## WU-03: Socket, message-broker, and special delivery owners

**Exclusive package ownership:**

- `pkg/plugin/datadog`
- `pkg/plugin/error_log_logger`
- `pkg/plugin/kafka_logger`
- `pkg/plugin/rocketmq_logger`
- `pkg/plugin/sls_logger`
- `pkg/plugin/syslog`
- `pkg/plugin/tcp_logger`
- `pkg/plugin/udp_logger`

### Regression-first tests

- Add a Kafka test proving a canceled parent context reaches `WriteMessages` rather than a fresh background context.
- Add a RocketMQ test proving the supplied context bounds the send operation.
- Add an error-log test proving both the HTTP/message path and TCP path honor the supplied context.
- Add one retained TCP or syslog cancellation test proving a processor deadline shorter than the plugin timeout unblocks delivery.
- Add a UDP/datadog test for earlier context deadline selection without changing datagram payload behavior.
- Existing direct delivery tests use `context.Background()` where cancellation is not under test.

Run the new named tests before production edits and record the expected compile/behavior failures.

### Implementation

- Add `context.Context` to all delivery callbacks and direct invocations.
- Supply stable plugin IDs through `BatchDefaults`.
- Replace `context.Background()` in Kafka, RocketMQ, and error-log attempt paths with the supplied parent context.
- Use context-aware dialing and the earlier context/configured deadline for socket writes. Preserve existing partial-write and ambiguous-delivery behavior.
- Ensure cancellation helpers/goroutines have deterministic exit and do not outlive the attempt.
- Do not change producer configuration, payloads, sender ownership, TLS behavior, or backend retry policy.

### WU-03 acceptance

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/datadog ./pkg/plugin/error_log_logger ./pkg/plugin/kafka_logger ./pkg/plugin/rocketmq_logger ./pkg/plugin/sls_logger ./pkg/plugin/syslog ./pkg/plugin/tcp_logger ./pkg/plugin/udp_logger -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/error_log_logger ./pkg/plugin/kafka_logger ./pkg/plugin/rocketmq_logger ./pkg/plugin/syslog ./pkg/plugin/tcp_logger -run "(Batch|Send|Deliver|Context|Cancel|Timeout|Stop)" -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/datadog/... ./pkg/plugin/error_log_logger/... ./pkg/plugin/kafka_logger/... ./pkg/plugin/rocketmq_logger/... ./pkg/plugin/sls_logger/... ./pkg/plugin/syslog/... ./pkg/plugin/tcp_logger/... ./pkg/plugin/udp_logger/...'
git diff --check -- pkg/plugin/datadog pkg/plugin/error_log_logger pkg/plugin/kafka_logger pkg/plugin/rocketmq_logger pkg/plugin/sls_logger pkg/plugin/syslog pkg/plugin/tcp_logger pkg/plugin/udp_logger
```

## Documentation and combined verification

After all WUs are accepted:

- Add a concise `Logger batch resource ownership` section to `docs/design.md` documenting the central defaults, exact rejection boundary, non-blocking Push, fixed worker/deadline behavior, shutdown compatibility, and metric lifecycle.
- Do not edit every logger row in `docs/plugins.md`; the common design contract applies to all base-backed loggers and Zipkin.
- Inspect `rg` results for all `NewBatchProcessor`, `logger_batch.New`, `SendBatch`, `deliver`, and `Stop` call sites. No production sink may remain on the legacy constructor.

Run the combined impact-scoped gates from the Plan 09 worktree:

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/logger_batch ./pkg/plugin/base ./pkg/observability/metrics -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/clickhouse_logger ./pkg/plugin/datadog ./pkg/plugin/elasticsearch_logger ./pkg/plugin/error_log_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/http_logger ./pkg/plugin/kafka_logger ./pkg/plugin/lago ./pkg/plugin/loggly ./pkg/plugin/loki_logger ./pkg/plugin/rocketmq_logger ./pkg/plugin/skywalking_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/syslog ./pkg/plugin/tcp_logger ./pkg/plugin/tencent_cloud_cls ./pkg/plugin/udp_logger ./pkg/plugin/zipkin -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/logger_batch/... ./pkg/plugin/base/... ./pkg/observability/metrics/... ./pkg/plugin/clickhouse_logger/... ./pkg/plugin/datadog/... ./pkg/plugin/elasticsearch_logger/... ./pkg/plugin/error_log_logger/... ./pkg/plugin/google_cloud_logging/... ./pkg/plugin/http_logger/... ./pkg/plugin/kafka_logger/... ./pkg/plugin/lago/... ./pkg/plugin/loggly/... ./pkg/plugin/loki_logger/... ./pkg/plugin/rocketmq_logger/... ./pkg/plugin/skywalking_logger/... ./pkg/plugin/sls_logger/... ./pkg/plugin/splunk_hec_logging/... ./pkg/plugin/syslog/... ./pkg/plugin/tcp_logger/... ./pkg/plugin/tencent_cloud_cls/... ./pkg/plugin/udp_logger/... ./pkg/plugin/zipkin/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

The main agent then performs a scoped diff/call-site audit and dispatches one independent read-only code reviewer. Findings are repaired regression-first within the original ownership boundaries. Only an APPROVE review plus fresh unchanged-diff verification authorizes commit, push, ready PR creation, and merge after required remote checks pass.

## PR acceptance checklist

- [ ] Zero/unset pending is centrally bounded at 10,000 for every production processor.
- [ ] `pending >= max` rejects exactly at capacity and Push never waits for delivery capacity.
- [ ] Fixed workers bound active delivery calls; default maximum is one.
- [ ] Ready, active, and retrying entries remain counted in pending.
- [ ] Attempt timeout and retry delay are context-cancellable.
- [ ] All 19 production delivery owners honor the supplied context.
- [ ] Stop is bounded and idempotent without changing the route/plugin lifecycle signature.
- [ ] Pending, capacity drops, delivery failure/timeout, stopped drops, and shutdown timeout are observable with controlled metric labels.
- [ ] Dynamic pending/buffer gauges are correct across overlapping route generations and are removed after the last owner closes.
- [ ] Timer scheduling honors both inactivity and hard buffer duration without stale callback interference.
- [ ] Focused package tests, relevant race gates, scoped lint, build/clean, diff check, and independent review pass.
