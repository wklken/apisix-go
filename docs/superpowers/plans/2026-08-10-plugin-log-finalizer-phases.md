# Plugin Log and Finalizer Phases Implementation Plan

> Implementation status (2026-08-12): completed on the Plan 16 baseline. The
> production contract preserves `RequestStageLegacy == 0` but assigns every
> registered factory an explicit non-legacy request owner, uses the canonical
> 115-key/114-identity capability registry, captures one immutable bounded log
> snapshot, completes outcome before finalizers, and recycles request variables
> only after finalization. The implementation and tests are authoritative where
> older task sketches below mention provisional interfaces.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute request metrics, tracing completion, logger delivery, and infrastructure cleanup exactly once after every request outcome, then enforce complete explicit capability classification for every registered HTTP plugin.

**Architecture:** Before terminal execution, the route executor computes the union of logger capture requirements, captures/restores any authorized request-body prefix, and installs one capability-preserving bounded response observer. It registers one composite log finalizer that reads the lifecycle's final replacement request and deep-copies captured state. Inside the composite, log/tracer plugins execute in APISIX order—global then merged route/consumer, descending priority—with an independent recover boundary per plugin. After `RequestLifecycle.Finalize` returns, the outer route epilogue recycles pooled request state; recycling is not a lifecycle hook. A registry completeness gate rejects any registered HTTP plugin lacking an explicit multi-capability manifest.

**Tech Stack:** Go 1.26, request lifecycle/outcome foundation, explicit request/response executors, logger-batch safety contract, Prometheus metrics facade, trace/log plugin packages.

## Global Constraints

- Depends on the merged streaming/terminal phase plan.
- Pinned log order: global log plugins first, then merged route/consumer log plugins; within each scope priority is descending.
- The composite log finalizer runs after terminal/upstream/response completion for success, early auth/limit/validation stop, cache hit, stable pre-commit panic, post-commit abort, cancellation, and `http.ErrAbortHandler`.
- Each log/finalizer callback has its own recover boundary. One panic cannot skip later callbacks, replace the selected response, or suppress request-state recycling.
- The outer route owner calls `RecycleVars` only after all lifecycle finalizers, including the composite, return. Loggers consume immutable snapshots and may not retain pooled request maps or response-header maps. LIFO does not control recycling.
- The stable panic value and stack never enter configured log fields. Only bounded outcome/panic stage values are exposed.
- Metrics labels remain bounded. No route ID, URI, request ID, arbitrary plugin name, panic text, consumer username, or upstream address is added to a new metric label.
- Logger delivery remains non-blocking and bounded by the merged logger-batch plan. This PR changes phase ownership, not payload schemas, retry semantics, or sink transport configuration.
- A plugin may own multiple capabilities. Completeness is not a one-plugin/one-phase mapping.
- `finalizer` is per-request only. Route/process lifecycle owners use `generation_owner` and keep their existing Start/Stop retirement path; they never enter the request log/finalizer executor.
- Only after this PR's completeness and legacy scans pass may PR-014 and P1 5.5 be marked closed.

---

### Task 1: Define immutable log snapshots and child-safe log contract

**Files:**
- Create: `pkg/plugin/base/log_phase.go`
- Create: `pkg/plugin/base/log_phase_test.go`
- Modify: `pkg/plugin/base/logging.go`
- Modify: `pkg/plugin/base/logging_test.go`
- Modify: `pkg/plugin/base/access_log.go`
- Modify: `pkg/plugin/base/access_log_test.go`
- Modify: `pkg/apisix/ctx/lifecycle.go`
- Modify: `pkg/apisix/ctx/lifecycle_test.go`

**Interfaces:**

```go
type LogSnapshot struct {
    Request  RequestLogSnapshot
    Response ResponseLogSnapshot
    Outcome  ctx.ResponseOutcome
    Started  time.Time
    Finished time.Time
}

type RequestLogSnapshot struct {
    Access        AccessLogRequest
    Header        http.Header
    Body          []byte
    BodyTruncated bool
    APISIXVars    map[string]any
    RequestVars   map[string]any
    Consumer      SafeConsumerLogIdentity
}

type ResponseLogSnapshot struct {
    Status        int
    Header        http.Header
    Trailer       http.Header
    Body          []byte
    BodyTruncated bool
    Bytes         int64
}

type SafeConsumerLogIdentity struct {
    Username string
    GroupID  string
}

type LogCapturePolicy struct {
    RequestBodyBytes  int
    ResponseBodyBytes int
}

type LogCapturePolicyPlugin interface {
    LogCapturePolicy() LogCapturePolicy
}

type LogPhasePlugin interface {
    RunLogPhase(LogSnapshot) error
}

type FinalizerPhasePlugin interface {
    RunFinalizerPhase(LogSnapshot) error
}
```

