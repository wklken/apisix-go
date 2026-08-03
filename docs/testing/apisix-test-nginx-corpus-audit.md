# APISIX Test::Nginx Corpus Audit

> Source of truth for the pinned Apache APISIX `t/plugin/**/*.t` corpus: every
> source file and every TEST label is inventoried exactly once, with an owner,
> disposition, and evidence. This document is the input matrix for
> `t/plugin/corpus_scope.yaml` and the corpus accounting gate.

## Locked Execution State (Task 1 Step 1)

Recorded 2026-08-02 at the start of the corpus coverage program:

| Check | Value |
|---|---|
| Branch | `codex/plugin-integration-tests` |
| HEAD | `82309cc701ed36f3c0598ebecdde9990a2f9129c` |
| Working tree | only user-owned untracked `deepseek-handover.md`, `docs/superpowers/plans/2026-08-02-deepseek-review-remediation.md`, `docs/superpowers/plans/2026-08-02-full-test-nginx-corpus-coverage.md` |
| `origin/master` | `54549447b60d0956a9a69bb39e926f13b4471f7c` |
| merge-base `origin/master` HEAD | `54549447b60d0956a9a69bb39e926f13b4471f7c` (HEAD is ahead) |
| Upstream pin | `.cache/apache-apisix` at `c3d7d5ec69774121f53d2e20d29d09c816795dd7` |
| Upstream `t/plugin` tracked changes | none |

The 2026-08-02 deepseek-review-remediation plan was completed and accepted at
HEAD `82309cc`; its plan and handover files remain untracked and untouched.

## Recount (Task 1 Steps 2-3)

Run from repository root after `source .envrc` (Go test loader is the same as
`t/plugin`):

```bash
find .cache/apache-apisix/t/plugin -type f -name '*.t' | wc -l        # 321
rg --no-filename '^=== TEST ' .cache/apache-apisix/t/plugin --glob '*.t' | wc -l  # 5055
go test ./t/plugin -run TestTempCorpusRecount -count=1 -v             # 251 unique declared files, 3931 declared blocks
```

| Metric | Pinned upstream | Declared/selected locally | Gap |
|---|---:|---:|---:|
| `t/plugin/**/*.t` source files | 321 | 251 | 70 fully unreferenced |
| `=== TEST` blocks | 5,055 | 3,931 selected | 1,124 unselected |
| Fully unreferenced TEST blocks | — | — | 1,086 |
| Blocks omitted from partially selected shared sources | — | — | 38 |
| Duplicate declared source files | — | 0 | — |
| Declared sources missing from pinned checkout | — | 0 | — |

The 38 partially selected blocks are the unselected remainder of
`security-warning.t` (18 blocks) and `security-warning2.t` (20 blocks). These
numbers match the locked review snapshot exactly.

## Scope Buckets (Task 3 Step 1 input)

| Bucket | Source files | TEST blocks |
|---|---:|---:|
| 1. Missing sources for plugins already marked `Supported` (Task 3 Step 2) | 10 | 130 |
| 2. Registered APISIX 3.17 defaults marked `Deferred` (Task 5) | 35 | 448 |
| 3. Registered or unregistered native/runtime plugins (Task 6) | 20 | 443 |
| 4. Non-default, cross-plugin, regression-only, or newer plugin sources (Task 6) | 5 | 65 |
| Buckets 1-4 total (fully unreferenced) | 70 | 1,086 |
| Partially selected shared sources (unselected remainder) | 2 | 38 |

Buckets are derived from `docs/plugins.md` status rows and the
`pkg/plugin/init.go` registry at HEAD `82309cc`.

## Audit Matrix (Task 1 Step 4)

One row per upstream source/test-number ownership partition. Dispositions:
`converted`, `pending`, `blocked_runtime`, `blocked_design`, `non_plugin`.

