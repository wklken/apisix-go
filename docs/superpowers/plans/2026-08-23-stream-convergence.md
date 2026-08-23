# APISIX 3.17 Stream Convergence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Converge the APISIX stream data plane on immutable, independently published stream generations with complete TCP/TLS/mTLS/PROXY behavior, general stream plugin chaining, explicit protocol ownership, bounded lifecycle, and digest-bound qualification evidence.

**Architecture:** Static stream listener addresses are bound once by the supervisor and inherited by workers; generation compilation produces immutable listener policies, route plans, plugin chains, TLS material, and a `compiler.StreamSnapshot`. `generation.DomainStream` publishes, rolls back, and reports readiness independently from HTTP while the worker owns accepted connections and drains them under the existing lifecycle fence. Stream qualification extends the plan 08 evidence framework without altering the already immutable HTTP qualification bundle.

**Tech Stack:** Go 1.26, `net`, `crypto/tls`, `crypto/x509`, `encoding/binary`, existing generation/compiler/runtime/task/telemetry packages, Cobra lifecycle commands, Docker/real Kafka/MQTT/Dubbo fixtures, GitHub Actions qualification workflow.

**Spec:** `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`

## Global Constraints

- Execute this plan only after Task 8 has produced a passing, immutable HTTP qualification bundle and promoted digest. Never rewrite or append files inside that bundle.
- Compatibility target remains APISIX `3.17.0` at source commit `9ef2ecab67f652d38365049613610ef649bb4ad0` with the digest-qualified official oracle from `qualification/oracle.yaml`.
- Use the exact `generation.DomainStream`, `generation.Journal`, `generation.PublicationEngine`, `compiler.PreparedGeneration`, `compiler.StreamSnapshot`, `runtime.RuntimeDependencies`, `runtime.TaskRegistry`, supervisor listener/fence/status interfaces, telemetry interfaces, and qualification bundle interfaces defined by plans 03–08.
- HTTP and stream have independent published revisions, decisions, readiness, rollback, and recovery. A stream-only failure cannot replace or roll back a valid HTTP publication; a valid HTTP revision cannot make a required stream domain ready.
- First startup with no valid stream predecessor fails closed. A later invalid stream resource may use per-resource last-good only when the journal supplies a valid predecessor and records the exact `ResourceDecision`. Explicit delete is never resurrected as last-good.
- Listener addresses are static effective configuration. The supervisor binds them once; workers inherit descriptors/FDs and never call `net.Listen` for production listeners. TLS policy, routes, plugins, and upstream connections are generation-owned.
- Linux `amd64/arm64` is the production stream platform. Native macOS `amd64/arm64` runs stream smoke. Windows remains source-buildable experimental with no official artifact. UDP/QUIC, NGINX/OpenResty stream phases, Lua filters/scripts, external plugin runners, shared-dict exactness, and kernel eBPF/socket-routing behavior remain explicit deferred native/runtime gaps.
- No new global config/store/plugin/TLS/metric singleton is allowed. Secrets use `secret.Materializer`; shared clients use `runtime.ResourceRegistry`; all goroutines use `runtime.TaskRegistry` or request/connection-owned task groups.
- Every Go/Make command runs through `bash -lc 'source .envrc && ...'`. Verification is impact-scoped; real-process stream integration cases run one selector at a time.
- Decision 196C applies: atomically delete the mutable `Server.startStreamProxy`/`stream.Runtime.Reload` path, MQTT-only router construction, direct listener ownership, and their tests. No legacy adapter, fallback switch, or dual publication path remains.

---

## File and Responsibility Map

**Create:**

- `pkg/stream/types.go` — stable listener, connection, phase, owner, route-plan, and result vocabulary.
- `pkg/stream/listener.go`, `listener_test.go` — listener policy compilation plus downstream TLS/mTLS and PROXY parsing.
- `pkg/stream/chain.go`, `chain_test.go` — deterministic phase/priority/scope execution.
- `pkg/stream/tls.go`, `tls_test.go` — downstream SNI certificate selection and upstream trust/SNI/client-certificate compilation.
- `pkg/stream/connection_registry.go`, `connection_registry_test.go` — generation connection ownership and bounded drain snapshots.
- `pkg/stream/protocol.go`, `protocol_test.go` — raw TCP owner and protocol-owner boundary validation.
- `pkg/compiler/stream.go`, `stream_test.go` — dependency-closed immutable stream compilation.
- `pkg/worker/stream.go`, `stream_test.go` — inherited listener activation, accept, serve, quiesce, and drain.
- `scripts/stream_qualification.sh`, `stream_qualification_test.sh` — differential, real-dependency, outage/recovery, capacity, and digest contract.
- `t/plugin/stream-convergence.yaml` — pinned APISIX stream cases and source-block mapping.
- `docs/architecture/stream-convergence.md` — operator-visible contracts and deferred gaps.

**Modify:**

- `pkg/config/types.go`, `effective.go`, `validation.go` and focused tests — presence-aware listener policy validation without a parallel Go-only namespace.
- `pkg/resource/route.go`, `ssl.go` and tests — retain exact stream route, TLS, client certificate, SNI, and provenance fields.
- `pkg/plugin/descriptor.go`, `descriptor_test.go` — add stream phase constants while retaining one manifest-owned descriptor.
- `pkg/plugin/mqtt_proxy/{plugin.go,stream.go}` and tests — implement the stream protocol-owner interface without owning listeners.
- `pkg/plugin/kafka_proxy`, `pkg/plugin/dubbo_proxy`, `pkg/plugin/http_dubbo`, `pkg/plugin/dubbo` tests/docs — enforce HTTP/protocol boundaries; do not move them into the L4 chain.
- `pkg/compiler/{compiler.go,normalize.go,materialize.go,types.go}` and tests — compile stream closure into `PreparedGeneration`.
- `pkg/supervisor/supervisor.go`, `publication_test.go`, and `pkg/worker/bootstrap.go`, `bootstrap_test.go` — retain pending candidates by publication identity, discard on Stage failure, bind tokens only during activation, and enqueue predecessor retirement after commit.
- `pkg/supervisor/{listeners.go,activation.go}` and tests — bind/duplicate stream listeners and fence `DomainStream` independently.
- `pkg/worker/bootstrap.go` and tests — serve/drain stream runtime beside HTTP with shared worker lifecycle.
- `pkg/telemetry` and focused tests — bounded stream metrics through the existing reporter/aggregator.
- `pkg/capability/manifest.yaml`, `cmd/capability-gen`, `t/plugin/corpus_scope.yaml` — honest stream evidence and qualification profile.
- `pkg/qualification`, `.github/workflows/qualification.yml`, `qualification/policy.json` — stream gates bound to the candidate digest.
- `docs/design.md`, `docs/plugins.md`, `README.md` — generated/current stream status and boundaries.

**Delete during Task 10:**

- `streamRuntimeOwner`, `Server.streamRuntime`, `Server.streamRoutes`, `Server.streamReloadMu`, `startStreamProxy`, `reloadStreamRoutes`, `reloadStreamRoutesIfStarted`, `closeStartedStreamRuntime`, and their mutable-reload tests from `pkg/server/server.go` and `pkg/server/stream_test.go`.
- `(*stream.Router).Reload`, `(*stream.Runtime).Reload`, listener creation in `stream.NewRuntime`, and direct `mqtt_proxy.ServeListener` ownership after equivalent immutable/worker-owned coverage exists.
- Global-store/config reads and `store.CommitStreamRouteLastGood` calls on the stream serving path.

## Stable Interfaces

The following additions preserve every fixed signature from plans 03–08. `compiler.StreamSnapshot.Revision()` and `Router()` remain unchanged; additions are read-only.

