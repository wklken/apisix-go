# APISIX 3.17 Remaining Plugin Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish the remaining valuable APISIX 3.17 plugin parity work in Go while excluding OpenResty/NGINX-native behavior.

**Architecture:** Continue making PR-sized slices by plugin or tightly related plugin family. Each slice starts from APISIX `release/3.17` source and the current local README/checklist, adds focused red/green tests, implements only practical Go-native behavior, then updates README and the parity checklist.

**Tech Stack:** Go 1.26, `net/http` middleware plugins under `pkg/plugin/<plugin_name>`, existing `pkg/apisix/ctx` variables, `pkg/store` consumer/resource lookup, existing JSON Schema validator, existing Redis/go-redis patterns, and existing plugin package tests.

## Global Constraints

- Do not implement `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, `inspect`, `serverless-pre-function`, or `serverless-post-function` in normal parity work; they are OpenResty/NGINX/Lua-runtime-native or external-runner subsystem work.
- Do not implement exact OpenResty phase timing, `ngx_lua` APIs, Lua code execution, NGINX buffering internals, shared-dict/lrucache exactness, OCSP/TLS stapling internals, or external plugin runner protocol compatibility unless explicitly requested.
- Prefer current local plugin patterns over new abstractions.
- Every behavior slice needs a focused failing test before implementation.
- For code changes, run focused package tests, `go test ./...`, `make build`, `rm -f apisix`, and `git diff --check`.
- Format touched Go files with `golines -m 120 -w --base-formatter gofmt --no-reformat-tags <files>` and `gofumpt -l -w <files>`.
- Update README percentages/support notes and `docs/apisix-3.17-plugin-parity-checklist.md` for every completed slice.

---

## Logger

| Plugin | Current | What remains |
|---|---:|---|
| `http-logger` | 76% | Shared batch processor, `max_pending_entries`, and basic `batch_process_entries` gauge hook are implemented. Remaining gaps are APISIX batch route/server label population, stale-object cleanup exactness, and encrypted `auth_header`. |
| `skywalking-logger` | 76% | Shared batch processor, `max_pending_entries`, SkyWalking JSON-array batch payloads, basic `sw8` trace correlation, and basic `batch_process_entries` gauge hook are implemented. Remaining gaps are APISIX batch route/server label population and stale-object cleanup exactness. |
| `tcp-logger` | 70% | Shared batch processor, `max_pending_entries`, and basic `batch_process_entries` gauge hook are implemented. Remaining gaps are OpenResty cosocket pooling, APISIX batch route/server label population, and stale-object cleanup exactness. |
| `kafka-logger` | 76% | Shared batch processor, `max_pending_entries`, `meta_format = origin`, and single-object / JSON-array Kafka batch payloads are implemented. Remaining gap is encrypted broker password storage after a project-level secret design exists. |
| `rocketmq-logger` | 72% | Shared batch processor, `max_pending_entries`, `meta_format = origin`, and single-object / JSON-array RocketMQ batch payloads are implemented. Remaining gaps are encrypted `secret_key` and `use_tls`; the current RocketMQ Go client exposes no TLS option. |
| `syslog` | 70% | Shared batch processor, `max_pending_entries`, and basic `batch_process_entries` gauge hook are implemented. Remaining gaps are OpenResty syslog connection pooling/TLS behavior parity, APISIX batch route/server label population, and stale-object cleanup exactness. |
| `udp-logger` | 70% | Shared batch processor, `max_pending_entries`, and basic `batch_process_entries` gauge hook are implemented. Remaining gaps are APISIX batch route/server label population and stale-object cleanup exactness. |
| `clickhouse-logger` | 76% | Shared batch processor, `max_pending_entries`, JSONEachRow batch payloads, and basic `batch_process_entries` gauge hook are implemented. Remaining gaps are APISIX batch route/server label population, stale-object cleanup exactness, and encrypted `password`. |
| `log-rotate` | 72% | Go-native rotation lifecycle, file recreation, `file-logger` current-path writes after rotation, history pruning, and compression are implemented. Remaining NGINX master `USR1`, OpenResty timer, and runtime log-path discovery behavior is out of scope. |
| `error-log-logger` | 69% | Shared batch processor buffering/retry semantics, basic `batch_process_entries` gauge hook, SkyWalking `$hostname` service-instance resolution, and Kafka broker `PLAIN` SASL are implemented for explicit error-log delivery. Remaining gaps are APISIX batch route/server label population, stale-object cleanup exactness, Lua-resty-kafka producer cache exactness, encrypted metadata fields, direct `ngx.errlog` capture, and OpenResty timer lifecycle. |
| `sls-logger` | 72% | Shared batch processor, concatenated RFC5424 batch writes, and basic `batch_process_entries` gauge hook are implemented. Remaining gaps are APISIX batch route/server label population, stale-object cleanup exactness, and encrypted `access_key_secret`. |
| `google-cloud-logging` | 67% | Shared batch processor, `max_pending_entries`, access-token caching/refresh, and multi-entry Cloud Logging writes are implemented. Remaining gaps are encrypted `auth_config.private_key` and body capture, which APISIX 3.17 does not define for this plugin. |
| `splunk-hec-logging` | 62% | Shared batch processor, `max_pending_entries`, concatenated JSON event-object batches, and HEC error-text extraction are implemented. Remaining gaps are encrypted `endpoint.token` and body capture, which APISIX 3.17 does not define for this plugin. |
| `file-logger` | 82% | Plugin config/metadata `path`, `log_format`, and Go-native current-path writes after external rotation are implemented. Remaining exact OpenResty file-cache semantics are out of scope. |
| `loggly` | 76% | Shared batch processor, HTTP/S newline bulk batching, UDP per-entry batch delivery, metadata delivery config fallback, `max_pending_entries`, and basic `batch_process_entries` gauge hook are implemented. Remaining gaps are APISIX batch route/server label population, stale-object cleanup exactness, and encrypted `customer_token`. |
| `elasticsearch-logger` | 84% | Shared batch processor, `max_pending_entries`, `_bulk` NDJSON batch delivery, and basic `batch_process_entries` gauge hook are implemented while preserving index expansion, auth, headers, and body-expression behavior. Remaining gaps are APISIX batch route/server label population, stale-object cleanup exactness, and encrypted `auth.password`. |
| `loki-logger` | 76% | Shared batch processor, `max_pending_entries`, one-stream multi-value Loki batches, and basic `batch_process_entries` gauge hook are implemented. Remaining gaps are APISIX batch route/server label population and stale-object cleanup exactness. |
| `tencent-cloud-cls` | 76% | Shared batch processor, `max_pending_entries`, multi-log protobuf batch payloads, and basic `batch_process_entries` gauge hook are implemented. Remaining gaps are APISIX batch route/server label population, stale-object cleanup exactness, and encrypted `secret_key`. The upstream APISIX 3.17 SDK has an lz4/zstd TODO but no plugin config/feature to match. |
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

## Auth

| Plugin | Current | What remains |
|---|---:|---|
| `openid-connect` | 68% | Authorization-code/session flow, PKCE, logout/revocation, token renewal, client assertion auth, session-flow claim schema behavior, proxy options. |
| `key-auth` | 75% | Encrypted consumer-field parity only if a project-level encrypted-secret design exists; otherwise keep current API-key, hide-credentials, and anonymous-consumer behavior. |
| `jwt-auth` | 85% | Encrypted consumer-field parity only if a project-level encrypted-secret design exists; otherwise verify asymmetric algorithm and anonymous-consumer coverage stays aligned with APISIX 3.17. |
| `saml-auth` | 55% | IdP metadata loading, IdP-initiated SSO/SLO gaps, artifact binding if practical, richer logout semantics, more metadata/userinfo forwarding. Do not implement encrypted `resty.session` parity. |
| `cas-auth` | 60% | IdP single logout XML session deletion, better service/session parity, more user metadata propagation. Do not implement OpenResty shared-dict clustering. |
| `basic-auth` | 70% | Encrypted consumer-field parity only if a project-level encrypted-secret design exists; keep current Basic extraction, validation, consumer attachment, and `hide_credentials` behavior. |
| `authz-casdoor` | 60% | Forward Casdoor user/access token metadata upstream, token/session reuse, better logout/session handling. Do not implement encrypted `resty.session` cookies. |
| `authz-keycloak` | 60% | Shared token/resource cache approximation, request decorators, richer resource metadata handling, refresh-token reuse where practical. |
| `wolf-rbac` | 65% | Public APIs for login/change password/user info, retry/backoff, fuller consumer plugin metadata behavior. |
| `ldap-auth` | 65% | LDAP search filters, StartTLS fallback discovery, richer DN/user matching. Do not add `anonymous_consumer`; APISIX 3.17 `ldap-auth` does not define it. |
| `jwe-decrypt` | 65% | Additional JWE algorithms, AAD/header authentication, encrypted consumer-field handling if a safe local pattern exists. |
| `hmac-auth` | 82% | Encrypted consumer-field parity only if a project-level encrypted-secret design exists; otherwise keep current digest, body, `hide_credentials`, and anonymous-consumer behavior. |
| `authz-casbin` | 70% | Add plugin metadata fallback if local metadata lookup supports it cleanly. |
| `opa` | 70% | Add fuller APISIX `with_route` / `with_service` payloads from local route/service context where available. |
| `forward-auth` | 86% | Audit remaining official fields and edge response-header behavior; keep current method/header/upstream forwarding behavior stable. |
| `dingtalk-auth` | 60% | Store/reuse access tokens in session, `ctx.external_user` equivalent, better error logging, encrypted storage notes. |
| `feishu-auth` | 60% | Store/reuse Feishu access tokens in session, `ctx.external_user` equivalent, better error logging, encrypted storage notes. |
| `multi-auth` | 60% | Add more APISIX auth plugin types and preserve per-plugin failure details in final response. |

### Auth Execution Tasks

- [ ] **Task A1: Continue `openid-connect` with PKCE or code flow**
  - Start with PKCE verifier/challenge generation and validation if current session scaffolding supports it.
  - Tests: auth redirect includes `code_challenge`, callback validates session verifier, invalid state/verifier is rejected.

- [ ] **Task A2: Expand `multi-auth` plugin coverage**
  - Add one auth plugin at a time, only if the target plugin already has stable handler tests.
  - Tests: one plugin failure followed by next plugin success, final failure body preserves useful failure detail.

- [ ] **Task A3: Improve SSO-style auth plugins**
  - Work one plugin per commit: `saml-auth`, `cas-auth`, `authz-casdoor`, `dingtalk-auth`, `feishu-auth`.
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

1. Bound `error-log-logger` batch-label/cache parity, or defer it if it requires OpenResty runtime behavior.
2. `ai-rate-limiting` Redis/shared policy.
3. `workflow` delegated actions for already implemented plugins.
4. `zipkin` span reporting transport.
5. `oas-validator` external `$ref` or response validation, whichever is smaller after source inspection.