| Source file | TEST labels | Owner plugin/subsystem | Current manifest | Disposition | Reason/evidence |
|---|---|---|---|---|---|
| `t/plugin/acl.t` | 1-58 | acl | acl.yaml | converted | mapped by the acl standalone manifest |
| `t/plugin/acl2.t` | 1-4 | acl | acl.yaml | converted | mapped by the acl standalone manifest |
| `t/plugin/ai-aws-content-moderation-secrets.t` | 1-5 | ai-aws-content-moderation | ai-aws-content-moderation.yaml | converted | mapped by the ai-aws-content-moderation standalone manifest |
| `t/plugin/ai-aws-content-moderation.t` | 1-16 | ai-aws-content-moderation | ai-aws-content-moderation.yaml | converted | mapped by the ai-aws-content-moderation standalone manifest |
| `t/plugin/ai-aws-content-moderation2.t` | 1-2 | ai-aws-content-moderation | ai-aws-content-moderation.yaml | converted | mapped by the ai-aws-content-moderation standalone manifest |
| `t/plugin/ai-prompt-decorator.t` | 1-17 | ai-prompt-decorator | ai-prompt-decorator.yaml | converted | mapped by the ai-prompt-decorator standalone manifest |
| `t/plugin/ai-prompt-guard.t` | 1-44 | ai-prompt-guard | ai-prompt-guard.yaml | converted | mapped by the ai-prompt-guard standalone manifest |
| `t/plugin/ai-proxy-anthropic.t` | 1-64 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-azure-openai.t` | 1-4 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-bedrock-single.t` | 1-18 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-bedrock.t` | 1-20 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-client-disconnect.t` | 1-4 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-fixture.t` | 1-14 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-flush.t` | 1-7 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-gemini.t` | 1-4 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-openrouter.t` | 1-4 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-passthrough.t` | 1-17 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-protocol-conversion.t` | 1-28 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-request-body-override.t` | 1-19 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-stream-limits.t` | 1-11 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-upstream-vars.t` | 1-7 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy-vertex-ai.t` | 1-12 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy.openai-compatible.t` | 1-5 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy.t` | 1-39 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy2.t` | 1-8 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-proxy3.t` | 1-18 | ai-proxy | ai-proxy.yaml | converted | mapped by the ai-proxy standalone manifest |
| `t/plugin/ai-rag.t` | 1-17 | ai-rag | ai-rag.yaml | converted | mapped by the ai-rag standalone manifest |
| `t/plugin/ai-rate-limiting-consumer-isolation.t` | 1-5 | ai-rate-limiting | ai-rate-limiting.yaml | converted | mapped by the ai-rate-limiting standalone manifest |
| `t/plugin/ai-rate-limiting-expression.t` | 1-13 | ai-rate-limiting | ai-rate-limiting.yaml | converted | mapped by the ai-rate-limiting standalone manifest |
| `t/plugin/ai-rate-limiting.t` | 1-40 | ai-rate-limiting | ai-rate-limiting.yaml | converted | mapped by the ai-rate-limiting standalone manifest |
| `t/plugin/ai-request-rewrite.t` | 1-15 | ai-request-rewrite | ai-request-rewrite.yaml | converted | mapped by the ai-request-rewrite standalone manifest |
| `t/plugin/ai-request-rewrite2.t` | 1-4 | ai-request-rewrite | ai-request-rewrite.yaml | converted | mapped by the ai-request-rewrite standalone manifest |
| `t/plugin/api-breaker.t` | 1-20 | api-breaker | api-breaker.yaml | converted | mapped by the api-breaker standalone manifest |
| `t/plugin/attach-consumer-label.t` | 1-17 | attach-consumer-label | attach-consumer-label.yaml | converted | mapped by the attach-consumer-label standalone manifest |
| `t/plugin/authz-casbin.t` | 1-21 | authz-casbin | authz-casbin.yaml | converted | mapped by the authz-casbin standalone manifest |
| `t/plugin/authz-casdoor.t` | 1-14 | authz-casdoor | authz-casdoor.yaml | converted | mapped by the authz-casdoor standalone manifest |
| `t/plugin/authz-keycloak.t` | 1-18 | authz-keycloak | authz-keycloak.yaml | converted | mapped by the authz-keycloak standalone manifest |
| `t/plugin/authz-keycloak2.t` | 1-19 | authz-keycloak | authz-keycloak.yaml | converted | mapped by the authz-keycloak standalone manifest |
| `t/plugin/authz-keycloak3.t` | 1-3 | authz-keycloak | authz-keycloak.yaml | converted | mapped by the authz-keycloak standalone manifest |
| `t/plugin/authz-keycloak4.t` | 1-4 | authz-keycloak | authz-keycloak.yaml | converted | mapped by the authz-keycloak standalone manifest |
| `t/plugin/authz-keycloak5.t` | 1-1 | authz-keycloak | authz-keycloak.yaml | converted | mapped by the authz-keycloak standalone manifest |
| `t/plugin/aws-lambda.t` | 1-9 | aws-lambda | aws-lambda.yaml | converted | mapped by the aws-lambda standalone manifest |
| `t/plugin/azure-functions.t` | 1-14 | azure-functions | azure-functions.yaml | converted | mapped by the azure-functions standalone manifest |
| `t/plugin/basic-auth-anonymous-consumer.t` | 1-8 | basic-auth | basic-auth.yaml | converted | mapped by the basic-auth standalone manifest |
| `t/plugin/basic-auth-realm.t` | 1-6 | basic-auth | basic-auth.yaml | converted | mapped by the basic-auth standalone manifest |
| `t/plugin/basic-auth.t` | 1-30 | basic-auth | basic-auth.yaml | converted | mapped by the basic-auth standalone manifest |
| `t/plugin/batch-requests-grpc.t` | 1-5 | batch-requests | batch-requests.yaml | converted | mapped by the batch-requests standalone manifest |
| `t/plugin/batch-requests.t` | 1-30 | batch-requests | batch-requests.yaml | converted | mapped by the batch-requests standalone manifest |
| `t/plugin/batch-requests2.t` | 1-11 | batch-requests | batch-requests.yaml | converted | mapped by the batch-requests standalone manifest |
| `t/plugin/body-transformer.t` | 1-20 | body-transformer | body-transformer.yaml | converted | mapped by the body-transformer standalone manifest |
| `t/plugin/brotli.t` | 1-37 | brotli | brotli.yaml | converted | mapped by the brotli standalone manifest |
| `t/plugin/cas-auth.t` | 1-22 | cas-auth | cas-auth.yaml | converted | mapped by the cas-auth standalone manifest |
| `t/plugin/chaitin-waf.t` | 1-15 | chaitin-waf | chaitin-waf.yaml | converted | mapped by the chaitin-waf standalone manifest |
| `t/plugin/clickhouse-logger.t` | 1-12 | clickhouse-logger | clickhouse-logger.yaml | converted | mapped by the clickhouse-logger standalone manifest |
| `t/plugin/clickhouse-logger2.t` | 1-8 | clickhouse-logger | clickhouse-logger.yaml | converted | mapped by the clickhouse-logger standalone manifest |
| `t/plugin/clickhouse-logger3.t` | 1-3 | clickhouse-logger | clickhouse-logger.yaml | converted | mapped by the clickhouse-logger standalone manifest |
| `t/plugin/client-control.t` | 1-12 | client-control | client-control.yaml | converted | mapped by the client-control standalone manifest |
| `t/plugin/consumer-restriction.t` | 1-53 | consumer-restriction | consumer-restriction.yaml | converted | mapped by the consumer-restriction standalone manifest |
| `t/plugin/consumer-restriction2.t` | 1-18 | consumer-restriction | consumer-restriction.yaml | converted | mapped by the consumer-restriction standalone manifest |
| `t/plugin/cors.t` | 1-37 | cors | cors.yaml | converted | mapped by the cors standalone manifest |
| `t/plugin/cors2.t` | 1-5 | cors | cors.yaml | converted | mapped by the cors standalone manifest |
| `t/plugin/cors3.t` | 1-15 | cors | cors.yaml | converted | mapped by the cors standalone manifest |
| `t/plugin/cors4.t` | 1-29 | cors | cors.yaml | converted | mapped by the cors standalone manifest |
| `t/plugin/csrf.t` | 1-15 | csrf | csrf.yaml | converted | mapped by the csrf standalone manifest |
| `t/plugin/data-mask.t` | 1-20 | data-mask | data-mask.yaml | converted | mapped by the data-mask standalone manifest |
| `t/plugin/datadog.t` | 1-13 | datadog | datadog.yaml | converted | mapped by the datadog standalone manifest |
| `t/plugin/degraphql.t` | 1-15 | degraphql | degraphql.yaml | converted | mapped by the degraphql standalone manifest |
| `t/plugin/dingtalk-auth.t` | 1-14 | dingtalk-auth | dingtalk-auth.yaml | converted | mapped by the dingtalk-auth standalone manifest |
| `t/plugin/echo.t` | 1-11 | echo | echo.yaml | converted | mapped by the echo standalone manifest |
| `t/plugin/elasticsearch-logger.t` | 1-27 | elasticsearch-logger | elasticsearch-logger.yaml | converted | mapped by the elasticsearch-logger standalone manifest |
| `t/plugin/error-log-logger-clickhouse.t` | 1-9 | error-log-logger | error-log-logger.yaml | converted | mapped by the error-log-logger standalone manifest |
| `t/plugin/error-log-logger-kafka.t` | 1-7 | error-log-logger | error-log-logger.yaml | converted | mapped by the error-log-logger standalone manifest |
| `t/plugin/error-log-logger-skywalking.t` | 1-8 | error-log-logger | error-log-logger.yaml | converted | mapped by the error-log-logger standalone manifest |
| `t/plugin/error-log-logger.t` | 1-15 | error-log-logger | error-log-logger.yaml | converted | mapped by the error-log-logger standalone manifest |
| `t/plugin/error-page.t` | 1-20 | error-page | error-page.yaml | converted | mapped by the error-page standalone manifest |
| `t/plugin/example.t` | 1-13 | example-plugin | example-plugin.yaml | converted | mapped by the example-plugin standalone manifest |
| `t/plugin/exit-transformer.t` | 1-20 | exit-transformer | exit-transformer.yaml | converted | mapped by the exit-transformer standalone manifest |
| `t/plugin/fault-injection.t` | 1-39 | fault-injection | fault-injection.yaml | converted | mapped by the fault-injection standalone manifest |
| `t/plugin/fault-injection2.t` | 1-7 | fault-injection | fault-injection.yaml | converted | mapped by the fault-injection standalone manifest |
| `t/plugin/feishu-auth.t` | 1-14 | feishu-auth | feishu-auth.yaml | converted | mapped by the feishu-auth standalone manifest |
| `t/plugin/file-logger-reopen.t` | 1-3 | file-logger | file-logger.yaml | converted | mapped by the file-logger standalone manifest |
| `t/plugin/file-logger.t` | 1-26 | file-logger | file-logger.yaml | converted | mapped by the file-logger standalone manifest |
| `t/plugin/file-logger2.t` | 1-15 | file-logger | file-logger.yaml | converted | mapped by the file-logger standalone manifest |
| `t/plugin/forward-auth.t` | 1-21 | forward-auth | forward-auth.yaml | converted | mapped by the forward-auth standalone manifest |
| `t/plugin/forward-auth2.t` | 1-5 | forward-auth | forward-auth.yaml | converted | mapped by the forward-auth standalone manifest |
| `t/plugin/forward-auth3.t` | 1-2 | forward-auth | forward-auth.yaml | converted | mapped by the forward-auth standalone manifest |
| `t/plugin/google-cloud-logging.t` | 1-25 | google-cloud-logging | google-cloud-logging.yaml | converted | mapped by the google-cloud-logging standalone manifest |
| `t/plugin/google-cloud-logging2.t` | 1-7 | google-cloud-logging | google-cloud-logging.yaml | converted | mapped by the google-cloud-logging standalone manifest |
| `t/plugin/google-cloud-logging3.t` | 1-1 | google-cloud-logging | google-cloud-logging.yaml | converted | mapped by the google-cloud-logging standalone manifest |
| `t/plugin/graphql-limit-count.t` | 1-26 | graphql-limit-count | graphql-limit-count.yaml | converted | mapped by the graphql-limit-count standalone manifest |
| `t/plugin/graphql-proxy-cache/disk.t` | 1-11 | graphql-proxy-cache | graphql-proxy-cache.yaml | converted | mapped by the graphql-proxy-cache standalone manifest |
| `t/plugin/graphql-proxy-cache/graphql.t` | 1-21 | graphql-proxy-cache | graphql-proxy-cache.yaml | converted | mapped by the graphql-proxy-cache standalone manifest |
| `t/plugin/graphql-proxy-cache/memory.t` | 1-16 | graphql-proxy-cache | graphql-proxy-cache.yaml | converted | mapped by the graphql-proxy-cache standalone manifest |
| `t/plugin/gzip.t` | 1-19 | gzip | gzip.yaml | converted | mapped by the gzip standalone manifest |
| `t/plugin/hmac-auth-anonymous-consumer.t` | 1-6 | hmac-auth | hmac-auth.yaml | converted | mapped by the hmac-auth standalone manifest |
| `t/plugin/hmac-auth-realm.t` | 1-6 | hmac-auth | hmac-auth.yaml | converted | mapped by the hmac-auth standalone manifest |
| `t/plugin/hmac-auth.t` | 1-40 | hmac-auth | hmac-auth.yaml | converted | mapped by the hmac-auth standalone manifest |
| `t/plugin/hmac-auth2.t` | 1-5 | hmac-auth | hmac-auth.yaml | converted | mapped by the hmac-auth standalone manifest |
| `t/plugin/hmac-auth3.t` | 1-8 | hmac-auth | hmac-auth.yaml | converted | mapped by the hmac-auth standalone manifest |
| `t/plugin/hmac-auth4.t` | 1-5 | hmac-auth | hmac-auth.yaml | converted | mapped by the hmac-auth standalone manifest |
| `t/plugin/http-dubbo.t` | 1-5 | http-dubbo | http-dubbo.yaml | converted | mapped by the http-dubbo standalone manifest |
| `t/plugin/http-logger-json.t` | 1-8 | http-logger | http-logger.yaml | converted | mapped by the http-logger standalone manifest |
| `t/plugin/http-logger-large-body.t` | 1-29 | http-logger | http-logger.yaml | converted | mapped by the http-logger standalone manifest |
| `t/plugin/http-logger-log-format.t` | 1-21 | http-logger | http-logger.yaml | converted | mapped by the http-logger standalone manifest |
| `t/plugin/http-logger-new-line.t` | 1-10 | http-logger | http-logger.yaml | converted | mapped by the http-logger standalone manifest |
| `t/plugin/http-logger.t` | 1-29 | http-logger | http-logger.yaml | converted | mapped by the http-logger standalone manifest |
| `t/plugin/http-logger2.t` | 1-15 | http-logger | http-logger.yaml | converted | mapped by the http-logger standalone manifest |
| `t/plugin/http-logger3.t` | 1-2 | http-logger | http-logger.yaml | converted | mapped by the http-logger standalone manifest |
| `t/plugin/ip-restriction.t` | 1-35 | ip-restriction | ip-restriction.yaml | converted | mapped by the ip-restriction standalone manifest |
| `t/plugin/jwe-decrypt.t` | 1-23 | jwe-decrypt | jwe-decrypt.yaml | converted | mapped by the jwe-decrypt standalone manifest |
| `t/plugin/jwt-auth-anonymous-consumer.t` | 1-7 | jwt-auth | jwt-auth.yaml | converted | mapped by the jwt-auth standalone manifest |
| `t/plugin/jwt-auth-more-algo.t` | 1-17 | jwt-auth | jwt-auth.yaml | converted | mapped by the jwt-auth standalone manifest |
| `t/plugin/jwt-auth-realm.t` | 1-6 | jwt-auth | jwt-auth.yaml | converted | mapped by the jwt-auth standalone manifest |
| `t/plugin/jwt-auth.t` | 1-59 | jwt-auth | jwt-auth.yaml | converted | mapped by the jwt-auth standalone manifest |
| `t/plugin/jwt-auth2.t` | 1-9 | jwt-auth | jwt-auth.yaml | converted | mapped by the jwt-auth standalone manifest |
| `t/plugin/jwt-auth3.t` | 1-21 | jwt-auth | jwt-auth.yaml | converted | mapped by the jwt-auth standalone manifest |
| `t/plugin/jwt-auth4.t` | 1-11 | jwt-auth | jwt-auth.yaml | converted | mapped by the jwt-auth standalone manifest |
| `t/plugin/kafka-logger-large-body.t` | 1-28 | kafka-logger | kafka-logger.yaml | converted | mapped by the kafka-logger standalone manifest |
| `t/plugin/kafka-logger-log-format.t` | 1-5 | kafka-logger | kafka-logger.yaml | converted | mapped by the kafka-logger standalone manifest |
| `t/plugin/kafka-logger.t` | 1-29 | kafka-logger | kafka-logger.yaml | converted | mapped by the kafka-logger standalone manifest |
| `t/plugin/kafka-logger2.t` | 1-27 | kafka-logger | kafka-logger.yaml | converted | mapped by the kafka-logger standalone manifest |
| `t/plugin/kafka-logger3.t` | 1-2 | kafka-logger | kafka-logger.yaml | converted | mapped by the kafka-logger standalone manifest |
| `t/plugin/kafka-logger4.t` | 1-8 | kafka-logger | kafka-logger.yaml | converted | mapped by the kafka-logger standalone manifest |
| `t/plugin/kafka-proxy.t` | 1-2 | kafka-proxy | kafka-proxy.yaml | converted | mapped by the kafka-proxy standalone manifest |
| `t/plugin/key-auth-anonymous-consumer.t` | 1-11 | key-auth | key-auth.yaml | converted | mapped by the key-auth standalone manifest |
| `t/plugin/key-auth-realm.t` | 1-6 | key-auth | key-auth.yaml | converted | mapped by the key-auth standalone manifest |
| `t/plugin/key-auth-upstream-domain-node.t` | 1-7 | key-auth | key-auth.yaml | converted | mapped by the key-auth standalone manifest |
| `t/plugin/key-auth.t` | 1-34 | key-auth | key-auth.yaml | converted | mapped by the key-auth standalone manifest |
| `t/plugin/lago.t` | 1-2 | lago | lago.yaml | converted | mapped by the lago standalone manifest |
| `t/plugin/ldap-auth-realm.t` | 1-7 | ldap-auth | ldap-auth.yaml | converted | mapped by the ldap-auth standalone manifest |
| `t/plugin/ldap-auth.t` | 1-28 | ldap-auth | ldap-auth.yaml | converted | mapped by the ldap-auth standalone manifest |
| `t/plugin/limit-conn-redis-cluster.t` | 1-10 | limit-conn | limit-conn.yaml | converted | mapped by the limit-conn standalone manifest |
| `t/plugin/limit-conn-redis.t` | 1-26 | limit-conn | limit-conn.yaml | converted | mapped by the limit-conn standalone manifest |
| `t/plugin/limit-conn-variable.t` | 1-18 | limit-conn | limit-conn.yaml | converted | mapped by the limit-conn standalone manifest |
| `t/plugin/limit-conn.t` | 1-34 | limit-conn | limit-conn.yaml | converted | mapped by the limit-conn standalone manifest |
| `t/plugin/limit-conn2.t` | 1-13 | limit-conn | limit-conn.yaml | converted | mapped by the limit-conn standalone manifest |
| `t/plugin/limit-conn3.t` | 1-3 | limit-conn | limit-conn.yaml | converted | mapped by the limit-conn standalone manifest |
| `t/plugin/limit-count-consumer-group-credentials.t` | 1-8 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-consumer-isolation.t` | 1-5 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis-cluster.t` | 1-18 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis-cluster2.t` | 1-2 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis-cluster3.t` | 1-6 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis-delayed-sync.t` | 1-5 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis-delayed-sync2.t` | 1-9 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis-sentinel.t` | 1-15 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis.t` | 1-22 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis2.t` | 1-10 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis3.t` | 1-11 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis4.t` | 1-5 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-redis5.t` | 1-3 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-rules.t` | 1-22 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-sliding.t` | 1-8 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count-variable.t` | 1-13 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count.t` | 1-42 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count2.t` | 1-22 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count3.t` | 1-13 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count4.t` | 1-5 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-count5.t` | 1-8 | limit-count | limit-count.yaml | converted | mapped by the limit-count standalone manifest |
| `t/plugin/limit-req-redis-cluster.t` | 1-22 | limit-req | limit-req.yaml | converted | mapped by the limit-req standalone manifest |
| `t/plugin/limit-req-redis.t` | 1-30 | limit-req | limit-req.yaml | converted | mapped by the limit-req standalone manifest |
| `t/plugin/limit-req-shared-counter.t` | 1-3 | limit-req | limit-req.yaml | converted | mapped by the limit-req standalone manifest |
| `t/plugin/limit-req.t` | 1-21 | limit-req | limit-req.yaml | converted | mapped by the limit-req standalone manifest |
| `t/plugin/limit-req2.t` | 1-10 | limit-req | limit-req.yaml | converted | mapped by the limit-req standalone manifest |
| `t/plugin/limit-req3.t` | 1-3 | limit-req | limit-req.yaml | converted | mapped by the limit-req standalone manifest |
| `t/plugin/log-rotate.t` | 1-6 | log-rotate | log-rotate.yaml | converted | mapped by the log-rotate standalone manifest |
| `t/plugin/log-rotate2.t` | 1-5 | log-rotate | log-rotate.yaml | converted | mapped by the log-rotate standalone manifest |
| `t/plugin/log-rotate3.t` | 1-6 | log-rotate | log-rotate.yaml | converted | mapped by the log-rotate standalone manifest |
| `t/plugin/loggly.t` | 1-22 | loggly | loggly.yaml | converted | mapped by the loggly standalone manifest |
| `t/plugin/loki-logger.t` | 1-21 | loki-logger | loki-logger.yaml | converted | mapped by the loki-logger standalone manifest |
| `t/plugin/loki-logger2.t` | 1-1 | loki-logger | loki-logger.yaml | converted | mapped by the loki-logger standalone manifest |
| `t/plugin/mocking.t` | 1-22 | mocking | mocking.yaml | converted | mapped by the mocking standalone manifest |
| `t/plugin/multi-auth.t` | 1-22 | multi-auth | multi-auth.yaml | converted | mapped by the multi-auth standalone manifest |
| `t/plugin/multi-auth2.t` | 1-16 | multi-auth | multi-auth.yaml | converted | mapped by the multi-auth standalone manifest |
| `t/plugin/node-status.t` | 1-5 | node-status | node-status.yaml | converted | mapped by the node-status standalone manifest |
| `t/plugin/oas-validator.t` | 1-32 | oas-validator | oas-validator.yaml | converted | mapped by the oas-validator standalone manifest |
| `t/plugin/oas-validator2.t` | 1-55 | oas-validator | oas-validator.yaml | converted | mapped by the oas-validator standalone manifest |
| `t/plugin/oas-validator3.t` | 1-23 | oas-validator | oas-validator.yaml | converted | mapped by the oas-validator standalone manifest |
| `t/plugin/opa.t` | 1-13 | opa | opa.yaml | converted | mapped by the opa standalone manifest |
| `t/plugin/opa2.t` | 1-12 | opa | opa.yaml | converted | mapped by the opa standalone manifest |
| `t/plugin/opa3.t` | 1-2 | opa | opa.yaml | converted | mapped by the opa standalone manifest |
| `t/plugin/openfunction.t` | 1-15 | openfunction | openfunction.yaml | converted | mapped by the openfunction standalone manifest |
| `t/plugin/openid-connect-identity-headers.t` | 1-4 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect-redis.t` | 1-4 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect.t` | 1-54 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect10.t` | 1-12 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect2.t` | 1-21 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect3.t` | 1-6 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect4.t` | 1-6 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect5.t` | 1-2 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect6.t` | 1-8 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect7.t` | 1-10 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect8.t` | 1-8 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/openid-connect9.t` | 1-6 | openid-connect | openid-connect.yaml | converted | mapped by the openid-connect standalone manifest |
| `t/plugin/opentelemetry.t` | 1-48 | opentelemetry | opentelemetry.yaml | converted | mapped by the opentelemetry standalone manifest |
| `t/plugin/opentelemetry2.t` | 1-4 | opentelemetry | opentelemetry.yaml | converted | mapped by the opentelemetry standalone manifest |
| `t/plugin/opentelemetry3.t` | 1-4 | opentelemetry | opentelemetry.yaml | converted | mapped by the opentelemetry standalone manifest |
| `t/plugin/opentelemetry4-bugfix-pb-state.t` | 1-3 | opentelemetry | opentelemetry.yaml | converted | mapped by the opentelemetry standalone manifest |
| `t/plugin/opentelemetry5.t` | 1-13 | opentelemetry | opentelemetry.yaml | converted | mapped by the opentelemetry standalone manifest |
| `t/plugin/opentelemetry6.t` | 1-9 | opentelemetry | opentelemetry.yaml | converted | mapped by the opentelemetry standalone manifest |
| `t/plugin/openwhisk.t` | 1-21 | openwhisk | openwhisk.yaml | converted | mapped by the openwhisk standalone manifest |
| `t/plugin/proxy-cache/disk.t` | 1-29 | proxy-cache | proxy-cache.yaml | converted | mapped by the proxy-cache standalone manifest |
| `t/plugin/proxy-cache/memory.t` | 1-47 | proxy-cache | proxy-cache.yaml | converted | mapped by the proxy-cache standalone manifest |
| `t/plugin/proxy-control.t` | 1-4 | proxy-control | proxy-control.yaml | converted | mapped by the proxy-control standalone manifest |
| `t/plugin/proxy-mirror.t` | 1-31 | proxy-mirror | proxy-mirror.yaml | converted | mapped by the proxy-mirror standalone manifest |
| `t/plugin/proxy-mirror2.t` | 1-3 | proxy-mirror | proxy-mirror.yaml | converted | mapped by the proxy-mirror standalone manifest |
| `t/plugin/proxy-mirror3.t` | 1-2 | proxy-mirror | proxy-mirror.yaml | converted | mapped by the proxy-mirror standalone manifest |
| `t/plugin/proxy-rewrite.t` | 1-57 | proxy-rewrite | proxy-rewrite.yaml | converted | mapped by the proxy-rewrite standalone manifest |
| `t/plugin/public-api.t` | 1-8 | public-api | public-api.yaml | converted | mapped by the public-api standalone manifest |
| `t/plugin/real-ip.t` | 1-24 | real-ip | real-ip.yaml | converted | mapped by the real-ip standalone manifest |
| `t/plugin/redirect.t` | 1-48 | redirect | redirect.yaml | converted | mapped by the redirect standalone manifest |
| `t/plugin/redirect2.t` | 1-3 | redirect | redirect.yaml | converted | mapped by the redirect standalone manifest |
| `t/plugin/referer-restriction.t` | 1-13 | referer-restriction | referer-restriction.yaml | converted | mapped by the referer-restriction standalone manifest |
| `t/plugin/request-id.t` | 1-28 | request-id | request-id.yaml | converted | mapped by the request-id standalone manifest |
| `t/plugin/request-id2.t` | 1-4 | request-id | request-id.yaml | converted | mapped by the request-id standalone manifest |
| `t/plugin/request-id3.t` | 1-5 | request-id | request-id.yaml | converted | mapped by the request-id standalone manifest |
| `t/plugin/request-validation.t` | 1-53 | request-validation | request-validation.yaml | converted | mapped by the request-validation standalone manifest |
| `t/plugin/request-validation2.t` | 1-2 | request-validation | request-validation.yaml | converted | mapped by the request-validation standalone manifest |
| `t/plugin/response-rewrite.t` | 1-27 | response-rewrite | response-rewrite.yaml | converted | mapped by the response-rewrite standalone manifest |
| `t/plugin/rocketmq-logger-log-format.t` | 1-5 | rocketmq-logger | rocketmq-logger.yaml | converted | mapped by the rocketmq-logger standalone manifest |
| `t/plugin/rocketmq-logger.t` | 1-19 | rocketmq-logger | rocketmq-logger.yaml | converted | mapped by the rocketmq-logger standalone manifest |
| `t/plugin/rocketmq-logger2.t` | 1-18 | rocketmq-logger | rocketmq-logger.yaml | converted | mapped by the rocketmq-logger standalone manifest |
| `t/plugin/saml-auth-post.t` | 1-6 | saml-auth | saml-auth.yaml | converted | mapped by the saml-auth standalone manifest |
| `t/plugin/saml-auth.t` | 1-15 | saml-auth | saml-auth.yaml | converted | mapped by the saml-auth standalone manifest |
| `t/plugin/server-info.t` | 1-2 | server-info | server-info.yaml | converted | mapped by the server-info standalone manifest |
| `t/plugin/skywalking-logger.t` | 1-14 | skywalking-logger | skywalking-logger.yaml | converted | mapped by the skywalking-logger standalone manifest |
| `t/plugin/skywalking-logger2.t` | 1-1 | skywalking-logger | skywalking-logger.yaml | converted | mapped by the skywalking-logger standalone manifest |
| `t/plugin/skywalking.t` | 1-15 | skywalking | skywalking.yaml | converted | mapped by the skywalking standalone manifest |
| `t/plugin/skywalking2.t` | 1-2 | skywalking | skywalking.yaml | converted | mapped by the skywalking standalone manifest |
| `t/plugin/sls-logger.t` | 1-17 | sls-logger | sls-logger.yaml | converted | mapped by the sls-logger standalone manifest |
| `t/plugin/splunk-hec-logging.t` | 1-16 | splunk-hec-logging | splunk-hec-logging.yaml | converted | mapped by the splunk-hec-logging standalone manifest |
| `t/plugin/splunk-hec-logging2.t` | 1-1 | splunk-hec-logging | splunk-hec-logging.yaml | converted | mapped by the splunk-hec-logging standalone manifest |
| `t/plugin/syslog.t` | 1-21 | syslog | syslog.yaml | converted | mapped by the syslog standalone manifest |
| `t/plugin/tcp-logger.t` | 1-17 | tcp-logger | tcp-logger.yaml | converted | mapped by the tcp-logger standalone manifest |
| `t/plugin/tencent-cloud-cls.t` | 1-22 | tencent-cloud-cls | tencent-cloud-cls.yaml | converted | mapped by the tencent-cloud-cls standalone manifest |
| `t/plugin/traffic-label.t` | 1-20 | traffic-label | traffic-label.yaml | converted | mapped by the traffic-label standalone manifest |
| `t/plugin/traffic-label2.t` | 1-18 | traffic-label | traffic-label.yaml | converted | mapped by the traffic-label standalone manifest |
| `t/plugin/traffic-split.t` | 1-21 | traffic-split | traffic-split.yaml | converted | mapped by the traffic-split standalone manifest |
| `t/plugin/traffic-split2.t` | 1-22 | traffic-split | traffic-split.yaml | converted | mapped by the traffic-split standalone manifest |
| `t/plugin/traffic-split3.t` | 1-20 | traffic-split | traffic-split.yaml | converted | mapped by the traffic-split standalone manifest |
| `t/plugin/traffic-split4.t` | 1-19 | traffic-split | traffic-split.yaml | converted | mapped by the traffic-split standalone manifest |
| `t/plugin/traffic-split5.t` | 1-12 | traffic-split | traffic-split.yaml | converted | mapped by the traffic-split standalone manifest |
| `t/plugin/ua-restriction.t` | 1-33 | ua-restriction | ua-restriction.yaml | converted | mapped by the ua-restriction standalone manifest |
| `t/plugin/udp-logger.t` | 1-14 | udp-logger | udp-logger.yaml | converted | mapped by the udp-logger standalone manifest |
| `t/plugin/uri-blocker.t` | 1-22 | uri-blocker | uri-blocker.yaml | converted | mapped by the uri-blocker standalone manifest |
| `t/plugin/wolf-rbac.t` | 1-42 | wolf-rbac | wolf-rbac.yaml | converted | mapped by the wolf-rbac standalone manifest |
| `t/plugin/workflow-without-case.t` | 1-7 | workflow | workflow.yaml | converted | mapped by the workflow standalone manifest |
| `t/plugin/workflow.t` | 1-20 | workflow | workflow.yaml | converted | mapped by the workflow standalone manifest |
| `t/plugin/workflow2.t` | 1-8 | workflow | workflow.yaml | converted | mapped by the workflow standalone manifest |
| `t/plugin/workflow3.t` | 1-7 | workflow | workflow.yaml | converted | mapped by the workflow standalone manifest |
| `t/plugin/ai-aliyun-content-moderation.t` | 1-72 | ai-aliyun-content-moderation | - | blocked_runtime | registered Deferred default; Task 5 native/runtime decision required |
| `t/plugin/ai-cache-semantic.t` | 1-66 | ai-cache | - | blocked_design | unregistered AI cache subsystem; Task 6 scope decision required |
| `t/plugin/ai-cache-streaming.t` | 1-42 | ai-cache | - | blocked_design | unregistered AI cache subsystem; Task 6 scope decision required |
| `t/plugin/ai-cache.t` | 1-57 | ai-cache | - | blocked_design | unregistered AI cache subsystem; Task 6 scope decision required |
| `t/plugin/ai-lakera-guard-chain.t` | 1-4 | ai-lakera-guard | - | blocked_design | unregistered AI guard subsystem; Task 6 scope decision required |
| `t/plugin/ai-lakera-guard-secrets.t` | 1-5 | ai-lakera-guard | - | blocked_design | unregistered AI guard subsystem; Task 6 scope decision required |
| `t/plugin/ai-lakera-guard.t` | 1-57 | ai-lakera-guard | - | blocked_design | unregistered AI guard subsystem; Task 6 scope decision required |
| `t/plugin/ai-prompt-template.t` | 1-9 | ai-prompt-template | - | blocked_runtime | registered Deferred default; Task 5 native/runtime decision required |
| `t/plugin/ai-proxy-kafka-log.t` | 1-6 | ai-proxy | ai-proxy.yaml | converted | kafka summaries/payloads logging integration GREEN |
| `t/plugin/ai-proxy-kafka-log.t` | 7-8 | ai-proxy-multi | - | blocked_design | uses ai-proxy-multi (Deferred separate subsystem) |
| `t/plugin/ai-proxy-kafka-log.t` | 9 | ai-proxy | - | blocked_runtime | Lua unit test of base.set_logging |
| `t/plugin/ai-proxy-multi-chash-healthcheck-stable-ring.t` | 1-1 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi-construct-upstream-panic.t` | 1-5 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi-construct-upstream.t` | 1-2 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi-domain-healthcheck-repro.t` | 1-1 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi-domain-healthcheck.t` | 1-4 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi-healthcheck-stale-picker.t` | 1-1 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi-retry.t` | 1-6 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi.balancer.t` | 1-23 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi.openai-compatible.t` | 1-4 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi.t` | 1-15 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi2.t` | 1-8 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-proxy-multi3.t` | 1-14 | ai-proxy-multi | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/ai-transport-http.t` | 1-7 | ai-transport-http | - | blocked_design | AI transport subsystem; Task 6 scope decision required |
| `t/plugin/ai.t` | 1-14 | ai | - | blocked_runtime | registered Deferred default; Task 5 native/runtime decision required |
| `t/plugin/ai2.t` | 1-6 | ai | - | blocked_runtime | registered Deferred default; Task 5 native/runtime decision required |
| `t/plugin/ai3.t` | 1-2 | ai | - | blocked_runtime | registered Deferred default; Task 5 native/runtime decision required |
| `t/plugin/ai4.t` | 1-12 | ai | - | blocked_runtime | registered Deferred default; Task 5 native/runtime decision required |
| `t/plugin/ai5.t` | 1-2 | ai | - | blocked_runtime | registered Deferred default; Task 5 native/runtime decision required |
| `t/plugin/body-transformer-multipart.t` | 1-2 | body-transformer | body-transformer.yaml | converted | multipart request/response conversion integration GREEN |
| `t/plugin/body-transformer-multipart.t` | 3-4 | body-transformer | - | blocked_design | requires context._multipart Lua object helpers; deferred per docs/plugins.md |
| `t/plugin/body-transformer2.t` | 3 | body-transformer | body-transformer.yaml | converted | key-auth rejection integration GREEN |
| `t/plugin/body-transformer2.t` | 1-2 | body-transformer | - | blocked_design | template executes arbitrary Lua; deferred per docs/plugins.md |
| `t/plugin/chaitin-waf-reject.t` | 1-4 | chaitin-waf | chaitin-waf.yaml | converted | block-mode reject and monitor-mode pass-through integration GREEN |
| `t/plugin/chaitin-waf-timeout.t` | 1-2 | chaitin-waf | chaitin-waf.yaml | converted | read-timeout fallback integration GREEN |
| `t/plugin/consumer-bug-fix.t` | 1-5 | consumer framework | - | non_plugin | consumer framework regression tests, not a single plugin behavior |
| `t/plugin/custom_sort_plugins.t` | 1-17 | plugin framework | - | non_plugin | plugin sorting framework ordering tests, not a plugin behavior |
| `t/plugin/dubbo-proxy/route.t` | 1-11 | dubbo-proxy | - | blocked_design | registered non-default protocol subsystem; Task 6 separate subsystem decision required |
| `t/plugin/dubbo-proxy/upstream.t` | 1-4 | dubbo-proxy | - | blocked_design | registered non-default protocol subsystem; Task 6 separate subsystem decision required |
| `t/plugin/elasticsearch-logger2.t` | 1-3, 5-9 | elasticsearch-logger | elasticsearch-logger.yaml | converted | pending-drop, header auth, index-variable integration GREEN |
| `t/plugin/elasticsearch-logger2.t` | 4, 10 | elasticsearch-logger | - | blocked_runtime | Lua unit tests of internal _resolve_index_vars |
| `t/plugin/ext-plugin/conf_token.t` | 1-2 | ext-plugin-pre/post-req/resp | - | blocked_runtime | external plugin runner protocol; unregistered defaults |
| `t/plugin/ext-plugin/extra-info.t` | 1-11 | ext-plugin-pre/post-req/resp | - | blocked_runtime | external plugin runner protocol; unregistered defaults |
| `t/plugin/ext-plugin/http-req-call.t` | 1-31 | ext-plugin-pre/post-req/resp | - | blocked_runtime | external plugin runner protocol; unregistered defaults |
| `t/plugin/ext-plugin/request-body.t` | 1-5 | ext-plugin-pre/post-req/resp | - | blocked_runtime | external plugin runner protocol; unregistered defaults |
| `t/plugin/ext-plugin/response.t` | 1-16 | ext-plugin-pre/post-req/resp | - | blocked_runtime | external plugin runner protocol; unregistered defaults |
| `t/plugin/ext-plugin/sanity.t` | 1-22 | ext-plugin-pre/post-req/resp | - | blocked_runtime | external plugin runner protocol; unregistered defaults |
| `t/plugin/ext-plugin/sanity2.t` | 1-1 | ext-plugin-pre/post-req/resp | - | blocked_runtime | external plugin runner protocol; unregistered defaults |
| `t/plugin/grpc-transcode-reload-bugfix.t` | 1-1 | grpc-transcode | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/grpc-transcode.t` | 1-31 | grpc-transcode | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/grpc-transcode2.t` | 1-18 | grpc-transcode | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/grpc-transcode3.t` | 1-14 | grpc-transcode | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/grpc-transcode4.t` | 1-3 | grpc-transcode | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/grpc-web.t` | 1-18 | grpc-web | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/inspect.t` | 1-14 | inspect | - | blocked_runtime | Lua/OpenResty inspection runtime; unregistered default |
| `t/plugin/mcp-bridge.t` | 1-1 | mcp-bridge | - | blocked_runtime | registered Deferred default; Task 5 native/runtime decision required |
| `t/plugin/ocsp-stapling.t` | 1-33 | ocsp-stapling | - | blocked_runtime | NGINX-native TLS/OCSP internals |
| `t/plugin/plugin.t` | 1-28 | plugin framework | - | non_plugin | plugin framework registration/lifecycle tests, not a plugin behavior |
| `t/plugin/prometheus-ai-cache.t` | 1-15 | prometheus+ai-cache | - | blocked_design | prometheus AI cache metrics depend on unregistered ai-cache subsystem |
| `t/plugin/prometheus-ai-proxy.t` | 1-26 | prometheus+ai-proxy | - | blocked_design | prometheus AI proxy metrics depend on deferred ai-proxy-multi subsystem |
| `t/plugin/prometheus-ai-proxy2.t` | 1-3 | prometheus+ai-proxy | - | blocked_design | prometheus AI proxy metrics depend on deferred ai-proxy-multi subsystem |
| `t/plugin/prometheus-label-filter.t` | 1-7 | prometheus | - | blocked_design | prometheus exporter internals; Task 5 separate subsystem decision required |
| `t/plugin/prometheus-metric-expire.t` | 1-1 | prometheus | - | blocked_design | prometheus exporter internals; Task 5 separate subsystem decision required |
| `t/plugin/prometheus.t` | 1-41 | prometheus | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/prometheus2.t` | 1-50 | prometheus | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/prometheus3.t` | 1-7 | prometheus | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/prometheus4.t` | 1-15 | prometheus | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/proxy-rewrite2.t` | 2-4, 6-8 | proxy-rewrite | proxy-rewrite.yaml | converted | X-Forwarded-* pass/customize/untrusted integration GREEN |
| `t/plugin/proxy-rewrite2.t` | 1, 5 | proxy-rewrite | - | blocked_runtime | serverless-pre-function Lua log hooks |
| `t/plugin/proxy-rewrite3.t` | 1-22, 25-43 | proxy-rewrite | proxy-rewrite.yaml | converted | method/host/header/regex-uri/unsafe-uri integration GREEN |
| `t/plugin/proxy-rewrite3.t` | 23-24 | proxy-rewrite | - | blocked_design | /$uri/remain CRLF re-encoding is OpenResty-native |
| `t/plugin/response-rewrite2.t` | 1-25 | response-rewrite | response-rewrite.yaml | converted | filter/header schema and behavior integration GREEN |
| `t/plugin/response-rewrite3.t` | 1-22 | response-rewrite | response-rewrite.yaml | converted | gzip/brotli upstream decode integration GREEN |
| `t/plugin/serverless.t` | 1-26 | serverless-pre/post-function | - | blocked_runtime | Lua/OpenResty runtime; bounded Go compat exists but full parity is out of scope |
| `t/plugin/zipkin.t` | 1-23 | zipkin | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/zipkin2.t` | 1-11 | zipkin | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/zipkin3.t` | 1-6 | zipkin | - | blocked_design | registered Deferred default; Task 5 separate subsystem decision required |
| `t/plugin/security-warning.t` | 5-6 | cas-auth | cas-auth.yaml | converted | mapped by the cas-auth standalone manifest |
| `t/plugin/security-warning.t` | 1-4, 7-20 | cross-plugin security-warning family | - | pending | cross-plugin TLS security-warning family; outside current selections, Task 3/6 scope decision required |
| `t/plugin/security-warning2.t` | 19-20 | wolf-rbac | wolf-rbac.yaml | converted | mapped by the wolf-rbac standalone manifest |
| `t/plugin/security-warning2.t` | 1-18, 21-22 | cross-plugin security-warning family | - | pending | cross-plugin TLS security-warning family; outside current selections, Task 3/6 scope decision required |

## Checkpoint (Task 1 Step 5)

- All 321 pinned source files are represented in the matrix above.
- All 5,055 TEST labels occur exactly once across the rows (verified
  programmatically from the pinned checkout headers).
- Disposition counts: 251 `converted` rows, 70 `pending`/`blocked_*`/`non_plugin`
  rows for fully unreferenced sources, and 2 unselected `pending` partitions of
  the shared security-warning sources.

Conversion work does not start while any file or label is absent from this
matrix.

## Task 4 Wave Evidence (2026-08-02)

The six approved Supported-plugin waves were executed RED-GREEN against the
real APISIX-Go process. Each source family below lists the manifest cases, the
focused integration command, and the package command that passed.

| Wave | Source blocks converted | Product fixes (focused RED first) | Evidence commands |
|---|---|---|---|
| chaitin-waf | 6/6 (reject 1-4, timeout 1-2) | none | `go test ./t/plugin -run 'TestPluginIntegration/chaitin-waf/(block-mode-reject|monitor-mode-reject-passes-through|read-timeout-falls-back)$'`; `go test ./pkg/plugin/chaitin_waf` |
| body-transformer | 3/7 (multipart 1-2, bt2 3); 4 blocked_design | none | `go test ./t/plugin -run 'TestPluginIntegration/body-transformer/(multipart-request-to-json|multipart-response-to-json|key-auth-failure-preserves-body-transformer-config)$'`; `go test ./pkg/plugin/body_transformer` |
| elasticsearch-logger | 8/10 (1-3, 5-9); 2 blocked_runtime | `${arg_id}` brace-form index variable resolution (`TestHandlerResolvesBraceFormApisixVariableInIndex`) | `go test ./t/plugin -run 'TestPluginIntegration/elasticsearch-logger/(max-pending-entries-drop|header-auth-sent|date-variable-index|apisix-variable-index|combined-variable-index|template-not-mutated-across-requests|dollar-brace-variable-no-time-replacement)$'`; `go test ./pkg/plugin/elasticsearch_logger` |
| ai-proxy (kafka-log) | 6/9 (1-6); 3 blocked (7-8 design, 9 runtime) | none | `go test ./t/plugin -run 'TestPluginIntegration/ai-proxy/(kafka-log-summaries-and-payloads|kafka-log-summaries-no-payload|kafka-log-no-summary-no-payload)$'`; `go test ./pkg/plugin/ai_proxy ./pkg/plugin/ai_runtime ./pkg/plugin/kafka_logger` |
| response-rewrite | 47/47 (rr2 1-25, rr3 1-22) | unknown `options` flag rejection (`TestPostInitRejectsUnknownFilterOptionsFlag`); remove-wins-over-add header order (`TestHandlerRemoveWinsOverAddForSameHeader`); unsupported-encoding filter warning + Content-Encoding clearing (`TestHandlerSkipsFiltersWhenEncodedBodyCannotBeDecoded`, `TestHandlerWarnsWhenFiltersSeeUnsupportedEncoding`) | `go test ./t/plugin -run 'TestPluginIntegration/response-rewrite/...'` (26 cases); `go test ./pkg/plugin/response_rewrite` |
| proxy-rewrite | 47/51 (pr2 2-8, pr3 1-22+25-43); pr2 1+5 blocked_runtime, pr3 23-24 blocked_design | method enum validation (`TestPostInitRejectsUnknownMethod`); server-level X-Forwarded-Proto/Host/Port defaults + untrusted overwrite (`normalizeForwardedHeaders`); decoded-path route matching (`TestPinDecodedRoutePathMatchesEncodedRequestURI`); RawPath clearing when unsafe disabled | `go test ./t/plugin -run 'TestPluginIntegration/proxy-rewrite/...'` (25 cases); `go test ./pkg/plugin/proxy_rewrite ./pkg/server ./pkg/route` |

Total converted blocks after Task 4: **4,048** (was 3,931). Pending/blocked
plugin-owned blocks: **957**. Non-plugin blocks: **50**. Full gate results:
`go test ./...` PASS, `make lint` PASS (0 issues), `make build` PASS,
`git diff --check` clean.

Remaining 957 blocks are the Task 5 deferred-default and Task 6 native/runtime
families listed in the matrix above; they stay blocked pending the scope
decisions required by Tasks 5-6 of the master plan.

## Task 7 Converted-Corpus Semantic Re-Audit

### Scope decision and locked state

On 2026-08-02 the user explicitly declined Task 5 and Task 6 support for the
current phase. Their `pending`, `blocked_runtime`, `blocked_design`, and
`non_plugin` rows remain visible and are not reclassified as converted. Task 7
therefore audits only the current converted manifests; it does not claim all
5,055 upstream blocks are implemented.

| Check | Value |
|---|---|
| Review branch | `codex/task7-corpus-reaudit` |
| Reviewed HEAD | `df129144d69d889dcbdb65f6e40c3b9554b380c8` |
| Upstream source pin | `c3d7d5ec69774121f53d2e20d29d09c816795dd7` |
| Review mode | source-to-manifest semantic comparison only; record findings, do not repair |
| Invalidation rule | any later edit to a reviewed manifest or its owning production paths invalidates that owner row |

Results use `PASS` only when the current manifest has an observable assertion
for every non-setup source behavior. `FINDING` means source labels are selected
mechanically but at least one source behavior can break without failing the
manifest. Passing integration/package commands are execution evidence, not a
substitute for the source comparison.

### Owner review ledger

| Owner | Source labels reviewed | Manifest blob | Result | Behavior proof or finding | Evidence commands |
|---|---|---|---|---|---|
| `server-info` | `server-info.t` 1-2 | `7b37b7666e75cbc1e68933c6b5417becbc080153` | FINDING F-001 | TEST 2 is observed through exact `/v1/server_info` status/body fields. TEST 1 instead requires an etcd record under `/data_plane/server_info/<id>` with the configured 60-second report TTL; the manifest maps it to the same control API and asserts no etcd key, lease, put, renewal, or registration lifecycle. | integration batch PASS; `go test ./pkg/plugin/server_info` PASS |
| `node-status` | `node-status.t` 1-5 | `0de2109384913cad47627a3839afa2e2db09e93a` | PASS | Setup TEST 1 is consumed by real requests. Exact assertions cover GET 200 with UUID and `accepted`, PATCH 404, and configured instance ID. The Go-native control route replaces the Lua admin setup without weakening the observable plugin behavior. | integration batch PASS; `go test ./pkg/plugin/node_status` PASS |
| `redirect2` | `redirect2.t` 1-3 | `838344f1e9e46a588e57f1d4e00ad4bf217f75de` | PASS | TESTS 1-2 assert exact 302 Location including original query append; TEST 3 asserts the old `append_query_string: false` shape is accepted and produces exact HTTPS Location without query data. | integration batch PASS; `go test ./pkg/plugin/redirect` PASS |
| `proxy-control` | `proxy-control.t` 1-4 | `5f4d9f5b0c80bb702da703df361dc050bc7d9738` | FINDING F-002 | Upstream distinguishes `request_buffering: false` from `true` with a 51,200-byte body and the count/timing of NGINX temporary-file buffering log events. Both manifest cases send only 15 bytes and assert the same upstream body/200 response, so they still pass if `request_buffering` is ignored or both branches behave identically. | integration batch PASS; `go test ./pkg/plugin/proxy_control` PASS |
| `kafka-proxy` | `kafka-proxy.t` 1-2 | `73fcc72bd412c04ba1152da0447daf0c38001508` | FINDING F-003 | TEST 1 schema branches are independently observable. TEST 2 starts from plaintext `admin-secret` and proves admin readback is decrypted while etcd persistence is exact ciphertext. The manifest starts from pre-encrypted `$encrypted://...` data and only asserts an unrelated 200 upstream response; it never exercises write-time encryption, decrypted readback, or encrypted-at-rest state. | integration batch PASS; `go test ./pkg/plugin/kafka_proxy ./pkg/data_encryption` PASS |
| `public-api` | `public-api.t` 1-8 | `c8f45e9b287d84a3103dfeba962324f94fa7d078` | PASS | Exact schema rejection, direct registered API dispatch to the wolf-rbac 401 boundary, missing/wrong API 404s, valid key-auth pass-through, and missing-key-auth rejection are observed. Setup-only route/consumer blocks are consumed by the named requests. | integration batch PASS; `go test ./pkg/plugin/public_api` reports no test files |

