# Runtime Safety and Stable Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound worker memory, high-cost work, logging and telemetry while making health, metrics, tracing and diagnostics generation-safe projections of the supervisor's authoritative lifecycle state.

**Architecture:** `pkg/runtime` combines a finite configured or cgroup-derived worker envelope with explicit component reservations and pressure admission; ordinary compatibility traffic keeps APISIX limits, while strict mode enables explicit Go admission and sustained hard pressure ends only the affected worker. Workers coalesce bounded metric deltas and send them over the supervisor protocol; the supervisor owns the durable-in-process aggregator, stable scrape/health endpoints and series lifecycle. OpenTelemetry and asynchronous loggers remain generation resources owned through plan 04's `RuntimeDependencies`, `ResourceRegistry` and `TaskRegistry`; diagnostics bind a separate authenticated, audited listener.

**Tech Stack:** Go 1.26 standard-library `runtime/debug`, `runtime/metrics`, `net/http`, `crypto/subtle`, existing Prometheus client, existing OpenTelemetry SDK/OTLP HTTP exporter, plan 04 `pkg/runtime`, plan 05 `pkg/supervisor`, `pkg/worker`, `pkg/lifecycle` and framed IPC.

**Spec:** `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`

## Global Constraints

- Compatibility target is Apache APISIX 3.17.0 at commit `9ef2ecab67f652d38365049613610ef649bb4ad0`.
- Preserve the APISIX namespace; all new policy keys in this plan live under `apisix_go.runtime.safety.*`, `apisix_go.telemetry.*`, or `apisix_go.diagnostics.*`.
- Preserve plan 02's `EffectiveConfig.Paths`; extend `EffectiveConfig` additively with `Runtime RuntimePolicy`. Plan 05 owns only `RuntimePolicy.Lifecycle`; this plan must consume that exact type without redefining it.
- Source `.envrc` before every Go or Make command.
- Use impact-scoped tests; do not run `go test ./...`, `go test ./pkg/...`, or `make test`.
- Run focused race tests for budget, task, logger, IPC, aggregation and series retirement changes, plus `source .envrc && make build` after each production cutover.
- Add no dependency. Use the repository's Prometheus and OpenTelemetry modules and the standard library for cgroup/procfs parsing, authentication and pprof.
- Compatibility mode maps explicit APISIX/NGINX limits and otherwise adds no hidden request/connection ceiling. In particular, `proxy.max_in_flight` missing or zero is unlimited; remove the implicit `1024` default.
- Strict admission is active only through explicit `apisix_go.runtime.safety.admission.*` values. It never changes compatibility mode silently.
- A missing finite cgroup/configured memory limit is allowed for local compatibility mode but is reported as unbounded; strict and `http-data-plane-v1` qualification reject it.
- Soft pressure rejects only new high-cost work and remains reversible. Sustained hard pressure requests a worker-local fatal exit; it never kills the supervisor or mutates the journal.
- Every production goroutine uses plan 04's `TaskRegistry` or `RequestTaskGroup`; do not add `go`, `WaitGroup.Go`, or a hidden background context.
- Plugin panics are recoverable only through plan 04's `plugin.PanicError`. Unknown core panics run request finalizers exactly once and then terminate the worker. After commit, flush or hijack, abort the connection and never attempt a second response.
- Async logging provides at-least-attempted delivery, never exactly-once delivery. Queues, bytes, attempts, deadlines, retries, drops and shutdown flush are bounded and observable.
- OpenTelemetry is off by default. There is no stdout exporter, package-init provider, process-global provider/propagator, or implicit `AlwaysSample` path. An explicit APISIX `always_on` sampler remains a configuration choice, not a default.
- Diagnostics are disabled by default and never share the data-plane, public API, health or metrics listener. When enabled they require authentication and emit lifecycle audit records without credentials or profile bytes.
- Health JSON and health metrics are projections of one supervisor state snapshot. Metrics, exporter and diagnostics failures do not independently change traffic readiness.
- Do not edit or stage the four user-owned untracked files under `docs/reviews/`.

---

## File and Responsibility Map

**Create:**

- `pkg/config/runtime_policy.go` — extend plan 05's existing lifecycle policy with safety, telemetry and diagnostics fields.
- `pkg/config/runtime_policy_test.go` — presence, aliases, profile validation, redaction and invalid-combination tests.
- `pkg/runtime/memory_source.go`, `memory_source_linux.go`, `memory_source_other.go` — configured/cgroup envelope and current worker memory samples.
- `pkg/runtime/budget.go`, `budget_test.go` — component reservations, soft shedding, sustained-hard detection and snapshots.
- `pkg/runtime/admission.go`, `admission_test.go` — compatibility and strict connection/request/high-cost gates.
- `pkg/telemetry/types.go` — bounded metric delta protocol independent of transport.
- `pkg/telemetry/reporter.go`, `reporter_test.go` — worker-side coalescing, overflow and bounded flush.
- `pkg/telemetry/aggregator.go`, `aggregator_test.go` — supervisor-owned monotonic aggregation and worker gauge retirement.
- `pkg/telemetry/cardinality.go`, `cardinality_test.go` — per-family/global budgets, overflow labels and TTL rules.
- `pkg/telemetry/prometheus.go`, `prometheus_test.go` — supervisor registry/handler and APISIX family projection.
- `pkg/diagnostics/server.go`, `server_test.go` — separate authenticated pprof/runtime listener with bounded requests.
- `pkg/diagnostics/audit.go`, `audit_test.go` — bounded diagnostics-to-`lifecycle.Event` audit mapping.
- `scripts/runtime_capacity.sh`, `scripts/runtime_capacity_test.sh` — real-process overload/recovery evidence runner and contract test.
- `docs/architecture/runtime-safety-observability.md` — operator-visible budgets, state, telemetry, diagnostics and failure semantics.

**Modify:**

- `pkg/config/effective.go`, `decode.go`, `extension_env.go`, `validation.go`, `redact.go` — decode `apisix_go.runtime`, `.telemetry` and `.diagnostics` into `EffectiveConfig.Runtime` with provenance and secret redaction.
- `conf/config-default.yaml`, `conf/config-production.yaml`, `docs/configuration.md`, `docs/production-profile.md` — document explicit Go policy; remove hidden `proxy.max_in_flight: 1024`.
- `pkg/runtime/dependencies.go` — add no field; safety owners obtain `BudgetManager` through `RuntimeDependencies.Resources` under a fixed worker-scoped resource key.
- `pkg/compiler/compiler.go`, `normalize.go`, `materialize.go`, `types.go` — compilation and retained generation reservations.
- `pkg/server/request_body_limit.go`, HTTP body/spool owner from plan 06, `pkg/proxy/cluster.go`, `pkg/plugin/proxy_cache/zones.go`, `pkg/plugin/graphql_proxy_cache/plugin.go` — reserve/release body, spool, connection and cache capacity.
- `pkg/plugin/logger_batch/processor.go`, `processor_test.go`, `pkg/plugin/base/types.go` and every logger constructor — byte-bounded queue, owned tasks and deadline/retry/shutdown contract.
- `pkg/plugin/file_logger/processor.go`, `writer_registry.go` and tests — use the same byte reservation/task/barrier semantics; retain path-keyed writer sharing.
- `pkg/observability/metrics/*.go` and tests — move global worker metrics into the reporter/supervisor aggregator; retain APISIX names and validated labels.
- `pkg/plugin/prometheus/plugin.go`, `pkg/route/extra.go` — route-compatible metric enablement points at the supervisor-owned stable handler.
- `pkg/plugin/otel/plugin.go`, `provider.go` and tests — generation-owned provider/exporter/sampler/propagators with no global lookup.
- `cmd/root.go`, `pkg/observability/otel/init.go`, `init_test.go` — delete the blank-import stdout provider path atomically.
- `pkg/server/route_handler.go`, `pkg/plugin/panic.go`, `pkg/apisix/ctx/lifecycle.go` and tests — preserve the plan 04 panic/finalizer boundary while emitting bounded telemetry.
- Plan 05 supervisor/worker/IPC/state files named in its stable interface — carry aggregate batches, pressure exit, stable health/metrics and diagnostics ownership without changing its lifecycle protocol.
- `Makefile` — `runtime-capacity-contract` and `runtime-capacity` targets.

**Delete during atomic cutovers:**

- `pkg/observability/otel/init.go`, `pkg/observability/otel/init_test.go`
- `pkg/observability/metrics/expiration_runtime.go` after series TTL moves to the supervisor aggregator
- global `initOnce`, exported collector variables, readiness mutex/state, `ConfiguredExportServer`, `StartExportServer`, and their tests/facades from `pkg/observability/metrics`

No zero-argument metric API, global collector mirror, legacy readiness recorder, global OTel provider or logger constructor without ownership dependencies may remain for tests.

## Stable Interfaces

These interfaces extend the total plan after plans 04 and 05. If implementation evidence changes a signature, amend the total plan, plan 02, plan 05 and every later consumer in one documentation change before product code.

```go
// package config
type RuntimePolicy struct {
	Lifecycle   LifecyclePolicy       `mapstructure:"lifecycle"`
	Safety      RuntimeSafetyPolicy   `mapstructure:"safety"`
	Telemetry   TelemetryPolicy       `mapstructure:"telemetry"`
	Diagnostics DiagnosticsPolicy     `mapstructure:"diagnostics"`
}

type RuntimeSafetyPolicy struct {
	Memory    MemoryPolicy          `mapstructure:"memory"`
	Budgets   ComponentBudgetPolicy `mapstructure:"budgets"`
	Admission AdmissionPolicy       `mapstructure:"admission"`
}

type MemoryPolicy struct {
	LimitBytes              int64         `mapstructure:"limit_bytes"`
	SupervisorReserveBytes  int64         `mapstructure:"supervisor_reserve_bytes"`
	ReplacementReserveBytes int64         `mapstructure:"replacement_reserve_bytes"`
	GoLimitPercent          int           `mapstructure:"go_limit_percent"`
	SoftPercent             int           `mapstructure:"soft_percent"`
	HardPercent             int           `mapstructure:"hard_percent"`
	SampleInterval          time.Duration `mapstructure:"sample_interval"`
	HardSustain             time.Duration `mapstructure:"hard_sustain"`
}

type ComponentBudgetPolicy struct {
	RequestBodyMemoryBytes int64 `mapstructure:"request_body_memory_bytes"`
	SpoolDiskBytes         int64 `mapstructure:"spool_disk_bytes"`
	CacheMemoryBytes       int64 `mapstructure:"cache_memory_bytes"`
	CacheDiskBytes         int64 `mapstructure:"cache_disk_bytes"`
	LoggerMemoryBytes      int64 `mapstructure:"logger_memory_bytes"`
	CompileMemoryBytes     int64 `mapstructure:"compile_memory_bytes"`
	GenerationMemoryBytes  int64 `mapstructure:"generation_memory_bytes"`
}

type AdmissionPolicy struct {
	MaxActiveConnections int `mapstructure:"max_active_connections"`
	MaxActiveRequests    int `mapstructure:"max_active_requests"`
	MaxHighCostRequests  int `mapstructure:"max_high_cost_requests"`
}

type TelemetryPolicy struct {
	WorkerQueueBytes    int64         `mapstructure:"worker_queue_bytes"`
	MaxFrameBytes       int           `mapstructure:"max_frame_bytes"`
	FlushInterval       time.Duration `mapstructure:"flush_interval"`
	MaxTotalSeries      int           `mapstructure:"max_total_series"`
	GenerationSeriesTTL time.Duration `mapstructure:"generation_series_ttl"`
}

type DiagnosticsPolicy struct {
	Enabled            bool          `mapstructure:"enabled"`
	Address            string        `mapstructure:"address"`
	BearerToken        string        `mapstructure:"bearer_token" secret:"true"`
	ReadHeaderTimeout  time.Duration `mapstructure:"read_header_timeout"`
	WriteTimeout       time.Duration `mapstructure:"write_timeout"`
	MaxProfileDuration time.Duration `mapstructure:"max_profile_duration"`
	MaxConcurrent      int           `mapstructure:"max_concurrent"`
}

type EffectiveConfig struct {
	Config     Config
	Provenance Provenance
	Profiles   ProfileSelection
	Paths      RuntimePaths
	Runtime    RuntimePolicy
}
```

