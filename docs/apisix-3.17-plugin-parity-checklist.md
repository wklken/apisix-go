# APISIX 3.17 Plugin Parity Checklist

Generated from upstream `apisix/cli/config.lua` on the `release/3.17` branch, local `pkg/plugin/init.go`, and the README plugin matrix.

## Summary

- Upstream APISIX 3.17 default plugin count: 104
- Locally registered default plugins: 100
- Missing default plugins: `ext-plugin-pre-req`, `inspect`, `ext-plugin-post-req`, `ext-plugin-post-resp`
- Registration smoke test: `pkg/plugin/TestNewRegistersGoScopeAPISIX317Defaults` instantiates all 100 locally registered entries.
- Currently high-enough for the documented Go-native scope (`monitor`): 89
- Normal parity work remaining (`implement`): 0
- Deferred native/runtime or separate-subsystem work: 9 (`defer-native`: 6, `defer-large`: 3)
- Explicitly not required in normal parity: 6 (`not-required-native`)
- Classification:
  - `implement`: normal Go parity/config work remains; currently no plugin is in this state.
  - `defer-native`: OpenResty/NGINX/Lua-runtime-specific behavior.
  - `defer-large`: separate subsystem design needed.
  - `not-required-native`: OpenResty/NGINX/Lua-runtime-native behavior that should not be implemented unless explicitly requested.
  - `monitor`: currently high enough for this pass; revisit after lower-parity plugins.

## Default Plugin Matrix

| Plugin | Registered | README | Next |
|---|---:|---|---|
| `real-ip` | yes | 90% | monitor |
| `ai` | yes | 20% | defer-native |
| `client-control` | yes | 100% | monitor |
| `proxy-buffering` | yes | 75% | monitor |
| `proxy-control` | yes | 75% | monitor |
| `request-id` | yes | 90% | monitor |
| `zipkin` | yes | 82% | defer-native |
| `ext-plugin-pre-req` | no | unsupported: No need | not-required-native |
| `fault-injection` | yes | 97% | monitor |
| `mocking` | yes | 97% | monitor |
| `serverless-pre-function` | yes | 45% | not-required-native |
| `cors` | yes | 87% | monitor |
| `ip-restriction` | yes | 95% | monitor |
| `ua-restriction` | yes | 95% | monitor |
| `referer-restriction` | yes | 98% | monitor |
| `csrf` | yes | 80% | monitor |
| `uri-blocker` | yes | 95% | monitor |
| `request-validation` | yes | 90% | monitor |
| `chaitin-waf` | yes | 80% | monitor |
| `multi-auth` | yes | 85% | monitor |
| `openid-connect` | yes | 98% | monitor |
| `saml-auth` | yes | 85% | monitor |
| `cas-auth` | yes | 85% | monitor |
| `authz-casbin` | yes | 85% | monitor |
| `authz-casdoor` | yes | 85% | monitor |
| `wolf-rbac` | yes | 75% | monitor |
| `ldap-auth` | yes | 75% | monitor |
| `hmac-auth` | yes | 82% | monitor |
| `basic-auth` | yes | 70% | monitor |
| `jwt-auth` | yes | 85% | monitor |
| `jwe-decrypt` | yes | 90% | monitor |
| `key-auth` | yes | 75% | monitor |
| `dingtalk-auth` | yes | 65% | monitor |
| `feishu-auth` | yes | 65% | monitor |
| `acl` | yes | 78% | monitor |
| `consumer-restriction` | yes | 85% | monitor |
| `attach-consumer-label` | yes | 80% | monitor |
| `forward-auth` | yes | 90% | monitor |
| `opa` | yes | 78% | monitor |
| `authz-keycloak` | yes | 85% | monitor |
| `data-mask` | yes | 80% | monitor |
| `proxy-cache` | yes | 99% | monitor |
| `body-transformer` | yes | 88% | monitor |
| `ai-prompt-template` | yes | 95% | defer-native |
| `ai-prompt-decorator` | yes | 90% | monitor |
| `ai-prompt-guard` | yes | 88% | monitor |
| `ai-rag` | yes | 98% | monitor |
| `ai-rate-limiting` | yes | 90% | monitor |
| `ai-proxy-multi` | yes | 98% | defer-large |
| `ai-proxy` | yes | 99% | monitor |
| `ai-aws-content-moderation` | yes | 98% | monitor |
| `ai-aliyun-content-moderation` | yes | 96% | defer-native |
| `proxy-mirror` | yes | 73% | monitor |
| `graphql-proxy-cache` | yes | 95% | monitor |
| `proxy-rewrite` | yes | 99% | monitor |
| `workflow` | yes | 85% | monitor |
| `api-breaker` | yes | 95% | monitor |
| `graphql-limit-count` | yes | 98% | monitor |
| `limit-conn` | yes | 96% | monitor |
| `limit-count` | yes | 98% | monitor |
| `limit-req` | yes | 96% | monitor |
| `gzip` | yes | 98% | monitor |
| `traffic-label` | yes | 96% | monitor |
| `traffic-split` | yes | 97% | monitor |
| `redirect` | yes | 95% | monitor |
| `response-rewrite` | yes | 97% | monitor |
| `oas-validator` | yes | 99% | monitor |
| `mcp-bridge` | yes | 75% | defer-native |
| `degraphql` | yes | 78% | monitor |
| `kafka-proxy` | yes | 75% | monitor |
| `grpc-transcode` | yes | 95% | defer-large |
| `grpc-web` | yes | 88% | defer-large |
| `http-dubbo` | yes | 75% | monitor |
| `public-api` | yes | 75% | monitor |
| `prometheus` | yes | 82% | defer-native |
| `datadog` | yes | 88% | monitor |
| `lago` | yes | 76% | monitor |
| `loki-logger` | yes | 76% | monitor |
| `elasticsearch-logger` | yes | 84% | monitor |
| `echo` | yes | 95% | monitor |
| `loggly` | yes | 76% | monitor |
| `http-logger` | yes | 76% | monitor |
| `splunk-hec-logging` | yes | 62% | monitor |
| `skywalking-logger` | yes | 76% | monitor |
| `google-cloud-logging` | yes | 67% | monitor |
| `sls-logger` | yes | 72% | monitor |
| `tcp-logger` | yes | 70% | monitor |
| `kafka-logger` | yes | 76% | monitor |
| `rocketmq-logger` | yes | 72% | monitor |
| `syslog` | yes | 70% | monitor |
| `udp-logger` | yes | 70% | monitor |
| `file-logger` | yes | 82% | monitor |
| `clickhouse-logger` | yes | 76% | monitor |
| `tencent-cloud-cls` | yes | 76% | monitor |
| `inspect` | no | unsupported: lua feature | not-required-native |
| `example-plugin` | yes | 80% | monitor |
| `aws-lambda` | yes | 95% | monitor |
| `azure-functions` | yes | 95% | monitor |
| `openwhisk` | yes | 93% | monitor |
| `openfunction` | yes | 94% | monitor |
| `serverless-post-function` | yes | 45% | not-required-native |
| `ext-plugin-post-req` | no | unsupported: No need | not-required-native |
| `ext-plugin-post-resp` | no | unsupported: No need | not-required-native |
| `ai-request-rewrite` | yes | 90% | monitor |