The route executor takes the maximum authorized request/response body limit across active log bindings, never an unbounded sum. Before terminal execution it calls the existing shared request-body capture once and restores `r.Body`; it installs a `httpsnoop.Wrap` response observer that captures at most that maximum while preserving optional interfaces. The union-sized capture remains private to `LogExecutor`. Before every callback, the executor deep-clones final request data and slices request/response body prefixes to that binding's own `LogCapturePolicy`; only this per-binding snapshot reaches `RunLogPhase` or `RunFinalizerPhase`. A plugin authorized for 1 KiB can never observe bytes captured solely for a 512 KiB peer. Snapshots never expose the live request, body stream, pooled maps, consumer plugin config, mutable headers, or another binding's larger capture.

- [ ] **Step 1: Add snapshot and lifecycle ordering regressions**

Add:

```go
func TestLogSnapshotDoesNotAliasRequestOrResponse(t *testing.T)
func TestLogSnapshotRedactsProtectedConsumerConfiguration(t *testing.T)
func TestRequestLifecycleRunsLogCompositeBeforeRequestStateRecycle(t *testing.T)
func TestRequestLifecycleFinalizerPanicStillRecyclesState(t *testing.T)
func TestLogCapturePreservesWriterCapabilities(t *testing.T)
func TestLogCaptureUsesMaximumAuthorizedBoundOnce(t *testing.T)
func TestLogCaptureRestrictsEveryBindingToItsOwnPolicy(t *testing.T)
func TestLogSnapshotUsesFinalReplacementRequest(t *testing.T)
```

- [ ] **Step 2: Run tests and record compile-red**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/apisix/ctx -run "(LogSnapshot|LogComposite|FinalizerPanic)" -count=1'
```

- [ ] **Step 3: Implement snapshots by reusing the centralized log view**

Do not invent a second redaction policy. Extend the existing shared request/response capture and `AccessLogRequest` helpers. Snapshot response status/bytes/commit/flush/hijack from the outer outcome owner. Snapshot final request variables inside the composite; the outer route epilogue recycles only after `Finalize` completes.

- [ ] **Step 4: Run focused race tests**

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/base ./pkg/apisix/ctx -run "(LogSnapshot|LogComposite|RequestLifecycle)" -count=3'
```

### Task 2: Add the composite log/finalizer executor

**Files:**
- Create: `pkg/plugin/log_executor.go`
- Create: `pkg/plugin/log_executor_test.go`
- Modify: `pkg/plugin/executor.go`
- Modify: `pkg/plugin/response_capability.go`

**Interfaces:**

```go
type LogBinding struct {
    Plugin     Plugin
    Scope      Scope
    Provenance ResourceProvenance
    Policy     base.LogCapturePolicy
}

type LogExecutor struct { /* cloned global and merged slices */ }

func NewLogExecutor(bindings []LogBinding) LogExecutor
func (e LogExecutor) Prepare(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, error)
func (e LogExecutor) Register(r *http.Request, snapshot func() base.LogSnapshot) error
```

`Prepare` performs the one pre-terminal request/response capture and updates the lifecycle final request. `Register` adds one lifecycle finalizer, not one callback per plugin. The composite sorts and runs log methods first, then per-request finalizer methods, with a private recover boundary and a newly cloned, policy-limited snapshot around each invocation. The maximum union capture is never passed to plugin code.

- [ ] **Step 1: Add exact order and isolation tests**

Cover global-before-route despite conflicting priority, same-name consumer override, plugin implementing both log and finalizer, early stop, normal response, pre-commit panic, post-flush abort, original `ErrAbortHandler`, callback error, callback panic, exactly-once repeated finalization, and two bindings with 1 KiB/512 KiB limits where the smaller binding cannot read the larger prefix.

- [ ] **Step 2: Run focused tests and record compile-red**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "^TestLogExecutor" -count=1'
```

- [ ] **Step 3: Implement independent callback boundaries**

Log bounded owner/stage metadata for callback errors and panics; do not expose panic values. Record `RequestPanicFinalizer` through the existing bounded metric. Return no aggregate error to the response path because the response is already selected; errors are observable through logs/metrics.

- [ ] **Step 4: Run log executor race tests**

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin -run "^TestLogExecutor" -count=3'
```

