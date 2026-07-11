# APISIX 3.17 Plugin Parity Checklist

Generated from upstream `apisix/cli/config.lua` on the `release/3.17` branch, local `pkg/plugin/init.go`, and the README plugin matrix.

## Summary

- Upstream APISIX 3.17 default plugin count: 104
- Locally registered default plugins: 100
- Missing default plugins: `ext-plugin-pre-req`, `inspect`, `ext-plugin-post-req`, `ext-plugin-post-resp`
- Classification:
  - `implement`: normal Go parity/config work remains.
  - `defer-native`: OpenResty/NGINX/Lua-runtime-specific behavior.
  - `defer-large`: separate subsystem design needed.
  - `not-required-native`: OpenResty/NGINX/Lua-runtime-native behavior that should not be implemented unless explicitly requested.
  - `monitor`: currently high enough for this pass; revisit after lower-parity plugins.

## Default Plugin Matrix

| Plugin | Registered | README | Next |
|---|---:|---|---|
| `real-ip` | yes | 85% | monitor |
| `ai` | yes | 20% | implement |
| `client-control` | yes | 100% | monitor |
| `proxy-buffering` | yes | 60% | implement |
| `proxy-control` | yes | 60% | implement |
| `request-id` | yes | 85% | monitor |
| `zipkin` | yes | 82% | defer-native |
| `ext-plugin-pre-req` | no | unsupported: No need | not-required-native |
| `fault-injection` | yes | 88% | monitor |
| `mocking` | yes | 97% | monitor |
| `serverless-pre-function` | yes | 45% | not-required-native |
| `cors` | yes | 80% | monitor |
| `ip-restriction` | yes | 90% | monitor |
| `ua-restriction` | yes | 95% | monitor |
| `referer-restriction` | yes | 95% | monitor |
| `csrf` | yes | 72% | monitor |
| `uri-blocker` | yes | 95% | monitor |
| `request-validation` | yes | 85% | monitor |
| `chaitin-waf` | yes | 55% | implement |
| `multi-auth` | yes | 85% | monitor |
| `openid-connect` | yes | 98% | monitor |
| `saml-auth` | yes | 85% | monitor |
| `cas-auth` | yes | 85% | monitor |
| `authz-casbin` | yes | 85% | monitor |
| `authz-casdoor` | yes | 85% | monitor |
| `wolf-rbac` | yes | 75% | implement |
| `ldap-auth` | yes | 75% | monitor |
| `hmac-auth` | yes | 82% | monitor |
| `basic-auth` | yes | 70% | monitor |
| `jwt-auth` | yes | 85% | monitor |
| `jwe-decrypt` | yes | 90% | monitor |
| `key-auth` | yes | 75% | monitor |
| `dingtalk-auth` | yes | 65% | implement |
| `feishu-auth` | yes | 65% | implement |
| `acl` | yes | 70% | monitor |
| `consumer-restriction` | yes | 80% | monitor |
| `attach-consumer-label` | yes | 70% | monitor |
| `forward-auth` | yes | 90% | monitor |
| `opa` | yes | 90% | monitor |
| `authz-keycloak` | yes | 85% | monitor |
| `data-mask` | yes | 65% | implement |
| `proxy-cache` | yes | 78% | monitor |
| `body-transformer` | yes | 55% | implement |
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
| `graphql-proxy-cache` | yes | 88% | monitor |
| `proxy-rewrite` | yes | 98% | monitor |
| `workflow` | yes | 70% | implement |
| `api-breaker` | yes | 95% | monitor |
| `graphql-limit-count` | yes | 95% | monitor |
| `limit-conn` | yes | 96% | monitor |
| `limit-count` | yes | 98% | monitor |
| `limit-req` | yes | 96% | monitor |
| `gzip` | yes | 98% | monitor |
| `traffic-label` | yes | 63% | implement |
| `traffic-split` | yes | 80% | monitor |
| `redirect` | yes | 95% | monitor |
| `response-rewrite` | yes | 84% | monitor |
| `oas-validator` | yes | 62% | implement |
| `mcp-bridge` | yes | 75% | defer-native |
| `degraphql` | yes | 65% | implement |
| `kafka-proxy` | yes | 35% | implement |
| `grpc-transcode` | yes | 55% | implement |
| `grpc-web` | yes | 68% | implement |
| `http-dubbo` | yes | 55% | implement |
| `public-api` | yes | 60% | implement |
| `prometheus` | yes | 82% | defer-native |
| `datadog` | yes | 88% | monitor |
| `lago` | yes | 76% | monitor |
| `loki-logger` | yes | 76% | monitor |
| `elasticsearch-logger` | yes | 84% | monitor |
| `echo` | yes | 90% | monitor |
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
| `example-plugin` | yes | 60% | implement |
| `aws-lambda` | yes | 95% | monitor |
| `azure-functions` | yes | 95% | monitor |
| `openwhisk` | yes | 93% | monitor |
| `openfunction` | yes | 94% | monitor |
| `serverless-post-function` | yes | 45% | not-required-native |
| `ext-plugin-post-req` | no | unsupported: No need | not-required-native |
| `ext-plugin-post-resp` | no | unsupported: No need | not-required-native |
| `ai-request-rewrite` | yes | 90% | monitor |

## Next Priority Lanes

1. Raise high-value auth parity: `key-auth`, `jwt-auth`, `hmac-auth`, then `openid-connect`.
2. Raise traffic parity where Go-native support is practical: Redis policies for `limit-*`, and additional `workflow` delegated plugin actions.
3. Finish the remaining AI provider conversions, Bedrock EventStream, and multi-instance health/DNS behavior.
4. Keep OpenResty/NGINX/Lua-runtime-native plugins out of normal parity work: `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, `inspect`, `serverless-pre-function`, and `serverless-post-function`.
