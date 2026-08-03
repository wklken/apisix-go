# Child Plan: chaitin-waf Missing Corpus Sources

> Owner row of `docs/superpowers/plans/2026-08-02-full-test-nginx-corpus-coverage.md` Task 4.
> Sources: `t/plugin/chaitin-waf-reject.t` (4 blocks), `t/plugin/chaitin-waf-timeout.t` (2 blocks).

## Source Contract Extraction

### chaitin-waf-reject.t

Upstream harness preprocessor: stream server on 8088/8089 runs `lib.chaitin_waf_server.reject()`; `apisix.stream_proxy` is enabled. Requests go to `/do` (not `/t`).

| TEST | Title | Contract |
|---|---|---|
| 1 | set route | Plugin metadata PUT: `{"mode": "block", "nodes": [{host 127.0.0.1, port 8088}, {host 127.0.0.1, port 8089}]}`. Route `/*` GET with `chaitin-waf.upstream.servers=[httpbun.org]`, upstream 127.0.0.1:1980. Response `passed`. |
| 2 | pass | GET /hello → 403 with body `{"code": 403, "success":false, "message": "blocked by Chaitin SafeLine Web Application Firewall", "event_id": "b3c6ce574dc24f09a01f634a39dca83b"}`; headers `X-APISIX-CHAITIN-WAF: yes`, `X-APISIX-CHAITIN-WAF-STATUS: 403`, `X-APISIX-CHAITIN-WAF-ACTION: reject`, `X-APISIX-CHAITIN-WAF-TIME` numeric. |
| 3 | plugin mode monitor prepare | Metadata PUT without mode (defaults monitor): single node port 8089. Route `/*` with `chaitin-waf.mode=monitor`, `match=[{vars: [[http_waf, ==, true]]}]`. Response `passed`. |
| 4 | plugin mode monitor | GET /hello with headers `waf: true`, `trigger: block` → 200, upstream body `hello world`; headers `X-APISIX-CHAITIN-WAF: yes`, `X-APISIX-CHAITIN-WAF-STATUS: 403`, `X-APISIX-CHAITIN-WAF-ACTION: reject` (monitor mode passes through). |

### chaitin-waf-timeout.t

Upstream harness: stream server runs `lib.chaitin_waf_server.timeout()`.

| TEST | Title | Contract |
|---|---|---|
| 1 | set route | Same route/metadata shape as reject test 1 (two nodes 8088/8089). Response `passed`. |
| 2 | timeout | GET /hello → 200 `hello world`; header `X-APISIX-CHAITIN-WAF: timeout`. |

## Disposition Plan

All 6 blocks `converted` after evidence. The Go plugin (`pkg/plugin/chaitin_waf/plugin.go`) already implements reject decision bodies, monitor mode, match vars, and the `timeout` header on read timeout.

## Steps

1. Add cases to `t/plugin/chaitin-waf.yaml`:
   - `block-mode-reject` (chaitin-waf-reject.t [1, 2]) — metadata mode block; WAF fixture replies `status: 403, body: {"status":403,"event_id":"b3c6ce574dc24f09a01f634a39dca83b"}`; assert 403 body + 4 headers.
   - `monitor-mode-reject-passes-through` (chaitin-waf-reject.t [3, 4]) — monitor mode + `match`; request with `Waf: true` header; fixture replies 403 decision; assert 200 + headers.
   - `read-timeout-falls-back` (chaitin-waf-timeout.t [1, 2]) — fixture uses `delay` longer than the configured `read_timeout` (or closes); assert 200 + `X-APISIX-CHAITIN-WAF: timeout`.
2. Existing manifest already has a `waf` HTTP fixture pattern; reuse it.
3. Run focused integration RED:
   ```bash
   source .envrc
   go test ./t/plugin -run 'TestPluginIntegration/chaitin-waf/(block-mode-reject|monitor-mode-reject-passes-through|read-timeout-falls-back)$' -count=1 -v
   ```
4. Focused package RED only if the integration mismatch reproduces in `pkg/plugin/chaitin_waf`; fix minimally.
5. Run `go test ./pkg/plugin/chaitin_waf -count=1` + integration GREEN + `TestUpstreamCorpusAccounting`.
6. Update ledger: mark reject 1-4 and timeout 1-2 `converted`, `manifest: chaitin-waf.yaml`; record evidence in the audit doc.
