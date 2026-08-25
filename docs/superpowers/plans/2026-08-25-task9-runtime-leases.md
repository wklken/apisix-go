# Task 9 Runtime Bundle, Protocol Leases, and Observer Injection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace mutable HTTP/TLS/stream runtime ownership with one composite immutable generation bundle whose request, batch, hijack, TLS, and stream users hold explicit leases, while preserving cluster and stream observability.

**Architecture:** `GenerationEngine` owns one atomic `activeBundle` with independently replaceable HTTP and stream owner slots. Protocol code acquires a domain lease from that bundle and never reads Store or mutates a router; retirement only closes a prepared generation after its active slots and all leases reach zero. Compiler construction receives explicit runtime observers, and a compiler-owned overlap registry delays cluster metric deletion until the final same-name resource instance is released.

**Tech Stack:** Go 1.26, `sync`, `sync/atomic`, `net/http`, `crypto/tls`, `go-chi`, immutable `compiler.PreparedGeneration` snapshots, `runtime.ResourceRegistry`, focused `go test` and `go test -race` gates.

**Spec:** `docs/superpowers/plans/2026-08-25-immutable-task9-joint-cutover.md`, Task 9 in `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`, and `docs/superpowers/plans/2026-08-24-journal-immutable-cutover-reorder.md`.

## Global Constraints

- Execute from the Task 9 integration ancestry based on `16b28bb54162da6e94560d772c46640c719cdde6`; if the integration base advances, recreate the lane from that merged base instead of rebasing an in-flight dependent lane.
- Source `.envrc` before every Go command and set `GOFLAGS=-mod=readonly`; do not run broad `go test ./...` or `make test` as routine verification.
- Do not add a feature flag, compatibility switch, second selectable production path, forwarding `Replace`/`Reload` facade, or Store-backed fallback.
- Ordinary generation retirement never closes HTTP hijacks, stream connections, or listeners. Terminal server shutdown rejects new leases, closes listeners, cancels active stream connections, and may force-close tracked HTTP hijacks under the server drain policy.
- `FinalizeActivation` only changes ownership and queues retirement; it performs no close, wait, IPC, or drain work.
- `drained` is a one-way signal. Tentative activation must not deactivate predecessor domains, and this design does not add a compensating `activationHolds` counter or any mechanism that tries to reopen `drained`.
- Workers own only the files assigned below and return diffs plus command evidence. They do not commit, push, merge, delete another lane's files, or revert concurrent changes; the Task 9 integration owner performs reviewed integration.
- Final production activation is atomic: server/bootstrap and legacy deletion must land in the same integration checkpoint so no commit exposes both legacy and immutable production selection.

---

## File and Responsibility Map

| Lane | Exclusive files | Responsibility | Must not edit |
| --- | --- | --- | --- |
| Contract/owner | Create `pkg/server/generation_owner.go`, `pkg/server/generation_owner_test.go` | Composite bundle, per-domain active-slot state, lease retention/release, drain eligibility | `generation_engine.go`, providers, bootstrap |
| Route helper prerequisite | Modify `pkg/route/plugin_compile.go`, `prepared_handler.go`, `upstream_compile.go`, `upstream_options.go`, `compiler.go`, `router.go` and directly corresponding tests | Move live pure Builder helpers without wrappers or behavior change | server activation, Builder deletion |
| HTTP/TLS | Modify `pkg/server/route_handler.go`, `route_handler_test.go`, `tls.go`, `tls_test.go`; create `pkg/server/generation_conn.go`, `generation_conn_test.go` | Request/batch/hijack/TLS lease consumption and terminal hijack registry | engine transaction, bootstrap, Store deletion |
| Stream | Modify `pkg/stream/runtime.go`, `runtime_test.go`, `router.go`, `router_test.go` | Listener-only runtime, `RouterSource`, per-connection leases, immutable router | server/bootstrap, compiler |
| Observers | Create `pkg/compiler/runtime_observers.go`, `runtime_observers_test.go`; modify `worker_factory.go`, `worker_factory_test.go`, `worker_factory_recovery_test.go`, `prepared_generation.go`, `http_cluster.go`, `http_cluster_test.go`, `stream_compile.go`, `stream_compile_test.go` | Required observer injection, same-name overlap safety, stream result attachment | server metrics implementation, proxy registry deletion |
| Engine handoff | Owned by the sibling engine plan: `pkg/server/generation_engine.go`, its tests and retirement loop | Atomic swap, rollback, finalize, recovery and source methods | Protocol internals in this plan |
| Bootstrap handoff | Owned by sibling bootstrap plan: `cmd/root.go`, `pkg/server/server.go` and startup/shutdown tests | Construct observers/factory/engine, pass lease sources, terminal close order | Owner/lease internals |
| Legacy handoff | Owned by sibling deletion plan: `pkg/server/reload.go`, Store/Event files, `pkg/route/builder.go`, `pkg/proxy/registry.go` and obsolete tests | Delete old callers and owners after the new production path is green | Reintroducing adapters or flags |

The route helper lane must finish before `builder.go` deletion. Observer injection and helper extraction may run in parallel. HTTP/TLS and stream lanes may run in parallel after the owner contract is fixed, but their production caller swap is serialized with engine/bootstrap integration.

## Frozen Runtime Contracts

```go
type activeBundle struct {
	http   *generationOwner
	stream *generationOwner
}

type ownerDomain uint8

const (
	ownerDomainHTTP ownerDomain = 1 << iota
	ownerDomainStream
)

type generationOwner struct {
	prepared *compiler.PreparedGeneration
	// mu protects activeDomains, leases and drain closure.
}

type httpGenerationLease struct {
	Snapshot *compiler.HTTPSnapshot
	Release  func()
	retain   func() (httpGenerationLease, bool)
}

type streamGenerationLease struct {
	Router  *stream.Router
	Release func()
}

type httpLeaseSource func() (httpGenerationLease, bool)
type streamLeaseSource func() (streamGenerationLease, bool)
```