Batch command:

```bash
go test ./t/plugin -run 'TestPluginIntegration/(server-info|node-status|redirect2|proxy-control|kafka-proxy|public-api)(/|$)' -count=1
```

Result: PASS. This does not clear F-001 through F-003 because those findings
identify source behavior that the passing manifests do not assert.

#### Batch 2

| Owner | Source labels reviewed | Manifest blob | Result | Behavior proof or finding | Evidence commands |
|---|---|---|---|---|---|
| `http-dubbo` | `http-dubbo.t` 1-5 | `e9baa74441fa3c981a9776f37379e262b446e467` | PASS | Exact Dubbo request frames and decoded POJO/array responses are asserted; timeout, void response, and upstream failure retain their status/log boundaries. | integration batch PASS; `go test ./pkg/plugin/http_dubbo` PASS |
| `example-plugin` | `example.t` 1-13 | `69e84e77f9d907236696c2b557e745424924e451` | FINDING F-004 | Schema failures are observable, but TESTS 6-7 require exact configured-plugin load/filter output and the missing-module error, while the manifest only activates a route. TESTS 8-9 require the plugin to produce the configured port and TESTS 12-13 require body-filter/delayed-body-filter phase logs; the manifest supplies `1981` directly from its upstream and checks no phase log, so a no-op plugin still passes. | integration batch PASS; `go test ./pkg/plugin/example_plugin` PASS |
| `client-control` | `client-control.t` 1-12 | `76a5c85f82bfc7eaae879047562a252e64e61698` | PASS | Exact-limit, over-limit, chunked rejection, zero/unlimited behavior, 30 MiB global versus 50 MiB route ordering, 31 MiB payload acceptance/rejection, and cleanup state are observable. Setup blocks are consumed by their request cases. | integration batch PASS; package has no test files |
| `echo` | `echo.t` 1-11 | `3a568cf586299ee41ab136b345754ad8a7c8cfee` | FINDING F-005 | Body/header and chunked response behavior is asserted. TEST 7, however, reads the stored plugin configuration and proves response filtering did not mutate persisted config. The manifest replays the same request behavior but never reads/asserts stored configuration, so dirty-data mutation can regress without failure. | integration batch PASS; `go test ./pkg/plugin/echo` PASS |
| `referer-restriction` | `referer-restriction.t` 1-13 | `97d26ca963f585c71e0143d09806db547eb6ff85` | PASS | Wildcard/exact whitelist, missing and malformed Referer handling, bypass, invalid schema branches, blacklist reject message, allowed host, and whitelist/blacklist exclusivity all have observable status/body/log assertions. | integration batch PASS; `go test ./pkg/plugin/referer_restriction` PASS |
| `openfunction` | `openfunction.t` 1-15 | `b2ac7f1e75bab13d98f493c6ee89af6ed1a8fa89` | PASS | Schema branches, GET/POST body forwarding, service-token and client Authorization precedence, not-found mapping, and wildcard path forwarding are asserted at the loopback function boundary and client response. | integration batch PASS; `go test ./pkg/plugin/openfunction` PASS |
| `aws-lambda` | `aws-lambda.t` 1-9 | `ab6d1d1e3f621145d9b5f58d05d151167b5dea04` | FINDING F-006 | Endpoint, API-key, IAM signature, and canonical-query behavior are observable. TEST 8 starts from plaintext credentials and proves plaintext admin readback plus non-plaintext persisted values. The manifest starts from fixed `$encrypted://...` values and only proves runtime decryption into outbound headers; write-time encryption, admin readback, and encrypted-at-rest state are absent. | integration batch PASS; `go test ./pkg/plugin/aws_lambda ./pkg/data_encryption` PASS |

