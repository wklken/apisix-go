# Bug Scan Verification Report

> Date: 2026-08-09
>
> Source report: `docs/bug-scan-report-2026-08-09.md`
>
> Verified revision: `7e48af1ace9de1871f73b735bd65eb41b80fc7c5`
>
> Report baseline: `9fc12ea`

## Method

Each row was checked against the current production source, callers, existing tests, and the APISIX 3.17 contract when the claim depended on upstream semantics. Scanner text was treated as candidate inventory, not proof. A row is:

- **Confirmed** when the current code still has the stated failure and the proposed direction preserves the project contract.
- **Partial** when only part of a compound row is correct or the failure exists under a narrower contract.
- **Rejected** when current code already handles it, APISIX 3.17 intentionally has the stated behavior, or the proposed remedy would manufacture a false success.
- **Unverifiable** when the report does not identify concrete code or behavior.

The report contains 41 rows: 27 confirmed, 5 partial, 8 rejected, and 1 unverifiable. No production code was changed during verification.

## High Risk

| ID | Status | Verification |
|---|---|---|
| H01 request-id pooled alias | **Confirmed** | `rangeID` returns `util.BytesToString(id)` after deferring `bytePool.Put(id)`; the unsafe string aliases a buffer already returned to the pool. |
| H02 MQTT absolute deadlines | **Confirmed** | CONNECT preread sets a client read deadline and replay sets an upstream write deadline; neither is cleared before the tunnel copy starts. |
| H03 chaitin-waf body truncation | **Confirmed** | `askWAF` replaces `r.Body` with the bounded inspection copy and leaves the original `ContentLength`; the existing data-integrity plan already covers the correct full-body replay design. |
| H04 ACL all-label claim | **Rejected** | APISIX 3.17 `contains_label` returns on the first matching configured label for both allow and deny paths. The local any-label behavior is parity, not an authorization bypass. |
| H05 OpenID default audience/issuer claim | **Rejected** | APISIX documents audience matching as opt-in (`match_with_client_id: false`) and issuer validation as skipped when neither configured issuers nor discovery issuer is available. Local behavior matches that contract; operators can enable the existing validators. |
| H06 traffic-split request replacement | **Confirmed** | `WithHealthReporter` clones the request, but `applyTrafficSplitTarget` returns only `bool`. URL pointer mutations survive accidentally; scalar `Host` and cloned context do not. |
| H07 proxy-cache HEAD poisoning | **Confirmed** | GET and HEAD intentionally share the cache key, while a HEAD miss can store an empty body with a non-zero length. The existing cache-correctness plan already covers making HEAD misses non-mutating. |

## Medium Risk

