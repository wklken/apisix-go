# HTTP Representation Integrity Implementation Plan

> **For agentic workers:** Execute only the work unit assigned in the implementation brief. Use regression-first implementation. Do not delegate, commit, push, create a PR, or modify files outside the owned paths.

**Goal:** Close PR-008, PR-009, and P1 5.4 by making body transforms, content negotiation, and proxy-cache variants describe one consistent HTTP representation across identity, gzip, zlib-wrapped deflate, and brotli.

**Architecture:** Separate byte replacement from raw buffer mutation in `plugin/base`; coordinate gzip and brotli through one per-request negotiation state so q-values, not middleware nesting, select the representation; keep proxy-cache's existing Vary index/PURGE owner and repair only the repeated-header signature plus end-to-end producer contract tests.

**Validated base:** `origin/master` at `341082bff5d4166ffb5384819078b7902153f5de`.

## Confirmed current defects

- `echo`, `body-transformer`, and `exit-transformer` replace response bytes while leaving stale `Content-Encoding`, validators, range metadata, or digests. `response-rewrite` has a partial four-header cleanup that is not shared.
- `BufferedResponseWriter` treats the first 1xx as final, so `103` followed by `200` commits the wrong final status. It can also replay bodies for HEAD/101/204/304 through nested transform writers.
- gzip defaults `vary` to false, emits raw RFC 1951 bytes for `deflate`, and has incomplete `Accept-Encoding` parsing. Brotli only adds Vary after successful compression and uses a separate, order-sensitive parser.
- With both plugins enabled, request order is brotli(996) -> gzip(995) -> upstream and response order is upstream -> gzip -> brotli. The inner plugin wins even when the client's q-values prefer the outer plugin; `_meta.priority` can reverse the winner.
- An encoded cache variant followed by an identity response without `Vary: Accept-Encoding` correctly causes proxy-cache to delete the variant index and store an identity base entry. Later encoded requests then hit the identity entry. The cache owner is behaving correctly; compression producers must mark identity variants too.
- `cacheutil.VarySignature` uses `Header.Get`, so repeated request header lines that differ only after the first value collide.

## Global contracts

- Every actual body replacement invalidates these body-derived headers case-insensitively, including noncanonical duplicate map keys: `Content-Length`, `Content-Encoding`, `Content-Range`, `Content-MD5`, `Digest`, `Content-Digest`, `Repr-Digest`, `ETag`, `Last-Modified`.
- Do not automatically delete `Content-Type`, `Content-Language`, `Content-Location`, `Accept-Ranges`, `Vary`, cache policy headers, or unrelated extension headers.
- Preserve raw `SetBody` for callers that already own representation metadata. New `ReplaceBody` is the semantic body-replacement API.
- A replacement clears `Content-Length`; do not synthesize a new length. A failed/unsupported decode that leaves bytes unchanged must preserve existing encoding and length.
- Informational `100..199` responses except `101` do not consume the final response slot. HEAD/101/204/304 never emit buffered body bytes.
- Missing or empty `Accept-Encoding` selects identity. Parsing is case-insensitive; explicit coding/identity q-values override wildcard; invalid q-values are unavailable rather than silently treated as q=1.
- Available content codings are route-owned: gzip alone offers gzip+deflate, brotli alone offers br, and both together offer br+gzip+deflate. Equal positive q-values use server rank `br > gzip > deflate`; explicit identity participates when present.
- Identity remains the fallback when no positive compression coding is selected unless `identity;q=0` or an applicable `*;q=0` excludes it. If every eligible representation is unacceptable, return an empty 406.
- `deflate` is zlib-wrapped RFC 1950 data, not raw DEFLATE.
- Any eligible response whose representation can vary by `Accept-Encoding` carries exactly one case-insensitive `Accept-Encoding` Vary token on encoded, identity, and 406 results. An explicit legacy `vary:false` cannot suppress cache-safety.
- Existing `Content-Encoding` is never encoded again. 204 is not negotiated; 304 preserves upstream representation metadata but keeps the Vary contract. HEAD may advertise the selected coding but emits no body and removes an untrustworthy original length.
- Proxy-cache keeps its current base key and Vary index. PURGE must remove the base entry, index, and every registered variant in memory and on disk.
- All Go commands run through `bash -lc 'source .envrc && ...'`. Run only the listed focused packages/cases plus the build smoke gate.

## Work unit 1 — Body replacement and buffered-writer semantics

**Dependency:** none.

**Exclusive ownership:**

- `pkg/plugin/base/representation.go` (new)
- `pkg/plugin/base/representation_test.go` (new)
- `pkg/plugin/base/writer.go`
- `pkg/plugin/base/writer_test.go`
- `pkg/plugin/echo/plugin.go`
- `pkg/plugin/echo/plugin_test.go`
- `pkg/plugin/body_transformer/plugin.go`
- `pkg/plugin/body_transformer/plugin_test.go`
- `pkg/plugin/exit_transformer/plugin.go`
- `pkg/plugin/exit_transformer/plugin_test.go`
- `pkg/plugin/response_rewrite/plugin.go`
- `pkg/plugin/response_rewrite/plugin_test.go`

