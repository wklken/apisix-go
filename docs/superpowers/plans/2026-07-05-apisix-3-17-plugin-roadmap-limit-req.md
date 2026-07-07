# APISIX 3.17 Plugin Roadmap And Limit Req Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prioritize valuable Apache APISIX 3.17 plugins for `apisix-go` and implement the first traffic-control slice with `limit-req`.

**Architecture:** Keep APISIX plugin names and priorities where they fit the current Go middleware chain. Implement feature slices as route/global-rule middleware first, and defer Nginx-only, Lua-only, stream, and complex body-filter behavior until the Go runtime has matching extension points.

**Tech Stack:** Go 1.26, `net/http` middleware, existing plugin registry in `pkg/plugin/init.go`, existing JSON schema validation through `pkg/util.Validate`.

## Global Constraints

- Match the current plugin package pattern: embed `base.BasePlugin`, expose `Config()`, set defaults in `PostInit()`, and register in `pkg/plugin/init.go`.
- Do not add dependencies for the first `limit-req` slice.
- Use upstream APISIX 3.17 branch `release/3.17` at `9ef2ecab67f652d38365049613610ef649bb4ad0` as the reference source.
- Keep unsupported APISIX config explicit in README notes.
- For code changes, run `go test ./...` and `make build`.

---

## Evidence Snapshot

- Official APISIX 3.17 has 116 top-level Lua plugin files under `apisix/plugins`.
- Official APISIX 3.17 has 114 plugin docs pages under `docs/en/latest/plugins`.
- Before this slice, `apisix-go` had 28 Go plugin packages under `pkg/plugin`, excluding `base`.
- Before this slice, README listed 76 plugins.
- Go packages not represented as README checklist entries: `otel`, `request-context`.
- Before this slice, README-listed plugins missing from Go included high-value core items: `limit-req`, `limit-conn`, `jwt-auth`, `response-rewrite`, `forward-auth`, `proxy-cache`, `traffic-split`, `proxy-mirror`, `openid-connect`, `opa`, `zipkin`, and several loggers.

## Valuable Plugin Backlog

### P0: Core Gateway Parity

These are common APISIX gateway features, are in the upstream default plugin list, and fit `apisix-go`'s current HTTP middleware/proxy model.

- `limit-req`: request-rate throttling; first implementation should support local policy, `rate`, `burst`, `key`, `key_type=var`, `rejected_code`, `rejected_msg`, `nodelay`, and `allow_degradation`.
- `limit-conn`: concurrent request limiting; local policy first.
- `response-rewrite`: status/header/body rewrite; start with status and headers, then add buffered body replacement/filtering.
- `jwt-auth`: widely used consumer auth; can reuse current consumer/store patterns after key-auth/basic-auth review.
- `forward-auth`: practical integration plugin for external auth services.
- `proxy-mirror`: useful traffic-copy behavior, implement async best-effort HTTP mirror.
- `traffic-split`: common canary/weighted upstream behavior, but it touches upstream selection and needs careful integration.

### P1: Security And Policy

- `acl`: consumer allow/deny lists; depends on consumer identity from auth plugins.
- `hmac-auth`: common APISIX auth plugin, but signature canonicalization needs exact upstream matching.
- `openid-connect`: high value but config-heavy and dependency-heavy; plan separately.
- `opa`: useful policy plugin; implement with existing HTTP client patterns.
- `data-mask`: request query/header/body masking before logs; Go middleware support is partial because APISIX runs it in the log phase.
- `oas-validator`: implemented as a request-phase OpenAPI validator with a documented useful subset; deeper `$ref` / metadata TTL parity remains future work.

### P2: Observability

- `opentelemetry`: current package is named/registered as `otel`; align upstream name or document alias behavior.
- `zipkin`: upstream default tracer; simple enough after tracing model is settled.
- `datadog`: metrics/logging integration; lower priority than OpenTelemetry parity.
- `node-status`: admin/runtime status endpoint; useful after server metrics shape is clearer.
- Missing loggers by likely value: `kafka-logger`, `clickhouse-logger`, `loki-logger`, `splunk-hec-logging`, `google-cloud-logging`, `rocketmq-logger`.

### P3: Protocol, Serverless, And Specialized Integrations

- Protocol/query bridge batch is complete for `http-dubbo`.
- Function bridge batch is complete for `serverless-*`, `aws-lambda`, `azure-functions`, `openwhisk`, and `openfunction`.
- Spec/AI plugins: remaining `ai-prompt-*`, remaining content moderation plugins.

### Skip Or Defer Unless A Runtime Need Appears

- `ext-plugin-*`: current project goal is Go plugins, not APISIX external plugin runner parity.
- `inspect`: Lua/Nginx diagnostic feature.
- `ocsp-stapling`: Nginx/TLS feature.
- `example-plugin`: upstream example only.
- `log-rotate`: runtime operational concern, not route plugin behavior.
- `gm`, `chaitin-waf`, `wolf-rbac`, `cas-auth`, `authz-*`, vendor auth plugins: valuable only for users of those systems.

---

### Task 1: Implement `limit-req` Local Policy

**Files:**
- Create: `pkg/plugin/limit_req/plugin.go`
- Create: `pkg/plugin/limit_req/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin.Plugin`, `base.BasePlugin`, `net/http` middleware chain.
- Produces: `plugin.New("limit-req")` returning `*limit_req.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover default config, local rate rejection, custom rejection message, and separate keys.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/limit_req`

Expected: fail before implementation because the package does not exist.

- [x] **Step 3: Implement minimal local-policy plugin**

Implement local leaky-bucket state per resolved key using only the standard library.

- [x] **Step 4: Register plugin and update README**

Add `limit_req` import/case in `pkg/plugin/init.go`; mark `limit-req` checked with unsupported Redis notes.

- [x] **Step 5: Verify**

Run: `gofumpt -w pkg/plugin/limit_req/plugin.go pkg/plugin/limit_req/plugin_test.go pkg/plugin/init.go`, `go test ./pkg/plugin/limit_req`, `go test ./...`, and `make build`.

### Task 2: Implement `limit-conn` Local Policy

**Files:**
- Create: `pkg/plugin/limit_conn/plugin.go`
- Create: `pkg/plugin/limit_conn/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin.Plugin`, `base.BasePlugin`, `net/http` middleware chain.
- Produces: `plugin.New("limit-conn")` returning `*limit_conn.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover defaults, same-key concurrent rejection, custom rejection message, and separate-key concurrency.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/limit_conn`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement minimal local-policy plugin**

Implemented local in-memory active connection counts per resolved key. `conn + burst` is the admission ceiling; requests above `conn` and within `burst` sleep for `default_conn_delay` multiples.

- [x] **Step 4: Register plugin and update README**

Added `limit_conn` import/case in `pkg/plugin/init.go`; marked `limit-conn` checked with unsupported `rules`, Redis, and Redis Cluster notes.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/limit_conn -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 3: Implement `jwt-auth` HS Algorithms

**Files:**
- Create: `pkg/plugin/jwt_auth/plugin.go`
- Create: `pkg/plugin/jwt_auth/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/store/consumer_kv.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin.Plugin`, `base.BasePlugin`, `store.GetConsumerByPluginKey`, `ctx.AttachConsumer`.
- Produces: `plugin.New("jwt-auth")` returning `*jwt_auth.Plugin`; consumer lookup key `jwt-auth:<consumer.plugins.jwt-auth.key>`.

- [x] **Step 1: Write failing tests**

Tests cover Bearer token auth, consumer attachment, missing token challenge, invalid signature rejection, default expired-token rejection, and `hide_credentials`.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/jwt_auth -count=1`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement minimal HS JWT plugin**

Implemented token extraction from header/query/cookie, HS256/HS384/HS512 verification, default `exp`/`nbf` validation when claims exist, optional required `claims_to_verify`, consumer attachment, and `hide_credentials`.

- [x] **Step 4: Register plugin, index consumers, and update README**

Added `jwt_auth` import/case in `pkg/plugin/init.go`; added `jwt-auth` consumer key indexing in `pkg/store/consumer_kv.go`; marked README support with asymmetric algorithm and anonymous consumer limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/jwt_auth -count=1 -timeout=15s -v`, `go test ./...`, and `make build`.

### Task 4: Implement `acl` Consumer Labels

**Files:**
- Create: `pkg/plugin/acl/plugin.go`
- Create: `pkg/plugin/acl/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/resource/route.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: authenticated consumer attached at `$consumer` by auth plugins.
- Produces: `plugin.New("acl")` returning `*acl.Plugin`; `resource.Consumer.Labels` parsed from APISIX consumer JSON.

- [x] **Step 1: Write failing tests**

Tests cover missing authentication, `allow_labels`, `deny_labels`, custom rejection response, and comma-separated label parsing.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/acl -count=1`

Observed: fail before implementation because `Config`, `Plugin`, and `resource.Consumer.Labels` were undefined.

- [x] **Step 3: Implement minimal consumer-label ACL**

Implemented label matching for string, comma-separated string, JSON string arrays, `[]string`, and `[]any` label values.

- [x] **Step 4: Register plugin and update README**

Added `acl` import/case in `pkg/plugin/init.go`; added Security checklist entry with external-user limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/acl -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 5: Implement `response-rewrite` Status, Body, And Headers

**Files:**
- Create: `pkg/plugin/response_rewrite/plugin.go`
- Create: `pkg/plugin/response_rewrite/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin.Plugin`, `base.BasePlugin`, `net/http` middleware chain.
- Produces: `plugin.New("response-rewrite")` returning `*response_rewrite.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover status/body rewrite, base64 body decoding, header add/set/remove, and old-form header set config.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/response_rewrite -count=1`

Observed: fail before implementation because `Config`, `Plugin`, and `Headers` were undefined.

- [x] **Step 3: Implement response capture and rewrite**

Implemented a response recorder middleware that captures upstream status, headers, and body, then replays a rewritten response.

- [x] **Step 4: Register plugin and update README**

Added `response_rewrite` import/case in `pkg/plugin/init.go`; marked README support with unsupported `vars` and `filters` notes.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/response_rewrite -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 6: Implement `forward-auth` Core HTTP Flow

**Files:**
- Create: `pkg/plugin/forward_auth/plugin.go`
- Create: `pkg/plugin/forward_auth/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin.Plugin`, `base.BasePlugin`, `net/http` middleware chain.
- Produces: `plugin.New("forward-auth")` returning `*forward_auth.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover successful auth with request/upstream headers, failed auth with client headers, auth-service errors with `status_on_error`, and POST body forwarding/restoration.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/forward_auth -count=1`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement core forward-auth middleware**

Implemented auth service calls, APISIX-style `X-Forwarded-*` headers, configured request/extra headers, success upstream header injection, failure client header/body/status forwarding, request body forwarding for POST, and request body restoration.

- [x] **Step 4: Register plugin and update README**

Added `forward_auth` import/case in `pkg/plugin/init.go`; marked README support with unsupported TLS/keepalive and variable-resolution notes.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/forward_auth -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 7: Implement `proxy-mirror` HTTP Mirroring

**Files:**
- Create: `pkg/plugin/proxy_mirror/plugin.go`
- Create: `pkg/plugin/proxy_mirror/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin.Plugin`, `base.BasePlugin`, `net/http` middleware chain.
- Produces: `plugin.New("proxy-mirror")` returning `*proxy_mirror.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover mirrored method/path/query/header/body, preserving the upstream request body, `path` replace mode, and `path_concat_mode = "prefix"`.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_mirror -count=1`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement best-effort HTTP mirroring**

Implemented asynchronous mirror requests with body preservation, header cloning, path replace/prefix behavior, and `sample_ratio`.

- [x] **Step 4: Register plugin and update README**

Added `proxy_mirror` import/case in `pkg/plugin/init.go`; marked README support with unsupported gRPC mirror and APISIX resolver notes.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_mirror -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 8: Implement `opa` Core HTTP Authorization

**Files:**
- Create: `pkg/plugin/opa/plugin.go`
- Create: `pkg/plugin/opa/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin.Plugin`, `base.BasePlugin`, `net/http` middleware chain, optional APISIX consumer var.
- Produces: `plugin.New("opa")` returning `*opa.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover POSTing APISIX-style OPA input, allowed requests, denied requests with custom status/body/headers, invalid decision handling, and `send_headers_upstream`.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/opa -count=1`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement core OPA middleware**

Implemented OPA `/v1/data/<policy>` calls, APISIX-style request input, deny-by-default on decision-call errors, invalid-decision `503`, custom deny responses, upstream header injection, timeout, keepalive, and `ssl_verify`.

- [x] **Step 4: Register plugin and update README**

Added `opa` import/case in `pkg/plugin/init.go`; marked README support with full `with_route` / `with_service` payload limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/opa -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 9: Implement `hmac-auth` Core Signature Verification

**Files:**
- Create: `pkg/plugin/hmac_auth/plugin.go`
- Create: `pkg/plugin/hmac_auth/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/store/consumer_kv.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin.Plugin`, `base.BasePlugin`, `store.GetConsumerByPluginKey`, `ctx.AttachConsumer`.
- Produces: `plugin.New("hmac-auth")` returning `*hmac_auth.Plugin`; consumer lookup key `hmac-auth:<consumer.plugins.hmac-auth.key_id>`.

- [x] **Step 1: Write failing tests**

Tests cover APISIX-style `Signature` Authorization parsing, successful HMAC auth with consumer attachment, stale-date rejection, request body digest validation with body restoration, and `hide_credentials`.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/hmac_auth -count=1`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement core HMAC auth middleware**

Implemented consumer lookup by `key_id`, SHA1/SHA256/SHA512 signature validation, signed-header requirements, clock-skew checks, optional SHA-256 request body digest validation, and credential hiding.

- [x] **Step 4: Register plugin, index consumers, and update README**

Added `hmac_auth` import/case in `pkg/plugin/init.go`; added `hmac-auth` consumer key indexing in `pkg/store/consumer_kv.go`; marked README support with `anonymous_consumer` limitation.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/hmac_auth -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 10: Implement `echo` Response Body And Header Edits

**Files:**
- Create: `pkg/plugin/echo/plugin.go`
- Create: `pkg/plugin/echo/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin.Plugin`, `base.BasePlugin`, `net/http` middleware chain.
- Produces: `plugin.New("echo")` returning `*echo.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover response body replacement, `before_body` / `after_body`, response header setting, and content-length removal after body edits.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/echo -count=1`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement echo middleware**

Implemented response capture and replay for `body`, `before_body`, `after_body`, and string/number response headers.

- [x] **Step 4: Register plugin and update README**

Added `echo` import/case in `pkg/plugin/init.go`; marked README support for the implemented APISIX echo fields.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/echo -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 11: Implement `traffic-split` Inline Upstream Selection

**Files:**
- Create: `pkg/plugin/traffic_split/plugin.go`
- Create: `pkg/plugin/traffic_split/plugin_test.go`
- Create: `pkg/route/traffic_split_test.go`
- Modify: `pkg/route/builder.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin.Plugin`, `base.BasePlugin`, `net/http` middleware chain, route reverse-proxy director.
- Produces: `plugin.New("traffic-split")` returning `*traffic_split.Plugin`; request-context upstream override consumed by `pkg/route`.

- [x] **Step 1: Write failing plugin tests**

Tests cover inline upstream override, weighted round-robin across inline upstreams, and fallback to the route upstream when a weighted entry has no upstream.

- [x] **Step 2: Write failing proxy integration test**

Test covers applying a traffic-split override to the reverse-proxy request target.

- [x] **Step 3: Implement plugin and proxy override hook**

Implemented weighted inline upstream target selection and a small request-context handoff that the route director uses before normal route load balancing.

- [x] **Step 4: Register plugin and update README**

Added `traffic_split` import/case in `pkg/plugin/init.go`; marked README support with `match.vars` and `upstream_id` limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/traffic_split -count=1 -timeout=10s -v`, `go test ./pkg/route -run TestApplyTrafficSplitOverrideUpdatesProxyTarget -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 12: Implement `node-status` API Endpoint

**Files:**
- Create: `pkg/plugin/node_status/plugin.go`
- Create: `pkg/route/node_status_test.go`
- Modify: `pkg/route/extra.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `conf.plugins` through `config.GlobalConfig.Plugins`.
- Produces: `/apisix/status` extra route when `node-status` is enabled and `plugin.New("node-status")` returning `*node_status.Plugin`.

- [x] **Step 1: Write failing route tests**

Tests cover registering `/apisix/status` when enabled and skipping it when disabled.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/route -run 'TestRegisterExtraRoutes.*NodeStatus' -count=1`

Observed: fail before implementation because `/apisix/status` returned `404`.

- [x] **Step 3: Implement node-status endpoint**

Implemented the APISIX-compatible response shape with Go-side request counters and zeroed NGINX-only connection states.

- [x] **Step 4: Register plugin and update README**

Added `node_status` import/case in `pkg/plugin/init.go`; registered the extra route only when `node-status` is enabled in `conf.plugins`; documented exact NGINX counter limitation.

- [x] **Step 5: Verify**

Run: `go test ./pkg/route -run 'TestRegisterExtraRoutes.*NodeStatus' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 13: Align Existing OTel Middleware With Official `opentelemetry` Plugin Name

**Files:**
- Create: `pkg/plugin/init_test.go`
- Modify: `pkg/plugin/otel/plugin.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: existing `pkg/plugin/otel` middleware.
- Produces: `plugin.New("opentelemetry")` and compatibility alias `plugin.New("otel")`, both reporting plugin name `opentelemetry`.

- [x] **Step 1: Write failing registry tests**

Tests cover official plugin name lookup, APISIX priority, accepting empty route config, and keeping the `otel` alias.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin -run 'TestNew(OpenTelemetry|Opentelemetry|Otel)' -count=1`

Observed: fail before implementation because `opentelemetry` returned nil and `otel` still reported name `otel`.

- [x] **Step 3: Implement official alias and metadata alignment**

Registered `opentelemetry`, changed the plugin name to `opentelemetry`, updated priority to APISIX `12009`, and loosened the schema to accept official route config fields while keeping the previous `server_name` field for compatibility.

- [x] **Step 4: Update README**

Marked `opentelemetry` as partial support, documenting that this is the existing Go tracing middleware and not APISIX collector/exporter metadata parity.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin -run 'TestNew(OpenTelemetry|Opentelemetry|Otel)' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 14: Implement `server-info` Control Endpoint

**Files:**
- Create: `pkg/plugin/server_info/plugin.go`
- Modify: `pkg/route/node_status_test.go`
- Modify: `pkg/route/extra.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `conf.plugins` through `config.GlobalConfig.Plugins`.
- Produces: `/v1/server_info` extra route when `server-info` is enabled and `plugin.New("server-info")` returning `*server_info.Plugin`.

- [x] **Step 1: Write failing route tests**

Tests cover registering `/v1/server_info` when enabled and skipping it when disabled.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/route -run 'TestRegisterExtraRoutes.*(NodeStatus|ServerInfo)' -count=1`

Observed: fail before implementation because `/v1/server_info` returned `404`.

- [x] **Step 3: Implement server-info endpoint**

Implemented the APISIX-compatible response shape with hostname/id, `version`, `boot_time`, and `etcd_version: "unknown"`.

- [x] **Step 4: Register plugin and update README**

Added `server_info` import/case in `pkg/plugin/init.go`; registered the extra route only when `server-info` is enabled in `conf.plugins`; documented that periodic etcd reporting and lease keepalive are unsupported.

- [x] **Step 5: Verify**

Run: `go test ./pkg/route -run 'TestRegisterExtraRoutes.*(NodeStatus|ServerInfo)' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 15: Implement `loggly` UDP Syslog Logger

**Files:**
- Create: `pkg/plugin/loggly/plugin.go`
- Create: `pkg/plugin/loggly/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `base.BaseLoggerPlugin` and APISIX log fields from `pkg/apisix/log`.
- Produces: `plugin.New("loggly")` returning `*loggly.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover defaults, RFC5424 message shape, tags, severity mapping, and UDP send to a local listener.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/loggly -count=1`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement UDP syslog logger**

