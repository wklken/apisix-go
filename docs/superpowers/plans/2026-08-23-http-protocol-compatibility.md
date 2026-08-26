# APISIX 3.17 HTTP Protocol Compatibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close and qualify the Go-applicable APISIX 3.17 HTTP observable contract across listener, router, plugin, proxy, body-streaming, and upstream behavior without retaining a legacy HTTP path.

**Architecture:** Listener-local policy compiles immutable downstream protocol and trust settings into each inherited worker listener. The immutable route compiler produces one routing/plugin/protocol/body plan per route, while owned upstream clusters implement DNS, retry, health, authority, and transport behavior. Differential cases pin every promoted behavior to APISIX 3.17 source and keep unsupported native/runtime features explicit.

**Tech Stack:** Go 1.26.6, `net/http`, `net/http/httputil`, `crypto/tls`, `golang.org/x/net/http2`, existing immutable `compiler.PreparedGeneration`, capability manifest, worker listener set, and `t/plugin` harness.

**Spec:** `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`

## Global Constraints

- Compatibility target is Apache APISIX 3.17.0 at commit `9ef2ecab67f652d38365049613610ef649bb4ad0`.
- Preserve the APISIX namespace; version Go-native extensions separately.
- Source `.envrc` before every Go or Make command.
- Use impact-scoped tests; do not run `go test ./...`, `go test ./pkg/...`, or `make test`.
- Run focused race tests for listener, cancellation, pooling, health, and streaming ownership changes; run `source .envrc && make build` after the atomic cutover.
- Consume plans 01–05: `capability.Manifest`, `config.EffectiveConfig`, immutable `compiler.PreparedGeneration`, `runtime.RuntimeDependencies`, and the worker-owned inherited listener set must already exist.
- Compatibility mode preserves pinned APISIX defaults and observable bugs. Strict mode diverges only through an active manifest divergence with an approved ADR and owner reference.
- Stream request and response bodies by default; buffering requires a compiled body requirement and bounded memory/temp-spool policy.
- Never combine listener flags through a process-global OR. One listener's HTTP/2, h2c, TLS, PROXY protocol, timeout, and trust settings cannot affect another listener.
- HTTP/3/QUIC, Lua `filter_func`/script execution, external plugin runner compatibility, and `inspect` remain explicit gaps and cannot be marked `Full`.
- Decision 196C applies to every task: equivalence tests land with the replacement, and the replaced parsed-but-ignored or legacy path is deleted in the same commit with no adapter.
- Keep the four existing untracked files under `docs/reviews/` outside implementation commits.

---

## File and Responsibility Map

**Create:**

- `pkg/server/listener_policy.go` — immutable per-listener protocol, timeout, TLS, PROXY, and trust policy compilation.
- `pkg/server/listener_runtime.go` — build one `http.Server` and wrapped `net.Listener` from one listener policy.
- `pkg/server/proxy_protocol.go` — bounded PROXY v1/v2 parsing and connection metadata.
- `pkg/server/real_ip.go` — trusted-hop client address resolution.
- `pkg/route/matcher.go` — APISIX radixtree host/URI/method/remote/vars priority matcher.
- `pkg/route/protocol_plan.go` — downstream/upstream HTTP, gRPC, and WebSocket compatibility plan.
- `pkg/proxy/resolver.go` — owned DNS cache with TTL, negative results, and cancellation.
- `pkg/proxy/authority.go` — distinct dial endpoint, HTTP authority, and TLS SNI selection.
- `pkg/proxy/body_plan.go` — streaming/buffering decision and bounded request spool owner.
- `pkg/proxy/body_plan_test.go`, `resolver_test.go`, `authority_test.go`.
- `pkg/server/listener_policy_test.go`, `listener_protocol_test.go`, `proxy_protocol_test.go`, `real_ip_test.go`.
- `pkg/route/matcher_test.go`, `protocol_plan_test.go`, `phase_compat_test.go`.
- `t/plugin/http-protocol.yaml` — pinned core HTTP cases with explicit core-capability exemption.
- `docs/architecture/http-compatibility.md` — downstream/upstream matrix, defaults, divergences, and gaps.

**Modify:**

- `pkg/compiler/types.go`, `http.go` — store listener-independent route protocol/body plans in `HTTPSnapshot`.
- `pkg/route/compiler.go`, `router.go`, `plugin_compile.go`, `upstream_compile.go` — consume the new matcher and plans.
- `pkg/plugin/executor.go`, `pkg/plugin/base/{phase_descriptor,response_phase,streaming_phase,writer}.go` — one audited phase path and exact 1xx/trailer behavior.
- `pkg/apisix/ctx/context.go`, `remote.go`; `pkg/apisix/variable/{nginx,request,apisix}.go` — immutable request facts and APISIX/NGINX variables.
- `pkg/proxy/{proxy,transport,cluster,retry,health,active_health}.go` — protocol-aware transports, authority, retries, health, reuse, and cancellation.
- `pkg/server/server.go`, `tls.go`, `route_handler.go` — consume worker listener policies and remove global listener decisions.
- `pkg/resource/route.go` — presence-aware upstream HTTP compatibility fields required by the pinned schema.
- `pkg/capability/{types,manifest.yaml,load_test.go}`, `docs/plugins.md`, `README.md`, and `t/plugin/README.md` — protocol capabilities, generated status, and evidence.
- `t/plugin/{case.go,corpus_scope.yaml,corpus_test.go,runner_test.go}` — core protocol source mapping and differential case support.
- `docs/design.md`, `docs/plugins.md`, `README.md` — generated evidence/gap projection only.

**Delete in the owning atomic task:**

- `frontendHTTP2Enabled`, `frontendPlainHTTP2Enabled`, `configuredListenAddresses`, and `configuredTLSListenAddresses` after Task 2 installs listener-local policy.
- `routeRegistrar`, `wildcardDispatcher`, `routeDecisionIndex`, and their old matcher helpers from `pkg/route/router.go` after Task 4 installs `RouteIndex`.
- `RequestStageLegacy`, `legacyRemainderBindings`, `Executor.thenLegacy`, `Executor.legacyBindings`, `NewExecutor`, and legacy-only tests after Task 5 installs the manifest-owned phase path.
- `bufferRequestBodyIfNeeded` and request buffering flags that bypass `BodyPlan` after Task 8.
- stale proxy streaming comments and every compatibility field that is parsed but neither compiled nor explicitly rejected after Task 9.