`activeBundle` is immutable after publication. One owner may occupy both slots. `generationOwner` tracks HTTP and stream slot activity independently: a replaced HTTP slot must reject a stale HTTP acquisition even when the same owner remains active in the stream slot. Acquisition loaded just before a swap may either complete on that owner or fail and retry the current pointer; it must never attach an HTTP request to an owner retained only by stream.

The engine keeps predecessor domain slots active throughout tentative activation because `drained` cannot be reopened. `Activate` marks candidate domains active and atomically publishes the candidate bundle, but does not change predecessor `activeDomains`. `RollbackActivation` atomically restores the complete predecessor bundle and then deactivates only the candidate domains; the predecessor requires no reactivation and remains non-drained even when it had zero leases during the tentative window. Only durable `FinalizeActivation` deactivates the replaced predecessor domains. When this removes the owner's final active domain, the engine enqueues it exactly once even if leases remain. `drained` closes only when `activeDomains == 0 && leases == 0`; the retirement loop waits for that one-way signal and alone calls `PreparedGeneration.Close`.

```go
type RouterLease struct {
	Router  *Router
	Release func()
}

type RouterSource func() (RouterLease, bool)

func NewRuntime(context.Context, []config.TcpListen, RouterSource) (*Runtime, error)

type WorkerRuntimeObservers struct {
	Cluster proxy.ClusterObserver
	Stream  func(stream.Result)
}

func NewWorkerCompilerFactory(
	manifest *capability.Manifest,
	effective *config.EffectiveConfig,
	materializer secret.Materializer,
	observers WorkerRuntimeObservers,
) (*WorkerCompilerFactory, error)
```

The server-facing `GenerationEngine.acquireHTTP` and `GenerationEngine.acquireStream` methods implement the source functions. Test fakes implement the same closures. Batch and hijack retention use the unexported `httpGenerationLease.retain`, so children remain pinned to the request's exact owner instead of reacquiring whatever is current.

All fixture names used below are test-only helpers created in the named test file in the same RED step; none is a production seam. Their contracts are fixed as follows: owner fixtures build a real `compiler.PreparedGeneration` through the existing worker-factory fixture path and expose drain/close counters; HTTP/TLS fixtures return counted `httpGenerationLease` values whose `retain` returns the same snapshot; router fixtures return counted `RouterLease` values around real `CompileRouter` results; switchable sources use `atomic.Pointer` and expose `Store`; server helpers use `httptest` or `net.Pipe`; recording observers implement both `proxy.ClusterObserver` and `proxy.UpstreamStatusObserver`. Every wait uses an explicit channel or a one-second test deadline; no sleep is used to infer lifecycle state.

---

### Task 1: Prove and Extract the Pure Route Helper Prerequisite

**Files:**
- Modify: `pkg/route/plugin_compile.go`
- Modify: `pkg/route/prepared_handler.go`
- Modify: `pkg/route/upstream_compile.go`
- Modify: `pkg/route/upstream_options.go`
- Modify: `pkg/route/compiler.go`
- Modify: `pkg/route/router.go`
- Modify: directly corresponding `pkg/route/*_test.go` files
- Read-only until legacy cutover: `pkg/route/builder.go`

**Interfaces:**
- Consumes: current pure helper implementations and tests in `builder.go`.
- Produces: the same unexported names in their actual consumer files, with no forwarding declaration left in `builder.go`; `Builder` remains buildable but owns lifecycle only.

- [ ] **Step 1: Record the exact move inventory and current callers**

Run:

```bash
rg -n '^(func|type|var|const) (applyFinalProxyRewrite|applyTrafficSplitOverride|applyUpstreamTargetCompiled|attachHTTPRetriesCompiled|bufferRequestBodyIfNeeded|buildKafkaPubSubProxyHandlerStrictWithSSLResolver|buildSystemPluginConfigs|buildTransparentUpgradeHandler|compileUpstreamTargets|consumerPluginSources|deduplicateGlobalRules|ensureRouteLifecycle|errEmptyUpstream|healthReporter|httpRetryCount|isWebsocketUpgradeRequest|markUpstreamStart|matchedHost|materializedPluginSource|materializedPluginSources|newErrorHandler|newMetadataPluginWithDescriptor|newModifyResponse|newRequestPipelineWithLog|parsePluginMetadata|pinDecodedRoutePath|pluginMetadata|requireWebsocketEnablement|resolveRouteUpstreamWithGetter|routeDubboTerminal|routeHTTPDubboTerminal|routeKafkaTerminal|routeProtocolTerminals|selectMaterializedPluginSources|selectProxyHandler|trafficSplitRoundTripper|validateHTTPUpstreamType|validateRouteCompatibility|validateUnsupportedUpstreamDiscovery|withDirectorError)\b' pkg/route/builder.go
rg -n '\b(applyFinalProxyRewrite|applyTrafficSplitOverride|applyUpstreamTargetCompiled|attachHTTPRetriesCompiled|bufferRequestBodyIfNeeded|buildKafkaPubSubProxyHandlerStrictWithSSLResolver|buildSystemPluginConfigs|buildTransparentUpgradeHandler|compileUpstreamTargets|consumerPluginSources|deduplicateGlobalRules|ensureRouteLifecycle|httpRetryCount|isWebsocketUpgradeRequest|markUpstreamStart|matchedHost|newErrorHandler|newMetadataPluginWithDescriptor|newModifyResponse|newRequestPipelineWithLog|parsePluginMetadata|pinDecodedRoutePath|requireWebsocketEnablement|resolveRouteUpstreamWithGetter|routeDubboTerminal|routeHTTPDubboTerminal|routeKafkaTerminal|routeProtocolTerminals|selectMaterializedPluginSources|selectProxyHandler|trafficSplitRoundTripper|validateHTTPUpstreamType|validateRouteCompatibility|validateUnsupportedUpstreamDiscovery|withDirectorError)\b' pkg/route --glob '*.go'
```

Expected: every declaration is in `builder.go`; every non-Builder consumer is mapped to one of the six destination files before editing.

