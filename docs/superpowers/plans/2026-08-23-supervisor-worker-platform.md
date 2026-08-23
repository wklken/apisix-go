# Supervisor, Worker, and Platform Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the single-process startup/reload owner with one long-lived supervisor and normally one active exec-created Go worker, using versioned IPC, inherited listeners, reversible activation, bounded drain, and explicit platform support.

**Architecture:** The supervisor is PID 1/runtime authority: it owns provider watch, writable journal, stable health/metrics, listener descriptors, worker processes, revision fences, activation, rollback, and restart policy. Workers receive immutable generation artifacts and inherited descriptors over an authenticated parent-child channel, compile/load without accepting, report READY, then accept only after ACTIVATE. `generation.PublicationEngine` maps journal activation directly onto worker quiesce/activate/rollback/finalize so durable truth and active process truth cannot split.

**Tech Stack:** Go 1.26.6, `os/exec`, `net`, `syscall`/`golang.org/x/sys/unix` behind build tags, length-prefixed JSON frames, bbolt journal, immutable `compiler.PreparedGeneration`, Cobra, `net/http`, and existing route hijack/task ownership.

**Spec:** `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`

## Global Constraints

- Compatibility target is Apache APISIX 3.17.0 at commit `9ef2ecab67f652d38365049613610ef649bb4ad0`.
- Preserve the APISIX namespace; lifecycle configuration lives only below the versioned Go-extension key `apisix_go.runtime.lifecycle`.
- Source `.envrc` before every Go or Make command.
- Use impact-scoped tests; do not run `go test ./...`, `go test ./pkg/...`, or `make test`.
- Run focused race tests for process state, listener ownership, activation, drain, and telemetry queues; run `source .envrc && make build` after code cutovers.
- Use `os/exec`; never fork without exec and never copy NGINX's CPU-count worker topology.
- Normally exactly one worker accepts new connections. A prepared/READY worker owns no accept loop until ACTIVATE; a quiesced predecessor can RESUME during rollback.
- The supervisor is the only provider watcher and writable journal owner. Workers never open bbolt or acknowledge provider revisions.
- Preserve plan 03's exact `PublicationEngine` signatures and order: Prepare → Stage; Stage failure → `DiscardPrepared` with an uncanceled cleanup context; activation/commit failure → RollbackActivation then Abort; commit success → FinalizeActivation; committed replay → read-only exact-fence `ConfirmActive` before acknowledgement.
- Preserve plan 04's `compiler.PreparedGeneration` ownership: rejected new generations close; predecessor leases remain live until finalize; hijacked connections keep their generation until natural close or bounded worker termination.
- Linux amd64/arm64 is the first production platform. This plan establishes native macOS amd64/arm64 build-smoke evidence; plan 08 packages and publishes only those natively smoked architectures. Windows remains source-buildable experimental, has no official artifact, and returns an explicit unsupported lifecycle error rather than selecting another runtime path.
- The container runs the supervisor as PID 1. Signals, systemd, Kubernetes, launchd, and Windows service controls translate into canonical lifecycle commands only.
- Decision 196C: the final cutover removes old startup, in-process reload scheduling, listener binding, signal branching, and proxy-only facades in one atomic commit; no compatibility adapter remains.
- Keep the four existing untracked files under `docs/reviews/` outside implementation commits.

---

## File and Responsibility Map

**Create:**

- `pkg/config/runtime_policy.go`, `runtime_policy_test.go` — `RuntimePolicy`, `LifecyclePolicy`, defaults, exact extension-key decoding and validation.
- `pkg/lifecycle/types.go`, `types_test.go` — canonical command, worker state, status, event, and telemetry contracts.
- `pkg/lifecycle/codec.go`, `codec_test.go` — versioned bounded framed IPC.
- `pkg/lifecycle/artifact.go`, `artifact_test.go` — digest-checked bounded begin/chunk/end transfer into private temporary spools.
- `pkg/lifecycle/secret.go`, `secret_test.go` — generation-scoped secret request/reply messages with redacted diagnostics.
- `pkg/platform/socketpair_unix.go`, `process_linux.go`, `process_darwin.go` — Linux/macOS socketpair plus OS-specific parent-death/process-group control.
- `pkg/platform/socketpair_windows.go`, `process_windows.go` — source-buildable explicit unsupported lifecycle implementation.
- `pkg/platform/platform_test.go`, `platform_unix_test.go`.
- `pkg/worker/bootstrap.go`, `bootstrap_test.go` — cross-exec bootstrap input, worker-local registry/compiler construction, and load/compile/probe/READY/ACTIVATE loop.
- `pkg/worker/secrets.go`, `secrets_test.go` — scoped IPC-backed `secret.Materializer` client.
- `pkg/worker/listeners.go`, `listeners_test.go` — inherited descriptor reconstruction and accept gate.
- `pkg/worker/drain.go`, `drain_test.go` — HTTP, hijack, task and generation drain accounting.
- `pkg/supervisor/supervisor.go`, `supervisor_test.go` — authoritative state machine and stable status.
- `pkg/supervisor/process.go`, `process_test.go` — exec launch, IPC and child ownership.
- `pkg/supervisor/listeners.go`, `listeners_test.go` — bind once, duplicate to workers, close once.
- `pkg/supervisor/activation.go`, `activation_test.go` — direct `generation.PublicationEngine` ownership, including prepared discard and infallible finalize.
- `pkg/supervisor/secrets.go`, `secrets_test.go` — exact-generation secret capability authorization.
- `pkg/supervisor/retirement.go`, `retirement_test.go` — asynchronous predecessor drain/timeout/terminate queue.
- `pkg/supervisor/restart.go`, `restart_test.go` — probation, bounded restart, rollback, terminal readiness.
- `pkg/supervisor/control.go`, `control_test.go` — canonical local lifecycle command endpoint.
- `cmd/supervisor.go`, `cmd/worker.go`, `cmd/lifecycle.go`, focused tests.
- `docs/architecture/supervisor-worker-lifecycle.md` — protocol, state machine, ownership and platform contract.

**Modify:**

- `pkg/config/effective.go`, `defaults.go`, `decode.go`, `validation.go`, `extension_env.go`, `types.go`, `effective_test.go` — install `EffectiveConfig.Runtime` and lifecycle extension aliases.
- `pkg/generation/coordinator.go`, `coordinator_test.go` — consume the already-approved reversible activation protocol without signature drift.
- `pkg/server/route_handler.go`, `route_handler_test.go` — expose generation request/hijack drain counters to worker.
- `pkg/runtime/task_registry.go`, `task_registry_test.go` — named generation task drain snapshots and join.
- `pkg/server/server.go`, `server_test.go`, `reload.go`, `reload_test.go` — worker serving only; remove provider/listener/process ownership.
- `cmd/root.go`, `root_test.go` — default command is supervisor; internal worker command is non-interactive.
- `Dockerfile`, `.github/workflows/platform.yml`, `.github/workflows/ci.yml` — PID 1 entrypoint and build/native-smoke platform matrix; artifact publication remains owned by plan 08.
- `docs/design.md`, `README.md`, deployment examples.

**Delete during Task 10's atomic cutover:**

- `pkg/server/reload.go`, `pkg/server/reload_test.go` after their remaining pure compile helpers move to compiler/worker owners.
- `pkg/server/generation_engine.go`, `pkg/server/generation_engine_test.go` after worker-local compile/load moves into `worker.Bootstrap` and process-level publication moves into `supervisor.Supervisor`.
- Old `Server.Start`, `startConfigProvider`, `startEtcdWatcher`, `startHTTPListeners`, listener-retention helpers, reload scheduler/event fields, direct signal branching, and all production callers.
- Any temporary `legacy`, `fallback`, `inProcess`, or `singleProcess` supervisor/worker switch introduced during earlier tasks.

## Shared Interfaces

