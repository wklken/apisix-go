# HTTP Plugin Capability Migration Manifest

**Baseline:** `origin/master@e61cb4db62c025d7ea79335551439e2afc35a918` (merged Plan 13 scoped-rewrite baseline)

**Purpose:** This is the exact planning source for Plans 12–17. It maps every registered HTTP plugin identity to one or more explicit capabilities and its primary migration PR. Production code ultimately owns the same static table; the final completeness test compares it against every factory key in `pkg/plugin/init.go`.

## Identity and capability rules

- The registry contains 115 factory keys and 114 implementation identities.
- `otel` is a factory alias of the `opentelemetry` identity and must resolve to identical capabilities.
- `request-context` is the factory key; `request_context` is only that plugin's `GetName` identity and is not a second factory.
- Capabilities are: `system`, `request_rewrite`, `consumer_rewrite`, `request_access`, `before_proxy`, `conditional_terminal`, `header_filter`, `buffered_body_filter`, `streaming_body_filter`, `protocol_owner`, `log`, `finalizer`, `generation_owner`, `separate_subsystem`.
- Multiple `conditional_terminal` plugins are legal and ordered by scope/stage/priority. `protocol_owner` is exclusive only when the route build matrix says two protocol owners cannot coexist.
- `log` body observation is not `buffered_body_filter`; it uses the bounded log-capture observer from Plan 17.
- A later plan may implement an already-declared secondary capability, but it may not add an unlisted identity or capability without updating this manifest and its completeness test first.

## Plan 12 pre-migrated lifecycle owner

| Identity | Capabilities | Primary plan |
| --- | --- | --- |
| limit-conn | request_access, conditional_terminal, finalizer | 12 |

## Plan 13 scoped rewrite identities

| Identity | Capabilities | Primary plan |
| --- | --- | --- |
| ai-prompt-decorator | request_rewrite, conditional_terminal | 13 |
| ai-prompt-template | request_rewrite, conditional_terminal | 13 |
| ai-rag | request_rewrite, conditional_terminal | 13 |
| ai-request-rewrite | request_rewrite, conditional_terminal | 13 |
| data-mask | request_rewrite, conditional_terminal | 13 |
| degraphql | request_rewrite, conditional_terminal | 13 |
| example-plugin | request_rewrite, system, separate_subsystem | 13/17 |
| jwe-decrypt | request_rewrite, conditional_terminal | 13 |
| proxy-control | request_rewrite | 13 |
| proxy-mirror | request_rewrite, before_proxy | 13 |
| proxy-rewrite | request_rewrite | 13 |
| real-ip | request_rewrite | 13 |
| request-context | system, request_rewrite, finalizer | 11/12/17 |
| request-id | request_rewrite, conditional_terminal | 12/13 |
| traffic-label | request_rewrite | 13 |
| traffic-split | request_rewrite, conditional_terminal | 13 |

## Plan 14 consumer and access identities

| Identity | Capabilities | Primary plan |
| --- | --- | --- |
| acl | request_access, conditional_terminal | 14 |
| ai-aws-content-moderation | request_access, conditional_terminal | 14 |
| ai-prompt-guard | request_access, conditional_terminal | 14 |
| attach-consumer-label | consumer_rewrite | 14 |
| authz-casbin | request_access, conditional_terminal | 14 |
| authz-casdoor | request_access, conditional_terminal | 14 |
| authz-keycloak | request_access, conditional_terminal | 14 |
| basic-auth | request_access, conditional_terminal | 14 |
| cas-auth | request_access, conditional_terminal | 14 |
| chaitin-waf | request_access, conditional_terminal | 14 |
| client-control | request_access, conditional_terminal | 14 |
| consumer-restriction | request_access, conditional_terminal | 14 |
| csrf | request_access, conditional_terminal | 14 |
| dingtalk-auth | request_access, conditional_terminal | 14 |
| feishu-auth | request_access, conditional_terminal | 14 |
| forward-auth | request_access, conditional_terminal | 14 |
| graphql-limit-count | request_access, conditional_terminal | 14 |
| hmac-auth | request_access, conditional_terminal | 14 |
| ip-restriction | request_access, conditional_terminal | 14 |
| jwt-auth | request_access, conditional_terminal | 14 |
| key-auth | request_access, conditional_terminal | 14 |
| ldap-auth | request_access, conditional_terminal | 14 |
| limit-count | request_access, conditional_terminal | 14 |
| limit-req | request_access, conditional_terminal | 14 |
| multi-auth | request_access, conditional_terminal | 14 |
| oas-validator | request_access, conditional_terminal | 14 |
| opa | request_access, conditional_terminal | 14 |
| openid-connect | request_access, conditional_terminal | 14 |
| referer-restriction | request_access, conditional_terminal | 14 |
| request-validation | request_access, conditional_terminal | 14 |
| saml-auth | request_access, conditional_terminal | 14 |
| ua-restriction | request_access, conditional_terminal | 14 |
| uri-blocker | request_access, conditional_terminal | 14 |
| wolf-rbac | request_access, conditional_terminal | 14 |
| workflow | request_access, conditional_terminal | 14 |