| ID | Status | Verification |
|---|---|---|
| M01 synthetic SSE terminal on failure | **Rejected** | An upstream disconnect or byte-limit error is an incomplete stream. Appending a normal `[DONE]` or `message_stop` would falsely report success; no cross-protocol error-frame contract is specified. |
| M02 AI whole-request timeout | **Confirmed** | Both AI proxy clients use `http.Client.Timeout`, which includes response-body streaming and terminates healthy streams after the configured total duration. |
| M03 ai-proxy-multi health lifecycle race | **Confirmed** | lazy start writes lifecycle channels under `healthStart`, while `Stop` reads them under a different once/atomic boundary; stop-before-start can return before the goroutine is published. |
| M04 ai-request-rewrite Anthropic endpoint | **Confirmed** | `preferredProtocol` selects OpenAI Chat for Anthropic, producing an OpenAI body and `/v1/chat/completions` under the Anthropic host instead of Anthropic Messages `/v1/messages`. |
| M05 basic-auth diagnostic secrets | **Confirmed** | malformed Basic errors include the raw token or decoded credential and flow into multi-auth diagnostics/logging. |
| M06 authentication cookies | **Partial** | DingTalk, Feishu, and Casdoor manual cookies lack `Secure`/`SameSite`. SAML already derives `Secure` and `SameSite` for all session and deletion cookies, so the SAML claim is stale. |
| M07 local fixed window | **Confirmed** | each increment replaces `resetAt` with `now+period` and refreshes TTL, producing a sliding expiration instead of a fixed window. The report's exact 150-second example is not generally derivable, but the defect is real. |
| M08 real-ip and `$remote_addr` | **Confirmed** | real-ip writes only the APISIX context value; the Nginx-variable resolver still reads `r.RemoteAddr`, so downstream limit plugins use the proxy address. |
| M09 sliding delayed rejection as 500 | **Rejected** | current `runSlidingLimit` explicitly recognizes `errSlidingWindowRejected` and returns configured quota headers and `rejected_code`. |
| M10 gRPC-Web trailers-only response | **Rejected** | the gRPC-Web protocol explicitly permits trailers-only responses to carry trailers with the response headers and no body message. |
| M11 response-rewrite unsupported encoding | **Confirmed** | unsupported/invalid encoded bodies remain byte-for-byte unchanged while `Content-Encoding` is deleted. This follows an existing upstream-parity test but corrupts HTTP representation metadata; the remediation plan records the intentional parity divergence. |
| M12 gzip quality values | **Confirmed** | the handler gates on substring presence, so `gzip;q=0` still enters compression and selects gzip. |
| M13 CORS OPTIONS/wildcard claim | **Rejected** | APISIX 3.17 short-circuits every OPTIONS request and emits wildcard CORS headers for wildcard configuration; `Vary: Origin` is not required when the representation is invariant at `*`. |
| M14 brotli buffering/length | **Confirmed** | the body is fully buffered before `max_response_size` is checked, and the final writer suppresses `Content-Length` even on uncompressed pass-through. |
| M15 Elasticsearch bulk item errors | **Confirmed** | HTTP 200 is treated as success without decoding the bulk response `errors` flag or per-item status. |
| M16 error-log/SLS blocked writes | **Confirmed** | both transports bound dialing but not writing; their batch defaults permit an unbounded pending queue, and shutdown waits for the blocked sender. |
| M17 route priority | **Confirmed** | `resource.Route.Priority` is decoded but never used by route construction; matching is determined by registration order. |
| M18 bad route and duplicate parameter | **Partial** | fail-closed initial construction and last-known-good reload are now deliberate. Duplicate `:id` parameters still convert to duplicate chi parameter names and can panic during initial registration. |

## Low Risk

| ID | Status | Verification |
|---|---|---|
| L01 runtime log variables | **Partial** | registered `$balancer_*` and `$upstream_*` values can be hidden by static whitelists. `$matched_host` is not currently registered, so that example cannot be restored by a resolver change alone. |
| L02 request-body error cache | **Confirmed** | `readRequestBody` caches the partial bytes even when reading or closing fails; later readers receive the bytes with no error. |
| L03 slash-bearing resource IDs | **Partial** | slash IDs are outside APISIX's documented ID syntax, so preserving them is not required. Standalone accepts them and silently emits an ambiguous store key; the fix is validation, not slash-ID support. |
| L04 nil request-variable map | **Confirmed** | `RegisterRequestVar` writes without the nil guard already present in `RegisterApisixVar`. |
| L05 standalone watcher shutdown | **Confirmed** | watcher sends have no cancellation path, and the server never stops the fsnotify goroutine before the store consumer stops. |
| L06 delayed-sync double commit | **Confirmed** | ticker flush snapshots a delta outside the lock; an expired-window inline flush can commit the same delta first, after which the ticker commits it again and encounters the unhandled `localDelta < mutation.delta` state. |
| L07 workflow duplicate limiter | **Rejected** | workflow actions own independent limiter configuration and intentionally invoke their handler before the normal chain. Neither local nor APISIX 3.17 has the claimed generic skip mechanism; configuring both creates two policies. |
| L08 rule headers ignore metadata | **Rejected** | APISIX 3.17 intentionally derives rule headers from `header_prefix` or the 1-based rule index; plugin metadata applies only to the non-rule path. |
| L09 data-mask empty body/body close | **Confirmed** | JSON rules parse an empty body as invalid JSON, and the local body reader replaces the body without closing the original reader. |
| L10 request-validation empty body | **Confirmed** | empty content is rejected by JSON parsing before the configured schema can decide whether a missing value is valid. |
| L11 batch-requests panic/timeout wait | **Confirmed** | `httptest.NewRequest` can panic for an invalid request target, and the timeout path waits for the worker even when it ignores cancellation. |
| L12 TCP/syslog partial writes | **Partial** | TCP retry duplication is consistent with at-least-once delivery and cannot be eliminated without acknowledgement/deduplication. Syslog retaining only an unwritten suffix across connections can create malformed frames and is actionable. |
| L13 ai-rag HTTP timeout | **Confirmed** | the plugin client has a transport but no request timeout; ordinary server requests can wait indefinitely. |
| L14 proxy-cache stampede | **Confirmed** | concurrent misses have no per-key coordination; all execute upstream and race to store the same representation. |
| L15 response-writer capabilities | **Confirmed** | Zipkin and Lago wrappers do not expose `Unwrap`, so `ResponseController` cannot reach flush/hijack capabilities of the underlying writer. |
| L16 opencode catch-all | **Unverifiable** | the row supplies neither concrete paths nor the referenced raw reports. Its examples require separate, identified findings before verification or planning. |

