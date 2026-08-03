# Child Plan: proxy-rewrite Missing Corpus Sources

> Owner row of `docs/superpowers/plans/2026-08-02-full-test-nginx-corpus-coverage.md` Task 4.
> Sources: `t/plugin/proxy-rewrite2.t` (8 blocks), `t/plugin/proxy-rewrite3.t` (43 blocks).

## Source Contract Extraction

### proxy-rewrite2.t (X-Forwarded-* passthrough/customization; data-plane YAML provider)

| TEST | Title | Contract |
|---|---|---|
| 1 | access $upstream_uri before proxy-rewrite | `serverless-pre-function` (Lua) logs `upstream_uri` at rewrite; proxy-rewrite `uri=/plugin_proxy_rewrite`; error log `serverless []`; upstream sees `/plugin_proxy_rewrite`. **Uses Lua serverless; assertion is the log. Native boundary for the log part; the proxy-rewrite URI rewrite itself is convertible.** |
| 2 | default X-Forwarded-Proto | Plain route; GET /echo; upstream response header `X-Forwarded-Proto: http`. |
| 3 | pass X-Forwarded-Proto | Request header `X-Forwarded-Proto: https`; upstream sees `https`. |
| 4 | customize X-Forwarded-Proto | proxy-rewrite `headers.X-Forwarded-Proto=https`; request `grpc`; upstream sees `https`. |
| 5 | make sure X-Forwarded-Proto hit the core.request.header cache | serverless Lua + proxy-rewrite header; **Lua-native log assertion**. |
| 6 | pass duplicate X-Forwarded-Proto | Request headers `X-Forwarded-Proto: http` + `grpc`; upstream sees `http, grpc`. |
| 7 | customize X-Forwarded-Port | proxy-rewrite `headers.X-Forwarded-Port=10080`; request `8080`; upstream sees `10080`. |
| 8 | pass duplicate X-Forwarded-Proto, but not configured trusted_addresses | No trusted addresses; request two values; upstream sees only `http` (first value). |

### proxy-rewrite3.t (method/host/headers/regex_uri/CRLF safety)

| TEST | Title | Contract |
|---|---|---|
| 1, 2 | set route(rewrite method) / hit route | Route /hello → `uri=/plugin_proxy_rewrite, method=POST, host=apisix.iresty.com`; GET /hello; upstream sees method POST. Log assertion is a fixture echo; assert via upstream fixture request capture instead. |
| 3, 4 | update rewrite method to GET | Same route with method GET; upstream sees GET. |
| 5 | wrong value of method key | `check_schema({method='GET1'})` fails with `property "method" validation failed: matches none of the enum values`. Schema-rejection case. |
| 6, 7 | rewrite method with headers | Route with `method=POST` + `headers.x-api-version=v1`; GET /hello; upstream sees POST and `x-api-version: v1`. |
| 8-11 | use_real_request_uri_unsafe | `use_real_request_uri_unsafe: true`: request `GET /print%5Furi%5Fdetailed` → upstream `request_uri` keeps `%5F` encoding. false (default): normalized to `/print_uri_detailed`. Upstream fixture echoes `ngx.var.uri` / `ngx.var.request_uri` equivalent (request path). |
| 12, 13 | rewrite X-Forwarded-Host | proxy-rewrite `headers.X-Forwarded-Host=test.com`; request `apisix.ai`; upstream sees `test.com`. |
| 14-18 | add/set/remove headers | `headers.add.test=123`, `set.test2=2233`, `remove=[hello]`; multi-header request `test: sssss, bbb` → upstream sees `test: sssss, bbb, 123`; remove deletes `hello`. |
| 19, 20 | header priority | add+set same name → set wins (`test_in_set`); no DEPRECATED log. |
| 21, 22 | host rewrite with CRLF-injected URI | `host=test.xxxx.com`, `uri=/hello*`; request path with `%0d%0a` encodings; upstream must see rewritten host `test.xxxx.com` and the raw encoded request URI. |
| 23, 24 | uri with $uri variable + CRLF | `uri=/$uri/remain`; upstream sees `/hello<encoded>/remain`. |
| 25, 26 | regex_uri with args | `regex_uri=[^/test/(.*)/(.*)/(.*), /$1_$2_$3?a=c]`; GET /test/plugin/proxy/rewrite → upstream `/plugin_proxy_rewrite?a=c`. |
| 27, 28 | variables in headers captured by regex_uri | `regex_uri` + `headers.add.X-Request-ID=$1/$2/$3`; upstream sees `plugin/proxy/rewrite`. |
| 29, 30 | variables in header when not matched regex_uri | Unmatched regex_uri; header vars unresolved → `X-Request-ID` absent/passthrough; request `X-Foo: Foo` preserved. |
| 31, 32 | optional capture groups | `^/test/(not_matched)?.*` with `$1/$2` → `test1///test2`. |
| 33, 34 | set X-Forwarded-Port before proxy | proxy-rewrite `headers.X-Forwarded-Port=9882`; request `9881`; upstream sees `9882`. |
| 35, 36 | set X-Forwarded-For before proxy | proxy-rewrite `headers.X-Forwarded-For=22.22.22.22`; request `11.11.11.11`; upstream sees `22.22.22.22, 127.0.0.1` (remote addr appended). |
| 37-39 | multiple regex_uris | Two regex pairs; `/test/a/b/c/hello` → `/hello/a_b_c`; `/world` → `/world/a_b_c`. |
| 40, 41 | regex uri with unsafe allowed | `regex_uri=[/hello/(.+), /hello?unsafe_variable=$1]` + `use_real_request_uri_unsafe: true`; GET `/hello/%ED%85%8C...` → upstream `/hello?unsafe_variable=%ED%85%8C...` (encoded preserved). |
| 42, 43 | unsafe uri normalized (unsafe not allowed) | `use_real_request_uri_unsafe: false`; GET `/print%5Furi%5Fdetailed` → normalized `/print_uri_detailed`. |