### Fixed API

```go
func InvalidateBodyDerivedHeaders(header http.Header)
func AppendVaryToken(header http.Header, token string)
func ResponseAllowsBody(method string, status int) bool
func (w *BufferedResponseWriter) ReplaceBody(body []byte)
```

- `InvalidateBodyDerivedHeaders` iterates actual map keys with `strings.EqualFold`; `Header.Del` alone is insufficient for direct lowercase duplicates.
- `AppendVaryToken` parses every Vary field-value by comma, compares tokens case-insensitively, preserves unrelated tokens, and adds the requested token once.
- `ResponseAllowsBody` returns false for HEAD and status 100..199, 204, and 304. `101` is final and bodyless.
- `GetOrCreateTransformResponseWriter` records the request method. `NewBufferedResponseWriter` retains the empty-method compatibility default.
- `WriteHeader(100..199 except 101)` snapshots informational headers without setting `wroteHeader`; `Commit` replays them in order before the final response.
- `SetStatusCode` and final commit discard body/derived length when the final status is bodyless. Nested pipeline commits must not resurrect bytes.

### Regression-first acceptance

- Base tests cover all nine headers with canonical and lowercase duplicate keys; Vary multi-value dedupe; standalone/shared `ReplaceBody`; `103 -> 200`; final 101/204/304 body rejection; 200 remapped to 204; HEAD through two nested transform writers; and nested no-body status propagation.
- `echo` and response-side `body-transformer` use `ReplaceBody` only when bytes change.
- `exit-transformer` tracks whether Lua returned a non-nil replacement body; nil preserves representation headers, a replacement invalidates them, and it no longer writes a replacement `Content-Length`.
- `response-rewrite` uses `ReplaceBody` for configured replacement and successful filter rewrite. Its unsupported/failed decoder path remains byte-for-byte and header-for-header unchanged.

### Focused commands

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base -run "^(TestInvalidateBodyDerivedHeaders|TestAppendVaryToken|TestBufferedResponseWriter|TestTransformPipeline)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/echo ./pkg/plugin/body_transformer ./pkg/plugin/exit_transformer ./pkg/plugin/response_rewrite -run "(Body|Representation|NoBody|Head|Informational|EncodedBodyCannotBeDecoded)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin/echo ./pkg/plugin/body_transformer ./pkg/plugin/exit_transformer ./pkg/plugin/response_rewrite -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/base/... ./pkg/plugin/echo/... ./pkg/plugin/body_transformer/... ./pkg/plugin/exit_transformer/... ./pkg/plugin/response_rewrite/...'
git diff --check -- pkg/plugin/base pkg/plugin/echo pkg/plugin/body_transformer pkg/plugin/exit_transformer pkg/plugin/response_rewrite
```

## Work unit 2 — Unified gzip/brotli negotiation and status semantics

**Dependency:** work unit 1 fixed helpers must exist and pass before dispatch.

**Exclusive ownership:**

- `pkg/plugin/compression/negotiation.go` (new)
- `pkg/plugin/compression/negotiation_test.go` (new)
- `pkg/plugin/compression/integration_test.go` (new)
- `pkg/plugin/gzip/plugin.go`
- `pkg/plugin/gzip/compress.go`
- `pkg/plugin/gzip/plugin_test.go`
- `pkg/plugin/brotli/plugin.go`
- `pkg/plugin/brotli/plugin_test.go`
- `t/plugin/gzip.yaml`
- `t/plugin/brotli.yaml`

### Fixed API

```go
type Coding string

const (
    Identity Coding = "identity"
    Gzip     Coding = "gzip"
    Deflate  Coding = "deflate"
    Brotli   Coding = "br"
)

type ResponseMeta struct {
    Method string
    Status int
    Header http.Header
}

type Offer struct {
    Coding   Coding
    Rank     int
    Eligible func(ResponseMeta) bool
}

type Decision struct {
    Coding        Coding
    NotAcceptable bool
    Vary          bool
    IdentityAllowed bool
}