## Shared Interfaces

```go
// package server
type DownstreamProtocol string
const (
	DownstreamHTTP1 DownstreamProtocol = "http/1.1"
	DownstreamHTTP2 DownstreamProtocol = "h2"
	DownstreamH2C   DownstreamProtocol = "h2c"
)

type ListenerPolicy struct {
	ID string
	Address string
	TLS bool
	ProxyProtocol bool
	Protocols []DownstreamProtocol
	ReadHeaderTimeout time.Duration
	ReadBodyTimeout time.Duration
	IdleTimeout time.Duration
	RealIP RealIPPolicy
}

func CompileListenerPolicies(*config.EffectiveConfig) ([]ListenerPolicy, error)
func NewListenerRuntime(ListenerPolicy, *compiler.HTTPSnapshot) (*ListenerRuntime, error)
func (r *ListenerRuntime) Serve(net.Listener) error
func (r *ListenerRuntime) Shutdown(context.Context) error

type ListenerRuntime struct {
	policy    ListenerPolicy
	server    *http.Server
	tlsConfig *tls.Config
}

type RealIPPolicy struct {
	Header string
	Recursive bool
	Trusted []netip.Prefix
}
func ResolveClientAddress(RealIPPolicy, netip.Addr, http.Header) netip.Addr

// package route
type RoutePredicate func(*http.Request) bool
type CompiledRoute struct {
	ID string
	URIs []string
	Methods []string
	Hosts []string
	Priority int
	Predicate RoutePredicate
	Handler http.Handler
}
type RouteMatch struct { RouteID string; Captures map[string]string }
type RouteIndex struct {
	roots map[string]*radixNode
	routes map[string]CompiledRoute
}
type radixNode struct {
	static map[string]*radixNode
	parameter *radixNode
	wildcard []CompiledRoute
	terminal []CompiledRoute
}
func CompileRouteIndex([]CompiledRoute) (*RouteIndex, error)
func CompileRoutes(CompileInput) ([]CompiledRoute, error)
func (i *RouteIndex) Match(*http.Request) (RouteMatch, bool)

type ProtocolPlan struct {
	UpstreamScheme string
	ForceHTTP2 bool
	AllowWebSocket bool
	ForwardInformational bool
	ForwardTrailers bool
}
func CompileProtocolPlan(resource.Route, resource.Service, resource.Upstream) (ProtocolPlan, error)

// package proxy
type BodyMode uint8
const (
	BodyStream BodyMode = iota
	BodyMemory
	BodySpool
)
type BodyDirection string
const (
	BodyRequest  BodyDirection = "request"
	BodyResponse BodyDirection = "response"
)
type BodyRequirement struct { RequestReplay bool; RequestBytes bool; ResponseBytes bool }
type BodyPlan struct {
	Request BodyMode
	Response BodyMode
	MemoryLimit int64
	SpoolLimit int64
	TempDir string
}
var ErrBodyBudgetExceeded = errors.New("body budget exceeded")
type BodyBudget interface {
	Reserve(context.Context, BodyDirection, BodyMode, int64) (release func(), err error)
}
type bodyBudgetKey struct {
	direction BodyDirection
	mode      BodyMode
}
type planBodyBudget struct {
	plan BodyPlan
	mu   sync.Mutex
	used map[bodyBudgetKey]int64
}
type planBodyReservation struct {
	budget *planBodyBudget
	key    bodyBudgetKey
	bytes  int64
	once   sync.Once
}
func NewBodyBudget(BodyPlan) BodyBudget
func CompileBodyPlan(config.ProfileSelection, config.RuntimePaths, resource.Upstream, []BodyRequirement) (BodyPlan, error)
type ReplayableBody struct {
	memory    []byte
	spoolPath string
	reader    io.ReadCloser
	budget    BodyBudget
	releases  []func()
	closeOnce sync.Once
	closeErr  error
}
func MaterializeRequestBody(context.Context, BodyPlan, BodyBudget, io.ReadCloser) (*ReplayableBody, error)
func (b *ReplayableBody) Read([]byte) (int, error)
func (b *ReplayableBody) Open() (io.ReadCloser, error)
func (b *ReplayableBody) InMemory() bool
func (b *ReplayableBody) Close() error

type Authority struct { DialAddress, Host, ServerName string }
func ResolveAuthority(resource.Upstream, string, string) (Authority, error)

type Resolver interface { LookupIP(context.Context, string) ([]netip.Addr, error); Close() error }
type ResolverConfig struct {
	Servers []netip.AddrPort
	PositiveTTL time.Duration
	NegativeTTL time.Duration
	Timeout time.Duration
	UseSearch bool
}
func NewResolver(ResolverConfig) (Resolver, error)

// package config
type HTTPFieldOwner struct { Path, Owner string }
func HTTPFieldOwners() []HTTPFieldOwner
func ValidateHTTPCompatibility(*EffectiveConfig) error
```

`NewBodyBudget` enforces the `BodyPlan` per `(BodyDirection, BodyMode)` with checked addition. `BodyStream` returns a no-op release without charging retained bytes; `BodyMemory` uses `MemoryLimit`; `BodySpool` uses `SpoolLimit`. Every returned release closure is idempotent. Plan 07 Task 3 consumes this exact interface and implements `NewRuntimeBodyBudget` by composing it with the worker `runtime.BudgetManager`; plan 06's implementation files do not import `pkg/runtime`.

`compiler.HTTPSnapshot` retains the existing `Revision() uint64` and `Handler() http.Handler` API from plan 04. These additions are private compiled inputs beneath that interface; plan 06 must not change the journal/publication boundary.

### Task 1: Establish the Core HTTP Capability and Oracle Ledger

**Files:**
- Modify: `pkg/capability/types.go`, `manifest.yaml`, `load_test.go`
- Create: `docs/architecture/http-compatibility.md`
- Modify: `t/plugin/case.go`, `corpus_test.go`, `corpus_scope.yaml`

**Interfaces:**
- Consumes: `capability.Evidence`, `Divergence`, and pinned target from plan 01.
- Produces: `ProtocolCapability`, `Manifest.Protocols`, `Manifest.Protocol(string)`, and source-accounted HTTP cases used by Tasks 2–10.

- [ ] **Step 1: Write the failing manifest and source-accounting tests**

