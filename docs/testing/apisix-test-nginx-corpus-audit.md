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