- [ ] **Step 2: Run the focused helper behavior baseline**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/route -run "(Plugin|Prepared|Upstream|Router|TrafficSplit|Websocket|Kafka|Dubbo|Retry|Lifecycle)" -count=1'
```

Expected: PASS before the mechanical move. Record any pre-existing failure with its test name and message; do not classify it as a move failure.

- [ ] **Step 3: Move declarations without changing bodies**

Move plugin materialization and terminal-selection helpers to `plugin_compile.go`; request lifecycle and response helpers to `prepared_handler.go`; target compilation and protocol validation to `upstream_compile.go`; retry/timeout option helpers to `upstream_options.go`; compilation orchestration helpers to `compiler.go`; and host/path/router selection helpers to `router.go`. Move associated imports with the declarations. Do not retain aliases or delegating functions in `builder.go`.

- [ ] **Step 4: Re-run behavior and symbol gates**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/route -run "(Plugin|Prepared|Upstream|Router|TrafficSplit|Websocket|Kafka|Dubbo|Retry|Lifecycle)" -count=1'
rg -n '^(func|type|var|const) (applyFinalProxyRewrite|applyTrafficSplitOverride|applyUpstreamTargetCompiled|attachHTTPRetriesCompiled|bufferRequestBodyIfNeeded|buildSystemPluginConfigs|compileUpstreamTargets|ensureRouteLifecycle|newRequestPipelineWithLog|selectProxyHandler|validateRouteCompatibility)\b' pkg/route/builder.go
```

Expected: tests PASS and the final command returns no matches. Return the diff and evidence to the integration owner; do not commit it.

---

### Task 2: Implement Composite Bundle and Generation Owner Leases

**Files:**
- Create: `pkg/server/generation_owner.go`
- Create: `pkg/server/generation_owner_test.go`

**Interfaces:**
- Consumes: `compiler.PreparedGeneration.HTTP()`, `compiler.PreparedGeneration.Stream()`, `compiler.PreparedGeneration.Close(context.Context)` and immutable snapshot accessors.
- Produces: `activeBundle`, `generationOwner`, `httpGenerationLease`, `streamGenerationLease`, `activateDomains`, `deactivateDomains`, `acquireHTTP`, `acquireStream`, `drained`, and `closePrepared` for the engine lane.

- [ ] **Step 1: Write RED lifecycle tests**

Add tests with real prepared-generation fixtures or a package-private close spy:

```go
func TestGenerationOwnerHTTPRetirementDoesNotRetireActiveStreamSlot(t *testing.T) {
	owner := newTestGenerationOwner(t, 41, true, true)
	owner.activateDomains(ownerDomainHTTP | ownerDomainStream)
	httpLease, ok := owner.acquireHTTP()
	if !ok { t.Fatal("acquireHTTP() = false") }
	owner.deactivateDomains(ownerDomainHTTP)
	if _, ok := owner.acquireHTTP(); ok { t.Fatal("stale HTTP acquisition succeeded") }
	streamLease, ok := owner.acquireStream()
	if !ok { t.Fatal("stream slot was retired with HTTP") }
	httpLease.Release()
	streamLease.Release()
	assertNotDrained(t, owner)
	owner.deactivateDomains(ownerDomainStream)
	assertDrained(t, owner)
}

func TestGenerationOwnerLeaseReleaseIsExactlyOnce(t *testing.T) {
	owner := newTestGenerationOwner(t, 42, true, false)
	owner.activateDomains(ownerDomainHTTP)
	lease, ok := owner.acquireHTTP()
	if !ok { t.Fatal("acquireHTTP() = false") }
	lease.Release()
	lease.Release()
	owner.deactivateDomains(ownerDomainHTTP)
	assertDrained(t, owner)
}
```

Also add `TestActiveBundlePreservesUntouchedDomainOwner`, `TestGenerationOwnerRetainedHTTPLeasePinsSameSnapshot`, `TestGenerationOwnerDoesNotClosePreparedOnDrain`, and `TestGenerationOwnerClosePreparedWaitsForDrainAndClosesOnce`.

- [ ] **Step 2: Run RED tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestGenerationOwner|TestActiveBundle)" -count=1'
```

Expected: FAIL to compile because the owner types and constructors do not exist.

- [ ] **Step 3: Implement the minimal owner state machine**

Use one mutex for `activeDomains`, `leases`, and drain closure. `activateDomains` rejects an already-active requested domain; `deactivateDomains` rejects a missing domain and reports whether the owner now has no active domain so the engine can enqueue it exactly once. `acquireHTTP`/`acquireStream` verify that domain and snapshot exist, increment `leases`, and return a close-once release. The HTTP lease's private `retain` calls the same owner's HTTP acquisition, preserving the exact owner. Neither deactivation nor release calls or enqueues `PreparedGeneration.Close`; release only closes `drained` when the already-enqueued owner reaches zero leases.

```go
func (owner *generationOwner) acquireHTTP() (httpGenerationLease, bool) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.activeDomains&ownerDomainHTTP == 0 || owner.prepared == nil || owner.prepared.HTTP() == nil {
		return httpGenerationLease{}, false
	}
	owner.leases++
	return owner.newHTTPLeaseLocked(), true
}

func (owner *generationOwner) release() {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.leases == 0 { panic("release generation owner without lease") }
	owner.leases--
	owner.signalDrainedLocked()
}
```

`closePrepared(ctx)` waits for `drained` or `ctx.Done`, then invokes `prepared.Close` once and replays the first close error. It is called only by the engine retirement loop or terminal engine shutdown.

- [ ] **Step 4: Run focused and race GREEN tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestGenerationOwner|TestActiveBundle)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^(TestGenerationOwner|TestActiveBundle)" -count=1'
```

Expected: PASS. Return the two new files and evidence; do not edit `generation_engine.go` in this lane.

---

### Task 3: Bind HTTP Requests and Batch Dispatch to Exact Generation Leases

**Files:**
- Modify: `pkg/server/route_handler.go`
- Modify: `pkg/server/route_handler_test.go`