```go
// package plugin — additive constants on the existing Phase type
const (
	PhaseStreamPreread Phase = "stream_preread"
	PhaseStreamAccess  Phase = "stream_access"
	PhaseStreamContent Phase = "stream_content"
	PhaseStreamLog     Phase = "stream_log"
)

// package stream
type ListenerTransport string
const (
	TransportTCP ListenerTransport = "tcp"
	TransportTLS ListenerTransport = "tls"
)

type ProxyProtocolMode string
const (
	ProxyProtocolOff    ProxyProtocolMode = "off"
	ProxyProtocolAccept ProxyProtocolMode = "accept"
	ProxyProtocolEmit   ProxyProtocolMode = "emit"
)

type ClientAuthMode string
const (
	ClientAuthNone    ClientAuthMode = "none"
	ClientAuthRequire ClientAuthMode = "require"
)

type ListenerPolicy struct {
	ID                string
	Address           string
	Transport         ListenerTransport
	ProxyProtocol     ProxyProtocolMode
	ProxyToUpstream   bool
	TLS               *tls.Config
	ClientAuth        ClientAuthMode
}

type UpstreamTLSPlan struct {
	Enabled           bool
	ServerName        string
	Verify            bool
	RootCAs           *x509.CertPool
	ClientCertificate *tls.Certificate
}

type Phase string
const (
	PhasePreread Phase = "preread"
	PhaseAccess  Phase = "access"
	PhaseContent Phase = "content"
	PhaseLog     Phase = "log"
)

type Connection struct {
	Client         net.Conn
	ListenerID     string
	Source         netip.AddrPort
	Destination    netip.AddrPort
	ServerName     string
	RouteID        string
	Protocol       string
	ClientID       string
	Upstream       net.Conn
	Attributes     map[string]string
}

type Decision string
const (
	DecisionContinue Decision = "continue"
	DecisionReject   Decision = "reject"
)

type Plugin interface {
	RunStream(context.Context, Phase, *Connection) (Decision, error)
}

type Binding struct {
	Plugin      Plugin
	Descriptor  plugin.Descriptor
	Priority    int
	Scope       plugin.Scope
	Provenance  plugin.ResourceProvenance
	InstanceKey plugin.InstanceKey
}

type Dialer func(context.Context, *Connection) (net.Conn, error)

type ProtocolOwner interface {
	Protocol() string
	Serve(context.Context, *Connection, Dialer) error
}

type RoutePlan struct {
	ID          string
	ListenerID  string
	RemoteNets  []netip.Prefix
	Bindings    []Binding
	Owner       ProtocolOwner
	Dial        Dialer
}

type Chain struct {
	preread []Binding
	access  []Binding
	content []Binding
	log     []Binding
	owner   ProtocolOwner
}

type Result struct {
	RouteID  string
	Listener string
	Remote   string
	ClientID string
	Protocol string
	Err      error
	BytesIn  uint64
	BytesOut uint64
	Duration time.Duration
	ErrCode  string
}

type Router struct {
	byListener map[string][]RoutePlan
}
func CompileRouter([]RoutePlan) (*Router, error)
func (r *Router) Serve(context.Context, string, net.Conn) Result
func (c *Chain) Serve(context.Context, *Connection, Dialer) Result

type ConnectionResidual struct {
	RouteID  string
	Protocol string
}
type DrainSnapshot struct {
	Active    int
	Residuals []ConnectionResidual
}
type ownedConnection struct {
	connection *Connection
}
type ConnectionRegistry struct {
	mu        sync.Mutex
	accepting bool
	nextID    uint64
	active    map[uint64]ownedConnection
}
func NewConnectionRegistry() *ConnectionRegistry
func (r *ConnectionRegistry) Admit(*Connection) (release func(), err error)
func (r *ConnectionRegistry) Quiesce()
func (r *ConnectionRegistry) Drain(context.Context) (DrainSnapshot, error)

func CompileListenerPolicies(*config.EffectiveConfig, []resource.SSL, secret.Materializer) ([]ListenerPolicy, error)
func CompileUpstreamTLS(resource.StreamRoute, *config.EffectiveConfig, []resource.SSL, secret.Materializer) (UpstreamTLSPlan, error)
func CompileChain([]Binding, ProtocolOwner) (*Chain, error)
func CompileProtocolOwner(string, plugin.Plugin) (ProtocolOwner, error)
```

`Phase` is the connection lifecycle; the additive `plugin.PhaseStream*` values are descriptor/manifest vocabulary. Compilation maps them exactly (`stream_preread→preread`, `stream_access→access`, `stream_content→content`, `stream_log→log`) and rejects every HTTP phase on a stream binding. Ordering is phase order, descending effective priority, canonical factory, then provenance. `Chain.Serve` runs preread then access then content filters, invokes its one protocol owner, and runs the reverse-ordered log slice exactly once through a single deferred finalization path after success, reject, owner error, cancellation, or recovered plugin panic. Content has exactly one owner; filters cannot replace it.

```go
// package compiler — plan 04's exact fields plus one additive immutable slice
type StreamSnapshot struct {
	artifact         generation.GenerationArtifact
	router           *stream.Router
	listenerPolicies []stream.ListenerPolicy
}
func (s *StreamSnapshot) Revision() uint64
func (s *StreamSnapshot) Router() *stream.Router
func (s *StreamSnapshot) ListenerPolicies() []stream.ListenerPolicy

// package stream
type Runtime struct {
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	listeners map[string]net.Listener
	policies  map[string]ListenerPolicy
	revision  uint64
	digest    [32]byte
	router    *Router
	registry  *ConnectionRegistry
	tasks     *runtime.TaskRegistry
	admission *runtime.AdmissionController
	reporter  *telemetry.Reporter
	active    bool
	closed    bool
}
func NewRuntime(
	context.Context,
	map[string]net.Listener,
	uint64,
	[32]byte,
	[]ListenerPolicy,
	*Router,
	*runtime.TaskRegistry,
	*runtime.AdmissionController,
	*telemetry.Reporter,
) (*Runtime, error)
func (r *Runtime) Activate(lifecycle.RevisionFence) error
func (r *Runtime) Quiesce() error
func (r *Runtime) Drain(context.Context) (DrainSnapshot, error)
func (r *Runtime) Close(context.Context) error

// package worker — additive ownership transfer on plan 05 ListenerSet.
func (s *ListenerSet) Claim([]string) (map[string]net.Listener, error)
```

`StreamSnapshot.ListenerPolicies()` returns a slice copy and clones each non-nil `tls.Config`. Before construction, worker bootstrap calls `ListenerSet.Claim` once with the sorted static stream listener IDs. `Claim` atomically rejects unknown IDs, duplicate IDs, any ID already registered through `RegisterHTTP`, a second claim, and calls after `Serve` begins; on success it removes those listeners from the `ListenerSet` close/shutdown set and transfers the returned map to `stream.Runtime`. `NewRuntime` owns and eventually closes that exact map; it does not import `pkg/compiler`, bind, mutate a router, load resources, or read config/store globals. `Activate` starts accepts only when its argument's `Stream` revision and `ArtifactDigests[generation.DomainStream]` equal the runtime revision/digest. `Quiesce` stops new accepts without closing established connections. `Drain` stops accepts, drains connections and waits serve tasks; `Close` closes every claimed listener exactly once before `PreparedGeneration.Close` stops tasks and releases resources.

The existing cross-plan interfaces remain exact:

```go
// package generation
const DomainStream Domain = "stream"
type PublicationEngine interface {
	Prepare(context.Context, ApplyTicket, Snapshot, map[Domain]PublishedGeneration) (PublicationSet, error)
	DiscardPrepared(context.Context, PublicationSet) error
	Activate(context.Context, PublicationToken, PublicationSet) error
	RollbackActivation(context.Context, PublicationToken, PublicationSet) error
	FinalizeActivation(context.Context, PublicationToken, PublicationSet)
}

// package compiler
func (c *Compiler) Prepare(context.Context, generation.ApplyTicket, generation.Snapshot,
	map[generation.Domain]generation.PublishedGeneration) (*PreparedGeneration, error)
func (p *PreparedGeneration) Stream() (*StreamSnapshot, bool)

// package qualification
func Evaluate(*capability.Manifest, *Result) error
func WriteBundle(string, *Result, []string) (*BundleManifest, error)
func VerifyBundle(string) (*Result, error)
```

### Task 1: Validate the Static Stream Listener Matrix Without Creating a Parallel Namespace

**Files:**
- Modify: `pkg/config/types.go`, `effective.go`, `validation.go`
- Modify: `pkg/config/effective_test.go`, `release_gate_test.go`
- Modify: `conf/config-default.yaml`, `docs/configuration.md`
- Create: `pkg/stream/listener.go`, `listener_test.go`

**Interfaces:** Consumes plan 02 `config.EffectiveConfig` and existing `apisix.stream_proxy.tcp`. Produces `stream.ListenerPolicy` and `CompileListenerPolicies` exactly as declared above.

- [ ] **Step 1: Write failing presence-aware listener matrix tests**

```go
func TestCompileListenerPoliciesCoversTCPAndTLSProxyMatrix(t *testing.T) {
	effective := streamEffectiveFixture(t, []config.TcpListen{
		{Addr: "127.0.0.1:9000"},
		{Addr: "127.0.0.1:9443", Tls: true},
		{Addr: "127.0.0.1:9001", ProxyProtocol: true},
		{Addr: "127.0.0.1:9002", ProxyProtocolToUpstream: true},
	})
	policies, err := CompileListenerPolicies(effective, streamSSLFixtures(t), fixtureMaterializer(t))
	if err != nil { t.Fatal(err) }
	if len(policies) != 4 { t.Fatalf("policies = %d", len(policies)) }
	if policies[1].Transport != TransportTLS || policies[2].ProxyProtocol != ProxyProtocolAccept || !policies[3].ProxyToUpstream {
		t.Fatalf("listener matrix = %#v", policies)
	}
}
```

Add tables for duplicate normalized addresses, UDP, empty/wildcard-invalid address, missing TLS certificate, conflicting exact/wildcard SNI, invalid client CA, unsupported TLS version, and qualification requiring non-empty listeners when stream is selected.

- [ ] **Step 2: Run the listener tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/config ./pkg/stream -run "^(TestCompileListenerPolicies|TestStreamListenerValidation)" -count=1'`

Expected: FAIL because `ListenerPolicy` and compilation do not exist.

- [ ] **Step 3: Implement exact APISIX keys and deterministic IDs**

Keep external keys at `apisix.stream_proxy.tcp[].{addr,tls,proxy_protocol,proxy_protocol_to_upstream}` and SSL resources under `ssls`; do not add `apisix_go.stream`. Normalize address with `netip.ParseAddrPort`/`net.SplitHostPort`, derive listener ID as `"stream/tcp/" + normalizedAddress`, sort by ID, and reject duplicates. Qualification rejects UDP; compatibility reports it unsupported rather than ignoring it.

- [ ] **Step 4: Compile downstream TLS and mTLS**

Build one immutable `tls.Config` with `MinVersion=tls.VersionTLS12`, `GetConfigForClient` exact-SNI before longest wildcard selection, and resource-owned certificates. `resource.SSL.Client` selects `ClientAuthRequire`, client CA pool, and verified chains. Materialize certificate/key fields with generation/resource/field scope; never include plaintext in an error or digest.

- [ ] **Step 5: Run focused tests and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/config ./pkg/resource ./pkg/stream -run "^(TestCompileListenerPolicies|TestStreamListenerValidation|TestStreamTLS|TestStreamMTLS)" -count=1'`

Expected: PASS.

```bash
git add pkg/config pkg/resource pkg/stream/listener.go pkg/stream/listener_test.go conf/config-default.yaml docs/configuration.md
git commit -m "feat(stream): compile listener security policies"
```

### Task 2: Parse and Emit PROXY Protocol Before TLS and Routing

**Files:**
- Create: `pkg/stream/proxy_protocol.go`, `proxy_protocol_test.go`
- Modify: `pkg/stream/listener.go`, `listener_test.go`

**Interfaces:** Consumes `ListenerPolicy.ProxyProtocol`/`ProxyToUpstream`. Produces bounded PROXY v1/v2 parsing and upstream v1 emission inside the stream listener/dial path; no public API is added.

- [ ] **Step 1: Write failing ordering and trust tests**

```go
func TestProxyHeaderIsParsedBeforeTLSHandshakeAndRouteMatch(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	go func() {
		_, _ = io.WriteString(client, "PROXY TCP4 198.51.100.7 203.0.113.9 4567 443\r\n")
		_, _ = client.Write(clientHelloFixture(t, "mqtt.example.test"))
	}()
	meta, wrapped, err := acceptProxyHeader(context.Background(), server, 108)
	if err != nil { t.Fatal(err) }
	if meta.Source.String() != "198.51.100.7:4567" { t.Fatalf("source = %s", meta.Source) }
	assertStartsWithTLSClientHello(t, wrapped)
}
```

Cover v1 `UNKNOWN`, v2 LOCAL/PROXY, IPv4/IPv6, truncated/oversized header, unsupported family/transport, timeout, header on disabled listener, and upstream header preceding application/TLS bytes.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/stream -run "^TestProxy" -count=1'`

Expected: FAIL because the parser is absent.

- [ ] **Step 3: Implement the bounded state machine**

Use a 108-byte v1 maximum, the v2 16-byte fixed header plus declared payload capped at 536 bytes, `io.ReadFull`, deadline from the accept context, and a buffered connection that preserves unread bytes. Reject datagram/UNIX families. Parsing happens before TLS; upstream emission happens after target selection and before TLS handshake or application bytes.

- [ ] **Step 4: Run focused tests and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/stream -run "^(TestProxy|TestCompileListenerPolicies)" -count=1'`

Expected: PASS.

```bash
git add pkg/stream/proxy_protocol.go pkg/stream/proxy_protocol_test.go pkg/stream/listener.go pkg/stream/listener_test.go
git commit -m "feat(stream): enforce proxy protocol boundaries"
```

### Task 3: Compile an Immutable Dependency-Closed Stream Snapshot

**Files:**
- Create: `pkg/compiler/stream.go`, `stream_test.go`
- Modify: `pkg/compiler/compiler.go`, `normalize.go`, `materialize.go`, `types.go`
- Modify: `pkg/compiler/compiler_test.go`, `materialize_test.go`
- Modify: `pkg/supervisor/supervisor.go`, `publication_test.go`
- Modify: `pkg/worker/bootstrap.go`, `bootstrap_test.go`
- Modify: `pkg/generation/dependency.go`, `dependency_test.go`

**Interfaces:** Consumes exact plan 03 `Snapshot`, `PublishedGeneration`, `ResourceDecision`, `PublicationSet`, and `PublicationEngine` including `DiscardPrepared`; plan 04 `Compiler.Prepare`/`PreparedGeneration`; and plan 05 `supervisor.Supervisor` publication ownership. Produces additive `StreamSnapshot.ListenerPolicies()` while retaining `Revision()`/`Router()`. It does not recreate or extend `server.GenerationEngine`.

- [ ] **Step 1: Write failing independent-domain publication tests**