Implemented RFC5424 Loggly message generation, default tags/endpoint/severity, severity mapping by response status, and UDP sending.

- [x] **Step 4: Register plugin and update README**

Added `loggly` import/case in `pkg/plugin/init.go`; marked README support with HTTP/S bulk endpoint, batch processor, and body-capture limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/loggly -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 16: Implement `splunk-hec-logging` HTTP Event Collector Logger

**Files:**
- Create: `pkg/plugin/splunk_hec_logging/plugin.go`
- Create: `pkg/plugin/splunk_hec_logging/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `base.BaseLoggerPlugin`, APISIX-style log fields from `pkg/apisix/log`, and Splunk HEC endpoint config.
- Produces: `plugin.New("splunk-hec-logging")` returning `*splunk_hec_logging.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover APISIX/Splunk defaults, HEC event shape, token/channel headers, and POST delivery to a local HTTP server.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/splunk_hec_logging -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Endpoint`, and `Plugin` were undefined.

- [x] **Step 3: Implement HEC logger**

Implemented Splunk HEC event generation, endpoint defaults, token/channel headers, TLS verification config, and HTTP POST delivery.

- [x] **Step 4: Register plugin and update README**

Added `splunk_hec_logging` import/case in `pkg/plugin/init.go`; marked README support with batch processor and body-capture limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/splunk_hec_logging -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 17: Implement `loki-logger` HTTP Push Logger

**Files:**
- Create: `pkg/plugin/loki_logger/plugin.go`
- Create: `pkg/plugin/loki_logger/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `base.BaseLoggerPlugin`, APISIX-style log fields from `pkg/apisix/log`, and Loki endpoint config.
- Produces: `plugin.New("loki-logger")` returning `*loki_logger.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover APISIX/Loki defaults, stream payload shape, tenant/header precedence, label resolution from log fields, and POST delivery to a local HTTP server.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/loki_logger -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement Loki logger**

Implemented Loki stream payload generation, endpoint defaults, tenant/header handling, label resolution from log fields, TLS verification config, and HTTP POST delivery.

- [x] **Step 4: Register plugin and update README**

Added `loki_logger` import/case in `pkg/plugin/init.go`; marked README support with endpoint-selection, batch processor, and body-capture limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/loki_logger -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 18: Implement `clickhouse-logger` HTTP JSONEachRow Logger

**Files:**
- Create: `pkg/plugin/clickhouse_logger/plugin.go`
- Create: `pkg/plugin/clickhouse_logger/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `base.BaseLoggerPlugin`, APISIX-style log fields from `pkg/apisix/log`, and ClickHouse endpoint config.
- Produces: `plugin.New("clickhouse-logger")` returning `*clickhouse_logger.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover defaults, JSONEachRow insert body generation, deprecated `endpoint_addr` precedence, ClickHouse headers, and POST delivery to a local HTTP server.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/clickhouse_logger -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement ClickHouse logger**

Implemented HTTP POST delivery with `INSERT INTO <logtable> FORMAT JSONEachRow ...`, endpoint defaults, ClickHouse auth/database headers, TLS verification config, and metadata/instance log format handling.

- [x] **Step 4: Register plugin and update README**

Added `clickhouse_logger` import/case in `pkg/plugin/init.go`; marked README support with endpoint-selection, batch processor, and body-capture limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/clickhouse_logger -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 19: Implement `skywalking-logger` HTTP Log Reporter

**Files:**
- Create: `pkg/plugin/skywalking_logger/plugin.go`
- Create: `pkg/plugin/skywalking_logger/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `base.BaseLoggerPlugin`, APISIX-style log fields from `pkg/apisix/log`, SkyWalking OAP endpoint config, and optional `sw8` request header.
- Produces: `plugin.New("skywalking-logger")` returning `*skywalking_logger.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover defaults, `/v3/logs` endpoint construction, SkyWalking entry shape, `sw8` trace-context parsing, and POST delivery to a local HTTP server.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/skywalking_logger -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `internalSkyWalkingEndpoint`, and `parseTraceContext` were undefined.

- [x] **Step 3: Implement SkyWalking logger**

Implemented SkyWalking log entry generation, default service names, `/v3/logs` HTTP delivery, instance/metadata log format handling, and basic `sw8` trace correlation from request headers.

- [x] **Step 4: Register plugin and update README**

Added `skywalking_logger` import/case in `pkg/plugin/init.go`; marked README support with batch processor and body-capture limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/skywalking_logger -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 20: Implement `sls-logger` TLS Syslog Logger

**Files:**
- Create: `pkg/plugin/sls_logger/plugin.go`
- Create: `pkg/plugin/sls_logger/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `base.BaseLoggerPlugin`, APISIX-style log fields from `pkg/apisix/log`, and SLS TCP endpoint config.
- Produces: `plugin.New("sls-logger")` returning `*sls_logger.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover defaults, RFC5424 SLS structured data shape, JSON payload encoding, newline suffix, and TLS TCP delivery to a local listener.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/sls_logger -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement SLS logger**

Implemented RFC5424-style SLS message generation, TLS TCP sending with APISIX-like disabled certificate verification, defaults, and metadata/instance log format handling.

- [x] **Step 4: Register plugin and update README**

Added `sls_logger` import/case in `pkg/plugin/init.go`; marked README support with batch processor and body-capture limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/sls_logger -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 21: Implement `google-cloud-logging` Service Account Logger

**Files:**
- Create: `pkg/plugin/google_cloud_logging/plugin.go`
- Create: `pkg/plugin/google_cloud_logging/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `base.BaseLoggerPlugin`, APISIX-style log fields from `pkg/apisix/log`, Google service-account auth config or auth file, and Cloud Logging `entries_uri`.
- Produces: `plugin.New("google-cloud-logging")` returning `*google_cloud_logging.Plugin`.

- [x] **Step 1: Write failing tests**

Tests cover defaults, service-account JWT assertion claims/signature, Cloud Logging entry shape, auth-file loading, OAuth token exchange, and `entries:write` delivery to local HTTP servers.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/google_cloud_logging -count=1 -timeout=15s -v`

Observed: fail before implementation because `Config`, `Plugin`, `AuthConfig`, and `MonitoredResource` were undefined.

- [x] **Step 3: Implement Google Cloud logger**

Implemented service-account JWT bearer assertion signing, token exchange, auth-file loading, Cloud Logging entry generation, and HTTP delivery with `entries` plus `partialSuccess:false`.

- [x] **Step 4: Register plugin and update README**

Added `google_cloud_logging` import/case in `pkg/plugin/init.go`; marked README support with batch processor and default `httpRequest` expansion limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/google_cloud_logging -count=1 -timeout=15s -v`, `go test ./...`, and `make build`.

### Task 22: Implement `zipkin` B3 Propagation And V2 Reporting

**Files:**
- Create: `pkg/plugin/zipkin/plugin.go`
- Create: `pkg/plugin/zipkin/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `net/http` middleware requests and Zipkin endpoint config.
- Produces: `plugin.New("zipkin")` returning `*zipkin.Plugin`, injected B3 headers for upstream handlers, and Zipkin v2 span reports.

- [x] **Step 1: Write failing tests**

Tests cover defaults, single B3 parsing, invalid B3 rejection, B3 injection, v2 span reporting, and sampled=0 report suppression.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/zipkin -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `parseSingleB3` were undefined.

- [x] **Step 3: Implement Zipkin tracer**

Implemented B3 extraction/injection, basic sampling, status capture, Zipkin v2 server-span generation, and synchronous POST delivery to the configured endpoint.

- [x] **Step 4: Register plugin and update README**

Added `zipkin` import/case in `pkg/plugin/init.go`; marked README support with multi-phase span tree, batch processor, variable export, and v1 layout limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/zipkin -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 23: Implement `datadog` DogStatsD Metrics

**Files:**
- Create: `pkg/plugin/datadog/plugin.go`
- Create: `pkg/plugin/datadog/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/global-rule HTTP middleware and Datadog plugin metadata defaults.
- Produces: `plugin.New("datadog")` returning `*datadog.Plugin`, plus DogStatsD UDP metrics.

- [x] **Step 1: Write failing tests**

Tests cover metadata defaults, tag generation, DogStatsD line format, UDP metric sending, and middleware capture of status/request/response sizes.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/datadog -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Metadata`, and `metricEntry` were undefined.

- [x] **Step 3: Implement Datadog metrics plugin**

Implemented DogStatsD metric generation and UDP delivery for request count, request latency, APISIX latency, ingress size, and egress size with constant/status/path/method/scheme tags.

- [x] **Step 4: Register plugin and update README**

Added `datadog` import/case in `pkg/plugin/init.go`; marked README support with batch processor, name lookup, consumer/upstream tag, and upstream latency limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/datadog -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 24: Implement `proxy-control` Request Buffering Control

**Files:**
- Create: `pkg/plugin/proxy_control/plugin.go`
- Create: `pkg/plugin/proxy_control/plugin_test.go`
- Create: `pkg/route/proxy_control_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/route/builder.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/global-rule HTTP middleware config with `request_buffering`.
- Produces: `plugin.New("proxy-control")`, per-request proxy buffering context, and route-side request body buffering before upstream proxying.

- [x] **Step 1: Write failing tests**

Tests cover default `request_buffering`, explicit disabled buffering, context propagation, route request body buffering, and streaming-body preservation when disabled.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_control ./pkg/route -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `GetRequestBuffering`, `WithRequestBuffering`, and `bufferRequestBodyIfNeeded` were undefined.

- [x] **Step 3: Implement proxy-control plugin and route buffering**

Implemented official plugin name, priority, schema, default `request_buffering = true`, explicit `false` support through pointer config, per-request context propagation, and in-memory body buffering before reverse proxy forwarding.

- [x] **Step 4: Register plugin and update README**

Added `proxy-control` import/case in `pkg/plugin/init.go`; marked README support with APISIX-Runtime/NGINX dynamic control and disk-backed buffering limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_control ./pkg/route -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 25: Implement `proxy-buffering` Streaming Response Control

**Files:**
- Create: `pkg/plugin/proxy_buffering/plugin.go`
- Create: `pkg/plugin/proxy_buffering/plugin_test.go`
- Create: `pkg/proxy/handler_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `pkg/proxy/proxy.go`
- Modify: `pkg/route/builder.go`
- Modify: `pkg/route/proxy_control_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/global-rule HTTP middleware config with `disable_proxy_buffering`.
- Produces: `plugin.New("proxy-buffering")`, per-request proxy buffering context, and route-side selection of an immediate-flush reverse proxy for streaming responses.

- [x] **Step 1: Write failing tests**

Tests cover default `disable_proxy_buffering`, context propagation, reverse-proxy flush interval construction, and route selection of the streaming proxy handler.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_buffering ./pkg/proxy ./pkg/route -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `GetDisableProxyBuffering`, `WithDisableProxyBuffering`, `NewProxyHandlerWithFlushInterval`, and `selectProxyHandler` were undefined.

- [x] **Step 3: Implement proxy-buffering plugin and proxy selection**

Implemented official plugin name, priority, schema, default `disable_proxy_buffering = false`, per-request context propagation, configurable reverse-proxy `FlushInterval`, and immediate-flush handler selection when buffering is disabled.

- [x] **Step 4: Register plugin and update README**

Added `proxy-buffering` import/case and registry test; marked README support with NGINX internals and disk-backed response buffering limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_buffering ./pkg/proxy ./pkg/route -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 26: Implement `traffic-label` Header Labeling

**Files:**
- Create: `pkg/plugin/traffic_label/plugin.go`
- Create: `pkg/plugin/traffic_label/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/global-rule HTTP middleware config with `rules`, optional `match`, `actions`, `set_headers`, and `weight`.
- Produces: `plugin.New("traffic-label")`, request header mutation for the first matching rule, and weighted action selection.

- [x] **Step 1: Write failing tests**

Tests cover first matching rule behavior, no-match pass-through, weighted action selection including pass-through actions, header match conditions, `!=`, and `$var` header value expansion.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/traffic_label ./pkg/plugin -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Rule`, `Action`, and `plugin.New("traffic-label")` were undefined.

- [x] **Step 3: Implement traffic-label plugin**

Implemented official plugin name, priority, schema, first-match rule evaluation, `==` / `!=` match conditions, `AND` / `OR` connectors, weighted action sequences, `set_headers`, and variable resolution for common APISIX request variables.

- [x] **Step 4: Register plugin and update README**

Added `traffic-label` import/case and registry test; marked README support with full `lua-resty-expr` and full APISIX variable resolution limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/traffic_label ./pkg/plugin -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 27: Implement `workflow` Conditional Return Action

**Files:**
- Create: `pkg/plugin/workflow/plugin.go`
- Create: `pkg/plugin/workflow/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/global-rule HTTP middleware config with `rules`, optional `case`, and APISIX action-array config such as `["return", {"code": 403}]`.
- Produces: `plugin.New("workflow")`, first-match rule execution, and conditional early HTTP return responses.

- [x] **Step 1: Write failing tests**

Tests cover official action-array config parsing, matching `return` action behavior, no-match pass-through, and first matching rule precedence.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/workflow ./pkg/plugin -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Rule`, `Action`, `ReturnAction`, and `plugin.New("workflow")` were undefined.

- [x] **Step 3: Implement workflow plugin**

Implemented official plugin name, priority, schema, APISIX action-array unmarshalling, first-match rule evaluation, common request-variable matching, and the `return` action response body.

- [x] **Step 4: Register plugin and update README**

Added `workflow` import/case and registry test; marked README support with `limit-count`, `limit-conn`, full `lua-resty-expr`, and delegated plugin log-handler limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/workflow ./pkg/plugin -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 28: Implement `proxy-cache` In-Memory Response Cache

**Files:**
- Create: `pkg/plugin/proxy_cache/plugin.go`
- Create: `pkg/plugin/proxy_cache/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/global-rule HTTP middleware config with `cache_strategy`, `cache_zone`, `cache_key`, `cache_method`, `cache_http_status`, `cache_ttl`, `cache_bypass`, `no_cache`, `hide_cache_headers`, and `cache_set_cookie`.
- Produces: `plugin.New("proxy-cache")`, in-memory response cache entries, and `Apisix-Cache-Status` values for `MISS`, `HIT`, `EXPIRED`, and `BYPASS`.

- [x] **Step 1: Write failing tests**

Tests cover official defaults, GET response caching, cached header/body replay, TTL refresh, `no_cache`, `cache_bypass`, unsupported method pass-through, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_cache ./pkg/plugin -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `cacheStatusHeader`, `plugin.New("proxy-cache")`, and cache helpers were undefined.

- [x] **Step 3: Implement proxy-cache plugin**

Implemented official plugin name, priority, schema, default config values, cache key variable resolution, in-memory TTL entries, method/status filtering, `no_cache`, `cache_bypass`, `cache_set_cookie`, and response replay.

- [x] **Step 4: Register plugin and update README**

Added `proxy-cache` import/case and registry test; marked README support with disk cache zone, `Vary`, `Cache-Control`, consumer-isolation override, and stale-serving limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_cache ./pkg/plugin -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 29: Implement `body-transformer` Template Body Rewrites

**Files:**
- Create: `pkg/plugin/body_transformer/plugin.go`
- Create: `pkg/plugin/body_transformer/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/global-rule HTTP middleware config with `request` and/or `response`, `input_format`, `template`, and `template_is_base64`.
- Produces: `plugin.New("body-transformer")`, request body rewrites before upstream forwarding, and response body rewrites after upstream response capture.

- [x] **Step 1: Write failing tests**

Tests cover JSON request transformation, GET args transformation, base64 templates with `_ctx.var.*`, response transformation, invalid JSON rejection, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/body_transformer ./pkg/plugin -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Transform`, and `plugin.New("body-transformer")` were undefined.

- [x] **Step 3: Implement body-transformer plugin**

Implemented official plugin name, priority, schema, request body replacement, response capture/replacement, JSON/form/query/plain decoding, base64 template decoding, direct variable substitution, `_body`, `_ctx.var.*`, `_escape_json()`, and `_escape_xml()`.

- [x] **Step 4: Register plugin and update README**

Added `body-transformer` import/case and registry test; marked README support with XML/multipart and full `lua-resty-template` expression limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/body_transformer ./pkg/plugin -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 30: Implement `degraphql` HTTP-to-GraphQL Request Mapping

**Files:**
- Create: `pkg/plugin/degraphql/plugin.go`
- Create: `pkg/plugin/degraphql/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/global-rule HTTP middleware config with `query`, `variables`, and optional `operation_name`.
- Produces: `plugin.New("degraphql")`, POST JSON body rewriting, GET query argument rewriting, and method/body validation.

- [x] **Step 1: Write failing tests**

Tests cover POST body rewriting, GET query rewriting, variable selection, `operationName`, unsupported methods, invalid POST variable body handling, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/degraphql ./pkg/plugin -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `plugin.New("degraphql")` were undefined.

- [x] **Step 3: Implement degraphql plugin**

Implemented official plugin name, priority, schema, GET/POST method handling, variable extraction from query args or JSON request body, and GraphQL request payload generation.

- [x] **Step 4: Register plugin and update README**

Added `degraphql` import/case and registry test; marked README support with GraphQL AST and multi-operation validation limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/degraphql ./pkg/plugin -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 31: Implement `grpc-web` Browser Protocol Translation

**Files:**
- Create: `pkg/plugin/grpc_web/plugin.go`
- Create: `pkg/plugin/grpc_web/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/global-rule HTTP middleware config with optional `cors_allow_headers`.
- Produces: `plugin.New("grpc-web")`, CORS preflight responses, gRPC-Web request body translation, and gRPC-Web response trailer chunk encoding.

- [x] **Step 1: Write failing tests**

Tests cover CORS preflight, invalid method/content-type/body rejection, text-mode request decode and response encode, binary-mode pass-through, upstream `application/grpc` content type, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/grpc_web -count=1`

Observed: fail before implementation because `Config` and `Plugin` were undefined.

- [x] **Step 3: Implement grpc-web plugin**

Implemented official plugin name, priority, schema, default `cors_allow_headers`, supported content types, base64 request decoding, upstream content type rewrite, and response trailer chunk encoding.

- [x] **Step 4: Register plugin and update README**

Added `grpc-web` import/case and registry test; marked README support with URI `:ext`, exact OpenResty trailer variable, and streaming filter limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/grpc_web ./pkg/plugin -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 32: Implement `grpc-transcode` Descriptor-Set Transcoding

**Files:**
- Create: `pkg/plugin/grpc_transcode/plugin.go`
- Create: `pkg/plugin/grpc_transcode/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `pkg/resource/route.go`
- Modify: `pkg/store/getter.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: APISIX `protos` resource content, route/global-rule plugin config with `proto_id`, `service`, `method`, optional `deadline`, and optional `pb_option`.
- Produces: `plugin.New("grpc-transcode")`, HTTP query/JSON to framed protobuf request transcoding, framed protobuf response to JSON transcoding, and basic gRPC status to HTTP status mapping.

- [x] **Step 1: Write failing tests**

Tests cover GET query transcoding, POST JSON body transcoding, upstream method/path/header rewrite, gRPC response JSON decoding, nonzero gRPC status mapping, missing proto resources, and numeric `proto_id` parsing.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/grpc_transcode -count=1`

Observed: fail before implementation because `Config`, `Plugin`, `fetchProtoContent`, and `errProtoNotFound` were undefined.

- [x] **Step 3: Implement grpc-transcode plugin**

Implemented official plugin name, priority, schema, default `pb_option`, base64 FileDescriptorSet proto loading, dynamic protobuf request encoding, gRPC request framing, `grpc-timeout`, framed response decoding, JSON response rendering, and gRPC status mapping.

- [x] **Step 4: Register plugin and update README**

Added `grpc-transcode` import/case and registry test; added typed proto resource lookup; marked README support with plain `.proto`, imported-source, `pb_option`, status-details, and streaming limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/grpc_transcode ./pkg/plugin -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 33: Implement `batch-requests` Public Batch Endpoint

**Files:**
- Create: `pkg/plugin/batch_requests/plugin.go`
- Create: `pkg/route/batch_requests_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `pkg/route/extra.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `conf.plugins` enablement for `batch-requests`, optional `plugin_attr.batch-requests.uri`, and APISIX batch request bodies with `query`, `headers`, `timeout`, and `pipeline`.
- Produces: `plugin.New("batch-requests")`, an extra POST route at `/apisix/batch-requests` by default, internal route dispatch for each pipeline item, and aggregated response JSON.

- [x] **Step 1: Write failing tests**

Tests cover endpoint registration when enabled, disabled endpoint behavior, global/per-item query and header merging, response aggregation, bad request validation, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/route ./pkg/plugin -run 'BatchRequests|TestNewBatchRequests' -count=1 -timeout=10s -v`