All byte/count fields are non-negative and presence-aware. Zero safety budgets mean “share only the finite worker/global envelope” in compatibility mode; strict and `http-data-plane-v1` require a finite memory envelope and positive component/telemetry/diagnostics limits for every enabled feature. Default percentages are `GoLimitPercent=90`, `SoftPercent=80`, `HardPercent=95`; default sampling is one second and hard sustain is thirty seconds. Percentages must satisfy `50 <= GoLimitPercent <= 95`, `50 <= SoftPercent < HardPercent <= 100`. `MaxActive* == 0` disables only the strict Go-specific gate. Diagnostics defaults are disabled; when enabled, address, a bearer token of at least 32 bytes, positive timeouts and positive concurrency are required.

```go
// package runtime
type BudgetClass string
const (
	BudgetRequestBodyMemory BudgetClass = "request-body-memory"
	BudgetSpoolDisk         BudgetClass = "spool-disk"
	BudgetCacheMemory       BudgetClass = "cache-memory"
	BudgetCacheDisk         BudgetClass = "cache-disk"
	BudgetLoggerMemory      BudgetClass = "logger-memory"
	BudgetCompileMemory     BudgetClass = "compile-memory"
	BudgetGenerationMemory  BudgetClass = "generation-memory"
)

type WorkClass string
const (
	WorkOrdinary WorkClass = "ordinary"
	WorkHighCost WorkClass = "high-cost"
)

type MemorySource interface {
	Limit(context.Context) (bytes int64, source string, err error)
	Current(context.Context) (bytes int64, err error)
}

type PressureLevel string
const (
	PressureNormal PressureLevel = "normal"
	PressureSoft   PressureLevel = "soft"
	PressureHard   PressureLevel = "hard"
)

type BudgetSnapshot struct {
	LimitBytes  int64
	CurrentBytes int64
	Level       PressureLevel
	Used        map[BudgetClass]int64
	Limits      map[BudgetClass]int64
}

type BudgetManager struct {
	policy         config.RuntimeSafetyPolicy
	source         MemorySource
	onHard         HardPressureHandler
	setMemoryLimit func(int64) int64
	workerLimit    int64
	mu             sync.RWMutex
	used           map[BudgetClass]int64
	snapshot       BudgetSnapshot
	hardSince      time.Time
	hardSignaled   bool
	started        bool
}

type Reservation struct {
	manager     *BudgetManager
	class       BudgetClass
	bytes       int64
	releaseOnce sync.Once
}
func (r *Reservation) Release()

type HardPressureHandler func(BudgetSnapshot)

func NewBudgetManager(config.RuntimeSafetyPolicy, MemorySource, HardPressureHandler) (*BudgetManager, error)
func (m *BudgetManager) Start(*TaskRegistry) error
func (m *BudgetManager) Admit(WorkClass) error
func (m *BudgetManager) Reserve(BudgetClass, int64) (*Reservation, error)
func (m *BudgetManager) Snapshot() BudgetSnapshot

type AdmissionController struct {
	profile           config.ProfileSelection
	apisix            config.Config
	policy            config.AdmissionPolicy
	budgets           *BudgetManager
	activeConnections atomic.Int64
	activeRequests    atomic.Int64
	activeHighCost    atomic.Int64
}

func NewAdmissionController(config.ProfileSelection, config.Config, config.AdmissionPolicy, *BudgetManager) (*AdmissionController, error)
func (a *AdmissionController) WrapListener(net.Listener) net.Listener
func (a *AdmissionController) WrapHTTP(http.Handler) http.Handler
func (a *AdmissionController) BeginHighCost() (release func(), err error)
```

`NewBudgetManager` computes the finite worker envelope as the smaller positive configured/cgroup limit minus supervisor and replacement reserves, calls `debug.SetMemoryLimit(envelope*GoLimitPercent/100)`, and never treats `debug.SetMemoryLimit` as the hard kill. `Start` registers the sampler as `TaskCore`; the hard callback fires once only after every sample in `HardSustain` remains at or above the hard watermark. A normal sample immediately clears soft shedding and resets the hard timer. The callback is internal to `worker.Bootstrap.Run`: it sends a terminal `lifecycle.Status{State: lifecycle.StateFailed, Ready: false, Terminal: true, ReasonCode: lifecycle.ReasonMemoryPressureHard}` through plan 05's existing `lifecycle.Codec`, cancels the worker run context, and causes `Run` to return `runtime.ErrSustainedHardPressure`. The supervisor handles the resulting worker exit through its existing restart/probation rules; this plan adds no lifecycle method or fatal-exit interface.

```go
// package proxy; BodyDirection, BodyMode, BodyPlan and BodyBudget are defined by plan 06.
type bodyBudgetKey struct {
	direction BodyDirection
	mode      BodyMode
}

type runtimeBodyBudget struct {
	plan    BodyPlan
	manager *runtime.BudgetManager
	mu      sync.Mutex
	used    map[bodyBudgetKey]int64
}

type runtimeBodyReservation struct {
	budget *runtimeBodyBudget
	worker *runtime.Reservation
	key    bodyBudgetKey
	bytes  int64
	once   sync.Once
}

func NewRuntimeBodyBudget(BodyPlan, *runtime.BudgetManager) BodyBudget
func (b *runtimeBodyBudget) Reserve(context.Context, BodyDirection, BodyMode, int64) (release func(), err error)
func (r *runtimeBodyReservation) release()
```

`runtimeBodyBudget.Reserve` first enforces the per-request `BodyPlan` limit for `(BodyDirection, BodyMode)`, then reserves the same bytes from the worker `BudgetManager`: `BodyMemory` maps to `BudgetRequestBodyMemory` and `BodySpool` maps to `BudgetSpoolDisk`. Its returned closure calls `runtimeBodyReservation.once.Do`, releases the worker reservation, then decrements per-request usage. `BodyStream` reserves nothing. Cancellation or any later chunk/spool error makes plan 06's `MaterializeRequestBody` invoke all previously returned release closures before returning.

```go
// package telemetry
type MetricKind string
const (
	KindCounter   MetricKind = "counter"
	KindGauge     MetricKind = "gauge"
	KindHistogram MetricKind = "histogram"
)

type Label struct { Name, Value string }
type Point struct {
	Name      string
	Kind      MetricKind
	Labels    []Label
	Counter   uint64
	Gauge     float64
	Bounds    []float64
	Buckets   []uint64
	Sum       float64
}
type Batch struct {
	Generation uint64
	Sequence   uint64
	Points     []Point
	Dropped    uint64
}

type BatchSender interface {
	SendTelemetry(context.Context, Batch) error
}

type seriesKey string

type pendingPoint struct {
	point Point
	bytes int64
}

type Reporter struct {
	policy     config.TelemetryPolicy
	generation uint64
	sender     BatchSender
	tasks      *runtime.TaskRegistry
	mu         sync.Mutex
	points     map[seriesKey]pendingPoint
	queueBytes int64
	sequence   uint64
	dropped    uint64
	closed     bool
	closeOnce  sync.Once
	closeErr   error
}

func NewReporter(config.TelemetryPolicy, uint64, BatchSender, *runtime.TaskRegistry) (*Reporter, error)
func (r *Reporter) AddCounter(string, []Label, uint64)
func (r *Reporter) SetGauge(string, []Label, float64)
func (r *Reporter) Observe(string, []Label, []float64, float64)
func (r *Reporter) Flush(context.Context) error
func (r *Reporter) Close(context.Context) error

type workerKey struct {
	pid        int
	generation uint64
}

type aggregatePoint struct {
	point     Point
	lastSeen  time.Time
	retiredAt time.Time
}

type cardinalityBudget struct {
	maxTotal  int
	total     int
	perFamily map[string]int
}

type Aggregator struct {
	policy        config.TelemetryPolicy
	registry      *prometheus.Registry
	status        lifecycle.StatusProvider
	mu            sync.RWMutex
	stable        map[seriesKey]aggregatePoint
	generation    map[workerKey]map[seriesKey]aggregatePoint
	gauges        map[workerKey]map[seriesKey]aggregatePoint
	lastSequences map[workerKey]uint64
	cardinality   cardinalityBudget
	handler       http.Handler
}

func NewAggregator(config.TelemetryPolicy, *prometheus.Registry, lifecycle.StatusProvider) (*Aggregator, error)
func (a *Aggregator) Apply(lifecycle.Status, Batch) error
func (a *Aggregator) RetireWorker(lifecycle.Status)
func (a *Aggregator) Handler() http.Handler
func (a *Aggregator) Expire(time.Time) int
```

Reporter methods never block the request path: they coalesce deltas under the byte budget, preserve existing admitted keys, and redirect unseen keys to bounded overflow points when full. `Flush` creates one frame no larger than `MaxFrameBytes`; sequence is strictly increasing per worker generation. The aggregator rejects duplicate/out-of-order sequence numbers, unknown names/kinds/labels, histogram shape changes and oversized batches. Stable counters/histograms omit worker/generation identity and only increase; per-worker gauges are summed and removed immediately on `RetireWorker`; generation-detail counter/histogram series expire only after `GenerationSeriesTTL`.