```go
func TestHTTPProtocolCapabilitiesPinAPISIX317(t *testing.T) {
	m := mustLoadManifest(t)
	for _, id := range []string{"downstream-http", "routing", "plugin-phases", "upstream-http", "body-streaming"} {
		claim, ok := m.Protocol(id)
		if !ok { t.Fatalf("protocol capability %q missing", id) }
		if claim.TargetCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
			t.Fatalf("%s target = %q", id, claim.TargetCommit)
		}
	}
}
```

- [ ] **Step 2: Run the focused test and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/capability ./t/plugin -run "^(TestHTTPProtocolCapabilities|TestHTTPCorpusSourceAccounting)$" -count=1'`

Expected: FAIL because protocol capabilities and the HTTP ledger do not exist.

- [ ] **Step 3: Add exact protocol evidence types and ledger rows**

```go
type ProtocolCapability struct {
	ID string `yaml:"id"`
	TargetCommit string `yaml:"target_commit"`
	Behavior BehaviorStatus `yaml:"behavior"`
	KnownGaps []string `yaml:"known_gaps"`
	Evidence Evidence `yaml:"evidence"`
	DivergenceIDs []string `yaml:"divergence_ids"`
	SupportedPlatforms []string `yaml:"supported_platforms"`
}
func (m *Manifest) Protocol(id string) (ProtocolCapability, bool)
```

Map the pinned files `apisix/http/router/radixtree_{uri,host_uri,uri_with_parameter}.lua`, `apisix/{balancer,init,schema_def}.lua`, `apisix/core/{ctx,request,response}.lua`, and applicable `t/router/*`, `t/node/{vars,grpc-proxy*,upstream*,healthcheck*,ssl,http_host,wildcard-host}.t`. Each test block is `converted`, `not_applicable` with a Go boundary, or `deferred` with owner/reason; a filename-only count is invalid.

- [ ] **Step 4: Run ledger tests and commit**

Run: `bash -lc 'source .envrc && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./pkg/capability ./t/plugin -run "^(TestHTTPProtocolCapabilities|TestHTTPCorpusSourceAccounting|TestCorpusEvidenceMatchesCompatibilityTarget)$" -count=1'`

Expected: PASS; HTTP/3, Lua execution, external runners, and `inspect` remain explicit non-Full gaps.

```bash
git add pkg/capability t/plugin docs/architecture/http-compatibility.md
git commit -m "docs(compat): establish HTTP protocol evidence ledger"
```

### Task 2: Compile and Serve Listener-Local HTTP Protocol Policies

**Files:**
- Create: `pkg/server/listener_policy.go`, `listener_runtime.go`, `listener_policy_test.go`, `listener_protocol_test.go`
- Modify: `pkg/server/server.go`, `tls.go`
- Modify: `pkg/worker/bootstrap.go`, `bootstrap_test.go`

**Interfaces:**
- Consumes: `config.EffectiveConfig`, plan 04 `(*compiler.HTTPSnapshot).Handler()`/`TLSConfig()`, and plan 05 `worker.HTTPRuntime`, `ListenerSet.RegisterHTTP(string, HTTPRuntime)`, `ListenerSet.Serve(context.Context)`, and `ListenerSet.Shutdown(context.Context)`.
- Produces: `CompileListenerPolicies`, `NewListenerRuntime`, `ListenerRuntime.Serve(net.Listener) error`, `ListenerRuntime.Shutdown(context.Context) error`, and one `http.Protocols` value per listener; `*ListenerRuntime` satisfies `worker.HTTPRuntime`.

- [ ] **Step 1: Write a failing mixed-listener matrix test**

```go
func TestCompileListenerPoliciesDoesNotORHTTP2AcrossListeners(t *testing.T) {
	effective := effectiveConfigWithListeners(t,
		httpListener("plain-h1", 9080, false),
		httpListener("plain-h2c", 9081, true),
		tlsListener("tls-h1", 9443, false))
	policies, err := CompileListenerPolicies(effective); if err != nil { t.Fatal(err) }
	assertProtocols(t, policies, "plain-h1", DownstreamHTTP1)
	assertProtocols(t, policies, "plain-h2c", DownstreamHTTP1, DownstreamH2C)
	assertProtocols(t, policies, "tls-h1", DownstreamHTTP1)
}
```

Add network tests using `http2.Transport{AllowHTTP:true}` for h2c, TLS ALPN for h2, gRPC content-type over h2, HTTP/1 keep-alive reuse, client cancellation, request/response trailers, `103 Early Hints`, WebSocket upgrade, and listener-specific header/body/idle timeouts.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/server -run "^(TestCompileListenerPolicies|TestListenerProtocol)" -count=1'`

Expected: FAIL because listener decisions still use global OR helpers.

- [ ] **Step 3: Implement one runtime per policy and delete global helpers**

```go
func NewListenerRuntime(policy ListenerPolicy, snapshot *compiler.HTTPSnapshot) (*ListenerRuntime, error) {
	if snapshot == nil { return nil, errors.New("HTTP listener runtime requires a snapshot") }
	protocols := new(http.Protocols)
	protocols.SetHTTP1(slices.Contains(policy.Protocols, DownstreamHTTP1))
	protocols.SetHTTP2(slices.Contains(policy.Protocols, DownstreamHTTP2))
	protocols.SetUnencryptedHTTP2(slices.Contains(policy.Protocols, DownstreamH2C))
	server := &http.Server{Handler: snapshot.Handler(), Protocols: protocols,
		ReadHeaderTimeout: policy.ReadHeaderTimeout, ReadTimeout: policy.ReadBodyTimeout,
		IdleTimeout: policy.IdleTimeout}
	var tlsConfig *tls.Config
	if policy.TLS {
		tlsConfig = snapshot.TLSConfig()
		if tlsConfig == nil { return nil, fmt.Errorf("HTTP listener %q requires TLS material", policy.ID) }
	}
	return &ListenerRuntime{policy: policy, server: server, tlsConfig: tlsConfig}, nil
}

func (r *ListenerRuntime) Serve(listener net.Listener) error {
	if r.tlsConfig != nil { listener = tls.NewListener(listener, r.tlsConfig.Clone()) }
	err := r.server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) { return nil }
	return err
}

func (r *ListenerRuntime) Shutdown(ctx context.Context) error {
	return r.server.Shutdown(ctx)
}
```