### Task 3: Move the shared logger base and serverless log configuration into log phase

**Files:**
- Modify: `pkg/plugin/base/types.go`
- Create: `pkg/plugin/base/types_test.go`
- Modify production/test files under:
  - `pkg/plugin/clickhouse_logger`
  - `pkg/plugin/elasticsearch_logger`
  - `pkg/plugin/file_logger`
  - `pkg/plugin/google_cloud_logging`
  - `pkg/plugin/http_logger`
  - `pkg/plugin/kafka_logger`
  - `pkg/plugin/lago`
  - `pkg/plugin/loggly`
  - `pkg/plugin/loki_logger`
  - `pkg/plugin/rocketmq_logger`
  - `pkg/plugin/skywalking_logger`
  - `pkg/plugin/sls_logger`
  - `pkg/plugin/splunk_hec_logging`
  - `pkg/plugin/syslog`
  - `pkg/plugin/tcp_logger`
  - `pkg/plugin/tencent_cloud_cls`
  - `pkg/plugin/udp_logger`
- Modify direct logger production/tests under:
  - `pkg/plugin/datadog`
- Modify: `pkg/plugin/serverless/plugin.go`
- Modify: `pkg/plugin/serverless/plugin_test.go`

**Interfaces:**

```go
func (p *BaseLoggerPlugin) RunLogPhase(snapshot LogSnapshot) error
```

Concrete plugins keep their payload builder but receive snapshot data rather than a live post-`next` request.

- [ ] **Step 1: Add base and representative sink regressions**

Test early auth rejection, success, panic outcome, cancellation, immutable per-binding snapshot, logger-batch capacity rejection, and no request-path wait. Cover at least HTTP, retained socket, Kafka/RocketMQ, and datadog. For both serverless plugin names configured with `phase=log`, assert success, early stop, and panic each invoke the log function exactly once and the legacy Handler no longer owns post-`next` log execution.

- [ ] **Step 2: Run red tests before migration**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin/{http_logger,tcp_logger,kafka_logger,rocketmq_logger,datadog,serverless} -run "(LogPhase|EarlyResponse|PanicOutcome|Snapshot)" -count=1'
```

- [ ] **Step 3: Implement the shared phase method and mechanical owners**

Remove post-`next` Fire calls from production Handler paths. Keep direct Handler only as a compatibility adapter for isolated package tests; route assembly installs `RunLogPhase` exclusively. Do not change batch delivery callback signatures or sink payload fields.

- [ ] **Step 4: Run every changed logger package**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{clickhouse_logger,datadog,elasticsearch_logger,file_logger,google_cloud_logging,http_logger,kafka_logger,lago,loggly,loki_logger,rocketmq_logger,serverless,skywalking_logger,sls_logger,splunk_hec_logging,syslog,tcp_logger,tencent_cloud_cls,udp_logger} -count=1'
```

### Task 4: Migrate request metrics and tracing completion

**Files:**
- Modify: `pkg/plugin/request_context/plugin.go`
- Modify: `pkg/plugin/request_context/plugin_test.go`
- Modify: `pkg/plugin/prometheus/plugin.go`
- Modify: `pkg/plugin/prometheus/plugin_test.go`
- Modify: `pkg/plugin/node_status/plugin.go`
- Modify: `pkg/plugin/node_status/plugin_test.go`
- Modify: `pkg/plugin/skywalking/plugin.go`
- Modify: `pkg/plugin/skywalking/plugin_test.go`
- Modify: `pkg/plugin/zipkin/plugin.go`
- Modify: `pkg/plugin/zipkin/plugin_test.go`
- Modify: `pkg/plugin/otel/plugin.go`
- Modify: `pkg/plugin/otel/plugin_test.go`
- Modify tests only: `pkg/plugin/error_log_logger/plugin_test.go`
- Modify tests only: `pkg/plugin/log_rotate/plugin_test.go`
- Modify tests only: `pkg/plugin/server_info/plugin_test.go`

- [ ] **Step 1: Add outcome matrix tests**

Assert request totals/duration/status/bytes and trace span start/completion for normal, early auth/limit, cache hit, stable panic 500, post-write abort, cancellation, and hijack. Request variables must still be readable during phase and recycled afterward. For otel/skywalking/zipkin, require a request-phase start method that stores span state on the replacement request plus a per-request finalizer end method that runs exactly once; removing the legacy Handler must not remove span start. Add classification tests proving error-log-logger, log-rotate, and server-info retain only their existing generation/process Start/Stop ownership and never enter request `RunLogPhase`/`RunFinalizerPhase`; node-status remains the separately installed server wrapper.