```go
func TestPrepareKeepsHTTPAndPublishesIndependentStreamRevision(t *testing.T) {
	previous := publishedFixture(t, generation.DomainHTTP, 40)
	ticket, desired := streamDesiredFixture(t, 41)
	prepared, err := compilerFixture(t).Prepare(context.Background(), ticket, desired,
		map[generation.Domain]generation.PublishedGeneration{generation.DomainHTTP: previous})
	if err != nil { t.Fatal(err) }
	streamSnapshot, ok := prepared.Stream()
	if !ok || streamSnapshot.Revision() != 41 { t.Fatalf("stream snapshot = %#v/%t", streamSnapshot, ok) }
	set := prepared.PublicationSet()
	if _, changed := set.Domains[generation.DomainHTTP]; changed { t.Fatal("stream change republished HTTP") }
}

func TestStreamCandidateStageFailureDiscardsPendingWorker(t *testing.T) {
	fixture := supervisorPublicationFixture(t)
	fixture.journal.StageErr = errStage
	err := fixture.coordinator.Apply(context.Background(), fixture.ticket, fixture.desired)
	if !errors.Is(err, errStage) { t.Fatalf("error = %v", err) }
	if fixture.candidate.StopCount() != 1 || fixture.candidate.WaitCount() != 1 {
		t.Fatalf("candidate cleanup = stop:%d wait:%d", fixture.candidate.StopCount(), fixture.candidate.WaitCount())
	}
	if fixture.candidate.ActivateCount() != 0 { t.Fatal("activated after Stage failure") }
}
```

Add cases for route→service→upstream→SSL/secret closure, missing dependency, invalid first startup, valid predecessor last-good, explicit delete, corrupt published stream artifact, stream failure leaving HTTP artifact untouched, `DiscardPrepared` publication-identity mismatch, repeated discard idempotence, and candidate STOP/kill/wait cleanup joined with the original Stage error.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/compiler ./pkg/generation ./pkg/supervisor ./pkg/worker -run "^(TestPrepareKeepsHTTP|TestStreamDependency|TestStreamPublication|TestStreamCandidateStageFailure)" -count=1'`

Expected: FAIL until stream compilation and closure are complete.

- [ ] **Step 3: Compile canonical immutable inputs**

Decode only the dependency-closed `stream_routes`, `services`, `upstreams`, `ssls`, `secrets`, plugin metadata, and enabled plugin list. Resolve references before plugin/TLS materialization, sort by resource key, clone all maps/bytes, compile listener policies/routes/chains, and compute the `GenerationArtifact` from canonical bytes. `StreamSnapshot.Router()` never exposes mutation.

- [ ] **Step 4: Enforce last-good/delete/security semantics**

On update, reuse a predecessor resource only when the desired resource is invalid, the predecessor is valid and not tombstoned, and its complete dependency closure still exists. Invalid/missing certs, client CAs, client keys, trust roots, SNI, PROXY configuration, or protocol owner fail closed under strict/qualification. Explicit delete emits `DispositionDeleted` and removes the route/cert even if a predecessor exists.

- [ ] **Step 5: Wire reversible activation boundaries**

`Supervisor.Prepare` launches the candidate worker and stores it by canonical `PublicationSet` identity before any journal token exists. The worker owns its local `PreparedGeneration`; the supervisor accepts READY only after the returned stream revision/digest and publication identity match. If `Journal.Stage` fails, the coordinator calls `Supervisor.DiscardPrepared(context.WithoutCancel(ctx), set)`, which removes the matching pending worker, sends STOP, kills if required, waits for exit, and thereby closes the worker-owned prepared generation; cleanup errors are joined with the Stage error.

Only after Stage returns does `Supervisor.Activate(token, set)` bind that journal token to the matching pending worker, quiesce the predecessor, activate the candidate behind the exact HTTP/stream revision fence, and retain both processes for rollback. `RollbackActivation` stops the candidate and resumes the predecessor. After durable commit, `FinalizeActivation` performs no IPC and no blocking cleanup: it marks the candidate active and only enqueues predecessor retirement for `Supervisor.Run`. No compiler/runtime journal write, token-before-Stage map, server facade, or `server.GenerationEngine` forwarding path is permitted.

- [ ] **Step 6: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/compiler ./pkg/generation ./pkg/supervisor ./pkg/worker -run "^(TestPrepareKeepsHTTP|TestStreamDependency|TestStreamPublication|TestStreamCandidateStageFailure|TestSupervisor.*Stream)" -count=1'`

Expected: PASS.

```bash
git add pkg/compiler pkg/generation pkg/supervisor/supervisor.go pkg/supervisor/publication_test.go pkg/worker/bootstrap.go pkg/worker/bootstrap_test.go
git commit -m "feat(stream): publish immutable stream generations"
```

### Task 4: Execute a General Stream Plugin Phase, Priority, and Scope Chain

**Files:**
- Modify: `pkg/plugin/descriptor.go`, `descriptor_test.go`
- Create: `pkg/stream/chain.go`, `chain_test.go`
- Modify: `pkg/stream/router.go`, `router_test.go`
- Modify: `pkg/compiler/stream.go`, `stream_test.go`
- Modify: `pkg/capability/manifest.yaml`, `cmd/capability-gen/main_test.go`

**Interfaces:** Consumes plan 04 `plugin.Descriptor`, `plugin.Scope`, `plugin.ResourceProvenance`, `plugin.InstanceKey`. Produces `stream.Plugin`, `Binding`, `CompileChain` and the exact phase mapping under Stable Interfaces.

- [ ] **Step 1: Write failing deterministic-chain tests**

```go
func TestCompileChainOrdersPhasePriorityScopeAndAlwaysLogs(t *testing.T) {
	recorder := &phaseRecorder{}
	chain, err := CompileChain([]Binding{
		streamBinding(t, recorder, plugin.PhaseStreamLog, 10, "log-b"),
		streamBinding(t, recorder, plugin.PhaseStreamAccess, 20, "access-low"),
		streamBinding(t, recorder, plugin.PhaseStreamAccess, 100, "access-high"),
	}, rawOwnerFixture())
	if err != nil { t.Fatal(err) }
	result := chain.Serve(context.Background(), connectionFixture(t), dialFixture(t))
	if result.ErrCode != "" { t.Fatalf("result = %#v", result) }
	if got := recorder.Names(); !slices.Equal(got, []string{"access-high", "access-low", "owner", "log-b"}) {
		t.Fatalf("order = %v", got)
	}
}
```

Cover reject short-circuit, panic recovery/final log, `_meta.disable`, `_meta.priority`, service→route merge, unsupported consumer/global scope, duplicate content owner, HTTP phase on stream plugin, map-order determinism, and generation task ownership.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/stream ./pkg/compiler -run "^(TestCompileChain|TestStreamDescriptor|TestStreamPluginScope)" -count=1'`

Expected: FAIL because stream phases and chain do not exist.

- [ ] **Step 3: Add manifest-owned stream phases and compile bindings**

Add the four `plugin.PhaseStream*` constants. `DescriptorForFactory` remains the only phase/priority/scope source; extend its strict parser. Merge service then route plugin config using APISIX precedence, resolve `_meta` once, create generation-owned instances/leases, and sort bindings deterministically. Qualification rejects a stream plugin declaring an HTTP-only phase or unsupported scope.

- [ ] **Step 4: Execute phases with one terminal owner**

Preread may populate bounded attributes; access may continue/reject; content invokes exactly one `ProtocolOwner`; log runs in reverse registration order after every terminal outcome, including reject, dial error, cancellation, and panic. No filter may directly accept listeners, dial outside the supplied `Dialer`, or retain plaintext secret values.