## Disposition Plan

- proxy-rewrite2 tests 2, 3, 4, 6, 7, 8 → `converted` (6 blocks). Tests 1, 5 use serverless-pre-function Lua log assertions → `blocked_runtime` (native Lua) unless an existing Go serverless compatibility path covers them.
- proxy-rewrite3 tests 1-43 → `converted` after evidence. Test 5 is schema-rejection (existing pattern in manifest). Tests 2/4/7 upstream method assertions map to fixture request capture.

## Steps

1. Extend `t/plugin/proxy-rewrite.yaml` (which currently covers `proxy-rewrite.t` tests 1-57) with a `sources:` multi-source form adding `proxy-rewrite2.t` and `proxy-rewrite3.t`, or create supplemental `proxy-rewrite2.yaml`-style cases inside the same manifest. Use behavior names; never generic `source-N`.
2. Build the X-Forwarded-* cases: upstream fixture asserts request headers. Note Go proxy header pipeline: verify whether Go sets default `X-Forwarded-Proto` when absent (may need focused package work; RED first).
3. Build method/host/header/regex_uri cases against the fixture.
4. Build CRLF cases: the harness client sends paths with `%0d%0a` as-is (net/http encodes `%`); the upstream fixture must see the encoded request URI.
5. Run focused integration RED per case family:
   ```bash
   source .envrc
   go test ./t/plugin -run 'TestPluginIntegration/proxy-rewrite/(forwarded-proto-default|forwarded-proto-pass|forwarded-proto-customize|forwarded-proto-duplicate|forwarded-proto-untrusted|forwarded-port-customize|method-rewrite|method-update|schema-rejects-method|method-with-headers|unsafe-uri-preserved|unsafe-uri-normalized|forwarded-host-rewrite|add-set-remove-headers|header-priority|host-rewrite-crlf|uri-variable-crlf|regex-uri-args|capture-header-vars|unmatched-capture|optional-capture|forwarded-port-set|forwarded-for-set|multi-regex-uri|regex-unsafe-variable)$' -count=1 -v
   ```
6. Focused package RED in `pkg/plugin/proxy_rewrite` only for confirmed mismatches.
7. Run `go test ./pkg/plugin/proxy_rewrite -count=1` + integration GREEN + `TestUpstreamCorpusAccounting`.
8. Update ledger for converted blocks; record evidence.