```go
// package config
type RuntimePolicy struct { Lifecycle LifecyclePolicy `mapstructure:"lifecycle"` }
type LifecyclePolicy struct {
	ProtocolVersion uint16        `mapstructure:"protocol_version"`
	StartupTimeout  time.Duration `mapstructure:"startup_timeout"`
	ReadyTimeout    time.Duration `mapstructure:"ready_timeout"`
	CatchUpTimeout  time.Duration `mapstructure:"catch_up_timeout"`
	DrainTimeout    time.Duration `mapstructure:"drain_timeout"`
	TerminateGrace  time.Duration `mapstructure:"terminate_grace"`
	ProbationPeriod time.Duration `mapstructure:"probation_period"`
	RestartWindow   time.Duration `mapstructure:"restart_window"`
	MaxRestarts     int           `mapstructure:"max_restarts"`
	OrphanGrace     time.Duration `mapstructure:"orphan_grace"`
	IPCMaxFrameBytes int          `mapstructure:"ipc_max_frame_bytes"`
}

// EffectiveConfig adds exactly:
Runtime RuntimePolicy

// package lifecycle
type CommandKind string
const (
	CommandActivate CommandKind = "activate"
	CommandQuiesce  CommandKind = "quiesce"
	CommandResume   CommandKind = "resume"
	CommandDrain    CommandKind = "drain"
	CommandStop     CommandKind = "stop"
	CommandReload   CommandKind = "reload"
	CommandStatus   CommandKind = "status"
)
type WorkerState string
const (
	StateStarting WorkerState = "starting"
	StateLoading WorkerState = "loading"
	StateCatchingUp WorkerState = "catching-up"
	StateReady WorkerState = "ready"
	StateActive WorkerState = "active"
	StateQuiesced WorkerState = "quiesced"
	StateDraining WorkerState = "draining"
	StateStopped WorkerState = "stopped"
	StateFailed WorkerState = "failed"
)
type RevisionFence struct {
	Desired uint64
	HTTP uint64
	Stream uint64
	ArtifactDigests map[generation.Domain][32]byte
}
func FenceFor(generation.PublicationSet) RevisionFence
func RecoveryFence(generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration) (RevisionFence, error)
func (f RevisionFence) Equal(RevisionFence) bool
type Command struct { ID string; Kind CommandKind; Fence RevisionFence; Deadline time.Time }
type Status struct {
	State WorkerState
	WorkerPID int
	Generation uint64
	Fence RevisionFence
	Ready bool
	Terminal bool
	ReasonCode string
}
type Event struct { At time.Time; CommandID string; From, To WorkerState; ReasonCode string }

type StatusProvider interface { Status() Status }
type AuditSink interface { RecordLifecycle(Event) }
type WorkerTelemetry struct {
	WorkerPID int
	Generation uint64
	Requests uint64
	ActiveRequests int64
	HijackedConnections int64
	OwnedTasks int64
}
type WorkerTelemetrySink interface { SubmitWorkerTelemetry(context.Context, WorkerTelemetry) error }

const (
	ReasonNone                  = ""
	ReasonFenceMismatch         = "revision-fence-mismatch"
	ReasonStartupTimeout        = "startup-timeout"
	ReasonReadyTimeout          = "ready-timeout"
	ReasonWorkerExited          = "worker-exited"
	ReasonRestartBudgetExceeded = "restart-budget-exceeded"
	ReasonNoHealthyGeneration   = "no-healthy-generation"
	ReasonDrainTimeout          = "drain-timeout"
	ReasonMemoryPressureHard    = "memory-pressure-hard"
	ReasonProtocolViolation     = "protocol-violation"
	ReasonPlatformUnsupported   = "platform-unsupported"
	ReasonCoreInvariant         = "core-invariant"
)

// IPC envelope
type MessageType string
const (
	MessageHello MessageType = "hello"
	MessageLoad MessageType = "load"
	MessageArtifactBegin MessageType = "artifact-begin"
	MessageArtifactChunk MessageType = "artifact-chunk"
	MessageArtifactEnd MessageType = "artifact-end"
	MessageReady MessageType = "ready"
	MessageSecretAttemptOpen MessageType = "secret-attempt-open"
	MessageSecretAttemptOpenResponse MessageType = "secret-attempt-open-response"
	MessageSecretAttemptClose MessageType = "secret-attempt-close"
	MessageSecretRequest MessageType = "secret-request"
	MessageSecretResponse MessageType = "secret-response"
	MessageCommand MessageType = "command"
	MessageStatus MessageType = "status"
	MessageTelemetry MessageType = "telemetry"
	MessageError MessageType = "error"
)
type Frame struct {
	Version uint16
	Sequence uint64
	Type MessageType
	Payload json.RawMessage
}
type Codec struct {
	reader          *bufio.Reader
	writer          io.Writer
	version         uint16
	maxFrameBytes   uint32
	sendSequence    uint64
	receiveSequence uint64
	sendMu          sync.Mutex
	receiveMu       sync.Mutex
}
func NewCodec(io.Reader, io.Writer, uint16, int) *Codec
func (c *Codec) Send(context.Context, MessageType, any) error
func (c *Codec) Receive(context.Context, any) (MessageType, error)

type ListenerDescriptor struct {
	Name        string
	Network     string
	Address     string
	InheritedFD int
}
type ArtifactDescriptor struct {
	ID     string
	Digest [32]byte
	Size   uint64
}
type ArtifactBegin struct {
	TransferID string
	Artifact   ArtifactDescriptor
}
type ArtifactChunk struct {
	TransferID string
	Offset     uint64
	Data       []byte
}
type ArtifactEnd struct {
	TransferID string
	Size       uint64
	Digest     [32]byte
}
const MaxArtifactBytes uint64 = 256 << 20
func MaxArtifactChunkBytes(maxFrameBytes int) int
type WorkerBootstrapInput struct {
	ProtocolVersion    uint16
	EffectiveConfig    ArtifactDescriptor
	CapabilityManifest ArtifactDescriptor
}
type LoadMode string
const (
	LoadCandidate LoadMode = "candidate"
	LoadRecovery  LoadMode = "recovery"
)
type LoadRequest struct {
	Mode      LoadMode
	Ticket    *generation.ApplyTicket
	Revisions *generation.RevisionSet
	Desired   *ArtifactDescriptor
	Published ArtifactDescriptor
	Listeners []ListenerDescriptor
}
type Ready struct {
	Fence       RevisionFence
	Publication ArtifactDescriptor
}
type SecretAttemptOpen struct {
	RequestID   string
	Mode        LoadMode
	Attempt     secret.AttemptID
	Publication ArtifactDescriptor
}
type SecretAttemptOpenResponse struct {
	RequestID string
	Attempt   secret.AttemptID
	ErrorCode string
}
type SecretAttemptClose struct { Attempt secret.AttemptID }
type SecretRequest struct {
	RequestID       string
	DesiredRevision uint64
	Scope           secret.Scope
	Reference       string
}
type SecretResponse struct {
	RequestID   string
	Value       []byte
	ValueDigest [32]byte
	ErrorCode   string
}
type ArtifactAssembler struct {
	file       *os.File
	transferID string
	descriptor ArtifactDescriptor
	written    uint64
	hash       hash.Hash
	closed     bool
}
func NewArtifactAssembler(string, ArtifactBegin) (*ArtifactAssembler, error)
func (a *ArtifactAssembler) Append(ArtifactChunk) error
func (a *ArtifactAssembler) Finish(ArtifactEnd) (*os.File, error)
func (a *ArtifactAssembler) Close() error
func NewIPCTelemetrySink(*Codec) WorkerTelemetrySink

// package secret; plan 04 exposes this constructor so the worker can create
// registration-bound Value owners without receiving a keyring or encryption service.
type ScopedResolver interface {
	ResolveScoped(context.Context, Scope, string) (string, error)
}
type ScopedAttemptBroker interface {
	ScopedResolver
	AuthorizeCandidate(context.Context, AttemptID, generation.ApplyTicket,
		generation.PublicationSet) error
	AuthorizeRecovery(context.Context, AttemptID, generation.RevisionSet,
		map[generation.Domain]generation.PublishedGeneration) error
	RevokeAttempt(context.Context, AttemptID) error
}
func NewScopedMaterializer(ScopedAttemptBroker, *capability.SecretDeclarationCatalog) Materializer

// package platform
var ErrLifecycleUnsupported error
type IPCPair struct {
	Parent *os.File
	Child  *os.File
}
func NewIPC() (IPCPair, error)
func ConfigureChild(*exec.Cmd) error
func TerminateProcessGroup(context.Context, *exec.Cmd, time.Duration) error

// package worker
type HTTPRuntime interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
}
type Bootstrap struct {
	Input     lifecycle.WorkerBootstrapInput
	IPC       *lifecycle.Codec
	Listeners *ListenerSet
	Telemetry lifecycle.WorkerTelemetrySink
}
func (b *Bootstrap) Run(context.Context) error

type ListenerSet struct {
	mu        sync.Mutex
	listeners map[string]net.Listener
	http      map[string]HTTPRuntime
	active    bool
	closed    bool
}
func OpenListenerSet([]lifecycle.ListenerDescriptor) (*ListenerSet, error)
func (s *ListenerSet) RegisterHTTP(string, HTTPRuntime) error
func (s *ListenerSet) Serve(context.Context) error
func (s *ListenerSet) Shutdown(context.Context) error
func (s *ListenerSet) Activate() error
func (s *ListenerSet) Quiesce() error
func (s *ListenerSet) Resume() error
func (s *ListenerSet) Close() error

// package supervisor
type ListenerSet struct {
	mu        sync.Mutex
	listeners map[string]net.Listener
	closed    bool
}
func BindListenerSet(*config.EffectiveConfig) (*ListenerSet, error)
func (s *ListenerSet) DuplicateForChild() ([]*os.File, []lifecycle.ListenerDescriptor, error)
func (s *ListenerSet) Close() error

type workerProcess struct {
	cmd      *exec.Cmd
	ipc      *lifecycle.Codec
	status   lifecycle.Status
	fence    lifecycle.RevisionFence
	prepared generation.PublicationSet
}
type activationRecord struct {
	token       generation.PublicationToken
	next        *workerProcess
	predecessor *workerProcess
	set         generation.PublicationSet
}
type retirementRecord struct {
	predecessor *workerProcess
	replacement generation.PublicationSet
	enqueuedAt  time.Time
}
type Supervisor struct {
	config      *config.EffectiveConfig
	journal     generation.Journal
	policy      config.LifecyclePolicy
	listeners   *ListenerSet
	telemetry   lifecycle.WorkerTelemetrySink
	audit       lifecycle.AuditSink
	mu          sync.RWMutex
	status      lifecycle.Status
	active      *workerProcess
	previous    *workerProcess
	pending     map[uint64]*workerProcess
	activations map[generation.PublicationToken]*activationRecord
	retiring    []*retirementRecord
	retireCond  *sync.Cond
}
func New(config *config.EffectiveConfig, journal generation.Journal,
	sink lifecycle.WorkerTelemetrySink, audit lifecycle.AuditSink) (*Supervisor, error)
func (s *Supervisor) InstallRecovery(context.Context, generation.RecoveryState) error
func (s *Supervisor) Prepare(context.Context, generation.ApplyTicket, generation.Snapshot,
	map[generation.Domain]generation.PublishedGeneration) (generation.PublicationSet, error)
func (s *Supervisor) DiscardPrepared(context.Context, generation.PublicationSet) error
func (s *Supervisor) Activate(context.Context, generation.PublicationToken, generation.PublicationSet) error
func (s *Supervisor) RollbackActivation(context.Context, generation.PublicationToken, generation.PublicationSet) error
func (s *Supervisor) FinalizeActivation(context.Context, generation.PublicationToken, generation.PublicationSet)
func (s *Supervisor) ConfirmActive(context.Context, generation.PublicationSet) error
func (s *Supervisor) Run(context.Context) error
func (s *Supervisor) Execute(context.Context, lifecycle.Command) (lifecycle.Status, error)
func (s *Supervisor) Status() lifecycle.Status
```