## Planning Disposition

- Reuse `docs/superpowers/plans/2026-08-08-data-integrity-and-cache-correctness.md` for H03 and H07.
- Extend the MQTT work from `docs/superpowers/plans/2026-08-08-runtime-security-and-lifecycle.md` so both client-read and upstream-write deadlines are cleared.
- Implement the remaining confirmed scopes through `docs/superpowers/plans/2026-08-09-bug-scan-remediation-suite.md` and its child plans.
- Do not implement rejected rows. M01 and the TCP half of L12 need a new, explicit delivery/protocol contract before code changes.

## Remediation Ledger (2026-08-09)

Remediated on branch `codex/backlog-bug-scan` from base `7e48af1`. Each row records the commit and the focused regression/gate command that failed before and passed after. All owning package gates and `make lint` / `make build` pass. The plan-amendment rows note intentional deviations approved by the plan owner.

| ID | Commit | Focused command |
|---|---|---|
| H01 | `14b788b` fix(request-id): own pooled range id bytes | `go test -race ./pkg/plugin/request_id -count=1` |
| H02 | `2b86093` fix(mqtt-proxy): clear CONNECT phase deadlines | `go test ./pkg/plugin/mqtt_proxy -count=1` |
| H03 | `4eeec9e` fix(chaitin-waf): preserve full request body | `go test ./pkg/plugin/chaitin_waf -count=1` |
| H06 | `0114551` fix(route): retain traffic split request state | `go test ./pkg/route -run "TrafficSplit\|Health" -count=1` |
| H07 | `5f09679` fix(proxy-cache): keep HEAD misses out of GET cache | `go test ./pkg/plugin/proxy_cache -count=1` |
| M02 | `54c48c5` fix(ai-proxy): use progress timeouts for streams | `go test ./pkg/plugin/ai_proxy ./pkg/proxy -count=1` |
| M03 | `c383fa3` fix(ai-proxy-multi): own health loop lifecycle | `go test -race ./pkg/plugin/ai_proxy_multi -run "Health" -count=1` |
| M04 | `7260bb7` fix(ai-request-rewrite): use Anthropic messages API | `go test ./pkg/plugin/ai_protocols ./pkg/plugin/ai_request_rewrite -count=1` |
| M05 | `83ace37` fix(basic-auth): redact malformed credentials | `go test ./pkg/plugin/basic_auth ./pkg/plugin/multi_auth -count=1` |
| M06 | `4653369` fix(auth): harden manual session cookies (+`b263e06`) | `go test ./pkg/plugin/base ./pkg/plugin/dingtalk_auth ./pkg/plugin/feishu_auth ./pkg/plugin/authz_casdoor -count=1` |
| M07 | `f2eb2ff` fix(limit-count): anchor local fixed windows | `go test ./pkg/plugin/limit_count -run "LocalFixed\|Local.*Window" -count=1` |
| M08 | `2a7f042` fix(real-ip): expose trusted remote address to variables | `go test ./pkg/apisix/variable ./pkg/plugin/real_ip ./pkg/plugin/limit_count -count=1` |
| M11 | `f053b7e` fix(response-rewrite): preserve unsupported encodings | `go test ./pkg/plugin/response_rewrite -count=1` |
| M12 | `bfbbedb` fix(gzip): honor encoding quality values | `go test ./pkg/plugin/gzip -count=1` |
| M14 | `0716f97` fix(brotli): bound response buffering | `go test ./pkg/plugin/brotli -count=1` |
| M15 | `bfb3ee3` fix(elasticsearch-logger): detect bulk item failures | `go test ./pkg/plugin/elasticsearch_logger -count=1` |
| M16 | `189fa2b` + `93878d9` error-log/sls bounded delivery | `go test ./pkg/plugin/error_log_logger ./pkg/plugin/sls_logger -count=1` |
| M17 | `dccbfa8` fix(route): honor resource priority | `go test ./pkg/route -run "RoutePriority\|Wildcard\|URI" -count=1` |
| M18 | `848a85c` fix(route): reject duplicate path parameters | `go test ./pkg/route -run "URI\|RouteBuild" -count=1` |
| L01 | `83b0b3d` fix(logging): resolve registered runtime variables | `go test ./pkg/apisix/log -count=1` |
| L02 | `f6332ef` fix(ctx): retain request body read errors | `go test ./pkg/apisix/ctx -count=1` |
| L03 | `52c88b9` fix(standalone): validate ids and stop watcher | `go test -race ./pkg/config ./pkg/server -run "Standalone" -count=1` |
| L04 | `f5e21f4` fix(ctx): guard missing request variable map | `go test ./pkg/apisix/ctx -count=1` |
| L05 | `52c88b9` fix(standalone): validate ids and stop watcher | `go test -race ./pkg/config ./pkg/server -run "Standalone" -count=1` |
| L06 | `4c0291b` fix(limit-count): serialize delayed delta commits | `go test -race ./pkg/plugin/limit_count -run "DelayedSync" -count=1` |
| L09 | `f051df2` fix(data-mask): accept empty request bodies | `go test ./pkg/plugin/data_mask -count=1` |
| L10 | `faf8734` fix(request-validation): validate missing bodies by schema | `go test ./pkg/plugin/request_validation -count=1` |
| L11 | `069624d` fix(batch-requests): bound invalid and timed out items | `go test ./pkg/plugin/batch_requests -count=1` |
| L12 | `8bd7137` fix(syslog): preserve frames after partial writes | `go test ./pkg/plugin/syslog -count=1` |
| L13 | `9d5e5d1` fix(ai-rag): bound provider requests | `go test ./pkg/plugin/ai_rag -count=1` |
| L14 | `54eaa14` fix(proxy-cache): serialize concurrent fills | `go test -race ./pkg/plugin/proxy_cache -run "ConcurrentMiss\|FillLock" -count=1` |
| L15 | `1b9797f` fix(observability): preserve writer capabilities | `go test ./pkg/plugin/zipkin ./pkg/plugin/lago -count=1` |

