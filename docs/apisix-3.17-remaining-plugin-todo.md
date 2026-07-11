# APISIX 3.17 Remaining Plugin TODO Plan

This is the active TODO list for remaining APISIX 3.17 plugin parity work. It lists only work that is not done yet and groups it into five parts: Logger, Auth, AI, Observability, and Others.

OpenResty-native, NGINX-native, Lua-runtime-native, serverless function plugins, and missing/deferred official defaults are not normal TODOs. Not required unless explicitly requested: `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, `inspect`, `serverless-pre-function`, `serverless-post-function`, OCSP stapling internals, exact OpenResty phase timing, `ngx_lua` APIs, Lua code execution, NGINX buffering internals, and shared-dict/lrucache exactness. Do not create normal plugin implementation tasks for these native/runtime-only items.

Use this file as the planning backlog. Use `README.md` and `docs/apisix-3.17-plugin-parity-checklist.md` as the status surfaces after each implemented slice.

## Logger

Shared logger batch processor behavior now includes route/server-aware `batch_process_entries`, complete-label-only
metric emission, `max_pending_entries`, retries, and graceful reload/shutdown buffer flushing. Exact APISIX Lua
`batch-processor-manager` stale-object cache cleanup is runtime-cache behavior and is not a normal Go logger TODO.

| Plugin | What needs to be done |
|---|---|
| `http-logger` | Only remaining normal parity is encrypted `auth_header` after a project-level secret design exists. |
| `skywalking-logger` | No normal Go logger TODO remains. |
| `tcp-logger` | No normal Go logger TODO remains; OpenResty cosocket pooling is not required. |
| `kafka-logger` | Only remaining gap is encrypted broker password storage after a project-level secret design exists. |
| `rocketmq-logger` | Remaining gaps are encrypted `secret_key` and `use_tls`; the current RocketMQ Go client exposes no TLS option. |
| `syslog` | No normal Go logger TODO remains; native connection pooling/TLS exactness is not required. |
| `udp-logger` | No normal Go logger TODO remains. |
| `clickhouse-logger` | Only remaining normal parity is encrypted `password` after a project-level secret design exists. |
| `sls-logger` | Only remaining normal parity is encrypted `access_key_secret` after a project-level secret design exists. |
| `google-cloud-logging` | Remaining gaps are encrypted `auth_config.private_key` and request/response body capture only if a future APISIX version defines it. |
| `splunk-hec-logging` | Remaining gaps are encrypted `endpoint.token` and request/response body capture only if a future APISIX version defines it. |
| `elasticsearch-logger` | Only remaining normal parity is encrypted `auth.password` after a project-level secret design exists. |
| `loggly` | Only remaining normal parity is encrypted `customer_token` after a project-level secret design exists. |
| `loki-logger` | No normal Go logger TODO remains. |
| `tencent-cloud-cls` | Only remaining normal parity is encrypted `secret_key` after a project-level secret design exists. The upstream APISIX 3.17 SDK has an lz4/zstd TODO but no plugin config/feature to match. |
| `lago` | Remaining gaps are encrypted `token` and exotic OpenResty/NGINX-only variable fidelity. APISIX `batch_max_size` default of 100, request-start event timestamps, and common dynamic request/response variables are implemented. |
| `error-log-logger` | Remaining normal parity is encrypted metadata fields after a project-level secret design exists. Keep direct `ngx.errlog` capture, OpenResty timer lifecycle, route/server `batch_process_entries` labels for global explicit delivery, stale batch-manager cache cleanup, and Lua-resty-kafka producer cache exactness out of normal scope. |

## Auth

| Plugin | What needs to be done |
|---|---|
| `openid-connect` | No normal Go Auth TODO remains. Cookie/Redis sessions, encrypted configuration fields, and `refresh_session_interval` are implemented; exact `lua-resty-session` runtime behavior remains out of scope. |
| `key-auth` | No normal Go Auth TODO remains; encrypted consumer fields, API-key extraction, hide-credentials, and anonymous-consumer behavior are implemented. |
| `jwt-auth` | No normal Go Auth TODO remains; encrypted consumer fields, asymmetric algorithms, and anonymous-consumer behavior are implemented. |
| `basic-auth` | No normal Go Auth TODO remains; encrypted consumer fields, Basic extraction, consumer attachment, and `hide_credentials` are implemented. |
| `hmac-auth` | No normal Go Auth TODO remains; encrypted consumer fields, digest/body validation, `hide_credentials`, and anonymous-consumer behavior are implemented. |
| `saml-auth` | No normal Go Auth TODO remains; encrypted `sp_private_key` / `secret` fields are implemented. Exact `lua-resty-saml` session/runtime behavior remains out of scope. |
| `cas-auth` | No normal Go Auth TODO remains; encrypted `cookie.secret` is implemented. OpenResty shared-dict clustering is out of scope; user metadata forwarding is not an APISIX 3.17 plugin behavior. |
| `authz-casdoor` | No normal Go Auth TODO remains; encrypted `client_secret` is implemented. Distributed/exact `resty.session` behavior is out of scope; user/access-token forwarding is not an APISIX 3.17 plugin behavior. |
| `authz-keycloak` | No normal Go Auth TODO remains. Encrypted `client_secret`, discovery/service-account token caches, and `cache_ttl_seconds` are implemented; Lua `http_request_decorator` functions and cross-process shared-dict fidelity are out of scope. |
| `wolf-rbac` | No normal Go Auth TODO remains. Public login/change-password/user-info APIs, TLS verification, transient 5xx retry/backoff, and consumer configuration are implemented; cross-process OpenResty consumer-cache fidelity is out of scope. |
| `ldap-auth` | No normal Go Auth TODO remains; encrypted consumer `user_dn` is implemented. Do not add LDAP search filters, StartTLS, or `anonymous_consumer`; APISIX 3.17 does not define them. |
| `jwe-decrypt` | No normal Go Auth TODO remains; encrypted consumer fields are implemented. Do not add alternate algorithms or AAD/header authentication; APISIX 3.17 uses direct AES-256-GCM with the same bounded behavior. |
| `multi-auth` | All APISIX 3.17 `type = auth` plugins are supported. Preserve APISIX's generic final denial response; per-plugin diagnostics are logging detail. |
| `dingtalk-auth` | No normal Go Auth TODO remains. Encrypted configuration fields, `$external_user`, and the local access-token cache are implemented; exact OpenResty logging/session/cache behavior is out of scope. |
| `feishu-auth` | No normal Go Auth TODO remains. Encrypted configuration fields and `$external_user` are implemented; exact OpenResty logging/session behavior is out of scope. |
| `authz-casbin` | No normal Go Auth TODO remains; plugin-metadata model/policy fallback and metadata reload are implemented. |
| `opa` | No normal Go Auth TODO remains. `with_route` / `with_service` emit full route/service resource documents from the route builder, with bounded direct-plugin fallback context. |
| `forward-auth` | No normal Go Auth TODO remains. APISIX 3.17's schema accepts string `extra_headers` values; its defensive numeric runtime fallback is not a normal configurable feature. |

## AI

| Plugin | What needs to be done |
|---|---|
| `ai` | Keep deferred unless a Go-native equivalent is explicitly requested; most APISIX behavior replaces runtime/router/balancer internals. |
| `ai-proxy-multi` | Deferred large/runtime item: explicit per-address DNS snapshots and per-IP health state. Standard Go HTTP already resolves DNS while preserving hostname/SNI; all official user-facing config is accepted and active. |
| `ai-rate-limiting` | Deferred native item: cross-process shared counters and exact OpenResty log-phase timing. All official normal config and single-process behavior are implemented. |
| `ai-prompt-template` | Deferred native item: exact OpenResty template LRU behavior. APISIX 3.17 hardcodes JSON input; its normal template behavior is implemented. |
| `ai-prompt-guard` | Deferred native item: exact OpenResty PCRE flags/engine behavior. All official config and protocol extraction behavior are implemented. |
| `ai-aliyun-content-moderation` | Deferred native item: exact OpenResty body-filter chunk timing. Request/response, interval/cache-triggered realtime streams, final-packet annotation, session reuse, and risk variables are implemented. |

## Observability

| Plugin | What needs to be done |
|---|---|
| `zipkin` | No normal Go TODO remains. Multi-phase spans, `set_ngx_var`, exact phase timing, and OpenResty batch behavior remain deferred native/runtime items. |
| `skywalking` | No normal Go TODO remains. Native OpenResty tracer/shared-buffer behavior and exact delayed body-filter/streaming phase timing remain deferred. |
| `opentelemetry` | No normal Go TODO remains. `set_ngx_var`, phase child spans, and exact OpenResty log-phase timing remain deferred native/runtime items. |
| `prometheus` | No normal route/plugin-attribute TODO remains. Metric expiration, privileged-agent offload, exact NGINX lifecycle counters, and stream metrics remain deferred native/runtime items. |
| `node-status` | No normal Go TODO remains. Exact NGINX reading/writing/waiting connection-state counters remain deferred native behavior. |
| `datadog` | No normal Go TODO remains. Exact OpenResty log-phase timing and stale batch-manager object-cache behavior remain deferred runtime details. |

## Others

| Plugin | What needs to be done |
|---|---|
| `batch-requests` | No normal Go TODO remains. `ssl_verify` is accepted but inapplicable to in-process dispatch; true HTTP pipelining and NGINX real-ip header config remain deferred native/runtime behavior. |
| `redirect` | No normal Go TODO remains. Explicit `https_port`, `apisix.ssl.listen` fallback, forwarded schemes, query preservation, and host/port replacement are implemented. |
| `echo` | No normal Go TODO remains. Body timing and official body/header schema edge behavior are covered. |
| `gzip` | No normal implementation work; only documentation/tests remain because NGINX `buffers` is native and not required. |
| `brotli` | No normal supported-library TODO remains. `lgwin` is active; the Go encoder exposes neither `mode` nor `lgblock`, and NGINX streaming internals remain out of scope. |
| `real-ip` | Improve variable-source coverage and schema validation where practical. Keep NGINX variable cache flushing and APISIX-Base `set_real_ip` internals out of scope. |
| `server-info` | Add UID persistence and etcd reporting/lease keepalive if useful for the Go control plane. |
| `error-page` | Limit rewrites to APISIX-generated errors if the Go response pipeline can distinguish them; expose metadata schema if local plugin interfaces support it. |
| `exit-transformer` | Support more documented non-Lua response transformations. Arbitrary Lua execution and `core.response.exit()` callback fidelity are out of scope. |
| `attach-consumer-label` | Add non-string label serialization if APISIX source requires it and local consumer labels preserve types. |
| `azure-functions` | No normal Go TODO remains. Encrypted route/metadata keys, authorization precedence, wildcard `:ext` forwarding, and HTTP/2 response filtering are implemented. |
| `openfunction` | No normal Go TODO remains. Encrypted service tokens, wildcard `:ext` forwarding, and HTTP/2 response filtering are implemented. |
| `openwhisk` | No normal Go TODO remains. Encrypted service tokens, official name validation, and scalar/list result headers and body values are implemented. |
| `aws-lambda` | No normal Go TODO remains. Encrypted API-key/IAM credentials, APISIX-compatible SigV4 canonicalization, and wildcard `:ext` forwarding are implemented. |
| `response-rewrite` | Only remaining normal parity is APISIX secret-reference resolution for `body`. Exact OpenResty PCRE semantics and streaming chunk body filters remain out of scope. |
| `proxy-rewrite` | Only small URI safe-encoding parity/test gaps remain; avoid risky rewrites. |
| `fault-injection` | No normal Go TODO remains. Exact OpenResty PCRE semantics, the complete NGINX variable catalog, and rewrite-phase timing remain deferred native/runtime details. |
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
| `proxy-cache` | Design disk cache zones and stale serving separately; keep current in-memory path stable. |
| `graphql-proxy-cache` | Only deeper GraphQL parser parity remains normal Go work. `graphql.max_size`, route/service/identity cache keys, and the public purge endpoint are implemented; NGINX disk cache zones remain out of scope. |
| `request-validation` | Add missing schema edge cases around headers/forms and improve APISIX-style rejection details. |
| `proxy-mirror` | Add gRPC mirroring and APISIX DNS resolver behavior only if the Go proxy layer exposes enough hooks. |
| `workflow` | Add more delegated actions for implemented plugins and better condition expression coverage. |
| `graphql-limit-count` | Only deeper GraphQL parser parity and exact group-configuration mismatch validation remain. Rules, string expressions, groups, metadata headers, Redis Cluster, and `graphql.max_size` are implemented. |
| `api-breaker` | Only shared-dict and log-phase exactness remain, which are native; add user-visible breaker-window parity only if practical. |
| `traffic-split` | Add inline-upstream `hash_on` / `key` / `pass_host` / `upstream_host` / timeout / retry / health-check behavior where the Go proxy layer can support it. Expression syntax, numeric IDs, route fallback weighting, and explicit zero weights are implemented. |
| `traffic-label` | No normal Go TODO remains. Exact OpenResty cached round-robin behavior, the complete NGINX variable catalog, and access-phase timing remain deferred runtime details. |
| `request-id` | Add plugin-attr snowflake config and etcd-backed machine leasing if local config/store patterns make it practical. |
| `oas-validator` | Add external `$ref`, metadata TTL refresh, more OpenAPI parameter styles, non-JSON body schemas, and response validation. |
| `mcp-bridge` | Deferred native/runtime items: cross-worker shared-dict session recovery and exact `ngx.pipe` timeout/worker-exit semantics. Official ping, SSE/message flow, cancellation-aware backpressure, and subprocess cleanup are implemented. |
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

1. Continue high-value auth: `openid-connect`, `multi-auth`, then SSO-style plugins.
2. Build the AI protocol abstraction and apply it to `ai-proxy`, `ai-proxy-multi`, and prompt plugins.
3. Improve observability starting with `zipkin`, then `prometheus`, then `datadog`.
4. Work through Others in small slices: `workflow`, validation/transformation plugins, then protocol bridge plugins.
5. Revisit logger encrypted-secret parity only after a project-level secret/encryption design exists.
