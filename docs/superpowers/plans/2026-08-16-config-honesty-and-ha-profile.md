# Configuration Honesty and HA Production Profile Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task in an isolated worktree. This plan does not authorize subagents.

**Goal:** Introduce one explicit `http-data-plane-v1` production profile, reject configuration that the Go data plane does not implement, enforce WebSocket semantics, and make SIGHUP/telemetry behavior honest enough to qualify the profile across multiple replicas.

**Architecture:** Add `deployment.profile` as an opt-in strict contract. General config loading rejects unsupported feature activation rather than silently ignoring it. The profile validator layers stronger topology requirements on top: HTTP-only, data-plane etcd, verified etcd TLS, exact stateless allowlist, body limit, no Admin/discovery/native/runtime features, and no process-level access-log claims. Dynamic resources retain compatibility fields only when their behavior is implemented; discovery upstreams fail route compilation and WebSocket upgrades are gated by the effective route/service flag.

**Tech Stack:** Go 1.26, Viper/mapstructure, strict route builder, `net/http` WebSocket upgrade semantics, existing readiness/etcd lifecycle.

## Frozen contracts

- Supported profile values are empty (compatibility mode) and `http-data-plane-v1`. Any other non-empty value fails startup.
- `http-data-plane-v1` is the only profile eligible for a production-ready claim. It is multi-replica safe because every enabled plugin is stateless and all dynamic configuration comes from shared etcd.
- The profile's exact plugin list is `request-id`, `cors`, `key-auth`, `jwt-auth`, `basic-auth`, `prometheus`; no extra plugin is accepted.
- The profile requires `debug:false`, `apisix.enable_admin:false`, `proxy_mode:http`, no stream listeners/plugins, verified etcd TLS, at least one trusted proxy CIDR, and `client_max_body_size > 0`.
- Local limiters, local/OIDC/CAS/DingTalk/Feishu sessions, proxy-cache, MCP, SLS, discovery, Admin, WASM, XRPC, QUIC, HTTP/3, Lua/serverless, ext-plugin, inspect, and GM are rejected by the profile rather than called HA-capable.
- A WebSocket upgrade is accepted only when the effective route or service sets `enable_websocket:true`; ordinary HTTP requests are unaffected.
- `SIGHUP` is not reload. Until whole-process config reload exists, it gracefully shuts down and returns a non-nil unsupported-reload error so supervisors cannot record it as a successful reload.

### Task 1: Add the explicit production profile and real-file gate

**Files:**
- Modify: `pkg/config/types.go`
- Modify: `pkg/config/init.go`
- Modify: `pkg/config/init_test.go`
- Modify: `pkg/config/release_gate_test.go`
- Modify: `conf/config-production.yaml`
- Modify: `docs/configuration.md`

- [ ] **Step 1: Add the complete profile rejection matrix**

Start from a valid config fixture and mutate one field per row: unknown profile, debug true, wrong role/provider, unverified etcd, zero body limit, Admin enabled, extra/missing/reordered plugin, stream mode/listener/plugin, no trusted address, discovery config, ext-plugin command, WASM plugin, XRPC protocol, QUIC, HTTP/3, and non-empty unsupported process access-log settings.

Expected errors name the exact field and `http-data-plane-v1`; do not dump the whole config.

- [ ] **Step 2: Run focused red tests**

```bash
bash -lc 'source .envrc && go test ./pkg/config -run "(ProductionProfile|ProductionConfig|UnsupportedRuntimeConfig)" -count=1'
```

Expected: compile failure for `Deployment.Profile` and acceptance of several unsupported rows.

- [ ] **Step 3: Add the profile field and validator**

```go
type Deployment struct {
	Profile string `mapstructure:"profile"`
	// existing fields
}

const HTTPDataPlaneV1Profile = "http-data-plane-v1"
```

After generic runtime validation, dispatch to `validateHTTPDataPlaneV1(cfg)` only for the exact profile. Compare the plugin slice to the exact canonical order with `slices.Equal`; a set-equivalent but reordered list fails so the checked artifact is deterministic.

Add to `conf/config-production.yaml`:

```yaml
apisix:
  enable_admin: false

nginx_config:
  http:
    client_max_body_size: 10485760

deployment:
  profile: http-data-plane-v1
```

The 10 MiB value is the v1 profile default and is operator-overridable with a positive value.

### Task 2: Reject unsupported top-level feature activation

**Files:**
- Modify: `pkg/config/init.go`
- Modify: `pkg/config/init_test.go`
- Modify: `pkg/config/release_gate_test.go`

- [ ] **Step 1: Add generic fail-closed validation**

Outside the production profile, reject only explicit activation that currently lies: `apisix.enable_admin:true`, non-empty `discovery`, non-empty `ext-plugin.cmd`, non-empty `wasm.plugins`, non-empty `xrpc.protocols`, and any SSL listener with `enable_quic` or `enable_http3`. Leave inert Admin detail defaults readable when Admin itself is false.

Use one helper per bounded section; do not serialize/reflect the whole config:

```go
if cfg.Apisix.EnableAdmin {
	return errors.New("apisix.enable_admin is unsupported by the Go data plane")
}
if len(cfg.Discovery) != 0 {
	return errors.New("discovery providers are unsupported by the Go data plane")
}
```

- [ ] **Step 2: Update default compatibility config**

Because `conf/config-default.yaml` currently says `enable_admin: true` while no Admin listener exists, change it to false and update the comment to state that enabling it fails startup. Add a real-file load regression so the default cannot drift back.

### Task 3: Make upstream discovery and WebSocket resource fields truthful