**Interfaces:**
- Consumes: `httpLeaseSource` and `httpGenerationLease.retain` from Task 2; existing `batch_requests.WithDispatchLeaseFactory`.
- Produces: `newRouteHandler(httpLeaseSource) *routeHandler`, request fail-closed behavior, and exact-owner child dispatch retention.

- [ ] **Step 1: Replace mutable route-set tests with RED lease tests**

Add a deterministic fake source whose generations return distinct status codes and counted releases:

```go
func TestRouteHandlerRequestRetainsLoadedGenerationAcrossSwap(t *testing.T) {
	old := newHTTPLeaseFixture(http.StatusOK)
	next := newHTTPLeaseFixture(http.StatusCreated)
	source := newSwitchableHTTPLeaseSource(old)
	routes := newRouteHandler(source.Acquire)
	started, unblock := old.blockHandler()
	requestDone := serveAsync(routes)
	<-started
	source.Store(next)
	assertHTTPStatus(t, routes, http.StatusCreated)
	assertReleaseCount(t, old, 0)
	close(unblock)
	<-requestDone
	assertReleaseCount(t, old, 1)
}
```

Add `TestRouteHandlerUnavailableHTTPDomainReturns503`, `TestRouteHandlerBatchDispatchRetainsParentGenerationAfterSwap`, `TestRouteHandlerBatchReleaseIsExactlyOnce`, and `TestRouteHandlerDoesNotAcquireNewGenerationForChildDispatch`.

- [ ] **Step 2: Run RED tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestRouteHandlerRequestRetains|TestRouteHandlerUnavailable|TestRouteHandlerBatch)" -count=1'
```

Expected: FAIL because `newRouteHandler` still accepts a mutable handler/stop pair and batch retention is tied to `routeSet`.

- [ ] **Step 3: Replace `routeSet` request counting with source leases**

`ServeHTTP` acquires once, returns 503 when unavailable, defers `Release`, and passes the lease into `serveRouteRequestForGeneration`. The batch factory calls `lease.retain`, not `source`, and recursively dispatches with the retained snapshot handler. Keep the request lifecycle, panic, response capture, finalizer, and body-limit code byte-for-byte unless the parameter change requires a direct edit.

```go
func (handler *routeHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	lease, ok := handler.source()
	if !ok || lease.Snapshot == nil || lease.Snapshot.Handler() == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	defer lease.Release()
	serveRouteRequestForGeneration(w, request, lease.Snapshot.Handler(), &lease)
}
```

Use the actual existing `HTTPSnapshot` handler accessor name from `pkg/compiler/http.go`; do not add a duplicate accessor. Delete `Replace`, route-set retirement, request counters, and stop callbacks only at the production integration checkpoint after engine/bootstrap callers are ready.

- [ ] **Step 4: Run GREEN tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestRouteHandler|TestRequest|TestResponse|TestBodyLimit)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^(TestRouteHandlerRequestRetains|TestRouteHandlerBatch)" -count=1'
```

Expected: PASS. If the lane uses an uncalled additive constructor while legacy production remains live, keep it package-private; it is removed or renamed to the final `newRouteHandler` in the atomic integration checkpoint, never selected by a flag.

---

### Task 4: Give Successful Hijacks a Connection-Lifetime Lease

**Files:**
- Create: `pkg/server/generation_conn.go`
- Create: `pkg/server/generation_conn_test.go`
- Modify: `pkg/server/route_handler.go`
- Modify: `pkg/server/route_handler_test.go`

**Interfaces:**
- Consumes: the current request's `httpGenerationLease.retain` and `httpsnoop` hijack hook.
- Produces: a `generationConn` whose first `Close` closes the underlying connection, unregisters it from terminal tracking, and releases the retained lease exactly once.

- [ ] **Step 1: Write RED natural-drain and terminal-close tests**

```go
func TestGenerationHijackNaturalCloseReleasesLease(t *testing.T) {
	fixture := newHTTPLeaseFixture(http.StatusSwitchingProtocols)
	routes := newRouteHandler(fixture.Acquire)
	conn := hijackAndReturnConnection(t, routes)
	assertReleaseCount(t, fixture, 1) // request lease only
	fixture.Retire()
	assertUnderlyingOpen(t, conn)
	if err := conn.Close(); err != nil { t.Fatal(err) }
	assertReleaseCount(t, fixture, 2) // distinct hijack lease
}

func TestRouteHandlerTerminalCloseForcesHijackAndReleasesLease(t *testing.T) {
	fixture := newHTTPLeaseFixture(http.StatusSwitchingProtocols)
	routes := newRouteHandler(fixture.Acquire)
	conn := hijackAndReturnConnection(t, routes)
	routes.Close()
	assertUnderlyingClosed(t, conn)
	assertReleaseCount(t, fixture, 2)
}
```

Also add failed-hijack, double-close, panic-after-hijack, and concurrent terminal-close/natural-close cases. Replace the old expectation that ordinary replacement closes hijacks; the new expectation is that retirement leaves them open.

- [ ] **Step 2: Run RED tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestGenerationHijack|TestRouteHandlerTerminalClose|TestRouteHandlerRetiresHijacked)" -count=1'
```

Expected: FAIL because the current hook registers the raw connection and unregisters it when the handler returns, while ordinary retirement force-closes it.

- [ ] **Step 3: Implement the wrapped lease**

Acquire the distinct retained lease only after a successful hijack. Wrap the raw connection, return the wrapper to the handler, and rebuild the returned `bufio.ReadWriter` around the wrapper when necessary so every close path reaches it. Register the wrapper in a server-terminal registry; do not unregister it when `ServeHTTP` returns.

```go
type generationConn struct {
	net.Conn
	closeOnce sync.Once
	closeErr  error
	release   func()
	unregister func()
}