Observed: fail before implementation because `/apisix/batch-requests` returned 404 and `plugin.New("batch-requests")` returned nil.

- [x] **Step 3: Implement batch-requests plugin**

Implemented official plugin name, priority, schema, extra-route handler, default endpoint URI, request validation, default method handling, global/per-item query and header merge, request body forwarding, internal route dispatch with isolated chi context, and aggregated status/reason/header/body responses.

- [x] **Step 4: Register plugin and update README**

Added `batch-requests` import/case and registry test; mounted the endpoint from `registerExtraRoutes`; marked README support with HTTP pipelining, metadata override, real-ip header, and `ssl_verify` limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/route ./pkg/plugin -run 'BatchRequests|TestNewBatchRequests' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 34: Implement `public-api` Internal Endpoint Exposure

**Files:**
- Create: `pkg/plugin/public_api/plugin.go`
- Create: `pkg/route/public_api_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `pkg/route/extra.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/global-rule plugin config with optional `uri`, plus internal public API registrations from extra runtime routes.
- Produces: `plugin.New("public-api")`, registered internal public API dispatch, and 404 when no public API target matches.

- [x] **Step 1: Write failing tests**

Tests cover exposing `batch-requests` at a custom route, using the request URI when `uri` is empty, 404 for unknown internal URIs, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/route ./pkg/plugin -run 'PublicAPI|TestNewPublicAPI' -count=1 -timeout=10s -v`

Observed: fail before implementation because `pkg/plugin/public_api` did not exist and `plugin.New("public-api")` returned nil.

- [x] **Step 3: Implement public-api plugin**

Implemented official plugin name, priority, schema, optional `uri` override, internal public API registry, middleware dispatch to registered runtime handlers, and 404 for unmatched public API targets.

- [x] **Step 4: Register plugin and update README**

Added `public-api` import/case and registry test; registered `node-status`, `server-info`, and `batch-requests` extra routes as public API targets; marked README support with arbitrary endpoint discovery and Prometheus proxy limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/route ./pkg/plugin -run 'PublicAPI|TestNewPublicAPI' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 35: Implement `brotli` Response Compression

**Files:**
- Create: `pkg/plugin/brotli/plugin.go`
- Create: `pkg/plugin/brotli/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: route/global-rule plugin config with `types`, `min_length`, `comp_level`, `mode`, `lgwin`, `lgblock`, `http_version`, and `vary`.
- Produces: `plugin.New("brotli")`, conditional Brotli response compression, `Content-Encoding: br`, content length removal, optional `Vary`, and ETag weakening.

- [x] **Step 1: Write failing tests**

Tests cover matching response compression and decode, missing `Accept-Encoding` skip, too-small response skip, already encoded response skip, wildcard accepted encoding/type support, positive quality values, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/brotli ./pkg/plugin -run 'Brotli|TestNewBrotli' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("brotli")` returned nil. A later red test verified `br;q=0.5` was incorrectly skipped before the parser fix.

- [x] **Step 3: Implement brotli plugin**

Implemented official plugin name, priority, schema, defaults, Accept-Encoding parsing, response capture, content type and minimum length matching, Brotli compression through `github.com/andybalholm/brotli`, content-encoding skip, content length removal, optional `Vary`, and strong ETag weakening.

- [x] **Step 4: Register plugin and update README**

Added `brotli` import/case and registry test; marked README support with NGINX streaming and `mode` / `lgwin` / `lgblock` runtime-tuning limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/brotli ./pkg/plugin -run 'TestHandler|TestNewBrotli' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 36: Implement `multi-auth` Auth Fallback

**Files:**
- Create: `pkg/plugin/multi_auth/plugin.go`
- Create: `pkg/plugin/multi_auth/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `auth_plugins`, each containing one configured auth plugin.
- Produces: `plugin.New("multi-auth")`, ordered auth fallback across supported auth plugins, and the official generic authorization failure response when all configured plugins fail.

- [x] **Step 1: Write failing tests**

Tests cover key-auth success after basic-auth failure, basic-auth success after key-auth failure, all-auth-failed rejection, unsupported auth plugin validation, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/multi_auth ./pkg/plugin -run 'MultiAuth|TestNewMultiAuth' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `AuthPluginConfig` were undefined and `plugin.New("multi-auth")` returned nil.

- [x] **Step 3: Implement multi-auth plugin**

Implemented official plugin name, priority, schema, `auth_plugins` validation, direct composition for `basic-auth`, `key-auth`, `jwt-auth`, and `hmac-auth`, probe execution for ordered fallback, pass-through when any auth plugin succeeds, and `401 {"message":"Authorization Failed"}` when all fail.

- [x] **Step 4: Register plugin and update README**

Added `multi-auth` import/case and registry test; marked README support with supported auth plugin list and failure-detail limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/multi_auth ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewMultiAuth' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 37: Implement `authz-casbin` Authorization

**Files:**
- Create: `pkg/plugin/authz_casbin/plugin.go`
- Create: `pkg/plugin/authz_casbin/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: route/service plugin config with `model` / `policy` or `model_path` / `policy_path`, plus the configured `username` request header.
- Produces: `plugin.New("authz-casbin")`, Casbin authorization over request username, path, and method, `anonymous` fallback when the username header is missing, and `403 {"message":"Access Denied"}` when policy denies the request.

- [x] **Step 1: Write failing tests**

Tests cover policy allow for the configured username header, deny response when method/path policy does not match, anonymous fallback, model/policy loading from files, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/authz_casbin ./pkg/plugin -run 'AuthzCasbin|TestNewAuthzCasbin' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("authz-casbin")` returned nil.

- [x] **Step 3: Implement authz-casbin plugin**

Implemented official plugin name, priority, schema, Casbin enforcer creation from inline model/policy text or model/policy files, username header lookup with `anonymous` fallback, allow pass-through, deny response, and service-unavailable response for unusable enforcer state.

- [x] **Step 4: Register plugin and update README**

Added `authz-casbin` import/case and registry test; marked README support with plugin metadata fallback limitation.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/authz_casbin ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewAuthzCasbin' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 38: Implement `ldap-auth` Authentication

**Files:**
- Create: `pkg/plugin/ldap_auth/plugin.go`
- Create: `pkg/plugin/ldap_auth/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `pkg/store/consumer_kv.go`
- Modify: `README.md`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: route/service plugin config with `base_dn`, `ldap_uri`, `use_tls`, `tls_verify`, `uid`, and `realm`; consumer config with `ldap-auth.user_dn`; HTTP Basic credentials in `Authorization`.
- Produces: `plugin.New("ldap-auth")`, LDAP bind authentication, consumer lookup by generated user DN, APISIX consumer context attachment, and official 401-style auth errors with `WWW-Authenticate`.

- [x] **Step 1: Write failing tests**

Tests cover Basic credential extraction, successful LDAP auth with consumer attachment, missing auth header, invalid auth header, failed LDAP bind, missing related consumer, consumer `user_dn` indexing, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ldap_auth ./pkg/plugin -run 'LDAPAuth|TestHandler|TestNewLDAPAuth' -count=1 -timeout=10s -v`

Observed: fail before implementation because `ldapAuthenticator`, `Config`, and `Plugin` were undefined and `plugin.New("ldap-auth")` returned nil.

- [x] **Step 3: Implement ldap-auth plugin**

Implemented official plugin name, priority, schema, defaults, Basic authorization parsing with APISIX-style whitespace stripping, LDAP bind through `github.com/go-ldap/ldap/v3`, TLS verification option, generated user DN lookup, consumer attachment, and 401 responses.

- [x] **Step 4: Register plugin and update README**

Added `ldap-auth` import/case and registry test; indexed consumer `ldap-auth.user_dn`; marked README support with LDAP search/filter and anonymous-consumer limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ldap_auth ./pkg/plugin -run 'TestHandler|TestNewLDAPAuth' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 39: Implement `jwe-decrypt` Authentication

**Files:**
- Create: `pkg/plugin/jwe_decrypt/plugin.go`
- Create: `pkg/plugin/jwe_decrypt/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `pkg/store/consumer_kv.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `header`, `forward_header`, and `strict`; consumer config with `jwe-decrypt.key`, `secret`, and `is_base64_encoded`; compact JWE tokens from the configured request header.
- Produces: `plugin.New("jwe-decrypt")`, consumer lookup by JWE `kid`, AES-256-GCM plaintext forwarding to `forward_header`, strict missing-token rejection, and official 400-style JWE errors.

- [x] **Step 1: Write failing tests**

Tests cover Bearer JWE decrypt and plaintext forwarding, base64url consumer secret handling, strict missing-token rejection, invalid compact JWE rejection, unknown `kid` rejection, consumer key indexing, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/jwe_decrypt ./pkg/plugin -run 'JWEDecrypt|TestHandler|TestNewJWEDecrypt' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("jwe-decrypt")` returned nil.

- [x] **Step 3: Implement jwe-decrypt plugin**

Implemented official plugin name, priority, schema, defaults, compact JWE parsing, `Bearer` token extraction, `kid` validation, consumer lookup, raw or base64url secret handling, AES-GCM decrypt, and forwarding plaintext to the configured request header.

- [x] **Step 4: Register plugin and update README**

Added `jwe-decrypt` import/case and registry test; indexed consumer `jwe-decrypt.key`; marked README support with algorithm and encrypted-field limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/jwe_decrypt ./pkg/plugin -run 'TestHandler|TestNewJWEDecrypt' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 40: Implement `kafka-logger`

**Files:**
- Create: `pkg/plugin/kafka_logger/plugin.go`
- Create: `pkg/plugin/kafka_logger/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: route/service plugin config with `brokers` or deprecated `broker_list`, `kafka_topic`, `key`, `producer_type`, `required_acks`, `timeout`, producer batching knobs, and `log_format`.
- Produces: `plugin.New("kafka-logger")`, post-response log collection through `BaseLoggerPlugin`, JSON log payload encoding, and Kafka writes through `github.com/segmentio/kafka-go`.

- [x] **Step 1: Write failing tests**

Tests cover log JSON encoding and topic/key send behavior, deprecated `broker_list` conversion with defaults, handler log-format capture after upstream response, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/kafka_logger ./pkg/plugin -run 'KafkaLogger|TestSend|TestPostInit|TestHandler' -count=1 -timeout=10s -v`

Observed: fail before implementation because `kafkaMessage`, `Config`, `Broker`, `kafkaSender`, and `Plugin` were undefined and `plugin.New("kafka-logger")` returned nil.

- [x] **Step 3: Implement kafka-logger plugin**

Implemented official plugin name, priority, schema, defaults, direct `brokers` and deprecated `broker_list` address handling, injectable Kafka sender, `kafka-go` writer creation, JSON log encoding, topic/key delivery, and BaseLoggerPlugin handler integration.

- [x] **Step 4: Register plugin and update README**

Added `kafka-logger` import/case and registry test; marked README support with SASL, batch processor, body capture, and `meta_format = origin` limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/kafka_logger ./pkg/plugin -run 'TestSend|TestPostInit|TestHandler|TestNewKafkaLogger' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 41: Implement `rocketmq-logger`

**Files:**
- Create: `pkg/plugin/rocketmq_logger/plugin.go`
- Create: `pkg/plugin/rocketmq_logger/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: route/service plugin config with `nameserver_list`, `topic`, `key`, `tag`, `timeout`, `access_key`, `secret_key`, body-capture knobs, and `log_format`.
- Produces: `plugin.New("rocketmq-logger")`, post-response log collection through `BaseLoggerPlugin`, JSON log payload encoding, and RocketMQ sync producer writes through `github.com/apache/rocketmq-client-go/v2`.

- [x] **Step 1: Write failing tests**

Tests cover log JSON encoding with topic/key/tag delivery, default timeout/body-byte settings, handler log-format capture after upstream response, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/rocketmq_logger ./pkg/plugin -run 'RocketMQLogger|TestSend|TestPostInit|TestHandler' -count=1 -timeout=10s -v`

Observed: fail before implementation because `rocketmqMessage`, `Config`, `rocketmqSender`, and `Plugin` were undefined and `plugin.New("rocketmq-logger")` returned nil.

- [x] **Step 3: Implement rocketmq-logger plugin**

Implemented official plugin name, priority, schema, defaults, injectable sender, RocketMQ producer creation from nameservers and optional credentials, JSON log encoding, topic/key/tag delivery, and BaseLoggerPlugin handler integration.

- [x] **Step 4: Register plugin and update README**

Added `rocketmq-logger` import/case and registry test; marked README support with batch processor, body capture, `meta_format = origin`, and `use_tls` limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/rocketmq_logger ./pkg/plugin -run 'TestSend|TestPostInit|TestHandler|TestNewRocketMQLogger' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 42: Implement `tencent-cloud-cls`

**Files:**
- Create: `pkg/plugin/tencent_cloud_cls/plugin.go`
- Create: `pkg/plugin/tencent_cloud_cls/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `cls_host`, `cls_topic`, `scheme`, `ssl_verify`, `secret_id`, `secret_key`, `sample_ratio`, `global_tag`, body-capture knobs, and `log_format`.
- Produces: `plugin.New("tencent-cloud-cls")`, post-response log collection through `BaseLoggerPlugin`, Tencent CLS sha1 authorization, and `/structuredlog` protobuf delivery.

- [x] **Step 1: Write failing tests**

Tests cover CLS default values, protobuf request delivery with auth headers and global tags, handler log-format capture after upstream response, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/tencent_cloud_cls ./pkg/plugin -run 'TestPostInit|TestSend|TestHandler|TestNewTencentCloudCLS' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("tencent-cloud-cls")` returned nil.

- [x] **Step 3: Implement tencent-cloud-cls plugin**

Implemented official plugin name, priority, schema, defaults, sample-ratio handler behavior, CLS protobuf encoding, global tags, sha1 signing, resty HTTP delivery, and metadata/config `log_format` support.

- [x] **Step 4: Register plugin and update README**

Added `tencent-cloud-cls` import/case and registry test; marked README support with batch processor, body capture, `max_pending_entries`, and compression limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/tencent_cloud_cls ./pkg/plugin -run 'TestPostInit|TestSend|TestHandler|TestNewTencentCloudCLS' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 43: Implement `cas-auth`

**Files:**
- Create: `pkg/plugin/cas_auth/plugin.go`
- Create: `pkg/plugin/cas_auth/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `idp_uri`, `cas_callback_uri`, `logout_uri`, and `cookie.secret` / `cookie.secure` / `cookie.samesite`.
- Produces: `plugin.New("cas-auth")`, CAS login redirects, ticket `serviceValidate`, HMAC-signed initiation cookie, per-config session cookie, local session refresh, and logout redirect.

- [x] **Step 1: Write failing tests**

Tests cover unauthenticated login redirect, callback ticket validation and session creation, existing-session pass-through, logout deletion, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/cas_auth ./pkg/plugin -run 'TestUnauthenticated|TestCallback|TestExistingSession|TestLogout|TestNewCASAuth' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `CookieConfig`, and `Plugin` were undefined and `plugin.New("cas-auth")` returned nil.

- [x] **Step 3: Implement cas-auth plugin**

Implemented official plugin name, priority, schema, defaults, login redirect, absolute/relative callback service URL calculation, ticket validation against CAS `/serviceValidate`, XML CAS user extraction, signed initiation cookie, local session storage, session refresh, and logout handling.

- [x] **Step 4: Register plugin and update README**

Added `cas-auth` import/case and registry test; marked README support with shared-dict clustering, IdP SLO XML deletion, and upstream user metadata limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/cas_auth ./pkg/plugin -run 'TestUnauthenticated|TestCallback|TestExistingSession|TestLogout|TestNewCASAuth' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 44: Implement `authz-casdoor`

**Files:**
- Create: `pkg/plugin/authz_casdoor/plugin.go`
- Create: `pkg/plugin/authz_casdoor/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `endpoint_addr`, `client_id`, `client_secret`, and `callback_url`.
- Produces: `plugin.New("authz-casdoor")`, OAuth authorize redirects, callback state validation, access token exchange against Casdoor `/api/login/oauth/access_token`, per-`client_id` session cookie, and authenticated pass-through.

- [x] **Step 1: Write failing tests**

Tests cover unauthenticated authorize redirect, callback token exchange and original URI redirect, authenticated session pass-through, invalid state rejection, invalid token response handling, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/authz_casdoor ./pkg/plugin -run 'TestUnauthenticated|TestCallback|TestInvalidToken|TestNewAuthzCasdoor' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("authz-casdoor")` returned nil.

- [x] **Step 3: Implement authz-casdoor plugin**

Implemented official plugin name, priority, schema, session cookie name hashing from `client_id`, authorize redirect, callback path derivation, state validation, form-encoded access-token exchange, token lifetime handling, and authenticated session pass-through.

- [x] **Step 4: Register plugin and update README**

Added `authz-casdoor` import/case and registry test; marked README support with encrypted session, distributed-session, HTTPS warning, and upstream metadata limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/authz_casdoor ./pkg/plugin -run 'TestUnauthenticated|TestCallback|TestInvalidToken|TestNewAuthzCasdoor' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 45: Implement `wolf-rbac`

**Files:**
- Create: `pkg/plugin/wolf_rbac/plugin.go`
- Create: `pkg/plugin/wolf_rbac/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `pkg/store/consumer_kv.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: consumer plugin config with `appid`, `server`, `header_prefix`, and `ssl_verify`; route/service plugin config for defaults.
- Produces: `plugin.New("wolf-rbac")`, consumer lookup by `appid`, `V1#appid#wolf_token` token parsing, Wolf `/wolf/rbac/access_check` calls, user info header injection, and authenticated consumer attachment.

- [x] **Step 1: Write failing tests**

Tests cover Wolf permission request shape, consumer attachment, user-info headers, missing/invalid token rejection, Wolf denial propagation, token extraction from query/cookie, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/wolf_rbac ./pkg/plugin -run 'TestHandler|TestFetchToken|TestNewWolfRBAC' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `fetchRBACToken` were undefined and `plugin.New("wolf-rbac")` returned nil.

- [x] **Step 3: Implement wolf-rbac plugin**

Implemented official plugin name, priority, schema, route defaults, token lookup/parsing, consumer lookup by `appid`, Wolf permission HTTP call, response/forwarded user-info headers, and consumer attachment.

- [x] **Step 4: Register plugin and update README**

Added `wolf-rbac` import/case and registry test; added `wolf-rbac:appid` consumer KV index; marked README support with public API, retry backoff, and metadata limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/wolf_rbac ./pkg/plugin -run 'TestHandler|TestFetchToken|TestNewWolfRBAC' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 46: Implement `authz-keycloak`