The concrete transport is plan 05's `lifecycle.Codec`. Add `MessageMetricBatch MessageType = "metric_batch"` beside, not in place of, its existing `MessageTelemetry` carrying `lifecycle.WorkerTelemetry`; do not change `Frame`, `Codec`, `WorkerTelemetry`, `WorkerTelemetrySink` or their signatures. Consume plan 05's existing `lifecycle.ReasonMemoryPressureHard` constant in worker status construction and tests; this plan must not redeclare it or use a duplicate literal. The supervisor supplies the authenticated `lifecycle.Status` for `Apply`, and the batch generation must equal that status generation. Health and metrics consume the exact `lifecycle.StatusProvider` interface; this plan creates no second lifecycle state or transport.

---

### Task 1: Extend Effective Configuration With Explicit Runtime Policy

**Files:**

- Modify: plan 05-created `pkg/config/runtime_policy.go`
- Create: `pkg/config/runtime_policy_test.go`
- Modify: `pkg/config/effective.go`, `decode.go`, `extension_env.go`, `validation.go`, `redact.go`
- Modify: `conf/config-default.yaml`, `conf/config-production.yaml`
- Modify documentation interface only: total plan and plan 02 in the same implementation documentation commit

**Interfaces:**

- Consumes: plan 02 `EffectiveConfig`, `Provenance`, presence-aware `apisix_go` decoder and plan 05 `LifecyclePolicy`.
- Produces: the exact `RuntimePolicy`, safety, telemetry and diagnostics types above; external paths and APISIXGO aliases are deterministic.

- [ ] **Step 1: Write failing presence, alias and redaction tests**

```go
func TestLoadEffectiveRuntimePolicyPreservesPresenceAliasesAndSecrets(t *testing.T) {
	req := loadRequestFixture(t, SecurityStrict, `apisix_go:
  runtime: {safety: {memory: {limit_bytes: 536870912}}}
  telemetry: {worker_queue_bytes: 1048576, max_frame_bytes: 65536, flush_interval: 1s, max_total_series: 10000, generation_series_ttl: 10m}
  diagnostics: {enabled: true, address: 127.0.0.1:9092, bearer_token: 0123456789abcdef0123456789abcdef, read_header_timeout: 5s, write_timeout: 30s, max_profile_duration: 10s, max_concurrent: 1}
`)
	req.Environment["APISIXGO_RUNTIME_SAFETY_MEMORY_LIMIT_BYTES"] = "1073741824"
	effective, err := LoadEffective(req)
	if err != nil { t.Fatal(err) }
	if effective.Runtime.Safety.Memory.LimitBytes != 1073741824 { t.Fatalf("limit = %d", effective.Runtime.Safety.Memory.LimitBytes) }
	if got := effective.Provenance["apisix_go.runtime.safety.memory.limit_bytes"].Origin; got != "APISIXGO_RUNTIME_SAFETY_MEMORY_LIMIT_BYTES" { t.Fatalf("origin = %q", got) }
	dump, err := RenderEffectiveRedacted(effective); if err != nil { t.Fatal(err) }
	if bytes.Contains(dump, []byte(effective.Runtime.Diagnostics.BearerToken)) { t.Fatal("diagnostics token leaked") }
}
```

Add tables for absent versus explicit zero, negative bytes, invalid percentages/order, non-positive intervals, strict finite-budget requirements, diagnostics disabled with empty fields, enabled diagnostics with short token/non-loopback address, and compatibility zero budgets.

- [ ] **Step 2: Run the config contract and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^TestLoadEffectiveRuntimePolicy" -count=1'`

Expected: FAIL because plan 05's `EffectiveConfig.Runtime` contains only `Lifecycle`; `Safety`, `Telemetry`, `Diagnostics` and their policy types do not exist.

- [ ] **Step 3: Add the exact policy types and decoder path**

Use the Stable Interfaces declarations. Extend plan 02's private `apisix_go` decode struct rather than adding fields to APISIX `Config`:

```go
type goExtension struct {
	RuntimePaths RuntimePaths     `mapstructure:"runtime_paths"`
	Runtime      RuntimeSubpolicy `mapstructure:"runtime"`
	Telemetry    TelemetryPolicy  `mapstructure:"telemetry"`
	Diagnostics  DiagnosticsPolicy `mapstructure:"diagnostics"`
}

type RuntimeSubpolicy struct {
	Lifecycle LifecyclePolicy     `mapstructure:"lifecycle"`
	Safety    RuntimeSafetyPolicy `mapstructure:"safety"`
}
```

Assemble `EffectiveConfig.Runtime` from that private shape. Extend the schema index and provenance matcher for every leaf. APISIXGO aliases remove only `apisix_go`, for example `APISIXGO_RUNTIME_SAFETY_BUDGETS_LOGGER_MEMORY_BYTES`, `APISIXGO_TELEMETRY_MAX_FRAME_BYTES`, and `APISIXGO_DIAGNOSTICS_ENABLED`.

- [ ] **Step 4: Implement exact defaulting and validation**

Apply the stable percentage/time defaults only when absent, not when explicitly zero. Reject nonzero component sums that overflow `int64`; validate disk and memory budgets independently. Compatibility permits zero component/admission values. Strict/qualification requires a finite memory source at runtime, but static validation requires positive component budgets, telemetry queue/frame/interval/series/TTL, and complete diagnostics fields only when diagnostics is enabled. Require `Telemetry.MaxFrameBytes <= Lifecycle.IPCMaxFrameBytes-256`; the fixed 256-byte allowance covers the plan 05 frame envelope, and validation fails rather than silently shrinking either configured value.

- [ ] **Step 5: Add checked-in policy examples**

Remove `proxy.max_in_flight: 1024` from `conf/config-default.yaml`. Keep all Go-specific admission keys absent/zero there. Add these exact reference values to `conf/config-production.yaml`; they are explicit Go deployment policy, not APISIX defaults:

```yaml
apisix_go:
  runtime:
    safety:
      memory:
        limit_bytes: 536870912
        supervisor_reserve_bytes: 33554432
        replacement_reserve_bytes: 134217728
        go_limit_percent: 90
        soft_percent: 80
        hard_percent: 95
        sample_interval: 1s
        hard_sustain: 30s
      budgets:
        request_body_memory_bytes: 67108864
        spool_disk_bytes: 1073741824
        cache_memory_bytes: 134217728
        cache_disk_bytes: 2147483648
        logger_memory_bytes: 67108864
        compile_memory_bytes: 67108864
        generation_memory_bytes: 134217728
      admission:
        max_active_connections: 10000
        max_active_requests: 2048
        max_high_cost_requests: 256
  telemetry:
    worker_queue_bytes: 4194304
    max_frame_bytes: 262144
    flush_interval: 1s
    max_total_series: 100000
    generation_series_ttl: 10m
  diagnostics:
    enabled: false
```

Plan 05's production `runtime.lifecycle.ipc_max_frame_bytes` must be at least `262400` for this profile; use `1048576` as its checked-in value. Do not add a diagnostics token or address while diagnostics is disabled.

- [ ] **Step 6: Run config/redaction tests**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^(TestLoadEffectiveRuntimePolicy|TestRenderEffectiveRedacted|TestExtensionEnv)" -count=1'`

Expected: PASS; no Go key appears under the APISIX namespace or `plugin_attr`.

- [ ] **Step 7: Commit the additive config cutover**

```bash
git add pkg/config conf/config-default.yaml conf/config-production.yaml docs/superpowers/plans/2026-08-23-{apisix-go-convergence-program,static-effective-config,supervisor-worker-platform,runtime-safety-observability}.md
git commit -m "feat(config): define runtime safety policy"
```

### Task 2: Enforce the Container and Go Memory Envelope

**Files:**

- Create: `pkg/runtime/memory_source.go`, `memory_source_linux.go`, `memory_source_other.go`
- Create: `pkg/runtime/memory_source_test.go`, `memory_source_linux_test.go`
- Create: `pkg/runtime/budget.go`, `budget_test.go`
- Modify: plan 05 worker bootstrap/status-send files and tests

**Interfaces:**

- Consumes: `config.RuntimeSafetyPolicy`, plan 04 `TaskRegistry.Go(TaskSpec, func(context.Context) error)`, plan 05 `worker.Bootstrap.Run(context.Context) error`, `lifecycle.Codec.Send(context.Context, MessageType, any) error`, `lifecycle.Status` and existing supervisor worker-exit handling.
- Produces: `MemorySource`, `BudgetManager`, `BudgetSnapshot`, `Reservation` and pressure levels above.

- [ ] **Step 1: Write failing cgroup and pressure tests**

```go
func TestBudgetManagerShedsAtSoftAndSignalsAfterSustainedHard(t *testing.T) {
	source := &fakeMemorySource{limit: 1000, current: 100}
	hard := make(chan BudgetSnapshot, 1)
	m, err := NewBudgetManager(testSafetyPolicy(1000), source, func(s BudgetSnapshot) { hard <- s })
	if err != nil { t.Fatal(err) }
	m.sample(time.Unix(1, 0)); source.current = 810; m.sample(time.Unix(2, 0))
	if !errors.Is(m.Admit(WorkHighCost), ErrSoftPressure) { t.Fatal("high-cost work admitted at soft pressure") }
	if err := m.Admit(WorkOrdinary); err != nil { t.Fatalf("ordinary work rejected: %v", err) }
	source.current = 960; m.sample(time.Unix(3, 0)); m.sample(time.Unix(34, 0))
	select { case <-hard: default: t.Fatal("hard callback not fired") }
	source.current = 100; m.sample(time.Unix(35, 0))
	if err := m.Admit(WorkHighCost); err != nil { t.Fatalf("admission did not recover: %v", err) }
}

func TestReservationConcurrentReleaseSubtractsOnce(t *testing.T) {
	m := testBudgetManager(t, map[BudgetClass]int64{BudgetLoggerMemory: 64})
	r, err := m.Reserve(BudgetLoggerMemory, 32)
	if err != nil { t.Fatal(err) }
	var wg sync.WaitGroup
	for range 8 { wg.Go(r.Release) }
	wg.Wait()
	if got := m.Snapshot().Used[BudgetLoggerMemory]; got != 0 { t.Fatalf("used = %d", got) }
}

func TestMemoryPressureStatusConsumesPlan05Reason(t *testing.T) {
	status := lifecycle.Status{State: lifecycle.StateFailed, Ready: false, Terminal: true,
		ReasonCode: lifecycle.ReasonMemoryPressureHard}
	if status.ReasonCode != lifecycle.ReasonMemoryPressureHard { t.Fatalf("reason = %q", status.ReasonCode) }
}
```