The first authenticated `MessageHello` contains the only `WorkerBootstrapInput` for the exec-created worker. `LoadRequest`, `SecretAttemptOpen` and `Ready` contain metadata only and cannot repeat or override bootstrap metadata or load identity. Strict decoding enforces exactly one load shape: `LoadCandidate` requires non-nil `Ticket` and `Desired`, forbids `Revisions`, and streams desired plus published predecessors; `LoadRecovery` requires non-nil committed `Revisions`, forbids `Ticket` and `Desired`, and streams only verified published predecessors. Both require the published descriptor and listener descriptors. Desired, published predecessors, effective configuration, capability manifest, and the resulting `generation.PublicationSet` are each canonical-encoded and transferred as `ArtifactBegin` → ordered `ArtifactChunk` frames → `ArtifactEnd`; no single frame may contain a `generation.Snapshot`, `generation.PublishedGeneration`, `generation.PublicationSet`, keyring, compiler, registry, process pointer, or secret resolver. The resulting publication set is transferred exactly once before `SecretAttemptOpen`; the successful authorization pins that spool/descriptor, and later READY may only repeat the same descriptor rather than streaming replacement bytes. `generation.Snapshot` and values containing one are never encoded or decoded directly with `encoding/json`: the sender uses `CanonicalBytes()` and accessors to populate an explicit wire DTO, while the receiver reconstructs the value with `generation.NewSnapshot(...)` and verifies canonical bytes, digest, and artifact ID before use. `ArtifactAssembler` rejects duplicate IDs, gaps, overlaps, declared or observed size above `MaxArtifactBytes`, extra bytes, truncated end, and digest mismatch; it streams into a mode-0600 temporary spool and unlinks it on every error. Because JSON base64 expands `ArtifactChunk.Data`, `MaxArtifactChunkBytes` returns `floor((maxFrameBytes-256)/4)*3`, and the codec still enforces the final encoded frame limit.

`WorkerBootstrapInput` is the complete cross-exec bootstrap contract. After reconstructing and strictly decoding its effective-config and capability-manifest artifacts, the worker constructs Task 6's manifest-derived `capability.SecretDeclarationCatalog`, creates `secret.NewScopedMaterializer(scopedAttemptBroker, catalog)` and calls plan 04's exact `compiler.NewWorkerCompilerFactory(manifest, effective, materializer)` locally; the factory verifies the catalog digest, owns its worker-local resource registry and atomically returns an owned `PreparedGeneration` from `PrepareGeneration` or `PrepareRecovery`. The scoped materializer implements `RegisterCandidate`/`RegisterRecovery` locally: it recomputes the canonical mode-specific `AttemptID`, derives only the exact allowed closure from those arguments, and calls the broker's mode-specific authorization before returning a registration. The IPC broker canonical-encodes that exact `PublicationSet`, streams it as a bounded artifact, sends `SecretAttemptOpen`, and waits for the matching successful response before any `SecretRequest` can be emitted. For candidate mode the supervisor verifies ticket/digest identity, reconstructs the exact set, recomputes `CandidateAttemptID`, verifies every candidate snapshot/decision/closure key is backed by the already-streamed desired or predecessor artifacts, and intersects that closure with the manifest-derived desired grant. For recovery it recomputes the exact set from committed revisions plus verified published artifacts and requires `RecoveryAttemptID`. Only then does it install an attempt grant and answer success; rejection closes the candidate without returning secret material. Final READY must reference byte-identical preauthorized publication bytes, not a later replacement. `RevokeAttempt` sends `SecretAttemptClose` and removes only that process/attempt grant.

The supervisor never attempts to marshal a Go interface, bare compiler or pointer across exec. Both candidate and recovery grants bind attempt ID, exact desired revision (`Ticket.DesiredRevision` or `Revisions.Desired`), plugin, resource key, explicit `Scope.Source`, field, strict policy and raw reference digest. Every later secret request must match the already-authorized attempt and grant byte-for-byte; source is never inferred from resource kind, and no response is sent based on a closure learned only at READY. The supervisor returns only the requested material, never a keyring or encryption service. Both endpoints redact `Reference` and `Value`, bind responses to `RequestID`, verify `ValueDigest`, zero transient plaintext buffers after materialization, and reject requests after failed authorization, discard, rollback, retirement or attempt close. Candidate and recovery workers at the same desired revision may coexist because process identity plus distinct domain-separated attempt IDs keep their A/B grants and responses isolated.

`Supervisor.InstallRecovery` validates the complete `RecoveryState` before launching anything: every present candidate must match its domain revision; every absent domain with a non-zero committed revision must have an exact recovery failure and remains unavailable. It sends `LoadRecovery` only when at least one verified published candidate exists. An empty published map creates no process; a true zero-domain commit installs only the initialized desired fence, while all-domains-damaged recovery records terminal/unready failures and cannot confirm a non-empty publication.

Plan 06 consumes `worker.HTTPRuntime`, `ListenerSet.RegisterHTTP`, `ListenerSet.Serve`, and `ListenerSet.Shutdown` exactly as declared here. Plan 07 consumes `lifecycle.StatusProvider`, `AuditSink`, `WorkerTelemetry`, `WorkerTelemetrySink`, stable states/reason codes including `ReasonMemoryPressureHard`, and command/event identities exactly as declared here.

### Task 1: Add the Config-Owned Lifecycle Policy

**Files:**
- Create: `pkg/config/runtime_policy.go`, `runtime_policy_test.go`
- Modify: `pkg/config/effective.go`, `defaults.go`, `decode.go`, `validation.go`, `extension_env.go`, `types.go`, `effective_test.go`

**Interfaces:**
- Consumes: plan 02 presence-aware extension tree, provenance and `LoadEffective`.
- Produces: exact `RuntimePolicy`, `LifecyclePolicy`, defaults, validation, and `EffectiveConfig.Runtime`.

- [ ] **Step 1: Write failing defaults/alias/validation tests**

```go
func TestLifecyclePolicyDefaultsAndExtensionAliases(t *testing.T) {
	effective := loadEffectiveForTest(t, LoadRequest{Environment: map[string]string{
		"APISIXGO_RUNTIME_LIFECYCLE_MAX_RESTARTS": "5",
	}})
	p := effective.Runtime.Lifecycle
	if p.ProtocolVersion != 1 || p.ReadyTimeout != 30*time.Second || p.DrainTimeout != 240*time.Second || p.MaxRestarts != 5 {
		t.Fatalf("lifecycle = %+v", p)
	}
	assertSource(t, effective.Provenance, "apisix_go.runtime.lifecycle.max_restarts", SourceEnvironment)
}
```

Also table-test zero/negative durations, `max_restarts < 0`, frame size outside `4096..16777216`, probation longer than restart window, and unknown keys.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^TestLifecyclePolicy" -count=1'`

Expected: FAIL because `EffectiveConfig.Runtime` is absent.

- [ ] **Step 3: Implement exact keys, aliases, defaults, and validation**

Canonical keys and defaults:

```text
apisix_go.runtime.lifecycle.protocol_version=1
apisix_go.runtime.lifecycle.startup_timeout=30s
apisix_go.runtime.lifecycle.ready_timeout=30s
apisix_go.runtime.lifecycle.catch_up_timeout=30s
apisix_go.runtime.lifecycle.drain_timeout=240s
apisix_go.runtime.lifecycle.terminate_grace=10s
apisix_go.runtime.lifecycle.probation_period=30s
apisix_go.runtime.lifecycle.restart_window=60s
apisix_go.runtime.lifecycle.max_restarts=3
apisix_go.runtime.lifecycle.orphan_grace=5s
apisix_go.runtime.lifecycle.ipc_max_frame_bytes=1048576
```

Each has the exact environment alias `APISIXGO_RUNTIME_LIFECYCLE_<UPPER_FIELD>`. Durations must be positive; protocol version must equal `1`; frame bytes are `4096..16777216`; restart window must be at least probation; max restarts is `0..100`. There is no enable flag and no single-process fallback.

- [ ] **Step 4: Run tests and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^(TestLifecyclePolicy|TestLoadEffective|TestExtensionEnvironment)" -count=1'`

Expected: PASS.

```bash
git add pkg/config
git commit -m "feat(config): add supervisor lifecycle policy"
```

### Task 2: Define Canonical Lifecycle State and Versioned Framed IPC

**Files:**
- Create: `pkg/lifecycle/types.go`, `types_test.go`, `codec.go`, `codec_test.go`, `artifact.go`, `artifact_test.go`, `secret.go`, `secret_test.go`

**Interfaces:**
- Consumes: `LifecyclePolicy.ProtocolVersion` and frame limit.
- Produces: every lifecycle/status/telemetry/codec interface in Shared Interfaces.

- [ ] **Step 1: Write failing framing and state tests**