Second batch command:

```bash
go test ./t/plugin -run 'TestPluginIntegration/(http-dubbo|example-plugin|client-control|echo|referer-restriction|openfunction|aws-lambda)(/|$)' -count=1
```

Result: PASS; F-004 through F-006 remain semantic gaps despite the passing
execution.

#### Batch 3

| Owner | Source labels reviewed | Manifest blob | Result | Behavior proof or finding | Evidence commands |
|---|---|---|---|---|---|
| `mocking` | `mocking.t` 1-22 | `f1ef5958988a04976768fe5f25f111b2ce1be580` | PASS | Exact example/schema response values, content type, request-variable expansion, empty unknown variable, configured headers, and route-id header expansion are asserted. | integration/package batch PASS |
| `lago` | `lago.t` 1-2 | `b54e59d1c0ceacf91a6bfe48ad40f4c45c35c7f9` | PASS | Schema branches, authenticated consumer isolation, request classification, exact event endpoint/auth/payload fields, and allowed/denied client results are observed by protocol fixtures. | integration/package batch PASS |
| `attach-consumer-label` | `attach-consumer-label.t` 1-17 | `8f8053f4239dd5e2caf1a0e6b84fb0c29a51aed7` | PASS | Schema branches, unauthenticated absence, authenticated labels, label removal/re-addition, global-rule behavior, and stripping of forged client headers are asserted at the upstream boundary. | integration/package batch PASS |
| `exit-transformer` | `exit-transformer.t` 1-20 | `7bc393c31913c909fbf72b69f8f60b71742b9c99` | PASS | Syntax/runtime failures, status remapping, auth and rate-limit bodies, request-conditional transforms, table bodies, and content-type outcomes have exact status/body/log/header assertions. | integration/package batch PASS |
| `azure-functions` | `azure-functions.t` 1-14 | `9089db145c525dc531314fe817345df89a346114` | PASS | Schema, endpoint/body/header propagation, HTTP/2 connection-header stripping, API-key precedence and metadata fallback, and all wildcard path forms are observed at fixture and client boundaries; final setup is followed by a stronger executable request. | integration/package batch PASS |
| `error-page` | `error-page.t` 1-20 | `a0c3ad556073116dfa9b365f7655688cca9bcb79` | PASS | Missing/added/deleted metadata, exact 500/502/503/404 pages, unconfigured fallback, custom content type, upstream 500 pass-through, and proxy-error interception are independently observable. | integration/package batch PASS |
| `brotli` | `brotli.t` 1-37 | `170e3f712ce8c18b447c40459dfe2dd452f0c7c7` | PASS | Negotiation, quality/wildcard exclusions, compression level/size, minimum length, HTTP version, content types, Vary, schema rejection, body round-trip, pre-encoded upstream, and ETag/Last-Modified behavior use decoded-body and exact header assertions. | integration/package batch PASS |
| `uri-blocker` | `uri-blocker.t` 1-22 | `27db5e9491fd50e712a7f768559d0bda4aa55967` | PASS | Invalid regex/schema, single/multiple/SQL rules, misses, custom messages, case-insensitivity, and normalized anchored-path bypass all retain exact status/body/log behavior. | integration/package batch PASS |
| `csrf` | `csrf.t` 1-15 | `5e470188b256b4f14dc0dc95470845eee80a3f8b` | FINDING F-007 | Cookie/header/signature/expiry behavior is observable. TEST 15 starts from plaintext `userkey` and asserts plaintext admin readback plus exact encrypted etcd storage. The manifest starts from the fixed ciphertext and only proves runtime decryption, leaving write-time encryption and persistence untested. | integration batch PASS; `go test ./pkg/plugin/csrf ./pkg/data_encryption` PASS |
| `openwhisk` | `openwhisk.t` 1-21 | `82eba6ee0a862156a1681a7aa2c9e2451cbe7797` | FINDING F-008 | Request mapping, malformed bodies, errors, package actions, status, headers, and body are observed. TEST 5 explicitly reads encrypted `service_token` from etcd after a plaintext route write; the manifest begins with the known ciphertext and asserts only its decrypted outbound Authorization header, so encrypted-at-rest persistence is absent. | integration batch PASS; `go test ./pkg/plugin/openwhisk ./pkg/data_encryption` PASS |
| `real-ip` | `real-ip.t` 1-24 | `0031874a815b0845a1e7d09aaf3806051b2e52ad` | PASS | IPv4/IPv6/port parsing, X-Forwarded-For selection, recursive and plugin/global trusted-address rules, rejection paths, and forged forwarding removal are asserted through status and upstream-observed address/port fields. | integration/package batch PASS |