func (connection *generationConn) Close() error {
	connection.closeOnce.Do(func() {
		connection.closeErr = connection.Conn.Close()
		connection.unregister()
		connection.release()
	})
	return connection.closeErr
}
```

Ordinary owner retirement only prevents new owner acquisitions. `routeHandler.Close` is terminal: atomically rejects new requests, snapshots tracked wrappers, closes them outside the registry mutex, and waits only according to the sibling bootstrap/server drain policy.

- [ ] **Step 4: Run GREEN and race tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestGenerationHijack|TestRouteHandlerTerminalClose|TestRouteHandlerRetiresHijacked|TestRouteHandlerSuccessfulHijack)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^(TestGenerationHijack|TestRouteHandlerTerminalClose)" -count=1'
```

Expected: PASS, including exactly-once release under concurrent close.

---

### Task 5: Select TLS Only from the Current Immutable HTTP Snapshot

**Files:**
- Modify: `pkg/server/tls.go`
- Modify: `pkg/server/tls_test.go`

**Interfaces:**
- Consumes: `httpLeaseSource`, `compiler.HTTPSnapshot.TLSConfig() *tls.Config`, and the existing base TLS compilation.
- Produces: `buildFrontendTLSConfig(*config.Config, httpLeaseSource) (*tls.Config, error)` with generation-bound `GetConfigForClient` and no Store selector.

- [ ] **Step 1: Write RED generation TLS tests**

Add fixtures with different certificates/client CAs in N and N+1:

```go
func TestFrontendTLSSelectorUsesOneHTTPGenerationLease(t *testing.T) {
	old := newTLSLeaseFixture(t, "old.example", oldCertificate(t))
	next := newTLSLeaseFixture(t, "new.example", newCertificate(t))
	source := newSwitchableHTTPLeaseSource(old)
	selector := frontendTLSConfigSelector(source.Acquire)
	assertSelectedCertificate(t, selector, "old.example", old.serial)
	source.Store(next)
	assertSelectedCertificate(t, selector, "new.example", next.serial)
	assertAcquireReleaseBalanced(t, old, next)
}
```

Add `TestFrontendTLSSelectorRollbackRestoresExactPredecessor`, `TestFrontendTLSSelectorFailsClosedWithoutHTTPDomain`, `TestFrontendTLSSelectorUsesSnapshotClientCAAndDepth`, and `TestFrontendTLSSelectorReleasesLeaseOnSelectionError`.

- [ ] **Step 2: Run RED tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestFrontendTLSSelector|TestFrontendTLSConfig)" -count=1'
```

Expected: FAIL because `tls.go` calls `store.GetSSLCertificateForSNI` and `store.GetSSLCertificateConfigForSNI`.

- [ ] **Step 3: Implement snapshot-only selection**

The outer listener config contains protocol-level defaults and one `GetConfigForClient` closure. That closure acquires HTTP, obtains the immutable snapshot config, invokes its SNI selector when present, clones the final config before release, and fails closed when no HTTP/TLS snapshot is available. Do not install a Store `GetCertificate` fallback.

```go
func frontendTLSConfigSelector(source httpLeaseSource) func(*tls.ClientHelloInfo) (*tls.Config, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		lease, ok := source()
		if !ok || lease.Snapshot == nil || lease.Snapshot.TLSConfig() == nil {
			return nil, errHTTPGenerationUnavailable
		}
		defer lease.Release()
		selected := lease.Snapshot.TLSConfig()
		if selected.GetConfigForClient != nil {
			candidate, err := selected.GetConfigForClient(hello)
			if err != nil { return nil, err }
			if candidate != nil { selected = candidate }
		}
		return selected.Clone(), nil
	}
}
```

Keep `frontendTLSTrustedCertificate` only if bootstrap compilation still consumes it. Remove `frontendTLSCertificateSelector` and `frontendTLSServerName` when call-site scans prove they are unused.

- [ ] **Step 4: Run GREEN and Store-absence tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestFrontendTLSSelector|TestFrontendTLSConfig|TestEnforceClientCertificateDepth)" -count=1'
! rg -n '\bstore\.(GetSSLCertificateForSNI|GetSSLCertificateConfigForSNI)\(' pkg/server/tls.go
```

Expected: PASS and no Store matches.

---

### Task 6: Make Stream Runtime a Listener and Connection-Lease Owner

**Files:**
- Modify: `pkg/stream/runtime.go`
- Modify: `pkg/stream/runtime_test.go`
- Modify: `pkg/stream/router.go`
- Modify: `pkg/stream/router_test.go`

**Interfaces:**
- Consumes: frozen `RouterLease`, `RouterSource`, and detached `CompileRouter` output.
- Produces: `NewRuntime(context.Context, []config.TcpListen, RouterSource) (*Runtime, error)`; no `Runtime.Reload`, `Router.Reload`, `ErrFrozenRouter`, mutable route lock, or runtime route compilation.

- [ ] **Step 1: Write RED connection-isolation tests**

```go
func TestRuntimePinsRouterLeaseForConnectionLifetime(t *testing.T) {
	old := newBlockingRouterLeaseFixture(t, 71)
	next := newRouterLeaseFixture(t, 72)
	source := newSwitchableRouterSource(old)
	runtime := newTestRuntime(t, source.Acquire)
	oldConn := dialStreamRuntime(t, runtime)
	old.WaitStarted(t)
	source.Store(next)
	newConn := dialStreamRuntime(t, runtime)
	assertRouterRevision(t, newConn, 72)
	assertReleaseCount(t, old, 0)
	oldConn.Close()
	old.WaitReleased(t)
}
```

Add `TestRuntimeRejectsConnectionWhenRouterUnavailable`, `TestRuntimeReleasesLeaseWhenServeReturnsError`, `TestRuntimeTerminalCloseCancelsConnectionsAndReleasesLeases`, `TestRuntimeAcceptFailureDoesNotAcquireLease`, and `TestRuntimeSourceRollbackAffectsOnlyNewConnections`.

