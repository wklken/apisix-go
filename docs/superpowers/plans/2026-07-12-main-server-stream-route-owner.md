# Main Server Stream-Route Owner Implementation Plan

> **For agentic workers:** Execute this plan task-by-task in the current worktree. The repository already contains unrelated uncommitted parity work; preserve it and do not reset or reformat unrelated files.

**Goal:** Bind APISIX 3.17 TCP `stream_routes` to the Go server lifecycle so a configured MQTT stream route can match the listener/remote address, select an upstream, run the plugin-owned stream boundary, and shut down cleanly.

**Architecture:** Add the APISIX stream-route resource and store parser first. Add a package-local `pkg/stream` runtime that owns listeners, immutable route snapshots, route matching, weighted upstream selection, raw bidirectional forwarding, and result callbacks. The runtime initializes the existing `mqtt-proxy` plugin through the same schema/parse/PostInit path used by HTTP routes, while unsupported stream plugins fail explicitly. Wire the runtime into `pkg/server` only when `apisix.proxy_mode` includes `stream` and `apisix.stream_proxy.tcp` is configured.

**Tech Stack:** Go 1.26, `net`, `context`, bbolt-backed `pkg/store`, existing `pkg/proxy.LoadBalancer`, existing `pkg/plugin/mqtt_proxy.Plugin`, Viper config, and focused `go test` fixtures using `net.Pipe`/TCP listeners.

## Global Constraints

- Preserve existing uncommitted changes and touch only files required for this stream-route slice.
- Match APISIX 3.17 stream-route fields: `server_addr`, `server_port`, `remote_addr`, `upstream_id`, `upstream`, and `plugins`.
- Do not add HTTP middleware assumptions to the stream path.
- Do not invent TLS, UDP, SNI, health checks, or Kafka stream semantics in this slice; reject unsupported configuration explicitly.
- Every new behavior gets a failing focused test before production code.
- Use existing dependencies and format touched Go files with the repository's `golines`/`gofumpt` commands.
- Run focused tests, `direnv exec . go test ./... -count=1`, `direnv exec . make build`, remove `apisix`, and run `git diff --check` before calling the slice complete.

---

### Task 1: Add the stream-route resource and store boundary

**Files:**
- Modify: `pkg/resource/route.go`
- Modify: `pkg/store/getter.go`
- Modify: `pkg/store/store.go`
- Modify: `pkg/config/standalone.go`
- Create: `pkg/store/stream_route_test.go`

**Interfaces:**
- `resource.StreamRoute` owns `ID`, `ServerAddr`, `ServerPort`, `RemoteAddr`, `Plugins`, `UpstreamID`, and inline `Upstream`.
- `store.ParseStreamRoute([]byte) (resource.StreamRoute, error)` parses one etcd value and decrypts plugin configs using the existing store helper.
- `store.GetStreamRoute(id string) (resource.StreamRoute, error)` and `store.ListStreamRoutes() ([]resource.StreamRoute, error)` read the existing `stream_routes` bucket.

- [x] **Step 1: Write the failing parser/list tests**

Add tests that parse this APISIX-shaped value and assert every routing field survives:

```go
func TestParseStreamRoutePreservesMatchUpstreamAndPlugins(t *testing.T) {
	route, err := ParseStreamRoute([]byte(`{
		"id":"mqtt",
		"server_addr":"127.0.0.1",
		"server_port":1883,
		"remote_addr":"192.0.2.0/24",
		"plugins":{"mqtt-proxy":{"protocol_level":4}},
		"upstream":{"type":"roundrobin","scheme":"tcp","nodes":{"127.0.0.1:2883":1}}
	}`))
	if err != nil {
		t.Fatalf("ParseStreamRoute() error = %v", err)
	}
	if route.ID != "mqtt" || route.ServerPort != 1883 || route.ServerAddr != "127.0.0.1" || route.RemoteAddr != "192.0.2.0/24" {
		t.Fatalf("route match fields = %#v", route)
	}
	if _, ok := route.Plugins["mqtt-proxy"]; !ok {
		t.Fatal("mqtt-proxy config was not preserved")
	}
	if len(route.Upstream.Nodes) != 1 || route.Upstream.Nodes[0].Host != "127.0.0.1" {
		t.Fatalf("upstream = %#v", route.Upstream)
	}
}
```

Add a missing-bucket `GetStreamRoute` test only through the existing store fixture pattern; do not add a new global-store reset mechanism in this slice.

- [x] **Step 2: Run the focused tests and verify the expected compile failure**

Run:

```bash
direnv exec . go test ./pkg/store -run 'TestParseStreamRoutePreservesMatchUpstreamAndPlugins|TestGetStreamRoute' -count=1
```

Expected: compile failure because `resource.StreamRoute` and the stream getters do not exist.

- [x] **Step 3: Implement the smallest resource/store boundary**