Third batch command:

```bash
go test ./t/plugin -run 'TestPluginIntegration/(mocking|lago|attach-consumer-label|exit-transformer|azure-functions|error-page|brotli|uri-blocker|csrf|openwhisk|real-ip)(/|$)' -count=1
```

Result: PASS; F-007 and F-008 remain unasserted persistence behavior.

#### Batch 4

| Owner | Source labels reviewed | Manifest blob | Result | Behavior proof or finding | Evidence commands |
|---|---|---|---|---|---|
| `authz-casdoor` | `authz-casdoor.t` 1-14 | `ceb1cb016f2420ff0f5476cca2028b682399acca` | FINDING F-009 | Redirect, callback, state, token, cookie scoping, and expiry behavior are observable. TEST 10 writes plaintext `client_secret`, reads the plaintext through the admin API, and reads exact ciphertext from etcd. The manifest starts from that ciphertext and only proves provider-boundary decryption, so write-time encryption, decrypted readback, and encrypted-at-rest storage can all regress without failure. | integration batch PASS; `go test ./pkg/plugin/authz_casdoor ./pkg/data_encryption` PASS |
| `authz-keycloak` | five sources, all 45 labels | `8eaddfb231e18e9081b870665aaa99ef5839511f` | FINDING F-010 | Direct/discovery authorization, password grant, lazy permissions, redirects, secret references, TLS, and repeated-request isolation have observable provider/client assertions. `authz-keycloak3.t` TEST 3 writes a plaintext `client_secret` and proves plaintext admin readback plus exact encrypted etcd storage; its manifest starts from fixed ciphertext and asserts only provider-boundary decryption. | integration batch PASS; `go test ./pkg/plugin/authz_keycloak ./pkg/data_encryption` PASS |
| `feishu-auth` | `feishu-auth.t` 1-14 | `2df15874b48e506b3b2a7fd461be1c09c8446757` | FINDING F-011 | Redirect, query/header code, provider exchange, signed-cookie reuse/expiry, rotation, and forged-header clearing are observable. TEST 14 writes plaintext `secret_fallbacks`, reads plaintext through the admin API, and proves the persisted value is encrypted. The manifest starts from ciphertext and proves runtime fallback decryption plus absence from generated configs/logs, but never asserts the write/read/persist lifecycle or bbolt value. | integration batch PASS; `go test ./pkg/plugin/feishu_auth ./pkg/data_encryption` PASS |
| `jwe-decrypt` | `jwe-decrypt.t` 1-23 | `5549221e8fdac121a924f453ee2781090fa3ea60` | FINDING F-012 | Schema, extraction, raw/base64 secrets, bearer variants, consumer replacement, forwarded plaintext, and malformed/decryption failures are observable. TEST 7 reads the exact encrypted consumer `key` and `secret` stored after the plaintext setup. The manifest instead creates the consumer with fixed ciphertext and only proves request-time decryption, with no stored-state assertion. | integration batch PASS; `go test ./pkg/plugin/jwe_decrypt ./pkg/data_encryption` PASS |
| `jwt-auth` | seven sources, all 130 labels | `44b94d0c011cff71d250b8f62d4d22a95ae917d9` | PASS | Independent cases retain schema, token-location/hiding, anonymous/realm, HS/RS/PS/ES/EdDSA, claim/leeway, base64, Vault/environment, replacement, and context assertions. `jwt-auth3.t` TEST 14 starts from plaintext and the manifest independently asserts exact bbolt ciphertext while forbidding the plaintext secret. | integration batch PASS; `go test ./pkg/plugin/jwt_auth ./pkg/data_encryption` PASS |
| `openid-connect` | twelve sources, all 141 labels | `48adc9bb12dab846a1f1fde50921ae14fd9ea870` | PASS | Independent provider fixtures and client assertions retain bearer/code, introspection/JWT/JWKS, discovery, sessions/PKCE/Redis, claims, renewal/logout/revocation, forwarding/proxy, TLS, and header behavior. All four upstream encryption cases start from plaintext and assert the exact bbolt ciphertext for client secret, RSA private key, and Redis password while forbidding plaintext. | integration batch PASS; `go test ./pkg/plugin/openid_connect ./pkg/data_encryption` PASS |
| `rocketmq-logger` | three sources, all 42 labels | `3f9bc3842e1d98f8dc06c31c363516e85b8f52eb` | FINDING F-013 | Schema, nameserver/broker protocol, signing, formats, partitioning, expressions, compression, reload, errors, and pending publication are observable. `rocketmq-logger2.t` TEST 18 requires plaintext admin readback and exact encrypted-at-rest `secret_key`. The manifest starts from plaintext but only proves successful authenticated publication; it never reads the admin representation or asserts the stored ciphertext/non-plaintext state. | integration batch PASS; `go test ./pkg/plugin/rocketmq_logger ./pkg/data_encryption` PASS |
| `splunk-hec-logging` | two sources, all 17 labels | `0673f1c52a37a612a00ef3d3b3f5aa6d893e03bb` | FINDING F-014 | Schema, non-blocking errors, exact HEC headers/envelopes, custom/default formats, batching, keepalive, extras, and pending overflow are observable. TEST 14 writes a plaintext token, reads it decrypted through the admin API, and proves non-empty non-plaintext etcd storage. The manifest starts from fixed ciphertext and asserts only the outbound Authorization header. | integration batch PASS; `go test ./pkg/plugin/splunk_hec_logging ./pkg/data_encryption` PASS |
| `request-id` | three sources, all 37 labels | `8a922fe89f6e58104d9c174007929f99cce0bb7d` | FINDING F-015 | UUIDv4, NanoID, range-id, KSUID, custom headers, include/exclude, client echo, and upstream-failure behavior are observable. The merged UUIDv7 case weakens TEST 21 from 180 concurrent requests to 180 sequential requests. More importantly, TEST 26 forces sequence overflow and requires a cached-time refresh, while TEST 27 injects a backwards clock and requires the encoded timestamp to stay fixed; ordinary sequential requests trigger neither branch. | integration batch PASS; `go test ./pkg/plugin/request_id` PASS |