**Files:**
- Create: `pkg/plugin/authz_keycloak/plugin.go`
- Create: `pkg/plugin/authz_keycloak/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `discovery`, `token_endpoint`, `resource_registration_endpoint`, `client_id`, `client_secret`, `permissions`, `lazy_load_paths`, `http_method_as_scope`, `policy_enforcement_mode`, `access_denied_redirect_uri`, and password-grant token generation URI.
- Produces: `plugin.New("authz-keycloak")`, Bearer token extraction/prefixing, UMA decision requests, discovery/lazy resource resolution, ENFORCING access-denied handling, redirect-on-denied, and password grant token proxying.

- [x] **Step 1: Write failing tests**

Tests cover static permission UMA request shape, method-as-scope, ENFORCING empty-permission denial, access-denied redirect, password grant token generation, discovery, lazy resource lookup, service account token usage, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/authz_keycloak ./pkg/plugin -run 'TestHandler|TestPasswordGrant|TestLazyLoad|TestNewAuthzKeycloak' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `defaultGrantType` were undefined and `plugin.New("authz-keycloak")` returned nil.

- [x] **Step 3: Implement authz-keycloak plugin**

Implemented official plugin name, priority, schema, defaults, token endpoint/discovery resolution, service-account token cache, lazy resource lookup, static permissions, method scope derivation, UMA decision form requests, access denied response/redirect behavior, and password grant token proxying.

- [x] **Step 4: Register plugin and update README**

Added `authz-keycloak` import/case and registry test; marked README support with shared cache, refresh token, proxy/decorator, resource metadata, and keepalive limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/authz_keycloak ./pkg/plugin -run 'TestHandler|TestPasswordGrant|TestLazyLoad|TestNewAuthzKeycloak' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 47: Implement `openid-connect`

**Files:**
- Create: `pkg/plugin/openid_connect/plugin.go`
- Create: `pkg/plugin/openid_connect/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `client_id`, `client_secret`, `discovery`, `introspection_endpoint`, `introspection_endpoint_auth_method`, `bearer_only`, `realm`, `required_scopes`, `unauth_action`, output-header flags, `ssl_verify`, `timeout`, and `introspection_addon_headers`.
- Produces: `plugin.New("openid-connect")`, bearer token extraction from `Authorization` and `X-Access-Token`, discovery fallback for token introspection, trusted output header injection, scope enforcement, and unauthenticated pass/deny behavior.

- [x] **Step 1: Write failing tests**

Tests cover discovery-backed introspection, `client_secret_basic`, trusted output header replacement, `X-Access-Token` bearer input, required-scope denial, bearer-only missing-token 401, `unauth_action = pass`, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/openid_connect ./pkg/plugin -run 'TestHandler|TestNewOpenIDConnect' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("openid-connect")` returned nil.

- [x] **Step 3: Implement openid-connect plugin**

Implemented official plugin name, priority, schema subset, defaults, output header clearing, bearer extraction, discovery caching, introspection form requests, `client_secret_basic` / `client_secret_post`, addon introspection headers, active-token handling, required-scope enforcement, and `X-Access-Token` / `X-Userinfo` injection.

- [x] **Step 4: Register plugin and update README**

Added `openid-connect` import/case and registry test; marked README support with authorization-code/session, logout/revocation, JWKS/public-key verification, PKCE, Redis session, token renewal, claim schema, proxy, and client assertion limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/openid_connect ./pkg/plugin -run 'TestHandler|TestNewOpenIDConnect' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 48: Implement `log-rotate`

**Files:**
- Create: `pkg/plugin/log_rotate/plugin.go`
- Create: `pkg/plugin/log_rotate/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official global `plugin_attr.log-rotate` keys `interval`, `max_kept`, `max_size`, `timeout`, and `enable_compression`; local Go path overrides `log_dir`, `access_log`, `error_log`, and `enable_access_log`.
- Produces: `plugin.New("log-rotate")`, official priority/schema/name, APISIX timestamped rotated filenames, max-size and interval checks, current-file recreation, retention pruning, and `.tar.gz` compression.

- [x] **Step 1: Write failing tests**

Tests cover max-size rotation, APISIX timestamp naming, access/error current-file recreation, history pruning by `max_kept`, compression to `.tar.gz`, official default values, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/log_rotate ./pkg/plugin -run 'TestRotate|TestDefaults|TestNewLogRotate' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("log-rotate")` returned nil.

- [x] **Step 3: Implement log-rotate plugin**

Implemented official plugin name, priority, empty route schema, plugin-attr loading, defaults, max-size and interval rotation, direct file rename/recreation, retention scan/prune, and stdlib tar+gzip compression.

- [x] **Step 4: Register plugin and update README**

Added `log-rotate` import/case and registry test; marked README support with OpenResty timer, NGINX `USR1`, NGINX path discovery, and shell `tar` limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/log_rotate ./pkg/plugin -run 'TestRotate|TestDefaults|TestNewLogRotate' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 49: Implement `error-log-logger`

**Files:**
- Create: `pkg/plugin/error_log_logger/plugin.go`
- Create: `pkg/plugin/error_log_logger/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official global route schema `{}`, metadata-shaped config with `tcp`, `skywalking`, `clickhouse`, `kafka`, `level`, `timeout`, `keepalive`, and batch knobs.
- Produces: `plugin.New("error-log-logger")`, official priority/schema/name, level filtering for captured error-log lines, TCP/TLS delivery, SkyWalking `/v3/logs` payloads, ClickHouse JSONEachRow inserts, and Kafka topic/key publishing.

- [x] **Step 1: Write failing tests**

Tests cover TCP delivery with level filtering, SkyWalking log-entry shape, ClickHouse insert body/headers, Kafka topic/key/value publishing, official default values, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/error_log_logger ./pkg/plugin -run 'TestSendLogs|TestDefaults|TestNewErrorLogLogger' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, sink config types, and `kafkaMessage` were undefined and `plugin.New("error-log-logger")` returned nil.

- [x] **Step 3: Implement error-log-logger plugin**

Implemented official plugin name, priority, empty route schema, metadata-shaped config, defaults, severity parsing/filtering, TCP/TLS sender, SkyWalking sender, ClickHouse sender, Kafka sender, and no-op global HTTP handler.

- [x] **Step 4: Register plugin and update README**

Added `error-log-logger` import/case and registry test; marked README support with direct `ngx.errlog`, OpenResty timer, batch retry, Kafka SASL, and encrypted metadata limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/error_log_logger ./pkg/plugin -run 'TestSendLogs|TestDefaults|TestNewErrorLogLogger' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 50: Implement `skywalking`

**Files:**
- Create: `pkg/plugin/skywalking/plugin.go`
- Create: `pkg/plugin/skywalking/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `sample_ratio` and `plugin_attr.skywalking` values `service_name`, `service_instance_name`, `endpoint_addr`, and `report_interval`.
- Produces: `plugin.New("skywalking")`, official priority/schema/name, `sw8` extraction/injection, sampled request timing, SkyWalking-style segment payload construction, and HTTP reporting to `/v3/segments`.

- [x] **Step 1: Write failing tests**

Tests cover defaults, SW8 context parsing, SW8 injection, segment report shape, incoming trace ID preservation, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/skywalking ./pkg/plugin -run 'TestPostInit|TestParseSW8|TestHandler|TestNewSkywalking' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `parseSW8` were undefined and `plugin.New("skywalking")` returned nil.

- [x] **Step 3: Implement skywalking plugin**

Implemented official plugin name, priority, route schema, plugin-attr loading, defaults, SW8 decode/encode, trace and segment IDs, request span timing, APISIX component ID, status tags, `$hostname` service instance handling, and HTTP segment reporting.

- [x] **Step 4: Register plugin and update README**

Added `skywalking` import/case and registry test; marked README support with native OpenResty tracer, shared buffer, delayed body-filter lifecycle, streaming finish, and full reference-fidelity limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/skywalking ./pkg/plugin -run 'TestPostInit|TestParseSW8|TestHandler|TestNewSkywalking' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 51: Implement `chaitin-waf`

**Files:**
- Create: `pkg/plugin/chaitin_waf/plugin.go`
- Create: `pkg/plugin/chaitin_waf/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `mode`, `match`, `append_waf_resp_header`, `append_waf_debug_header`, `config`, and metadata-shaped `nodes` / `mode` / `config`.
- Produces: `plugin.New("chaitin-waf")`, official priority/schema/name, WAF request forwarding, monitor/block/off behavior, official WAF response headers, body restoration, and block responses with `event_id`.

- [x] **Step 1: Write failing tests**

Tests cover allowed WAF pass, request body restoration, blocked rejection body/headers, monitor-mode nonblocking rejection, no-match skipping, defaults, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/chaitin_waf ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewChaitinWAF' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Node`, `MatchRule`, WAF header constants, and `wafDecision` were undefined and `plugin.New("chaitin-waf")` returned nil.

- [x] **Step 3: Implement chaitin-waf plugin**

Implemented official plugin name, priority, schema, config defaults, metadata fallback, request matching for common vars, WAF HTTP forwarding, response header shaping, pass/reject decisions, monitor/block/off handling, body restoration, and official block body.

- [x] **Step 4: Register plugin and update README**

Added `chaitin-waf` import/case and registry test; marked README support with `resty.t1k`, health checker, round-robin, full expression, header-filter, Unix socket, and SafeLine binary protocol limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/chaitin_waf ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewChaitinWAF' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 52: Implement `gm`

**Files:**
- Create: `pkg/plugin/gm/plugin.go`
- Create: `pkg/plugin/gm/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with official empty object schema, and local SSL config shape with `gm`, `cert`, `key`, `certs`, `keys`, and `snis`.
- Produces: `plugin.New("gm")`, official priority/schema/name, no-op HTTP handler, and APISIX GM SSL marker validation requiring exactly one signing cert/key pair.

- [x] **Step 1: Write failing tests**

Tests cover HTTP pass-through behavior, ordinary SSL validation, GM dual-cert sign pair requirement, exact sign pair count validation, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/gm ./pkg/plugin -run 'TestHandler|TestValidateSSLConfig|TestNewGM' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Plugin`, `SSLConfig`, and `ValidateSSLConfig` were undefined and `plugin.New("gm")` returned nil.

- [x] **Step 3: Implement gm plugin**

Implemented official plugin name, priority, empty route schema, no-op HTTP handler, and local GM SSL validation for APISIX-style dual certificate config.

- [x] **Step 4: Register plugin and update README**

Added `gm` import/case and registry test; marked README support with Tongsuo/APISIX-Runtime NTLS, SM2/SM3/SM4 TLS handshake, dynamic TLS install, SSL schema injection, and real dual-certificate serving limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/gm ./pkg/plugin -run 'TestHandler|TestValidateSSLConfig|TestNewGM' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

## Official APISIX 3.17 Inventory Audit After Task 104

Run against `https://api.github.com/repos/apache/apisix/contents/apisix/plugins?ref=release/3.17`:

- Official top-level Lua plugin files: 116.
- README plugin documentation/source links: 117.
- `plugin.New` registered plugin names: 115.
- README no longer has unchecked APISIX plugin links after `gm`, but README is not an exhaustive official 3.17 plugin inventory.

Official APISIX 3.17 plugin docs not represented by README plugin links yet:

None after Task 91.

Official top-level Lua plugin names not represented one-to-one by README plugin links:

- `serverless-pre-function` and `serverless-post-function` are documented under the official `serverless` docs page.

Suggested next valuable batches after Task 104:

1. Re-audit nested helper/plugin directories and docs-only plugins against official APISIX 3.17.

### Task 53: Implement `data-mask`

**Files:**
- Create: `pkg/plugin/data_mask/plugin.go`
- Create: `pkg/plugin/data_mask/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `request`, `max_body_size`, `max_req_post_args`, masking item `type`, `body_format`, `name`, `action`, `regex`, and `value`.
- Produces: `plugin.New("data-mask")`, official priority/schema/name, query/header/urlencoded-body masking, simple JSONPath JSON-body masking, and `remove` / `replace` / `regex` actions.

- [x] **Step 1: Write failing tests**

Tests cover query/header/urlencoded-body masking, JSON body masking with simple JSONPath and `[*]`, defaults, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/data_mask ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewDataMask' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `MaskRule` were undefined and `plugin.New("data-mask")` returned nil.

- [x] **Step 3: Implement data-mask plugin**

Implemented official plugin name, priority, schema, defaults, query/header/urlencoded masking, simple JSONPath masking for dot paths and `[*]`, request body rewrite, and masking actions.

- [x] **Step 4: Register plugin and update README**

Added `data-mask` import/case and registry test; marked README support with APISIX log-phase-only behavior, full `jsonpath`, temporary-file request body, access-log request-line, and upstream preservation limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/data_mask ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewDataMask' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 54: Implement `error-page`

**Files:**
- Create: `pkg/plugin/error_page/plugin.go`
- Create: `pkg/plugin/error_page/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official empty route/service config and metadata-shaped `enable`, `error_404`, `error_500`, `error_502`, and `error_503` pages.
- Produces: `plugin.New("error-page")`, official priority/schema/name, response body/content-type/content-length rewrite for configured error pages, and default APISIX-style HTML error bodies.

- [x] **Step 1: Write failing tests**

Tests cover custom 404 body/content-type/content-length rewrite, disabled metadata pass-through, unconfigured status pass-through, successful response pass-through, default 500 HTML page, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/error_page ./pkg/plugin -run 'TestHandler|TestDefaultErrorPage|TestNewErrorPage' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Metadata`, `Plugin`, and `ErrorPage` were undefined and `plugin.New("error-page")` returned nil.

- [x] **Step 3: Implement error-page plugin**

Implemented official plugin name, priority, empty route schema, metadata loading fallback, APISIX-style default HTML bodies, response capture, and configured error-page rewrite.

- [x] **Step 4: Register plugin and update README**

Added `error-page` import/case and registry test; marked README support with response-source detection, filter-phase, metadata schema exposure, and APISIX-generated-only rewrite limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/error_page ./pkg/plugin -run 'TestHandler|TestDefaultErrorPage|TestNewErrorPage' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 55: Implement `exit-transformer`

**Files:**
- Create: `pkg/plugin/exit_transformer/plugin.go`
- Create: `pkg/plugin/exit_transformer/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with official `functions` array of Lua source strings.
- Produces: `plugin.New("exit-transformer")`, official priority/schema/name, chained response capture, documented status-remap Lua pattern support, and documented normalized JSON error-body/header pattern support.

- [x] **Step 1: Write failing tests**

Tests cover the documented 401-to-403 Lua pattern, documented normalized JSON error body and `X-Error-Code` header pattern, chained transforms, successful response pass-through, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/exit_transformer ./pkg/plugin -run 'TestHandler|TestNewExitTransformer' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("exit-transformer")` returned nil.

- [x] **Step 3: Implement exit-transformer plugin**

Implemented official plugin name, priority, schema, response capture, chained transformation application, documented status remap parsing, and documented normalized error JSON/header output.

- [x] **Step 4: Register plugin and update README**

Added `exit-transformer` import/case and registry test; marked README support with arbitrary Lua execution, `core.response.exit()` integration, Lua cache, APISIX-generated-only response detection, and general Lua mutation limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/exit_transformer ./pkg/plugin -run 'TestHandler|TestNewExitTransformer' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 56: Implement `attach-consumer-label`

**Files:**
- Create: `pkg/plugin/attach_consumer_label/plugin.go`
- Create: `pkg/plugin/attach_consumer_label/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: authenticated request context with `$consumer` and route/service plugin config `headers`, where each value is a `$label_key` reference.
- Produces: `plugin.New("attach-consumer-label")`, official priority/schema/name, and configured request headers populated from authenticated consumer labels.

- [x] **Step 1: Write failing tests**

Tests cover configured label attachment, missing labels remaining absent, pass-through without authenticated consumer context, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/attach_consumer_label ./pkg/plugin -run 'TestHandler|TestNewAttachConsumerLabel' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("attach-consumer-label")` returned nil.

- [x] **Step 3: Implement attach-consumer-label plugin**

Implemented official plugin name, priority, schema, and request-header mutation from configured `$label` references using the existing `$consumer` context set by auth plugins.

- [x] **Step 4: Register plugin and update README**

Added `attach-consumer-label` import/case and registry test; marked README support with non-string label serialization, independent authentication, and Lua/OpenResty phase-fidelity limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/attach_consumer_label ./pkg/plugin -run 'TestHandler|TestNewAttachConsumerLabel' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 57: Implement `serverless-pre-function` And `serverless-post-function`

**Files:**
- Create: `pkg/plugin/serverless/plugin.go`
- Create: `pkg/plugin/serverless/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: route/service plugin config with `phase` and `functions`, where each function is a Lua chunk that returns a Lua function.
- Produces: `plugin.New("serverless-pre-function")`, `plugin.New("serverless-post-function")`, official priorities/schemas/names, request-phase Lua execution, returned `code` / `body` short-circuiting, and response capture for filter/log phases.

- [x] **Step 1: Write failing tests**

Tests cover pre-function returned response short-circuiting, pre-function request header mutation, post-function documented JSON body-filter rewrite, invalid non-function Lua validation, and registry validation for both official names.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/serverless ./pkg/plugin -run 'TestPreFunction|TestPostFunction|TestPostInitRejectsLua|TestNewServerless' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `NewPreFunction`, and `NewPostFunction` were undefined and both `plugin.New(...)` registry calls returned nil.

- [x] **Step 3: Implement serverless plugins**

Added `github.com/yuin/gopher-lua` and implemented shared serverless plugin execution with APISIX-compatible config validation, Lua chunks returning functions, sequential function calls, `ngx.log`, `ngx.say`, `ngx.req.set_header`, `ngx.header`, `ngx.status`, `ngx.arg`, `require("cjson")`, and selected `require("apisix.core")` helpers including `response.hold_body_chunk`.

- [x] **Step 4: Register plugins and update README**

Added registry cases/tests for `serverless-pre-function` and `serverless-post-function`; marked README support with OpenResty/APISIX runtime, shared-dict/lrucache, custom variable registration, streaming chunks, and exact phase lifecycle limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/serverless ./pkg/plugin -run 'TestPreFunction|TestPostFunction|TestPostInitRejectsLua|TestNewServerless' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 58: Implement `azure-functions` And `openfunction`

**Files:**
- Create: `pkg/plugin/function_upstream/plugin.go`
- Create: `pkg/plugin/azure_functions/plugin.go`
- Create: `pkg/plugin/azure_functions/plugin_test.go`
- Create: `pkg/plugin/openfunction/plugin.go`
- Create: `pkg/plugin/openfunction/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `function_uri`, optional auth object, timeout, SSL verify, and keepalive fields.
- Produces: `plugin.New("azure-functions")`, `plugin.New("openfunction")`, official priorities/schemas/names, function request forwarding, auth-header processing, and relayed function responses.

- [x] **Step 1: Write failing tests**

Tests cover Azure function request forwarding and response relay, Azure auth header injection without overwriting client headers, OpenFunction Basic auth injection, and registry validation for both official names.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/azure_functions ./pkg/plugin/openfunction ./pkg/plugin -run 'TestHandler|TestNewAzureFunctions|TestNewOpenFunction' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `Authorization` were undefined and both `plugin.New(...)` registry calls returned nil.

- [x] **Step 3: Implement generic function upstream and plugin wrappers**

Implemented shared generic upstream request forwarding and thin Azure/OpenFunction wrappers. Azure injects `X-Functions-Key` and `X-Functions-Clientid` from plugin config only when client headers are absent. OpenFunction injects Basic authorization from `authorization.service_token`.

- [x] **Step 4: Register plugins and update README**

Added registry cases/tests and README support notes. Marked metadata master-key fallback, wildcard `:ext` path forwarding, HTTP/2 connection-header filtering, APISIX keepalive semantics, and active `ssl_verify` transport control as limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/azure_functions ./pkg/plugin/openfunction ./pkg/plugin -run 'TestHandler|TestNewAzureFunctions|TestNewOpenFunction' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 59: Implement `openwhisk`

**Files:**
- Create: `pkg/plugin/openwhisk/plugin.go`
- Create: `pkg/plugin/openwhisk/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `api_host`, `service_token`, `namespace`, optional `package`, `action`, `result`, `timeout`, SSL verify, and keepalive fields.
- Produces: `plugin.New("openwhisk")`, official priority/schema/name, OpenWhisk action endpoint invocation, Basic auth, OpenWhisk query parameters, JSON result parsing, and relayed action response headers/status/body.

- [x] **Step 1: Write failing tests**