```go
func TestCodecRejectsWrongVersionAndOversizeBeforeDecode(t *testing.T) {
	var wire bytes.Buffer
	writer := NewCodec(nil, &wire, 1, 64)
	if err := writer.Send(context.Background(), MessageStatus, Status{State: StateReady}); err != nil { t.Fatal(err) }
	frame := wire.Bytes()
	binary.BigEndian.PutUint16(frame[4:6], 2)
	_, err := NewCodec(bytes.NewReader(frame), nil, 1, 64).Receive(context.Background(), new(Status))
	if !errors.Is(err, ErrProtocolVersion) { t.Fatalf("Receive() error = %v", err) }
}

func TestArtifactAssemblerRejectsOutOfOrderAndDigestMismatch(t *testing.T) {
	body := bytes.Repeat([]byte("generation"), 8192)
	digest := sha256.Sum256(body)
	descriptor := ArtifactDescriptor{ID: "desired-41", Digest: digest, Size: uint64(len(body))}
	begin := ArtifactBegin{TransferID: "transfer-desired-41", Artifact: descriptor}
	assembler, err := NewArtifactAssembler(t.TempDir(), begin)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = assembler.Close() })
	if err := assembler.Append(ArtifactChunk{TransferID: begin.TransferID, Offset: 1, Data: body[:32]});
		!errors.Is(err, ErrArtifactOffset) {
		t.Fatalf("Append() error = %v, want ErrArtifactOffset", err)
	}
	assembler, err = NewArtifactAssembler(t.TempDir(), begin)
	if err != nil { t.Fatal(err) }
	if err := assembler.Append(ArtifactChunk{TransferID: begin.TransferID, Offset: 0, Data: body}); err != nil { t.Fatal(err) }
	_, err = assembler.Finish(ArtifactEnd{TransferID: begin.TransferID, Size: uint64(len(body)), Digest: [32]byte{1}})
	if !errors.Is(err, ErrArtifactDigest) { t.Fatalf("Finish() error = %v, want ErrArtifactDigest", err) }
}

func TestProtocolPayloadTypesCannotEmbedGenerationObjects(t *testing.T) {
	if _, ok := reflect.TypeOf(LoadRequest{}).FieldByName("Bootstrap"); ok {
		t.Fatal("LoadRequest duplicates WorkerBootstrapInput from HELLO")
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(LoadRequest{}), reflect.TypeOf(SecretAttemptOpen{}), reflect.TypeOf(Ready{})} {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.Type == reflect.TypeOf(generation.Snapshot{}) ||
				field.Type == reflect.TypeOf(generation.PublicationSet{}) ||
				field.Type == reflect.TypeOf(generation.PublishedGeneration{}) {
				t.Fatalf("%s embeds forbidden full object %s", typ.Name(), field.Type)
			}
		}
	}
}
```

Cover monotonic sequence, truncated prefix/payload, unknown type, duplicate sequence, deadline cancellation, invalid state transition, one HELLO bootstrap followed by any number of LOAD generations, rejection of a second HELLO, stable reason-code format including `ReasonMemoryPressureHard`, and secret-free error frames. Artifact cases cover begin/chunk/end success, `MaxArtifactChunkBytes` with JSON expansion, exact `MaxArtifactBytes`, duplicate transfer ID, gaps, overlaps, truncated end, too many bytes, wrong digest, mode-0600 spool, cleanup after every failure, and round-tripping canonical desired/published/publication encodings without holding the complete bytes in memory. Secret-attempt tests prove open carries only mode/attempt/publication metadata, never repeats ticket/revisions, requires the publication artifact to complete first, matches response request/attempt IDs, and makes close idempotent. Secret message tests prove diagnostics never format request references or response values.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/lifecycle -run "^(TestCodec|TestLifecycle|TestArtifact|TestProtocolPayload|TestSecret)" -count=1'`

Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement the frame format and transition table**

Wire format is `uint32 payload_length | uint16 version | uint64 sequence | uint8 type_length | type | JSON payload`, network byte order. Length covers bytes after the prefix; validate length/version/type/sequence before JSON decode. Valid transitions are starting→loading→catching-up→ready→active→quiesced→active, active/quiesced→draining→stopped, and any non-stopped state→failed; all others return `ErrInvalidTransition`. `FenceFor` copies desired plus independent HTTP/stream revisions and every domain artifact digest; `Equal` compares all components including map cardinality and never treats one fence as an implicit subset of another. `RecoveryFence` deliberately projects the complete committed `RevisionSet` onto only the verified `PublishedGeneration` entries: it keeps `Revisions.Desired`, validates each present artifact against its domain revision, and omits failed/missing domains. Therefore a partial-damaged recovery has one exact projected fence, while `RecoveryState.Failures` separately proves why a non-zero committed domain is absent.

Implement `ArtifactAssembler` as a single-pass state machine. `ArtifactBegin` reserves one descriptor after validating ID, digest, and `Size <= MaxArtifactBytes`; chunks must use the same transfer ID and exact next offset, and are hashed while written to a private temporary file; `ArtifactEnd` must repeat the declared size and digest, fsync/rewind the completed spool, and transfer file ownership to the caller. Canonical decode reads from that spool and verifies the decoded generation identity against `LoadRequest`, `SecretAttemptOpen` or `Ready` metadata before use. The attempt-open state machine accepts only a completed publication spool tied to the current LOAD, consumes one matching response before permitting secret requests, and retains that exact descriptor for READY equality. Never call `io.ReadAll` for these artifacts and never place a full generation value in `Frame.Payload`.

- [ ] **Step 4: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/lifecycle -run "^(TestCodec|TestLifecycle|TestArtifact|TestProtocolPayload|TestSecret)" -count=1'`

Expected: PASS.

```bash
git add pkg/lifecycle
git commit -m "feat(lifecycle): define framed worker protocol"
```

### Task 3: Own Socketpairs, Exec, and Listener Descriptors by Platform

**Files:**
- Create: `pkg/platform/socketpair_unix.go`, `process_linux.go`, `process_darwin.go`, `socketpair_windows.go`, `process_windows.go`, `platform_test.go`, `platform_unix_test.go`
- Create: `pkg/supervisor/listeners.go`, `listeners_test.go`, `process.go`, `process_test.go`

**Interfaces:**
- Consumes: worker executable path, listener specs from effective config, IPC protocol.
- Produces: `platform.NewIPC()`, `platform.ConfigureChild(*exec.Cmd)`, and supervisor-owned `ListenerSet` duplication.

- [ ] **Step 1: Write failing FD ownership tests**

```go
func TestListenerSetDuplicatesForChildWithoutTransferringSupervisorOwnership(t *testing.T) {
	set := bindListenerSetForTest(t)
	files, manifest, err := set.DuplicateForChild(); if err != nil { t.Fatal(err) }
	for _, file := range files { _ = file.Close() }
	if len(manifest) != 1 { t.Fatalf("manifest = %+v", manifest) }
	assertListenerAccepts(t, set.Address())
}
```

Also test socketpair EOF, CLOEXEC on parent/unrelated FDs, child-only inherited IPC FD, descriptor manifest order, partial duplication cleanup, exec failure cleanup, process-group ownership, and Unix child death when supervisor exits after orphan grace.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/platform ./pkg/supervisor -run "^(TestIPC|TestListenerSet|TestExecWorker)" -count=1'`

Expected: FAIL because platform and supervisor packages do not exist.

- [ ] **Step 3: Implement Unix and Windows boundaries**

`socketpair_unix.go` starts with `//go:build linux || darwin`; Linux and macOS use `unix.Socketpair(AF_UNIX, SOCK_STREAM|SOCK_CLOEXEC, 0)`, duplicate listeners with `(*net.TCPListener).File`, and assign child FD 3 to IPC followed by manifest listener FDs 4..N through `exec.Cmd.ExtraFiles`. `process_linux.go` uses `//go:build linux`, a new process group, and `Pdeathsig`; `process_darwin.go` uses `//go:build darwin` and a new process group. On both systems IPC EOF starts `OrphanGrace` and then self-termination, so macOS does not pretend to have Linux `Pdeathsig`. Both Windows files use `//go:build windows`, compile without Unix constants, and return `platform.ErrLifecycleUnsupported`; this is an explicit experimental limitation, not a second runtime.

- [ ] **Step 4: Run tests/cross-build and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/platform ./pkg/supervisor -run "^(TestIPC|TestListenerSet|TestExecWorker)" -count=1'`

Run: `bash -lc 'source .envrc && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c ./pkg/platform -o .cache/tmp/platform-windows.test.exe'`

Expected: PASS.

```bash
git add pkg/platform pkg/supervisor
git commit -m "feat(supervisor): own worker IPC and listeners"
```

### Task 4: Bootstrap a Worker That Cannot Accept Before ACTIVATE

**Files:**
- Create: `pkg/worker/bootstrap.go`, `bootstrap_test.go`, `listeners.go`, `listeners_test.go`, `secrets.go`, `secrets_test.go`
- Modify: `pkg/compiler/types.go`, `compiler_test.go`
- Modify: `pkg/secret/materializer.go`, `materializer_test.go` — add `ScopedAttemptBroker`/`NewScopedMaterializer` plus local `RegisterCandidate`/`RegisterRecovery` attempt views; keep `secret.Value` construction inside `pkg/secret`.

**Interfaces:**
- Consumes: cross-exec `lifecycle.WorkerBootstrapInput`, chunked artifact spools, `compiler.PreparedGeneration`, scoped secret IPC, and inherited descriptor manifest.
- Produces: exact `worker.Bootstrap.Run`, worker-local registries/compiler, IPC-backed scoped `secret.Materializer`, and accept-gated `worker.ListenerSet`/`HTTPRuntime` registration.

- [ ] **Step 1: Write the failing READY/accept fence test**