- [ ] **Step 2: Run focused metrics/trace tests before edits**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{request_context,prometheus,node_status,skywalking,zipkin,otel,error_log_logger,log_rotate,server_info} -run "(LogPhase|Outcome|EarlyResponse|Panic|Abort|GenerationOwner)" -count=1'
```

- [ ] **Step 3: Move completion work into log/finalizer methods**

`request-context` request-phase initialization remains unchanged. Its direct legacy Handler fallback may create/finalize a local lifecycle for package compatibility, but production uses the outer route lifecycle. Otel, skywalking, and zipkin implement both the existing explicit request-phase contract for span start/context propagation and `FinalizerPhasePlugin` for span end/exporter enqueue.

- [ ] **Step 4: Run complete affected packages and races**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{request_context,prometheus,node_status,skywalking,zipkin,otel,error_log_logger,log_rotate,server_info} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/{request_context,skywalking,zipkin,otel} -run "(LogPhase|Outcome|Panic|Abort)" -count=3'
```

### Task 5: Install log bindings in route generations

**Files:**
- Modify: `pkg/route/builder.go`
- Create: `pkg/route/log_phase_test.go`
- Modify: `pkg/route/http_logger_test.go`
- Modify: `pkg/route/builder_lifecycle_test.go`

- [ ] **Step 1: Add route-level log matrix**

Cover global/route/consumer order, consumer override, early auth/limit, cache hit, buffered transform, SSE completion, websocket/hijack, pre/post-commit panic, logger panic isolation, route-generation retirement, and request-state recycling. Add the closure combination `key-auth reject + CORS + response-rewrite/error-page + logger`: upstream is not called, eligible response phases run once, logger runs once, and state recycles afterward.

- [ ] **Step 2: Register one composite after request initialization**

Build log bindings from retained provenance. Prepare bounded captures and register the composite after system request-context initializes lifecycle state. After lifecycle `Finalize` returns, the outer owner recycles pooled state unconditionally. The route generation's plugin Stop lifecycle remains separate and waits for logger-batch cleanup on generation retirement.

- [ ] **Step 3: Run route tests**

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "(LogPhase|HTTPLogger|BuilderLifecycle|StreamingPhase|BufferedResponsePhase)" -count=1'
bash -lc 'source .envrc && go test ./pkg/route -count=1'
```

### Task 6: Enforce complete capability classification and remove legacy post-next paths

**Files:**
- Create: `pkg/plugin/capability_registry.go`
- Create: `pkg/plugin/capability_registry_test.go`
- Create: `pkg/plugin/capability_fixture_test.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `pkg/plugin/executor.go`
- Modify: `pkg/route/builder.go`

**Interfaces:**

```go
type Capability uint32

const (
    CapabilitySystem Capability = 1 << iota
    CapabilityRequestRewrite
    CapabilityConsumerRewrite
    CapabilityRequestAccess
    CapabilityBeforeProxy
    CapabilityConditionalTerminal
    CapabilityHeaderFilter
    CapabilityBufferedBodyFilter
    CapabilityStreamingBodyFilter
    CapabilityProtocolOwner
    CapabilityLog
    CapabilityFinalizer
    CapabilityGenerationOwner
    CapabilitySeparateSubsystem
)

func CapabilitiesFor(name string) (Capability, bool)
```

The authoritative exact mapping is `2026-08-10-plugin-capability-manifest.md`. The completeness test uses the same static table as production and requires all 115 registry keys to resolve to 114 implementation identities. `otel` aliases `opentelemetry`; `request-context` is the factory key while `request_context` is only its implementation `GetName`.

- [ ] **Step 1: Add all-registered-plugin completeness test**

For every factory registered in `pkg/plugin/init.go`, require at least one capability or an explicit native/separate-subsystem deferral already documented in `docs/plugins.md`. A plugin may have multiple bits. Unknown names fail strict route build through the existing allowlist/factory boundary.

- [ ] **Step 2: Add legacy Handler behavior scan test**

Production route assembly must not call post-`next` behavior from a migrated plugin. Keep adapters only for audited request-only legacy handlers and package-level compatibility. The fixture catalog must construct all 114 implementation identities. If an identity genuinely cannot be instantiated in a unit test, it must appear in one fixed exception table with a checked static ownership proof (for example a route-owned protocol owner); the test fails for every undeclared skip, stale exception, missing identity, or duplicate fixture. Every constructible fixture is scanned for double phase invocation and post-`next` behavior.