- [ ] **Step 2: Run RED tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/stream -run "^(TestRuntimePinsRouter|TestRuntimeRejectsConnection|TestRuntimeReleasesLease|TestRuntimeTerminalClose|TestRuntimeSourceRollback)" -count=1'
```

Expected: FAIL because Runtime owns one mutable router and exposes `Reload`.

- [ ] **Step 3: Implement listener-only Runtime**

Validate listener specifications and non-nil source at construction. After each successful `Accept`, acquire one router lease. If unavailable, close that connection and continue accepting. Each connection goroutine defers `Release` and invokes only its leased router. `Runtime.Close` cancels the terminal context, closes listeners, and waits for listener/connection goroutines; no generation retirement method touches Runtime.

```go
func (runtime *Runtime) serveConnection(listener net.Listener, conn net.Conn) {
	lease, ok := runtime.source()
	if !ok || lease.Router == nil {
		_ = conn.Close()
		return
	}
	defer lease.Release()
	_ = lease.Router.Serve(runtime.ctx, listener, conn)
}
```

`Router` becomes immutable: compile entries once in `CompileRouter`; remove the mutex, `enabledPlugins`, `frozen`, `ErrFrozenRouter`, `NewRouter`, and `Reload` after all legacy callers are removed in the same integration checkpoint. `Serve` and match functions read the fixed entries directly.

- [ ] **Step 4: Run GREEN and race tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/stream -run "^(TestRuntime|TestCompileRouter|TestRouter|TestStream)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/stream -run "^(TestRuntimePinsRouter|TestRuntimeTerminalClose|TestRuntimeSourceRollback)" -count=1'
```

Expected: PASS. The final integration revision must also satisfy:

```bash
! rg -n 'func \([^)]*\*(Runtime|Router)\) Reload\(' pkg/stream --glob '*.go'
```

---

### Task 7: Inject Cluster and Stream Observers into Every Prepared Generation

**Files:**
- Create: `pkg/compiler/runtime_observers.go`
- Create: `pkg/compiler/runtime_observers_test.go`
- Modify: `pkg/compiler/worker_factory.go`
- Modify: `pkg/compiler/worker_factory_test.go`
- Modify: `pkg/compiler/worker_factory_recovery_test.go`
- Modify: `pkg/compiler/prepared_generation.go`
- Modify: `pkg/compiler/http_cluster.go`
- Modify: `pkg/compiler/http_cluster_test.go`
- Modify: `pkg/compiler/stream_compile.go`
- Modify: `pkg/compiler/stream_compile_test.go`

**Interfaces:**
- Consumes: `proxy.ClusterObserver`, optional `proxy.UpstreamStatusObserver`, `stream.Result`, `runtime.ResourceRegistry`, and `stream.CompileInput.OnResult`.
- Produces: required `WorkerRuntimeObservers`, the four-argument `NewWorkerCompilerFactory`, and an internal same-name cluster observer lifetime registry shared by every generation from one factory.

- [ ] **Step 1: Write RED constructor and stream callback tests**

```go
func TestWorkerCompilerFactoryRequiresRuntimeObservers(t *testing.T) {
	_, err := NewWorkerCompilerFactory(manifest(t), effective(t), materializer(t), WorkerRuntimeObservers{})
	if !errors.Is(err, ErrInvalidInput) { t.Fatalf("error = %v", err) }
}

func TestPreparedStreamRouterUsesFactoryObserver(t *testing.T) {
	results := make(chan stream.Result, 1)
	factory := newWorkerFactory(t, WorkerRuntimeObservers{
		Cluster: proxy.NopClusterObserver{},
		Stream: func(result stream.Result) { results <- result },
	})
	prepared := prepareStreamGeneration(t, factory)
	servePreparedStreamOnce(t, prepared)
	select {
	case <-results:
	case <-time.After(time.Second): t.Fatal("stream observer was not called")
	}
}
```

Also test that a nil stream callback is accepted when stream proxy mode is disabled and rejected when enabled. Update every constructor call found by `rg -n 'NewWorkerCompilerFactory\(' pkg --glob '*.go'` with an explicit observer value.

- [ ] **Step 2: Write RED same-name N/N+1 overlap tests**

```go
func TestClusterObserverDeletesMetricsOnlyAfterFinalSameNameLease(t *testing.T) {
	observer := newRecordingClusterObserver()
	factory := newWorkerFactory(t, WorkerRuntimeObservers{Cluster: observer, Stream: func(stream.Result) {}})
	old := prepareHTTPGenerationWithCluster(t, factory, "orders", []string{"10.0.0.1:80"})
	next := prepareHTTPGenerationWithCluster(t, factory, "orders", []string{"10.0.0.2:80"})
	closePrepared(t, old)
	observer.assertClusterNotDeleted(t, "orders")
	observer.assertTargetDeleted(t, "orders", "10.0.0.1:80")
	closePrepared(t, next)
	observer.assertClusterDeletedOnce(t, "orders")
	observer.assertTargetDeleted(t, "orders", "10.0.0.2:80")
}
```

Add an exact-config ResourceRegistry reuse case: two generations sharing the same resource digest must produce one observer lifetime acquisition and only the final resource release deletes metrics.