Add the resource type beside `Route`, use the existing `Plugins map[string]PluginConfig` shape, add the three getter/parser functions beside the existing route functions, and include `stream_routes` in the same event-update bucket set as routes/services/upstreams so runtime snapshots can reload after etcd changes.

Expose the standalone model without enabling a separate standalone loader yet:

```go
type ApisixConfigurationStandalone struct {
	Routes       []*Route        `json:"routes,omitempty" yaml:"routes,omitempty"`
	StreamRoutes []*StreamRoute  `json:"stream_routes,omitempty" yaml:"stream_routes,omitempty"`
	// existing fields remain unchanged
}
```

`config.StreamRoute` should reuse the same JSON/YAML fields as `resource.StreamRoute` only if doing so does not create an import cycle; otherwise define the minimal standalone mirror with `server_addr`, `server_port`, `remote_addr`, `plugins`, `upstream_id`, and `upstream`.

- [x] **Step 4: Run the focused tests and inspect the diff**

Run:

```bash
direnv exec . go test ./pkg/store -run 'TestParseStreamRoutePreservesMatchUpstreamAndPlugins|TestGetStreamRoute' -count=1
git diff --check
```

Expected: parser and getter tests pass, with no unrelated store/resource diff.

### Task 2: Implement route matching and upstream dialing

**Files:**
- Create: `pkg/stream/router.go`
- Create: `pkg/stream/router_test.go`

**Interfaces:**
- `type Result struct { RouteID, Listener, Remote, ClientID, Protocol string; Err error }` is emitted after each connection.
- `type Router struct { ... }` is constructed by `NewRouter(routes []resource.StreamRoute, enabledPlugins []string, onResult func(Result)) (*Router, error)`.
- `func (r *Router) Serve(ctx context.Context, listener net.Listener, client net.Conn) error` matches one route and owns one connection.
- `func (r *Router) Reload(routes []resource.StreamRoute) error` builds a complete replacement snapshot before publishing it.

- [x] **Step 1: Write failing route-match and raw-forward tests**

Use a TCP listener and a fake upstream listener. Cover:

1. A route with matching `server_port` and `remote_addr` forwards bytes to the upstream.
2. A route with a non-matching `server_port` closes the client without dialing.
3. An empty `remote_addr` matches any peer; exact IP and CIDR values match the client address.
4. Missing upstream nodes and unsupported `tls` scheme return explicit errors.
5. `Result` contains route ID, listener address, remote address, and the terminal error.

- [x] **Step 2: Run the focused tests and verify the expected failure**

Run:

```bash
direnv exec . go test ./pkg/stream -run 'TestRouter' -count=1
```

Expected: compile failure because the `pkg/stream` package does not exist.

- [x] **Step 3: Implement immutable route snapshots and matching**

Build each route's upstream target list as `scheme://host:port` (use `tcp://` for an empty scheme), select with `proxy.NewWeightedRRLoadBalance`, and publish the complete slice under a mutex only after all routes validate. Match `server_port` against the listener port, allow blank `server_addr`, and compare `remote_addr` as either an exact IP or CIDR after stripping the peer port.

Implement a bounded `dialUpstream` using `net.Dialer.DialContext` with the route's connect timeout when present, and reject non-`tcp`/empty schemes. Implement raw forwarding with two `io.Copy` goroutines, close both sockets on first completion or context cancellation, and normalize ordinary EOF/closed-connection errors to nil.

- [x] **Step 4: Run route tests and race tests**

Run:

```bash
direnv exec . go test ./pkg/stream -run 'TestRouter' -count=1
direnv exec . go test -race ./pkg/stream -count=1
```

Expected: all router tests pass and no race is reported.

### Task 3: Bind the MQTT stream plugin and stream metadata

**Files:**
- Modify: `pkg/stream/router.go`
- Modify: `pkg/stream/router_test.go`
- Modify: `pkg/plugin/mqtt_proxy/stream.go` only if the existing callback boundary needs a narrowly scoped adapter.

**Interfaces:**
- A stream route with `plugins["mqtt-proxy"]` is initialized with `plugin.New("mqtt-proxy")`, `Init`, `util.Validate`, `util.Parse`, and `PostInit`.
- The concrete plugin is type-asserted to the existing `ServeStream`/`ServeListener` boundary; no HTTP `Handler` is called.
- The router's `Result` callback receives MQTT `ClientID` and protocol level from `mqtt_proxy.StreamInfo`.

- [x] **Step 1: Write the failing MQTT route integration test**

Add a route with `mqtt-proxy: {protocol_level: 4}` and a fake TCP upstream. Send a valid CONNECT plus payload, assert the upstream sees the exact bytes, assert the fake upstream response reaches the client, and assert the callback records the route ID and `ClientID`.

Also add a rejection test for an unknown stream plugin and a malformed MQTT CONNECT; neither may dial the upstream.

- [x] **Step 2: Run the focused test and verify it fails**

Run:

```bash
direnv exec . go test ./pkg/stream -run 'TestRouterMQTT|TestRouterRejects' -count=1
```

Expected: the integration test fails because the router currently only has raw forwarding and no plugin initialization.

- [x] **Step 3: Implement plugin initialization and metadata mapping**

Use the existing `util.Validate`/`util.Parse` helpers. If the route's plugin map is empty, use raw forwarding. If it contains `mqtt-proxy`, invoke `ServeStream` with the route's upstream dialer and translate the returned `StreamInfo` into `Result`. Reject every other configured stream plugin with a named error instead of silently ignoring it.

- [x] **Step 4: Run focused MQTT tests and the package race test**

Run:

```bash
direnv exec . go test ./pkg/stream -run 'TestRouterMQTT|TestRouterRejects' -count=1
direnv exec . go test -race ./pkg/stream ./pkg/plugin/mqtt_proxy -count=1
```

Expected: exact replay, metadata, malformed rejection, and cancellation all pass.

### Task 4: Own listeners and wire the server lifecycle

**Files:**
- Create: `pkg/stream/runtime.go`
- Create: `pkg/stream/runtime_test.go`
- Modify: `pkg/server/server.go`
- Modify: `pkg/server/route_handler_test.go` or create `pkg/server/stream_test.go`

**Interfaces:**
- `func NewRuntime(ctx context.Context, specs []config.TcpListen, routes []resource.StreamRoute, enabledPlugins []string, onResult func(Result)) (*Runtime, error)` opens configured TCP listeners and publishes the initial route snapshot.
- `func (r *Runtime) Reload(routes []resource.StreamRoute) error` atomically replaces snapshots for new connections.
- `func (r *Runtime) Close(ctx context.Context) error` cancels active streams, closes listeners, and waits for accept/connection goroutines.

- [x] **Step 1: Write failing runtime lifecycle tests**

Cover one configured `127.0.0.1:0` listener, a route to a fake upstream, a client round trip, cancellation closing the accepted connection, and `Reload` changing the upstream for the next connection. Add invalid TLS/listener address rejection.

- [x] **Step 2: Run focused tests and verify failure**

Run:

```bash
direnv exec . go test ./pkg/stream -run 'TestRuntime' -count=1
```

Expected: compile failure because `Runtime` does not exist.

- [x] **Step 3: Implement listener ownership**

Open one listener per `config.TcpListen`, reject `Tls: true` with an explicit unsupported error, create a child context for the runtime, accept connections in goroutines, call the router, emit a `Result`, and make `Close` idempotent. Route snapshots affect new connections while active connections retain their own cancellation context.

- [x] **Step 4: Integrate `Server.Start` and `Server.shutdown`**

Add a `streamRuntime *stream.Runtime` field. During `Start`, only create it when `config.GlobalConfig.Apisix.ProxyMode` is `stream` or `http&stream` and TCP specs are configured; load `store.ListStreamRoutes()` after the initial store fetch. On store route/upstream events, call `Reload` after validating the complete snapshot. During shutdown, close the stream runtime within the supplied shutdown context before returning.

- [x] **Step 5: Run server/runtime tests**

Run:

```bash
direnv exec . go test ./pkg/stream ./pkg/server -count=1
direnv exec . go test -race ./pkg/stream ./pkg/server -count=1
```

Expected: runtime lifecycle and server shutdown tests pass.

### Task 5: Sync documentation and complete verification

**Files:**
- Modify: `docs/apisix-3.17-plugin-parity-execution-todo.md`
- Modify: `docs/apisix-3.17-remaining-plugin-todo.md`
- Modify: `docs/apisix-3.17-protocol-bridge-design.md`
- Modify: `README.md`
- Modify: `docs/apisix-3.17-plugin-parity-checklist.md`

- [x] **Step 1: Update the implementation status**

Mark only the MQTT main-server stream-route and `StreamInfo` runtime-context items complete. Keep Kafka owner binding, SASL handshake, health abstraction, and gRPC streaming unchecked. Record that TLS/UDP/SNI and non-MQTT stream plugins remain deferred.

- [x] **Step 2: Run the complete verification gate**

```bash
direnv exec . go test ./... -count=1
direnv exec . make build
rm -f apisix
test ! -e apisix
git diff --check
```

Expected: exit code 0, all packages compile/test, generated binary is absent, and no whitespace errors are reported.

- [x] **Step 3: Review changed symbols and callers**

Run:

```bash
rg -n "StreamRoute|ListStreamRoutes|NewRuntime|ServeListener|streamRuntime" pkg docs
git diff --stat
git diff -- docs/apisix-3.17-plugin-parity-execution-todo.md docs/apisix-3.17-remaining-plugin-todo.md docs/apisix-3.17-protocol-bridge-design.md README.md
```

Confirm every changed line belongs to stream-route ownership or its documentation, and do not claim the overall APISIX parity goal complete while the remaining queue is still open.