- [ ] **Step 3: Run completeness and call-site scans**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/route -run "(CapabilityRegistry|RegistryCompleteness|CapabilityFixture|LegacyHandler|DoublePhase)" -count=1'
rg -n 'BuildPluginChain|\.Handler\(next\)|WithTransformPipeline|RunLogPhase|RunFinalizerPhase' pkg/plugin pkg/route cmd t
```

- [ ] **Step 4: Remove dead/proxy-only adapters**

For every moved/extracted/deleted symbol, run `rg` across production and tests. Delete adapters used only by obsolete tests unless they preserve a documented direct-plugin compatibility boundary. Remove transform-count plumbing when no production response plugin consumes it.

### Task 7: Verification, review, documentation, and independent PR delivery

**Files:**
- Modify: `docs/design.md`
- Modify: `docs/plugins.md`
- Include: `docs/superpowers/plans/2026-08-10-plugin-log-finalizer-phases.md`

- [ ] **Step 1: Run affected package and lifecycle race gates**

```bash
bash -lc 'source .envrc && go test ./pkg/apisix/ctx ./pkg/plugin/base ./pkg/plugin ./pkg/route ./pkg/server ./pkg/plugin/{request_context,prometheus,node_status,skywalking,zipkin,otel,clickhouse_logger,datadog,elasticsearch_logger,error_log_logger,file_logger,google_cloud_logging,http_logger,kafka_logger,lago,loggly,log_rotate,loki_logger,rocketmq_logger,server_info,serverless,skywalking_logger,sls_logger,splunk_hec_logging,syslog,tcp_logger,tencent_cloud_cls,udp_logger} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/apisix/ctx ./pkg/plugin ./pkg/route ./pkg/server -run "(LogPhase|Finalizer|Panic|Abort|Lifecycle|Generation)" -count=3'
```

- [ ] **Step 2: Run dead-code and classification scans**

```bash
rg -n 'Capability[A-Z]|CapabilitiesFor|BuildPluginChain|RunLogPhase|RunFinalizerPhase|AddFinalizer' pkg cmd t
```

Document every remaining legacy compatibility path and why production does not depend on post-`next` semantics.

- [ ] **Step 3: Run scoped lint/build/diff gates**

```bash
bash -lc 'source .envrc && golangci-lint run ./pkg/apisix/ctx/... ./pkg/plugin/... ./pkg/route/... ./pkg/server/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 4: Independent merge-level review**

Review must verify pinned log order, early/panic/abort coverage, immutable snapshots, callback isolation, request-state lifetime, bounded metrics, logger ownership, capability completeness, and no production post-next legacy behavior. It must explicitly confirm the seven-plan chain now closes PR-014 and P1 5.5.

- [ ] **Step 5: Deliver the independent PR**

After approval, commit:

```bash
git commit -m "refactor(plugin): execute log and finalizer phases"
```

Open one ready PR, wait for CI, and merge. Update the production-readiness ledger only after the merge commit exists.

## Fast-plan-impl Dispatch Ownership

1. **WU-01 log contracts/capture/executor/route/completeness** owns `pkg/plugin/base/log_phase*`, `pkg/plugin/base/logging.go`, `pkg/plugin/base/logging_test.go`, `pkg/plugin/base/access_log.go`, `pkg/plugin/base/access_log_test.go`, lifecycle additions, `pkg/plugin/log_executor*`, `pkg/plugin/capability_registry*`, `pkg/plugin/capability_fixture_test.go`, and named `pkg/route/**` files; freeze interfaces first.
2. **WU-02 logger and serverless-log migration** owns `pkg/plugin/base/types.go`, `pkg/plugin/base/types_test.go`, every logger directory listed in Task 3, datadog, and serverless, but no WU-01 base file.
3. **WU-03 metrics, tracing, and generation-owner audit** owns request-context, prometheus, node-status, skywalking, zipkin, otel, and test-only changes under error-log-logger/log-rotate/server-info. WU-02/WU-03 start after WU-01 and cannot edit its core/route files.

## Explicit Deferrals

- New log payload fields, sampling policies, route-ID metric labels, and sink transport changes.
- Broader request-data schema/redaction work remains in the request-data plan.
- Production metrics inventory and node-status product changes remain in the production-metrics plan; this PR only supplies correct lifecycle timing.
