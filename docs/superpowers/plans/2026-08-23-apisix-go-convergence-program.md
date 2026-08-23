# APISIX-Go Architecture Convergence Program Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Converge apisix-go from its current single-process, locally validated APISIX 3.17 implementation into a governed, immutable, hot-upgradable, evidence-qualified Go data plane.

**Architecture:** Nine vertically testable subprojects replace one concern at a time while keeping `master` runnable. The dependency spine is governance → static config → durable generations → immutable compiler → supervisor → HTTP closure; runtime safety may start beside HTTP closure but its body-budget slice waits for the HTTP body contract, qualification consumes both completed milestones, and stream convergence follows the qualified HTTP milestone.

**Tech Stack:** Go 1.26, Cobra, Viper only as an input reader, bbolt, etcd v3, go-chi/chi, `net/http`, Prometheus, OpenTelemetry, Buildx/OCI, GitHub Actions, and Docker-based real dependency gates. Task 8 retires the pre-existing GoReleaser/Linux-download path.

**Spec:** `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`

## Global Constraints

- Compatibility target is Apache APISIX 3.17.0 at commit `9ef2ecab67f652d38365049613610ef649bb4ad0`.
- Preserve the APISIX namespace; version Go-native extensions separately.
- Source `.envrc` before every Go or Make command.
- Use impact-scoped tests; do not run `go test ./...`, `go test ./pkg/...`, or `make test` unless the affected infrastructure makes narrower proof impossible.
- Run focused race tests for concurrency-sensitive changes and `source .envrc && make build` for code changes.
- Do not add dependencies when the standard library or an existing project dependency supplies the required behavior.
- Do not retain temporary legacy adapters or proxy-only facades; each cutover removes its replaced path in the same reviewed unit.
- Keep the four existing untracked files under `docs/reviews/` outside implementation commits unless the project owner separately authorizes them.
- Linux amd64/arm64 is the first production platform; publish native-smoked macOS amd64/arm64 binaries; keep Windows source-buildable as experimental without an official artifact.
- Every behavior promotion records evidence maturity; no documentation row is its own proof.

---

## Program Dependency Graph

```text
01 Governance / Manifest / Profiles
        │
        ▼
02 Static Config / Effective Config
        │
        ▼
03 Durable Desired / Published Generations
        │
        ▼
04 Immutable Compiler / Plugin Lifecycle
        │
        ▼
05 Supervisor / Worker / Platform Lifecycle
        ├──────────────┐
        ▼              ▼
06 HTTP Closure   07 Runtime Safety / Observability
        └──────────────┬──────────────┘
                       ▼
08 Differential Qualification / Release
                       │
                       ▼
09 Stream Convergence
```

## Shared Program Interfaces

The child plans must use these names consistently:

```go
// package capability
type Namespace string
type Domain string
type EvidenceKind string
type BehaviorStatus string
type EvidenceState string
type DivergenceStatus string

type Target struct {
	Name         string
	Version      string
	SourceCommit string
	Image        string
}

type Factory struct {
	Key         string
	ImportPath  string
	ImportAlias string
	Constructor string
}

type EvidenceClaim struct {
	State  EvidenceState
	Refs   []string
	Owner  string
	Reason string
}

type Evidence struct {
	Schema         EvidenceClaim
	Unit           EvidenceClaim
	Upstream       EvidenceClaim
	Differential   EvidenceClaim
	RealDependency EvidenceClaim
	Failure        EvidenceClaim
	Recovery       EvidenceClaim
}

type PluginCapability struct {
	Name               string
	Implementation     string
	Namespace          Namespace
	Domains            []Domain
	APISIXDefault      bool
	Factories          []Factory
	Phases             []string
	Priority           int
	Scopes             []string
	InstanceScope      string
	Behavior           BehaviorStatus
	BehaviorSummary    string
	KnownGaps          []string
	Evidence           Evidence
	DivergenceIDs      []string
	SupportedPlatforms []string
}

type QualificationProfile struct {
	Name             string
	Domains          []string
	RequiredPlugins  []string
	RequiredEvidence []EvidenceKind
}

type Divergence struct {
	ID               string
	Status           DivergenceStatus
	Compatibility    string
	ADR              string
	OwnerApprovalRef string
}

type Manifest struct {
	SchemaVersion         int
	Target                Target
	Plugins               []PluginCapability
	QualificationProfiles []QualificationProfile
	Divergences           []Divergence
}

func Load() (*Manifest, error)
func (m *Manifest) Plugin(name string) (PluginCapability, bool)
func (m *Manifest) Qualification(name string) (QualificationProfile, bool)
func (m *Manifest) QualifiedPlugins(profile string) []string

// package config
type CompatibilityTarget string
type SecurityProfile string
type QualificationProfile string

type ProfileSelection struct {
	Compatibility CompatibilityTarget
	Security      SecurityProfile
	Qualification QualificationProfile
}

type SourceKind string

type FieldSource struct {
	Kind     SourceKind
	Origin   string
	Explicit bool
}

type Provenance map[string]FieldSource

type RuntimePaths struct {
	DataDir    string
	RuntimeDir string
	LogDir     string
	TempDir    string
}

type EffectiveConfig struct {
	Config     Config
	Provenance Provenance
	Profiles   ProfileSelection
	Paths      RuntimePaths
	Runtime    RuntimePolicy
}

// Task 2 establishes the first four fields. Task 5 adds Lifecycle, and Task 7
// completes this final RuntimePolicy without changing the APISIX namespace.
type RuntimePolicy struct {
	Lifecycle   LifecyclePolicy
	Safety      RuntimeSafetyPolicy
	Telemetry   TelemetryPolicy
	Diagnostics DiagnosticsPolicy
}

type LifecyclePolicy struct {
	ProtocolVersion  uint16
	StartupTimeout   time.Duration
	ReadyTimeout     time.Duration
	CatchUpTimeout   time.Duration
	DrainTimeout     time.Duration
	TerminateGrace   time.Duration
	ProbationPeriod  time.Duration
	RestartWindow    time.Duration
	MaxRestarts      int
	OrphanGrace      time.Duration
	IPCMaxFrameBytes int
}

type RuntimeSafetyPolicy struct {
	Memory    MemoryPolicy
	Budgets   ComponentBudgetPolicy
	Admission AdmissionPolicy
}

type MemoryPolicy struct {
	LimitBytes              int64
	SupervisorReserveBytes  int64
	ReplacementReserveBytes int64
	GoLimitPercent          int
	SoftPercent             int
	HardPercent             int
	SampleInterval          time.Duration
	HardSustain             time.Duration
}

type ComponentBudgetPolicy struct {
	RequestBodyMemoryBytes int64
	SpoolDiskBytes         int64
	CacheMemoryBytes       int64
	CacheDiskBytes         int64
	LoggerMemoryBytes      int64
	CompileMemoryBytes     int64
	GenerationMemoryBytes  int64
}

type AdmissionPolicy struct {
	MaxActiveConnections int
	MaxActiveRequests    int
	MaxHighCostRequests  int
}

type TelemetryPolicy struct {
	WorkerQueueBytes    int64
	MaxFrameBytes       int
	FlushInterval       time.Duration
	MaxTotalSeries      int
	GenerationSeriesTTL time.Duration
}

type DiagnosticsPolicy struct {
	Enabled            bool
	Address            string
	BearerToken        string
	ReadHeaderTimeout  time.Duration
	WriteTimeout       time.Duration
	MaxProfileDuration time.Duration
	MaxConcurrent      int
}

type LoadRequest struct {
	DefaultPath  string
	OverridePath string
	Environment  map[string]string
	CLIOverrides map[string]any
	Manifest     *capability.Manifest
	DefaultPaths RuntimePaths
}

func LoadEffective(req LoadRequest) (*EffectiveConfig, error)
func DefaultRuntimePaths() (RuntimePaths, error)
func JournalPath(e *EffectiveConfig) string
func (p ProfileSelection) Validate(manifest *capability.Manifest) error
func ValidateQualificationPlugins(enabled []string, selection ProfileSelection, manifest *capability.Manifest) error

// package generation
type Domain string
const (
	DomainHTTP   Domain = "http"
	DomainStream Domain = "stream"
)

type RevisionSet struct {
	Desired uint64
	HTTP    uint64
	Stream  uint64
}

// package runtime
type RuntimeDependencies struct {
	Config    *config.EffectiveConfig
	Secrets   secret.Materializer
	Resources *ResourceRegistry
	Tasks     *TaskRegistry
}

// package secret
type ScopedResolver interface {
	ResolveScoped(context.Context, Scope, string) (string, error)
}
func NewScopedMaterializer(ScopedResolver) Materializer

// package lifecycle
const ReasonMemoryPressureHard = "memory-pressure-hard"

// package worker
type HTTPRuntime interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
}

// package compiler
func (s *HTTPSnapshot) Handler() http.Handler
func (s *HTTPSnapshot) TLSConfig() *tls.Config

// package proxy
type BodyDirection string
const (
	BodyRequest  BodyDirection = "request"
	BodyResponse BodyDirection = "response"
)
type BodyBudget interface {
	Reserve(context.Context, BodyDirection, BodyMode, int64) (release func(), err error)
}
func MaterializeRequestBody(context.Context, BodyPlan, BodyBudget, io.ReadCloser) (*ReplayableBody, error)

// package generation
type GenerationArtifact struct {
	Domain   Domain
	Revision uint64
	Digest   [32]byte
	Snapshot string
}

type PublicationEngine interface {
	Prepare(context.Context, ApplyTicket, Snapshot, map[Domain]PublishedGeneration) (PublicationSet, error)
	DiscardPrepared(context.Context, PublicationSet) error
	Activate(context.Context, PublicationToken, PublicationSet) error
	FinalizeActivation(context.Context, PublicationToken, PublicationSet)
	RollbackActivation(context.Context, PublicationToken, PublicationSet) error
}
```