- [ ] **Step 5: Run focused/race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/plugin ./pkg/stream ./pkg/compiler -run "^(TestCompileChain|TestStreamDescriptor|TestStreamPluginScope|TestStreamChain)" -count=1'`

Expected: PASS.

```bash
git add pkg/plugin pkg/stream pkg/compiler pkg/capability/manifest.yaml cmd/capability-gen/main_test.go
git commit -m "feat(stream): compile deterministic plugin chains"
```

### Task 5: Make Protocol Owners Explicit and Reject Cross-Domain Mixing

**Files:**
- Create: `pkg/stream/protocol.go`, `protocol_test.go`
- Modify: `pkg/plugin/mqtt_proxy/plugin.go`, `stream.go`, tests
- Modify: `pkg/plugin/kafka_proxy/plugin.go`, `websocket.go`, tests
- Modify: `pkg/plugin/dubbo_proxy/plugin.go`, tests
- Modify: `pkg/plugin/http_dubbo/plugin.go`, tests
- Modify: `pkg/plugin/dubbo/transport.go`, tests
- Modify: `pkg/capability/manifest.yaml`, `cmd/capability-gen/main_test.go`

**Interfaces:** Produces `ProtocolOwner`. MQTT implements it; raw TCP uses the core owner. Kafka PubSub remains `kafka_proxy.ServePubSubWebSocket`; `dubbo-proxy` and `http-dubbo` remain HTTP terminal owners sharing `pkg/plugin/dubbo` transport.

- [ ] **Step 1: Write failing owner-boundary tests**

```go
func TestProtocolOwnerBoundaryRejectsHTTPOwnersInStreamChain(t *testing.T) {
	for _, factory := range []string{"kafka-proxy", "dubbo-proxy", "http-dubbo"} {
		_, err := CompileProtocolOwner(factory, pluginFixture(t, factory))
		if err == nil || !strings.Contains(err.Error(), "HTTP protocol owner") {
			t.Fatalf("factory %s error = %v", factory, err)
		}
	}
}
```

Also prove raw TCP has no plugin terminal, MQTT prereads/replays CONNECT once, Kafka binary PubSub framing stays WebSocket/HTTP, both Dubbo dialects keep separate request/response codecs, and shared Dubbo transport never owns routing/listeners.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/stream ./pkg/plugin/mqtt_proxy ./pkg/plugin/kafka_proxy ./pkg/plugin/dubbo_proxy ./pkg/plugin/http_dubbo ./pkg/plugin/dubbo -run "^(TestProtocolOwner|TestMQTT|TestKafkaPubSub|TestDubboOwner)" -count=1'`

Expected: FAIL until owner interfaces and manifest boundaries exist.

- [ ] **Step 3: Implement raw TCP and MQTT owners**

The raw owner performs bounded bidirectional half-close-aware bridging. `mqtt_proxy.Plugin` implements `Protocol() string { return "mqtt" }` and `Serve` using its existing bounded CONNECT parser/replay, but deletes `ServeListener`; it receives only the generation dialer. Both report bytes, duration, client ID, and stable error codes without payload/credentials.

- [ ] **Step 4: Lock Kafka and Dubbo to HTTP ownership**

Manifest domains/phases identify Kafka PubSub as an HTTP WebSocket terminal, `dubbo-proxy` as HTTP→Hessian Dubbo, and `http-dubbo` as HTTP→FastJSON Dubbo. Add type-assertion tests proving none implements `stream.ProtocolOwner`. Cross-wiring fails compilation/manifest tests; no adapter translates Kafka PubSub or Dubbo into an L4 stream plugin.

- [ ] **Step 5: Run focused tests and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/stream ./pkg/plugin/mqtt_proxy ./pkg/plugin/kafka_proxy ./pkg/plugin/dubbo_proxy ./pkg/plugin/http_dubbo ./pkg/plugin/dubbo -run "^(TestProtocolOwner|TestMQTT|TestKafkaPubSub|TestDubboOwner|TestServeStream)" -count=1'`

Expected: PASS.

```bash
git add pkg/stream/protocol.go pkg/stream/protocol_test.go pkg/plugin/mqtt_proxy pkg/plugin/kafka_proxy pkg/plugin/dubbo_proxy pkg/plugin/http_dubbo pkg/plugin/dubbo pkg/capability/manifest.yaml cmd/capability-gen/main_test.go
git commit -m "refactor(stream): establish protocol owner boundaries"
```

### Task 6: Own Stream Connection Lifetime, Listener Handoff, and Drain in the Worker

**Files:**
- Create: `pkg/stream/connection_registry.go`, `connection_registry_test.go`
- Create: `pkg/worker/stream.go`, `stream_test.go`
- Modify: `pkg/worker/bootstrap.go`, `bootstrap_test.go`
- Modify: `pkg/worker/listeners.go`, `listeners_test.go`
- Modify: `pkg/supervisor/listeners.go`, `listeners_test.go`
- Modify: `pkg/supervisor/activation.go`, `activation_test.go`
- Modify: `pkg/lifecycle/types.go`, `types_test.go`
- Modify: `pkg/stream/runtime.go`, `runtime_test.go`

**Interfaces:** Consumes plan 05 `ListenerDescriptor`, worker/supervisor `ListenerSet`, `RevisionFence`, `Status`, `PreparedGeneration`, HTTP registration/drain behavior, and task drain. Produces additive `worker.ListenerSet.Claim([]string) (map[string]net.Listener, error)`, `ConnectionRegistry`, `DrainSnapshot`, and the replacement `stream.NewRuntime` lifecycle.

- [ ] **Step 1: Write failing fence/handoff/drain tests**

```go
func TestStreamRuntimeDoesNotAcceptBeforeMatchingActivationFence(t *testing.T) {
	snapshot := streamSnapshotFixture(t, 52)
	runtime := inheritedStreamRuntimeFixture(t, snapshot)
	if err := runtime.Activate(lifecycle.RevisionFence{}); err == nil { t.Fatal("activated without matching stream fence") }
	fence := lifecycle.RevisionFence{Stream: 52,
		ArtifactDigests: map[generation.Domain][32]byte{generation.DomainStream: streamDigestFixture(t)}}
	if err := runtime.Activate(fence); err != nil { t.Fatal(err) }
}

func TestListenerSetClaimTransfersAndClosesStreamListenerExactlyOnce(t *testing.T) {
	listener := &countingListener{Listener: listenerFixture(t)}
	set := listenerSetFixture(t, map[string]net.Listener{"stream/tcp/127.0.0.1:9000": listener})
	claimed, err := set.Claim([]string{"stream/tcp/127.0.0.1:9000"})
	if err != nil { t.Fatal(err) }
	if err := set.Close(); err != nil { t.Fatal(err) }
	if listener.CloseCount() != 0 { t.Fatal("ListenerSet closed transferred listener") }
	runtime := streamRuntimeFixture(t, claimed)
	if err := runtime.Close(context.Background()); err != nil { t.Fatal(err) }
	if err := runtime.Close(context.Background()); err != nil { t.Fatal(err) }
	if listener.CloseCount() != 1 { t.Fatalf("close count = %d", listener.CloseCount()) }
}

type countingListener struct {
	net.Listener
	closes atomic.Int32
}
func (l *countingListener) Close() error {
	l.closes.Add(1)
	return l.Listener.Close()
}
func (l *countingListener) CloseCount() int { return int(l.closes.Load()) }
```

Add tests for supervisor bind-once/duplicate-for-child, worker not calling `net.Listen`, candidate catch-up before READY, quiesce preserving established streams, replacement accepting only after activation, old generation drain, half-close, idle connection, stuck connection residual, repeated drain/close, worker crash, and FD close exactly once.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/supervisor ./pkg/worker ./pkg/stream ./pkg/lifecycle -run "^(TestStreamRuntime|TestStreamListenerHandoff|TestStreamDrain)" -count=1'`