```go
func TestWorkerReadyDoesNotAcceptUntilActivateFenceMatches(t *testing.T) {
	fixture := newWorkerFixture(t, fenceForRevision(41))
	fixture.SendLoad(41)
	fixture.ExpectStatus(lifecycle.StateReady)
	assertDialTimesOut(t, fixture.Address())
	fixture.SendCommand(lifecycle.Command{ID: "activate-41", Kind: lifecycle.CommandActivate, Fence: fenceForRevision(41)})
	fixture.ExpectStatus(lifecycle.StateActive)
	assertHTTPStatus(t, fixture.Address(), http.StatusNoContent)
}

func TestListenerSetRegistersAndServesHTTPRuntimeByDescriptorID(t *testing.T) {
	set := openListenerSetForTest(t, "http-main")
	runtime := &fakeHTTPRuntime{}
	if err := set.RegisterHTTP("http-main", runtime); err != nil { t.Fatal(err) }
	done := make(chan error, 1)
	go func() { done <- set.Serve(context.Background()) }()
	assertDialTimesOut(t, set.Address("http-main"))
	if err := set.Activate(); err != nil { t.Fatal(err) }
	assertEventually(t, runtime.serveStarted)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := set.Shutdown(ctx); err != nil { t.Fatal(err) }
	if runtime.shutdownCalls != 1 { t.Fatalf("Shutdown calls = %d, want 1", runtime.shutdownCalls) }
	if err := <-done; err != nil { t.Fatal(err) }
}

func TestWorkerBuildsCompilerLocallyAndRequestsOnlyScopedSecrets(t *testing.T) {
	fixture := newWorkerFixture(t, fenceForRevision(42))
	fixture.SendHello(lifecycle.WorkerBootstrapInput{
		ProtocolVersion: 1,
		EffectiveConfig: fixture.EffectiveConfigDescriptor(),
		CapabilityManifest: fixture.CapabilityManifestDescriptor(),
	})
	fixture.SendBootstrapArtifacts(validEffectiveConfigBytes(t), validCapabilityManifestBytes(t))
	ticket := ticketForRevision(42)
	desired := fixture.DesiredDescriptor(42)
	fixture.SendLoad(lifecycle.LoadRequest{
		Mode:      lifecycle.LoadCandidate,
		Ticket:    &ticket,
		Desired:   &desired,
		Published: fixture.PublishedDescriptor(41),
		Listeners: fixture.ListenerDescriptors(),
	})
	fixture.SendGenerationArtifacts(desiredWithSecretRef(t, "$secret://vault/test1/route/key"), emptyPublished(t))
	open := fixture.ExpectCandidateAttemptOpen()
	wantAttempt := secret.CandidateAttemptID(ticket, fixture.ExpectedCandidatePublicationSet(t))
	if open.Attempt != wantAttempt { t.Fatalf("attempt open = %x, want %x", open.Attempt, wantAttempt) }
	fixture.ApproveSecretAttempt(open)
	request := fixture.ReceiveSecretRequest()
	wantScope := secret.Scope{Generation: 42, Attempt: wantAttempt, Plugin: "key-auth",
		Resource: generation.ResourceKey{Kind: "routes", ID: "r1"},
		Source: capability.SecretPluginConfig, Field: "key"}
	if request.DesiredRevision != 42 || request.Scope != wantScope || request.Reference != "$secret://vault/test1/route/key" {
		t.Fatalf("secret request = %+v", request)
	}
	fixture.SendSecretResponse(request.RequestID, []byte("resolved"))
	fixture.ExpectStatus(lifecycle.StateReady)
	fixture.AssertWorkerCreatedResourceAndTaskRegistries()
	fixture.AssertNoParentPointerOrKeyringTransferred()
}

func TestWorkerRecoveryLoadsOnlyCommittedPublishedArtifacts(t *testing.T) {
	fixture := newBootstrappedWorkerFixture(t)
	revisions := generation.RevisionSet{Desired: 42, HTTP: 40, Stream: 41}
	fixture.SendLoad(lifecycle.LoadRequest{Mode: lifecycle.LoadRecovery, Revisions: &revisions,
		Published: fixture.PublishedDescriptorForRevisions(revisions), Listeners: fixture.ListenerDescriptors()})
	fixture.SendPublishedArtifacts(publishedWithSecretRef(t, revisions, "$secret://vault/test1/route/key"))
	open := fixture.ExpectRecoveryAttemptOpen()
	wantAttempt := secret.RecoveryAttemptID(revisions, fixture.VerifiedPublished(t))
	if open.Attempt != wantAttempt { t.Fatalf("recovery attempt open = %x, want %x", open.Attempt, wantAttempt) }
	fixture.ApproveSecretAttempt(open)
	request := fixture.ReceiveSecretRequest()
	if request.DesiredRevision != revisions.Desired || request.Scope.Attempt != wantAttempt {
		t.Fatalf("secret request revision/attempt = %d/%x", request.DesiredRevision, request.Scope.Attempt)
	}
	fixture.SendSecretResponse(request.RequestID, []byte("resolved"))
	ready := fixture.ExpectReady()
	fixture.AssertPublicationMatchesCommittedRevisions(ready.Publication, revisions)
	fixture.AssertNoDesiredArtifactTransferred()
}

func TestSupervisorRecoveryUsesVerifiedSubsetFenceAndKeepsMissingDomainUnready(t *testing.T) {
	recovery := recoveryFixture(t, generation.RevisionSet{Desired: 42, HTTP: 40, Stream: 41},
		map[generation.Domain]generation.PublishedGeneration{generation.DomainHTTP: publishedHTTP(t, 40)},
		[]generation.RecoveryFailure{{Domain: generation.DomainStream, Code: "artifact-corrupt"}})
	fixture := newSupervisorFixture(t)
	if err := fixture.supervisor.InstallRecovery(context.Background(), recovery); err != nil { t.Fatal(err) }
	ready := fixture.ExpectRecoveryReady()
	want, err := lifecycle.RecoveryFence(recovery.Revisions, recovery.Published)
	if err != nil { t.Fatal(err) }
	if !ready.Fence.Equal(want) { t.Fatalf("READY fence = %#v, want %#v", ready.Fence, want) }
	fixture.AssertDomainActive(generation.DomainHTTP, 40)
	fixture.AssertDomainUnavailable(generation.DomainStream)
	fixture.AssertReadinessFalse("artifact-corrupt")
}

func TestWorkerRejectsSecondHelloInsteadOfReplacingBootstrap(t *testing.T) {
	fixture := newWorkerFixture(t, fenceForRevision(43))
	first := fixture.ValidBootstrapInput()
	fixture.SendHello(first)
	fixture.SendBootstrapArtifacts(validEffectiveConfigBytes(t), validCapabilityManifestBytes(t))
	fixture.SendHello(first)
	fixture.ExpectProtocolError(lifecycle.ReasonProtocolViolation)
	fixture.AssertNoLoadAccepted()
}
```

Cover duplicate/missing runtime IDs, a runtime registered for the wrong descriptor, Serve before/after activation, shutdown order and joined errors, invalid descriptor manifest, bootstrap config/manifest digest mismatch, unknown config fields, load/compile/probe failure, chunk gaps/truncation/oversize, READY publication digest mismatch, stale/lower fence, duplicate command ID idempotency, secret attempt-open response/request-ID/digest mismatch, secret response request-ID/digest mismatch, secret request before attempt authorization or after candidate discard, parent channel EOF, and cancellation. Add a table that rejects unknown mode, candidate without ticket/desired, candidate with revisions, recovery without revisions, recovery with ticket/desired, recovery artifact revision mismatch, attempt-ID mismatch/change, candidate publication resources outside desired/predecessor inputs, READY bytes different from the authorized set and recovery grant requests outside committed closures. Add an overlap test that keeps recovery Published=B active while launching a same-desired-revision candidate=A; assert different attempt IDs, correct per-process values, cross-attempt response rejection and no premature revocation of B during candidate rollback.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/worker ./pkg/secret -run "^(TestWorker|TestListenerSet|TestScopedSecret|TestMaterializer)" -count=1'`

Expected: FAIL because worker bootstrap does not exist.

- [ ] **Step 3: Implement load→compile→probe→READY→ACTIVATE**

The hidden worker command receives only inherited FD numbers plus exactly one first HELLO containing `WorkerBootstrapInput`. `Bootstrap.Run` rejects LOAD before HELLO and rejects every later HELLO as `ReasonProtocolViolation`; it never merges or replaces bootstrap state. It reconstructs the effective-config and capability-manifest spools named by that HELLO through the artifact state machine, strictly decodes both, constructs the manifest-derived declaration catalog plus IPC-backed `secret.NewScopedMaterializer(scopedAttemptBroker, catalog)`, and calls `compiler.NewWorkerCompilerFactory` locally. A `LoadCandidate` request reconstructs its desired snapshot and calls the single atomic `WorkerCompilerFactory.PrepareGeneration`; that factory computes the effective set, calls local `RegisterCandidate`, blocks in `AuthorizeCandidate` until the supervisor has verified and installed the exact attempt grant, constructs `GenerationCapability` from the returned registration and sends only scopes bearing its `CandidateAttemptID`. A `LoadRecovery` request has no desired artifact and calls `PrepareRecovery(ctx, *request.Revisions, published, onFailure)`; it analogously calls local `RegisterRecovery`/`AuthorizeRecovery` and uses `RecoveryAttemptID`. Any mixed, missing or mode-incompatible field is a protocol violation. No registry, bare compiler, materializer implementation, keyring, journal handle, or process pointer crosses exec.

The worker reconstructs descriptors and registers every compiler-produced HTTP listener runtime by descriptor ID before serving; duplicate, missing, and unused IDs fail preparation. `ListenerSet.Serve` starts each registered runtime on its matching inherited listener but holds accepts behind the common gate. `RegisterHTTP` is forbidden after Serve begins. `ListenerSet.Shutdown` closes the gate, invokes every registered runtime's `Shutdown` in sorted descriptor-ID order, waits all Serve calls, and joins exact errors so plan 06 has one HTTP drain owner.

For candidate LOAD, the supervisor streams canonical desired and published-predecessor bytes; for recovery LOAD, it streams only verified published predecessors and the committed `RevisionSet` remains metadata. The worker calls the mode-specific atomic factory entrypoint, which returns a fully owned prepared generation or cleans up on failure. Candidate preparation emits the Task 5 publication set; recovery reconstructs the exact committed set with `DesiredRevision = Revisions.Desired` and byte-identical verified published candidates, rejecting any present HTTP/stream artifact revision that differs from the matching `RevisionSet` field. The worker probes it, enters catching-up, canonical-encodes that `PublicationSet` to a private spool, streams it to the supervisor, and finally sends READY containing only the publication descriptor and fence. The supervisor reconstructs and verifies the publication bytes before marking the process READY. Both sides validate candidate fences against the ticket; for recovery both compute `RecoveryFence(revisions, verifiedPublished)` and require READY/FenceFor(reconstructedSet) to equal that projected fence exactly. Missing non-zero domains are validated only through `RecoveryState.Failures` and keep readiness false; they are never forged into the worker's partial set. ACTIVATE opens gates only after exact fence equality. Replacing or discarding a catching-up candidate closes its `PreparedGeneration`, disables its secret capability, sends STOP, waits through `TerminateGrace`, and kills it if necessary.