If implementation evidence requires a signature change, amend this master plan
and every consuming child plan in the same documentation change before coding
against the new signature.

### Task 1: Establish Governance and Generated Truth

**Files:**
- Execute: `docs/superpowers/plans/2026-08-23-governance-manifest-profiles.md`
- Produces: `docs/architecture/compatibility-contract.md`, `pkg/capability/manifest.go`, generated plugin/status artifacts, ADR template and gate

**Interfaces:**
- Consumes: current plugin registry, `docs/plugins.md`, pinned APISIX corpus metadata
- Produces: `capability.Manifest`, `config.ProfileSelection`, evidence maturity vocabulary, generated status inputs used by every subsequent plan

- [ ] **Step 1: Execute the governance child plan task-by-task**

Run each exact red/green/commit cycle in `docs/superpowers/plans/2026-08-23-governance-manifest-profiles.md`.

- [ ] **Step 2: Run the governance milestone gate**

Run: `bash -lc 'source .envrc && go test ./pkg/capability ./pkg/config ./pkg/plugin ./t/plugin -run "^(TestCapabilityManifest|TestProfileSelection|TestCapabilityManifestSelection|TestUpstreamCorpusAccounting)$" -count=1'`

Expected: PASS, and generated documentation has no uncommitted drift.

- [ ] **Step 3: Checkpoint milestone history and worktree state**

Run: `git status --short && git log --oneline -n 12`

Expected: the child plan's task commits are present, no governance change is left staged or unstaged, and only the four pre-existing user-owned `docs/reviews/` files may remain untracked. Do not create a second milestone commit.

### Task 2: Replace Static Configuration Semantics

**Files:**
- Execute: `docs/superpowers/plans/2026-08-23-static-effective-config.md`
- Modify during atomic cutover: `cmd`, `pkg/config`, `pkg/data_encryption`, `pkg/plugin/base`, `pkg/plugin`, `pkg/server`, `pkg/route`, `pkg/store`, `pkg/observability/metrics`
- Produces: presence-aware config tree, explicit merge/provenance, profile validation, `config test` and redacted `config dump`

**Interfaces:**
- Consumes: `capability.Manifest`, `config.ProfileSelection`
- Produces: `config.EffectiveConfig`, `config.Provenance`, `config.LoadEffective(config.LoadRequest)`, immutable `data_encryption.Service`, and explicit plugin/server dependencies used by the journal, compiler, supervisor and diagnostics; Task 4 consumes the service and atomically upgrades plugin materialization to `secret.Materializer`