Add Linux fixtures for cgroup v2 `memory.max`/`memory.current`, v1 limit/usage, `max`, the v1 unlimited sentinel, malformed files, configured-smaller wins, reserve underflow, and non-Linux configured-only behavior.

- [ ] **Step 2: Run memory tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/runtime -run "^(TestMemorySource|TestBudgetManager)" -count=1'`

Expected: FAIL because the source and manager do not exist.

- [ ] **Step 3: Implement fail-closed envelope discovery**

On Linux parse `/proc/self/cgroup` and `/proc/self/mountinfo`, then the resolved cgroup v2 files or v1 memory controller files; never assume `/sys/fs/cgroup` is the active mount. Treat `max`, zero, negative and v1 sentinel-like values at or above `1<<60` as unbounded. Return the smaller positive configured/cgroup limit and its source string. Other platforms expose configured limit only.

- [ ] **Step 4: Implement reservations and the owned sampler**

Construct `BudgetManager.used` and both maps in its first immutable `snapshot`; guard `used`, `snapshot`, `hardSince`, `hardSignaled` and `started` with `mu`. `Snapshot` returns cloned maps. Reservations reject negative/zero-invalid requests, use checked addition, enforce a positive per-class limit when present and always enforce the effective worker limit. `Reservation.Release` uses `releaseOnce.Do` to subtract `bytes` from `class` exactly once under the manager lock. `Start` sets `started` under the same lock, rejects a second start, registers owner `runtime.memory-pressure` as `runtime.TaskCore`, and uses one ticker with no raw goroutine. Inject `setMemoryLimit func(int64) int64` in tests and call `debug.SetMemoryLimit` exactly once before worker READY.

- [ ] **Step 5: Bind hard pressure to the existing plan 05 worker run boundary**

Add `runtime.ErrSustainedHardPressure` and keep the callback private to `worker.Bootstrap.Run`. On the first sustained-hard callback, send the exact terminal status below through `Bootstrap.IPC`, cancel the worker run context, join owned tasks to the plan 05 deadline, and return `runtime.ErrSustainedHardPressure` from `Run`:

```go
status := lifecycle.Status{
	State:       lifecycle.StateFailed,
	WorkerPID:   os.Getpid(),
	Generation: activeGeneration,
	Fence:       activeFence,
	Ready:       false,
	Terminal:    true,
	ReasonCode:  lifecycle.ReasonMemoryPressureHard,
}
sendCtx, sendCancel := context.WithTimeout(context.WithoutCancel(ctx), b.Config.Runtime.Lifecycle.TerminateGrace)
defer sendCancel()
sendErr := b.IPC.Send(sendCtx, lifecycle.MessageStatus, status)
cancel(runtime.ErrSustainedHardPressure)
residuals, stopErr := tasks.Stop(drainCtx)
recordBoundedResidualOwners(residuals)
return errors.Join(runtime.ErrSustainedHardPressure, sendErr, stopErr)
```

The status send uses a context bounded by plan 05 `TerminateGrace`; `context.WithoutCancel` alone is not a deadline. The supervisor consumes the existing terminal status/process exit and applies its existing restart/probation rules. Do not add `Fatal`, `ExitWorker`, a new command/message kind, `os.Exit` in a library, a supervisor signal, or listener closure.

- [ ] **Step 6: Run memory race/platform tests**

```bash
bash -lc 'source .envrc && go test -race ./pkg/runtime -run "^(TestMemorySource|TestBudgetManager|TestReservation)" -count=1'
bash -lc 'source .envrc && GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go test ./pkg/runtime -run "^TestConfiguredMemorySource" -c -o .cache/tmp/runtime-darwin.test'
bash -lc 'source .envrc && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test ./pkg/runtime -run "^TestConfiguredMemorySource" -c -o .cache/tmp/runtime-windows.test.exe'
```

Expected: PASS; platform-neutral files import no procfs/syscall symbols.

- [ ] **Step 7: Commit memory enforcement**

```bash
git add pkg/runtime pkg/worker pkg/supervisor
git commit -m "feat(runtime): enforce worker memory envelope"
```

### Task 3: Budget Bodies, Spool, Caches, Compilation and Generations

**Files:**

- Modify: `pkg/runtime/budget.go`, `budget_test.go`
- Create: `pkg/proxy/body_budget_runtime.go`, `body_budget_runtime_test.go`
- Modify: `pkg/compiler/compiler.go`, `normalize.go`, `materialize.go`, `types.go`, tests
- Modify: `pkg/server/request_body_limit.go`, tests
- Modify: plan 06 body/spool owner and tests
- Modify: `pkg/plugin/proxy_cache/zones.go`, `disk.go`, tests
- Modify: `pkg/plugin/graphql_proxy_cache/plugin.go`, tests

**Interfaces:**

- Consumes: `BudgetManager.Reserve`, `Admit`, `compiler.PreparedGeneration.Close`, plan 06 exact `BodyDirection`, `BodyMode`, `BodyPlan`, `BodyBudget`, `MaterializeRequestBody` and `RuntimeDependencies.Resources`.
- Produces: `proxy.NewRuntimeBodyBudget(BodyPlan, *runtime.BudgetManager) BodyBudget` plus exact, non-overlapping reservation ownership for every high-cost memory/disk component.

- [ ] **Step 1: Write failing cross-component release tests**

```go
func TestPreparedGenerationReleasesCompileAndGenerationBudgetsExactlyOnce(t *testing.T) {
	manager := testBudgetManager(t, map[runtime.BudgetClass]int64{
		runtime.BudgetCompileMemory: 100, runtime.BudgetGenerationMemory: 100,
	})
	prepared, err := compileBudgetFixture(t, manager, 60, 40)
	if err != nil { t.Fatal(err) }
	if got := manager.Snapshot().Used[runtime.BudgetCompileMemory]; got != 0 { t.Fatalf("compile used = %d", got) }
	if got := manager.Snapshot().Used[runtime.BudgetGenerationMemory]; got != 40 { t.Fatalf("generation used = %d", got) }
	if err := prepared.Close(context.Background()); err != nil { t.Fatal(err) }
	_ = prepared.Close(context.Background())
	if got := manager.Snapshot().Used[runtime.BudgetGenerationMemory]; got != 0 { t.Fatalf("leaked = %d", got) }
}

func TestRuntimeBodyBudgetCombinesRequestAndWorkerLimits(t *testing.T) {
	manager := testBudgetManager(t, map[runtime.BudgetClass]int64{runtime.BudgetRequestBodyMemory: 12})
	plan := proxy.BodyPlan{Request: proxy.BodyMemory, MemoryLimit: 10}
	budget := proxy.NewRuntimeBodyBudget(plan, manager)
	release, err := budget.Reserve(context.Background(), proxy.BodyRequest, proxy.BodyMemory, 8)
	if err != nil { t.Fatal(err) }
	if _, err := budget.Reserve(context.Background(), proxy.BodyRequest, proxy.BodyMemory, 3); !errors.Is(err, proxy.ErrBodyBudgetExceeded) {
		t.Fatalf("Reserve() error = %v", err)
	}
	release()
	if got := manager.Snapshot().Used[runtime.BudgetRequestBodyMemory]; got != 0 { t.Fatalf("worker body used = %d", got) }
}
```

Add unknown-length request incremental reservation, response-direction isolation, cancellation after multiple chunks, buffered-body rejection before upstream work, spool grow/truncate/delete, cache-zone acquire/final lease release, disk quota, failed compile cleanup and overlapping generation tests. Assert `ReplayableBody.Close` and every materialization failure invoke all accumulated release closures exactly once.

- [ ] **Step 2: Run component tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/runtime ./pkg/compiler ./pkg/server ./pkg/proxy ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache -run "(Budget|Reservation|Body|Spool|Zone|PreparedGeneration)" -count=1'`

Expected: FAIL because owners do not reserve capacity.

- [ ] **Step 3: Reserve body and spool bytes at ownership boundaries**

`proxy.NewRuntimeBodyBudget` constructs one request-local adapter; `pkg/runtime` never imports `pkg/proxy`. `Reserve` checks context, direction/mode, positive bytes and checked-addition against the `BodyPlan` before calling the worker manager; if the worker reservation fails it rolls back the request counter. `BodyMemory` uses `BudgetRequestBodyMemory`, `BodySpool` uses `BudgetSpoolDisk`, and `BodyStream` returns a no-op release. Content-Length pre-reserves only when buffering is compiled; unknown-length bodies reserve each read chunk before retaining it. Memory-to-spool transfer acquires disk before releasing memory. Every error/cancel/413 path closes input/file, removes partial spool and invokes all previously accumulated release closures. Soft pressure rejects a new buffering/spooling operation with the existing stable APISIX 503/413 boundary selected by the compiled body plan; it never truncates a live stream.

- [ ] **Step 4: Reserve cache definitions and entries**

Reserve declared memory-zone capacity once per digest-keyed shared zone lease and disk bytes per persisted envelope. An insertion acquires growth before publishing the entry; quota failure runs the existing eviction policy and retries once, then skips caching without failing proxy traffic. PURGE, expiration, replacement and final lease close release exact bytes.

- [ ] **Step 5: Separate compile peak from retained generation bytes**

Reserve canonical input plus normalized working bytes under `BudgetCompileMemory` before materialization. Release that reservation on every return. Before publication, reserve defensive-copy snapshots, compiled indexes and generation-private buffers under `BudgetGenerationMemory`; store that lease on `PreparedGeneration` after task/resource leases and release it only during `Close`. A failed new generation never charges the predecessor.

- [ ] **Step 6: Run component race tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/runtime ./pkg/compiler ./pkg/server ./pkg/proxy ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache -run "(Budget|Reservation|Body|Spool|Cache|PreparedGeneration)" -count=1'`

Expected: PASS with zero leaked bytes after failures and generation drain.

- [ ] **Step 7: Commit component budgets**

```bash
git add pkg/runtime pkg/compiler pkg/server pkg/proxy pkg/plugin/proxy_cache pkg/plugin/graphql_proxy_cache
git commit -m "feat(runtime): budget high-cost component state"
```

### Task 4: Separate APISIX Compatibility Limits From Strict Admission

**Files:**

- Create: `pkg/runtime/admission.go`, `admission_test.go`
- Modify: `pkg/config/defaults.go`, `validation.go`, tests
- Modify: `pkg/proxy/cluster.go`, tests
- Modify: plan 05 worker listener/server construction and tests
- Modify: `pkg/server/server.go`, tests

**Interfaces:**