Remove all four global listener helpers named in the deletion map. Worker bootstrap builds one runtime per compiled policy, registers it under exact `ListenerPolicy.ID`, rejects duplicate/missing/unused inherited descriptor IDs, calls the existing `ListenerSet.Serve`, and uses only `ListenerSet.Shutdown` for ordered HTTP drain. Reject `send_timeout != 0` with the existing exact config field because Go cannot emulate NGINX write-idle semantics; do not accept it silently.

- [ ] **Step 4: Run race/building-block tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/server -run "^(TestCompileListenerPolicies|TestListenerProtocol|TestHTTPServerTimeout)" -count=1'`

Expected: PASS and `rg -n 'frontendHTTP2Enabled|frontendPlainHTTP2Enabled|configuredListenAddresses|configuredTLSListenAddresses' pkg/server --glob '*.go'` prints nothing.

```bash
git add pkg/server pkg/worker
git commit -m "refactor(server): make HTTP protocols listener local"
```

### Task 3: Enforce TLS, SNI, mTLS, PROXY Protocol, and Real-IP Trust Per Listener

**Files:**
- Create: `pkg/server/proxy_protocol.go`, `real_ip.go`, tests
- Modify: `pkg/server/listener_policy.go`, `listener_runtime.go`, `tls.go`, `pkg/apisix/ctx/remote.go`

**Interfaces:**
- Consumes: `ListenerPolicy` and the defensive `*tls.Config` clone returned only by plan 04 `(*compiler.HTTPSnapshot).TLSConfig()`.
- Produces: bounded PROXY v1/v2 listener metadata and `ResolveClientAddress`.

- [ ] **Step 1: Write failing trust-boundary tests**

```go
func TestResolveClientAddressWalksTrustedChainRightToLeft(t *testing.T) {
	policy := RealIPPolicy{Header: "X-Forwarded-For", Recursive: true,
		Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	h := http.Header{"X-Forwarded-For": []string{"198.51.100.7, 10.1.2.3"}}
	if got := ResolveClientAddress(policy, netip.MustParseAddr("10.9.8.7"), h); got.String() != "198.51.100.7" {
		t.Fatalf("client = %s", got)
	}
}
```

Also cover untrusted spoofing, empty trust in compat vs strict, malformed PROXY headers, v1/v2 size limits, TLS SNI exact/wildcard/fallback selection, per-SSL mTLS CA/depth, handshake without SNI, and a TLS listener whose neighboring listener has PROXY disabled.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/server ./pkg/apisix/ctx -run "^(TestProxyProtocol|TestResolveClientAddress|TestListenerTLS|TestEffectiveRemoteIP)" -count=1'`

Expected: FAIL because PROXY and real-IP policy are not listener-owned.

- [ ] **Step 3: Implement bounded connection metadata and TLS selection**

`ListenerRuntime.Serve` wraps only policies with `ProxyProtocol=true`; parsing is deadline-bound, accepts LOCAL/PROXY v2 and UNKNOWN/TCP4/TCP6 v1, rejects unsupported families before HTTP parsing, and stores socket/proxy peers in `http.Server.ConnContext`. PROXY parsing wraps the raw inherited listener before `tls.NewListener`. `ResolveClientAddress` uses that peer then the configured header/trust chain. SNI certificate selection, fallback SNI, client CA/depth, and mTLS policy are already compiled into the clone from `HTTPSnapshot.TLSConfig()`; plan 06 neither rebuilds an SSL index nor reads journal/store SSL resources.

- [ ] **Step 4: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/server ./pkg/apisix/ctx ./pkg/apisix/variable -run "^(TestProxyProtocol|TestResolveClientAddress|TestListenerTLS|TestEffectiveRemoteIP|TestRemoteAddr)" -count=1'`

Expected: PASS; no request can enable PROXY parsing or trust its own forwarded chain.

```bash
git add pkg/server pkg/apisix/ctx pkg/apisix/variable
git commit -m "feat(server): enforce listener TLS and client trust"
```

### Task 4: Replace the Router with APISIX Radixtree Matching Semantics

**Files:**
- Create: `pkg/route/matcher.go`, `matcher_test.go`
- Modify: `pkg/route/router.go`, `compiler.go`, `pkg/resource/route.go`

**Interfaces:**
- Consumes: plan 04 `route.CompileInput`, including its exact `Routes []resource.Route` field and immutable service/upstream/binding maps.
- Produces: `CompileRoutes(CompileInput) ([]CompiledRoute, error)`, then `CompileRouteIndex` and `(*RouteIndex).Match`; plan 04 does not produce `[]CompiledRoute`.

- [ ] **Step 1: Write failing priority/conflict tests**

```go
func TestRouteIndexUsesPriorityThenPinnedRadixtreeSpecificity(t *testing.T) {
	index := mustCompileRouteIndex(t,
		compiledRoute("wild", "/foo/*", "*.example.com", 10),
		compiledRoute("exact", "/foo/bar", "api.example.com", 10),
		compiledRoute("high", "/foo/*", "api.example.com", 20))
	match, ok := index.Match(httptest.NewRequest(http.MethodGet, "http://api.example.com/foo/bar", nil))
	if !ok || match.RouteID != "high" { t.Fatalf("match = %+v/%t", match, ok) }
}
```

Table cases cover `uri`/`uris`, exact/prefix/parameter URI, encoded slash policy, trailing slash, host/hosts exact and one-label wildcard, port stripping, methods/405 Allow, remote_addr/addrs, route status, vars expressions, equal-priority deterministic conflict, and duplicate normalized route rejection.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/route -run "^(TestRouteIndex|TestRouteConflict|TestRadixtree)" -count=1'`

Expected: FAIL because the current matcher rejects `vars` and uses the legacy dispatcher.

- [ ] **Step 3: Implement immutable indexes and atomic deletion**

Implement `CompileRoutes` by iterating a defensive copy of `CompileInput.Routes`, resolving each route's service/upstream and already-compiled bindings exclusively from the same `CompileInput`, and producing a fresh `[]CompiledRoute`. Compile three APISIX modes (`radixtree_uri`, `radixtree_host_uri`, `radixtree_uri_with_parameter`) selected from effective config. Parse `vars` through the existing expression evaluator into immutable predicates; continue to reject Lua `filter_func` and script fields as explicit gaps. Delete all old matcher types/helpers named in the deletion map in this commit.