### Plan amendments recorded during execution

- **Track 2 Task 2 test header**: the plan's second regression header `base64("alice:secret")` decodes with a colon and returns nil error, making the test impossible on both RED and GREEN. Corrected to a colon-less value (`alicesecret`) per the plan's stated "encoded and decoded secrets" intent.
- **Track 5 Task 1/2 send timeout**: the plan's literal `NewProgressTimeoutTransport(transport, timeout, timeout)` is broken — the shared transport's send path cancels the context after the request body is written, failing every body-carrying request. Approved `(0, timeout)` read-side inactivity timeout, reusing the shared transport unchanged.
- **Track 3 Task 6**: standalone ID validation exempts the `secrets` bucket, where slash-bearing IDs (`vault/test1`) are real store keys.

### Pre-existing verification failures fixed as a side effect of the gate

- `pkg/plugin/serverless/plugin.go` `isArrayTable` unused function (dead duplicate of `pkg/plugin/luautil/convert.go`): pre-existing at base `7e48af1`, removed in `241a127` so `make lint` passes.

## Primary Contract References

- [APISIX 3.17 ACL implementation](https://raw.githubusercontent.com/apache/apisix/release/3.17/apisix/plugins/acl.lua) for H04 any-label semantics.
- [APISIX OpenID Connect documentation](https://apisix.apache.org/docs/apisix/plugins/openid-connect/) for H05 opt-in audience matching and issuer fallback behavior.
- [gRPC-Web protocol](https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-WEB.md) for M10 trailers-only responses.
- [APISIX 3.17 CORS implementation](https://raw.githubusercontent.com/apache/apisix/release/3.17/apisix/plugins/cors.lua) for M13 OPTIONS and wildcard behavior.
- [APISIX 3.17 limit-count implementation](https://raw.githubusercontent.com/apache/apisix/release/3.17/apisix/plugins/limit-count/init.lua) for L08 rule header-prefix semantics.
- [APISIX Admin API documentation](https://apisix.apache.org/docs/apisix/admin-api/) for L03 ID syntax and M17 route priority.