- [ ] **Step 3: Run RED tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^(TestWorkerCompilerFactoryRequiresRuntimeObservers|TestPreparedStreamRouterUsesFactoryObserver|TestClusterObserverDeletesMetricsOnlyAfterFinalSameNameLease)" -count=1'
```

Expected: FAIL because the constructor has three arguments, clusters use `proxy.NopClusterObserver{}`, and stream compilation omits `OnResult`.

- [ ] **Step 4: Implement observer validation and immutable capture**

Clone the struct into the factory. Reject a nil or typed-nil cluster observer. Determine stream enablement from the owned effective configuration and reject a nil stream callback only when stream mode is enabled. Copy the observer set into each `PreparedGeneration`; never mutate it during activation.

```go
type WorkerRuntimeObservers struct {
	Cluster proxy.ClusterObserver
	Stream  func(stream.Result)
}
```

In `compileAndAttachStream`, set `OnResult: prepared.observers.Stream` in `stream.CompileInput` before `CompileRouter` freezes the router.

- [ ] **Step 5: Implement generation-safe cluster metric lifetime**

A direct observer pass-through is insufficient: N and N+1 may have different cluster resources with the same logical name, and closing N must not call `DeleteCluster("orders")` while N+1 is live. Implement a factory-owned overlap registry that acquires a per-resource observer wrapper inside the `runtime.ResourceRegistry` creation callback. The wrapper forwards request/health signals synchronously, suppresses resource-local delete calls, and its resource cleanup decrements cluster-name and target reference counts. Only the final same-name resource release forwards `DeleteCluster`; only the final target reference forwards `DeleteUpstreamStatus` when the sink implements `proxy.UpstreamStatusObserver`.

The resource creator and cleanup order is:

```go
observerLease := prepared.clusterObservers.acquire(owned.Name, slices.Collect(maps.Keys(owned.Targets)))
cluster, err := proxy.NewCluster(owned, observerLease.Observer())
if err != nil {
	observerLease.Release()
	return nil, nil, err
}
return cluster, func(context.Context) error {
	cluster.Close()
	observerLease.Release()
	return nil
}, nil
```

The overlap registry mutex protects only bookkeeping; it snapshots required sink calls, unlocks, then invokes the synchronous observer to avoid lock inversion with health goroutines.

- [ ] **Step 6: Run GREEN and race tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^(TestWorkerCompilerFactory|TestPreparedStream|TestClusterObserver|TestHTTPCluster)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler -run "^(TestPreparedStreamRouterUsesFactoryObserver|TestClusterObserverDeletesMetricsOnlyAfterFinalSameNameLease|TestHTTPCluster)" -count=1'
! rg -n 'proxy\.NewCluster\([^\n]*proxy\.NopClusterObserver' pkg/compiler --glob '*.go'
```

Expected: tests PASS and the absence command finds no compiler-owned no-op cluster construction. Return the observer diff and constructor call-site inventory; server production observer construction remains the bootstrap lane's responsibility.

---

### Task 8: Integrate Engine Sources Without a Dual Production Path

**Files:**
- Consume only; sibling engine lane modifies `pkg/server/generation_engine.go`, `generation_engine_test.go`
- Consume only; sibling bootstrap lane modifies `pkg/server/server.go`, `cmd/root.go`, and focused tests
- Consume only; sibling legacy lane deletes `pkg/server/reload.go`, `pkg/route/builder.go`, `pkg/proxy/registry.go` and Store TLS/stream loaders

**Interfaces:**
- Consumes: all contracts above and the engine's atomic bundle pointer.
- Produces: one final caller graph: HTTP/TLS/stream sources all read the same engine bundle; no Event/Builder/Store/Reload selection remains.

- [ ] **Step 1: Add RED engine protocol-isolation tests before caller replacement**

```go
func TestGenerationEngineTentativeActivationRollbackDoesNotDrainPredecessorWithoutLeases(t *testing.T) {
	old := preparedGeneration(t, 79, withHTTPStatus(200))
	next := preparedGeneration(t, 80, withHTTPStatus(201))
	engine := newGenerationEngineWithActive(t, old)
	token, set := prepareActivation(t, engine, next)

	activate(t, engine, token, set)
	assertOwnerNotDrained(t, engine.ownerFor(old))
	assertPreparedOpen(t, old)

	rollbackActivation(t, engine, token, set)
	assertActivePrepared(t, engine, old)
	assertOwnerNotDrained(t, engine.ownerFor(old))
	assertPreparedOpen(t, old)
	assertOwnerDrained(t, engine.ownerFor(next))
}

func TestGenerationEngineTLSHTTPAndStreamReadSameCompositeBundle(t *testing.T) {
	old := preparedGeneration(t, 80, withHTTPStatus(200), withTLSName("old.example"), withStreamRoute("old"))
	next := preparedGeneration(t, 81, withHTTPStatus(201), withTLSName("new.example"), withStreamRoute("new"))
	engine := newGenerationEngineWithActive(t, old)
	oldRequest := acquireBlockedHTTPRequest(t, engine)
	oldStream := acquireBlockedStreamConnection(t, engine)
	activateAndFinalize(t, engine, next)
	assertHTTPStatusFromEngine(t, engine, 201)
	assertTLSNameFromEngine(t, engine, "new.example")
	assertStreamRouteFromEngine(t, engine, "new")
	oldRequest.AssertStatus(t, 200)
	oldStream.AssertRoute(t, "old")
}
```

Add HTTP-only activation preserving the exact stream owner pointer, stream-only activation preserving the HTTP/TLS owner, tentative activation leases surviving rollback, durable finalize deactivating only replaced predecessor slots, unavailable-domain fail-closed, and natural hijack/stream drain tests. The zero-lease predecessor test above is mandatory: it proves tentative activation cannot irreversibly close the predecessor's one-way `drained` channel before rollback.

- [ ] **Step 2: Run RED engine tests**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestGenerationEngineTLSHTTPAndStream|TestGenerationEngineHTTPOnly|TestGenerationEngineStreamOnly|TestGenerationEngineTentative|TestGenerationEngineHijacked)" -count=1'
```

Expected: FAIL because the engine does not yet implement the bundle and source contracts.

- [ ] **Step 3: Hand the fixed calls to the engine lane**

The engine owns the atomic pointer and implements:

```go
func (engine *GenerationEngine) acquireHTTP() (httpGenerationLease, bool)
func (engine *GenerationEngine) acquireStream() (streamGenerationLease, bool)
```

Each method loads the current bundle, tries the requested owner's domain acquisition, and retries only when the bundle pointer changed during a failed acquisition. A stable unavailable slot returns false. `Activate` calls `candidate.activateDomains(replaced)`, atomically stores the candidate bundle, and deliberately leaves predecessor domains active. `RollbackActivation` atomically restores the predecessor bundle and then calls `candidate.deactivateDomains(replaced)`; it never calls `predecessor.activateDomains` because those domains were never deactivated. `FinalizeActivation`, and only finalize after durable commit, calls `predecessor.deactivateDomains(replaced)` and enqueues the owner exactly once when it has no remaining active domain, even if its leases have not drained. The retirement loop waits for `drained`; finalize never closes or waits inline.

- [ ] **Step 4: Perform one serialized production caller swap**

After Tasks 2-7 and engine tests are green on one integration base, the integration owner applies these changes as one reviewed checkpoint:

1. Bootstrap constructs `WorkerRuntimeObservers` with the generation-safe production metrics sink and `logStreamResult`, then calls the four-argument factory constructor.
2. Server constructs route handler, TLS listener config, and stream Runtime from `engine.acquireHTTP`/`engine.acquireStream`.
3. Server starts listeners only after recovery installation.
4. Legacy lane removes `routeHandler.Replace`, Store TLS callbacks, stream Runtime/Router Reload, Builder event reload, and their production callers in the same checkpoint.
5. No package-private transitional constructor, legacy source closure, selection flag, or forwarding method survives the checkpoint.

- [ ] **Step 5: Verify natural retirement and terminal shutdown separately**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server ./pkg/stream -run "(GenerationEngine|RouteHandler|Hijack|TLS|Runtime|Stream|Shutdown)" -count=1'
```