- [ ] **Step 4: Run matcher tests and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/route -run "^(TestRouteIndex|TestRouteConflict|TestRadixtree|TestRouteHost|TestURI|TestVariableRoute)" -count=1'`

Expected: PASS and `rg -n 'routeRegistrar|wildcardDispatcher|routeDecisionIndex' pkg/route --glob '*.go'` prints nothing.

```bash
git add pkg/route pkg/resource
git commit -m "refactor(route): match APISIX radixtree semantics"
```

### Task 5: Make Variables, Consumer Merge, Plugin Phases, and Error Bodies Exact

**Files:**
- Modify: `pkg/plugin/executor.go`, `pkg/plugin/base/{phase_descriptor,response_phase,streaming_phase,writer}.go`
- Modify: `pkg/route/plugin_compile.go`, `phase_compat_test.go`
- Modify: `pkg/apisix/ctx/context.go`, `pkg/apisix/variable/nginx.go`, `pkg/apisix/variable/request.go`, `pkg/apisix/variable/apisix.go`

**Interfaces:**
- Consumes: manifest-resolved `plugin.Binding.Descriptor` and immutable consumer/group bindings from plan 04.
- Produces: one phase pipeline: global rewrite → route/service rewrite → auth → consumer/group merge → access → before-proxy → upstream → header/body filter → log/finalize.

- [ ] **Step 1: Write failing phase and merge tests**

```go
func TestCompiledPipelineConsumerWinnerKeepsPhasePriority(t *testing.T) {
	recorder := newPhaseRecorder()
	h := compilePhaseFixture(t, recorder,
		globalBinding("g", 3000), routeBinding("p", 2000), consumerBinding("p", 2500))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got, want := recorder.Events(), []string{"global:g", "consumer:p", "upstream", "log:p", "log:g"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}
```

Add exact JSON error assertions for 404, 405, auth failure, schema/config failure, 413, 429, 500, 502, 503, 504, and client-cancel 499; variable tables cover `$host`, `$http_host`, `$remote_addr`, `$server_addr`, `$server_port`, `$scheme`, `$request_uri`, `$uri`, `$route_id`, `$consumer_name`, `$balancer_ip`, `$balancer_port`, `$upstream_status`, and retry count.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/route ./pkg/apisix/ctx ./pkg/apisix/variable -run "^(TestCompiledPipeline|TestConsumerMerge|TestHTTPErrorBody|Test.*Var)" -count=1'`

Expected: FAIL while legacy fallback can still execute an unowned phase.

- [ ] **Step 3: Install the descriptor-only pipeline and delete legacy execution**

Every binding must have a manifest descriptor at compile time. Consumer/group bindings replace same-factory route/service bindings while global/system scope remains separate; priority sorts within each phase, not across phases. Delete every legacy executor symbol named in the deletion map and move tests to `compiler.PreparedGeneration` fixtures.

- [ ] **Step 4: Run phase tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/plugin ./pkg/route ./pkg/apisix/ctx ./pkg/apisix/variable -run "^(TestCompiledPipeline|TestConsumerMerge|TestHTTPErrorBody|TestRequestPipeline|TestResponse|Test.*Var)" -count=1'`

Expected: PASS and the legacy symbol scan is empty.

```bash
git add pkg/plugin pkg/route pkg/apisix
git commit -m "refactor(plugin): enforce APISIX HTTP phase semantics"
```

### Task 6: Compile the Downstream-to-Upstream Protocol Matrix

**Files:**
- Create: `pkg/route/protocol_plan.go`, `protocol_plan_test.go`
- Modify: `pkg/route/upstream_compile.go`, `pkg/proxy/proxy.go`, `pkg/server/route_handler.go`

**Interfaces:**
- Consumes: route/service/upstream resources and downstream request protocol.
- Produces: `CompileProtocolPlan` controlling HTTP/1, HTTP/2, h2c, gRPC, WebSocket, 1xx, trailers, cancellation, and reuse.

- [ ] **Step 1: Write the failing protocol matrix test**

```go
func TestCompileProtocolPlanSeparatesDownstreamAndUpstream(t *testing.T) {
	plan, err := CompileProtocolPlan(resource.Route{EnableWebsocket: true}, resource.Service{},
		resource.Upstream{Scheme: "grpcs"})
	if err != nil { t.Fatal(err) }
	if plan.UpstreamScheme != "https" || !plan.ForceHTTP2 || !plan.AllowWebSocket {
		t.Fatalf("plan = %+v", plan)
	}
}
```

Add real server cases for H1→H1, H1→H2, H2→H1, H2→H2, h2c downstream, gRPC unary/stream, WebSocket only when enabled, request/response trailers, 103, cancel propagation, and no reuse after protocol/connection errors.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/route ./pkg/proxy ./pkg/server -run "^(TestCompileProtocolPlan|TestHTTPProtocolMatrix|TestGRPC|TestWebSocket|TestTrailer|TestInformational)" -count=1'`

Expected: FAIL because current upstream HTTP/2 configuration is transport-global rather than route-plan exact.

- [ ] **Step 3: Implement route-owned protocol selection**

`grpc`/`grpcs` always select an HTTP/2-capable transport and preserve TE/trailers; normal `http`/`https` may negotiate HTTP/2 but never infer it from downstream. WebSocket is an H1 upgrade and remains generation-owned until close. A downstream cancellation cancels dialing, request body, response body, and retry selection without emitting a second response after commit.

- [ ] **Step 4: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/route ./pkg/proxy ./pkg/server -run "^(TestCompileProtocolPlan|TestHTTPProtocolMatrix|TestGRPC|TestWebSocket|TestTrailer|TestInformational|TestCancellation|TestConnectionReuse)" -count=1'`

Expected: PASS.

```bash
git add pkg/route pkg/proxy pkg/server
git commit -m "feat(http): compile downstream upstream protocol plans"
```

### Task 7: Own DNS, Retry, Load Balancing, Passive Health, Host, and SNI

**Files:**
- Create: `pkg/proxy/resolver.go`, `authority.go`, tests
- Modify: `pkg/proxy/{transport,cluster,retry,loadbalance,priority,health,active_health}.go`
- Modify: `pkg/route/upstream_compile.go`, `pkg/resource/route.go`