- [ ] **Step 1: Execute the static configuration child plan task-by-task**

Run every test-first task in `docs/superpowers/plans/2026-08-23-static-effective-config.md` without retaining a fallback to `config.GlobalConfig`.

- [ ] **Step 2: Run the configuration milestone gate**

Run: `bash -lc 'source .envrc && go test ./pkg/config ./pkg/data_encryption ./pkg/plugin/base ./pkg/plugin ./pkg/server ./pkg/route ./pkg/store ./pkg/observability/metrics ./cmd -run "^(TestLoadEffective|TestMergePresence|TestProfileSelection|TestConfigCommand|TestDependencies|TestServerConfig)" -count=1 && make build'`

Expected: PASS; `rg -n 'config\.GlobalConfig' --glob '*.go' cmd pkg` prints no production call site.

- [ ] **Step 3: Checkpoint milestone history and worktree state**

Run: `git status --short && git log --oneline -n 12`

Expected: the child plan's task commits are present, no static-config change is left staged or unstaged, and only the four pre-existing user-owned `docs/reviews/` files may remain untracked. Do not create a second milestone commit.

### Task 3: Persist Desired and Published Generations

**Files:**
- Execute: `docs/superpowers/plans/2026-08-23-durable-generation-journal.md`
- Modify during atomic cutover: `pkg/store`, `pkg/etcd`, `pkg/config`, `pkg/server`, `pkg/observability/metrics`
- Produces: versioned bbolt journal, migrations, domain revisions, published snapshots, ack and offline-start contracts

**Interfaces:**
- Consumes: `config.EffectiveConfig`, provider mutations, `generation.Domain`
- Produces: `generation.Journal`, `generation.RevisionSet`, `generation.GenerationArtifact`, and reversible `generation.PublicationEngine` transactions consumed by compiler publication and supervisor recovery

- [ ] **Step 1: Execute the generation journal child plan task-by-task**

Follow `docs/superpowers/plans/2026-08-23-durable-generation-journal.md`, preserving explicit-delete and dependency-closure semantics. The coordinator must discard a prepared generation if staging fails, roll back an activated generation if journal commit fails, and may mark the old generation retiring only after commit succeeds.

- [ ] **Step 2: Run the durable-state milestone gate**

Run: `bash -lc 'source .envrc && go test ./pkg/generation ./pkg/store ./pkg/etcd ./pkg/config ./pkg/server ./pkg/observability/metrics -run "^(TestJournal|TestPublished|TestOffline|TestExplicitDelete|TestAcknowledg|TestReadiness)" -count=1'`

Expected: PASS, including restart and unknown-newer-schema failure cases.

- [ ] **Step 3: Checkpoint milestone history and worktree state**

Run: `git status --short && git log --oneline -n 16`

Expected: the child plan's journal commits are present, no generation change is left staged or unstaged, and only the four pre-existing user-owned `docs/reviews/` files may remain untracked. Do not create a second milestone commit.

### Task 4: Install the Immutable Compiler and Owned Plugin Runtime

**Files:**
- Execute: `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`
- Modify during atomic cutover: `pkg/generation`, `pkg/secret`, `pkg/runtime`, `pkg/compiler`, `pkg/resource`, `pkg/route`, `pkg/plugin`, `pkg/proxy`, `pkg/stream`, `pkg/server`, `pkg/apisix/ctx`, `pkg/observability/metrics`
- Produces: compiler phases, immutable snapshots, dependency closure, plugin scope identity, resource and task registries

**Interfaces:**
- Consumes: `config.EffectiveConfig`, `generation.Journal`, `capability.Manifest`
- Produces: `compiler.Compiler`, `compiler.PreparedGeneration`, the reversible activation implementation for `generation.PublicationEngine`, `runtime.RuntimeDependencies`, `runtime.ResourceRegistry`, `runtime.TaskRegistry`

- [ ] **Step 1: Execute the immutable compiler child plan task-by-task**

Use the atomic cutovers in `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`; do not preserve proxy-only methods from `pkg/route.Builder`.

- [ ] **Step 2: Run the compiler milestone gate**