- [ ] **Step 4: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/worker ./pkg/compiler ./pkg/secret -run "^(TestWorker|TestListenerSet|TestScopedSecret|TestPreparedGeneration|TestCompilerProbe|TestMaterializer)" -count=1'`

Expected: PASS.

```bash
git add pkg/worker pkg/compiler pkg/secret
git commit -m "feat(worker): load immutable generations behind accept gates"
```

### Task 5: Launch, Fence, and Activate Workers Through PublicationEngine

**Files:**
- Create: `pkg/supervisor/supervisor.go`, `supervisor_test.go`, `activation.go`, `activation_test.go`, `secrets.go`, `secrets_test.go`
- Modify: `pkg/generation/coordinator_test.go`

**Interfaces:**
- Consumes: exact `generation.PublicationEngine`, worker-owned `compiler.PreparedGeneration`, worker READY/fence and listener ownership.
- Produces: supervisor implementation of Prepare/DiscardPrepared/Activate/RollbackActivation/FinalizeActivation/ConfirmActive and exact-generation secret authorization.

- [ ] **Step 1: Write failing activation/commit rollback tests**

```go
func TestSupervisorPublicationEngineCommitFailureResumesOldWorker(t *testing.T) {
	fixture := newSupervisorFixture(t, activeWorker(50), readyWorker(51))
	fixture.journal.commitErr = errors.New("disk full")
	_, err := fixture.coordinator.Apply(context.Background(), desiredBatch(51))
	if err == nil { t.Fatal("Apply() error = nil") }
	fixture.AssertWorkerState(50, lifecycle.StateActive)
	fixture.AssertWorkerStopped(51)
	fixture.AssertPublishedRevision(50)
}

func TestSupervisorPublicationEngineStageFailureDiscardsReadyCandidate(t *testing.T) {
	stageErr := errors.New("stage failed")
	fixture := newSupervisorFixture(t, activeWorker(52), readyWorker(53))
	fixture.journal.stageErr = stageErr
	ctx, cancel := context.WithCancel(context.Background())
	fixture.journal.onStage = cancel
	_, err := fixture.coordinator.Apply(ctx, desiredBatch(53))
	if !errors.Is(err, stageErr) { t.Fatalf("Apply() error = %v, want %v", err, stageErr) }
	fixture.AssertWorkerState(52, lifecycle.StateActive)
	fixture.AssertWorkerStopped(53)
	fixture.AssertCandidatePreparedGenerationClosed(53, 1)
	fixture.AssertNoPendingGeneration(53)
}

func TestSupervisorFinalizeOnlyEnqueuesRetirement(t *testing.T) {
	fixture := newSupervisorFixture(t, activeWorker(54), readyWorker(55))
	token, set := fixture.ActivateWithoutCommit(55)
	fixture.FailAllWorkerIPC()
	fixture.supervisor.FinalizeActivation(context.Background(), token, set)
	fixture.AssertActiveWorker(55)
	fixture.AssertRetirementQueued(54)
	fixture.AssertCommandsSentAfterFinalize(nil)
}
```

Also prove activation failure after old QUIESCE resumes old; stale READY is killed; repeated `DiscardPrepared` is success without touching the active worker; candidate stop failure is joined with Stage failure; commit success calls finalize and acknowledges before the retirement loop performs DRAIN; no ack precedes ACTIVE+commit+finalize; only one worker accepts at every observed transition. Secret tests first require a successful `SecretAttemptOpen` handshake whose bounded canonical publication artifact produces the exact mode-specific attempt ID and closure grant; no `SecretRequest` may receive material before that acknowledgement. After authorization, require `DesiredRevision`, `Scope.Generation`, `Scope.Attempt`, plugin/resource/source/field scope and reference digest to match the installed grant. Cross-generation/attempt/source, post-discard/revoke, unknown field/reference, duplicate request ID, closure omission, changed READY publication and active-predecessor borrowing all return stable redacted error codes.

Add `TestSupervisorPublicationEngineConfirmActive` subtests for an exact requested-domain subset, a missing/mismatched artifact, a canceled context, an initialized zero-domain fence, and an active worker containing an additional independently active older domain. Include a deterministic lock-wait cancellation case: hold the supervisor mutex, start ConfirmActive, cancel, then unlock. Assert success only for exact requested identities, `generation.ErrActiveGenerationMismatch` for identity failures, context cancellation precedence before and immediately after the lock, and zero IPC/process/activation/retirement calls in every confirmation case.

Add `TestSupervisorPublicationEngineZeroDomainNoop` for first commit, Stage failure/discard, commit failure followed by same-cursor retry, and committed replay. Assert the journal can stage/commit the synthetic empty set and finalize initializes the confirmation fence, but Supervisor launches no candidate, sends no IPC, never quiesces/replaces the active worker and never enqueues retirement. Synthetic discard removes only the exact pending record; commit failure/rollback preserves the same owner, and replay performs only the read-only confirmation.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/supervisor ./pkg/generation -run "^(TestSupervisorPublicationEngine|TestCoordinator.*Activation)" -count=1'`

Expected: FAIL because process activation is not the publication owner.

- [ ] **Step 3: Implement the exact transaction**

For an empty required-domain ticket, `Prepare` creates only a synthetic pending record and returns an empty `PublicationSet`; it does not derive a grant, launch a worker or perform IPC. For a non-empty ticket, `Prepare` derives only the manifest/declaration/reference grant candidates, launches a worker, and transfers bootstrap/config/manifest/desired/predecessor data using metadata plus bounded artifact frames. Before materialization, `SecretAttemptOpen` supplies the exact candidate publication artifact; the supervisor verifies it as specified above, narrows the candidates to its exact closure/decisions, installs a grant keyed by pending worker identity plus `AttemptID`, and only then acknowledges authorization. The worker locally creates and retains its `compiler.PreparedGeneration`; READY must repeat the preauthorized descriptor, and the supervisor rechecks byte identity plus the exact `PublicationSet` fence without performing a second or retrospective authorization. The attempt grant exists only from successful open through discard, rollback, process exit, explicit revoke, or successful activation ownership transfer.

For a synthetic empty publication, `DiscardPrepared` atomically removes only the exact pending record and performs no grant, process or IPC operation. For a non-empty publication, it atomically removes the matching pending record, disables its secret capability, then sends STOP, waits through `TerminateGrace`, kills if necessary, and waits for process exit so the worker closes its `PreparedGeneration`. It is idempotent after successful removal and never affects active/predecessor processes. A mismatched publication identity returns `ErrPreparedWorkerNotFound`. Coordinator calls it with `context.WithoutCancel`; stop/kill/wait errors are returned so `errors.Join` retains both Stage and cleanup evidence.

For a synthetic empty set, `Activate` only binds the journal token to the synthetic record, rollback only deletes that record, and finalize sets the initialized fence; none of them sends IPC, changes the active worker or enqueues retirement. For a non-empty set, `Activate` sends QUIESCE to old and waits QUIESCED, sends ACTIVATE to new and waits ACTIVE; any error leaves the activation record for rollback. Non-empty `RollbackActivation` idempotently quiesces/stops new, disables its secret capability and verifies process exit, then resumes old and verifies ACTIVE. Non-empty `FinalizeActivation` locks the supervisor, validates token/set/worker identity, records the new active worker and updates the exact per-domain identities named by its publication fence without deleting untouched independently active domains, appends `{predecessor,replacement,enqueuedAt}` to `retiring`, removes activation/pending state, signals `retireCond`, unlocks, and returns. It sends no IPC, waits on no process, closes no generation, and has no recoverable failure. `ConfirmActive` checks context both before and immediately after acquiring the supervisor mutex, then compares every domain requested by the replay set against the active worker/fence; additional independently active domains are permitted. Mismatch returns `generation.ErrActiveGenerationMismatch` and never launches or switches a worker. Recovery must install every verified recovered domain identity and set the separate initialized fence before provider startup, including when the recovered active set is empty. Missing token/fence during finalize is a `ReasonCoreInvariant` fatal panic because journal commit already succeeded. The main loop owns all retirement I/O in Task 6. Do not add a forwarding method to `server.GenerationEngine`; Task 10 deletes that temporary predecessor implementation in the startup cutover.

- [ ] **Step 4: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/supervisor ./pkg/generation -run "^(TestSupervisorPublicationEngine|TestSupervisorFinalize|TestSupervisorSecret|TestCoordinator|TestSingleActiveWorker)" -count=1'`

Expected: PASS.

```bash
git add pkg/supervisor pkg/generation
git commit -m "feat(supervisor): activate workers with journal rollback"
```

### Task 6: Drain HTTP Requests, WebSockets, Hijacks, Tasks, and Generations

**Files:**
- Create: `pkg/worker/drain.go`, `drain_test.go`
- Create: `pkg/supervisor/retirement.go`, `retirement_test.go`
- Modify: `pkg/worker/listeners.go`, `listeners_test.go`
- Modify: `pkg/server/route_handler.go`, `route_handler_test.go`
- Modify: `pkg/runtime/task_registry.go`, `task_registry_test.go`
- Modify: `pkg/compiler/types.go`, `compiler_test.go`

**Interfaces:**
- Consumes: route request/hijack registration, registered `worker.HTTPRuntime`, `runtime.TaskRegistry`, `PreparedGeneration.Close`, and supervisor retirement records.
- Produces: `worker.DrainSnapshot`, unified `Drain(context.Context) error`, residual-owner reporting, and asynchronous supervisor predecessor retirement.