Fourth batch command:

```bash
go test ./t/plugin -run 'TestPluginIntegration/(authz-casdoor|authz-keycloak|feishu-auth|jwe-decrypt|jwt-auth|openid-connect|rocketmq-logger|splunk-hec-logging|request-id)(/|$)' -count=1
```

Result: PASS in 54.337s; package and data-encryption batch PASS. F-009
through F-015 remain semantic gaps despite successful execution.

#### Batch 5

| Owner | Source labels reviewed | Manifest blob | Result | Behavior proof or finding | Evidence commands |
|---|---|---|---|---|---|
| `ai-prompt-decorator` | `ai-prompt-decorator.t` 1-17 | `40c39a05e15ab16d416dff694241aaeb5766c6cb` | PASS | Exact provider-boundary JSON assertions cover chat prepend/append/both, independent repeated requests, empty schema rejection, Responses instructions/string/array inputs, and the chat regression path. | integration/package batch PASS |
| `ai-rag` | `ai-rag.t` 1-17 | `66c4ab45808386c908966705dbcb127b9e835bdc` | PASS | Provider presence/auth/error branches, request validation, exact embedding/search/RAG payload transformations for chat and Responses, and self-signed TLS rejection versus `ssl_verify: false` provider delivery are observable. | integration/package batch PASS |
| `degraphql` | `degraphql.t` 1-15 | `da5f72de9a7a768e298e553ea58b74e7158a80a3` | PASS | Exact GraphQL POST/GET bodies, variables, operation name, forced content type, missing/invalid body errors, schema rejection, and response pass-through are asserted at fixture and client boundaries. | integration/package batch PASS |
| `loki-logger` | two sources, all 22 labels | `b2900b57c8baac50c0eec86b8a79015c7d765445` | PASS | Schema, tenant/auth headers, endpoint, default/custom records, static/dynamic/post-upstream labels, per-request isolation, metadata enrichment, cardinality, batching, and exact pending-overflow diagnostics are observable in decoded Loki pushes. | integration/package batch PASS |
| `data-mask` | `data-mask.t` 1-20 | `6a060d3642836f594852ba18be30b95832e0e099` | FINDING F-016 | Schema, argument limits, missing-body safety, multi-value query handling, and non-string JSON behavior are observable. TESTS 2, 4, 6, and 8 inspect masked query/header/body values in the file-logger record, and TEST 20 inspects the masked access-log request line. The manifest substitutes upstream request mutation assertions and configures neither logger, so logger capture/order regressions can pass. | integration batch PASS; `go test ./pkg/plugin/data_mask` PASS |
| `clickhouse-logger` | three sources, all 23 labels | `35589ddb6eaa63b0869086f3ac4f9d4d2b38e906` | FINDING F-017 | Schema, headers/envelopes, single-endpoint delivery, bodies/expressions, environment resolution, and overflow are observable. `clickhouse-logger.t` TEST 8 requires log evidence that 12 requests selected both configured endpoints. The manifest accepts `^/(one|two)$` independently for every delivery and has no both-endpoints assertion, so routing all batches to one endpoint still passes. | integration batch PASS; `go test ./pkg/plugin/clickhouse_logger` PASS |
| `request-validation` | two sources, all 55 labels | `303d0a384d3b2c969bcda8eb11108a8ed1b457ab` | PASS | Body/header schema types, enums, required fields, exact custom messages/codes, invalid config rejection, urlencoded charset and argument volume, and duplicate-key normalization have direct status/body/upstream assertions. | integration/package batch PASS |
| `skywalking-logger` | two sources, all 15 labels | `f6c6c1a52126d2a3776f6d91b7e245526d051474` | PASS | Schema, exact v3 log envelopes, hostname instance, valid/malformed SW8 context, metadata/route precedence, preserved request/response bodies, and pending-overflow counts/diagnostic are observable. | integration/package batch PASS |
| `ua-restriction` | `ua-restriction.t` 1-33 | `0dceb8495951fc3e9c345cfd937969323820a533` | FINDING F-018 | Schema, allow/deny regex and ordering, custom message, disabled mode, missing User-Agent, and bypass behavior are observable. TESTS 22-25 update one route from an active denylist to a configuration with no plugin, then prove access is allowed. The manifest uses independent variants and replaces removal with `_meta.disable: true`; neither the live update nor the absent-plugin state is tested. | integration batch PASS; `go test ./pkg/plugin/ua_restriction` PASS |

Fifth batch command:

```bash
go test ./t/plugin -run 'TestPluginIntegration/(ai-prompt-decorator|ai-rag|degraphql|loki-logger|data-mask|clickhouse-logger|request-validation|skywalking-logger|ua-restriction)(/|$)' -count=1
```

Result: PASS in 20.799s; package batch PASS. F-016 through F-018 remain
semantic gaps despite successful execution.

#### Batch 6

| Owner | Source labels reviewed | Manifest blob | Result | Behavior proof or finding | Evidence commands |
|---|---|---|---|---|---|
| `acl` | two sources, all 62 labels | `675b1d201af0068f940195f44c7967472177e1f8` | PASS | Consumer and external-user labels, allow/deny precedence, JSONPath and segmented/JSON parsers, multi-match parsing, comma-delimited labels, rejected codes/messages, and schema branches have direct status/body assertions. Final source DELETE blocks are cleanup-only and standalone case isolation supplies the equivalent clean state. | integration/package batch PASS |
| `authz-casbin` | `authz-casbin.t` 1-21 | `d5aba29b0bf1167bb002fdeb83a7841d29c35af2` | PASS | Schema, inline/metadata/file policies, route/method/user decisions, disabled behavior, and the deny-to-allow metadata update are observable; the update uses a changed snapshot and an authorization-based readiness probe before the post-update request. | integration/package batch PASS |
| `basic-auth` | three sources, all 44 labels | `20b023aafe5add1e7660993090685babc4d9c6a6` | PASS | Consumer/route schema, parsing and credentials, hide/preserve, anonymous limiter chains, realms, replacement/last-good behavior, and real Vault/environment resolution including fail-closed cases have exact client/upstream assertions. | integration/package batch PASS |
| `consumer-restriction` | two sources, all 71 labels | `d20e2ec1bae688639af432302789af3fbb9440c2` | FINDING F-019 | Static whitelist/blacklist, method, consumer/route/service/group IDs, auth chaining, rejection code/message, and missing-identity behavior are observable. TESTS 28-34 update one active route to remove `consumer-restriction`; the manifest starts an independent route with `_meta.disable: true`. TESTS 47-50 update the same consumer from route whitelist to blacklist, while the manifest uses two independent static consumers. Neither live replacement lifecycle can fail the current cases. | integration batch PASS; `go test ./pkg/plugin/consumer_restriction` PASS |
| `hmac-auth` | six sources, all 70 labels | `0cc73812d989b65eb44723bb2d2441b7b58e55d2` | PASS | Consumer/route schemas, signature parsing, clock skew/replay, SHA algorithm allowlists, signed-header defaults and cardinality, body digest and restoration, hiding, anonymous/realm chains, and real Vault/environment resolution have exact status/body/upstream assertions. | integration/package batch PASS |
| `key-auth` | four sources, all 58 labels | `ae33e21fea53242b9415e938ba48e63ea7b32e4b` | PASS | Header/query credentials, hiding only the configured credential locations, anonymous/realm/service/domain-node behavior, consumer replacement, and real Vault/environment plus unresolved-reference isolation are independently observable. | integration/package batch PASS |
| `ldap-auth` | two sources, all 35 labels | `a74da5889ec6f6bc3f1f1cda2af68b4c522874dd` | PASS | Schema, case-insensitive Basic parsing, exact realms, direct LDAP bind bytes, failed/successful auth, trusted/untrusted LDAPS, and Vault/environment references are asserted at protocol and client boundaries. | integration/package batch PASS |
| `multi-auth` | two sources, all 38 labels | `97e3bdb2cd074cacc25b5e78c784769dac213d82` | PASS | Ordered Basic/Key/JWT/HMAC alternatives, body replay, later-provider success, all-failed diagnostics, token locations/hiding, invalid nested configs, consumer chaining, JWT failures, and anonymous cases have exact upstream/client assertions. | integration/package batch PASS |
| `wolf-rbac` | `wolf-rbac.t` 1-42 plus `security-warning2.t` 19-20 | `352d4c4722a09b4375eb09f9ed6374ec829bdf87` | PASS | Schema, public login/permission APIs, token and credential errors, exact retry/permission behavior, JSON/raw/large passwords, Vault/environment references, duplicate-appid consumer update, trusted-IP/TLS warning, and consumer echo chaining are observable. | integration/package batch PASS |

Sixth batch command:

```bash
go test ./t/plugin -run 'TestPluginIntegration/(acl|authz-casbin|basic-auth|consumer-restriction|hmac-auth|key-auth|ldap-auth|multi-auth|wolf-rbac)(/|$)' -count=1
```

Result: PASS in 25.979s; package batch PASS. F-019 remains an unasserted
replacement/removal lifecycle.

#### Batch 7