## Plan 15 bounded response identities

| Identity | Capabilities | Primary plan |
| --- | --- | --- |
| api-breaker | request_access, conditional_terminal, finalizer | 15 |
| body-transformer | buffered_body_filter | 15 |
| echo | header_filter, buffered_body_filter | 15 |
| error-page | header_filter, buffered_body_filter | 15 |
| exit-transformer | header_filter, buffered_body_filter | 15 |
| graphql-proxy-cache | request_access, conditional_terminal, header_filter, buffered_body_filter | 15 |
| proxy-cache | request_access, conditional_terminal, header_filter, buffered_body_filter | 15 |
| response-rewrite | header_filter, buffered_body_filter | 15 |
| serverless-pre-function | request_rewrite, request_access, conditional_terminal, header_filter, buffered_body_filter, log | 15/17 |
| serverless-post-function | request_rewrite, request_access, conditional_terminal, header_filter, buffered_body_filter, log | 15/17 |

`api-breaker` observes final status through a per-request lifecycle finalizer; it is not a header/body mutator. Cache-store runs after the final transformed representation, not as an unordered peer body filter. Serverless `phase=log` remains deferred until Plan 17 even though its other configured phases migrate in Plan 15.

## Plan 16 streaming, protocol, and terminal identities

| Identity | Capabilities | Primary plan |
| --- | --- | --- |
| ai-aliyun-content-moderation | request_access, conditional_terminal, buffered_body_filter, streaming_body_filter | 16 |
| ai-proxy | conditional_terminal, protocol_owner, streaming_body_filter | 16 |
| ai-proxy-multi | conditional_terminal, protocol_owner, streaming_body_filter | 16 |
| ai-rate-limiting | request_access, conditional_terminal, buffered_body_filter, streaming_body_filter, finalizer | 16 |
| aws-lambda | conditional_terminal, protocol_owner | 16 |
| azure-functions | conditional_terminal, protocol_owner | 16 |
| brotli | header_filter, streaming_body_filter | 16 |
| cors | request_access, conditional_terminal, header_filter, streaming_body_filter | 16 |
| dubbo-proxy | request_rewrite, conditional_terminal, protocol_owner, separate_subsystem | 16 |
| fault-injection | request_access, conditional_terminal | 16 |
| grpc-transcode | request_rewrite, conditional_terminal, header_filter, streaming_body_filter, protocol_owner | 16 |
| grpc-web | request_rewrite, conditional_terminal, header_filter, streaming_body_filter, protocol_owner | 16 |
| gzip | header_filter, streaming_body_filter | 16 |
| http-dubbo | request_rewrite, conditional_terminal, protocol_owner, separate_subsystem | 16 |
| kafka-proxy | request_rewrite, conditional_terminal, protocol_owner, separate_subsystem | 16 |
| mcp-bridge | conditional_terminal, streaming_body_filter, protocol_owner | 16 |
| mocking | conditional_terminal | 16 |
| mqtt-proxy | protocol_owner, separate_subsystem | 16 |
| openfunction | conditional_terminal, protocol_owner | 16 |
| openwhisk | conditional_terminal, protocol_owner | 16 |
| proxy-buffering | request_rewrite, streaming_body_filter | 16 |
| public-api | conditional_terminal, protocol_owner, separate_subsystem | 16 |
| redirect | conditional_terminal | 16 |

Route-owned Kafka websocket and Dubbo/http-Dubbo terminal behavior is part of the response-plan provenance even where the plugin Handler itself only prepares context. MQTT remains a separately owned TCP stream subsystem and must not enter HTTP middleware execution.