- [ ] **Step 1: Write failing natural-hijack and deadline tests**

```go
func TestWorkerDrainWaitsForWebSocketThenClosesGeneration(t *testing.T) {
	fixture := activeWorkerWithWebSocket(t)
	done := make(chan error, 1)
	go func() { done <- fixture.Drain(context.Background()) }()
	assertNotClosed(t, done)
	fixture.CloseWebSocket()
	if err := <-done; err != nil { t.Fatal(err) }
	fixture.AssertGenerationClosedOnce()
}

func TestRetirementLoopTreatsDrainIPCFailureAsObservation(t *testing.T) {
	fixture := finalizedRetirementFixture(t, 60, 61)
	fixture.predecessor.drainErr = io.ErrClosedPipe
	if err := fixture.supervisor.runNextRetirement(context.Background()); err != nil {
		t.Fatalf("runNextRetirement() = %v; retirement failures are observed, not returned", err)
	}
	fixture.AssertWorkerTerminated(60)
	fixture.AssertAuditReason(60, lifecycle.ReasonWorkerExited)
	fixture.AssertActiveWorker(61)
	fixture.AssertPublishedRevision(61)
}

func TestRetirementLoopDrainTimeoutTerminatesThenKills(t *testing.T) {
	fixture := finalizedRetirementFixture(t, 62, 63)
	fixture.predecessor.blockDrain = true
	fixture.supervisor.policy.DrainTimeout = time.Second
	fixture.supervisor.policy.TerminateGrace = time.Second
	fixture.clock.Advance(2 * time.Second)
	if err := fixture.supervisor.runNextRetirement(context.Background()); err != nil { t.Fatal(err) }
	fixture.AssertTerminateThenKill(62)
	fixture.AssertAuditReason(62, lifecycle.ReasonDrainTimeout)
}
```

Also cover ordinary requests, HTTP/2 streams, raw hijacks, every registered HTTP runtime exactly once, generation-owned goroutines, task ignoring cancellation, drain deadline residual names/counts, and repeated Drain/Stop idempotency. Retirement cases cover natural drain, DRAIN send failure, worker exit before status, deadline, terminate failure, kill/reap, duplicate queue entries, shutdown cancellation, and audit/status continuity while the replacement remains active.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/worker ./pkg/supervisor ./pkg/server ./pkg/runtime ./pkg/compiler -run "^(TestWorkerDrain|TestRetirementLoop|TestRouteHandler|TestTaskRegistryDrain|TestPreparedGenerationClose)" -count=1'`

Expected: FAIL because no worker-wide drain owner joins all resource classes.

- [ ] **Step 3: Implement one ordered drain**

Worker drain order is close accept gates → `ListenerSet.Shutdown`, which invokes each registered `HTTPRuntime.Shutdown` and waits its `Serve` call → wait ordinary request count → wait hijack registry → cancel/join generation tasks → `PreparedGeneration.Close`. This makes plan 06 listener-local servers part of the same worker lifecycle instead of an unowned goroutine. Natural dynamic retirement does not forcibly close hijacks. When `DrainTimeout` expires, worker reports exact residual owner names/counts.

`Supervisor.Run` owns a retirement loop waiting on `retireCond`. It pops one `retirementRecord`, sends DRAIN, waits at most `DrainTimeout` for STOPPED/process exit, sends process-group termination on timeout or IPC failure, waits `TerminateGrace`, then kills and reaps if still live. DRAIN IPC failure, timeout, terminate failure, and forced kill are recorded through `AuditSink` and status/telemetry with stable reasons; they are retirement observations and never become a `FinalizeActivation` error or roll back the committed active generation. The record remains owned until the predecessor has exited and all supervisor-side FDs/capabilities are closed. Supervisor shutdown drains or forcibly retires every queued predecessor before closing original listeners.

- [ ] **Step 4: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/worker ./pkg/supervisor ./pkg/server ./pkg/runtime ./pkg/compiler -run "^(TestWorkerDrain|TestRetirementLoop|TestRouteHandler|TestTaskRegistryDrain|TestPreparedGenerationClose|TestHijack)" -count=1'`

Expected: PASS.

```bash
git add pkg/worker pkg/supervisor pkg/server pkg/runtime pkg/compiler
git commit -m "feat(worker): drain requests hijacks and owned tasks"
```

### Task 7: Add Crash Probation, Bounded Restart, Rollback, and Terminal Readiness

**Files:**
- Create: `pkg/supervisor/restart.go`, `restart_test.go`
- Modify: `pkg/supervisor/supervisor.go`, `supervisor_test.go`
- Modify: `pkg/observability/metrics/config_apply.go`, `config_apply_test.go`

**Interfaces:**
- Consumes: `LifecyclePolicy` restart fields, committed `RevisionSet` plus `PublishedGeneration` artifacts loaded through `WorkerCompilerFactory.PrepareRecovery`, worker exit/status.
- Produces: deterministic restart ledger and terminal `lifecycle.Status` projection.

- [ ] **Step 1: Write failing crash-loop tests**

```go
func TestSupervisorCrashLoopRollsBackThenBecomesTerminal(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	s := newSupervisorWithPolicy(t, clock, config.LifecyclePolicy{MaxRestarts: 2,
		RestartWindow: time.Minute, ProbationPeriod: 30 * time.Second})
	s.SetPublished(70); s.ActivateCandidate(71)
	s.Crash(71); s.Crash(71); s.Crash(71)
	if got := s.Status(); got.Generation != 70 || !got.Ready { t.Fatalf("rollback status = %+v", got) }
	s.Crash(70); s.Crash(70); s.Crash(70)
	if got := s.Status(); !got.Terminal || got.Ready || got.ReasonCode != "no-healthy-generation" {
		t.Fatalf("terminal status = %+v", got)
	}
}
```

Cover probation success reset, restart-window eviction, pre-READY crash, active crash, previous artifact corruption, restart cancellation, and stable health/metrics projection.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/supervisor ./pkg/observability/metrics -run "^(TestSupervisorCrash|TestLifecycleReadiness)" -count=1'`

Expected: FAIL because restart/probation ownership is absent.

- [ ] **Step 3: Implement bounded policy**

A candidate is probationary until `ProbationPeriod` elapses active without crash. Crashes inside `RestartWindow` consume the candidate budget; exhaustion rolls back to the last committed healthy predecessor. A restart or rollback of committed state sends only `LoadRecovery` with the predecessor's committed `RevisionSet` and verified published artifacts, then reconstructs the worker through `WorkerCompilerFactory.PrepareRecovery`; it never streams desired state into candidate preparation or reruns publication policy. If no predecessor can load/activate, status becomes terminal/unready with `no-healthy-generation`; no infinite restart goroutine remains.

- [ ] **Step 4: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/supervisor ./pkg/observability/metrics -run "^(TestSupervisorCrash|TestSupervisorProbation|TestLifecycleReadiness)" -count=1'`

Expected: PASS.

```bash
git add pkg/supervisor pkg/observability/metrics
git commit -m "feat(supervisor): bound worker restart and rollback"
```

### Task 8: Add Canonical Commands, PID 1 Behavior, and Control Adapters

**Files:**
- Create: `pkg/supervisor/control.go`, `control_test.go`
- Create: `cmd/supervisor.go`, `supervisor_test.go`, `worker.go`, `worker_test.go`, `lifecycle.go`, `lifecycle_test.go`
- Modify: `cmd/root.go`, `root_test.go`, `Dockerfile`

**Interfaces:**
- Consumes: `Supervisor.Execute`, platform signal/process adapter.
- Produces: `apisix run`, internal `apisix worker`, and `apisix lifecycle {status,reload,drain,stop}`.

- [ ] **Step 1: Write failing CLI/PID1 tests**

```go
func TestLifecycleCommandsSendCanonicalCommands(t *testing.T) {
	client := newRecordingControlClient()
	cmd := newLifecycleCommand(client)
	cmd.SetArgs([]string{"drain", "--timeout=15s"})
	if err := cmd.Execute(); err != nil { t.Fatal(err) }
	if got := client.Last(); got.Kind != lifecycle.CommandDrain || got.Deadline.IsZero() { t.Fatalf("command = %+v", got) }
}
```

Cover non-interactive worker bootstrap, invalid direct worker invocation, supervisor signal translation (TERM→stop, HUP→reload), child reaping, first-signal graceful shutdown, second-signal forced termination, stable JSON status, and unavailable control socket.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./cmd ./pkg/supervisor -run "^(TestLifecycleCommand|TestSupervisorCommand|TestPID1)" -count=1'`

Expected: FAIL because canonical commands do not exist.

- [ ] **Step 3: Implement adapters around canonical commands**

`run` is the default and starts supervisor. `worker` requires the inherited IPC FD and listener FDs, is hidden from operator help, reads `WorkerBootstrapInput` from the first authenticated HELLO, and rejects config paths, provider options, journal paths, and direct interactive invocation. It does not construct registries/compiler until config and manifest artifact digests verify. Unix signals, systemd/Kubernetes stop/reload, and launchd translate to `Supervisor.Execute`; they contain no independent lifecycle logic. Container entrypoint remains `/usr/bin/apisix` and default args become `run -c /usr/local/apisix/conf/config.yaml`.

- [ ] **Step 4: Run tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./cmd ./pkg/supervisor -run "^(TestLifecycleCommand|TestSupervisorCommand|TestPID1|TestControl)" -count=1'`

Expected: PASS.

```bash
git add cmd pkg/supervisor Dockerfile
git commit -m "feat(cli): expose canonical supervisor lifecycle commands"
```

### Task 9: Qualify Linux, macOS, and Experimental Windows Boundaries

**Files:**
- Create: `.github/workflows/platform.yml`
- Modify: `.github/workflows/ci.yml`
- Create: `scripts/smoke-supervisor.sh`, `scripts/test-supervisor-platform.sh`
- Modify: `Makefile`, `docs/architecture/supervisor-worker-lifecycle.md`