Expected: FAIL because worker-owned runtime/registry is absent.

- [ ] **Step 3: Extend descriptors without changing IPC framing**

Use existing `ListenerDescriptor{Name, Network, Address, InheritedFD}` with stream names formed as `"stream/tcp/" + address`; do not add a second FD message. Supervisor `BindListenerSet` binds configured stream sockets once and `DuplicateForChild` includes them in the existing `LoadRequest.Listeners`. Worker validates descriptor name/address against `StreamSnapshot.ListenerPolicies`, calls `Claim(sortedIDs)` without reading `ListenerSet` private maps, and passes the returned map to `stream.NewRuntime`. Unknown/duplicate/already-HTTP/previously-claimed IDs fail preparation before READY. Neither worker nor stream code calls `net.Listen`; after successful transfer only `stream.Runtime.Drain/Close` owns close, while `ListenerSet.Shutdown/Close` owns the remaining HTTP/unclaimed listeners.

- [ ] **Step 4: Implement ordered connection drain**

Accept registers each connection before dispatch and rejects after quiesce. Drain order is stop accepts → wait natural completion until deadline → cancel/close residual client and upstream connections → wait connection task groups → report sorted `{route,protocol}` residuals → close generation tasks/resources. On supervisor handoff the old generation retains its accepted sockets; new accepts go only to the active generation.

- [ ] **Step 5: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/supervisor ./pkg/worker ./pkg/stream ./pkg/lifecycle -run "^(TestStreamRuntime|TestStreamListenerHandoff|TestStreamDrain|TestConnectionRegistry)" -count=1'`

Expected: PASS.

```bash
git add pkg/supervisor pkg/worker pkg/stream pkg/lifecycle
git commit -m "feat(stream): own inherited listeners and connection drain"
```

### Task 7: Enforce Per-Listener, Per-Route, Client, and Server TLS Trust

**Files:**
- Create: `pkg/stream/tls.go`, `tls_test.go`
- Modify: `pkg/stream/router.go`, `router_test.go`
- Modify: `pkg/resource/route.go`, `ssl.go`, tests
- Modify: `pkg/compiler/stream.go`, `stream_test.go`

**Interfaces:** Produces `CompileUpstreamTLS` and consumes plan 04 `secret.Materializer`. Downstream listener TLS remains `ListenerPolicy.TLS`; upstream TLS is per compiled route/target.

- [ ] **Step 1: Write failing TLS trust/SNI tests**

```go
func TestCompileUpstreamTLSSeparatesDialHostTrustSNIAndClientIdentity(t *testing.T) {
	route := streamRouteTLSFixture(t, "10.0.0.8", 9443, "broker.example.test", "client-cert-1")
	plan, err := CompileUpstreamTLS(route, effectiveTLSFixture(t), sslFixtures(t), fixtureMaterializer(t))
	if err != nil { t.Fatal(err) }
	if !plan.Enabled || !plan.Verify || plan.ServerName != "broker.example.test" || plan.RootCAs == nil || plan.ClientCertificate == nil {
		t.Fatalf("TLS plan = %#v", plan)
	}
}
```

Cover exact/wildcard downstream SNI, fallback SNI, unknown SNI, missing client certificate, untrusted client, client cert depth, upstream IP dial with DNS SNI, hostname mismatch, custom/system roots, verify false compat behavior, strict verify requirement, encrypted key material, and secret-safe errors.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/stream ./pkg/resource ./pkg/compiler -run "^(TestCompileUpstreamTLS|TestStreamDownstreamTLS|TestStreamUpstreamTLS)" -count=1'`

Expected: FAIL because upstream TLS compilation is absent.

- [ ] **Step 3: Compile immutable trust and identity**

Resolve route/service/upstream precedence first. Dial address comes from selected node; `ServerName` comes from the compiled logical upstream authority and never from an untrusted client field. Load configured trust roots, fail on an empty/invalid pool when verify is true, resolve `client_cert_id` from dependency closure, materialize key/cert under route/upstream provenance, and clone `tls.Config` per dial.

- [ ] **Step 4: Enforce security profile behavior**

Compatibility preserves APISIX's explicit `verify` setting. Strict and `stream-data-plane-v1` require verification, TLS 1.2+, valid SNI, and complete client identity where configured. No error falls back from TLS to plaintext, mTLS to server-only TLS, verified to insecure, or PROXY-aware to direct peer identity.

- [ ] **Step 5: Run focused tests and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/stream ./pkg/resource ./pkg/compiler -run "^(TestCompileUpstreamTLS|TestStreamDownstreamTLS|TestStreamUpstreamTLS|TestStreamTLSProfile)" -count=1'`

Expected: PASS.

```bash
git add pkg/stream/tls.go pkg/stream/tls_test.go pkg/stream/router.go pkg/stream/router_test.go pkg/resource pkg/compiler/stream.go pkg/compiler/stream_test.go
git commit -m "feat(stream): enforce route-owned tls trust"
```

### Task 8: Bound Stream Connections, Buffers, Tasks, and Telemetry Cardinality

**Files:**
- Modify: `pkg/runtime/budget.go`, `budget_test.go`
- Modify: `pkg/runtime/admission.go`, `admission_test.go`
- Modify: `pkg/stream/runtime.go`, `router.go`, `bridge/bridge.go` and tests
- Modify: `pkg/telemetry/reporter.go`, `aggregator.go`, `prometheus.go` and tests
- Modify: `pkg/worker/stream.go`, `stream_test.go`
- Modify: `scripts/runtime_capacity.sh`, `runtime_capacity_test.sh`

**Interfaces:** Consumes plan 07 `BudgetManager`, `AdmissionController`, `Reporter`, `Aggregator`, `TaskRegistry`. Adds no new global metric API or lifecycle state.

- [ ] **Step 1: Write failing budget/cardinality tests**

```go
func TestStreamAdmissionAndTelemetryStayBoundedByExistingPolicies(t *testing.T) {
	sender := &collectingBatchSender{}
	tasks := runtime.NewTaskRegistry(context.Background(), func(runtime.TaskFailure) {})
	reporter, err := telemetry.NewReporter(config.TelemetryPolicy{
		WorkerQueueBytes: 1 << 20, MaxFrameBytes: 64 << 10,
		FlushInterval: time.Second, MaxTotalSeries: 32, GenerationSeriesTTL: time.Minute,
	}, 7, sender, tasks)
	if err != nil { t.Fatal(err) }
	for i := 0; i < 1000; i++ {
		reporter.AddCounter("apisix_stream_connections_total", []telemetry.Label{{Name: "route", Value: fmt.Sprintf("r-%d", i)}}, 1)
	}
	if err := reporter.Flush(context.Background()); err != nil { t.Fatal(err) }
	if len(sender.batch.Points) > 32 { t.Fatalf("series = %d", len(sender.batch.Points)) }
	if sender.batch.Dropped == 0 { t.Fatal("missing overflow evidence") }
}

type collectingBatchSender struct { batch telemetry.Batch }
func (s *collectingBatchSender) SendTelemetry(_ context.Context, batch telemetry.Batch) error {
	s.batch = batch
	return nil
}
```

Cover connection admission, preread bytes, PROXY/TLS buffers, bridge copy buffers, MQTT CONNECT size, active tasks, soft shed, hard-pressure worker exit, route/protocol/listener labels, retired generation gauges, overflow points, and no client ID/SNI/upstream address as unbounded labels.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/runtime ./pkg/stream ./pkg/telemetry ./pkg/worker -run "^(TestStreamAdmission|TestStreamBudget|TestStreamTelemetry)" -count=1'`