- Consumes: `config.ProfileSelection`, APISIX `nginx_config.event.worker_connections`, strict `AdmissionPolicy`, `BudgetManager`.
- Produces: exact `AdmissionController`; no implicit upstream or request ceiling in compatibility mode.

- [ ] **Step 1: Write failing compatibility/strict matrix tests**

```go
func TestAdmissionProfileMatrix(t *testing.T) {
	compat := newAdmissionFixture(t, config.SecurityCompat, config.AdmissionPolicy{})
	for i := 0; i < 2048; i++ { release, err := compat.BeginHighCost(); if err != nil { t.Fatal(err) }; release() }
	strict := newAdmissionFixture(t, config.SecurityStrict, config.AdmissionPolicy{MaxHighCostRequests: 1})
	release, err := strict.BeginHighCost(); if err != nil { t.Fatal(err) }; defer release()
	if _, err := strict.BeginHighCost(); !errors.Is(err, runtime.ErrAdmissionLimited) { t.Fatalf("second = %v", err) }
}
```

Add tests that `proxy.max_in_flight` absent/zero yields no cluster semaphore, an explicit positive APISIX value is honored, `worker_connections` limits accepted connections in compatibility mode, strict active-request overload returns stable 503 before response commit, and soft pressure rejects only `BeginHighCost`.

- [ ] **Step 2: Run admission tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/runtime ./pkg/proxy ./pkg/server -run "(Admission|MaxInFlight|WorkerConnections|Overload)" -count=1'`

Expected: FAIL because current defaults install `1024` and no profile-aware admission owner exists.

- [ ] **Step 3: Remove the hidden 1024 path atomically**

Delete `proxy.max_in_flight` builtin/default YAML value and any `if zero then 1024` cluster branch. `MaxInFlight > 0` remains an explicit Go extension already present in configuration and creates the cluster gate; zero creates none. Update docs and tests in the same commit; do not keep a legacy default constant.

- [ ] **Step 4: Implement compatibility connection and strict request gates**

`NewAdmissionController` stores defensive value copies of `profile`, `apisix` and `policy`, requires a non-nil manager, and initializes all three atomic counters to zero. The listener wrapper maps positive `worker_connections` to concurrent accepted connections for the single Go worker and releases on connection close. Strict `MaxActiveConnections` can lower, never raise, that explicit APISIX limit. `WrapHTTP` releases `activeRequests` on handler completion/hijack ownership transfer; `BeginHighCost` increments with compare-and-swap and returns a `sync.Once`-guarded decrement closure. Rejection records a bounded reason and uses stable 503 with `Retry-After: 1` only before commit; hijacked/committed paths close the connection.

- [ ] **Step 5: Run admission race/build tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/runtime ./pkg/proxy ./pkg/server -run "(Admission|MaxInFlight|WorkerConnections|Overload)" -count=1 && make build'`

Expected: PASS; compatibility has no unseen 1024 cap.

- [ ] **Step 6: Commit admission cutover**

```bash
git add pkg/config pkg/runtime pkg/proxy pkg/server pkg/worker conf docs/configuration.md
git commit -m "refactor(runtime): make admission limits explicit"
```

### Task 5: Make Every Async Logger Byte-Bounded and Generation-Owned

**Files:**

- Modify: `pkg/plugin/logger_batch/processor.go`, `processor_test.go`
- Modify: `pkg/plugin/file_logger/processor.go`, `writer_registry.go`, tests
- Modify: `pkg/plugin/base/types.go`, tests
- Modify: every production logger constructor returned by `rg -l 'NewBatchProcessor|logger_batch.New' pkg/plugin --glob '*.go' --glob '!*_test.go'`
- Modify: `pkg/observability/metrics/logger_batch.go`, tests

**Interfaces:**

- Consumes: plan 04 generation `TaskRegistry`, `BudgetLoggerMemory`, plugin `InstanceKey`, request finalizers.
- Produces: bounded `logger_batch.Processor` with explicit byte sizing, owned delivery tasks and shutdown evidence.

- [ ] **Step 1: Write failing byte/deadline/shutdown tests**

```go
func TestProcessorBoundsBytesAndFlushesWithinShutdownDeadline(t *testing.T) {
	delivered := make(chan int, 1)
	p := NewWithContext(Config{Name: "http", PluginID: "http-logger", MaxPendingEntries: 10,
		MaxPendingBytes: 8, DeliveryTimeout: time.Second, ShutdownTimeout: time.Second,
		Tasks: testTaskRegistry(t), Owner: "plugin/http-logger/route-r1"},
		func(_ context.Context, entries []map[string]any, _ int) (int, error) { delivered <- len(entries); return len(entries), nil })
	if !p.PushSized(map[string]any{"v": "1234"}, 4) { t.Fatal("first push rejected") }
	if p.PushSized(map[string]any{"v": "56789"}, 5) { t.Fatal("byte-over-budget push accepted") }
	if err := p.Shutdown(context.Background()); err != nil { t.Fatal(err) }
	if <-delivered != 1 { t.Fatal("accepted entry not attempted") }
}
```

Add retry cancellation, delivery timeout, queue/full/stopped/drop reasons, serialization-size mismatch, concurrent flush, uncooperative callback residual owner, file writer bytes, one-second sync and final lease flush tests.

- [ ] **Step 2: Run logger tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/logger_batch ./pkg/plugin/file_logger -run "(Bytes|Deadline|Retry|Shutdown|Flush|Task)" -count=1'`

Expected: FAIL because queues are entry-bounded and start raw goroutines.

- [ ] **Step 3: Add exact logger ownership fields**

Extend `logger_batch.Config` with `MaxPendingBytes int64`, `Tasks *runtime.TaskRegistry`, `Owner string`, and `Budget *runtime.BudgetManager`; require all four in production constructors. `PushSized(entry, encodedBytes)` reserves bytes before acceptance and releases them only on delivery/drop. Existing `Push` serializes once through the sink's canonical encoder to obtain an exact size; it is not retained as an unmeasured fallback.

- [ ] **Step 4: Replace every logger goroutine with named tasks**

Register workers, retry timers, shutdown barrier, cleanup and file flush loop through the generation task registry using `TaskPlugin` and owner suffixes `/delivery-N`, `/shutdown`, `/file-flush`. Remove `context.Background()` delivery roots. A delivery panic is reported as the plugin owner failure and drops that terminal batch; it never becomes a core panic.

- [ ] **Step 5: Preserve bounded delivery semantics**

Each attempt has `DeliveryTimeout`; retry count means retries after the first attempt; delay is cancellable; terminal failure releases bytes and increments one bounded drop reason. Shutdown refuses new pushes, seals the buffer, attempts queued work until the caller deadline, cancels tasks, returns residual owners, and releases sink resources only after callbacks join. File logger's shared 64 KiB writer budget is reserved once per canonical-path lease.

- [ ] **Step 6: Run affected logger race tests**

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/logger_batch ./pkg/plugin/file_logger ./pkg/observability/metrics -run "(LoggerBatch|Processor|Writer|Bytes|Shutdown|Flush|Drop)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/http_logger ./pkg/plugin/kafka_logger ./pkg/plugin/clickhouse_logger ./pkg/plugin/elasticsearch_logger ./pkg/plugin/sls_logger ./pkg/plugin/tcp_logger ./pkg/plugin/udp_logger ./pkg/plugin/syslog -run "(Batch|PostInit|Stop|Retry|Metadata)" -count=1'
```

Expected: PASS; accepted bytes plus active bytes never exceed the configured logger budget.

- [ ] **Step 7: Commit logger ownership**

```bash
git add pkg/plugin pkg/observability/metrics/logger_batch.go pkg/observability/metrics/logger_batch_test.go
git commit -m "refactor(logger): bound and own asynchronous delivery"
```

### Task 6: Add Bounded Worker Telemetry Batches Over Existing IPC

**Files:**

- Create: `pkg/telemetry/types.go`, `reporter.go`, `reporter_test.go`
- Modify: plan 05 IPC message/codec/worker endpoint files and tests
- Modify: plan 05 worker bootstrap to construct one reporter

**Interfaces:**

- Consumes: plan 05 `lifecycle.Codec`, `MessageTelemetry`, `WorkerTelemetry`, `WorkerTelemetrySink`, authenticated `lifecycle.Status` and plan 04 worker `TaskRegistry`.
- Produces: `Batch`, `BatchSender`, `Reporter` and `lifecycle.MessageMetricBatch`; existing `MessageTelemetry` and `WorkerTelemetry` remain unchanged.

- [ ] **Step 1: Write failing coalescing/frame tests**

```go
func TestReporterCoalescesWithoutBlockingAndBoundsFrame(t *testing.T) {
	sender := &captureSender{}
	reporter, err := NewReporter(config.TelemetryPolicy{WorkerQueueBytes: 4096, MaxFrameBytes: 1024,
		FlushInterval: time.Second, MaxTotalSeries: 10, GenerationSeriesTTL: time.Minute}, 7, sender, testTaskRegistry(t))
	if err != nil { t.Fatal(err) }
	for i := 0; i < 100; i++ { reporter.AddCounter("apisix_http_requests_total", nil, 1) }
	if err := reporter.Flush(context.Background()); err != nil { t.Fatal(err) }
	if got := sender.last.Points[0].Counter; got != 100 { t.Fatalf("delta = %d", got) }
	if encodedSize(sender.last) > 1024 { t.Fatalf("frame too large") }
}
```

Add queue overflow, stable existing-key updates at capacity, histogram bucket merge, gauge last-write, failed-send retention without double count, sequence, malformed inbound, unsupported protocol version and worker-generation fence tests.

- [ ] **Step 2: Run reporter/IPC tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/telemetry ./pkg/supervisor ./pkg/worker -run "(Reporter|Telemetry|IPC|Frame|Sequence)" -count=1'`

Expected: FAIL because reporter and telemetry message are absent.

- [ ] **Step 3: Implement a canonical bounded batch encoding**

`NewReporter` validates policy/generation/sender/tasks, initializes `points`, and stores no process-global state. All request-side mutations of `points`, `queueBytes`, `sequence`, `dropped` and `closed` use `mu`; `Close` stores one result through `closeOnce`. Sort points by name/kind/canonical labels and labels by name. Validate finite gauges/sums, strictly increasing finite histogram bounds, `len(Buckets)==len(Bounds)+1`, bounded UTF-8 names/labels and the configured frame size before sending. Do not serialize exemplars, arbitrary errors, request IDs, URIs, secret values or stack traces.

- [ ] **Step 4: Extend the existing protocol once**

Add exactly one message constant beside plan 05's existing `MessageTelemetry`:

```go
const MessageMetricBatch MessageType = "metric_batch"

type IPCBatchSender struct { codec *lifecycle.Codec }

func (s *IPCBatchSender) SendTelemetry(ctx context.Context, batch Batch) error {
	return s.codec.Send(ctx, lifecycle.MessageMetricBatch, batch)
}
```

Do not rename or repurpose `MessageTelemetry`: it continues to carry exact `lifecycle.WorkerTelemetry`. Do not change `Frame`, `Codec.Send`, `Codec.Receive`, `WorkerTelemetry` or `WorkerTelemetrySink`. The supervisor dispatch branch obtains `status := peer.Status()` from the authenticated plan 05 peer and calls `aggregator.Apply(status, batch)`; it rejects a batch whose generation differs from `status.Generation` and ignores any worker identity supplied by payload. No second socket, JSON side channel or protocol adapter is added.

- [ ] **Step 5: Own periodic flush through TaskRegistry**

Register `telemetry.flush` as a worker `TaskCore`; it uses one ticker and bounded send context. Request record methods never perform IPC. On shutdown, worker READY is withdrawn first, then `Reporter.Close` flushes once within the plan 05 drain deadline and reports unsent deltas as dropped.

- [ ] **Step 6: Run reporter/IPC race tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/telemetry ./pkg/supervisor ./pkg/worker -run "(Reporter|Telemetry|IPC|Frame|Sequence|Drain)" -count=1'`

Expected: PASS with bounded memory during a blocked supervisor reader.

- [ ] **Step 7: Commit telemetry IPC**

```bash
git add pkg/telemetry pkg/supervisor pkg/worker
git commit -m "feat(telemetry): aggregate bounded worker deltas"
```

### Task 7: Make the Supervisor Aggregator the Only Metrics Owner

**Files:**

- Create: `pkg/telemetry/aggregator.go`, `aggregator_test.go`, `cardinality.go`, `cardinality_test.go`, `prometheus.go`, `prometheus_test.go`
- Modify: `pkg/observability/metrics/*.go`, tests
- Modify: `pkg/plugin/prometheus/plugin.go`, `pkg/route/extra.go`, tests
- Delete: `pkg/observability/metrics/expiration_runtime.go`, tests after equivalent aggregator coverage
- Modify: plan 05 supervisor state/metrics listener and IPC dispatch files

**Interfaces:**

- Consumes: `telemetry.Batch`, exact plan 05 `lifecycle.StatusProvider`, `lifecycle.Status`, existing APISIX Prometheus config, and worker lifecycle notifications.
- Produces: `Aggregator`, stable Prometheus `Handler`, monotonic counter/histogram ownership and bounded series lifecycle.

- [ ] **Step 1: Write failing generation-continuity and TTL tests**

```go
func TestAggregatorKeepsStableCountersAcrossWorkerReplacement(t *testing.T) {
	a := newTestAggregator(t, 2, time.Minute)
	first := lifecycle.Status{State: lifecycle.StateActive, WorkerPID: 101, Generation: 1, Ready: true}
	second := lifecycle.Status{State: lifecycle.StateActive, WorkerPID: 202, Generation: 2, Ready: true}
	applyCounter(t, a, first, 1, "apisix_http_requests_total", 7)
	a.RetireWorker(first)
	applyCounter(t, a, second, 1, "apisix_http_requests_total", 5)
	if got := scrapeCounter(t, a.Handler(), "apisix_http_requests_total", nil); got != 12 { t.Fatalf("counter = %v", got) }
}
```

Add histogram bucket continuity, duplicate sequence, immediate worker/generation gauge removal, counter/histogram TTL after retirement, active series pinning, per-family/global overflow, bounded-control-label preservation, arbitrary route/consumer overflow and concurrent Apply/Expire/Gather tests.

- [ ] **Step 2: Run aggregator tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/telemetry -run "^(TestAggregator|TestCardinality|TestPrometheus)" -count=1'`

Expected: FAIL because the supervisor aggregator does not exist.

- [ ] **Step 3: Implement stable and generation-detail stores**

`NewAggregator` requires non-nil registry/status provider, initializes `stable`, `generation`, `gauges`, `lastSequences` and `cardinality.perFamily`, and builds `handler` from that private registry. `Apply`, `RetireWorker` and `Expire` mutate these fields under `mu`; gather copies a read snapshot under `RLock`. Use canonical `(name, kind, sorted labels)` keys. Stable APISIX request/logger/proxy counters and histograms exclude worker/generation and never expire while the supervisor lives. Generation-detail diagnostic counters/histograms carry bounded generation state and expire after TTL. Gauges are stored per worker and summed at collection; `RetireWorker` deletes them immediately. Never delete and recreate a stable counter/histogram during route reload.

- [ ] **Step 4: Enforce cardinality before collector creation**

Preserve fixed control labels (`code`, latency/bandwidth `type`, bounded outcome/reason) and replace only unbounded route/service/consumer/node/host/custom values with `__overflow__`. Apply existing `plugin_attr.prometheus.max_http_series`, `max_llm_series` and per-family `expire`, plus `TelemetryPolicy.MaxTotalSeries`. Increment a fixed `{family,reason}` overflow counter; invalid labels increment a fixed rejection counter and create no child.

- [ ] **Step 5: Replace global metrics calls with injected reporter/aggregator views**

Worker request/plugin/proxy owners receive a `*telemetry.Reporter` through their worker/generation construction; no package variable collector remains. Supervisor lifecycle/config/readiness events update the aggregator directly from authoritative state transitions. The Prometheus plugin controls APISIX request-family recording and export URI compatibility, but serving the handler belongs to the supervisor stable listener.

- [ ] **Step 6: Delete old metric owners atomically**

Delete global collector variables, `sync.Once`, readiness state mutex, expiration runtime, worker export server and zero-argument record facades after all callers switch. Do not keep reset hooks or test-only globals. Move useful label/schema tests to `pkg/telemetry`.

- [ ] **Step 7: Run aggregation race tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/telemetry ./pkg/observability/metrics ./pkg/plugin/prometheus ./pkg/server ./pkg/supervisor -run "(Aggregator|Cardinality|Series|Prometheus|Readiness|Generation|Metric)" -count=1'`

Expected: PASS; scrape values never decrease across a successful worker replacement.

- [ ] **Step 8: Commit the metrics-owner cutover**

```bash
git add pkg/telemetry pkg/observability/metrics pkg/plugin/prometheus pkg/route pkg/server pkg/supervisor
git commit -m "refactor(metrics): move stable aggregation to supervisor"
```

### Task 8: Project Health and Metrics From One Supervisor State Machine

**Files:**

- Modify: plan 05 lifecycle state/status and stable listener files plus tests
- Modify: `pkg/telemetry/prometheus.go`, tests
- Modify: `pkg/server/server.go`, health tests
- Delete: `pkg/observability/metrics/config_apply.go`, tests after equivalent supervisor-state tests

**Interfaces:**

- Consumes: exact plan 05 `lifecycle.StatusProvider.Status() lifecycle.Status`, `lifecycle.Status`, `lifecycle.RevisionFence` and `lifecycle.AuditSink`.
- Produces: `/livez`, `/readyz`, lifecycle/config/readiness metrics and audit transitions from the same immutable supervisor snapshot.

- [ ] **Step 1: Write a failing projection agreement test**

```go
func TestHealthAndMetricsProjectTheSameSupervisorSnapshot(t *testing.T) {
	provider := &fakeStatusProvider{status: lifecycle.Status{
		State: lifecycle.StateActive, WorkerPID: 101, Generation: 7,
		Fence: lifecycle.RevisionFence{Desired: 9, HTTP: 9, Stream: 8},
		Ready: false, Terminal: false, ReasonCode: lifecycle.ReasonNoHealthyGeneration,
	}}
	server := newStableEndpointFixture(t, provider)
	ready := getJSON(t, server.URL+"/readyz")
	if ready.Status != http.StatusServiceUnavailable { t.Fatalf("ready = %d", ready.Status) }
	if got := scrapeGauge(t, server.URL+"/metrics", "apisix_supervisor_ready", nil); got != 0 { t.Fatalf("gauge = %v", got) }
}
```

Cover offline last-good serving/not-ready, HTTP and stream independent fence revisions, provider recovery already encoded by a new `Status`, worker probation/rollback/crash terminal state, pressure-shed versus hard exit, exporter/diagnostics failure not changing `Status.Ready`, and concurrent `Status()` reads. Tests must not add `WithProviderHealthy`, `RevisionSet` or another state holder absent from plan 05.

- [ ] **Step 2: Run state projection tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/supervisor ./pkg/telemetry ./pkg/server -run "(HealthAndMetrics|Readiness|LifecycleState)" -count=1'`

Expected: FAIL while worker metrics state remains an independent readiness owner.

- [ ] **Step 3: Define one pure projection**

Add pure `ProjectHealth(lifecycle.Status) HealthDocument` and `ProjectHealthMetrics(lifecycle.Status) []telemetry.Point` functions. Liveness is true exactly when the supervisor control loop is running and `Status.Terminal` is false. Readiness equals `Status.Ready`; it is never recomputed from collectors, exporters or a second provider flag. Expose only bounded state/reason, PID/generation and `Status.Fence` desired/HTTP/stream revisions. Plan 05 owns transitions that combine provider state and plan 03 required domains into `Status.Ready`/`ReasonCode`. Soft shedding is visible as a bounded metric but does not mutate an already-serving status; the sustained-hard terminal status makes readiness false.

- [ ] **Step 4: Delete the worker readiness truth**

Remove `metrics.GetReadiness`, config-apply stage recorders and server-local `/livez`/`/readyz`. Worker/provider messages only drive validated supervisor transitions; endpoints and collectors read snapshots. No metric collector read-back participates in readiness.

- [ ] **Step 5: Run state/race tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/supervisor ./pkg/telemetry ./pkg/server ./pkg/generation -run "(HealthAndMetrics|Readiness|Lifecycle|Offline|Revision|Pressure)" -count=1'`

Expected: PASS; every tested state yields matching JSON and metric readiness.

- [ ] **Step 6: Commit unified state projection**

```bash
git add pkg/supervisor pkg/telemetry pkg/server pkg/observability/metrics pkg/generation
git commit -m "refactor(runtime): unify health and metric state"
```

### Task 9: Make OpenTelemetry a Generation-Owned Optional Resource

**Files:**

- Modify: `pkg/plugin/otel/plugin.go`, `provider.go`, tests
- Modify: `pkg/compiler/materialize.go`, tests
- Modify: `cmd/root.go`
- Delete: `pkg/observability/otel/init.go`, `init_test.go`
- Modify: `conf/config-default.yaml`, OTel docs

**Interfaces:**

