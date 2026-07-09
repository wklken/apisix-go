# APISIX 3.17 Remaining Plugin TODO Plan (5 Parts)

This is the active TODO list for remaining APISIX 3.17 plugin parity work. It lists only remaining implementation work and groups it into five parts: Logger, Auth, AI, Observability, and Others. OpenResty-native, NGINX-native, Lua-runtime-native, serverless function plugins, and missing/deferred official defaults are not normal TODOs.

Not required unless explicitly requested: `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, `inspect`, `serverless-pre-function`, `serverless-post-function`, OCSP stapling internals, exact OpenResty phase timing, `ngx_lua` APIs, Lua code execution, NGINX buffering internals, and shared-dict/lrucache exactness. Do not create normal plugin implementation tasks for these native/runtime-only items.

## Logger

| Plugin | What needs to be done |
|---|---|
| `http-logger` | Only lower-priority gaps remain: APISIX batch metric/stale-object cleanup exactness and encrypted `auth_header` after a project-level secret design exists. |
| `skywalking-logger` | Only lower-priority gaps remain: APISIX batch metric/stale-object cleanup exactness. |
| `tcp-logger` | Only lower-priority gaps remain: APISIX batch metric/stale-object cleanup exactness and OpenResty cosocket pooling, which is not required. |
| `kafka-logger` | Only remaining gap is encrypted broker password storage after a project-level secret design exists. |
| `rocketmq-logger` | Remaining gaps are encrypted `secret_key` and `use_tls`; the current RocketMQ Go client exposes no TLS option. |
| `syslog` | Only lower-priority gaps remain: APISIX batch metric/stale-object cleanup exactness and native connection pooling/TLS exactness, which is not required. |
| `udp-logger` | Only lower-priority gaps remain: APISIX batch metric/stale-object cleanup exactness. |
| `clickhouse-logger` | Only lower-priority gaps remain: APISIX batch metric/stale-object cleanup exactness and encrypted `password` after a project-level secret design exists. |
| `sls-logger` | Only lower-priority gaps remain: APISIX batch metric/stale-object cleanup exactness and encrypted `access_key_secret` after a project-level secret design exists. |
| `google-cloud-logging` | Remaining gaps are encrypted `auth_config.private_key` and request/response body capture only if a future APISIX version defines it. |
| `splunk-hec-logging` | Remaining gaps are encrypted `endpoint.token` and request/response body capture only if a future APISIX version defines it. |
| `elasticsearch-logger` | Remaining gaps are APISIX batch metric/stale-object cleanup exactness and encrypted `auth.password` after a project-level secret design exists. |
| `loggly` | Only lower-priority gaps remain: APISIX batch metric/stale-object cleanup exactness and encrypted `customer_token` after a project-level secret design exists. |
| `loki-logger` | Only lower-priority gaps remain: APISIX batch metric/stale-object cleanup exactness. |
| `tencent-cloud-cls` | Only lower-priority gaps remain: APISIX batch metric/stale-object cleanup exactness, encrypted `secret_key`, and optional lz4/zstd compression. |
| `lago` | Remaining gaps are fuller APISIX/NGINX variable template coverage, encrypted `token`, and request start-time fidelity. |
| `log-rotate` | Improve Go-native rotation lifecycle, file reopening, and compression behavior where practical. Keep NGINX master `USR1`, OpenResty timers, and runtime log-path discovery out of scope. |
| `error-log-logger` | Improve sink-specific auth/options beyond Kafka `PLAIN` SASL. Keep direct `ngx.errlog` capture, OpenResty timer lifecycle, APISIX batch Prometheus gauge/stale-object cleanup exactness, and encrypted metadata fields out of normal scope unless a separate design requests them. |
| `file-logger` | Remaining gap is Go-native file reopen/cache approximation if useful. Keep OpenResty file-cache exactness out of scope. |

## Auth

| Plugin | What needs to be done |
|---|---|
| `openid-connect` | Add authorization-code/session flow, PKCE, logout/revocation, token renewal, more client assertion auth, session-flow claim schema behavior, and proxy options where practical. |
| `key-auth` | Only remaining parity is encrypted consumer-field support after a project-level secret design exists; current API-key, hide-credentials, and anonymous-consumer behavior should stay stable. |
| `jwt-auth` | Only remaining parity is encrypted consumer-field support after a project-level secret design exists; keep asymmetric algorithms and anonymous-consumer coverage aligned with APISIX 3.17. |
| `basic-auth` | Only remaining parity is encrypted consumer-field support after a project-level secret design exists; keep Basic extraction, validation, consumer attachment, and `hide_credentials` stable. |
| `hmac-auth` | Only remaining parity is encrypted consumer-field support after a project-level secret design exists; keep digest, body, `hide_credentials`, and anonymous-consumer behavior stable. |
| `saml-auth` | Add IdP metadata loading, IdP-initiated SSO/SLO gaps, richer logout semantics, and more metadata/userinfo forwarding. Defer artifact binding unless it can be bounded cleanly. |
| `cas-auth` | Add IdP single logout XML session deletion, better service/session parity, and richer user metadata propagation. Keep OpenResty shared-dict clustering out of scope. |
| `authz-casdoor` | Forward Casdoor user/access-token metadata upstream, reuse token/session state, and improve logout/session behavior. |
| `authz-keycloak` | Add shared token/resource cache approximation, request decorators, richer resource metadata handling, and refresh-token reuse where practical. |
| `wolf-rbac` | Add public APIs for login/change password/user info, retry/backoff, and fuller consumer plugin metadata behavior. |
| `ldap-auth` | Add LDAP search filters, StartTLS fallback discovery, and richer DN/user matching. Do not add `anonymous_consumer`; APISIX 3.17 `ldap-auth` does not define it. |
| `jwe-decrypt` | Add additional JWE algorithms, AAD/header authentication, and encrypted consumer-field handling only after a safe local secret pattern exists. |
| `multi-auth` | Add more APISIX auth plugin types and preserve useful per-plugin failure details in the final response. |
| `dingtalk-auth` | Store/reuse access tokens in session, attach `ctx.external_user` equivalent metadata, improve error logging, and document encrypted-storage gaps. |
| `feishu-auth` | Store/reuse Feishu access tokens in session, attach `ctx.external_user` equivalent metadata, improve error logging, and document encrypted-storage gaps. |
| `authz-casbin` | Add plugin metadata fallback if local metadata lookup supports it cleanly. |
| `opa` | Add fuller APISIX `with_route` and `with_service` payloads from local route/service context where available. |
| `forward-auth` | Audit remaining official fields and response-header edge behavior; keep current method/header/upstream forwarding behavior stable. |

## AI

| Plugin | What needs to be done |
|---|---|
| `ai` | Keep deferred unless a Go-native equivalent is explicitly requested; most APISIX behavior replaces runtime/router/balancer internals. |
| `ai-proxy` | Add AI protocol detection/conversion registry, OpenAI Responses/Embeddings routing, provider-native request construction, AWS SigV4/GCP auth, streaming SSE/EventStream, streaming log variables, and active connection metrics. |
| `ai-proxy-multi` | Add health checks, DNS node resolution, host/SNI preservation, priority balancer parity, AI rate-limiting fallback integration, and the same provider/protocol/streaming gaps as `ai-proxy`. |
| `ai-request-rewrite` | Add AI protocol registry/conversion, Anthropic/Gemini/Vertex/Bedrock request construction, provider response filters, and fallback/error-response policy integration. |
| `ai-rate-limiting` | Add Redis/shared policy, `rules`, bounded `cost_expr`/expression support, string variable limits, streaming token usage, and automatic `ai-proxy`/`ai-proxy-multi` instance selection. |
| `ai-rag` | Add providers beyond Azure OpenAI/Azure AI Search, protocol append registry, broader Azure options, APISIX ctx/log variables, and streaming/body-filter behavior where practical. |
| `ai-prompt-template` | Add nested variable lookup, XML/form/multipart inputs, Anthropic/Responses-native outputs, and template cache behavior. |
| `ai-prompt-decorator` | Add Anthropic Messages, Bedrock Converse, OpenAI Embeddings, passthrough protocol decoration, and streaming-specific behavior. |
| `ai-prompt-guard` | Add full protocol extraction beyond OpenAI Chat/Responses, OpenAI Embeddings, passthrough detection, and AI-provider deny response shaping. |
| `ai-aws-content-moderation` | Add AWS credential provider/session token support, response-body moderation, streaming moderation, AI protocol content extraction, and log variables. |
| `ai-aliyun-content-moderation` | Add response-body moderation, streaming moderation, provider-compatible deny response shaping, SSE annotation, and APISIX log variables. |

## Observability

| Plugin | What needs to be done |
|---|---|
| `zipkin` | Add span reporting transport, richer propagation, sampling behavior, endpoint/service config parity, and error/status tagging. |
| `skywalking` | Improve Go-native segment reporting, reference fidelity, and body/timing lifecycle where practical. Keep native OpenResty tracer and shared tracing buffer out of scope. |
| `opentelemetry` | Add collector/exporter metadata parity, `trace_id_source`, and more attribute mapping where current Go tracing support allows it. Keep phase-child-span and log-phase exactness out of scope. |
| `prometheus` | Add broader APISIX exporter labels, extra-label variable expansion, metric expiration, and privileged-agent offload approximation. Stream metrics only if stream support exists later. |
| `node-status` | Persist node UID if practical and improve Go-native connection/runtime counters. Keep exact NGINX connection-state counters out of scope. |
| `datadog` | Add batch processor behavior, exact APISIX log-entry timing/source parity where practical, and richer tag/metric parity. |

## Others

| Plugin | What needs to be done |
|---|---|
| `batch-requests` | Add `ssl_verify` and safe APISIX subrequest edge behavior. Keep true HTTP pipelining and NGINX real-ip config parity out of scope. |
| `redirect` | Improve `plugin_attr.redirect.https_port` fallback only if local SSL listen config is available. |
| `echo` | Verify response-header/body timing parity and add focused edge tests if APISIX 3.17 behavior is not covered. |
| `gzip` | No normal implementation work; only documentation/tests remain because NGINX `buffers` is native and not required. |
| `brotli` | Add `mode`, `lgwin`, and `lgblock` runtime tuning if the Go encoder supports them cleanly. Keep NGINX streaming compression internals out of scope. |
| `real-ip` | Improve variable-source coverage and schema validation where practical. Keep NGINX variable cache flushing and APISIX-Base `set_real_ip` internals out of scope. |
| `server-info` | Add UID persistence and etcd reporting/lease keepalive if useful for the Go control plane. |
| `error-page` | Limit rewrites to APISIX-generated errors if the Go response pipeline can distinguish them; expose metadata schema if local plugin interfaces support it. |
| `exit-transformer` | Support more documented non-Lua response transformations. Arbitrary Lua execution and `core.response.exit()` callback fidelity are out of scope. |
| `attach-consumer-label` | Add non-string label serialization if APISIX source requires it and local consumer labels preserve types. |
| `azure-functions` | Add metadata master-key fallback, wildcard `:ext` path forwarding, and HTTP/2 connection-header filtering. |
| `openfunction` | Add wildcard `:ext` path forwarding and HTTP/2 connection-header filtering. |
| `openwhisk` | Improve OpenWhisk result body edge cases; keep OpenResty response-header exactness out of scope. |
| `aws-lambda` | Improve SigV4 header/query/path canonicalization edge cases and wildcard `:ext` path forwarding. |
| `response-rewrite` | Expand bounded `lua-resty-expr` variable/operator support and add deflate/brotli decode if practical. Streaming chunk body filters remain out of scope. |
| `proxy-rewrite` | Only small URI safe-encoding parity/test gaps remain; avoid risky rewrites. |
| `fault-injection` | Expand bounded `resty.expr` operator and APISIX variable support. |
| `mocking` | Improve schema random-value distribution only if a concrete parity bug appears; otherwise monitor. |
| `proxy-buffering` | Add practical Go proxy buffering knobs. Do not implement NGINX buffering internals. |
| `proxy-control` | Add control knobs that map to Go proxy behavior and document NGINX-native controls as unsupported. |
| `cors` | Tighten wildcard response-header semantics for methods/exposed headers if APISIX behavior can be pinned with tests. |
| `acl` | Add `external_user` label support if local request context exposes compatible metadata. |
| `uri-blocker` | Only PCRE/JIT regex exactness remains; monitor unless a concrete APISIX parity bug appears. |
| `ip-restriction` | Improve `ip_def` schema validation and matcher cache only if useful in Go; OpenResty shared LRU exactness is out of scope. |
| `ua-restriction` | Only OpenResty multi-value User-Agent fidelity remains; monitor unless needed. |
| `referer-restriction` | Improve APISIX `host_def` schema validation. |
| `consumer-restriction` | Add method enum schema parity and automatic consumer-group attachment if local consumer-group context supports it. |
| `csrf` | Improve Lua random-number formatting only if tests show user-visible mismatch; encrypted consumer-field parity needs a project-level secret design first. |
| `GM` | Keep real NTLS/Tongsuo/SM2/SM3/SM4 serving out of scope; maintain schema/marker validation and docs unless a Go-native TLS design is requested. |
| `chaitin-waf` | Add health checker/round-robin picker approximation, fuller expression support, and response header/body integration where practical. |
| `data-mask` | Add fuller JSONPath, log-phase-only behavior approximation, and request-line masking for logger output. |
| `body-transformer` | Add more template syntax, nested values, and XML/form/multipart handling if bounded. |
| `limit-req` | Add Redis Cluster only after standalone Redis behavior is stable and shared helpers make it low-risk. |
| `limit-conn` | Add Redis Cluster only after standalone Redis behavior is stable and shared helpers make it low-risk. |
| `limit-count` | Add Redis Cluster only after standalone Redis behavior is stable and shared helpers make it low-risk. |
| `proxy-cache` | Design disk cache zones and stale serving separately; keep current in-memory path stable. |
| `graphql-proxy-cache` | Add `graphql.max_size`, route/service ID cache keys, public purge endpoint if practical, and deeper GraphQL parser parity. |
| `request-validation` | Add missing schema edge cases around headers/forms and improve APISIX-style rejection details. |
| `proxy-mirror` | Add gRPC mirroring and APISIX DNS resolver behavior only if the Go proxy layer exposes enough hooks. |
| `workflow` | Add more delegated actions for implemented plugins and better condition expression coverage. |
| `graphql-limit-count` | Add Redis Cluster, `graphql.max_size`, and deeper GraphQL parsing parity. |
| `api-breaker` | Only shared-dict and log-phase exactness remain, which are native; add user-visible breaker-window parity only if practical. |
| `traffic-split` | Improve upstream balancer parity, health checks, retries, and bounded `lua-resty-expr` syntax where practical. |
| `traffic-label` | Add more variable/expression support and label propagation parity. |
| `request-id` | Add plugin-attr snowflake config and etcd-backed machine leasing if local config/store patterns make it practical. |
| `oas-validator` | Add external `$ref`, metadata TTL refresh, more OpenAPI parameter styles, non-JSON body schemas, and response validation. |
| `mcp-bridge` | Add session recovery, ping keepalive, process timeouts, backpressure controls, and fuller MCP protocol validation. |
| `degraphql` | Add more GraphQL parsing parity and variable handling. |
| `kafka-proxy` | Add actual Kafka upstream transport/proxying, websocket-to-Kafka forwarding, and SASL mechanisms beyond PLAIN. |
| `dubbo-proxy` | Needs a dedicated Dubbo transport design for HTTP-to-Dubbo proxying, Hessian2 conversion, multiplexing, and response mapping. |
| `grpc-transcode` | Add more protobuf/HTTP mapping parity, descriptor handling, error mapping, and streaming if practical. |
| `grpc-web` | Improve trailer/status fidelity, streaming parity, and CORS/header edge cases. |
| `http-dubbo` | Add Hessian2 serialization, more Dubbo response branches, route-builder integration, retries, and health checks. |
| `public-api` | Add more registered public APIs and Prometheus proxying where local runtime exposes them. |
| `mqtt-proxy` | Requires a separate stream/L4 subsystem: stream routes, MQTT CONNECT preread parsing, client-ID variables, and stream log phase. |
| `example-plugin` | Add metadata schema exposure and APISIX demo behavior without production-grade upstream rewrites beyond current local patterns. |

## Suggested Implementation Order

1. Finish lower-risk logger lifecycle gaps: `log-rotate`, `error-log-logger`, and `file-logger`.
2. Continue high-value auth: `openid-connect`, `multi-auth`, then SSO-style plugins.
3. Build the AI protocol abstraction and apply it to `ai-proxy`, `ai-proxy-multi`, and prompt plugins.
4. Improve observability starting with `zipkin`, then `prometheus`, then `datadog`.
5. Work through Others in small slices: `workflow`, validation/transformation plugins, then protocol bridge plugins.