Expected: FAIL until stream paths use shared budget/telemetry owners.

- [ ] **Step 3: Reserve and release exact stream resources**

Use `AdmissionController.WrapListener` for accepted connections, `BudgetManager.Reserve` for bounded preread/bridge buffers, and generation `TaskRegistry` for accept/telemetry tasks. Release on every success/error/cancel/panic path exactly once. Soft pressure rejects new ordinary stream connections; sustained hard pressure uses plan 07's existing terminal worker exit.

- [ ] **Step 4: Emit bounded aggregate telemetry**

Emit connection accepted/active/duration/bytes/error, TLS/mTLS/PROXY failures, route misses, plugin rejects, upstream attempts, drains/residuals, and stream revision. Fixed labels are listener ID, route ID, protocol, error code, and generation only where plan 07 permits TTL. Never label client ID, source IP, SNI, upstream address, certificate identity, or error text.

- [ ] **Step 5: Run race/capacity contract tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/runtime ./pkg/stream ./pkg/telemetry ./pkg/worker -run "^(TestStreamAdmission|TestStreamBudget|TestStreamTelemetry|TestStreamPressure)" -count=1'`

Run: `bash scripts/runtime_capacity_test.sh`

Expected: PASS.

```bash
git add pkg/runtime pkg/stream pkg/telemetry pkg/worker scripts/runtime_capacity.sh scripts/runtime_capacity_test.sh
git commit -m "feat(stream): bound runtime and telemetry resources"
```

### Task 9: Qualify Stream Parity, Dependencies, Capacity, Outage, and Recovery on the Exact Digest

**Files:**
- Create: `t/plugin/stream-convergence.yaml`
- Modify: `t/plugin/case.go`, `runner_test.go`, `corpus_scope.yaml`, `corpus_test.go`
- Create: `scripts/stream_qualification.sh`, `stream_qualification_test.sh`
- Modify: `pkg/capability/manifest.yaml`
- Modify: `pkg/qualification/evaluate.go`, `evaluate_test.go`
- Modify: `qualification/policy.json`
- Modify: `.github/workflows/qualification.yml`
- Modify: `.github/workflows/release-candidate.yml`, `.github/workflows/release.yml`
- Modify: `scripts/release_metadata.sh`, `scripts/release_metadata_test.sh`
- Modify: `cmd/capability-gen/main.go`, `main_test.go`

**Interfaces:** Consumes plan 08 `OracleLock`, `ArtifactIdentity`, `BuildMetadata`, `InputIdentity`, `EvidenceRecord`, `Result`, `BundleManifest`, `WriteBundle`, `VerifyBundle`, and build-once promotion contract. Produces a `stream-data-plane-v1` bundle and a fresh HTTP bundle for the same post-stream OCI digest, records both bundle hashes in release metadata, and promotes/signs that digest without rebuilding; the previously released HTTP bundle remains byte-for-byte immutable.

- [ ] **Step 1: Write failing stream qualification-policy tests**

```go
func TestEvaluateStreamRequiresIndependentRevisionAndRecoveryEvidence(t *testing.T) {
	m := streamQualificationManifestFixture(t)
	r := passingStreamResultFixture(t, m)
	r.Evidence = slices.DeleteFunc(r.Evidence, func(e EvidenceRecord) bool { return e.ID == "stream/recovery" })
	err := Evaluate(m, r)
	if err == nil || !strings.Contains(err.Error(), "stream/recovery") { t.Fatalf("error = %v", err) }
}
```

Add policy tests requiring listener matrix, TLS/mTLS/PROXY, plugin chain, owner boundary, independent revision/readiness/rollback, last-good/delete, security, real MQTT/Kafka/Dubbo, capacity, outage/recovery, platform, provenance, and unchanged HTTP qualification on the candidate digest.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/qualification ./pkg/capability ./t/plugin -run "^(TestEvaluateStream|TestStreamCorpus|TestStreamDifferential)" -count=1'`

Expected: FAIL because the stream profile/evidence is absent.

- [ ] **Step 3: Add pinned differential cases and honest corpus dispositions**

Map every applicable APISIX stream block at the pinned commit to `converted`, `not_applicable`, or `deferred` with owner/reason. Differential cases run the same listener/route/plugin/TLS/PROXY config, bytes, certificates, source identity, and dependency scripts against the oracle and candidate using plan 08 normalization. Do not normalize connection outcome, selected route/upstream, bytes, TLS alert, client identity decision, PROXY address, attempt count, or drain/recovery state.

- [ ] **Step 4: Add pinned real-dependency and failure gates**

Scheduled/release mode uses digest-pinned MQTT broker, Kafka, etcd, and Dubbo fixtures. Exercise TLS/mTLS, broker/server outage, reconnect, half-close, backpressure, slow client, cert rotation, invalid config last-good, explicit delete, compaction/restart recovery, worker crash, generation rollback, capacity pressure, and drain. A missing dependency or flaky/blocked/skipped record fails and remains recorded; no hidden retry.

- [ ] **Step 5: Preserve HTTP evidence and bind both results to one new candidate**

Never edit the Task 8 HTTP bundle. The release-candidate workflow records its hash before doing any post-stream work, builds the post-stream OCI index once, runs complete HTTP qualification into a new bundle for that digest, runs stream qualification into a separate bundle for the same digest, and calls `VerifyBundle` on both. `scripts/release_metadata.sh` accepts the exact post-stream OCI digest, new HTTP bundle hash, stream bundle hash, and unchanged previous HTTP bundle hash; it rejects a missing bundle, unequal candidate digest, mutated previous bundle, or a bundle hash different from the verified manifest.

- [ ] **Step 6: Prove same-run, no-rebuild promotion before signing**

Add this release contract to `scripts/release_metadata_test.sh` and the workflow assertion helpers:

```bash
candidate_job=$(job_block .github/workflows/release-candidate.yml qualify_post_stream)
promotion_job=$(job_block .github/workflows/release.yml promote)
for required in 'http_bundle_sha256' 'stream_bundle_sha256' 'previous_http_bundle_sha256'; do
  grep -Fq "$required" <<<"$candidate_job"
  grep -Fq "$required" <<<"$promotion_job"
done
verify_http=$(grep -n 'VerifyBundle.*http' <<<"$promotion_job" | cut -d: -f1)
verify_stream=$(grep -n 'VerifyBundle.*stream' <<<"$promotion_job" | cut -d: -f1)
sign=$(grep -n 'cosign sign --yes' <<<"$promotion_job" | cut -d: -f1)
test -n "$verify_http" && test -n "$verify_stream" && test -n "$sign"
test "$verify_http" -lt "$sign" && test "$verify_stream" -lt "$sign"
! grep -Eq 'docker build|docker buildx build|build-push-action|go build|goreleaser (build|release)' <<<"$promotion_job"
```

The release-candidate workflow uploads both verified bundles, their manifests, `build-metadata.json`, and the immutable previous-bundle hash in one artifact set. The release workflow downloads that set, recomputes all three hashes, checks both new results name the exact candidate digest and expected profiles, checks the old HTTP bundle hash without opening it for mutation, then signs and registry-copies the OCI digest. It may publish the already native-smoked macOS archives from plan 08, but it cannot rebuild the OCI index, either bundle, or any archive. A missing, stale, mixed-digest, or changed bundle stops before Cosign, provenance, tag copy, or release publication.

- [ ] **Step 7: Run hermetic contract tests and commit**

Run: `bash scripts/stream_qualification_test.sh`

Run: `bash scripts/release_metadata_test.sh`

