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
| `zipkin` | yes | 45% | implement |
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
| `multi-auth` | yes | 60% | implement |
| `openid-connect` | yes | 68% | implement |
| `saml-auth` | yes | 55% | implement |
| `cas-auth` | yes | 60% | implement |
| `authz-casbin` | yes | 70% | monitor |
| `authz-casdoor` | yes | 60% | implement |
| `wolf-rbac` | yes | 65% | implement |
| `ldap-auth` | yes | 65% | implement |
| `hmac-auth` | yes | 82% | monitor |
| `basic-auth` | yes | 70% | monitor |
| `jwt-auth` | yes | 85% | monitor |
| `jwe-decrypt` | yes | 65% | implement |
| `key-auth` | yes | 75% | monitor |
| `dingtalk-auth` | yes | 60% | implement |
| `feishu-auth` | yes | 60% | implement |
| `acl` | yes | 70% | monitor |
| `consumer-restriction` | yes | 80% | monitor |
| `attach-consumer-label` | yes | 70% | monitor |
| `forward-auth` | yes | 86% | monitor |
| `opa` | yes | 70% | monitor |
| `authz-keycloak` | yes | 60% | implement |
| `data-mask` | yes | 65% | implement |
| `proxy-cache` | yes | 78% | monitor |
| `body-transformer` | yes | 55% | implement |
| `ai-prompt-template` | yes | 55% | implement |
| `ai-prompt-decorator` | yes | 55% | implement |
| `ai-prompt-guard` | yes | 60% | implement |
| `ai-rag` | yes | 55% | implement |
| `ai-rate-limiting` | yes | 50% | implement |
| `ai-proxy-multi` | yes | 58% | implement |
| `ai-proxy` | yes | 58% | implement |
| `ai-aws-content-moderation` | yes | 55% | implement |
| `ai-aliyun-content-moderation` | yes | 50% | implement |
| `proxy-mirror` | yes | 73% | monitor |
| `graphql-proxy-cache` | yes | 55% | implement |
| `proxy-rewrite` | yes | 98% | monitor |
| `workflow` | yes | 70% | implement |
| `api-breaker` | yes | 95% | monitor |
| `graphql-limit-count` | yes | 62% | implement |
| `limit-conn` | yes | 87% | monitor |
| `limit-count` | yes | 86% | monitor |
| `limit-req` | yes | 84% | monitor |
| `gzip` | yes | 98% | monitor |
| `traffic-label` | yes | 63% | implement |
| `traffic-split` | yes | 80% | monitor |
| `redirect` | yes | 90% | monitor |
| `response-rewrite` | yes | 84% | monitor |
| `oas-validator` | yes | 62% | implement |
| `mcp-bridge` | yes | 55% | implement |
| `degraphql` | yes | 65% | implement |
| `kafka-proxy` | yes | 35% | implement |
| `grpc-transcode` | yes | 55% | implement |
| `grpc-web` | yes | 68% | implement |
| `http-dubbo` | yes | 55% | implement |
| `public-api` | yes | 60% | implement |
| `prometheus` | yes | 45% | implement |
| `datadog` | yes | 68% | implement |
| `lago` | yes | 76% | monitor |
| `loki-logger` | yes | 76% | implement |
| `elasticsearch-logger` | yes | 84% | monitor |
| `echo` | yes | 90% | monitor |
| `loggly` | yes | 76% | implement |
| `http-logger` | yes | 76% | implement |
| `splunk-hec-logging` | yes | 62% | monitor |
| `skywalking-logger` | yes | 76% | implement |
| `google-cloud-logging` | yes | 67% | monitor |
| `sls-logger` | yes | 72% | implement |
| `tcp-logger` | yes | 70% | implement |
| `kafka-logger` | yes | 76% | monitor |
| `rocketmq-logger` | yes | 72% | monitor |
| `syslog` | yes | 70% | implement |
| `udp-logger` | yes | 70% | implement |
| `file-logger` | yes | 82% | monitor |
| `clickhouse-logger` | yes | 76% | implement |
| `tencent-cloud-cls` | yes | 76% | implement |
| `inspect` | no | unsupported: lua feature | not-required-native |
| `example-plugin` | yes | 60% | implement |
| `aws-lambda` | yes | 70% | monitor |
| `azure-functions` | yes | 65% | implement |
| `openwhisk` | yes | 75% | monitor |
| `openfunction` | yes | 65% | implement |
| `serverless-post-function` | yes | 45% | not-required-native |
| `ext-plugin-post-req` | no | unsupported: No need | not-required-native |
| `ext-plugin-post-resp` | no | unsupported: No need | not-required-native |
| `ai-request-rewrite` | yes | 50% | implement |

## Next Priority Lanes

1. Finish the remaining `error-log-logger` batch-label/cache gaps if they can be bounded without OpenResty runtime behavior.
2. Raise high-value auth parity: `key-auth`, `jwt-auth`, `hmac-auth`, then `openid-connect`.
3. Raise traffic parity where Go-native support is practical: Redis policies for `limit-*`, and additional `workflow` delegated plugin actions.
4. Keep OpenResty/NGINX/Lua-runtime-native plugins out of normal parity work: `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, `inspect`, `serverless-pre-function`, and `serverless-post-function`.