- Consumes: plan 04 `RuntimeDependencies.Resources`, `Tasks`, plugin effective `InstanceKey`, manifest descriptor and scoped secret materializer.
- Produces: digest-keyed generation lease owning tracer provider, OTLP exporter, sampler, propagators and shutdown.

- [ ] **Step 1: Write failing disabled/ownership tests**

```go
func TestOTelDisabledCreatesNoExporterAndDoesNotInstallGlobals(t *testing.T) {
	beforeProvider := otel.GetTracerProvider()
	beforePropagator := otel.GetTextMapPropagator()
	resource, err := buildGenerationTelemetry(context.Background(), disabledFixture(), testDependencies(t))
	if err != nil { t.Fatal(err) }
	if resource != nil { t.Fatal("disabled telemetry created a resource") }
	if otel.GetTracerProvider() != beforeProvider || otel.GetTextMapPropagator() != beforePropagator { t.Fatal("global OTel state changed") }
}
```

Add explicit `always_on`, ratio and parent-based sampler tests; propagator order; bounded queue/drop; exporter timeout; equal-config resource reuse; different-config isolation; failed prepare cleanup; overlapping generations; shutdown deadline; and no stdout writes.

- [ ] **Step 2: Run OTel tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/otel ./pkg/compiler ./cmd -run "(OTel|OpenTelemetry|TracerProvider|Exporter|Sampler)" -count=1'`

Expected: FAIL because process startup installs stdout/AlwaysSample globals and provider code reads global metadata.

- [ ] **Step 3: Build provider only from compiled effective plugin input**

Absence of the `opentelemetry` capability returns no resource. Configured metadata/plugin config is normalized/materialized before resource construction. Support only declared propagators with deterministic order. Missing sampler defaults to `NeverSample`; explicit APISIX `always_on` maps to `AlwaysSample`, ratio is range-checked, parent-based recursively validates its root.

- [ ] **Step 4: Acquire a bounded provider resource**

Key the resource by provider config, materialized header digests, sampler, propagators and APISIX scope. Use only OTLP HTTP exporter with request timeout and bounded batch queue/size; request recording never blocks when the queue is full. The close function force-flushes within its deadline then shuts down provider/exporter through generation tasks.

- [ ] **Step 5: Remove every global/default OTel path**

Delete the blank import and entire `pkg/observability/otel`. Remove `otel.SetTracerProvider`, `SetTextMapPropagator`, store/global metadata reads and stdout exporter import. Plugin request hooks use their owned provider/tracer and propagator explicitly.

- [ ] **Step 6: Run OTel race and symbol gates**

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/otel ./pkg/compiler ./pkg/runtime ./cmd -run "(OTel|OpenTelemetry|TracerProvider|Exporter|Sampler|Generation)" -count=1'
test ! -d pkg/observability/otel
! rg -n 'stdouttrace|SetTracerProvider|SetTextMapPropagator|observability/otel' cmd pkg --glob '*.go'
rg -n 'AlwaysSample\(\)' pkg/plugin/otel --glob '*.go'
```

Expected: PASS. The final `rg` has exactly one production result in the explicit parsed `always_on` switch branch; unit tests prove missing sampler selects `NeverSample` and no other branch reaches `AlwaysSample`.

- [ ] **Step 7: Commit OTel ownership**

```bash
git add cmd pkg/plugin/otel pkg/compiler pkg/observability/otel conf docs
git commit -m "refactor(otel): own tracing by generation"
```

### Task 10: Add a Separate Authenticated and Audited Diagnostics Listener

**Files:**

- Create: `pkg/diagnostics/server.go`, `server_test.go`, `audit.go`, `audit_test.go`
- Modify: plan 05 supervisor listener/audit/shutdown files and tests
- Modify: `pkg/config/runtime_policy.go`, tests

**Interfaces:**

- Consumes: `config.DiagnosticsPolicy`, exact plan 05 `lifecycle.StatusProvider`, `lifecycle.AuditSink`, `lifecycle.Event` and stable listener ownership.
- Produces: `diagnostics.NewServer(config.DiagnosticsPolicy, lifecycle.StatusProvider, lifecycle.AuditSink) (*Server, error)`, `(*Server).Serve(net.Listener) error`, `(*Server).Shutdown(context.Context) error`.

```go
// package diagnostics
type Server struct {
	policy       config.DiagnosticsPolicy
	status       lifecycle.StatusProvider
	audit        lifecycle.AuditSink
	httpServer   *http.Server
	shutdownOnce sync.Once
	shutdownErr  error
}
```

- [ ] **Step 1: Write failing disabled/auth/audit tests**

```go
func TestDiagnosticsRequiresAuthAndAuditsSuccessfulProfile(t *testing.T) {
	audit := &captureAuditSink{}
	status := &fakeStatusProvider{status: lifecycle.Status{State: lifecycle.StateActive, Ready: true}}
	server := startDiagnosticsFixture(t, status, audit)
	if got := getStatus(t, server.URL+"/debug/pprof/goroutine", ""); got != http.StatusUnauthorized { t.Fatalf("unauth = %d", got) }
	if got := getStatus(t, server.URL+"/debug/pprof/goroutine", "Bearer "+testToken); got != http.StatusOK { t.Fatalf("auth = %d", got) }
	want := lifecycle.Event{CommandID: "diagnostics/goroutine", From: lifecycle.StateActive,
		To: lifecycle.StateActive, ReasonCode: "diagnostics-profile-success"}
	if len(audit.events) != 1 || !equalEventIgnoringTime(audit.events[0], want) { t.Fatalf("audit = %#v", audit.events) }
}
```

Add disabled-no-bind, data-plane/public/metrics path isolation, constant-time auth, wrong scheme, concurrency 429, profile duration clamp, cancellation, response headers, no token/profile/query/header/stack/error bytes in audit, bind failure, shutdown and diagnostics-failure-readiness-independence tests. The sink method returns no error by plan 05 contract; tests must not invent an audit-write failure response path.

- [ ] **Step 2: Run diagnostics tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/diagnostics ./pkg/supervisor -run "(Diagnostics|Profile|Audit)" -count=1'`

Expected: FAIL because the package and separate listener do not exist.

- [ ] **Step 3: Build an explicit private mux**

Import `net/http/pprof` by named functions; do not import it for side effects or use `http.DefaultServeMux`. Register only index/cmdline/profile/symbol/trace and a bounded redacted runtime snapshot. Wrap every endpoint with bearer auth using `subtle.ConstantTimeCompare`, concurrency admission and server timeouts. Require a literal loopback address in compat/strict unless a later accepted ADR adds authenticated remote diagnostics.

- [ ] **Step 4: Audit sensitive operations through the exact lifecycle sink**

After every authenticated diagnostics operation, call the exact void-returning plan 05 method:

```go
status := statusProvider.Status()
audit.RecordLifecycle(lifecycle.Event{
	At:         clock.Now(),
	CommandID:  boundedDiagnosticsOperation(r.URL.Path),
	From:       status.State,
	To:         status.State,
	ReasonCode: boundedDiagnosticsOutcome(statusCode),
})
```

`CommandID` is one of the fixed values `diagnostics/index`, `diagnostics/cmdline`, `diagnostics/profile`, `diagnostics/symbol`, `diagnostics/trace`, `diagnostics/goroutine`, or `diagnostics/runtime`; `ReasonCode` is one of `diagnostics-profile-success`, `diagnostics-profile-rejected`, or `diagnostics-profile-canceled`. Successful bearer authentication implies the single fixed diagnostics principal; the lifecycle event has no principal field, so do not encode one into a free-form string. Never record token, remote address, query string, headers, profile bytes, stack text, duration, status text or arbitrary errors. Because `AuditSink.RecordLifecycle(Event)` returns no error, diagnostics has no audit-failure 503 branch and this plan does not wrap or replace the interface.

- [ ] **Step 5: Bind and stop through plan 05 ownership**

Supervisor constructs `diagnostics.NewServer(policy, supervisor, auditSink)` and binds the separate listener only when enabled, after lifecycle audit is available and before announcing operational endpoints. Shutdown stops accepting diagnostics first and waits only to the configured profile/write deadline. The listener is never inherited by workers.

- [ ] **Step 6: Run diagnostics race tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/diagnostics ./pkg/supervisor -run "(Diagnostics|Profile|Audit|Shutdown|Readiness)" -count=1'`

Expected: PASS with no endpoint on the data-plane/public/health/metrics mux.

- [ ] **Step 7: Commit diagnostics boundary**

```bash
git add pkg/diagnostics pkg/supervisor pkg/config
git commit -m "feat(diagnostics): add authenticated audited listener"
```

### Task 11: Prove Task, Panic, Abort and Finalizer Safety End to End

**Files:**

- Modify: `pkg/server/route_handler.go`, `route_handler_test.go`
- Modify: `pkg/plugin/panic.go`, `panic_test.go`
- Modify: `pkg/apisix/ctx/lifecycle.go`, `lifecycle_test.go`
- Modify: `pkg/runtime/goroutine_contract_test.go`, `task_registry_test.go`
- Modify: `pkg/telemetry/reporter.go`, tests
- Modify: plan 05 worker exit/drain tests

**Interfaces:**

- Consumes: plan 04 `TaskRegistry`, `RequestTaskGroup`, `plugin.PanicError`, exactly-once finalizer and plan 05 worker fatal/drain behavior.
- Produces: bounded observations without weakening the already-fixed panic and ownership boundary.

- [ ] **Step 1: Write failing end-to-end panic matrix tests**

```go
func TestPluginAndCorePanicMatrixPreservesResponseAndFinalizers(t *testing.T) {
	for _, stage := range []string{"pre_commit", "post_commit", "post_flush", "post_hijack"} {
		t.Run("plugin/"+stage, func(t *testing.T) { assertPluginPanicBoundary(t, stage, true, false) })
		t.Run("core/"+stage, func(t *testing.T) { assertCorePanicBoundary(t, stage, true, true) })
	}
}
```

Each helper asserts finalizers run once in reverse order; plugin pre-commit writes one stable 500; plugin post-commit/flush/hijack closes only that connection with no second header/body; core panic re-panics after finalizers and causes the worker fatal path; finalizer panics do not skip later finalizers; telemetry labels contain only bounded owner class/stage.

- [ ] **Step 2: Run panic/ownership tests and confirm RED if observability changes regress behavior**

Run: `bash -lc 'source .envrc && go test ./pkg/server ./pkg/plugin ./pkg/apisix/ctx ./pkg/runtime ./pkg/worker -run "(PanicMatrix|Finalizer|TaskRegistry|Goroutine|Fatal)" -count=1'`

Expected: RED until reporter/worker integration preserves the plan 04 semantics for every stage.