**Interfaces:**
- Consumes: platform build-tag implementations and exact built artifact.
- Produces: Linux supervisor/worker build-smoke evidence, native macOS build-smoke evidence, and a Windows source-build check. It publishes no release artifact; plan 08 owns immutable packaging and publication.

- [ ] **Step 1: Write the failing platform-matrix script test**

```bash
required='linux/amd64 linux/arm64 darwin/amd64 darwin/arm64'
for target in $required; do
  grep -q "$target" .cache/tmp/built-targets.txt
done
grep -q 'windows/amd64 source-build' .cache/tmp/checked-targets.txt
test ! -e .cache/tmp/published-targets.txt
```

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && scripts/test-supervisor-platform.sh'`

Expected: FAIL because the supervisor platform matrix is absent.

- [ ] **Step 3: Implement build/smoke gates**

Platform CI builds Linux amd64/arm64 and macOS amd64/arm64 without publishing them. Linux smoke execs supervisor+worker and performs READY/activate/request/drain; macOS jobs run the same smoke natively on each architecture. Nightly/release validation cross-builds `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`, asserts the explicit lifecycle unsupported error in a compiled test, and uploads no Windows artifact. Plan 08 later packages only the exact qualified subjects.

- [ ] **Step 4: Run local script and commit**

Run: `bash -lc 'source .envrc && scripts/test-supervisor-platform.sh'`

Expected: PASS with the local host subset executed and unavailable native architectures reported as scheduled requirements, not local passes.

```bash
git add .github/workflows/platform.yml .github/workflows/ci.yml scripts Makefile docs/architecture/supervisor-worker-lifecycle.md
git commit -m "ci(platform): qualify supervisor worker targets"
```

### Task 10: Atomically Cut Startup to Supervisor and Delete the Old Runtime Path

**Files:**
- Modify: `pkg/server/server.go`, `server_test.go`, `cmd/root.go`, `root_test.go`
- Delete: `pkg/server/reload.go`, `reload_test.go`, `generation_engine.go`, `generation_engine_test.go`
- Modify: `pkg/supervisor/supervisor.go`, `pkg/worker/bootstrap.go`

**Interfaces:**
- Consumes: Tasks 1–9 complete supervisor/worker/platform path.
- Produces: the only production path `CLI → Supervisor → provider/journal → exec Worker → READY → reversible activation → finalize/drain`.

- [ ] **Step 1: Write the failing end-to-end ownership test**

```go
func TestRunCommandHasOneProviderJournalAndActiveWorker(t *testing.T) {
	fixture := startRunCommand(t)
	fixture.AssertSupervisorOwnsProviderAndJournal()
	fixture.AssertWorkerCannotOpenJournal()
	fixture.AssertActiveWorkerCount(1)
	fixture.Reload(92)
	fixture.AssertPublishedAndServingRevision(92)
}
```

Also test startup failure cleanup, offline published startup, activation rollback, graceful stop, forced drain, and exact provider ack fencing.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./cmd ./pkg/supervisor ./pkg/worker ./pkg/server -run "^(TestRunCommand|TestSupervisorStartup|TestOfflinePublished)" -count=1'`

Expected: FAIL while `Server.Start` remains a production owner.

- [ ] **Step 3: Switch ownership and delete old symbols in one change**

Move remaining worker-local serve helpers out of old `Server.Start`, delete both reload files, delete `pkg/server/generation_engine.go` and its tests, and remove every production caller of `server.GenerationEngine`. `worker.Bootstrap` directly owns worker-local `compiler.Compiler`/`PreparedGeneration`; `supervisor.Supervisor` directly implements `generation.PublicationEngine`. Do not replace the deleted engine with a delegating facade, test-only wrapper, type alias, flag, or environment branch; tests use supervisor/worker fixtures.

- [ ] **Step 4: Prove absence and run race gate**

```bash
! rg -n 'Server\.Start|server\.GenerationEngine|type GenerationEngine|NewGenerationEngine|startConfigProvider|startEtcdWatcher|startHTTPListeners|listenReloadEvent|runReloadScheduler|SendReloadEvent|singleProcess|inProcess|legacySupervisor' cmd pkg --glob '*.go'
! rg -n 'Bootstrap[[:space:]]+WorkerBootstrapInput' pkg/lifecycle --glob '*.go'
bash -lc 'source .envrc && go test -race ./pkg/generation ./pkg/compiler ./pkg/runtime ./pkg/lifecycle ./pkg/platform ./pkg/supervisor ./pkg/worker ./pkg/server ./cmd -run "^(TestCoordinator|TestPreparedGeneration|TestTaskRegistry|TestLifecycle|TestIPC|TestSupervisor|TestWorker|TestActivation|TestDrain|TestRollback|TestControl|TestRunCommand)" -count=1'
```

Expected: both commands PASS.

- [ ] **Step 5: Commit the atomic cutover**

```bash
git add -A cmd pkg/server pkg/supervisor pkg/worker
git commit -m "refactor(runtime): cut startup over to supervisor workers"
```

### Task 11: Document the Contract and Run the Milestone Gate

**Files:**
- Modify: `docs/architecture/supervisor-worker-lifecycle.md`, `docs/design.md`, `README.md`
- Modify: `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program.md` only if implemented signatures differ from the shared program block.

**Interfaces:**
- Consumes: implemented lifecycle/config/IPC/status/platform contracts.
- Produces: plan 06 worker-listener contract and plan 07 stable status/telemetry contract.

- [ ] **Step 1: Record exact state, frame, ownership, and platform tables**

The architecture document copies the implemented config defaults, frame layout, artifact begin/chunk/end limits and digest rules, secret-grant request/reply redaction contract, the single first-HELLO `WorkerBootstrapInput` source and bootstrap-free LOAD shape, command/state transition table, revision-fence comparison, FD ownership/close table, prepare/discard/activate/rollback/commit/finalize order, asynchronous retirement order, `HTTPRuntime` registration/drain ownership, crash policy, PID 1 behavior, and Linux/macOS/Windows qualification matrix. It explicitly states there is no alternate single-process mode.

- [ ] **Step 2: Run focused milestone verification**

```bash
bash -lc 'source .envrc && go test -race ./pkg/supervisor ./pkg/worker ./pkg/lifecycle ./pkg/platform ./pkg/runtime ./pkg/server ./cmd -run "^(TestSupervisor|TestWorker|TestActivation|TestDrain|TestRollback|TestControl|TestLifecycle|TestIPC|TestTaskRegistry|TestRouteHandler|TestPID1)" -count=1'
bash -lc 'source .envrc && make build'
bash -lc 'source .envrc && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...'
git diff --check
```

Expected: PASS; unavailable native macOS architecture smoke remains a required CI/release job and is not claimed from another host.

- [ ] **Step 3: Re-run removal and config ownership scans**

```bash
! rg -n 'Server\.Start|server\.GenerationEngine|type GenerationEngine|NewGenerationEngine|startConfigProvider|startEtcdWatcher|startHTTPListeners|listenReloadEvent|runReloadScheduler|SendReloadEvent|singleProcess|inProcess|legacySupervisor' cmd pkg --glob '*.go'
! rg -n 'Bootstrap[[:space:]]+WorkerBootstrapInput' pkg/lifecycle --glob '*.go'
rg -n 'apisix_go\.runtime\.lifecycle|APISIXGO_RUNTIME_LIFECYCLE' pkg/config docs/architecture/supervisor-worker-lifecycle.md
```

Expected: first command has no output; second shows every canonical key/alias in loader tests and the architecture table.

- [ ] **Step 4: Commit documentation**

```bash
git add docs/architecture/supervisor-worker-lifecycle.md docs/design.md README.md docs/superpowers/plans/2026-08-23-apisix-go-convergence-program.md
git commit -m "docs(runtime): specify supervisor worker lifecycle"
```

## Plan Self-Review

- Spec coverage: single supervisor/single active worker, exec, bounded digest-verified framed Unix IPC, cross-exec bootstrap, scoped secret request/reply, FD ownership, READY/revision fencing, prepared discard, reversible publication activation, infallible finalize plus asynchronous retirement, listener-runtime drain, hijacks/tasks, crash probation, PID 1, commands/adapters, platform matrix, and atomic legacy removal each have a named red/green task.
- Config coverage: `EffectiveConfig.Runtime RuntimePolicy`, `LifecyclePolicy`, all 11 canonical `apisix_go.runtime.lifecycle.*` keys, matching `APISIXGO_RUNTIME_LIFECYCLE_*` aliases, defaults, ranges and provenance tests are explicit.
- Interface consistency: `generation.PublicationEngine` signatures match plan 03, including `DiscardPrepared`; the first HELLO is the only `WorkerBootstrapInput` source and `LoadRequest` has no bootstrap field; `compiler.PreparedGeneration` ownership is unchanged; plan 06 receives exact `worker.HTTPRuntime` and `ListenerSet` methods; plan 07 receives exact lifecycle status/audit/worker telemetry interfaces and `ReasonMemoryPressureHard`.
- Ownership consistency: supervisor alone owns provider, journal, keyring/secret grant, listener originals, process state, retirement queue and stable readiness; workers locally construct registries/compiler and own immutable compiled generations, duplicated descriptors, HTTP runtimes, request/hijack/task lifetimes and request telemetry. LOAD/READY never carry a full generation object in one frame.
- Platform consistency: Unix-specific APIs stay under build tags; Linux is production, macOS artifacts require native smoke, Windows is source-build experimental with explicit unsupported lifecycle and no artifact.
- Verification consistency: Go/Make commands source `.envrc`, tests are impact-scoped, race checks cover concurrent ownership, and no full test aggregation is used.
- Completeness: every task names exact files, consumed/produced interfaces, failing test, red command, implementation contract, green command and commit; the final scans reject an alternate runtime path.