Run: `bash -lc 'source .envrc && go test -race ./pkg/generation ./pkg/secret ./pkg/runtime ./pkg/compiler ./pkg/resource ./pkg/route ./pkg/plugin ./pkg/proxy ./pkg/stream ./pkg/server ./pkg/apisix/ctx ./pkg/observability/metrics -run "^(TestCoordinator|TestMaterializer|TestCompiler|TestDependencyClosure|TestPluginScope|TestResourceRegistry|TestTaskRegistry|TestGeneration|TestRequestLifecycle)" -count=1'`

Expected: PASS; moved/deleted symbol call-site scans find no production-only legacy path, and a stage failure releases every unexposed prepared lease.

- [ ] **Step 3: Checkpoint milestone history and worktree state**

Run: `git status --short && git log --oneline -n 20`

Expected: the child plan's compiler/runtime commits are present, no Task 4 change is left staged or unstaged, and only the four pre-existing user-owned `docs/reviews/` files may remain untracked. Do not create a second milestone commit.

### Task 5: Add the Supervisor and Platform Lifecycle

**Files:**
- Execute: `docs/superpowers/plans/2026-08-23-supervisor-worker-platform.md`
- Modify during atomic cutover: `pkg/config`, `pkg/generation`, `pkg/compiler`, `pkg/runtime`, `pkg/lifecycle`, `pkg/platform`, `pkg/supervisor`, `pkg/worker`, `pkg/server`, `cmd`, `Dockerfile`, and platform CI configuration
- Produces: supervisor command, worker command, framed IPC with bounded digest-checked artifact transfer, scoped secret request capabilities, inherited listener/HTTP-runtime ownership, activation/drain/rollback, and platform control adapters

**Interfaces:**
- Consumes: `compiler.PreparedGeneration`, `generation.Journal`, `runtime.RuntimeDependencies`
- Produces: `supervisor.Supervisor`, `worker.Bootstrap`, `lifecycle.Command`, `lifecycle.Status`, stable listener and worker-generation ownership

- [ ] **Step 1: Execute the supervisor child plan task-by-task**

Follow `docs/superpowers/plans/2026-08-23-supervisor-worker-platform.md`, starting with one Linux/Unix active worker and explicit unsupported-capability behavior on other platforms.

- [ ] **Step 2: Run the supervisor milestone gate**

Run: `bash -lc 'source .envrc && go test -race ./pkg/config ./pkg/generation ./pkg/compiler ./pkg/runtime ./pkg/lifecycle ./pkg/platform ./pkg/supervisor ./pkg/worker ./pkg/server ./cmd -run "^(TestRuntimePolicy|TestCoordinator|TestPreparedGeneration|TestTaskRegistry|TestLifecycle|TestIPC|TestSupervisor|TestWorker|TestActivation|TestDrain|TestRollback|TestControl|TestRunCommand)" -count=1 && make build'`

Expected: PASS, including crash probation, READY fencing, inherited listener, WebSocket drain and orphan grace cases.

- [ ] **Step 3: Checkpoint milestone history and worktree state**

Run: `git status --short && git log --oneline -n 20`

Expected: the child plan's supervisor/platform commits are present, no Task 5 change is left staged or unstaged, and only the four pre-existing user-owned `docs/reviews/` files may remain untracked. Do not create a second milestone commit.

### Task 6: Close the APISIX 3.17 HTTP Protocol Contract

**Files:**
- Execute: `docs/superpowers/plans/2026-08-23-http-protocol-compatibility.md`
- Modify during atomic cutover: `pkg/config`, `pkg/capability`, `pkg/server`, `pkg/route`, `pkg/proxy`, `pkg/plugin`, `pkg/apisix`, `t/plugin`, generated status documentation
- Produces: listener/protocol matrix, exact routing/variables, TLS/PROXY, streaming body plan, DNS/retry/LB/authority and error behavior

**Interfaces:**
- Consumes: immutable compiler stages, worker listener set, capability manifest
- Produces: qualified HTTP `compiler.PreparedGeneration`, immutable TLS selection, registered `worker.HTTPRuntime` instances, the `proxy.BodyBudget` seam, protocol capability evidence and strict-schema APISIX differential cases