Tests cover action endpoint construction with package, POST body forwarding, Basic auth, default `blocking=true`, `result=true`, and `timeout=3000` query parameters, JSON result `statusCode` / `headers` / `body`, invalid JSON fallback to 503, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/openwhisk ./pkg/plugin -run 'TestHandler|TestNewOpenWhisk' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("openwhisk")` returned nil.

- [x] **Step 3: Implement openwhisk plugin**

Implemented official plugin name, priority, schema, defaults, OpenWhisk action endpoint construction, POST request forwarding, Basic auth, response JSON envelope parsing, action response header/status/body relay, and invalid response fallback to 503.

- [x] **Step 4: Register plugin and update README**

Added `openwhisk` import/case and registry test; marked README support with `ssl_verify`, APISIX keepalive pool, OpenResty response-header behavior, and body type edge-case limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/openwhisk ./pkg/plugin -run 'TestHandler|TestNewOpenWhisk' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 60: Implement `aws-lambda`

**Files:**
- Create: `pkg/plugin/aws_lambda/plugin.go`
- Create: `pkg/plugin/aws_lambda/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `function_uri`, optional `authorization.apikey`, optional `authorization.iam`, timeout, SSL verify, and keepalive fields.
- Produces: `plugin.New("aws-lambda")`, official priority/schema/name, generic upstream invocation, API key injection, IAM SigV4 signing, and relayed function responses.

- [x] **Step 1: Write failing tests**

Tests cover AWS Lambda/API Gateway request forwarding and response relay, configured API key injection, client API key preservation, IAM SigV4 `Authorization` / `X-Amz-Date` signing with fixed clock, and registry validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/aws_lambda ./pkg/plugin -run 'TestHandler|TestNewAWSLambda' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Authorization`, and test clock `now` were undefined and `plugin.New("aws-lambda")` returned nil.

- [x] **Step 3: Implement aws-lambda plugin**

Implemented official plugin name, priority, schema, defaults, generic upstream forwarding, API Gateway `X-Api-Key` injection without overwriting client headers, and IAM SigV4 signing with AWS4 credential scope, signed headers, `X-Amz-Date`, and request-body hash.

- [x] **Step 4: Register plugin and update README**

Added `aws-lambda` import/case and registry test; marked README support with SigV4 canonicalization edge-case, wildcard path forwarding, APISIX keepalive, and active `ssl_verify` limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/aws_lambda ./pkg/plugin -run 'TestHandler|TestNewAWSLambda' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 61: Implement `lago`

**Files:**
- Create: `pkg/plugin/lago/plugin.go`
- Create: `pkg/plugin/lago/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `endpoint_addrs`, `endpoint_uri`, `token`, `event_transaction_id`, `event_subscription_id`, `event_code`, `event_properties`, SSL verify, timeout, and keepalive fields.
- Produces: `plugin.New("lago")`, official priority/schema/name, Lago batch event payloads, Bearer-token HTTP delivery, and template resolution for transaction ID, subscription ID, and event properties.

- [x] **Step 1: Write failing tests**

Tests cover official defaults, template resolution, HTTP batch event delivery with Bearer token, handler-captured request/response variables, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/lago ./pkg/plugin -run 'TestPostInitSetsLagoDefaults|TestBuildEventResolvesConfiguredTemplates|TestSendPostsLagoBatchEvent|TestHandlerCapturesRequestAndResponseVariables|TestNewLago' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `lagoPayload`, and related helpers were undefined and `plugin.New("lago")` returned nil.

- [x] **Step 3: Implement lago plugin**

Implemented official plugin name, priority, schema, defaults, HTTP Lago batch event delivery, Bearer authorization, template resolution, response status capture, and request/header/APISIX variable lookup for configured templates.

- [x] **Step 4: Register plugin and update README**

Added `lago` import/case and registry test; marked README support with APISIX batch processor, random endpoint selection, complete variable coverage, request start-time fidelity, and body capture limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/lago ./pkg/plugin -run 'TestPostInitSetsLagoDefaults|TestBuildEventResolvesConfiguredTemplates|TestSendPostsLagoBatchEvent|TestHandlerCapturesRequestAndResponseVariables|TestNewLago' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 62: Implement `graphql-limit-count`

**Files:**
- Create: `pkg/plugin/graphql_limit_count/plugin.go`
- Create: `pkg/plugin/graphql_limit_count/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `count`, `time_window`, `key`, `key_type`, `rejected_code`, `rejected_msg`, `policy`, `allow_degradation`, and `show_limit_quota_header`.
- Produces: `plugin.New("graphql-limit-count")`, official priority/schema/name, POST GraphQL request validation, selection-depth parsing, local fixed-window quota consumption by depth cost, and `X-RateLimit-*` headers.

- [x] **Step 1: Write failing tests**

Tests cover nested and fragment GraphQL depth, JSON and `application/graphql` request bodies, depth-cost quota exhaustion, invalid request rejection, fixed-window reset, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/graphql_limit_count ./pkg/plugin -run 'TestGraphQL|TestHandler|TestWindowResets|TestNewGraphQLLimitCount' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `queryDepth`, and related helpers were undefined and `plugin.New("graphql-limit-count")` returned nil.

- [x] **Step 3: Implement graphql-limit-count plugin**

Implemented official plugin name, priority, schema, POST-only GraphQL request validation, JSON/raw GraphQL body extraction, selection-depth parser with fragment and inline-fragment support, local fixed-window quota counters, key resolution, rejected response handling, and rate-limit headers.

- [x] **Step 4: Register plugin and update README**

Added `graphql-limit-count` import/case and registry test; marked README support with Redis/Redis Cluster, full GraphQL spec parsing parity, `graphql.max_size`, and exact `resty.limit.count` behavior limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/graphql_limit_count ./pkg/plugin -run 'TestGraphQL|TestHandler|TestWindowResets|TestNewGraphQLLimitCount' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 63: Implement `graphql-proxy-cache`

**Files:**
- Create: `pkg/plugin/graphql_proxy_cache/plugin.go`
- Create: `pkg/plugin/graphql_proxy_cache/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `cache_strategy`, `cache_zone`, `cache_ttl`, `consumer_isolation`, and `cache_set_cookie`.
- Produces: `plugin.New("graphql-proxy-cache")`, official priority/schema/name, GET/POST GraphQL request validation, mutation bypass, in-memory TTL caching, `Apisix-Cache-Status`, and `APISIX-Cache-Key`.

- [x] **Step 1: Write failing tests**

Tests cover repeated POST JSON query caching, GET query caching, mutation bypass, invalid request rejection, expired cache refresh, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/graphql_proxy_cache ./pkg/plugin -run 'TestHandler|TestNewGraphQLProxyCache' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, cache header constants, and related helpers were undefined and `plugin.New("graphql-proxy-cache")` returned nil.

- [x] **Step 3: Implement graphql-proxy-cache plugin**

Implemented official plugin name, priority, schema, config defaults, GET/POST GraphQL extraction and validation, mutation bypass, MD5 cache key generation, in-memory TTL cache, `cache_set_cookie`, and cache status/key response headers.

- [x] **Step 4: Register plugin and update README**

Added `graphql-proxy-cache` import/case and registry test; marked README support with NGINX disk cache zones, public `PURGE` endpoint, configured `graphql.max_size`, route/service ID cache-key participation, full GraphQL parser parity, and exact APISIX `proxy-cache` handler limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/graphql_proxy_cache ./pkg/plugin -run 'TestHandler|TestNewGraphQLProxyCache' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 64: Implement `kafka-proxy`

**Files:**
- Create: `pkg/plugin/kafka_proxy/plugin.go`
- Create: `pkg/plugin/kafka_proxy/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with optional `sasl.username` and `sasl.password`.
- Produces: `plugin.New("kafka-proxy")`, official priority/schema/name, and request-context SASL metadata for future Kafka upstream transport integration.

- [x] **Step 1: Write failing tests**

Tests cover SASL request-context propagation, disabled/no-op behavior, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/kafka_proxy ./pkg/plugin -run 'TestHandler|TestNewKafkaProxy' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `SASL`, context getter helpers, and `plugin.New("kafka-proxy")` were undefined.

- [x] **Step 3: Implement kafka-proxy plugin**

Implemented official plugin name, priority, schema, optional SASL/PLAIN config, middleware no-op behavior when SASL is absent, and request-context getters for enabled flag, username, and password.

- [x] **Step 4: Register plugin and update README**

Added `kafka-proxy` import/case and registry test; marked README support with Kafka upstream transport/proxying, websocket forwarding, non-PLAIN SASL, and encrypted storage limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/kafka_proxy ./pkg/plugin -run 'TestHandler|TestNewKafkaProxy' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 65: Implement `oas-validator`

**Files:**
- Create: `pkg/plugin/oas_validator/plugin.go`
- Create: `pkg/plugin/oas_validator/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `spec`, `spec_url`, `spec_url_request_headers`, `ssl_verify`, `timeout`, `verbose_errors`, request-validation skip flags, `reject_if_not_match`, and `rejection_status_code`.
- Produces: `plugin.New("oas-validator")`, official priority/schema/name, inline and remote OpenAPI JSON spec loading, method/path matching, required path/query/header parameter checks, JSON request body schema validation, pass-through logging mode, and configurable rejection responses.

- [x] **Step 1: Write failing tests**

Tests cover inline OpenAPI spec rejection, valid body pass-through and body restoration, skip/pass-through modes, `spec_url` fetch with configured headers, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/oas_validator ./pkg/plugin -run 'TestHandler|TestNewOASValidator' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config` and `Plugin` were undefined and `plugin.New("oas-validator")` returned nil.

- [x] **Step 3: Implement oas-validator plugin**

Implemented official plugin name, priority, schema, defaults, lazy inline/remote spec compilation, HTTP `spec_url` fetch with custom headers, basic OpenAPI path-template matching, required path/query/header checks, JSON request body schema validation with restored body, `reject_if_not_match`, verbose errors, and JSON rejection responses.

- [x] **Step 4: Register plugin and update README**

Added `oas-validator` import/case and registry test; marked README support with OpenAPI `$ref` / `components`, metadata TTL refresh, full parameter style/explode, non-JSON body validation, and response-validation limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/oas_validator ./pkg/plugin -run 'TestHandler|TestNewOASValidator' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 66: Implement `dubbo-proxy`

**Files:**
- Create: `pkg/plugin/dubbo_proxy/plugin.go`
- Create: `pkg/plugin/dubbo_proxy/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `service_name`, `service_version`, and optional `method`.
- Produces: `plugin.New("dubbo-proxy")`, official priority/schema/name, enabled flag, Dubbo service name/version, and method request-context metadata. If `method` is unset, derives it from the request URI without the leading slash.

- [x] **Step 1: Write failing tests**

Tests cover Dubbo request-context metadata, URI-derived method fallback, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/dubbo_proxy ./pkg/plugin -run 'TestHandler|TestNewDubboProxy' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, context getter helpers, and `plugin.New("dubbo-proxy")` were undefined.

- [x] **Step 3: Implement dubbo-proxy plugin**

Implemented official plugin name, priority, schema, required service metadata, optional method, URI-derived method fallback, enabled flag, and request-context getters for future Dubbo upstream transport integration.

- [x] **Step 4: Register plugin and update README**

Added `dubbo-proxy` import/case and registry test; marked README support with OpenResty/Tengine Dubbo runtime, hessian2 Map conversion, `upstream_multiplex_count`, HTTP-to-Dubbo proxy transport, and Dubbo response conversion limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/dubbo_proxy ./pkg/plugin -run 'TestHandler|TestNewDubboProxy' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 67: Implement `ai-prompt-decorator`

**Files:**
- Create: `pkg/plugin/ai_prompt_decorator/plugin.go`
- Create: `pkg/plugin/ai_prompt_decorator/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `prepend` and/or `append` prompt messages containing `role` and `content`.
- Produces: `plugin.New("ai-prompt-decorator")`, official priority/schema/name, JSON request body rewrite, OpenAI Chat `messages` prepend/append, OpenAI Responses `instructions` prepend and `input` append, and replayable rewritten request body.

- [x] **Step 1: Write failing tests**

Tests cover OpenAI Chat message decoration, OpenAI Responses instruction/input decoration, invalid JSON rejection, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai_prompt_decorator ./pkg/plugin -run 'TestHandler|TestNewAIPromptDecorator' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Message`, and `plugin.New("ai-prompt-decorator")` were undefined.

- [x] **Step 3: Implement ai-prompt-decorator plugin**

Implemented official plugin name, priority, schema, JSON body parsing, OpenAI Chat `messages` prepend/append, OpenAI Responses `instructions` prepend and `input` append, request body replay, content-length update, and JSON error responses for empty/invalid bodies.

- [x] **Step 4: Register plugin and update README**

Added `ai-prompt-decorator` import/case and registry test; added README AI section with Anthropic Messages, Bedrock Converse, OpenAI Embeddings, passthrough protocol decoration, APISIX AI protocol registry, streaming-specific behavior, and real `ai-proxy` provider transport limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai_prompt_decorator ./pkg/plugin -run 'TestHandler|TestNewAIPromptDecorator' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 68: Implement `ai-prompt-guard`

**Files:**
- Create: `pkg/plugin/ai_prompt_guard/plugin.go`
- Create: `pkg/plugin/ai_prompt_guard/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `match_all_roles`, `match_all_conversation_history`, `allow_patterns`, and `deny_patterns`.
- Produces: `plugin.New("ai-prompt-guard")`, official priority/schema/name, compiled regex validation, allow-before-deny prompt checks, default last-user-message filtering, optional all-role/all-history filtering, OpenAI Chat message extraction, OpenAI Responses input extraction, and JSON rejection responses.

- [x] **Step 1: Write failing tests**

Tests cover allow-before-deny behavior, default last-user-message filtering, all-role/all-history checks, OpenAI Responses input checks, invalid JSON rejection, invalid regex rejection, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai_prompt_guard ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewAIPromptGuard' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `plugin.New("ai-prompt-guard")` were undefined.

- [x] **Step 3: Implement ai-prompt-guard plugin**

Implemented official plugin name, priority, schema, regex compilation in `PostInit`, empty/invalid body rejection, OpenAI Chat `messages` extraction, OpenAI Responses `instructions` / `input` extraction, default last-message filtering for non-Responses protocols, role filtering, allow-before-deny checks, and JSON rejection bodies.

- [x] **Step 4: Register plugin and update README**

Added `ai-prompt-guard` import/case and registry test; updated README AI section with OpenResty regex flag, full Anthropic/Bedrock/Embeddings/passthrough extraction, streaming, and provider-shaped deny response limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai_prompt_guard ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewAIPromptGuard' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 69: Implement `ai-prompt-template`

**Files:**
- Create: `pkg/plugin/ai_prompt_template/plugin.go`
- Create: `pkg/plugin/ai_prompt_template/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with named `templates`, each containing a `template` object with optional `model` and `messages`; request JSON body with `template_name` and template variable fields.
- Produces: `plugin.New("ai-prompt-template")`, official priority/schema/name, selected template rendering, top-level `{{field}}` substitution, OpenAI Chat-style request body replacement, request body replay, and official missing/unknown template error responses.

- [x] **Step 1: Write failing tests**

Tests cover selected template rendering, missing `template_name`, unknown template name, invalid JSON rejection, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai_prompt_template ./pkg/plugin -run 'TestHandler|TestNewAIPromptTemplate' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `NamedTemplate`, `Template`, `Message`, and `plugin.New("ai-prompt-template")` were undefined.

- [x] **Step 3: Implement ai-prompt-template plugin**

Implemented official plugin name, priority, schema, JSON body parsing, template lookup by `template_name`, top-level field flattening, `{{field}}` substitution, rendered OpenAI Chat-style body replacement, request body replay, content-length update, and JSON error responses for empty/invalid/missing/unknown template cases.

- [x] **Step 4: Register plugin and update README**

Added `ai-prompt-template` import/case and registry test; updated README AI section with full `body-transformer` / `lua-resty-template`, nested variable, XML/form/multipart input, Anthropic/Responses-native output, LRU cache, and real `ai-proxy` provider transport limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai_prompt_template ./pkg/plugin -run 'TestHandler|TestNewAIPromptTemplate' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 70: Implement `ai-request-rewrite`

**Files:**
- Create: `pkg/plugin/ai_request_rewrite/plugin.go`
- Create: `pkg/plugin/ai_request_rewrite/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `prompt`, `provider`, `auth`, `options`, `override.endpoint`, timeout, keepalive, and `ssl_verify`; request bodies to rewrite.
- Produces: `plugin.New("ai-request-rewrite")`, official priority/schema/name, OpenAI Chat-compatible sidecar LLM request construction, configured auth headers/query parameters, non-streaming `choices[].message.content` extraction, request body replacement, and request body replay.

- [x] **Step 1: Write failing tests**

Tests cover OpenAI-compatible provider request construction, configured auth header/query propagation, LLM response text replacement, missing request body rejection, LLM non-200 rejection, openai-compatible endpoint validation, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai_request_rewrite ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewAIRequestRewrite' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Auth`, `Override`, and `plugin.New("ai-request-rewrite")` were undefined.

- [x] **Step 3: Implement ai-request-rewrite plugin**

Implemented official plugin name, priority, schema, defaults, OpenAI-compatible endpoint validation, OpenAI Chat sidecar request body construction from configured prompt and original request body, option overlay, auth header/query propagation, provider HTTP call, non-streaming response text extraction, request body rewrite/replay, content-length update, and JSON error responses.

- [x] **Step 4: Register plugin and update README**

Added `ai-request-rewrite` import/case and registry test; updated README AI section with APISIX AI provider/protocol registry, protocol conversion, streaming response, non-OpenAI-native provider, Azure OpenAI model omission, token usage variable, provider response filter, and fallback/error-response policy limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai_request_rewrite ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewAIRequestRewrite' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 71: Implement `ai-rate-limiting`

**Files:**
- Create: `pkg/plugin/ai_rate_limiting/plugin.go`
- Create: `pkg/plugin/ai_rate_limiting/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `limit`, `time_window`, `show_limit_quota_header`, `limit_strategy`, `instances`, `rejected_code`, and `rejected_msg`; non-streaming JSON LLM responses with `usage`.
- Produces: `plugin.New("ai-rate-limiting")`, official priority/schema/name, local token quota windows, global and per-instance quota tracking, AI rate-limit headers, response-usage accounting, and request-context helpers for future AI instance selection.

- [x] **Step 1: Write failing tests**

Tests cover total-token charging and next-request rejection, custom rejection message/status, per-instance `prompt_tokens` accounting, unconfigured-instance pass-through, reset after the time window, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai_rate_limiting ./pkg/plugin -run 'TestHandler|TestNewAIRateLimiting' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `InstanceLimit`, `WithPickedAIInstanceName`, and `plugin.New("ai-rate-limiting")` were undefined.

- [x] **Step 3: Implement ai-rate-limiting plugin**

Implemented official plugin name, priority, schema, local in-memory token windows, global quota fallback, per-instance quota lookup from request context, non-streaming JSON `usage` parsing, `total_tokens` / `prompt_tokens` / `completion_tokens` strategies, AI quota headers, reset handling, custom rejection status/body, and pass-through for unconfigured instances.

- [x] **Step 4: Register plugin and update README**

Added `ai-rate-limiting` import/case and registry test; updated README AI section with `limit-count` policy sharing, `rules`, Lua expression, Redis, exact log-phase accounting, streaming usage, automatic AI instance selection, and fallback-to-next-instance limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai_rate_limiting ./pkg/plugin -run 'TestHandler|TestNewAIRateLimiting' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 72: Implement `ai-proxy`

**Files:**
- Create: `pkg/plugin/ai_proxy/plugin.go`
- Create: `pkg/plugin/ai_proxy/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `provider`, `auth`, `options`, `override.endpoint`, `override.llm_options`, timeout, request/response size limits, keepalive, and `ssl_verify`; JSON OpenAI Chat-compatible request bodies.
- Produces: `plugin.New("ai-proxy")`, official priority/schema/name, direct non-streaming LLM proxying, configured auth header/query propagation, configured model option overlay, provider response forwarding, and provider endpoint validation.