- [ ] **Step 3: Emit telemetry only after lifecycle outcome is final**

Record the bounded panic/finalizer/task outcome after `lifecycle.Complete` and `Finalize`, before re-panicking core values. Reporter failure/drop is ignored by response logic. Do not serialize panic values, errors, owners containing resource IDs or stacks into metric labels; detailed stack stays only in worker logs/lifecycle audit policy.

- [ ] **Step 4: Extend the production goroutine AST gate**

Scan `pkg/runtime`, `pkg/telemetry`, `pkg/diagnostics`, `pkg/observability`, logger packages and plan 05 supervisor/worker packages. Permit goroutine creation only inside the canonical `TaskRegistry` implementation and plan 05 process supervision primitive; no filename/function allowlist for feature code.

- [ ] **Step 5: Verify bounded join and hard exit residual reporting**

Add one logger exporter and one telemetry sender that ignore cancellation. Worker drain returns both exact bounded owner names at deadline; supervisor terminates that worker, retains monotonic aggregates already received, removes gauges, and keeps the new healthy generation active.

- [ ] **Step 6: Run the panic/task race gate**

Run: `bash -lc 'source .envrc && go test -race ./pkg/runtime ./pkg/telemetry ./pkg/diagnostics ./pkg/plugin ./pkg/server ./pkg/apisix/ctx ./pkg/worker ./pkg/supervisor -run "(Panic|Finalizer|Task|Ownership|Abort|Drain|Fatal)" -count=1'`

Expected: PASS with no race and no second response.

- [ ] **Step 7: Commit safety integration**

```bash
git add pkg/runtime pkg/telemetry pkg/diagnostics pkg/plugin pkg/server pkg/apisix/ctx pkg/worker pkg/supervisor
git commit -m "test(runtime): enforce panic and task safety"
```

### Task 12: Produce Capacity, Overload and Recovery Evidence and Remove Legacy Paths

**Files:**

- Create: `scripts/runtime_capacity.sh`, `scripts/runtime_capacity_test.sh`
- Modify: `Makefile`
- Create: `docs/architecture/runtime-safety-observability.md`
- Modify: `docs/configuration.md`, `docs/production-profile.md`, `docs/design.md`
- Verify/delete: legacy metrics/OTel/readiness/logger/task symbols listed above

**Interfaces:**

- Consumes: all Tasks 1–11, plan 05 executable supervisor/worker commands and immutable artifact identity.
- Produces: capacity evidence under ignored `.cache/capacity`, operator contract and qualified interfaces `runtime.BudgetManager`, `telemetry.Aggregator`, `diagnostics.Server` for plan 08.

- [ ] **Step 1: Write the fail-closed runner contract test**

The shell test must assert: explicit artifact path/digest, cgroup v2 memory limit, unique output directory, no `latest` tag, workload duration/concurrency, baseline and pressure phases, worker PID/generation transitions, metrics snapshots, logger sink mode, cleanup trap, and nonzero exit when any phase/evidence file is missing.

Run: `bash scripts/runtime_capacity_test.sh`

Expected: FAIL because runner/Make targets are absent.

- [ ] **Step 2: Implement exact capacity phases**

`scripts/runtime_capacity.sh` writes metadata plus machine-readable phase results for: below-soft normal traffic; soft high-cost shed with ordinary health/streaming traffic alive; recovery below soft; stalled logger exporter with bounded queue/drop; compile plus two-generation overlap; sustained hard pressure worker exit and supervisor replacement/rollback; post-replacement monotonic counter/histogram comparison; diagnostics isolation. Each phase has a deadline and records peak RSS/cgroup usage, reservations, response counts, drops, revisions, worker IDs and residual tasks.

- [ ] **Step 3: Add Make targets without claiming local Docker evidence**

```make
.PHONY: runtime-capacity-contract
runtime-capacity-contract:
	bash scripts/runtime_capacity_test.sh

.PHONY: runtime-capacity
runtime-capacity:
	bash scripts/runtime_capacity.sh
```

The real target requires a Linux cgroup v2 host and an immutable `APISIX_IMAGE` digest; local unit tests are not capacity qualification.

- [ ] **Step 4: Document the final contract**

Document envelope selection/reserves/percentages, all component budget units and release owners, compat versus strict admission, overload status, hard-exit/restart, logger guarantees, IPC drops, monotonic series, TTL, OTel disabled state, diagnostics auth/audit, health truth and exact metrics. State that component zero values are compatibility shared-envelope semantics, not proof of capacity.

- [ ] **Step 5: Run moved/deleted symbol audits**

```bash
! rg -n 'config\.GlobalConfig|initOnce|ConfiguredExportServer|StartExportServer|GetReadiness|RecordConfigApply(Stage)?(Success|Failure)|stdouttrace|SetTracerProvider|SetTextMapPropagator|NewWithContext\([^,]+,[^,]+\)' cmd pkg --glob '*.go'
! rg -n 'max_in_flight:\s*1024|DefaultMaxInFlight\s*=\s*1024' conf pkg docs/configuration.md --glob '*.yaml' --glob '*.go' --glob '*.md'
rg -n '\bgo (func|[A-Za-z_])|\.Go\(func' pkg/runtime pkg/telemetry pkg/diagnostics pkg/observability pkg/plugin/logger_batch pkg/plugin/file_logger pkg/server pkg/worker pkg/supervisor --glob '*.go' --glob '!*_test.go' > .cache/tmp/runtime-production-goroutines.txt
bash -lc 'source .envrc && go test ./pkg/runtime -run "^TestProductionGoroutinesUseOwnedPrimitives$" -count=1'
```

Expected: the two negative scans have no legacy production result. The inventory contains only canonical `TaskRegistry` and plan 05 process-supervision primitive definitions; the AST test rejects every feature-code goroutine.

- [ ] **Step 6: Run impact-scoped acceptance**

```bash
bash -lc 'source .envrc && go test -race ./pkg/config ./pkg/runtime ./pkg/telemetry ./pkg/diagnostics ./pkg/observability/metrics ./pkg/plugin/logger_batch ./pkg/plugin/file_logger ./pkg/plugin/otel ./pkg/server ./pkg/worker ./pkg/supervisor -run "^(TestRuntimePolicy|TestMemory|TestBudget|TestAdmission|TestReporter|TestAggregator|TestCardinality|TestDiagnostics|TestLogger|TestOTel|TestHealth|TestPanic|TestTask|TestCapacity)" -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/config/... ./pkg/runtime/... ./pkg/telemetry/... ./pkg/diagnostics/... ./pkg/observability/... ./pkg/plugin/logger_batch/... ./pkg/plugin/file_logger/... ./pkg/plugin/otel/... ./pkg/server/... ./pkg/worker/... ./pkg/supervisor/...'
bash -lc 'source .envrc && make build && make runtime-capacity-contract'
git diff --check
```

Expected: PASS. The real `make runtime-capacity APISIX_IMAGE=<immutable-digest>` remains a separately reported Linux qualification command, never silently skipped or represented by unit tests.

- [ ] **Step 7: Check Windows source build and worktree scope**

Run: `bash -lc 'source .envrc && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...' && git status --short`

Expected: PASS; only runtime-safety implementation/plan paths are changed and the four review documents remain untouched.

- [ ] **Step 8: Run plan/type/dependency/unfinished-marker self-checks**

```bash
rg -n '^### Task [0-9]+:' docs/superpowers/plans/2026-08-23-runtime-safety-observability.md
rg -n 'RuntimeDependencies|ResourceRegistry|TaskRegistry|RequestTaskGroup|PreparedGeneration|LifecyclePolicy|StatusProvider|AuditSink|WorkerTelemetry|MessageMetricBatch' docs/superpowers/plans/2026-08-23-{immutable-compiler-plugin-runtime,supervisor-worker-platform,runtime-safety-observability}.md
! rg -ni 'T[B]D|T[O]DO|implement l[a]ter|fill (this|these) in|place[h]older' docs/superpowers/plans/2026-08-23-runtime-safety-observability.md
git diff --check -- docs/superpowers/plans/2026-08-23-runtime-safety-observability.md
```

Expected: exactly twelve ordered tasks; every consumed interface resolves with one signature; the negative scan is empty; diff check passes. Manually confirm every Go/Make command sources `.envrc` and stays impact-scoped.

- [ ] **Step 9: Commit docs and runner**

```bash
git add Makefile scripts/runtime_capacity.sh scripts/runtime_capacity_test.sh docs/architecture/runtime-safety-observability.md docs/configuration.md docs/production-profile.md docs/design.md
git commit -m "docs(runtime): specify capacity and observability evidence"
```

## Self-Review Results

- **Spec coverage:** Tasks 1–4 define the container/Go/component envelope, body/spool/cache/compile/generation budgets, soft shed, hard exit and compatibility/strict admission. Task 5 completes async logger bounds and ownership. Tasks 6–8 establish bounded worker aggregates, supervisor monotonic metrics, cardinality/TTL and one health state. Task 9 removes implicit/global OTel. Task 10 isolates diagnostics. Task 11 preserves task/panic/finalizer/connection-abort behavior. Task 12 records real capacity, overload and recovery evidence and deletes old paths.
- **Dependency consistency:** The plan adds only `EffectiveConfig.Runtime` and consumes plan 04 `RuntimeDependencies`, `ResourceRegistry`, `TaskRegistry`, `RequestTaskGroup`, `PreparedGeneration` and panic interfaces unchanged. It consumes exact plan 05 `LifecyclePolicy`, `Bootstrap.Run`, `Codec`, `Status`, `StatusProvider`, `AuditSink`, `WorkerTelemetry` and `WorkerTelemetrySink`; it adds only `MessageMetricBatch` beside the existing telemetry message and introduces no second supervisor state, audit contract, fatal method or transport.
- **Type consistency:** Every budget owner uses `BudgetClass` and idempotent `Reservation`; worker telemetry always uses `Batch`/`Point`/`Label`; reporter sequence and generation flow unchanged into `Aggregator.Apply`; stable health and metrics read one plan 05 state snapshot.
- **Completeness scan:** Every code-producing task contains exact files, symbols, red/green tests, commands, expected results and commit scope. The final scans cover removed globals/adapters, unowned goroutines, hidden 1024 admission, unfinished markers, shared-interface drift and platform imports.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-23-runtime-safety-observability.md`. Execute it after plans 04 and 05 expose their declared interfaces; Tasks 1–5 may be prepared beside HTTP closure, while Tasks 6–12 require the supervisor IPC/state implementation. Use `superpowers:subagent-driven-development` (recommended, fresh worker and review per task) or `superpowers:executing-plans` (inline batches with checkpoints).