func Register(r *http.Request, offers ...Offer) (*http.Request, *State)
func (s *State) Decide(meta ResponseMeta) Decision
```

- The outer plugin creates request context state and registers its offers; inner plugins add offers before upstream produces the final response.
- `Decide` freezes once, is concurrency-safe and idempotent, and selects from the complete registered offer set. Registration order and plugin priority do not affect the result.
- gzip registers gzip and deflate offers; brotli registers br. Eligibility uses final status, content type, existing coding, and known Content-Length/min-length without reading or duplicating an unknown streaming body.
- Both plugins use `base.AppendVaryToken` and `base.ResponseAllowsBody`.
- gzip replaces `compress/flate` with `compress/zlib`, including pooled writer Reset behavior.
- gzip's wrapper forwards 1xx except 101 immediately without sealing the final status. Brotli's bounded writer does the same and preserves `Flusher`, `Hijacker`, and `Unwrap` behavior when it must pass through.
- A selected HEAD representation sets coding metadata but writes no body. 101/204/304 do not start compressors. Existing encodings remain single-layer.
- Preserve brotli's configured memory cap and one-way pass-through after exceeding it.
- Direct Brotli hijack does not commit a default response; informational headers are isolated from final headers. If Brotli exceeds its cap while identity is forbidden, it returns an empty 406 instead of an unacceptable identity fallback.
- An existing `Content-Encoding` freezes a pass-through decision before route-owned offer filtering. Every generated empty 406 invalidates all body-derived headers.

### Regression-first acceptance

- `TestNegotiationMatrix` covers missing/empty header, case-insensitive tokens/params, duplicates, wildcard override, strict q parsing, explicit identity, single-plugin and combined offer sets, tie rank, and 406.
- `TestNegotiationIndependentOfRegistrationOrder` registers br/gzip/deflate in both orders and gets the same result.
- Gzip tests prove deflate decodes with `zlib.NewReader` and fails raw-flate assumptions.
- Each plugin covers Vary on encoded/identity/406, type/min-length identity, existing encoding, `103 -> 200`, 101 capability path, HEAD, 204, and 304.
- A combined in-package handler test covers default and reversed nesting, q-preferred br/gzip/deflate, and exactly one decoding layer.
- Combined tests cover accepted pre-encoded gzip/br and declared/unknown-length Brotli cap overflow in both nesting orders when identity is forbidden.
- Existing gzip/brotli real-process expectations describe the cache-safe default Vary contract and removal of stale validators after compression.

### Focused commands

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/compression -run "^TestNegotiation" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/gzip -run "(Negotiation|Deflate|Vary|Status|Head|ExistingEncoding)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/brotli -run "(Negotiation|Vary|Status|Head|ExistingEncoding|Bounded)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/compression ./pkg/plugin/gzip ./pkg/plugin/brotli -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/compression/... ./pkg/plugin/gzip/... ./pkg/plugin/brotli/...'
bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/gzip/default-compression$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/gzip/buffer-schema-variants$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/brotli/negotiation$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/brotli/defaults$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/brotli/etag$" -count=1'
git diff --check -- pkg/plugin/compression pkg/plugin/gzip pkg/plugin/brotli t/plugin/gzip.yaml t/plugin/brotli.yaml
```

## Work unit 3 — Cache signature and real variant lifecycle

**Dependency:** work units 1 and 2 must be accepted first so the integration test observes the final representation producer.

**Exclusive ownership:**

- `pkg/plugin/cacheutil/cache.go`
- `pkg/plugin/cacheutil/cache_test.go`
- `pkg/plugin/proxy_cache/plugin_test.go`
- `t/plugin/proxy-cache.yaml`

### Required behavior

- `VarySignature` incorporates every value from `Header.Values` for each normalized Vary field, using collision-safe boundaries. A different second header line must produce a different signature.
- Do not change `proxy_cache/plugin.go`: its current transition from Vary index to no-Vary base entry and its memory/disk PURGE semantics are the intended owner behavior.
- Add a package characterization test proving a no-Vary identity store replaces an existing variant index; this documents why producers must emit Vary on eligible identity responses.
- Add one serialized real-process case with `proxy-cache`, gzip, and brotli on the same route. Exercise identity, gzip, deflate, and br as MISS -> HIT, repeat identity between encoded variants, assert coding/Vary/body/cache status, then PURGE and prove every representation misses again.
- Keep byte-wrapper correctness in WU2 unit tests; the integration case need only decode or validate the selected representation enough to prove variants are not confused.

### Focused commands

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/cacheutil -run "^(TestParseVaryHeader|TestVarySignature)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/proxy_cache -run "^(TestHandler.*Vary.*|TestHandlerPurgeRemovesVaryVariants|TestHandlerNoVaryReplacesVariantIndex)$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/proxy-cache/memory-representation-variants-test-41$" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/cacheutil ./pkg/plugin/proxy_cache -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/cacheutil/... ./pkg/plugin/proxy_cache/...'
git diff --check -- pkg/plugin/cacheutil pkg/plugin/proxy_cache t/plugin/proxy-cache.yaml
```

## Combined acceptance and delivery

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin/echo ./pkg/plugin/body_transformer ./pkg/plugin/exit_transformer ./pkg/plugin/response_rewrite ./pkg/plugin/compression ./pkg/plugin/gzip ./pkg/plugin/brotli ./pkg/plugin/cacheutil ./pkg/plugin/proxy_cache -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/base/... ./pkg/plugin/echo/... ./pkg/plugin/body_transformer/... ./pkg/plugin/exit_transformer/... ./pkg/plugin/response_rewrite/... ./pkg/plugin/compression/... ./pkg/plugin/gzip/... ./pkg/plugin/brotli/... ./pkg/plugin/cacheutil/... ./pkg/plugin/proxy_cache/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- Run every listed exact `t/plugin` case serially because real-process cases share ports/resources. The proxy-cache representation lifecycle itself runs only once.
- Run an independent merge-level review after all three work units pass.
- Stage only this plan and the exact owned paths above.
- Commit with `git commit -m "fix(http): keep response representations consistent"` and create one ready PR.