- [x] **Step 1: Write failing tests**

Tests cover OpenAI-compatible proxy request construction, auth header/query propagation, configured option overwrite, provider status/header/body forwarding, next-handler bypass, oversized request rejection, non-JSON rejection, openai-compatible endpoint validation, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai_proxy ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewAIProxy' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Auth`, `Override`, and `plugin.New("ai-proxy")` were undefined.

- [x] **Step 3: Implement ai-proxy plugin**

Implemented official plugin name, priority, schema, defaults, JSON content-type and body-size validation, provider endpoint resolution, OpenAI Chat-compatible request body option overlay, `override.llm_options.max_tokens` mapping, auth header/query forwarding, provider HTTP call, response size cap, and provider status/header/body forwarding without calling the next handler.

- [x] **Step 4: Register plugin and update README**

Added `ai-proxy` import/case and registry test; updated README AI section with AI protocol conversion, Responses/Embeddings routing, native provider request construction, AWS/GCP auth, streaming, request-body override merge, logging, active metrics, `ai-proxy-multi`, and lower-priority phase wrapping limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai_proxy ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewAIProxy' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 73: Implement `ai-proxy-multi`

**Files:**
- Create: `pkg/plugin/ai_proxy_multi/plugin.go`
- Create: `pkg/plugin/ai_proxy_multi/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `instances`, `balancer`, `fallback_strategy`, `max_retries`, `retry_on_failure_within_ms`, timeout, request/response size limits, keepalive, and `ssl_verify`; JSON OpenAI Chat-compatible request bodies.
- Produces: `plugin.New("ai-proxy-multi")`, official priority/schema/name, direct non-streaming LLM proxying, weighted round-robin and simple chash instance selection, configured auth header/query propagation, configured model option overlay, HTTP status fallback, provider response forwarding, and provider endpoint validation.

- [x] **Step 1: Write failing tests**

Tests cover weighted round-robin across two instances, `http_5xx` fallback to the next instance, header-based chash stickiness, oversized request rejection, openai-compatible endpoint validation, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai_proxy_multi ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewAIProxyMulti' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Balancer`, `Instance`, `Auth`, `Override`, and `plugin.New("ai-proxy-multi")` were undefined.

- [x] **Step 3: Implement ai-proxy-multi plugin**

Implemented official plugin name, priority, schema, defaults, JSON content-type and body-size validation, per-instance endpoint validation, weighted round-robin picker, header/cookie/basic-var chash picker, `fallback_strategy` handling for `http_429` and `http_5xx`, `max_retries`, `retry_on_failure_within_ms`, OpenAI Chat-compatible request body option overlay, `override.llm_options.max_tokens` mapping, auth header/query forwarding, provider HTTP calls, response size cap, and provider status/header/body forwarding without calling the next handler.

- [x] **Step 4: Register plugin and update README**

Added `ai-proxy-multi` import/case and registry test; updated README AI section with health check, DNS node resolution, priority balancer, AI rate-limiting fallback integration, AI protocol conversion, native provider request construction, AWS/GCP auth, streaming, request-body override merge, logging, active metrics, and lower-priority phase wrapping limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai_proxy_multi ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewAIProxyMulti' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 74: Implement `ai-aws-content-moderation`

**Files:**
- Create: `pkg/plugin/ai_aws_content_moderation/plugin.go`
- Create: `pkg/plugin/ai_aws_content_moderation/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `comprehend`, `moderation_categories`, and `moderation_threshold`; request bodies to check before LLM proxying.
- Produces: `plugin.New("ai-aws-content-moderation")`, official priority/schema/name, AWS Comprehend `DetectToxicContent` request construction, SigV4 signing, toxicity/category threshold evaluation, original request body replay, and JSON rejection responses.

- [x] **Step 1: Write failing tests**

Tests cover signed Comprehend request construction, original request body preservation, toxicity threshold rejection, category threshold rejection, invalid moderation response error handling, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai_aws_content_moderation ./pkg/plugin -run 'TestHandler|TestNewAIAWSContentModeration' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `Comprehend`, and `plugin.New("ai-aws-content-moderation")` were undefined.

- [x] **Step 3: Implement ai-aws-content-moderation plugin**

Implemented official plugin name, priority, schema, defaults, AWS Comprehend endpoint selection, JSON `DetectToxicContent` request body construction, dependency-free SigV4 signing, `comprehend.ssl_verify`, moderation service response decoding, category threshold checks, toxicity threshold checks, request body replay, and JSON error responses.

- [x] **Step 4: Register plugin and update README**

Added `ai-aws-content-moderation` import/case and registry test; updated README AI section with OpenResty AWS SDK credential-provider, `session_token`, response moderation, streaming moderation, provider-compatible deny response, AI protocol extraction, log variable, and encrypted secret storage limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai_aws_content_moderation ./pkg/plugin -run 'TestHandler|TestNewAIAWSContentModeration' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 75: Implement `ai-aliyun-content-moderation`

**Files:**
- Create: `pkg/plugin/ai_aliyun_content_moderation/plugin.go`
- Create: `pkg/plugin/ai_aliyun_content_moderation/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with Aliyun endpoint/region/credentials, request and response check flags, moderation services, length limits, risk bar, deny status/message, timeout, keepalive, and `ssl_verify`; JSON AI request bodies.
- Produces: `plugin.New("ai-aliyun-content-moderation")`, official priority/schema/name, Aliyun `TextModerationPlus` form request construction, HMAC-SHA1 signing, basic OpenAI Chat/Responses-style request content extraction, risk-level threshold evaluation, original request body replay, pass-through on moderation service errors, and JSON rejection responses.

- [x] **Step 1: Write failing tests**

Tests cover signed Aliyun form request construction, OpenAI Chat content extraction, original request body preservation, risk-level rejection at the configured bar, configured deny messages, `check_request=false`, moderation-service error pass-through, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai_aliyun_content_moderation ./pkg/plugin -run 'TestHandler|TestNewAIAliyunContentModeration' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `plugin.New("ai-aliyun-content-moderation")` were undefined.

- [x] **Step 3: Implement ai-aliyun-content-moderation plugin**

Implemented official plugin name, priority, schema, defaults, JSON content-type validation, request body replay, OpenAI Chat/Responses-style content extraction, chunking by `request_check_length_limit`, Aliyun form parameter construction, HMAC-SHA1 signature calculation, `TextModerationPlus` HTTP call, risk-level comparison, APISIX-like pass-through on moderation service errors, deny code/message handling, timeout, keepalive, and `ssl_verify`.

- [x] **Step 4: Register plugin and update README**

Added `ai-aliyun-content-moderation` import/case and registry test; updated README AI section with picked-instance, full AI protocol registry extraction, response moderation, streaming moderation, provider-compatible deny response, SSE risk annotation, log variable, and encrypted secret storage limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai_aliyun_content_moderation ./pkg/plugin -run 'TestHandler|TestNewAIAliyunContentModeration' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 76: Implement `ai-rag`

**Files:**
- Create: `pkg/plugin/ai_rag/plugin.go`
- Create: `pkg/plugin/ai_rag/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with Azure OpenAI embeddings endpoint/API key, Azure AI Search endpoint/API key, and `ssl_verify`; JSON request bodies with `ai_rag.embeddings.input` and `ai_rag.vector_search.fields`.
- Produces: `plugin.New("ai-rag")`, official priority/schema/name, Azure OpenAI embeddings requests, Azure AI Search vector query requests, provider status/body propagation, `ai_rag` request-body removal, OpenAI Chat message append, OpenAI Responses input append, and request body replay.

- [x] **Step 1: Write failing tests**

Tests cover Azure OpenAI embedding body/API-key forwarding, Azure AI Search vector query construction, Chat and Responses request-body mutation, missing `ai_rag` rejection, provider failure propagation, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai_rag ./pkg/plugin -run 'TestHandler|TestNewAIRAG' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `EmbeddingsProvider`, `AzureProvider`, and `plugin.New("ai-rag")` were undefined.

- [x] **Step 3: Implement ai-rag plugin**

Implemented official plugin name, priority, schema, `ssl_verify`, Azure OpenAI embeddings request forwarding, first embedding extraction from `data[].embedding`, Azure AI Search vector query construction, provider error propagation, `ai_rag` removal, Chat message append, Responses input append, content-length update, and request body replay.

- [x] **Step 4: Register plugin and update README**

Added `ai-rag` import/case and registry test; updated README AI section with supported Azure-only RAG behavior and unsupported provider/protocol/streaming gaps.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai_rag ./pkg/plugin -run 'TestHandler|TestNewAIRAG' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 77: Implement `http-dubbo`

**Files:**
- Create: `pkg/plugin/http_dubbo/plugin.go`
- Create: `pkg/plugin/http_dubbo/plugin_test.go`
- Create: `pkg/route/http_dubbo_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `pkg/route/builder.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with Dubbo service/method/parameter serialization settings and route upstream nodes.
- Produces: `plugin.New("http-dubbo")`, official priority/schema/name, request-context config handoff, route-upstream TCP dialing, Dubbo 2.x fastjson request frame construction, serialized-body passthrough, response status/body parsing, and HTTP response termination.

- [x] **Step 1: Write failing tests**

Tests cover plugin context handoff, Dubbo frame header/payload construction, generic invocation parameter serialization, `serialization_header_key` override behavior, fake TCP Dubbo response handling, TCP failure handling, registry/schema validation, and route-upstream target selection.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/http_dubbo ./pkg/plugin ./pkg/route -run 'TestHandler|TestBuildDubbo|TestServeDubbo|TestNewHTTPDubbo|TestServeHTTPDubbo' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `GetConfig`, `buildDubboRequest`, `plugin.New("http-dubbo")`, and route `serveHTTPDubboIfConfigured` were undefined.

- [x] **Step 3: Implement http-dubbo plugin**

Implemented official plugin name, priority, schema, defaults, request-context config handoff, Dubbo 2.x frame construction, JSON-array and object parameter serialization, pre-serialized body passthrough with header override, route-upstream TCP dialing, send/read/connect timeout handling, Dubbo response header/status parsing, and HTTP 200 body mapping for application responses.

- [x] **Step 4: Register plugin and update README**

Added `http-dubbo` import/case and registry test; added route terminal integration for configured `http-dubbo` requests; updated README and missing-inventory roadmap notes with supported behavior and APISIX phase/runtime limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/http_dubbo ./pkg/plugin ./pkg/route -run 'TestHandler|TestBuildDubbo|TestServeDubbo|TestNewHTTPDubbo|TestServeHTTPDubbo' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 78: Implement `dingtalk-auth`

**Files:**
- Create: `pkg/plugin/dingtalk_auth/plugin.go`
- Create: `pkg/plugin/dingtalk_auth/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with DingTalk app credentials, redirect URI, code extraction names, DingTalk endpoint overrides, cookie/session options, timeout, and `ssl_verify`.
- Produces: `plugin.New("dingtalk-auth")`, official priority/schema/name, no-code redirect, DingTalk token/userinfo HTTP calls, local signed session cookie, access-token cache, `X-Userinfo` header forwarding, and request pass-through after successful authentication.

- [x] **Step 1: Write failing tests**

Tests cover no-code redirect, code exchange and userinfo POST construction, session cookie creation and reuse, Base64 `X-Userinfo`, invalid code rejection, access-token caching, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/dingtalk_auth ./pkg/plugin -run 'TestHandler|TestNewDingTalkAuth' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `plugin.New("dingtalk-auth")` were undefined.

- [x] **Step 3: Implement dingtalk-auth plugin**

Implemented official plugin name, priority, schema, defaults, configurable code header/query extraction, no-code redirect, DingTalk access token POST, DingTalk userinfo POST, 7000-second access-token cache, signed local `dingtalk_session` cookie with `secret_fallbacks`, `cookie_expires_in`, spoofed `X-Userinfo` clearing, Base64 JSON `X-Userinfo` forwarding, timeout, and `ssl_verify`.

- [x] **Step 4: Register plugin and update README**

Added `dingtalk-auth` import/case and registry test; updated README auth section and missing-inventory roadmap notes with supported behavior and resty-session/encrypted-field limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/dingtalk_auth ./pkg/plugin -run 'TestHandler|TestNewDingTalkAuth' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 79: Implement `feishu-auth`

**Files:**
- Create: `pkg/plugin/feishu_auth/plugin.go`
- Create: `pkg/plugin/feishu_auth/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with Feishu app credentials, auth redirect URI, client redirect URI, code extraction names, Feishu endpoint overrides, cookie/session options, timeout, and `ssl_verify`.
- Produces: `plugin.New("feishu-auth")`, official priority/schema/name, no-code redirect, Feishu token/userinfo HTTP calls, local signed session cookie, `X-Userinfo` header forwarding, and request pass-through after successful authentication.

- [x] **Step 1: Write failing tests**

Tests cover no-code redirect, code exchange and userinfo GET construction, session cookie creation and reuse, Base64 `X-Userinfo`, invalid code rejection, failed userinfo rejection, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/feishu_auth ./pkg/plugin -run 'TestHandler|TestNewFeishuAuth' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `plugin.New("feishu-auth")` were undefined.

- [x] **Step 3: Implement feishu-auth plugin**

Implemented official plugin name, priority, schema, defaults, configurable code header/query extraction, no-code redirect, Feishu OAuth token POST, Feishu userinfo GET with Bearer token, signed local `feishu_session` cookie with `secret_fallbacks`, `cookie_expires_in`, spoofed `X-Userinfo` clearing, Base64 JSON `X-Userinfo` forwarding, timeout, and `ssl_verify`.

- [x] **Step 4: Register plugin and update README**

Added `feishu-auth` import/case and registry test; updated README auth section and missing-inventory roadmap notes with supported behavior and resty-session/encrypted-field/token-reuse limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/feishu_auth ./pkg/plugin -run 'TestHandler|TestNewFeishuAuth' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 80: Implement `saml-auth`

**Files:**
- Create: `pkg/plugin/saml_auth/plugin.go`
- Create: `pkg/plugin/saml_auth/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: route/service plugin config with SP issuer/certificate/private key, IdP SSO URI/certificate, login/logout callback URIs, logout redirect URI, authentication binding method, secret, and `secret_fallbacks`.
- Produces: `plugin.New("saml-auth")`, official priority/schema/name, SAML AuthnRequest generation for HTTP-Redirect and HTTP-POST, ACS SAMLResponse validation through `github.com/crewjam/saml`, signed local SAML session cookies, local external-user attachment, and SP-initiated logout redirect.

- [x] **Step 1: Write failing tests**

Tests cover unauthenticated IDP redirect with SAMLRequest/RelayState, HTTP-POST form binding, signed local session pass-through with `X-Userinfo`, SP-initiated logout request and session cleanup, invalid SAMLResponse rejection, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/saml_auth ./pkg/plugin -run 'TestUnauthenticated|TestHTTPPost|TestExistingSession|TestLogout|TestInvalidSAML|TestNewSAMLAuth' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, request/session helpers, and `plugin.New("saml-auth")` were undefined.

- [x] **Step 3: Implement saml-auth plugin**

Implemented official plugin name, priority, schema, defaults, SP certificate/private-key parsing, configured IdP metadata construction from `idp_uri` / `idp_cert`, HTTP-Redirect and HTTP-POST AuthnRequest generation, signed request-state cookies, ACS `SAMLResponse` parsing and validation via `github.com/crewjam/saml`, signed local SAML session cookies with `secret_fallbacks`, Base64 JSON `X-Userinfo`, local `$external_user` attachment when APISIX vars exist, SP-initiated logout redirect, and logout callback cleanup.

- [x] **Step 4: Register plugin and update README**

Added `saml-auth` import/case and registry test; updated README auth section and missing-inventory roadmap notes with supported behavior and resty-session/metadata/artifact/IdP-initiated limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/saml_auth ./pkg/plugin -run 'TestUnauthenticated|TestHTTPPost|TestExistingSession|TestLogout|TestInvalidSAML|TestNewSAMLAuth' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 81: Implement `mcp-bridge`

**Files:**
- Create: `pkg/plugin/mcp_bridge/plugin.go`
- Create: `pkg/plugin/mcp_bridge/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service plugin config with `base_uri`, `command`, and `args`.
- Produces: `plugin.New("mcp-bridge")`, official priority/schema/name, `GET {base_uri}/sse` SSE endpoint, subprocess stdio lifecycle, initial endpoint advertisement, `POST {base_uri}/message?sessionId=...` request body forwarding to subprocess stdin, stdout SSE forwarding, stderr notification forwarding, and subprocess cleanup on SSE disconnect.

- [x] **Step 1: Write failing tests**

Tests cover SSE endpoint headers and initial `endpoint` event, stdout line forwarding, POST message forwarding to session stdin, stderr forwarding as `notifications/stderr`, unknown session rejection, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/mcp_bridge ./pkg/plugin -run 'TestSSE|TestMessageEndpoint|TestStderr|TestNewMCPBridge' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, and `plugin.New("mcp-bridge")` were undefined.

- [x] **Step 3: Implement mcp-bridge plugin**

Implemented official plugin name, priority, schema, base URI matching, per-session subprocess launch, SSE `endpoint` advertisement, stdout line forwarding as SSE `message`, stderr forwarding as JSON-RPC `notifications/stderr`, POST message body writes to subprocess stdin, session map lookup, and subprocess/session cleanup.

- [x] **Step 4: Register plugin and update README**

Added `mcp-bridge` import/case and registry test; updated README AI/MCP section and missing-inventory roadmap notes with supported behavior and shared-dict/cross-worker/ping/partial-line limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/mcp_bridge ./pkg/plugin -run 'TestSSE|TestMessageEndpoint|TestStderr|TestNewMCPBridge' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 82: Implement `ai`

**Files:**
- Create: `pkg/plugin/ai/plugin.go`
- Create: `pkg/plugin/ai/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: empty global plugin config.
- Produces: `plugin.New("ai")`, official priority/schema/name, and pass-through compatibility registration.

- [x] **Step 1: Write failing tests**

Tests cover pass-through middleware behavior and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/ai ./pkg/plugin -run 'TestHandler|TestNewAI' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Plugin` was undefined and `plugin.New("ai")` returned nil.

- [x] **Step 3: Implement ai plugin**

Implemented official plugin name, priority, empty schema, config object, and pass-through middleware compatibility behavior.

- [x] **Step 4: Register plugin and update README**

Added `ai` import/case and registry test; updated README AI section and missing-inventory roadmap notes with supported registration behavior and router/upstream/balancer optimization limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/ai ./pkg/plugin -run 'TestHandler|TestNewAI' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 83: Implement `example-plugin`

**Files:**
- Create: `pkg/plugin/example_plugin/plugin.go`
- Create: `pkg/plugin/example_plugin/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: demo plugin config with required `i`, optional `s`, `t`, `ip`, and `port`.
- Produces: `plugin.New("example-plugin")`, official priority/schema/name, pass-through middleware, optional upstream override through the Go traffic-split override context when `ip` is configured, and control API `GET /v1/plugin/example-plugin/hello`.

- [x] **Step 1: Write failing tests**

Tests cover pass-through behavior, `ip` / `port` upstream override, control API text response, control API JSON response, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/example_plugin ./pkg/plugin -run 'TestHandler|TestControlAPI|TestNewExamplePlugin' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Config`, `Plugin`, `name`, and `plugin.New("example-plugin")` were undefined.

- [x] **Step 3: Implement example-plugin**

Implemented official plugin name, priority, schema, config object, pass-through middleware, optional route-upstream override through `traffic_split.WithOverride`, and `GET /v1/plugin/example-plugin/hello` text/JSON public API registration.

- [x] **Step 4: Register plugin and update README**

Added `example-plugin` import/case and registry test; added README Development section and removed the final top-level missing-plugin inventory entry.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/example_plugin ./pkg/plugin -run 'TestHandler|TestControlAPI|TestNewExamplePlugin' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 84: Implement `prometheus`

**Files:**
- Create: `pkg/plugin/prometheus/plugin.go`
- Create: `pkg/plugin/prometheus/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: existing Go Prometheus registry, `public-api` registry, and route/global plugin config with `prefer_name`.
- Produces: `plugin.New("prometheus")`, official priority/schema/name, pass-through middleware, and public API registration for `GET /apisix/prometheus/metrics`.