**Interfaces:**
- Consumes: effective DNS settings, route `ProtocolPlan`, upstream nodes/checks/retries/pass_host/upstream_host/TLS verify.
- Produces: generation-owned `Resolver`, `ResolveAuthority`, retry eligibility, and cluster identity including every effective transport value.

- [ ] **Step 1: Write failing authority and retry tests**

```go
func TestResolveAuthoritySeparatesDialHostAuthorityAndSNI(t *testing.T) {
	u := resource.Upstream{PassHost: "rewrite", UpstreamHost: "api.example.test", Scheme: "https"}
	a, err := ResolveAuthority(u, "192.0.2.10:443", "client.example.test"); if err != nil { t.Fatal(err) }
	if a.DialAddress != "192.0.2.10:443" || a.Host != "api.example.test" || a.ServerName != "api.example.test" {
		t.Fatalf("authority = %+v", a)
	}
}
```

Table tests cover pass/rewrite/node Host, DNS positive/negative TTL, search-option policy, IPv4/IPv6, cancellation, weighted RR, priority, zero weight, passive success/failure thresholds, all-unhealthy compat fail-open, strict fail-closed divergence, retries only for replayable bodies and pinned methods/status/transport failures, retry timeout, and per-attempt authority/SNI.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/proxy ./pkg/route -run "^(TestResolveAuthority|TestResolver|TestRetryCompatibility|TestLoadBalance|TestPassiveHealth)" -count=1'`

Expected: FAIL because SNI is not part of transport identity and DNS ownership is incomplete.

- [ ] **Step 3: Implement owned resolution and exact retry boundaries**

The resolver uses configured servers and TTLs, never `HTTP_PROXY`/`HTTPS_PROXY`; `http.Transport.Proxy` is nil. `TransportOption` and `ClusterKey` include upstream protocol, verify mode, root/client certificate fingerprints, ServerName, DNS policy, all timeouts, keepalive limits, and PROXY-to-upstream flag. Retry obtains a new healthy target, records each upstream status, and stops when body replay or method/status policy forbids another attempt.

- [ ] **Step 4: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/proxy ./pkg/route -run "^(TestResolveAuthority|TestResolver|TestRetryCompatibility|TestLoadBalance|TestPriority|TestPassiveHealth|TestActiveHealth|TestClusterKey)" -count=1'`

Expected: PASS.

```bash
git add pkg/proxy pkg/route pkg/resource
git commit -m "feat(proxy): own APISIX upstream selection and authority"
```

### Task 8: Stream Bodies by Default with Bounded Memory and Temporary Spool

**Files:**
- Create: `pkg/proxy/body_plan.go`, `body_plan_test.go`
- Modify: `pkg/route/plugin_compile.go`, `upstream_compile.go`
- Modify: `pkg/plugin/base/{phase_descriptor,response_phase,streaming_phase}.go`
- Modify: `pkg/apisix/ctx/context.go`

**Interfaces:**
- Consumes: plugin body requirements, profile selection, runtime temp path, upstream buffering/retry needs.
- Produces: `BodyDirection`, `BodyBudget.Reserve`, `NewBodyBudget`, `CompileBodyPlan`, and `MaterializeRequestBody(context.Context, BodyPlan, BodyBudget, io.ReadCloser)` for plan 07 Task 3; request spool implements `io.ReadCloser` plus replay opening without unbounded `[]byte`.

- [ ] **Step 1: Write failing body-plan and cleanup tests**

```go
func TestBodyPlanSpoolsPastMemoryAndCleansExactlyOnce(t *testing.T) {
	plan := BodyPlan{Request: BodySpool, MemoryLimit: 8, SpoolLimit: 32, TempDir: t.TempDir()}
	budget := NewBodyBudget(plan)
	body, err := MaterializeRequestBody(context.Background(), plan, budget, io.NopCloser(strings.NewReader("0123456789")))
	if err != nil { t.Fatal(err) }
	if body.InMemory() { t.Fatal("body remained in memory") }
	path := body.spoolPath
	if err := body.Close(); err != nil { t.Fatal(err) }
	if err := body.Close(); err != nil { t.Fatal(err) }
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) { t.Fatalf("spool still exists: %v", err) }
	impl := budget.(*planBodyBudget)
	impl.mu.Lock(); defer impl.mu.Unlock()
	for key, used := range impl.used {
		if used != 0 { t.Fatalf("budget %v retained %d bytes", key, used) }
	}
}
```

Add streaming-first, exact-limit, overflow-before-upstream, client-cancel cleanup, retry replay, response transform buffering, streaming logger prefix capture, concurrent quota, and no temp-path exposure cases.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/proxy ./pkg/route ./pkg/plugin/base ./pkg/apisix/ctx -run "^(TestBodyPlan|TestStreamingBody|TestSpool|TestRequestBody)" -count=1'`

Expected: FAIL because request buffering currently uses `io.ReadAll`.

- [ ] **Step 3: Compile requirements and delete unconditional buffering**

No body-reading plugin means `BodyStream`. Replay-only requests use memory then spool; body-transform plugins request full bounded bytes; response filters choose streaming unless their descriptor requires full bytes. Compat limits derive from APISIX upstream/body settings; strict limits derive from explicit strict quotas. `MaterializeRequestBody` requires a non-nil budget, reserves every retained read chunk before appending/writing, acquires spool bytes before releasing transferred memory bytes, and stores every release closure on `ReplayableBody`. Every read/write/cancel/overflow error closes the input/file, removes a partial spool, and invokes all accumulated releases. `ReplayableBody.Close` uses `closeOnce` to close/remove/release exactly once. Delete `bufferRequestBodyIfNeeded` and its bypass context flags.

- [ ] **Step 4: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/proxy ./pkg/route ./pkg/plugin/base ./pkg/apisix/ctx -run "^(TestBodyPlan|TestStreamingBody|TestSpool|TestRequestBody|TestResponseMode)" -count=1'`

Expected: PASS and `rg -n 'bufferRequestBodyIfNeeded|io\.ReadAll\(r\.Body\)' pkg/route pkg/proxy --glob '*.go'` prints nothing.

```bash
git add pkg/proxy pkg/route pkg/plugin/base pkg/apisix/ctx
git commit -m "refactor(http): stream bodies with bounded spool plans"
```

### Task 9: Preserve Flush, Chunking, Trailers, Informational Responses, and Abort Semantics

