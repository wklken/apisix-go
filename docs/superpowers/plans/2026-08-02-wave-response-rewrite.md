# Child Plan: response-rewrite Missing Corpus Sources

> Owner row of `docs/superpowers/plans/2026-08-02-full-test-nginx-corpus-coverage.md` Task 4.
> Sources: `t/plugin/response-rewrite2.t` (25 blocks), `t/plugin/response-rewrite3.t` (22 blocks).

## Source Contract Extraction

### response-rewrite2.t (filter schema, filters, header add/set/remove)

| TEST | Title | Contract |
|---|---|---|
| 1 | sanity | `check_schema` on 7 filter/body shapes: valid `{body}`, valid `{filters:[...]}`; invalid `{body+filters}` (dependent schema), empty filters array, missing `replace`, empty `regex`, invalid `scope`. |
| 2 | add plugin with valid filters | `filters=[{regex=Hello, scope=global, replace=World, options=jo}]` schema OK → `done`. |
| 3 | invalid filter required field | missing `replace` → `property "filters" validation failed: failed to validate item 1: property "replace" is required`. |
| 4 | invalid filter scope | `scope="two"` → scope enum failure. |
| 5 | invalid filter empty value | `regex=""` → string too short failure. |
| 6 | invalid filter regex options | `options="h"` → `regex "hello" validation failed: unknown flag "h" (flags "h")`. |
| 7, 8 | filters with vars | Route `/hello`, `vars=[[status,==,200]]`, `filters=[{regex: hello, replace: test}]`; upstream body `hello world` → response `test world`. |
| 9, 10 | filter substitute global | `filters=[{regex: l, replace: t, scope: global}]` → `hello world` becomes `hetto wortd`. |
| 11, 12 | filter replace with empty | `regex: hello, replace: ""` → `hello world` becomes ` world`. |
| 13, 14 | filter replace with words | `regex: \w\S+$, replace: *` → `hello world` becomes `hello *`. |
| 15, 16 | set multiple filters | Two filters (hello→HELLO, L→T) → `HETLO world`. |
| 17, 18 | filters no any match | regex `test` → unchanged `hello world`. |
| 19 | schema check for headers | Invalid header add/remove/set shapes → `object matches none of the required`. |
| 20, 21 | add headers | `headers.add=["Cache-Control: no-cache", "Cache-Control : max-age=0, must-revalidate"]` → response header `Cache-Control: no-cache, max-age=0, must-revalidate`. |
| 22, 23 | set headers | `headers.add=[Cache-Control: no-cache]` + `set.Cache-Control=max-age=0, must-revalidate` → set wins. |
| 24, 25 | remove headers | add Set-Cookie + set Cache-Control + remove both → both absent. |

### response-rewrite3.t (gzip/brotli upstreams)

Upstream harness: servers on 11451 (gzip, `gzip on; gzip_types *; gzip_min_length 1`) and 11452 (brotli) serve `hello world`.

| TEST | Title | Contract |
|---|---|---|
| 1, 2 | gzip upstream sanity | Route `/gzip_hello` no plugin; request with `Accept-Encoding: gzip` → `Content-Encoding: gzip` preserved. |
| 3, 4 | gzip + body conf | Route with `response-rewrite.body="new body\n"`; request gzip → body `new body`, `Content-Encoding` cleared. |
| 5, 6 | gzip + filter conf | `filters=[{regex: hello, replace: test}]`; gzip request → body `test world`, Content-Encoding cleared (gzip decode before filters). |
| 7, 8 | body conf + unsupported encoding | `Content-Encoding: deflate` mock upstream body; body conf rewrites body and clears header regardless. |
| 9, 10 | filter conf + unsupported encoding | `Content-Encoding: deflate`; log `filters may not work as expected due to unsupported compression encoding type: deflate`; body passthrough with header cleared. |
| 11, 12 | headers only conf + gzip | `headers.set.X-Server-id=3, X-Server-status=on, Content-Type=""`; gzip preserved (`Content-Encoding: gzip`) + new headers set + Content-Type empty. |
| 13, 14 | headers only conf + deflate | Same but `Content-Encoding: deflate` preserved. |
| 15, 16 | brotli upstream sanity | Route `/brotli_hello`; `Accept-Encoding: br` → `Content-Encoding: br`. |
| 17, 18 | brotli + body conf | body rewrite `new body`, header cleared. |
| 19, 20 | brotli + filter conf | `hello world hello world hello world` → `test world hello world hello world`. |
| 21, 22 | headers only conf + brotli | br preserved + headers set. |

## Disposition Plan

All 47 blocks `converted` after evidence. Go `response_rewrite` already implements filter schema validation, `vars`, filters with global scope, header add/set/remove with array string form, and gzip/brotli decode (`decodeFilterBody`).

## Steps

1. Extend `t/plugin/response-rewrite.yaml` to a multi-source form adding `response-rewrite2.t` and `response-rewrite3.t`, or add the cases to the existing manifest's `sources:`.
2. Schema cases (2-6, 19): reuse the existing config-rejection pattern (route stays 404 + log match). Test 1's multi-shape sanity can be one case with 404 + exact log.
3. Filter cases (7-18): upstream fixture responds `hello world` (or `hello world hello world hello world`); assert transformed body. Vars `[[status,==,200]]` activate on 200.
4. Header cases (20-25): fixture returns 200 with a `Set-Cookie`/`Cache-Control` base; assert add/set/remove.
5. gzip/brotli cases: fixture must serve real compressed bodies. Use `!!binary` YAML bodies (pre-compressed) with `Content-Encoding: gzip`/`br` headers — the harness fixture already supports binary bodies (see `brotli.yaml` `gzip-upstream-is-not-recompressed`). For the deflate mock cases, use plain text with `Content-Encoding: deflate`.
6. Run focused integration RED:
   ```bash
   source .envrc
   go test ./t/plugin -run 'TestPluginIntegration/response-rewrite/(filter-schema-sanity|filter-valid-schema|filter-missing-replace|filter-invalid-scope|filter-empty-regex|filter-invalid-options|filters-vars-match|filter-global-substitute|filter-replace-empty|filter-replace-words|multiple-filters|filter-no-match|headers-schema|headers-add|headers-set|headers-remove|gzip-upstream-preserved|gzip-body-rewrite|gzip-filter-decode|deflate-body-rewrite|deflate-filter-warning|gzip-headers-only|deflate-headers-only|brotli-upstream-preserved|brotli-body-rewrite|brotli-filter-decode|brotli-headers-only)$' -count=1 -v
   ```
7. Focused package RED in `pkg/plugin/response_rewrite` only for confirmed defects (e.g. header `add` string-array parsing, `\w\S+$` regex, options `jo` handling).
8. Run `go test ./pkg/plugin/response_rewrite -count=1` + integration GREEN.
9. Update ledger; record evidence.