- [x] **Step 1: Write failing tests**

Tests cover pass-through middleware, public API metrics endpoint registration, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/prometheus ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewPrometheus' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Plugin`, `MetricsURI`, and `plugin.New("prometheus")` were undefined.

- [x] **Step 3: Implement prometheus**

Implemented official plugin name, priority, schema, config object, pass-through middleware, and `GET /apisix/prometheus/metrics` public API registration using `promhttp.Handler()`.

- [x] **Step 4: Register plugin and update README**

Added `prometheus` import/case and registry test; updated README support notes with public API/export-server support and exporter parity limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/prometheus ./pkg/plugin -run 'TestHandler|TestPostInit|TestNewPrometheus' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 85: Implement `mqtt-proxy` Config Facade

**Files:**
- Create: `pkg/plugin/mqtt_proxy/plugin.go`
- Create: `pkg/plugin/mqtt_proxy/plugin_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: stream-plugin config shape with `protocol_name` and required `protocol_level`.
- Produces: `plugin.New("mqtt-proxy")`, official priority/schema/name, default `protocol_name`, and no-op HTTP middleware because this Go runtime has no stream-route plugin interface.

- [x] **Step 1: Write failing tests**

Tests cover pass-through middleware, default `protocol_name`, official schema validation, and registry/schema validation.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/mqtt_proxy ./pkg/plugin -run 'TestHandler|TestPostInitFillsDefaultProtocolName|TestSchemaValidatesOfficialConfig|TestNewMQTTProxy' -count=1 -timeout=10s -v`

Observed: fail before implementation because `Plugin` and `plugin.New("mqtt-proxy")` were undefined.

- [x] **Step 3: Implement mqtt-proxy config facade**

Implemented official plugin name, priority, schema, config object, default `protocol_name = "MQTT"`, and pass-through middleware.

- [x] **Step 4: Register plugin and update README**

Added `mqtt_proxy` import/case and registry test; added README Stream section with stream-route/L4 limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/mqtt_proxy ./pkg/plugin -run 'TestHandler|TestPostInitFillsDefaultProtocolName|TestSchemaValidatesOfficialConfig|TestNewMQTTProxy' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 86: Implement Prometheus `prefer_name` Metric Labels

**Files:**
- Modify: `pkg/plugin/request_context/plugin.go`
- Create: `pkg/plugin/request_context/plugin_test.go`
- Modify: `pkg/route/builder.go`
- Create: `pkg/route/prometheus_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route/service identity values in the synthetic `request-context` plugin and route-level `prometheus.prefer_name` config.
- Produces: Prometheus route/service metric labels that use route/service IDs by default and route/service names when `prefer_name` is true.

- [x] **Step 1: Write failing tests**

Tests cover default ID labels, `prefer_name` name labels, fallback when IDs are unavailable, and route-builder propagation of `prometheus.prefer_name`.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/request_context ./pkg/route -run 'TestMetricLabels|TestBuildRequestContextConfig' -count=1 -timeout=10s -v`

Observed: fail before implementation because `metricLabels`, `PrometheusPreferName`, and `buildRequestContextConfig` were undefined.

- [x] **Step 3: Implement label selection and builder propagation**

Implemented `request-context` label selection, changed metrics recording to use selected labels, and passed route-level `prometheus.prefer_name` into the synthetic request-context config.

- [x] **Step 4: Update README**

Updated the Prometheus support notes to include ID-vs-name label behavior and remove the previous `prefer_name` limitation.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/request_context ./pkg/route -run 'TestMetricLabels|TestBuildRequestContextConfig' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 87: Implement Prometheus Official Export Server Plugin Attrs

**Files:**
- Modify: `pkg/server/server.go`
- Create: `pkg/server/server_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin_attr.prometheus.export_uri`, `plugin_attr.prometheus.enable_export_server`, and nested `plugin_attr.prometheus.export_addr.ip` / `port`.
- Produces: dedicated Prometheus export server config that honors the official APISIX 3.17 plugin attr shape.

- [x] **Step 1: Write failing tests**

Tests cover default export server config and official nested plugin attr overrides.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/server -run 'TestPrometheusExportServerConfig' -count=1 -timeout=10s -v`

Observed: fail before implementation because `newPrometheusExportServerConfig` was undefined.

- [x] **Step 3: Implement export server config helper**

Implemented default config, official `enable_export_server`, `export_uri`, nested `export_addr`, and legacy flat `export_ip` / `export_port` fallback parsing.

- [x] **Step 4: Use helper in server startup and update README**

Changed server startup to skip the export server when disabled and to listen on the helper-derived official address/URI; updated README support notes.

- [x] **Step 5: Verify**

Run: `go test ./pkg/server -run 'TestPrometheusExportServerConfig' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 88: Implement Prometheus Metric Prefix And Bucket Plugin Attr Parsing

**Files:**
- Modify: `pkg/observability/metrics/prometheus.go`
- Create: `pkg/observability/metrics/prometheus_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `plugin_attr.prometheus.metric_prefix` and `plugin_attr.prometheus.default_buckets`.
- Produces: Prometheus metric names with the configured prefix and HTTP latency histograms with configured default buckets.

- [x] **Step 1: Write failing tests**

Tests cover default config, official YAML-style numeric bucket values, custom metric prefix, and invalid bucket fallback.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/observability/metrics -run 'TestPrometheusMetricConfig' -count=1 -timeout=10s -v`

Observed: fail before implementation because `newPrometheusMetricConfig` and `defaultLatencyBuckets` were undefined.

- [x] **Step 3: Implement metric attr parser**

Implemented default metric config, robust `metric_prefix` parsing, and `default_buckets` parsing for `[]float64` and YAML/Viper-style `[]interface{}` numeric values.

- [x] **Step 4: Update README**

Updated Prometheus support notes to include `metric_prefix` and `default_buckets`.

- [x] **Step 5: Verify**

Run: `go test ./pkg/observability/metrics -run 'TestPrometheusMetricConfig' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 89: Implement Datadog Route, Service, Consumer, And Balancer Tags

**Files:**
- Modify: `pkg/plugin/datadog/plugin.go`
- Modify: `pkg/plugin/datadog/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: APISIX request vars `$route_id`, `$route_name`, `$service_id`, `$service_name`, `$consumer_name`, `$consumer`, and `$balancer_ip`.
- Produces: DogStatsD tags `route_name`, `service_name`, `consumer`, and `balancer_ip` with APISIX-compatible `prefer_name` behavior.

- [x] **Step 1: Write failing tests**

Tests cover explicit `prefer_name=false`, APISIX resource tags, ID fallback when `prefer_name=false`, and handler extraction from request vars.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/datadog -run 'TestPostInitPreservesExplicitPreferNameFalse|TestGenerateTagsIncludesAPISIXResourceTags|TestGenerateTagsPreferNameFalseUsesIDs|TestHandlerCapturesAPISIXResourceTags' -count=1 -timeout=10s -v`

Observed: fail before implementation because `metricEntry` lacked APISIX resource fields and `Config` could not preserve explicit `prefer_name=false`.

- [x] **Step 3: Implement tag extraction**

Implemented JSON config decoding that preserves explicit `prefer_name=false`, route/service ID-vs-name selection, consumer tag extraction, and balancer IP tag extraction.

- [x] **Step 4: Update README**

Updated Datadog support notes to include route/service/consumer/balancer tags and narrowed the remaining limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/datadog -run 'TestPostInitPreservesExplicitPreferNameFalse|TestGenerateTagsIncludesAPISIXResourceTags|TestGenerateTagsPreferNameFalseUsesIDs|TestHandlerCapturesAPISIXResourceTags' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 90: Implement Datadog Matched Route Path Tags

**Files:**
- Modify: `pkg/plugin/request_context/plugin.go`
- Modify: `pkg/route/builder.go`
- Modify: `pkg/route/prometheus_test.go`
- Modify: `pkg/plugin/datadog/plugin.go`
- Modify: `pkg/plugin/datadog/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: route `uri` / `uris` and synthetic `request-context` APISIX vars.
- Produces: `$matched_uri` request var and Datadog `path` tag using the matched route pattern when `include_path` is true.

- [x] **Step 1: Write failing tests**

Tests cover route-builder propagation of `$matched_uri` and Datadog path tag preference for `$matched_uri` over the raw request URL path.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/route ./pkg/plugin/datadog -run 'TestBuildRequestContextConfigPassesPrometheusPreferName|TestHandlerUsesMatchedURIForPathTag' -count=1 -timeout=10s -v`

Observed: fail before implementation because `$matched_uri` was absent and Datadog emitted the raw request path.

- [x] **Step 3: Implement matched URI propagation**

Added `$matched_uri` to request-context config and APISIX vars, selected route `uri` or first `uris` as the matched pattern, and made Datadog prefer `$matched_uri` for `include_path`.

- [x] **Step 4: Update README**

Updated Datadog support notes to say `include_path` emits the matched route pattern.

- [x] **Step 5: Verify**

Run: `go test ./pkg/route ./pkg/plugin/datadog -run 'TestBuildRequestContextConfigPassesPrometheusPreferName|TestHandlerUsesMatchedURIForPathTag' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 91: Implement Datadog Upstream Latency Metric

**Files:**
- Modify: `pkg/route/builder.go`
- Modify: `pkg/route/proxy_control_test.go`
- Modify: `pkg/plugin/datadog/plugin.go`
- Modify: `pkg/plugin/datadog/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: reverse-proxy upstream request lifecycle and APISIX request vars.
- Produces: `$upstream_latency` request var and DogStatsD `upstream.latency` histogram metrics.

- [x] **Step 1: Write failing tests**

Tests cover `newModifyResponse` recording upstream latency and Datadog emitting `upstream.latency`.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/route ./pkg/plugin/datadog -run 'TestModifyResponseRecordsUpstreamLatency|TestHandlerCapturesUpstreamLatency' -count=1 -timeout=10s -v`

Observed: fail before implementation because upstream latency vars were undefined and Datadog emitted only the five base metrics.

- [x] **Step 3: Implement upstream latency recording**

Recorded upstream start time before proxy round-trip, stored upstream latency in request vars after upstream response, and made Datadog read `$upstream_latency`.

- [x] **Step 4: Update README**

Updated Datadog support notes to include upstream latency and narrowed the remaining limitation to batch/timing fidelity.

- [x] **Step 5: Verify**

Run: `go test ./pkg/route ./pkg/plugin/datadog -run 'TestModifyResponseRecordsUpstreamLatency|TestHandlerCapturesUpstreamLatency' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 92: Derive Datadog APISIX Latency From Upstream Latency

**Files:**
- Modify: `pkg/plugin/datadog/plugin.go`
- Modify: `pkg/plugin/datadog/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: request total latency and `$upstream_latency` request var.
- Produces: DogStatsD `apisix.latency` matching APISIX 3.17 `latency_details_in_ms` behavior: total latency minus upstream latency, clamped to zero.

- [x] **Step 1: Confirm official behavior**

Read official APISIX 3.17 `apisix/utils/log-util.lua`.

- [x] **Step 2: Write focused tests**

Tests cover no upstream latency, upstream subtraction, and negative-result clamping.

- [x] **Step 3: Implement APISIX latency derivation**

Added a helper to derive APISIX-side latency and used it when the Datadog handler builds the metric entry.

- [x] **Step 4: Update README**

Updated Datadog support notes to include APISIX-side latency derived from upstream latency.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/datadog -run 'TestApisixLatencySubtractsUpstreamLatency|TestHandlerCapturesUpstreamLatency' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 93: Implement `limit-count` Variable Key Resolution

**Files:**
- Modify: `pkg/plugin/limit_count/plugin.go`
- Create: `pkg/plugin/limit_count/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: APISIX/Nginx-style variables including `remote_addr`, `http_*`, `key_type = constant`, and `key_type = var_combination`.
- Produces: fixed-window quota buckets keyed like official `limit-count` for the supported local/Redis policies.

- [x] **Step 1: Write failing tests**

Tests cover HTTP header variables and variable-combination keys producing separate quota buckets.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/limit_count -run 'TestHandlerUsesHTTPVariableKey|TestHandlerUsesVariableCombinationKey' -count=1 -timeout=10s -v`

Observed: fail before implementation because both keys resolved to an empty string and collapsed requests into the same quota bucket.

- [x] **Step 3: Implement variable key resolver**

Implemented `var`, `constant`, and `var_combination` key resolution, HTTP header variable lookup, `remote_addr` host normalization, and removed per-request debug printing.

- [x] **Step 4: Update README**

Updated `limit-count` support notes to include supported key types and remaining gaps.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/limit_count -run 'TestHandlerUsesHTTPVariableKey|TestHandlerUsesVariableCombinationKey' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 94: Implement OpenTelemetry Sampler Config

**Files:**
- Modify: `pkg/plugin/otel/plugin.go`
- Create: `pkg/plugin/otel/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official `opentelemetry` route plugin `sampler` config.
- Produces: `otelchi` middleware using Go OpenTelemetry SDK samplers matching APISIX sampler names.

- [x] **Step 1: Read official behavior**

Read official APISIX 3.17 `apisix/plugins/opentelemetry.lua` sampler schema and sampler factory behavior.

- [x] **Step 2: Write failing tests**

Tests cover official default `always_off`, `always_on`, `trace_id_ratio`, and `parent_base` root sampler behavior.

- [x] **Step 3: Implement sampler config**

Added structured config, sampler defaults, sampler builder, configurable middleware server name, and removed the hardcoded debug print.

- [x] **Step 4: Update README**

Updated OpenTelemetry support notes to include sampler config support and remaining collector/attribute gaps.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/otel -run 'TestPostInitSetsSamplerDefaults|TestBuildSamplerUsesOfficialSamplerNames' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 95: Implement OpenTelemetry Additional Span Attributes

**Files:**
- Modify: `pkg/plugin/otel/plugin.go`
- Modify: `pkg/plugin/otel/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official `additional_attributes` and `additional_header_prefix_attributes` config.
- Produces: additional OpenTelemetry span attributes from APISIX/Nginx request variables and request headers.

- [x] **Step 1: Read official behavior**

Read official APISIX 3.17 `inject_attributes` behavior in `apisix/plugins/opentelemetry.lua`.

- [x] **Step 2: Write failing tests**

Tests cover request variable attributes, exact header attributes, wildcard header-prefix attributes, and skipping missing values.

- [x] **Step 3: Implement attribute extraction and injection**

Added deterministic attribute collection and injected attributes into the active OpenTelemetry span inside the existing `otelchi` middleware chain.

- [x] **Step 4: Update README**

Updated OpenTelemetry support notes to include additional span attributes and narrowed remaining gaps.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/otel -run 'TestPostInitSetsSamplerDefaults|TestBuildSamplerUsesOfficialSamplerNames|TestAdditionalSpanAttributesUseRequestVarsAndHeaders' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 96: Extend OpenTelemetry Additional Attributes To APISIX Vars

**Files:**
- Modify: `pkg/plugin/otel/plugin.go`
- Modify: `pkg/plugin/otel/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: APISIX-Go request context vars and request vars in addition to NGINX-style variables.
- Produces: OpenTelemetry span attributes for route/service/request timing variables configured through `additional_attributes`.

- [x] **Step 1: Write failing tests**

Tests cover `route_id`, `service_name`, and `$upstream_latency` attribute extraction from APISIX/request context.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/otel -run 'TestAdditionalSpanAttributesUseAPISIXAndRequestVars' -count=1 -timeout=10s -v`

Observed: fail before implementation because only NGINX variables were resolved.

- [x] **Step 3: Implement APISIX/request var resolution**

Added an attribute resolver that checks NGINX variables, APISIX vars, and request vars, then coerces scalar values to strings.

- [x] **Step 4: Update README**

Updated OpenTelemetry support notes to say `additional_attributes` can use NGINX/APISIX/request vars.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/otel -run 'TestPostInitSetsSamplerDefaults|TestBuildSamplerUsesOfficialSamplerNames|TestAdditionalSpanAttributesUseRequestVarsAndHeaders|TestAdditionalSpanAttributesUseAPISIXAndRequestVars' -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 97: Implement `proxy-cache` Consumer Isolation

**Files:**
- Modify: `pkg/plugin/proxy_cache/plugin.go`
- Modify: `pkg/plugin/proxy_cache/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: authenticated APISIX consumer context and `consumer_isolation` config.
- Produces: per-consumer proxy-cache key namespaces by default, with explicit `consumer_isolation = false` preserved.

- [x] **Step 1: Write failing tests**

Tests cover explicit `consumer_isolation = false` surviving defaults and separate cache buckets for different authenticated consumers.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestPostInitPreservesExplicitConsumerIsolationFalse|TestHandlerIsolatesCacheByConsumerByDefault' -count=1 -timeout=10s -v`

Observed: fail before implementation because explicit false was overwritten and different consumers shared the same cache key.

- [x] **Step 3: Implement consumer identity cache key prefixing**

Added explicit-false config decoding, official identity-variable detection, authenticated consumer key prefixing, and consumer variable lookup for cache keys.

- [x] **Step 4: Update README**

Updated `proxy-cache` support notes to include `consumer_isolation` and remove the stale unsupported note.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestPostInitPreservesExplicitConsumerIsolationFalse|TestHandlerIsolatesCacheByConsumerByDefault' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/proxy_cache -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 98: Implement `proxy-cache` Cache-Control Bypass Semantics

**Files:**
- Modify: `pkg/plugin/proxy_cache/plugin.go`
- Modify: `pkg/plugin/proxy_cache/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: request and upstream response `Cache-Control` headers.
- Produces: APISIX-style request bypass for `cache_control` requests with `no-cache` / `no-store`, and upstream response non-storage for `private` / `no-store` / `no-cache`.

- [x] **Step 1: Write failing tests**

Tests cover request `Cache-Control: no-cache` bypassing a stored entry and upstream response `no-store`, `private`, and `no-cache` directives preventing cache storage.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestHandlerCacheControlRequestNoCacheBypassesStoredEntry|TestHandlerCacheControlResponseDirectivesSkipStore' -count=1 -timeout=10s -v`

Observed: fail before implementation because request `no-cache` returned `HIT`, and upstream `no-store` / `private` / `no-cache` responses were stored and returned `HIT` on the second request.

- [x] **Step 3: Implement Cache-Control directive handling**

Added `Cache-Control` directive parsing, request-side `cache_control` bypass, and upstream response non-storage for personalized/non-storable directives.

- [x] **Step 4: Update README**

Updated `proxy-cache` support notes to include bounded `Cache-Control` support and keep full TTL, `Expires`, `only-if-cached`, stale, `Vary`, public purge, and disk cache limitations visible.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestHandlerCacheControlRequestNoCacheBypassesStoredEntry|TestHandlerCacheControlResponseDirectivesSkipStore' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/proxy_cache -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 99: Implement `proxy-cache` Cache-Control TTL Semantics

**Files:**
- Modify: `pkg/plugin/proxy_cache/plugin.go`
- Modify: `pkg/plugin/proxy_cache/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: request `Cache-Control: only-if-cached`, upstream `Cache-Control: s-maxage` / `max-age`, and upstream `Expires`.
- Produces: APISIX-style `504` on `only-if-cached` misses, positive upstream resource TTL derivation when `cache_control` is enabled, and non-storage for zero/missing/expired resource TTL.

- [x] **Step 1: Write failing tests**

Tests cover `only-if-cached` miss behavior, refusal to store missing/zero/expired resource TTL responses, and `max-age` overriding configured `cache_ttl`.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestHandlerCacheControlOnlyIfCachedMissReturnsGatewayTimeout|TestHandlerCacheControlRequiresPositiveResourceTTL|TestHandlerCacheControlUsesUpstreamMaxAgeTTL' -count=1 -timeout=10s -v`