**Files:**
- Modify: `pkg/proxy/proxy.go`
- Modify: `pkg/plugin/base/{writer,response_phase,streaming_phase,outcome_writer}.go`
- Modify: `pkg/server/route_handler.go`
- Test: `pkg/route/phase_compat_test.go`, `pkg/proxy/handler_test.go`, `pkg/server/route_handler_test.go`

**Interfaces:**
- Consumes: `ProtocolPlan`, `BodyPlan`, response outcome/finalizer ownership.
- Produces: one response writer path preserving legal 1xx/final/trailer/chunk/flush transitions.

- [ ] **Step 1: Write the failing wire-observable test**

```go
func TestStreamingResponsePreservesEarlyHintsFlushAndTrailer(t *testing.T) {
	result := runRawHTTPFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Trailer", "X-Checksum")
		_, _ = io.WriteString(w, "one")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "two")
		w.Header().Set("X-Checksum", "abc")
	})
	assertWireSequence(t, result, 103, 200, "one", "two", "X-Checksum: abc")
}
```

Add HEAD/204/304 no-body, chunk boundary, declared/undeclared trailer, flush before write, plugin panic before/after commit, upstream EOF, downstream cancel, hijack, and finalizer-once tests.

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/proxy ./pkg/plugin/base ./pkg/server ./pkg/route -run "^(TestStreamingResponse|TestInformational|TestTrailer|TestFlush|TestResponseAbort|TestFinalizer)" -count=1'`

Expected: FAIL where buffered and streaming writers do not share the full state machine.

- [ ] **Step 3: Implement one response state machine**

Informational headers never commit the final response. First final status freezes ownership; trailers remain mutable through body EOF; flush marks the connection committed; panic/error after commit returns `http.ErrAbortHandler` and never writes a JSON fallback. Remove the stale proxy streaming comments because the replacement is executable and tested.

- [ ] **Step 4: Run race tests and commit**

Run: `bash -lc 'source .envrc && go test -race ./pkg/proxy ./pkg/plugin/base ./pkg/server ./pkg/route -run "^(TestStreamingResponse|TestInformational|TestTrailer|TestFlush|TestResponseAbort|TestFinalizer|TestWebSocket)" -count=1'`

Expected: PASS.

```bash
git add pkg/proxy pkg/plugin/base pkg/server pkg/route
git commit -m "fix(http): preserve streaming response semantics"
```

### Task 10: Apply Compat/Strict Defaults and Eliminate Parsed-but-Ignored HTTP Fields

**Files:**
- Modify: `pkg/config/validation.go`, `pkg/config/types.go`, `pkg/config/effective_test.go`
- Modify: `pkg/server/listener_policy.go`
- Modify: `pkg/route/{protocol_plan,upstream_compile}.go`
- Modify: `pkg/proxy/{body_plan,transport,resolver}.go`
- Modify: `pkg/capability/manifest.yaml`, ADR index

**Interfaces:**
- Consumes: `config.ProfileSelection` and manifest divergence records.
- Produces: `ValidateHTTPCompatibility(*config.EffectiveConfig) error`; every accepted HTTP field has an owner.

- [ ] **Step 1: Write the failing ownership table test**

```go
func TestEveryAcceptedHTTPFieldHasBehaviorOrExplicitRejection(t *testing.T) {
	owners := make(map[string]string)
	for _, field := range HTTPFieldOwners() { owners[field.Path] = field.Owner }
	for _, path := range []string{
		"apisix.node_listen[].enable_http2", "apisix.node_listen[].proxy_protocol",
		"apisix.dns_resolver", "apisix.resolver_timeout", "nginx_config.http.real_ip_header",
		"nginx_config.http.upstream.keepalive", "proxy.tls_handshake_timeout",
	} {
		if owner := owners[path]; owner == "" { t.Errorf("field %s has no owner", path) }
	}
}
```

- [ ] **Step 2: Run and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/config ./pkg/server ./pkg/route ./pkg/proxy -run "^(TestEveryAcceptedHTTPField|TestCompatibilityDefaults|TestStrictDivergence)" -count=1'`

Expected: FAIL for accepted fields without a runtime owner.

- [ ] **Step 3: Enforce profile defaults and atomic field cleanup**

Compat preserves pinned trust, TLS verify, retry, all-unhealthy, timeout, Host, and body defaults. Strict enables only manifest-recorded trusted CIDRs, TLS verification, origin policy, and quotas. For every config/resource HTTP field, either compile it into listener/router/proxy/body behavior or reject it with its full field path during effective config or generation preparation. Remove the old decoder/storage branch in the same commit; no ignored field or fallback adapter remains.

- [ ] **Step 4: Run ownership scans and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/config ./pkg/server ./pkg/route ./pkg/proxy -run "^(TestEveryAcceptedHTTPField|TestCompatibilityDefaults|TestStrictDivergence|TestUnsupportedHTTPField)" -count=1'`

Expected: PASS; all active strict divergences have manifest ID, ADR, owner approval reference, and tests for both profiles.

```bash
git add pkg/config pkg/server pkg/route pkg/proxy pkg/capability docs/architecture
git commit -m "feat(compat): govern HTTP defaults and divergences"
```

### Task 11: Add APISIX Differential Cases, Update Evidence, and Run the HTTP Milestone Gate

**Files:**
- Create: `t/plugin/http-protocol.yaml`
- Modify: `t/plugin/case.go`, `t/plugin/runner_test.go`, `t/plugin/corpus_test.go`, `t/plugin/corpus_scope.yaml`
- Modify: `pkg/capability/manifest.yaml`, `cmd/capability-gen/main.go`, `cmd/capability-gen/main_test.go`
- Modify: `docs/architecture/http-compatibility.md`, `docs/design.md`, `docs/plugins.md`, `README.md`

**Interfaces:**
- Consumes: completed listener/router/plugin/proxy behaviors and the pinned APISIX oracle.
- Produces: normalized Go/APISIX differential results and honest evidence maturity.

- [ ] **Step 1: Add exact differential manifest cases**