Expected: PASS. Assertions must distinguish:

- ordinary finalize/retirement: no hijack close, no stream connection cancellation, no listener close, prepared owner remains open until natural lease release;
- terminal shutdown: new source acquisitions fail, HTTP and stream listeners stop, tracked hijacks and stream contexts close/cancel according to server policy, leases release, then engine closes prepared generations and factory exactly once.

---

### Task 9: Run Runtime Absence, Dead-Code, and Build Gates

**Files:**
- Verify only: final combined Task 9 integration diff

**Interfaces:**
- Consumes: merged Tasks 1-8 plus engine/bootstrap/legacy lanes.
- Produces: evidence that the runtime cutover is isolated, race-safe, buildable, and contains no legacy owner.

- [ ] **Step 1: Run exact focused correctness gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler ./pkg/route ./pkg/stream ./pkg/server -count=1'
```

Expected: PASS.

- [ ] **Step 2: Run exact runtime race gate**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler ./pkg/route ./pkg/stream ./pkg/server -run "(WorkerCompilerFactory|PreparedGeneration|ClusterObserver|RouteHandler|Hijack|TLS|Runtime|Router|GenerationEngine|Shutdown|Isolation)" -count=1'
```

Expected: PASS.

- [ ] **Step 3: Run direct absence gates**

```bash
! rg -n '\bstore\.(GetStore|Get[A-Z][A-Za-z0-9_]*|List[A-Z][A-Za-z0-9_]*|MaterializeSecret|ResolveSecretReference)\(' pkg/compiler pkg/route pkg/plugin pkg/server pkg/stream --glob '*.go' --glob '!*_test.go'
! rg -n 'type Builder|NewBuilder|ClusterRegistry' cmd pkg --glob '*.go'
! rg -n 'func \([^)]*\*(routeHandler|Runtime|Router)\) (Replace|Reload)\(|\b(routes|streamRuntime|router)\.(Replace|Reload)\(' pkg/server pkg/stream --glob '*.go'
test ! -e pkg/route/builder.go
test ! -e pkg/proxy/registry.go
```

Expected: every command exits zero with no matches.

- [ ] **Step 4: Run AST guard, build, lint, and diff checks**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^TestProductionRuntimeHasNoGlobalStoreReads$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make lint'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
git diff --check master...HEAD
```

Expected: PASS. Report any pre-existing failure by exact command, package, file, line, and message; never report a skipped or failing gate as passing.

- [ ] **Step 5: Audit moved/deleted symbols and lane boundaries**

List every moved/deleted function, method, type, import, and file from the final diff. For each, run `rg` across production and tests. Remove proxy-only or test-only facades unless a documented compatibility boundary requires them. Confirm the final diff contains no edits outside Task 9 ownership and that all product callers use the fixed contracts in this plan.

- [ ] **Step 6: Return the accepted diff for independent review**

Return the diff, command evidence, moved/deleted-symbol inventory, and any remaining uncertainty to the Task 9 integration owner. The owner requests the authorized independent read-only merge review and performs any commit or local fast-forward; implementation workers do not commit.

---

## Dependency and Parallelism Summary

```text
Task 1 route helper extraction ---------------------------┐
Task 7 observer injection --------------------------------+-- parallel Wave A
Task 2 bundle/owner contract -----------------------------┘
                        |
             ┌----------+----------┐
             |                     |
Tasks 3-5 HTTP/TLS/hijack     Task 6 stream source       parallel Wave B
             └----------+----------┘
                        |
         sibling GenerationEngine lane                   serial owner
                        |
      bootstrap + provider wiring may run in parallel    fixed signatures
                        |
     one serialized caller swap + legacy deletion        no dual path
                        |
              Task 9 gates and review
```

Files within one row of the ownership map are exclusive. Tasks 3-5 are one HTTP/TLS lane because they all modify `route_handler.go`; do not assign them to simultaneous workers. Task 6 is independent after Task 2. Task 7 is independent of protocol files. The engine lane begins only after Task 2 is merged. Bootstrap may not start until engine source signatures are merged. Destructive Reload/Builder/Store deletion is the final serialized checkpoint after all replacement callers pass focused tests.

## Handoff Acceptance Checklist

- Composite bundle preserves independently active HTTP and stream revisions and restores the complete predecessor on rollback.
- Request, batch, hijack, TLS, and stream acquisitions fail closed and release exactly once.
- Batch and hijack retain the exact request owner; they never reacquire current.
- Ordinary retirement drains naturally; terminal shutdown alone force-closes/cancels connections and listeners.
- TLS reads only immutable HTTP snapshots; stream Runtime compiles or reloads no routes.
- Worker factory requires explicit observers; same-name cluster overlap cannot delete N+1 metrics when N retires; stream result callbacks are captured before router freeze.
- Pure route helpers move before Builder deletion with no wrappers and no behavior change.
- Engine/bootstrap/legacy lanes consume the fixed interfaces without editing their owners.
- Focused tests, race tests, lint, build, AST guard, direct absence guards, dead-code scan, and diff check all pass or are reported precisely.