- [ ] **Step 1: Execute the HTTP compatibility child plan task-by-task**

Follow `docs/superpowers/plans/2026-08-23-http-protocol-compatibility.md` in its declared slices; keep HTTP/3 and Lua execution as explicit non-Full gaps.

- [ ] **Step 2: Run the HTTP milestone gate**

Run: `bash -lc 'source .envrc && go test -race ./pkg/config ./pkg/capability ./pkg/server ./pkg/route ./pkg/proxy ./pkg/plugin ./pkg/apisix ./t/plugin -run "^(TestHTTP|TestTLS|TestProxyProtocol|TestRoute|TestVariable|TestRetry|TestStreaming|TestAuthority|TestProtocol|TestDifferential|TestHTTPCorpus)" -count=1 && make build'`

Expected: PASS; each completed capability has a pinned upstream mapping and no parsed-but-ignored field.

- [ ] **Step 3: Checkpoint milestone history and worktree state**

Run: `git status --short && git log --oneline -n 20`

Expected: the child plan's HTTP compatibility commits are present, no Task 6 change is left staged or unstaged, and only the four pre-existing user-owned `docs/reviews/` files may remain untracked. Do not create a second milestone commit.

### Task 7: Enforce Runtime Safety and Stable Observability

**Files:**
- Execute: `docs/superpowers/plans/2026-08-23-runtime-safety-observability.md`
- Modify during atomic cutover: `pkg/config`, `pkg/runtime`, `pkg/compiler`, `pkg/telemetry`, `pkg/diagnostics`, `pkg/observability`, `pkg/plugin`, `pkg/proxy`, `pkg/route`, `pkg/server`, `pkg/worker`, `pkg/supervisor`, capacity scripts and Make targets
- Produces: memory/admission budgets, task and panic policy, supervisor telemetry aggregation, generation-safe OTel, diagnostics and audit

**Interfaces:**
- Consumes: `runtime.TaskRegistry`, supervisor worker states, immutable generation identities
- Produces: `runtime.BudgetManager`, `telemetry.Aggregator`, `diagnostics.Server`, lifecycle audit events and capacity evidence inputs

- [ ] **Step 1: Execute the runtime safety child plan task-by-task**

Follow `docs/superpowers/plans/2026-08-23-runtime-safety-observability.md`; exporter and diagnostics failures must never block traffic readiness. Tasks independent of HTTP may run beside Task 6, but the body/spool budget task starts only after Task 6 installs the `proxy.BodyBudget` contract.

- [ ] **Step 2: Run the runtime-safety milestone gate**

Run: `bash -lc 'source .envrc && go test -race ./pkg/config ./pkg/runtime ./pkg/telemetry ./pkg/diagnostics ./pkg/observability/metrics ./pkg/plugin/logger_batch ./pkg/plugin/file_logger ./pkg/plugin/otel ./pkg/server ./pkg/worker ./pkg/supervisor -run "^(TestRuntimePolicy|TestMemory|TestBudget|TestAdmission|TestTask|TestPanic|TestTelemetry|TestReporter|TestAggregator|TestSeries|TestCardinality|TestDiagnostics|TestAudit|TestLogger|TestOTel|TestHealth|TestCapacity)" -count=1'`

Expected: PASS, including cross-generation counter continuity, exporter overload and sensitive diagnostics authorization.

- [ ] **Step 3: Checkpoint milestone history and worktree state**

Run: `git status --short && git log --oneline -n 20`

Expected: the child plan's runtime-safety commits are present, no Task 7 change is left staged or unstaged, and only the four pre-existing user-owned `docs/reviews/` files may remain untracked. Do not create a second milestone commit.

### Task 8: Qualify and Promote One Immutable HTTP Artifact

**Files:**
- Execute: `docs/superpowers/plans/2026-08-23-parity-qualification-release.md`
- Modify: `pkg/qualification`, `t/plugin`, `scripts`, `Makefile`, release and scheduled workflows, OCI metadata, capability evidence and release documentation; delete `.goreleaser.yaml` and the Linux-download release path
- Produces: differential runner, real dependency matrix, evidence bundle, native-built and native-smoked macOS archives, Windows cross-build evidence without an artifact, build-once signing and same-digest promotion