Run: `bash -lc 'source .envrc && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./pkg/qualification ./pkg/capability ./t/plugin -run "^(TestEvaluateStream|TestStreamCorpus|TestStreamDifferential|TestHTTPQualificationRemainsImmutable)" -count=1'`

Expected: PASS. Real dependency selectors execute serially in scheduled/release workflow.

```bash
git add t/plugin/stream-convergence.yaml t/plugin/case.go t/plugin/runner_test.go t/plugin/corpus_scope.yaml t/plugin/corpus_test.go scripts/stream_qualification.sh scripts/stream_qualification_test.sh scripts/release_metadata.sh scripts/release_metadata_test.sh pkg/capability/manifest.yaml pkg/qualification qualification/policy.json .github/workflows/qualification.yml .github/workflows/release-candidate.yml .github/workflows/release.yml cmd/capability-gen
git commit -m "test(stream): qualify parity and recovery evidence"
```

### Task 10: Atomically Remove the Mutable MQTT-Only Runtime and Close Documentation

**Files:**
- Modify: `pkg/server/server.go`, `server_test.go`, `stream_test.go`
- Modify: `pkg/stream/router.go`, `router_test.go`, `runtime.go`, `runtime_test.go`
- Modify: `pkg/plugin/mqtt_proxy/stream.go`, `stream_test.go`
- Modify: `docs/design.md`, `docs/plugins.md`, `README.md`
- Create: `docs/architecture/stream-convergence.md`
- Modify: `cmd/capability-gen/main.go`, `main_test.go`
- Verify: every file changed by Tasks 1–9

**Interfaces:** Removes old owners only after Tasks 1–9 are green. Leaves one production path: supervisor-bound inherited listeners → worker `stream.Runtime` → immutable `compiler.StreamSnapshot` → generation-owned chain/owner.

- [ ] **Step 1: Write failing absence tests**

```go
func TestServerHasNoMutableStreamStartupOrReloadOwner(t *testing.T) {
	files := []string{"server.go", "stream_test.go"}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil { t.Fatal(err) }
		for _, symbol := range []string{"startStreamProxy", "reloadStreamRoutes", "streamRuntimeOwner", "CommitStreamRouteLastGood"} {
			if bytes.Contains(data, []byte(symbol)) { t.Fatalf("%s still contains %s", file, symbol) }
		}
	}
}
```

Run: `bash -lc 'source .envrc && go test ./pkg/server ./pkg/stream -run "^(TestServerHasNoMutableStream|TestRouterHasNoReload)" -count=1'`

Expected: FAIL while the old path exists.

- [ ] **Step 2: Delete the old owner atomically**

Remove every symbol listed under File and Responsibility Map, old startup/shutdown/reload event branches, config/store global reads, mutable route slices, MQTT listener ownership, tests that exercise only those APIs, and metrics readiness state deleted by plan 07. Move any still-valid pure assertions to compiler/worker tests. Do not retain deprecated wrappers or test-only proxies.

- [ ] **Step 3: Document exact supported/deferred boundaries**

Document listener matrix, SNI/mTLS/PROXY order, independent stream revision/readiness, publication/rollback/drain sequence, plugin phases/scopes, protocol owner table, TLS trust, budgets/telemetry, evidence commands, and platform matrix. List UDP/QUIC, NGINX/OpenResty stream timing/APIs, Lua stream filters/scripts, external runners, shared-dict exactness, and native kernel features as deferred with manifest owner/reason; do not claim parity for them.

- [ ] **Step 4: Run focused milestone and absence gates**

```bash
bash -lc 'source .envrc && go test -race ./pkg/stream/... ./pkg/compiler ./pkg/generation ./pkg/server ./pkg/supervisor ./pkg/worker ./pkg/runtime ./pkg/telemetry ./pkg/plugin/mqtt_proxy ./pkg/plugin/kafka_proxy ./pkg/plugin/dubbo_proxy ./pkg/plugin/http_dubbo ./pkg/plugin/dubbo -run "^(TestStream|TestMQTT|TestKafka|TestDubbo|TestGeneration)" -count=1'
bash -lc 'source .envrc && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./pkg/capability ./pkg/qualification ./t/plugin -run "^(TestStream|TestHTTPQualificationRemainsImmutable)" -count=1'
bash scripts/stream_qualification_test.sh
bash -lc 'source .envrc && make build'
git diff --check
```

Expected: PASS.

```bash
! rg -n 'startStreamProxy|reloadStreamRoutes|streamRuntimeOwner|streamReloadMu|CommitStreamRouteLastGood|func \(.*\*Router\) Reload|func \(.*\*Runtime\) Reload|ServeListener\(' pkg/server pkg/stream pkg/plugin/mqtt_proxy --glob '*.go'
! rg -n 'config\.GlobalConfig|ReplaceGlobalStoreForTest|GetGlobalStore|net\.Listen\(' pkg/stream pkg/worker --glob '*.go'
! rg -n '(legacy|fallback|mutable|mqttOnly|singleProcess).*(stream|listener|router)' pkg --glob '*.go'
```

Expected: all absence scans return no matches.

- [ ] **Step 5: Commit the cutover and documentation**

```bash
git add pkg/server pkg/stream pkg/plugin/mqtt_proxy docs/architecture/stream-convergence.md docs/design.md docs/plugins.md README.md cmd/capability-gen
git commit -m "refactor(stream): remove mutable mqtt-only runtime"
```

## Plan Self-Review

- **Spec coverage:** Tasks 1–2 cover TCP/TLS/mTLS/PROXY listeners; Task 3 covers immutable compilation, closure, independent publication/readiness/rollback, last-good and delete; Task 4 covers general plugin phases/priority/scope; Task 5 locks raw/MQTT/Kafka/Dubbo ownership; Task 6 covers inherited listeners, handoff, connection lifetime and drain; Task 7 covers client/server TLS trust and SNI; Task 8 covers budgets, tasks, telemetry and cardinality; Task 9 covers differential/real-dependency/capacity/outage/recovery evidence and HTTP bundle immutability; Task 10 removes old paths and records deferred gaps.
- **Placeholder scan:** All implementation actions name exact files, symbols, validation rules, commands, and expected outcomes. Digest values come from plan 08's verified oracle/artifact interfaces; no fake production digest or fill-in token is specified.
- **Type consistency:** `generation.DomainStream`, journal/publication signatures, `PreparedGeneration`, `StreamSnapshot.Revision/Router`, lifecycle fence/listener/status, runtime tasks/budgets, telemetry reporter/aggregator, and qualification result/bundle signatures are copied unchanged. Additive stream types are defined once under Stable Interfaces and used verbatim.
- **Dependency consistency:** Task 8 HTTP qualification is a hard predecessor. Tasks 1–2 precede compiler work; Task 3 precedes chains/owners/runtime; Tasks 4–7 precede safety/evidence; Task 8 precedes qualification; Task 10 deletes old paths only after replacement and evidence pass.
- **Command/path consistency:** All Go/Make commands source `.envrc`; real-process cases remain serial; every created path is declared before use; package commands are impact-scoped. No routine repository-wide Go test is introduced.
- **Code-fence consistency:** Every fenced block closes, every code step contains executable code or an exact command, and shell absence gates use nonzero-match semantics intentionally.

## Execution Handoff

Execute Tasks 1–10 in order after verifying the Task 8 HTTP bundle. Stop on the first unexpected RED result, missing dependency closure, security fallback, stream/HTTP revision coupling, unavailable real dependency, flaky evidence, or candidate-digest mismatch. Preserve both old HTTP and new stream evidence bundles; never repair qualification by rewriting evidence or re-enabling the deleted mutable runtime.