**Files:**
- Modify: `pkg/resource/route.go`
- Modify: `pkg/resource/route_test.go`
- Modify: `pkg/route/builder.go`
- Modify: `pkg/route/builder_lifecycle_test.go`
- Create: `pkg/route/websocket_contract_test.go`

- [ ] **Step 1: Preserve and reject discovery fields**

Add `DiscoveryType` and `ServiceName` to `resource.Upstream` and its custom `UnmarshalJSON` field table so they cannot disappear during decode:

```go
DiscoveryType string `json:"discovery_type,omitempty"`
ServiceName   string `json:"service_name,omitempty"`
```

In the common HTTP/stream upstream validation path, reject either non-empty field with an error naming the upstream/route provenance. Static `nodes` do not make the configuration acceptable; that is the silent-degradation case this test prevents.

- [ ] **Step 2: Decode route-level WebSocket intent**

Add `EnableWebsocket bool` to `resource.Route` (the service field already exists). Add route/service tests proving both fields survive standalone and etcd JSON decode.

- [ ] **Step 3: Gate upgrades at the effective route boundary**

Wrap the route terminal before installing the response plan:

```go
handler = requireWebsocketEnablement(handler, r.EnableWebsocket || service.EnableWebsocket)
```

The helper detects a WebSocket request only when `Connection` contains token `upgrade` and `Upgrade` equals `websocket`, case-insensitively. If disabled, return a stable APISIX-owned 400 JSON error before dialing upstream. If enabled, preserve the existing ReverseProxy hijack path. Add a real upstream WebSocket handshake test for enabled and disabled rows.

### Task 4: Fail closed on unused telemetry fields and SIGHUP

**Files:**
- Modify: `pkg/plugin/zipkin/plugin.go`
- Modify: `pkg/plugin/zipkin/plugin_test.go`
- Modify: `pkg/plugin/otel/provider.go`
- Modify: `pkg/plugin/otel/plugin_test.go`
- Modify: `cmd/root.go`
- Modify: `cmd/root_test.go`

- [ ] **Step 1: Reject unsupported Zipkin/OTel claims**

Zipkin emits v2 JSON only: accept omitted/`span_version:2`, reject `span_version:1`. Existing `service_name` and `server_addr` stay supported. OTel must reject `metadata.set_ngx_var:true` and any non-zero `batch_span_processor.inactive_timeout`; remove the incorrect mapping to `sdktrace.WithExportTimeout`. Collector `request_timeout` remains the actual exporter timeout owner.

```go
if p.config.SpanVersion != 0 && p.config.SpanVersion != 2 {
	return fmt.Errorf("zipkin span_version %d is unsupported; only v2 is emitted", p.config.SpanVersion)
}
```

- [ ] **Step 2: Separate SIGHUP from normal termination signals**

Refactor `runServer` behind a testable private helper receiving a signal channel. SIGINT/SIGTERM/SIGQUIT retain clean nil shutdown. SIGHUP performs the same bounded graceful shutdown but returns `errSIGHUPReloadUnsupported`; `Start`/Cobra therefore exits non-zero.

- [ ] **Step 3: Run focused config/resource/telemetry/signal tests**

```bash
bash -lc 'source .envrc && go test ./pkg/config ./pkg/resource ./pkg/route ./pkg/plugin/zipkin ./pkg/plugin/otel ./cmd -run "(ProductionProfile|Unsupported|Discovery|Websocket|SpanVersion|InactiveTimeout|SetNgxVar|SIGHUP)" -count=1'
```

### Task 5: Freeze the HA support contract

**Files:**
- Create: `docs/production-profile.md`
- Modify: `docs/configuration.md`
- Modify: `docs/design.md`
- Modify: `docs/plugins.md`
- Modify: `README.md`

- [ ] **Step 1: Document state ownership by topology**

Add a table for config, consumer state, request auth, rate limiting, sessions, cache, upstream health, secret cache, and metrics. For v1: etcd/consumer snapshots are shared inputs; requests/auth are stateless; metrics and upstream health are intentionally per replica; all other stateful plugins are excluded. Explain that per-replica health is an observation of that replica's connections, not a cross-replica quota.

- [ ] **Step 2: Define the external TLS boundary**

State that the plain listener is valid only behind a trusted TLS-terminating ingress whose source CIDRs are configured in `trusted_addresses`. Direct internet exposure requires the implemented frontend TLS config. Do not claim the profile validates the existence of an external load balancer.

- [ ] **Step 3: Update claims conservatively**

Do not remove the project-wide not-ready warning in this PR. Add wording that `http-data-plane-v1` is a candidate profile awaiting the release/operations qualification plan. Mark Admin/discovery/native/runtime and stream metrics as explicit exclusions.

### Task 6: Verify and commit

- [ ] **Step 1: Run impact-scoped gates**

```bash
bash -lc 'source .envrc && go test ./pkg/config ./pkg/resource ./pkg/route ./pkg/plugin/zipkin ./pkg/plugin/otel ./cmd -count=1'
bash -lc 'source .envrc && go test -race ./pkg/route ./cmd -run "(Websocket|SIGHUP|Reload|Shutdown)" -count=3'
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 2: Commit the profile PR**

```bash
git add pkg/config pkg/resource/route.go pkg/resource/route_test.go \
  pkg/route/builder.go pkg/route/builder_lifecycle_test.go pkg/route/websocket_contract_test.go \
  pkg/plugin/zipkin pkg/plugin/otel cmd/root.go cmd/root_test.go conf/config-default.yaml \
  conf/config-production.yaml README.md docs/configuration.md docs/design.md docs/plugins.md \
  docs/production-profile.md docs/superpowers/plans/2026-08-16-config-honesty-and-ha-profile.md
git commit -m "feat(config): enforce the HTTP data-plane production profile"
```
