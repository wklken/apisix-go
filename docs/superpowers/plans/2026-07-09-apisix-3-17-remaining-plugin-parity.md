# APISIX 3.17 Remaining Plugin Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish the remaining valuable APISIX 3.17 plugin parity work in Go while excluding OpenResty/NGINX-native behavior.

**Architecture:** Continue making PR-sized slices by plugin or tightly related plugin family. Each slice starts from APISIX `release/3.17` source and the current local README/checklist, adds focused red/green tests, implements only practical Go-native behavior, then updates README and the parity checklist.

**Tech Stack:** Go 1.26, `net/http` middleware plugins under `pkg/plugin/<plugin_name>`, existing `pkg/apisix/ctx` variables, `pkg/store` consumer/resource lookup, existing JSON Schema validator, existing Redis/go-redis patterns, and existing plugin package tests.

## Global Constraints

- Do not implement `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, `inspect`, `serverless-pre-function`, or `serverless-post-function` in normal parity work; they are OpenResty/NGINX/Lua-runtime-native or external-runner subsystem work.
- Do not implement exact OpenResty phase timing, `ngx_lua` APIs, Lua code execution, NGINX buffering internals, shared-dict/lrucache exactness, APISIX Lua batch-manager stale-object cache cleanup exactness, OCSP/TLS stapling internals, or external plugin runner protocol compatibility unless explicitly requested.
- Prefer current local plugin patterns over new abstractions.
- Every behavior slice needs a focused failing test before implementation.
- For code changes, run focused package tests, `go test ./...`, `make build`, `rm -f apisix`, and `git diff --check`.
- Format touched Go files with `golines -m 120 -w --base-formatter gofmt --no-reformat-tags <files>` and `gofumpt -l -w <files>`.
- Update README percentages/support notes and `docs/apisix-3.17-plugin-parity-checklist.md` for every completed slice.

---

## Logger

Shared logger batch processor behavior now includes route/server-aware `batch_process_entries`, complete-label-only
metric emission, `max_pending_entries`, retries, and graceful reload/shutdown buffer flushing.

| Plugin | Current | What remains |
|---|---:|---|
| `http-logger` | 76% | Shared batch processor, `max_pending_entries`, and route/server-aware `batch_process_entries` gauge hook are implemented. Remaining normal parity is encrypted `auth_header` after a project-level secret design exists. |
| `skywalking-logger` | 76% | Shared batch processor, `max_pending_entries`, SkyWalking JSON-array batch payloads, basic `sw8` trace correlation, and route/server-aware `batch_process_entries` gauge hook are implemented. No normal Go logger gap remains. |
| `tcp-logger` | 70% | Shared batch processor, `max_pending_entries`, and route/server-aware `batch_process_entries` gauge hook are implemented. No normal Go logger gap remains; OpenResty cosocket pooling is not required. |
| `kafka-logger` | 76% | Shared batch processor, `max_pending_entries`, `meta_format = origin`, and single-object / JSON-array Kafka batch payloads are implemented. Remaining gap is encrypted broker password storage after a project-level secret design exists. |
| `rocketmq-logger` | 72% | Shared batch processor, `max_pending_entries`, `meta_format = origin`, and single-object / JSON-array RocketMQ batch payloads are implemented. Remaining gaps are encrypted `secret_key` and `use_tls`; the current RocketMQ Go client exposes no TLS option. |
| `syslog` | 70% | Shared batch processor, `max_pending_entries`, and route/server-aware `batch_process_entries` gauge hook are implemented. No normal Go logger gap remains; OpenResty syslog connection pooling/TLS behavior parity is not required. |
| `udp-logger` | 70% | Shared batch processor, `max_pending_entries`, and route/server-aware `batch_process_entries` gauge hook are implemented. No normal Go logger gap remains. |
| `clickhouse-logger` | 76% | Shared batch processor, `max_pending_entries`, JSONEachRow batch payloads, and route/server-aware `batch_process_entries` gauge hook are implemented. Remaining normal parity is encrypted `password` after a project-level secret design exists. |
| `log-rotate` | 72% | Go-native rotation lifecycle, file recreation, `file-logger` current-path writes after rotation, history pruning, and compression are implemented. Remaining NGINX master `USR1`, OpenResty timer, and runtime log-path discovery behavior is out of scope. |
| `error-log-logger` | 69% | Shared batch processor buffering/retry semantics, SkyWalking `$hostname` service-instance resolution, and Kafka broker `PLAIN` SASL are implemented for explicit error-log delivery. Remaining normal parity is encrypted metadata fields after a project-level secret design exists; direct `ngx.errlog` capture, route/server `batch_process_entries` labels for global explicit delivery, Lua-resty-kafka producer cache exactness, and OpenResty timer lifecycle are out of scope. |
| `sls-logger` | 72% | Shared batch processor, concatenated RFC5424 batch writes, and route/server-aware `batch_process_entries` gauge hook are implemented. Remaining normal parity is encrypted `access_key_secret` after a project-level secret design exists. |
| `google-cloud-logging` | 67% | Shared batch processor, `max_pending_entries`, access-token caching/refresh, and multi-entry Cloud Logging writes are implemented. Remaining gaps are encrypted `auth_config.private_key` and body capture, which APISIX 3.17 does not define for this plugin. |
| `splunk-hec-logging` | 62% | Shared batch processor, `max_pending_entries`, concatenated JSON event-object batches, and HEC error-text extraction are implemented. Remaining gaps are encrypted `endpoint.token` and body capture, which APISIX 3.17 does not define for this plugin. |
| `file-logger` | 82% | Plugin config/metadata `path`, `log_format`, and Go-native current-path writes after external rotation are implemented. Remaining exact OpenResty file-cache semantics are out of scope. |
| `loggly` | 76% | Shared batch processor, HTTP/S newline bulk batching, UDP per-entry batch delivery, metadata delivery config fallback, `max_pending_entries`, and route/server-aware `batch_process_entries` gauge hook are implemented. Remaining normal parity is encrypted `customer_token` after a project-level secret design exists. |
| `elasticsearch-logger` | 84% | Shared batch processor, `max_pending_entries`, `_bulk` NDJSON batch delivery, and route/server-aware `batch_process_entries` gauge hook are implemented while preserving index expansion, auth, headers, and body-expression behavior. Remaining normal parity is encrypted `auth.password` after a project-level secret design exists. |
| `loki-logger` | 76% | Shared batch processor, `max_pending_entries`, one-stream multi-value Loki batches, and route/server-aware `batch_process_entries` gauge hook are implemented. No normal Go logger gap remains. |
| `tencent-cloud-cls` | 76% | Shared batch processor, `max_pending_entries`, multi-log protobuf batch payloads, and route/server-aware `batch_process_entries` gauge hook are implemented. Remaining normal parity is encrypted `secret_key` after a project-level secret design exists. The upstream APISIX 3.17 SDK has an lz4/zstd TODO but no plugin config/feature to match. |
| `lago` | 76% | Shared batch processor, retry semantics, APISIX `batch_max_size` default of 100, request-start event timestamps, and common dynamic request/response variables are implemented. Remaining gaps are encrypted `token` and exotic OpenResty/NGINX-only variable fidelity. |

### Logger Execution Tasks

- [x] **Task L1: Create shared logger batch processor**
  - Files: create `pkg/plugin/logger_batch/` or reuse an existing local shared package if present.
  - Tests: queue flush by size/time, drop at `max_pending_entries`, retry/backoff, graceful shutdown-free request lifecycle.
  - Verify: `go test ./pkg/plugin/logger_batch -count=1 -timeout=10s -v`.

- [x] **Task L2: Migrate one HTTP-style logger first**
  - Start with `http-logger` because it is easiest to verify with `httptest.Server`.
  - Tests: enqueue multiple entries, flush batch to server, reject overflow, preserve direct log_format/body behavior.
  - Commit before touching other loggers.

- [x] **Task L3: Apply batch processor to remaining network loggers in small groups**
  - Group 1: `tcp-logger` done, `udp-logger` done, `syslog` done.
  - Group 2: `clickhouse-logger` done, `loki-logger` done, `loggly` done.
  - Group 3: `skywalking-logger` done, `sls-logger` done, `tencent-cloud-cls` done.
  - Group 4: `google-cloud-logging` done, `splunk-hec-logging` done, `rocketmq-logger` done, `kafka-logger` done, `lago` done.

- [x] **Task L4: Fill route/server metric labels**
  - Route-local and global-rule loggers receive route ID and server address context.
  - Metrics are emitted only when APISIX-compatible `name`, `route_id`, and `server_addr` labels are all present.

- [x] **Task L5: Finish practical batch processor lifecycle parity**
  - Buffered entries are flushed when a processor is stopped.
  - Route builders own the logger processors they create; graceful server reload waits for old requests before stopping
    the retired route set, and graceful shutdown stops the active route set after HTTP requests quiesce.
  - Exact APISIX Lua `batch-processor-manager` stale-object cache cleanup remains out of normal scope.

## Auth

| Plugin | Current | What remains |
|---|---:|---|
| `openid-connect` | 98% | Encrypted private-key storage and exact OpenResty session semantics. Encrypted cookie/Redis session storage and `refresh_session_interval` are implemented. |
| `key-auth` | 75% | Encrypted consumer-field parity only if a project-level encrypted-secret design exists; otherwise keep current API-key, hide-credentials, and anonymous-consumer behavior. |
| `jwt-auth` | 85% | Encrypted consumer-field parity only if a project-level encrypted-secret design exists; otherwise verify asymmetric algorithm and anonymous-consumer coverage stays aligned with APISIX 3.17. |
| `saml-auth` | 85% | Encrypted `sp_private_key` / `secret` storage after a project-level secret design exists. Exact `lua-resty-saml` session/runtime behavior remains out of scope. |
| `cas-auth` | 85% | Encrypted `cookie.secret` storage after a project-level secret design exists. IdP single logout XML deletion is implemented; user metadata forwarding is not an APISIX 3.17 behavior and shared-dict clustering is out of scope. |
| `basic-auth` | 70% | Encrypted consumer-field parity only if a project-level encrypted-secret design exists; keep current Basic extraction, validation, consumer attachment, and `hide_credentials` behavior. |
| `authz-casdoor` | 85% | Encrypted `client_secret` storage after a project-level secret design exists. User/access-token forwarding is not an APISIX 3.17 behavior; exact `resty.session` runtime behavior is out of scope. |
| `authz-keycloak` | 85% | Encrypted `client_secret` storage after a project-level secret design exists. Process-shared discovery/service-account token caching, refresh-token reuse, expiry leeway, TLS verification, and keepalive settings are implemented. Lua request decorators and cross-process OpenResty shared-dict fidelity are out of scope. |
| `wolf-rbac` | 85% | Fuller consumer plugin metadata behavior. Public login/change-password/user-info APIs, TLS verification, and transient 5xx retry/backoff are implemented. |
| `ldap-auth` | 75% | Encrypted consumer `user_dn` support after a project-level secret design exists. Host-style LDAP addresses and direct-LDAPS `use_tls` behavior are implemented; do not add LDAP search filters, StartTLS, or `anonymous_consumer`, which APISIX 3.17 does not define. |
| `jwe-decrypt` | 90% | Encrypted consumer-field handling if a safe local pattern exists. Direct AES-256-GCM with 32-byte plain/base64url secrets is implemented; alternate algorithms and AAD/header authentication are not APISIX 3.17 plugin configurations. |
| `hmac-auth` | 82% | Encrypted consumer-field parity only if a project-level encrypted-secret design exists; otherwise keep current digest, body, `hide_credentials`, and anonymous-consumer behavior. |
| `authz-casbin` | 85% | No normal Go Auth TODO remains; plugin-metadata model/policy fallback and metadata reload are implemented. |
| `opa` | 90% | `with_route` / `with_service` emit full route/service resource documents from the route builder, with local ID/name/matched-URI fallback for direct plugin use. |
| `forward-auth` | 90% | POST body transport metadata forwarding is implemented. No normal TODO remains: APISIX 3.17's schema accepts string `extra_headers` values, while its numeric fallback is defensive runtime compatibility. |
| `dingtalk-auth` | 65% | Better error logging and encrypted storage notes. `$external_user` request-context metadata and local access-token caching are implemented. |
| `feishu-auth` | 65% | Keep access-token/session reuse aligned with the bounded official flow, improve error logging, encrypted storage notes. `$external_user` request-context metadata is implemented. |
| `multi-auth` | 85% | All APISIX 3.17 `type = auth` plugins are supported. Preserve APISIX's generic final denial response; per-plugin diagnostics are logging detail. |

### Auth Execution Tasks

- [ ] **Task A1: Continue `openid-connect` with PKCE or code flow**
  - [x] A1a: authorization-code cookie session flow, encrypted cookie storage, state validation, PKCE S256, token exchange, downstream token headers, and end-session logout redirect.
  - [x] A1b: renew expired cookie-session access tokens with a refresh token, including `renew_access_token_on_expiry`, `access_token_expires_in`, and `access_token_expires_leeway`.
  - [x] A1c: revoke refresh and access tokens on logout through the discovered RFC 7009 endpoint.
  - [x] A1d: apply static `authorization_params` and force authorization before cached-session reuse with `force_reauthorize`.
  - [x] A1e: support `private_key_jwt` and `client_secret_jwt` assertions for token and introspection endpoints.
  - [x] A1f: support `proxy_opts` HTTP/HTTPS proxies, Basic proxy credentials, and `no_proxy` host/domain bypasses.
  - [x] A1g: validate `{user, access_token, id_token}` against `claim_schema` before storing a code-flow session.
  - [x] A1h: support encrypted Redis sessions with configured TLS/auth/database/prefix/timeouts and logout deletion.
  - [x] A1i: silently reauthenticate valid sessions with `prompt=none` after `refresh_session_interval`.

- [x] **Task A2: Expand `multi-auth` plugin coverage**
  - [x] Add `ldap-auth`, `jwe-decrypt`, and `wolf-rbac` after normal authentication failures.
  - [x] Cover one plugin failure followed by a later plugin success. The final denial body remains the generic APISIX response.

- [ ] **Task A3: Improve SSO-style auth plugins**
  - [x] `cas-auth`: delete the matching local session on an IdP single-logout XML `SessionIndex` request.
  - [ ] Work one plugin per commit: `saml-auth`, `authz-casdoor`, `dingtalk-auth`, `feishu-auth`.
  - Focus on metadata forwarding/session reuse before native encrypted-session parity.

## AI

| Plugin | Current | What remains |
|---|---:|---|
| `ai` | 20% | Decide whether any Go-native equivalent is valuable; most APISIX behavior is runtime/router/balancer replacement and should stay deferred unless requested. |
| `ai-proxy` | 58% | AI protocol detection/conversion registry, OpenAI Responses/Embeddings routing, provider-native request construction, AWS SigV4/GCP auth, streaming SSE/EventStream, streaming log variables, active connection metrics. |
| `ai-proxy-multi` | 58% | Health checks, DNS node resolution, host/SNI preservation, priority balancer parity, AI rate-limiting fallback integration, same protocol/provider/streaming gaps as `ai-proxy`. |
| `ai-request-rewrite` | 50% | AI protocol registry/conversion, Anthropic/Gemini/Vertex/Bedrock request construction, provider response filters, fallback/error-response policy integration. |
| `ai-rate-limiting` | 50% | Redis/shared policy, `rules`, Lua `cost_expr`/expression, string variable limits, streaming token usage, automatic `ai-proxy`/`ai-proxy-multi` instance selection. |
| `ai-rag` | 55% | More providers beyond Azure OpenAI/Azure AI Search, protocol append registry, broader Azure options, APISIX ctx/log variables, streaming/body-filter behavior. |
| `ai-prompt-template` | 55% | Nested variable lookup, XML/form/multipart inputs, Anthropic/Responses-native outputs, template cache behavior. |
| `ai-prompt-decorator` | 55% | Anthropic Messages, Bedrock Converse, OpenAI Embeddings, passthrough protocol decoration, streaming-specific behavior. |
| `ai-prompt-guard` | 60% | Full protocol extraction beyond OpenAI Chat/Responses, OpenAI Embeddings, passthrough detection, AI-provider deny response shaping. |
| `ai-aws-content-moderation` | 55% | AWS credential provider/session token support, response body moderation, streaming moderation, AI protocol content extraction, log variables. |
| `ai-aliyun-content-moderation` | 50% | Response body moderation, streaming moderation, provider-compatible deny response shaping, SSE annotation, APISIX log variables. |

### AI Execution Tasks

- [ ] **Task AI1: Build a minimal AI protocol abstraction**
  - Define bounded request/response structs for OpenAI Chat, OpenAI Responses, and Embeddings.
  - Use it first in `ai-proxy`; do not refactor every AI plugin at once.

- [ ] **Task AI2: Add OpenAI Responses/Embeddings support to `ai-proxy`**
  - Tests: request detection, provider body construction, response forwarding, log variable extraction where non-streaming.

- [ ] **Task AI3: Reuse protocol abstraction in prompt plugins**
  - Apply to `ai-prompt-template`, `ai-prompt-decorator`, and `ai-prompt-guard`.
  - Keep each plugin in a separate commit.

- [ ] **Task AI4: Add Redis/shared policy to `ai-rate-limiting`**
  - Reuse existing Redis patterns from `limit-count`/`graphql-limit-count`.
  - Tests: shared quota, strategy-specific counters, `allow_degradation`.

## Observability

| Plugin | Current | What remains |
|---|---:|---|
| `zipkin` | 45% | Span reporting transport, richer propagation, sampling behavior, endpoint/service config parity, error/status tagging. |
| `skywalking` | 50% | Improve Go-native segment reporting, reference fidelity, and body/timing lifecycle where practical; keep native OpenResty tracer and shared tracing buffer out of scope. |
| `opentelemetry` | 47% | Add collector/exporter metadata parity, `trace_id_source`, and more attribute mapping where current Go tracing stack supports it; keep phase-child-span and log-phase exactness out of scope. |
| `prometheus` | 45% | Broader APISIX exporter labels, stream metrics if ever supported, extra-label variable expansion, metric expiration, privileged-agent offload approximation. |
| `node-status` | 55% | Persist node UID if practical and improve Go-native connection/runtime counters; keep exact NGINX connection-state counters out of scope. |
| `datadog` | 68% | Batch processor behavior, exact APISIX log-entry timing/source parity, richer tag/metric parity where stable. |

### Observability Execution Tasks

- [ ] **Task O1: Improve `zipkin` reporting**
  - Tests: started span emits expected JSON to `httptest.Server`, headers propagate, status/error tags are present.

- [ ] **Task O2: Improve `prometheus` labels and expiration**
  - Tests: route/service/consumer labels, configured extra labels from available variables, expiration behavior if implemented.

- [ ] **Task O3: Reuse logger batch processor for `datadog` if applicable**
  - Keep DogStatsD UDP semantics separate from HTTP logger batch semantics.

## Others

| Plugin | Current | What remains |
|---|---:|---|
| `batch-requests` | 70% | Add `ssl_verify` and any safe APISIX subrequest edge behavior; keep true HTTP pipelining and NGINX real-ip config parity out of scope. |
| `redirect` | 90% | Improve `plugin_attr.redirect.https_port` fallback only if local SSL listen config is available. |
| `echo` | 90% | Verify response-header/body timing parity; add focused edge tests if APISIX 3.17 has behavior not yet covered. |
| `gzip` | 98% | No normal parity work beyond documentation/tests; NGINX `buffers` is native and not required. |
| `brotli` | 75% | Add `mode`, `lgwin`, and `lgblock` runtime tuning if the Go encoder supports them cleanly; keep NGINX streaming compression internals out of scope. |
| `real-ip` | 85% | Improve variable-source coverage and schema validation where practical; keep NGINX variable cache flushing and APISIX-Base `set_real_ip` internals out of scope. |
| `server-info` | 45% | Add UID persistence and etcd reporting/lease keepalive if useful for the Go control plane. |
| `error-page` | 55% | Limit rewrites to APISIX-generated errors if the Go response pipeline can distinguish them; expose metadata schema if local plugin interfaces support it. |
| `exit-transformer` | 30% | Support more documented non-Lua response transformations; arbitrary Lua execution and `core.response.exit()` callback fidelity are out of scope. |
| `attach-consumer-label` | 70% | Add non-string label serialization if APISIX source requires it and local consumer labels preserve types. |
| `azure-functions` | 65% | Add metadata master-key fallback, wildcard `:ext` path forwarding, and HTTP/2 connection-header filtering. |
| `openfunction` | 65% | Add wildcard `:ext` path forwarding and HTTP/2 connection-header filtering. |
| `openwhisk` | 75% | Improve OpenWhisk result body edge cases; keep OpenResty response-header behavior exactness out of scope. |
| `aws-lambda` | 70% | Improve SigV4 header/query/path canonicalization edge cases and wildcard `:ext` path forwarding. |
| `response-rewrite` | 84% | Expand bounded `lua-resty-expr` variable/operator support and add deflate/brotli decode if practical; streaming chunk body filters remain out of scope. |
| `proxy-rewrite` | 98% | Only small URI safe-encoding parity/test gaps remain; avoid risky rewrites. |
| `fault-injection` | 88% | Expand bounded `resty.expr` operator and APISIX variable support. |
| `mocking` | 97% | Improve schema random-value distribution if needed; otherwise monitor only. |
| `proxy-buffering` | 60% | Any practical Go proxy buffering knobs; do not implement NGINX buffering internals. |
| `proxy-control` | 60% | More control knobs that map to Go proxy behavior; keep NGINX-native controls documented as unsupported. |
| `cors` | 80% | Tighten wildcard response-header semantics for methods/exposed headers if APISIX behavior can be pinned with tests. |
| `acl` | 70% | Add `external_user` label support if local request context exposes compatible external-user metadata. |
| `uri-blocker` | 95% | Only PCRE/JIT regex exactness remains; monitor unless a concrete APISIX parity bug appears. |
| `ip-restriction` | 90% | Improve `ip_def` schema validation and matcher cache only if useful in Go; OpenResty shared LRU exactness is out of scope. |
| `ua-restriction` | 95% | Only OpenResty multi-value User-Agent fidelity remains; monitor unless needed. |
| `referer-restriction` | 95% | Improve APISIX `host_def` schema validation. |
| `consumer-restriction` | 80% | Add method enum schema parity and automatic consumer-group attachment if local consumer-group context supports it. |
| `csrf` | 72% | Encrypted consumer-field parity only if a project-level encrypted-secret design exists; improve Lua random-number formatting only if tests show user-visible mismatch. |
| `GM` | 25% | Keep real NTLS/Tongsuo/SM2/SM3/SM4 serving out of scope; only maintain schema/marker validation and docs unless a Go-native TLS design is requested. |
| `chaitin-waf` | 55% | Health checker/round-robin picker approximation, fuller expression support, response header/body integration where practical. |
| `data-mask` | 65% | Fuller JSONPath, log-phase-only behavior approximation, request-line masking for logger output. |
| `body-transformer` | 55% | More template syntax, nested values, XML/form/multipart handling if bounded. |
| `limit-req` | 84% | Add Redis Cluster only after standalone Redis behavior is stable and shared helpers make it low-risk. |
| `limit-conn` | 87% | Add Redis Cluster only after standalone Redis behavior is stable and shared helpers make it low-risk. |
| `limit-count` | 86% | Add Redis Cluster only after standalone Redis behavior is stable and shared helpers make it low-risk. |
| `proxy-cache` | 78% | Disk cache zones and stale serving need a dedicated cache design; keep current in-memory path stable. |
| `graphql-proxy-cache` | 55% | `graphql.max_size`, route/service ID cache keys, public purge endpoint if practical, deeper GraphQL parser parity. |
| `request-validation` | 85% | Add missing schema edge cases around headers/forms and improve APISIX-style rejection details. |
| `proxy-mirror` | 73% | Add gRPC mirroring and APISIX DNS resolver behavior only if the Go proxy layer exposes enough hooks. |
| `workflow` | 70% | More delegated actions for implemented plugins and better condition expression coverage. |
| `graphql-limit-count` | 62% | Redis Cluster, `graphql.max_size`, deeper GraphQL parsing parity. |
| `api-breaker` | 95% | Shared-dict and log-phase exactness are native; only add user-visible breaker window parity if practical. |
| `traffic-split` | 80% | Improve upstream balancer parity, health checks, retries, and bounded `lua-resty-expr` syntax where practical. |
| `traffic-label` | 63% | More variable/expression support and label propagation parity. |
| `request-id` | 85% | Add plugin-attr snowflake config and etcd-backed machine leasing if local config/store patterns make it practical. |
| `oas-validator` | 62% | External `$ref`, metadata TTL refresh, more OpenAPI parameter styles, non-JSON body schemas, response validation. |
| `mcp-bridge` | 55% | Session recovery, ping keepalive, process timeouts, backpressure controls, fuller MCP protocol validation. |
| `degraphql` | 65% | More GraphQL parsing parity and variable handling. |
| `kafka-proxy` | 35% | Actual Kafka upstream transport/proxying, websocket-to-Kafka forwarding, SASL mechanisms beyond PLAIN. |
| `dubbo-proxy` | 30% | Needs a dedicated Dubbo transport design for HTTP-to-Dubbo proxying, Hessian2 conversion, multiplexing, and response mapping. |
| `grpc-transcode` | 55% | More protobuf/HTTP mapping parity, descriptor handling, error mapping, streaming if practical. |
| `grpc-web` | 68% | Trailer/status fidelity, streaming parity, CORS/header edge cases. |
| `http-dubbo` | 55% | Hessian2 serialization, more Dubbo response branches, route-builder integration, retries/health checks. |
| `public-api` | 60% | Additional registered public APIs and Prometheus proxying where local runtime exposes them. |
| `mqtt-proxy` | 15% | Requires stream/L4 route support, MQTT CONNECT preread parsing, client-ID variable registration, and stream log phase; treat as a separate stream subsystem slice. |
| `example-plugin` | 60% | Metadata schema exposure and APISIX demo behavior, while avoiding production-grade upstream rewrites beyond current local patterns. |

### Others Execution Tasks

- [ ] **Task X1: Improve `workflow` delegated actions**
  - Add one delegated plugin action per commit.
  - Tests: condition match, action execution, action failure behavior.

- [ ] **Task X2: Improve validation/transformation plugins**
  - Work order: `oas-validator`, `body-transformer`, `data-mask`, `traffic-label`.
  - Add bounded parser/expression support only where tests can pin behavior.

- [ ] **Task X3: Improve protocol bridge plugins**
  - Work order: `grpc-web`, `grpc-transcode`, `http-dubbo`, `degraphql`, `kafka-proxy`, `mcp-bridge`.
  - Avoid large transport subsystem rewrites without a dedicated design.

- [ ] **Task X4: Improve cloud/function plugins**
  - Work order: `azure-functions`, `openfunction`.
  - Focus on config/header/error parity and tests with `httptest.Server`.

## Deferred Native / Not Required

These are intentionally not implementation TODOs for normal parity work:

| Plugin | Reason |
|---|---|
| `ext-plugin-pre-req` | External plugin runner protocol/subsystem, not normal Go plugin parity. |
| `ext-plugin-post-req` | External plugin runner protocol/subsystem, not normal Go plugin parity. |
| `ext-plugin-post-resp` | External plugin runner protocol/subsystem, not normal Go plugin parity. |
| `inspect` | Lua/OpenResty runtime inspection feature. |
| `serverless-pre-function` | Lua/OpenResty `ngx_lua` function execution. |
| `serverless-post-function` | Lua/OpenResty `ngx_lua` function execution. |

## Suggested Next Five Slices

1. `ai-rate-limiting` Redis/shared policy.
2. `workflow` delegated actions for already implemented plugins.
3. `zipkin` span reporting transport.
4. `oas-validator` external `$ref` or response validation, whichever is smaller after source inspection.
5. Revisit logger encrypted-secret parity only after a project-level secret/encryption design exists.