| Owner | Source labels reviewed | Manifest blob | Result | Behavior proof or finding | Evidence commands |
|---|---|---|---|---|---|
| `api-breaker` | `api-breaker.t` 1-20 | `be7cc906802ff7af94e729c412cd7a49144287ac` | PASS | Schema, unhealthy thresholds, fallback status/body/headers, reset timeout, half-open recovery and relapse, and per-status counters are asserted with ordered requests and real waits. | integration/package batch PASS |
| `fault-injection` | two sources, all 46 labels | `fe37435b7c0c687eb9781e349044dd389149b4eb` | PASS | Abort/delay schemas, percentages, variables, status/body/header overrides, zero-percent bypass, real elapsed-time bounds, combined abort-delay ordering, and variable parsing regressions are observable. | integration/package batch PASS |
| `limit-conn` | six sources, all 104 labels | `f6200bd34f99eab34510f178838d7f1edbb5c8d0` | PASS | Local/Redis/Cluster counters, authentication/TLS/SNI, dynamic variables, route/global/consumer isolation, connection reuse, delays/logs, retries, and HTTP/2 use real concurrency, held upstream responses, exact status counts, and elapsed bounds. | integration/package batch PASS |
| `limit-count` | eighteen sources, all 252 labels | `828cae04306e37653403d011dff6c2a03399a509` | PASS | Fixed/sliding windows, delayed sync and queue overflow, Redis/Sentinel/Cluster/TLS/auth, shared and isolated counters, dynamic count, reset headers, connection reuse, access-log `rate_limiting_info`, reloads, and variable keys retain exact header/log/state assertions. | integration/package batch PASS |
| `limit-req` | six sources, all 89 labels | `3f5838f110436a2fba22115406f6b15e13e65286` | PASS | Local/Redis/Cluster leaky-bucket behavior, atomic concurrent limits, authentication/reuse, route/consumer/global scope, dynamic keys, delay versus nodelay timing, rejection status/logs, and config changes are directly observable. | integration/package batch PASS |
| `proxy-cache` | two sources, all 76 labels | `0d483f4e72355ce2a09f428b96ae37100d873c0c` | PASS | Memory/disk MISS/HIT/BYPASS, cache keys, methods/statuses, request directives, Vary variants, TTL expiry/refresh, PURGE including expired indexes, headers, and body integrity use ordered real requests and expiry waits. | integration/package batch PASS |
| `proxy-mirror` | three sources, all 36 labels | `6d7117e37e7c88906cf618a115e5ddb2ee1317e8` | PASS | Schema, exact mirror method/path/query/body/headers, percentage selection, timeout/non-blocking behavior, reload, large/concurrent bodies, and request isolation are asserted at primary and mirror fixtures; final DELETE is cleanup-only. | integration/package batch PASS |
| `traffic-split` | five sources, all 94 labels | `8e2417aa49b88cce607b78eea36d5eefb5b120de` | PASS | Ordered matches, weights/fallback/zero weights, inline and referenced upstreams, chash/pass-host, HTTPS, health/retry, timeouts, form-body matching, and reload behavior have exact distribution, fixture, timing, and transition assertions. | integration/package batch PASS |
| `workflow` | four sources, all 42 labels | `944639d67e99ea47af4bf849b06092ab39b137bd` | PASS | Return and limit actions, rule order/conditions, schema failures with last-good retention, counter sharing/isolation, concurrent limit-conn actions, phase interaction, plugin removal, and cross-route state have direct status/count/update assertions. | integration/package batch PASS |
| `batch-requests` | three sources, all 46 labels | `4730ca9f1347d44da8feefff0f93298daba96778` | PASS | Schema, ordered pipelines, headers/query/body propagation, per-item errors, first/last timeout timing, size/count limits, HTTP/gRPC subrequests, and config limit transitions use exact aggregate responses, body sizes, and elapsed bounds. | integration/package batch PASS |

Seventh batch command:

```bash
go test ./t/plugin -run 'TestPluginIntegration/(api-breaker|fault-injection|limit-conn|limit-count|limit-req|proxy-cache|proxy-mirror|traffic-split|workflow|batch-requests)(/|$)' -count=1
```

Result: PASS in 179.901s; package batch PASS. No new semantic finding was
identified in this batch.

#### Batch 8

| Owner | Source labels reviewed | Manifest blob | Result | Behavior proof or finding | Evidence commands |
|---|---|---|---|---|---|
| `ai-aws-content-moderation` | three sources, all 23 labels | `d0877259bac872e2a50669cf3b9cf0fee61aa39b` | PASS | Schema/secrets, SigV4 request construction, prompt extraction, provider allow/block/error responses, and credential precedence are asserted at the provider and client boundaries. | integration/package batch PASS |
| `ai-prompt-guard` | `ai-prompt-guard.t` 1-44 | `114d0fa5bf59fc1cdd54c297cebe5bfe9faa2324` | PASS | Allow/deny patterns, case sensitivity, array/string chat and Responses inputs, malformed/non-AI pass versus deny behavior, custom codes/messages, and schema conflicts have exact upstream/client assertions. | integration/package batch PASS |
| `ai-proxy` | twenty converted source partitions, 309 labels | `0825f7d9929850e8356c2f7bb1c2add4b3aa5e20` | PASS | Provider/protocol conversions, auth and endpoint paths, pass-through, non-stream/stream framing and usage, disconnect/flush limits, tool/reasoning/cache fields, Bedrock EventStream, query/method/body overrides, response budgets, upstream variables, logging, errors, and client isolation are observable through exact fixtures, chunks, timing, and logger payloads. | integration/package batch PASS |
| `ai-rate-limiting` | three sources, all 58 labels | `c1e15276cfc8ee623c8e7f7150712b53e0c20e7d` | PASS | Local/Redis token accounting, expressions, streaming/non-streaming usage, consumer isolation, headers/windows/rejection, config reloads, and Redis password persistence are asserted; the encryption case starts plaintext and checks exact bbolt ciphertext with plaintext forbidden. | integration/package batch PASS |
| `ai-request-rewrite` | two sources, all 19 labels | `a4e95764edbdbf59af41043da5d9c442007ea87d` | PASS | Chat/Responses prompt replacement, variable expansion, message roles, malformed/unsupported bodies, content type, and schema branches have exact upstream JSON and client assertions. | integration/package batch PASS |
| `body-transformer` | three converted partitions, 23 labels | `b0c405ece911f818994a5540b6068ef597c07188` | PASS | SOAP/JSON/template request-response transforms, variables/content types, schema/runtime errors, malformed bodies, multipart conversion, key-auth error preservation, and repeated-request isolation have exact fixture/body assertions. The four explicitly blocked Lua/multipart-object labels remain outside this ledger. | integration/package batch PASS |
| `chaitin-waf` | three sources, all 21 labels | `130e383da925c9c4e1bc48b11710880d50036723` | PASS | Schema, detector protocol, request fields, monitor/block decisions, provider errors, rejection bodies, and read-timeout fallback are observable at detector, upstream, and client boundaries. | integration/package batch PASS |
| `cors` | four sources, all 86 labels | `3e32c7ad2b0f99dd751fb8ed2453f11f3ecb9f49` | PASS | Preflight/simple requests, wildcard/regex/multiple origins, methods/headers/credentials/expose/max-age, upstream override/remove behavior, schema, and route/global interactions have exact response and upstream assertions. | integration/package batch PASS |
| `gzip` | `gzip.t` 1-19 | `465d6544823b97d3a96692283c2696a6955e8107` | PASS | Negotiation, buffers/levels/minimum length, MIME and status/version gating, Vary, validators, existing encodings, and decoded-body round trips have direct size/header/body assertions. | integration/package batch PASS |
| `proxy-rewrite` | three converted partitions, 104 labels | `058d263b5856809c75223723a205657b6fac1c8c` | FINDING F-020 | Method/URI/regex/host/scheme/header/query/variable behavior, forwarded-header trust, encoded paths, and schema branches are observable. `proxy-rewrite.t` TEST 35 reads the just-written stored plugin config and proves request processing did not dirty it; the manifest only exercises the rewritten upstream request and never reads/asserts stored configuration. | integration batch PASS; `go test ./pkg/plugin/proxy_rewrite` PASS |
| `redirect` | two sources, all 51 labels | `660e82917258e409f4075e34c25a6c5ad8b073d1` | PASS | HTTP-to-HTTPS, URI/regex redirects, query append/drop, encoded paths, status codes, variables, upstream fallthrough, and schema conflicts have exact Location/status/body assertions. | integration/package batch PASS |
| `response-rewrite` | three sources, all 74 labels | `65cc1739a3e714ad6e826ea070345b3eaa0d3baf` | FINDING F-021 | Status/body/header/filter behavior, variables, schema, remove-versus-add order, gzip/brotli decode, unsupported encodings, and upstream fallthrough are observable. `response-rewrite.t` TEST 15 reads the stored plugin config and proves header/body handling did not mutate it; the manifest asserts only the rewritten response, so dirty persisted config can regress without failure. | integration batch PASS; `go test ./pkg/plugin/response_rewrite` PASS |

Eighth batch command:

```bash
go test ./t/plugin -run 'TestPluginIntegration/(ai-aws-content-moderation|ai-prompt-guard|ai-proxy|ai-rate-limiting|ai-request-rewrite|body-transformer|chaitin-waf|cors|gzip|proxy-rewrite|redirect|response-rewrite)(/|$)' -count=1
```

Result: PASS in 95.650s; package batch PASS. F-020 and F-021 remain
unasserted stored-configuration invariants.

#### Batch 9

| Owner | Source labels reviewed | Manifest blob | Result | Behavior proof or finding | Evidence commands |
|---|---|---|---|---|---|
| `datadog` | `datadog.t` 1-13 | `f3e11cd5d897cc1a1c4ceede2b0ee03020a67a3c` | PASS | Metadata endpoint, exact DogStatsD lines/tags/order/coalescing, upstream latency, invalid resources, and runtime metadata update are observable. | integration/package batch PASS |
| `elasticsearch-logger` | two converted sources, all 27 labels | `ae70ce6c355e518321d784ad151f12f424c24583` | PASS | Version negotiation, bulk NDJSON, index/type compatibility, auth/headers, body capture, formats, deterministic two-endpoint selection, errors, and pending overflow are asserted. Both encryption cases start plaintext and check exact bbolt ciphertext with plaintext forbidden. | integration/package batch PASS |
| `error-log-logger` | four sources, all 39 labels | `e74a34b3e799e5e00fefbf72ae62c361c58dd3b5` | PASS | ClickHouse/Kafka/SkyWalking/TCP delivery, level filters, batching, hostname, legacy schema, metadata deletion, processor replacement, route removal, stale-log isolation, and exact encrypted secrets have protocol, transition, and persistence assertions. | integration/package batch PASS |
| `file-logger` | three sources, all 44 labels | `4ac6bb107665d98879b3fa8e5c0e5f72fcc125d7` | PASS | Schema, real file writes/reopen, default/nested/extra/route formats, depth warning, request/response and compressed bodies, domain-node host identity, truncation, and expression branches are asserted from file contents and client bodies. | integration/package batch PASS |
| `google-cloud-logging` | three sources, all 33 labels | `d1efaa18a0da261a0aa9fc483191840de7c30271` | PASS | Auth file/config, OAuth/JWT exchange, signed Logging API payloads, TLS controls, default/custom formats, batching/timeouts, body fields, pending overflow, and private-key encryption have exact provider, client, and bbolt assertions. | integration/package batch PASS |
| `http-logger` | seven sources, all 114 labels | `f7682b66974b78ac66a1aa3068d816bc920e1c55` | PASS | HTTP protocol/headers, JSON and newline concatenation, formats, body capture/limits/expressions/compression, retries/errors, batching, metadata/route precedence, and large-body boundaries are observable in exact sink requests. | integration/package batch PASS |
| `kafka-logger` | six sources, all 99 labels | `e8a0a369ec7055dc42999441f6776dae2c1fe056` | PASS | Real Kafka metadata/produce records, partitions, required acks, PLAIN/SCRAM, formats/bodies/compression, batching/shutdown, producer reload, errors, service sharing, and pending overflow are asserted. Password encryption starts plaintext and checks exact bbolt ciphertext. | integration/package batch PASS |
| `log-rotate` | three sources, all 17 labels | `fd81d741810e6de35fc3bf927c5dbdf37f818bf9` | PASS | Time/size rotation, alignment, compression, current/rotated contents, concurrent writers, hot-disable, reopen with missing files, disabled access log, ownership/permissions, and retention are asserted through ordered actions and post-shutdown filesystem checks. | integration/package batch PASS |
| `loggly` | `loggly.t` 1-22 | `c0fa1351323d20a3f986d32e3f00ebfbc86c3bac` | PASS | UDP/HTTP transport, tags/severity/PRIVAL, exact bulk payloads, formats, body expressions, metadata/route precedence, per-config batching, and failures are observable. Token encryption starts plaintext and checks exact bbolt ciphertext. | integration/package batch PASS |
| `sls-logger` | `sls-logger.t` 1-17 | `74a3df3139b8df433c185f64a0880255b6cd1fcf` | PASS | TLS protocol/signature, batching/timestamps, metadata/route formats and removal, request/response bodies, invalid metadata, and error behavior have exact sink/client/update assertions; secret encryption has exact bbolt evidence. | integration/package batch PASS |
| `syslog` | `syslog.t` 1-21 | `4f656400fc2650c7d4d4503b3474568c3f0b4f55` | PASS | UDP/TCP/TLS RFC5424 framing, schema, formats, metadata/route/service precedence and removal, body fields, variables, and error isolation are asserted at real protocol fixtures. | integration/package batch PASS |
| `tcp-logger` | `tcp-logger.t` 1-17 | `a44289e745137ecf815949d9323d54dac82f9a6e` | PASS | TCP/TLS delivery, schema, errors, metadata/route formats and removal, nested/default records, variables, and request/response bodies are asserted at the network boundary. | integration/package batch PASS |
| `tencent-cloud-cls` | `tencent-cloud-cls.t` 1-22 | `4485bf8d940eaa3e0e60ecfa27da11087b30a731` | PASS | CLS protobuf/signature/headers, HTTP/TLS failures, batching, formats/removal, body expressions, and metadata are observable; secret encryption starts plaintext and checks exact bbolt ciphertext with plaintext forbidden. | integration/package batch PASS |
| `udp-logger` | `udp-logger.t` 1-14 | `e051d3b9d563d1f099be8fdb82042c148d1aa732` | PASS | UDP delivery, schema/errors, default/metadata/route formats, removal, nested records, variables, and request/response bodies have exact datagram/client assertions. | integration/package batch PASS |