Observed: fail before implementation because `only-if-cached` called upstream with 200, resource TTL-less responses were cached, and `max-age=1` still used the configured 60-second TTL.

- [x] **Step 3: Implement TTL and `only-if-cached` handling**

Added response TTL derivation from `s-maxage`, `max-age`, and final `Expires`, positive-TTL storage enforcement under `cache_control`, and `504` handling for `only-if-cached` misses.

- [x] **Step 4: Update README**

Updated `proxy-cache` support notes to include TTL derivation and `only-if-cached`, while keeping request stale controls, `Vary`, public purge, stale serving, and disk cache limitations visible.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestHandlerCacheControlOnlyIfCachedMissReturnsGatewayTimeout|TestHandlerCacheControlRequiresPositiveResourceTTL|TestHandlerCacheControlUsesUpstreamMaxAgeTTL' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/proxy_cache -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 100: Implement `proxy-cache` Request Freshness Controls

**Files:**
- Modify: `pkg/plugin/proxy_cache/plugin.go`
- Modify: `pkg/plugin/proxy_cache/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: request `Cache-Control: max-age`, `max-stale`, and `min-fresh` directives.
- Produces: APISIX-style `STALE` refresh behavior for readable cached entries whose age or remaining freshness violates the request directive.

- [x] **Step 1: Write failing tests**

Tests cover `max-age`, `max-stale`, and `min-fresh` requests forcing a cached entry refresh with `Apisix-Cache-Status: STALE`.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestHandlerCacheControlRequestFreshnessDirectivesForceStaleRefresh' -count=1 -timeout=10s -v`

Observed: fail before implementation because `cacheEntry` did not track `storedAt` or `ttl`, so lookup could not evaluate request freshness directives.

- [x] **Step 3: Implement request freshness handling**

Added cached-entry `storedAt` and `ttl` metadata, then evaluated request `max-age`, `max-stale`, and `min-fresh` for unexpired entries when `cache_control` is enabled.

- [x] **Step 4: Update README**

Updated `proxy-cache` support notes to include request stale refresh controls and narrow remaining gaps to disk cache zones, `Vary`, public purge, and stale serving.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestHandlerCacheControlRequestFreshnessDirectivesForceStaleRefresh' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/proxy_cache -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 101: Implement `proxy-cache` PURGE

**Files:**
- Modify: `pkg/plugin/proxy_cache/plugin.go`
- Modify: `pkg/plugin/proxy_cache/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: request method `PURGE` and the configured proxy-cache key.
- Produces: APISIX-style cache entry deletion with `200` for existing entries and `404` for misses, without forwarding the request upstream.

- [x] **Step 1: Write failing tests**

Tests cover purging an existing cached entry, ensuring the upstream is not called for `PURGE`, and returning `404` for a purge miss.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestHandlerPurge' -count=1 -timeout=10s -v`

Observed: fail before implementation because `purgeMethod` and `PURGE` handling were undefined.

- [x] **Step 3: Implement PURGE handling**

Added `PURGE` handling before normal cacheable method checks, deleting the matching in-memory cache entry regardless of expiry.

- [x] **Step 4: Update README**

Updated `proxy-cache` support notes to include `PURGE`, removed the stale top-level `PURGE` unsupported note, and narrowed remaining proxy-cache gaps to disk cache zones, `Vary`, and stale serving.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestHandlerPurge' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/proxy_cache -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 102: Implement `proxy-cache` Vary Variants

**Files:**
- Modify: `pkg/plugin/proxy_cache/plugin.go`
- Modify: `pkg/plugin/proxy_cache/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: upstream `Vary` response headers and request header values named by `Vary`.
- Produces: APISIX-style in-memory variant cache keys, `Vary: *` non-storage, and PURGE cleanup for variant entries.

- [x] **Step 1: Write failing tests**

Tests cover separate variants for `Accept-Language`, `Vary: *` skipping storage, and PURGE deleting variant entries.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestHandlerCachesVaryVariantsByRequestHeaders|TestHandlerVaryStarSkipsStore|TestHandlerPurgeRemovesVaryVariants' -count=1 -timeout=10s -v`

Observed: fail before implementation because request variants shared the base key, `Vary: *` was stored, and PURGE could not clear variant entries.

- [x] **Step 3: Implement Vary variant storage**

Added a bounded in-memory variant index, stable request-header signatures, `Vary: *` non-storage, prior-index cleanup when storing no-Vary responses, and PURGE cleanup across base and variant keys.

- [x] **Step 4: Update README**

Updated `proxy-cache` support notes to include in-memory `Vary` support and narrow remaining gaps to disk cache zones and stale serving.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_cache -run 'TestHandlerCachesVaryVariantsByRequestHeaders|TestHandlerVaryStarSkipsStore|TestHandlerPurgeRemovesVaryVariants' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/proxy_cache -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 103: Implement `gzip` Wildcard Types And Min Length

**Files:**
- Modify: `pkg/plugin/gzip/plugin.go`
- Modify: `pkg/plugin/gzip/compress.go`
- Create: `pkg/plugin/gzip/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `types = "*"`, `types = ["*"]`, `min_length`, `Content-Length`, and optional `vary`.
- Produces: APISIX-style wildcard content-type matching, `Content-Length` based minimum-size skipping, and safe default `vary = false` behavior.

- [x] **Step 1: Write failing tests**

Tests cover JSON-style `types: "*"`, wildcard compression for any content type, and skipping compression when `Content-Length` is below `min_length`.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/gzip -run 'TestPostInitAcceptsWildcardTypesString|TestHandlerSkipsSmallContentLength|TestHandlerWildcardTypesCompressesAnyContentType' -count=1 -timeout=10s -v`

Observed: fail before implementation because `types: "*"` could not parse, the handler panicked on nil `vary`, and small `Content-Length` responses were still considered for compression.

- [x] **Step 3: Implement gzip wildcard and min-length behavior**

Added custom config decoding for `types`, wildcard content-type matching, default `vary = false`, and `Content-Length` based `min_length` checks in the gzip response writer.

- [x] **Step 4: Update README**

Updated `gzip` support notes to include `types = "*"`, `min_length`, and the supported gzip config fields, leaving only NGINX-native `buffers` unsupported.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/gzip -run 'TestPostInitAcceptsWildcardTypesString|TestHandlerSkipsSmallContentLength|TestHandlerWildcardTypesCompressesAnyContentType' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/gzip -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 104: Implement `proxy-rewrite` Regex URI

**Files:**
- Modify: `pkg/plugin/proxy_rewrite/plugin.go`
- Create: `pkg/plugin/proxy_rewrite/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `regex_uri` pattern/replacement pairs and request path.
- Produces: APISIX-style first-match URI replacement with lower priority than explicit `uri`, carried through the existing route-builder `proxy-rewrite` context payload.

- [x] **Step 1: Write failing tests**

Tests cover regex-derived URI rewriting, first matching pair behavior, explicit `uri` priority, and odd-length `regex_uri` rejection.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./pkg/plugin/proxy_rewrite -run 'TestHandlerDerivesURIFromRegexURI|TestHandlerUsesFirstMatchingRegexURIPair|TestHandlerURIHasPriorityOverRegexURI|TestPostInitRejectsOddRegexURI' -count=1 -timeout=10s -v`

Observed: fail before implementation because `RegexURI` was missing from `Config`.

- [x] **Step 3: Implement regex_uri handling**

Added schema/config support, PostInit validation/compilation, first-match replacement using Go regexp captures, and explicit `uri` priority over `regex_uri`.

- [x] **Step 4: Update README**

Updated `proxy-rewrite` support notes to include `regex_uri`, leaving `use_real_request_uri_unsafe` as the remaining documented gap.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_rewrite -run 'TestHandlerDerivesURIFromRegexURI|TestHandlerUsesFirstMatchingRegexURIPair|TestHandlerURIHasPriorityOverRegexURI|TestPostInitRejectsOddRegexURI' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/proxy_rewrite -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 105: Implement `response-rewrite` Vars And Filters

**Files:**
- Modify: `pkg/plugin/response_rewrite/plugin.go`
- Modify: `pkg/plugin/response_rewrite/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `vars` response expression conditions, response body `filters`, and header values containing APISIX-style `$var` references.
- Produces: response rewrite gating by common request/response variables, once/global regexp body substitutions, and resolved header add/set values.

- [x] **Step 1: Read official behavior**

Read official APISIX 3.17 `apisix/plugins/response-rewrite.lua`; `vars` gates header/body rewrite, `body` and `filters` are mutually exclusive, `filters` run once or globally over the captured response body, and header values resolve APISIX variables.

- [x] **Step 2: Write failing tests**

Tests cover response-status `vars` skip/match behavior, header value variable resolution, once/global body filters, and rejecting `body` plus `filters` together.

- [x] **Step 3: Implement vars and filters**

Added config/schema support, bounded `vars` expression evaluation for common APISIX request/response variables and comparison operators, regexp filter compilation/defaults, once/global response body substitution, and header value variable resolution.

- [x] **Step 4: Update README**

Updated `response-rewrite` support notes to include bounded `vars`, header variable resolution, and response body `filters`, with full expression, compressed body decoding, and streaming filter limitations.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/response_rewrite -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 106: Implement `proxy-rewrite` Real Request URI Support

**Files:**
- Modify: `pkg/plugin/proxy_rewrite/plugin.go`
- Modify: `pkg/plugin/proxy_rewrite/plugin_test.go`
- Create: `pkg/route/proxy_rewrite_test.go`
- Modify: `pkg/route/builder.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official `use_real_request_uri_unsafe` config and rewritten URI values that may contain query strings.
- Produces: proxy-rewrite context values derived from `RequestURI()` when configured, regex matching against that real request URI, and route-side path/query splitting for upstream proxy requests.

- [x] **Step 1: Read official behavior**

Read official APISIX 3.17 `apisix/plugins/proxy-rewrite.lua`; `use_real_request_uri_unsafe` switches the source URI to `real_request_uri`, lets `regex_uri` match that full URI, and skips OpenResty URI safe-encoding before setting `upstream_uri`.

- [x] **Step 2: Write failing tests**

Tests cover using the real request URI as the rewrite source, matching `regex_uri` against a URI with query string, and route-side splitting of rewritten `path?query` into `URL.Path` and `URL.RawQuery`.

- [x] **Step 3: Implement unsafe real request URI handling**

Added schema/config support, request URI source selection, no-op real-request rewrite propagation when no explicit `uri` / `regex_uri` is configured, and a route helper that applies rewritten URI path/query pieces to the upstream request.

- [x] **Step 4: Update README**

Updated `proxy-rewrite` support notes to include `use_real_request_uri_unsafe` and clarify the remaining exact URI encoding and header mutation gaps.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_rewrite ./pkg/route -run 'TestHandlerUsesRealRequestURIUnsafeAsRewriteSource|TestHandlerRegexURIMatchesRealRequestURIUnsafe|TestApplyProxyRewriteURIUpdatesPathAndQuery' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/proxy_rewrite ./pkg/route -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 107: Implement `limit-count` Rules

**Files:**
- Modify: `pkg/plugin/limit_count/plugin.go`
- Modify: `pkg/plugin/limit_count/plugin_test.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official `rules` config with per-rule `count`, `time_window`, `key`, and optional `header_prefix`.
- Produces: APISIX-style rule resolution, duplicate-rule-key rejection, independent rule limiters, and per-rule quota response headers.

- [x] **Step 1: Read official behavior**

Read official APISIX 3.17 `apisix/plugins/limit-count/init.lua`; rules are an alternative to top-level `count` / `time_window`, duplicate rule keys are rejected, unresolved rule keys are skipped, and all resolved rules run in order until one rejects.

- [x] **Step 2: Write failing tests**

Tests cover rules-only schema validation, resolved rules applying in order, per-rule header prefixes, duplicate key rejection, and preserving existing HTTP-variable / variable-combination behavior.

- [x] **Step 3: Implement rules**

Added schema/config support, per-rule limiter initialization for local/Redis policies, rule key variable resolution, per-rule header construction, duplicate key validation, and shared request limiting code for top-level and rule configs.

- [x] **Step 4: Update README**

Updated `limit-count` support notes to include `rules` and per-rule `header_prefix`, leaving string `count` / `time_window`, plugin metadata custom quota headers, and `redis-cluster` as remaining gaps.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/limit_count ./pkg/plugin -run 'TestHandlerAppliesResolvedRules|TestPostInitRejectsDuplicateRuleKeys|TestHandlerUsesHTTPVariableKey|TestHandlerUsesVariableCombinationKey|TestNewLimitCountAcceptsRulesOnlyConfig' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/limit_count ./pkg/plugin -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 108: Implement `forward-auth` Extra Header Variable Resolution

**Files:**
- Modify: `pkg/plugin/forward_auth/plugin.go`
- Modify: `pkg/plugin/forward_auth/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official `extra_headers` string values containing APISIX-style variables such as `$remote_addr` and `$request_uri`.
- Produces: resolved extra headers on the authorization service request.

- [x] **Step 1: Read official behavior**

Read official APISIX 3.17 `apisix/plugins/forward-auth.lua`; `extra_headers` values are resolved through `core.utils.resolve_var(value, ctx.var)` before the auth request is sent.

- [x] **Step 2: Write failing tests**

Tests cover `$remote_addr`, `$request_uri`, and embedded variable text in `extra_headers`.

- [x] **Step 3: Implement variable resolution**

Added bounded APISIX-style variable substitution for common request variables used by this Go runtime before setting `extra_headers` on the auth request.

- [x] **Step 4: Update README**

Updated `forward-auth` support notes to include `extra_headers` variable resolution and leave `ssl_verify` / keepalive controls as remaining gaps.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/forward_auth -run 'TestHandlerResolvesExtraHeaderVariables' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/forward_auth -count=1 -timeout=10s -v`, `go test ./...`, and `make build`.

### Task 109: Implement `traffic-split` Match Vars

**Files:**
- Modify: `pkg/plugin/traffic_split/plugin.go`
- Modify: `pkg/plugin/traffic_split/plugin_test.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official `rules[].match[].vars` config for traffic-split.
- Produces: ordered rule matching against common APISIX request variables before selecting a weighted inline upstream.

- [x] **Step 1: Read official behavior**

Read official APISIX 3.17 `apisix/plugins/traffic-split.lua`; rules are checked in order, a rule without `match` matches immediately, match entries are ORed, and each `vars` expression gates that rule's weighted upstream selection.

- [x] **Step 2: Write failing tests**

Tests cover first matching rule selection from request headers, route-upstream fallback when no `match.vars` entry matches, and schema validation for the official `match: [{vars: ...}]` shape.

- [x] **Step 3: Implement match vars**

Changed traffic-split to compile per-rule balancers, evaluate bounded `vars` expressions for common request variables at request time, and select the first matching rule's inline weighted upstream.

- [x] **Step 4: Update README**

Updated `traffic-split` support notes to include bounded `match.vars`, leaving `upstream_id` as the remaining documented gap.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/traffic_split ./pkg/plugin -run 'TestHandler|TestNewTrafficSplitAcceptsMatchVars' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/traffic_split ./pkg/plugin -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 110: Implement `limit-conn` Rules

**Files:**
- Modify: `pkg/plugin/limit_conn/plugin.go`
- Modify: `pkg/plugin/limit_conn/plugin_test.go`
- Modify: `pkg/plugin/init_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official `rules` config for `limit-conn` with per-rule `conn`, `burst`, and variable-containing `key`.
- Produces: local-policy concurrent request limiting across every resolved rule, with prior rule counters released when a later rule rejects.

- [x] **Step 1: Read official behavior**

Read official APISIX 3.17 `apisix/plugins/limit-conn.lua` and `apisix/plugins/limit-conn/init.lua`; rules are an alternative to top-level `conn` / `burst` / `key`, require shared `default_conn_delay`, skip entries whose `key` resolves no variables, and return 500 when no usable rules remain unless degradation is enabled.

- [x] **Step 2: Write failing tests**

Tests cover rules-only schema validation, resolved rule application across multiple rules, release of earlier rule counters when a later rule rejects, and all-unresolved rules returning 500.

- [x] **Step 3: Implement rules**

Added schema/config support, per-rule validation, resolved rule keys, multi-rule admission and release tracking, rule-local counter namespacing, and shared rejection handling for top-level and rules paths.

- [x] **Step 4: Update README**

Updated `limit-conn` support notes to include local `rules`, keeping string `conn` / `burst`, `only_use_default_delay`, Redis, and Redis Cluster as remaining gaps.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/limit_conn ./pkg/plugin -run 'TestHandlerAppliesResolvedRules|TestHandlerReturnsInternalServerErrorWhenAllRulesAreUnresolved|TestHandlerAllowsDegradationWhenAllRulesAreUnresolved|TestNewLimitConnAcceptsRulesOnlyConfig' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/limit_conn ./pkg/plugin -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 111: Implement `proxy-rewrite` Header Mutation

**Files:**
- Modify: `pkg/plugin/proxy_rewrite/plugin.go`
- Modify: `pkg/plugin/proxy_rewrite/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official `headers.add`, `headers.set`, `headers.remove`, and legacy `headers` set config for `proxy-rewrite`.
- Produces: request-phase header mutation before the next middleware/proxy handler sees the request.

- [x] **Step 1: Read official behavior**

Read official APISIX 3.17 `apisix/plugins/proxy-rewrite.lua`; request headers support `add`, `set`, `remove`, legacy object-as-set config, string or numeric values, and variable resolution before mutation.

- [x] **Step 2: Write failing tests**

Tests cover `add` / `set` / `remove`, bounded header value variable resolution, legacy object-as-set decoding, and numeric header values decoded as strings.

- [x] **Step 3: Implement header mutation**

Added custom header config decoding, request-phase header application, legacy set support, numeric value stringification, and bounded APISIX-style request variable replacement for common variables and `http_*` headers.

- [x] **Step 4: Update README**

Updated `proxy-rewrite` support notes to include request header mutation and keep exact URI safe-encoding plus regex-capture header variable resolution as remaining gaps.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/proxy_rewrite -run 'TestHandlerMutatesRequestHeaders|TestHeadersUnmarshalLegacySet' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/proxy_rewrite -count=1 -timeout=10s`, `go test ./...`, and `make build`.

### Task 112: Implement `api-breaker` Healthy Statuses And Header Vars

**Files:**
- Modify: `pkg/plugin/api_breaker/plugin.go`
- Create: `pkg/plugin/api_breaker/plugin_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: official `healthy.http_statuses`, `healthy.successes`, and `break_response_headers[].value` variable config for `api-breaker`.
- Produces: breaker recovery only from configured healthy statuses, and blocked responses with resolved headers written before the response status.

- [x] **Step 1: Read official behavior**

Read official APISIX 3.17 `apisix/plugins/api-breaker.lua`; unhealthy statuses increment the failure state, healthy statuses clear the unhealthy state only after configured successes, neutral statuses do not recover the breaker, and break response header values resolve APISIX variables before being sent.

- [x] **Step 2: Write failing tests**

Tests cover break response headers being visible on the actual response with `$request_method`, `$request_uri`, and `$remote_addr` resolution, plus half-open recovery only from configured healthy statuses.

- [x] **Step 3: Implement bounded parity**

Set break response headers before `WriteHeader`, added bounded request variable resolution, and changed status accounting so only configured unhealthy statuses fail the breaker and only configured healthy statuses recover it.

- [x] **Step 4: Update README**

Updated `api-breaker` support notes to include healthy statuses and break header variable resolution, leaving shared-dict host/URI state, exponential breaker windows, and exact log-phase timing as remaining gaps.

- [x] **Step 5: Verify**

Run: `go test ./pkg/plugin/api_breaker -run 'TestHandlerResolvesBreakResponseHeaders|TestHandlerUsesConfiguredHealthyStatusesForRecovery' -count=1 -timeout=10s -v`, `go test ./pkg/plugin/api_breaker -count=1 -timeout=10s`, `go test ./...`, and `make build`.
