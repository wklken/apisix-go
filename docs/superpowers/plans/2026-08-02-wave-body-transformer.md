# Child Plan: body-transformer Missing Corpus Sources

> Owner row of `docs/superpowers/plans/2026-08-02-full-test-nginx-corpus-coverage.md` Task 4.
> Sources: `t/plugin/body-transformer-multipart.t` (4 blocks), `t/plugin/body-transformer2.t` (3 blocks).

## Source Contract Extraction

### body-transformer-multipart.t

| TEST | Title | Contract |
|---|---|---|
| 1 | multipart request body to json request body conversion | Route `/echo` with `body-transformer.request.template = {"foo":"{{name .. " world"}}","bar":{{age+10}}}`. POST multipart/related body with fields `name=Larry`, `age=10`. Upstream must receive JSON `{"foo":"Larry world","bar":20}`. |
| 2 | multipart response body to json response body conversion | Route `/hello` with `proxy-rewrite.uri=/demo` + `body-transformer.response.template` same as test 1. Upstream fixture responds `Content-Type: multipart/related` with fields `name=Larry`, `age=10`. Client must receive `{"foo":"Larry world","bar":20}`. |
| 3 | multipart parse result accessible to template renderer | Template (base64) uses `{% if tonumber(context.age) > 18 then %}` + `context._multipart:set_simple("status", ...)` + `context._multipart:tostring()` raw output. **Lua object helper; not supported in Go.** |
| 4 | multipart parse response accessible to template renderer (test with age == 19) | Same template as test 3 with `age=19`; expects `major`. **Lua object helper; not supported in Go.** |

### body-transformer2.t

| TEST | Title | Contract |
|---|---|---|
| 1 | body transformer with decoded body (keyword: context) | Template executes arbitrary Lua: `context.name = "bar"; context.address = nil; context.age = context.age + 1; local body = core.json.encode(context)`. **Arbitrary Lua; not supported in Go.** |
| 2 | verify the transformed body | POST `/echo` with `{"name": "foo", "address":"LA", "age": 18}`; expects body `{"name": "bar", "age": 19}`. Consumes test 1's route/template. |
| 3 | body transformer plugin with key-auth that fails | Route `/foobar` with body-transformer `request.template=some-template` + key-auth. POST without key → 401 `Unauthorized`. Body-transformer + key-auth coexist. |

## Disposition Plan

- `converted` after evidence: multipart tests 1-2, body-transformer2 test 3 (3 blocks).
- `blocked_design` (native Lua): multipart tests 3-4 (`_multipart` object helpers), body-transformer2 tests 1-2 (arbitrary Lua execution). Reason per `docs/plugins.md`: "exact multipart helper semantics remain deferred", "Loops/arbitrary Lua ... remain deferred".

## Steps

1. Add cases to `t/plugin/body-transformer.yaml` with `source.file` and `source.tests`:
   - `multipart-request-to-json` (multipart.t [1])
   - `multipart-response-to-json` (multipart.t [2])
   - `key-auth-failure-preserves-body-transformer-config` (body-transformer2.t [3])
2. Run focused integration RED:
   ```bash
   source .envrc
   go test ./t/plugin -run 'TestPluginIntegration/body-transformer/(multipart-request-to-json|multipart-response-to-json|key-auth-failure-preserves-body-transformer-config)$' -count=1 -v
   ```
3. If a mismatch is real, add focused package RED in `pkg/plugin/body_transformer` for the same cause; fix minimally; GREEN.
4. Run `go test ./pkg/plugin/body_transformer -count=1` and the integration cases GREEN.
5. Update ledger labels for the 3 converted blocks to `converted` with `manifest: body-transformer.yaml`; keep multipart 3-4 and body-transformer2 1-2 `blocked_design` with reason.
6. Record command evidence in `docs/testing/apisix-test-nginx-corpus-audit.md`.