## Plan 17 log, tracing, system, and closure identities

| Identity | Capabilities | Primary plan |
| --- | --- | --- |
| ai | system | 17 |
| batch-requests | system, separate_subsystem | 17 |
| clickhouse-logger | log | 17 |
| datadog | log | 17 |
| elasticsearch-logger | log | 17 |
| error-log-logger | system, generation_owner, separate_subsystem | 17 |
| file-logger | log | 17 |
| gm | system | 17 |
| google-cloud-logging | log | 17 |
| http-logger | log | 17 |
| kafka-logger | log | 17 |
| lago | log | 17 |
| log-rotate | request_access, system, generation_owner | 17 |
| loggly | log | 17 |
| loki-logger | log | 17 |
| node-status | system, separate_subsystem | 17 |
| opentelemetry | request_rewrite, finalizer | 17 |
| prometheus | system, separate_subsystem | 17 |
| rocketmq-logger | log | 17 |
| server-info | system, generation_owner, separate_subsystem | 17 |
| skywalking | request_rewrite, finalizer | 17 |
| skywalking-logger | log | 17 |
| sls-logger | log | 17 |
| splunk-hec-logging | log | 17 |
| syslog | log | 17 |
| tcp-logger | log | 17 |
| tencent-cloud-cls | log | 17 |
| udp-logger | log | 17 |
| zipkin | request_rewrite, finalizer | 17 |

`finalizer` means a per-request `FinalizerPhasePlugin` only. `generation_owner` covers process/route-generation Start/Stop ownership and never installs a per-request finalizer. Existing `error-log-logger`, `log-rotate`, and `server-info` generation lifecycles are audited for exactly-once retirement but do not enter `RunLogPhase`; `node-status` remains a separately installed server wrapper.

## Conditional-terminal request-stage manifest

Every `conditional_terminal` identity has exactly one execution owner at runtime. An identity already executed through `RequestPhasePlugin` returns `Continue` or `Stop` there and is not also installed as a `TerminalPlugin`. `TerminalPlugin` is reserved for terminal-only and route-owned protocol owners below. Effective scope and priority still come from the materialized `Binding`.

| Request stage | Identities |
| --- | --- |
| `request_rewrite` | ai-prompt-decorator, ai-prompt-template, ai-rag, ai-request-rewrite, data-mask, degraphql, jwe-decrypt, request-id, traffic-split, grpc-transcode, grpc-web |
| `request_access` | limit-conn, acl, ai-aws-content-moderation, ai-prompt-guard, authz-casbin, authz-casdoor, authz-keycloak, basic-auth, cas-auth, chaitin-waf, client-control, consumer-restriction, csrf, dingtalk-auth, feishu-auth, forward-auth, graphql-limit-count, hmac-auth, ip-restriction, jwt-auth, key-auth, ldap-auth, limit-count, limit-req, multi-auth, oas-validator, opa, openid-connect, referer-restriction, request-validation, saml-auth, ua-restriction, uri-blocker, wolf-rbac, workflow, api-breaker, graphql-proxy-cache, proxy-cache, ai-aliyun-content-moderation, ai-rate-limiting, cors, fault-injection |
| `request_rewrite` or `request_access` (validated config) | serverless-pre-function, serverless-post-function; only these two stages are accepted and the materialized config selects one |
| `before_proxy` terminal owner | ai-proxy, ai-proxy-multi, aws-lambda, azure-functions, dubbo-proxy, http-dubbo, kafka-proxy, mcp-bridge, mocking, openfunction, openwhisk, public-api, redirect |

The completeness test derives the set of all `conditional_terminal` rows above and rejects a missing, duplicate, or extra stage declaration.

## Completeness acceptance

The final test must prove all of the following in one deterministic check:

1. Every registry factory key has an entry after alias normalization.
2. Every manifest identity has at least one capability and a primary migration plan.
3. No manifest identity is absent from the registry except the normalized implementation names documented above.
4. No plugin installed by production route assembly retains unclassified post-`next`, response-writer, flush, hijack, log, or cleanup behavior.
5. Multi-capability plugins execute each declared phase at most once; route-owned/separate-subsystem owners are never duplicated in the generic executor.

Until this acceptance passes on the final Plan 17 tree, PR-014 and the remaining P1 5.5 are not closed.