## Registered non-default protocol entries

These plugins are registered in the Go runtime and tracked by the execution TODO, but are not part of the 104-entry APISIX default-plugin count above.

| Plugin | Registered | README | Next |
|---|---:|---|---|
| `dubbo-proxy` | yes | 82% | defer-large |
| `mqtt-proxy` | yes | 65% | defer-large |

## Next Priority Lanes

1. Monitor `oas-validator` for other uncommon nested/explode combinations after its bounded request-validation surface and route-chain gate; any further work requires a concrete mismatch, while `grpc-transcode` and `grpc-web` streaming remain separate subsystem work.
2. Monitor `body-transformer` (reserved-helper protection, shared `_ctx.var.<name>` resolution, bounded single-/double-quoted Lua string literals, and bounded `elseif` branches are now covered), `data-mask` (bounded `max_req_post_args` prefix parsing and root-array JSONPath selectors), `acl` (including bounded `$..field[.suffix]` external-user lookup), `chaitin-waf`, and the smaller HTTP/admin plugins; `traffic-split` now also supports APISIX `vars_combinations` templates, explicit zero-weight exclusion for referenced resources, bracketed IPv6 node targets, and passive upstream health thresholds through the shared Go selector, while active probes remain deferred.
3. Follow [`apisix-3.17-protocol-bridge-design.md`](apisix-3.17-protocol-bridge-design.md) for the remaining transport work for `kafka-proxy`, `dubbo-proxy`, `http-dubbo`, and `mqtt-proxy`; the official Kafka PubSub protobuf owner, list-offset/fetch consumer, bounded TLS options, local SSL-resource ID resolution, sanitized fake auth/timeout mapping, and an in-process TLS/SASL wire fixture are implemented, while external broker smoke tests, persistent Dubbo multiplexing, and MQTT general stream-variable/plugin-chain/mTLS behavior remain deferred.
4. Keep the shared secret resolver as the gate for new credential-bearing fields; all currently integrated strict boundaries, including `csrf.key`, are migrated, while ordinary `response-rewrite.body` remains an explicitly accepted compatibility field.
5. Keep OpenResty/NGINX/Lua-runtime-native plugins out of normal parity work: `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, `inspect`, `serverless-pre-function`, and `serverless-post-function`.