**Interfaces:**
- Consumes: completed HTTP capability/evidence manifest, stable supervisor metrics, release artifact digest
- Produces: `qualification.Result`, immutable evidence bundle, signed promoted digest, production-readiness decision input

- [ ] **Step 1: Execute the qualification child plan task-by-task**

Follow `docs/superpowers/plans/2026-08-23-parity-qualification-release.md`; do not rebuild between qualification and promotion.

- [ ] **Step 2: Run the qualification milestone gate**

Run the exact release-candidate workflow and local deterministic preflight specified by the child plan.

Expected: one Linux OCI index digest is present in build metadata, every Linux test record, SBOM, signature, provenance and promotion record; each published macOS archive has its own native-smoked file digest in the same evidence bundle; blocked or flaky evidence fails qualification.

- [ ] **Step 3: Checkpoint milestone history and worktree state**

Run: `git status --short && git log --oneline -n 20`

Expected: the child plan's qualification/release commits are present, no Task 8 change is left staged or unstaged, and only the four pre-existing user-owned `docs/reviews/` files may remain untracked. Do not create a second milestone commit or rebuild the qualified artifact.

### Task 9: Converge the Stream Subsystem

**Files:**
- Execute: `docs/superpowers/plans/2026-08-23-stream-convergence.md`
- Modify: `pkg/stream`, `pkg/compiler`, `pkg/supervisor`, `pkg/worker`, stream plugins, `pkg/qualification`, `t/plugin`, release-candidate/release workflows and release metadata verification
- Produces: stream listener/TLS/PROXY matrix, immutable stream compiler, general plugin chain, protocol owners, stream qualification

**Interfaces:**
- Consumes: qualified supervisor, generation journal, compiler/resource lifecycle, telemetry and evidence framework
- Produces: qualified `generation.DomainStream`, a stream evidence bundle, a fresh HTTP evidence bundle for the same post-stream candidate digest, and a no-rebuild promotion that binds both bundle hashes; the earlier HTTP-only bundle remains immutable

- [ ] **Step 1: Execute the stream convergence child plan task-by-task**

Follow `docs/superpowers/plans/2026-08-23-stream-convergence.md` only after Task 8's HTTP artifact is qualified.

- [ ] **Step 2: Run the stream milestone gate**

Run: `bash -lc 'source .envrc && go test -race ./pkg/stream/... ./pkg/server ./pkg/plugin/mqtt_proxy ./pkg/plugin/kafka_proxy ./pkg/plugin/dubbo -run "^(TestStream|TestMQTT|TestKafka|TestDubbo|TestGeneration)" -count=1 && make build'`

Expected: PASS with listener handoff, last-good publication, TLS/mTLS, PROXY protocol, protocol owner and drain evidence. The post-stream candidate is signed/promoted only after release metadata verifies both fresh HTTP and stream bundle hashes for that exact digest.

- [ ] **Step 3: Checkpoint milestone history and worktree state**

Run: `git status --short && git log --oneline -n 20`

Expected: the child plan's stream commits are present, no Task 9 change is left staged or unstaged, and only the four pre-existing user-owned `docs/reviews/` files may remain untracked. Do not create a second milestone commit.

## Final Program Acceptance

- [ ] **Step 1: Verify every child plan completed its own focused and race gates**

Run: `rg -n '^\- \[ \]' docs/superpowers/plans/2026-08-23-{governance-manifest-profiles,static-effective-config,durable-generation-journal,immutable-compiler-plugin-runtime,supervisor-worker-platform,http-protocol-compatibility,runtime-safety-observability,parity-qualification-release,stream-convergence}.md`

Expected: no output in the executed plan copies.

- [ ] **Step 2: Verify status truth and release identity**

Run the generated-manifest drift check and evidence-bundle verifier defined by Tasks 1 and 8.

Expected: documentation status equals generated manifest state, and one promoted digest matches every qualification artifact.

- [ ] **Step 3: Make the production-readiness decision**

Remove the `not production ready` statement only when the HTTP and stream qualification profiles both report PASS and no required evidence is deferred, flaky, skipped or stale.