Ninth batch command:

```bash
go test ./t/plugin -run 'TestPluginIntegration/(datadog|elasticsearch-logger|error-log-logger|file-logger|google-cloud-logging|http-logger|kafka-logger|log-rotate|loggly|sls-logger|syslog|tcp-logger|tencent-cloud-cls|udp-logger)(/|$)' -count=1
```

Result: PASS in 70.473s; package, logger-batch, and data-encryption commands
PASS. No new semantic finding was identified in this batch.

#### Batch 10

| Owner | Source labels reviewed | Manifest blob | Result | Behavior proof or finding | Evidence commands |
|---|---|---|---|---|---|
| `cas-auth` | `cas-auth.t` plus owned `security-warning.t`, all 24 labels | `0097ea830b5066dfc6495ae92987829e5ab975c5` | PASS | Login/validation/session/logout, multi-SP/SLO, callback initiation and forgery failures, session expiry/isolation/reload, signatures, and TLS warning behavior use captured tickets/cookies and exact provider/client requests. | integration/package batch PASS |
| `dingtalk-auth` | `dingtalk-auth.t` 1-14 | `0b25bb20a3df5be02555b0c4b31ee8e7f63e37e1` | PASS | Schema, redirect, query/header code, provider token/user requests, signed-cookie reuse/expiry, and custom code locations have exact provider/client/cookie/timing assertions. | integration/package batch PASS |
| `forward-auth` | three sources, all 28 labels | `7f170378a8dc13807800dac308970a0d550f33b3` | PASS | Auth request method/body/header framing, allow/deny/error/degradation, client/upstream header propagation and clearing, spoof/CRLF resistance, body-size limits, chunked replay, and GET/POST semantics are observable in raw and decoded fixture assertions. | integration/package batch PASS |
| `graphql-limit-count` | `graphql-limit-count.t` 1-26 | `bf47193ef8cb188933fadbd33a86118b811390df` | PASS | GraphQL parsing, operation/field costs, variables, aliases/fragments, invalid requests, body limits, rejection headers/codes, and counter behavior have exact status/body/header assertions. | integration/package batch PASS |
| `graphql-proxy-cache` | three sources, all 48 labels | `38a1252ac6e9f431fded5878a359af51ff45c6b2` | PASS | Memory/disk MISS/HIT, GET/POST/data bodies/variables, consumer and route/host isolation, schema/errors, and PURGE strategy/key/route outcomes use ordered origin counts and exact cache headers/bodies. | integration/package batch PASS |
| `ip-restriction` | `ip-restriction.t` 1-35 | `8dfd111de936be2d7f21f9fa21519bda6224eb27` | FINDING F-022 | IPv4/IPv6/CIDR allow/deny, forwarded real IP, custom messages/codes, schema, disabled mode, and bypass are observable. TESTS 17-18 update the previously restricted route to a configuration with no plugin and then prove access succeeds; the manifest merges those labels with TEST 25 into a static `_meta.disable: true` case, so actual plugin removal/reload is untested. | integration batch PASS; `go test ./pkg/plugin/ip_restriction` PASS |
| `oas-validator` | three sources, all 110 labels | `3c6c54213b3b5ab163e46f1529a421f1582179a4` | PASS | OAS 3.0/3.1 files and URLs, operations/parameters/body/header validation, refs/pathItems, skip/reject/code/verbose modes, fetch headers/errors, and spec caching have exact provider-count and client/upstream assertions. | integration/package batch PASS |
| `opa` | three sources, all 27 labels | `7b70acc8757f56394f95aeb8aa61802dc27f8fd0` | PASS | Schema, exact OPA input document, allow/deny/errors, response status/body/multiheaders, upstream request/header mutation and stale-header clearing, TLS, and variable behavior are asserted at policy/upstream/client boundaries. | integration/package batch PASS |
| `opentelemetry` | six sources, all 81 labels | `7a2f3a8829560c90fd8a37a71e92e9d5638b538c` | PASS | Sampling/parenting, propagation and request-id sources, metadata hot update, spans/attributes/core tree, rejected requests, malformed IDs, custom headers, protobuf state regression, and HTTP/2 stream isolation have exact decoded OTLP trace/count/update assertions. | integration PASS; corrected `go test ./pkg/plugin/otel` PASS |
| `saml-auth` | two sources, all 21 labels | `45a9be9ca03c7d2b43c664fc81a2046e7fe67d57` | PASS | Signed Redirect/POST login, callback correlation, host-sensitive multi-SP sessions, logout/SLO hops, IdP identity/endpoints, mixed route fallback, and terminal failures use exact signed messages, cookies, and ordered actions. | integration/package batch PASS |
| `skywalking` | two sources, all 17 labels | `e7b0ded2d123edb992979463dd25025c6c1440bb` | PASS | SW8 propagation/generation, sampling, route/global activation, exact segment/log metadata and body capture, collector failure, auth rejection, cleanup state, and shutdown delivery have decoded envelope/count/timing assertions. Source TEST 13 is cleanup-only; the subsequent auth behavior is independently exercised. | integration/package batch PASS |
| `traffic-label` | two sources, all 38 labels | `18d29640b6f0c84f2cd42553e37b44b51e299538` | PASS | Ordered rules, variables/operators, request/response header actions, route/global/consumer scope, multiple actions, fallthrough, schema, and repeated-request isolation have exact upstream/client assertions. | integration/package batch PASS |

Tenth batch command:

```bash
go test ./t/plugin -run 'TestPluginIntegration/(cas-auth|dingtalk-auth|forward-auth|graphql-limit-count|graphql-proxy-cache|ip-restriction|oas-validator|opa|opentelemetry|saml-auth|skywalking|traffic-label)(/|$)' -count=1
```

Result: PASS in 43.895s. The initial package command named the nonexistent
`pkg/plugin/opentelemetry` directory; all other packages passed, and the
corrected `go test ./pkg/plugin/otel -count=1` passed. F-022 remains an
unasserted removal lifecycle.

### Task 7 audit checkpoint

The converted-manifest semantic audit is complete at the locked reviewed HEAD
and upstream pin. Programmatic reconciliation found exactly 99 manifest files
(excluding `corpus_scope.yaml`), 99 unique owner rows, 77 `PASS` results, and
22 findings numbered continuously from F-001 through F-022. Every recorded
manifest blob matches the current file. All ten focused integration batches
passed; the reviewed production and manifest files were not changed during the
audit.

The 22 recorded problems group into four evidence-backed classes:

| Class | Findings | Count | Missing invariant |
|---|---|---:|---|
| Encryption write/read/persistence lifecycle | F-003, F-006 through F-014 | 10 | plaintext input, decrypted control-plane readback, and encrypted-at-rest state are not all asserted |
| Stored-config non-mutation | F-005, F-020, F-021 | 3 | request/response processing can dirty the persisted plugin config without failing the manifest |
| Registration/update/removal lifecycle | F-001, F-018, F-019, F-022 | 4 | an external registration or a live resource transition is replaced by a static control endpoint, disabled state, or final snapshot |
| Weakened behavior boundary | F-002, F-004, F-015, F-016, F-017 | 5 | buffering logs, module/phase execution, UUIDv7 concurrency/time faults, logger records, or endpoint distribution are not directly observed |

This checkpoint records problems only. No finding has been repaired, no
manifest or production path has been modified, and no repair plan is included
in this audit artifact.

### Task 7 remediation acceptance

The checkpoint above remains the immutable result for reviewed HEAD
`df129144d69d889dcbdb65f6e40c3b9554b380c8`. The subsequent Task 7 repair
changed only test manifests, two focused package-test files, this audit, and
the repair plan. No production implementation or general harness path changed.

The Go-native evidence boundary is deliberate. NGINX temporary-file log
counts, Lua module errors, and OpenResty body-filter phase callbacks are not
portable APISIX Go contracts. Their source cases are represented by the
observable Go behavior they guard, while injectable implementation branches
are covered in focused package tests.

| Finding | Accepted repair evidence | Result |
|---|---|---|
| F-001 | `server-info.yaml` retains the real control API check; `pkg/etcd/server_info_test.go` now asserts the prefixed key, payload, one reused lease, keepalive, and the configured 60-second Grant TTL. | RESOLVED |
| F-002 | `proxy-control.yaml` sends the source-sized 51,200-byte body through both modes; `pkg/route/proxy_control_test.go` proves enabled buffering creates a replayable body and disabled buffering leaves the streaming body unread. | RESOLVED |
| F-003 | `kafka-proxy.yaml` starts with plaintext and asserts exact encrypted bbolt state with plaintext forbidden; `pkg/plugin/kafka_proxy` proves the encrypted password resolves into the SASL request context. | RESOLVED |
| F-004 | `example-plugin.yaml` uses separate default and override fixtures, requires zero default-upstream requests, and returns only the configured override response. Lua load/filter text and body-filter callbacks are non-portable; Go plugin registration/filtering and handler behavior remain package-tested. | RESOLVED |
| F-005 | `echo.yaml` asserts the original header and `before_body` values in the persisted route after response handling. | RESOLVED |
| F-006 | Both AWS API-key and IAM variants start plaintext, retain outbound authorization/signing checks, and assert exact encrypted bbolt fields with plaintext forbidden. | RESOLVED |
| F-007 | `csrf.yaml` starts with plaintext `userkey`, exercises runtime cookie behavior, and asserts exact encrypted bbolt state with plaintext forbidden. | RESOLVED |
| F-008 | `openwhisk.yaml` starts with the source service token, proves plaintext Authorization at the provider boundary, and asserts exact encrypted bbolt state. | RESOLVED |
| F-009 | `authz-casdoor.yaml` starts with the source client secret, proves plaintext token-exchange use, and asserts exact encrypted bbolt state. | RESOLVED |
| F-010 | `authz-keycloak.yaml` starts with the source client secret, proves plaintext password-grant use, and asserts exact encrypted bbolt state. | RESOLVED |
| F-011 | `feishu-auth.yaml` writes plaintext current/fallback secrets across rotation, proves the old session through the fallback, and asserts exact final bbolt ciphertexts with both plaintext secrets forbidden. | RESOLVED |
| F-012 | `jwe-decrypt.yaml` writes plaintext consumer key/secret, proves request-time decryption, and asserts both exact encrypted consumer fields in bbolt. | RESOLVED |
| F-013 | `rocketmq-logger.yaml` retains authenticated protocol delivery from plaintext `secret_key` and now asserts exact encrypted route persistence with plaintext forbidden. | RESOLVED |
| F-014 | `splunk-hec-logging.yaml` starts with the source token, proves plaintext HEC Authorization, and asserts exact encrypted bbolt state. | RESOLVED |
| F-015 | `request-id.yaml` adds a 180-request concurrent real-process batch while retaining sequential UUIDv7 uniqueness and monotonicity; `pkg/plugin/request_id` proves 180 concurrent uniqueness, sequence-overflow time refresh, and backwards-clock ordering with injected time. | RESOLVED |
| F-016 | `data-mask.yaml` writes the real file-logger request line and asserts the masked URI while forbidding both original query secrets. | RESOLVED |
| F-017 | `pkg/plugin/clickhouse_logger/plugin_test.go` injects deterministic endpoint selection and proves `SendBatch` reaches each configured endpoint; the manifest retains real multi-endpoint delivery without a probabilistic distribution assertion. | RESOLVED |
| F-018 | `ua-restriction.yaml` denies on one route ID, reloads that same ID with no plugin, waits through `config_probe`, and then proves the same user agent is allowed. | RESOLVED |
| F-019 | `consumer-restriction.yaml` now tests both live lifecycles: same-route plugin removal and same-consumer route whitelist-to-blacklist replacement with signed readiness probes. | RESOLVED |
| F-020 | `proxy-rewrite.yaml` asserts the original URI and header values remain in the persisted route after request rewriting. | RESOLVED |
| F-021 | `response-rewrite.yaml` asserts the original body and header configuration remain in the persisted route after response rewriting. | RESOLVED |
| F-022 | `ip-restriction.yaml` denies on one route ID, reloads that same ID without `ip-restriction`, waits through `config_probe`, and then proves the same forwarded IP is allowed. | RESOLVED |

Final acceptance on the repaired working tree:

```text
go test ./t/plugin -run TestPluginIntegration -count=1  PASS (467.676s)
go test ./... -count=1                                PASS (t/plugin 482.689s)
make lint                                             PASS (golangci-lint v2.12.2, 0 issues)
make build                                            PASS
git diff --check                                      PASS
```

Task 7 therefore closes all 22 findings. The converted corpus is 99/99 owner
manifests with no unresolved Task 7 semantic finding. This does not change the
separate Task 5/6 decision: 957 plugin-owned blocks and 50 non-plugin/framework
blocks remain intentionally outside the current supported goal. Their scope is
recorded separately in `docs/testing/task5-task6-deferred-scope.md`.