```yaml
schema_version: 1
source:
  repository: https://github.com/apache/apisix
  commit: 9ef2ecab67f652d38365049613610ef649bb4ad0
  file: t/router/radixtree-host-uri-priority.t
cases:
  - id: core-route-host-uri-priority
    capability: core-http-routing
    source: {tests: [1]}
    config:
      routes:
        - {id: low, uri: /hello/*, host: api.example.com, priority: 1, upstream: {nodes: {"{{dependency.upstream.address}}": 1}}}
        - {id: high, uri: /hello/world, host: api.example.com, priority: 10, upstream: {nodes: {"{{dependency.upstream.address}}": 1}}}
    request:
      method: GET
      path: /hello/world
      headers: {Host: [api.example.com]}
      body_base64: ""
    dependencies:
      - id: upstream
        kind: http
        address: 127.0.0.1:0
        responses:
          - {status: 200, headers: {Content-Type: [text/plain]}, body_base64: aGlnaA==, delay: 0s}
    expected:
      status: 200
      headers: {Content-Type: [text/plain]}
      body_base64: aGlnaA==
      route_id: high
      upstream_id: ""
      attempt_count: 1
      host: api.example.com
      server_name: ""
    determinism: {clock: "2026-08-23T00:00:00Z", rng_seed: "1"}
    normalization: obs-v1
```

This file is decoded only as plan 08's strict `qualification.DifferentialManifest`; its field names and nesting are exact. The runner substitutes the allocated dependency address for `{{dependency.upstream.address}}`. It must not infer legacy `name`, `input`, `upstream`, or `output` shapes.

Add cases for the protocol matrix, SNI/mTLS/PROXY/real-IP, vars/consumer phases/errors, retries/DNS/LB/passive health/authority, streaming/spool boundaries, flush/trailers/103/cancel/reuse. Normalization may remove Date, generated request IDs, ephemeral ports, and server version only when listed in a versioned normalization table; it cannot normalize status, headers under test, body bytes, attempt count, selected route, Host, or SNI.

- [ ] **Step 2: Run corpus tests then selected real-process cases**

Run: `bash -lc 'source .envrc && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./t/plugin -run "^(TestHTTPProtocolManifest|TestHTTPCorpusSourceAccounting|TestDifferentialNormalization)$" -count=1'`

Expected: PASS.

Run one case at a time: `bash -lc 'source .envrc && APISIX_GO_PLUGIN_SMOKE_CASE=http-protocol/core-route-host-uri-priority go test ./t/plugin -run "^TestPluginIntegration$" -count=1'`

Expected: PASS. Do not run concurrent real-process `t/plugin` commands.

- [ ] **Step 3: Update evidence without overstating gaps**

Promote only rows whose required unit, converted-upstream, differential, failure, and recovery evidence passed. HTTP/3/QUIC, Lua filter/script, external runners, and `inspect` remain explicit gaps. Generated `docs/plugins.md` and README summaries must derive from the manifest rather than manual claims.

- [ ] **Step 4: Run the focused milestone gate and absence scans**

```bash
bash -lc 'source .envrc && go test -race ./pkg/server ./pkg/route ./pkg/proxy ./pkg/plugin ./pkg/apisix/ctx ./pkg/apisix/variable -run "^(TestHTTP|TestTLS|TestProxyProtocol|TestRoute|TestRadixtree|TestVariable|TestConsumer|TestPhase|TestRetry|TestResolver|TestLoadBalance|TestPassiveHealth|TestStreaming|TestBodyPlan|TestAuthority|TestTrailer|TestInformational|TestCancellation|TestConnectionReuse)" -count=1'
bash -lc 'source .envrc && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./pkg/capability ./t/plugin -run "^(TestHTTPProtocolCapabilities|TestHTTPProtocolManifest|TestHTTPCorpusSourceAccounting|TestDifferentialNormalization|TestCorpusEvidenceMatchesCompatibilityTarget)$" -count=1'
bash -lc 'source .envrc && make build'
bash -lc 'source .envrc && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...'
git diff --check
```

Expected: PASS. Then run:

```bash
! rg -n 'frontendHTTP2Enabled|frontendPlainHTTP2Enabled|routeRegistrar|wildcardDispatcher|routeDecisionIndex|RequestStageLegacy|legacyRemainderBindings|thenLegacy|bufferRequestBodyIfNeeded' pkg --glob '*.go'
! rg -n 'not implemented.*(websocket|streaming)|parsed.but.ignored|silently ignore' pkg/server pkg/route pkg/proxy pkg/plugin pkg/apisix --glob '*.go'
```

Expected: no output.

- [ ] **Step 5: Commit evidence and documentation**

```bash
git add t/plugin pkg/capability docs README.md
git commit -m "test(compat): qualify APISIX 3.17 HTTP behavior"
```

## Plan Self-Review

- Spec coverage: downstream H1/H2/h2c/gRPC/WebSocket/trailers/1xx/cancel/reuse, listener TLS/PROXY/real-IP/trust/timeouts, radixtree matching, variables/consumer/phases/errors, upstream protocol/DNS/retry/LB/health/authority, bounded body plans, flush/chunk/trailers, compat/strict, gaps, evidence, and atomic deletion each map to a named red/green task.
- Type consistency: Tasks 2–11 use the exact `ListenerPolicy`, `ListenerRuntime.Serve/Shutdown`, `RealIPPolicy`, `RouteIndex`, `ProtocolPlan`, `BodyDirection`, `BodyPlan`, `BodyBudget`, `ReplayableBody`, `Authority`, and `Resolver` signatures declared under Shared Interfaces; plan 04 `compiler.HTTPSnapshot.Handler/TLSConfig` and `PreparedGeneration` remain unchanged.
- Dependency consistency: plan 01 supplies capability/evidence/divergence truth; plan 02 supplies effective profiles and runtime paths; plan 04 supplies `route.CompileInput` with `Routes []resource.Route`, immutable HTTP compilation/bindings, and the only TLS config; plan 05 supplies exact `worker.HTTPRuntime` registration/serve/shutdown over inherited listeners. Task 8 produces the exact body-budget seam consumed by plan 07 Task 3; plan 08 consumes the strict differential manifests and evidence produced by Task 11.
- Compatibility boundary: HTTP/3/QUIC, Lua execution, external runners, and `inspect` remain visible non-Full gaps; strict changes require manifest divergence plus approved ADR.
- Command consistency: every Go/Make command sources `.envrc`, real-process `t/plugin` runs are selected and serial, verification is impact-scoped, and Linux/macOS runtime plus experimental Windows source-build boundaries remain explicit.
- Legacy/field coverage: each replacement task deletes its old path in the same commit, and the final scans reject old matcher/executor/listener/body symbols and accepted-but-unowned fields.
