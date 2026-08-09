# Data Integrity and Cache Correctness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve complete request bodies through chaitin-waf, preserve JSON integers through request-validation normalization, and prevent HEAD responses from poisoning GET cache entries.

**Architecture:** Keep each fix inside its owning plugin. Reuse the request-context body reader for full-body replay, use the project JSON decoder with `UseNumber`, and preserve the shared GET/HEAD cache key while making HEAD misses read-only with respect to cache storage.

**Tech Stack:** Go 1.26, `net/http`, `pkg/apisix/ctx`, `pkg/json`, existing plugin unit tests.

## File Structure

- Modify `pkg/plugin/chaitin_waf/plugin.go` and its tests: separate full request replay from bounded WAF inspection.
- Modify `pkg/plugin/request_validation/plugin.go` and its tests: preserve JSON numbers during normalization.
- Modify `pkg/plugin/proxy_cache/plugin.go` and its tests: make HEAD cache misses storage-read-only while retaining shared GET hits.

## Global Constraints

- Run every Go command as `bash -lc 'source .envrc && ...'` from the repository root.
- Do not change APISIX plugin schemas or add dependencies.
- Keep JSON normalization after validation; do not forward the untouched body because the plugin intentionally prevents JSON interoperability mismatches.
- HEAD may consume an existing GET cache entry, but a HEAD miss must not create or replace a cache entry.
- Use impact-scoped tests, then `make build`; do not run `go test ./...` or `make test`.
- Subagents require explicit user authorization under `AGENTS.md`; inline execution is the default.

---

### Task 1: Preserve the complete body around chaitin-waf inspection

**Files:**
- Modify: `pkg/plugin/chaitin_waf/plugin.go`
- Test: `pkg/plugin/chaitin_waf/plugin_test.go`

**Interfaces:**
- Consumes: `ctx.ReadRequestBody(r *http.Request) ([]byte, error)`.
- Produces: `askWAF` sends at most `ReqBodySize * 1024` bytes to WAF while leaving the complete body replayable through `r.Body`.

- [ ] **Step 1: Add a regression test for a body larger than the WAF inspection limit**

Extend the existing restore test with a dedicated case:

```go
func TestHandlerSendsBoundedBodyToWAFAndPreservesFullBodyForUpstream(t *testing.T) {
	var wafBody []byte
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		wafBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read WAF body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(wafDecision{Status: http.StatusOK})
	}))
	t.Cleanup(waf.Close)

	p := newTestPlugin(t, Config{
		Mode:  "block",
		Nodes: []Node{nodeFromURL(t, waf.URL)},
		Config: WAFConfig{ReqBodySize: 1},
	})
	fullBody := bytes.Repeat([]byte("a"), 2*1024)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/upload", bytes.NewReader(fullBody))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if !bytes.Equal(got, fullBody) {
			t.Fatalf("upstream body length = %d, want %d", len(got), len(fullBody))
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if len(wafBody) != 1024 {
		t.Fatalf("WAF body length = %d, want 1024", len(wafBody))
	}
}
```

- [ ] **Step 2: Run the regression test and verify the current truncation**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/chaitin_waf -run "^TestHandlerSendsBoundedBodyToWAFAndPreservesFullBodyForUpstream$" -count=1'`

Expected: FAIL because the upstream receives 1024 bytes instead of 2048.

- [ ] **Step 3: Read and restore the complete body, then derive the WAF copy**

Replace the `io.LimitReader` read in `askWAF` with:

```go
body, err := ctx.ReadRequestBody(r)
if err != nil {
	return wafDecision{}, 0, err
}
inspectionBody := body
limit := cfg.ReqBodySize * 1024
if limit > 0 && len(inspectionBody) > limit {
	inspectionBody = inspectionBody[:limit]
}
```

Build the WAF request with `bytes.NewReader(inspectionBody)`. Do not replace `r.Body` in `askWAF`; `ctx.ReadRequestBody` has already restored the complete body and request variable.

- [ ] **Step 4: Run the focused package test**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/chaitin_waf -count=1'`

Expected: PASS.

- [ ] **Step 5: Commit the independently testable fix**

```bash
git add pkg/plugin/chaitin_waf/plugin.go pkg/plugin/chaitin_waf/plugin_test.go
git commit -m "fix(chaitin-waf): preserve full request body"
```

### Task 2: Preserve large JSON integers during request validation

**Files:**
- Modify: `pkg/plugin/request_validation/plugin.go`
- Test: `pkg/plugin/request_validation/plugin_test.go`

**Interfaces:**
- Consumes: `json.NewDecoder(io.Reader)` and its `UseNumber()` method.
- Produces: `parseJSON(data []byte) (any, error)` returns `json.Number` for untyped JSON numbers and `normalizeJSONBody` emits the exact decimal integer.

- [ ] **Step 1: Add a regression test for an integer above 2^53**

```go
func TestHandlerPreservesLargeIntegerDuringJSONNormalization(t *testing.T) {
	p := newTestPlugin(t, Config{BodySchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "integer"},
		},
	}})

	const body = `{"id":9007199254740993}`
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"http://example.com/validate",
		strings.NewReader(body),
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if string(got) != body {
			t.Fatalf("upstream body = %s, want %s", got, body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}
```

- [ ] **Step 2: Run the test and verify precision loss**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/request_validation -run "^TestHandlerPreservesLargeIntegerDuringJSONNormalization$" -count=1'`

Expected: FAIL because the normalized body contains `9007199254740992`.

- [ ] **Step 3: Decode JSON with `UseNumber`**

Replace the untyped `json.Unmarshal` call in `parseJSON` with:

```go
decoder := json.NewDecoder(bytes.NewReader(data))
decoder.UseNumber()
var result any
if err := decoder.Decode(&result); err != nil {
	return nil, err
}
return result, nil
```

Add the `bytes` import and remove no longer used imports only if this edit makes them unused.

- [ ] **Step 4: Run request-validation tests**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/request_validation -count=1'`

Expected: PASS.

- [ ] **Step 5: Commit the independently testable fix**

```bash
git add pkg/plugin/request_validation/plugin.go pkg/plugin/request_validation/plugin_test.go
git commit -m "fix(request-validation): preserve JSON number precision"
```

### Task 3: Make HEAD cache misses non-mutating

**Files:**
- Modify: `pkg/plugin/proxy_cache/plugin.go`
- Test: `pkg/plugin/proxy_cache/plugin_test.go`

**Interfaces:**
- Consumes: the existing shared `cacheKey`, `lookup`, and `fetchAndMaybeStore` path.
- Produces: HEAD can return `HIT` from a GET-created entry; a HEAD `MISS` calls upstream with `shouldStore=false`.

- [ ] **Step 1: Add a regression test for HEAD-first then GET**

```go
func TestHandlerDoesNotStoreHEADMissUnderGETCacheKey(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Length", "8")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("get-body"))
		}
	}))

	head := performRequest(t, handler, http.MethodHead, "/head-first", nil)
	get := performRequest(t, handler, http.MethodGet, "/head-first", nil)
	getHit := performRequest(t, handler, http.MethodGet, "/head-first", nil)

	if head.Header().Get(cacheStatusHeader) != "MISS" {
		t.Fatalf("HEAD cache status = %q, want MISS", head.Header().Get(cacheStatusHeader))
	}
	if get.Header().Get(cacheStatusHeader) != "MISS" || get.Body.String() != "get-body" {
		t.Fatalf("first GET = %q/%q, want MISS/get-body", get.Header().Get(cacheStatusHeader), get.Body.String())
	}
	if getHit.Header().Get(cacheStatusHeader) != "HIT" || getHit.Body.String() != "get-body" {
		t.Fatalf("second GET = %q/%q, want HIT/get-body", getHit.Header().Get(cacheStatusHeader), getHit.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}
```

- [ ] **Step 2: Run the test and verify the poisoned hit**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/proxy_cache -run "^TestHandlerDoesNotStoreHEADMissUnderGETCacheKey$" -count=1'`

Expected: FAIL because the first GET is served as a HIT from the HEAD-created entry.

- [ ] **Step 3: Disable storage for HEAD misses without changing lookup**

Immediately before the miss-path `fetchAndMaybeStore` call in `Handler`, derive:

```go
shouldStore := r.Method != http.MethodHead && !p.hasTruthyValue(r, p.config.NoCache)
p.fetchAndMaybeStore(w, r, next, key, "MISS", shouldStore)
```

Apply the same `r.Method != http.MethodHead` condition to EXPIRED and STALE refreshes so HEAD never replaces a shared entry. Leave lookup before this branch so HEAD can reuse a valid GET entry.

- [ ] **Step 4: Add the reciprocal GET-first/HEAD-hit assertion**

Add a second test that performs GET then HEAD on the same key, asserts the HEAD status is `HIT`, and asserts upstream was called once. Do not assert a response body from `httptest.ResponseRecorder`; the contract under test is cache reuse and headers.

- [ ] **Step 5: Run proxy-cache tests and build**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/proxy_cache -count=1'
bash -lc 'source .envrc && make build'
```

Expected: both PASS.

- [ ] **Step 6: Commit the independently testable fix**

```bash
git add pkg/plugin/proxy_cache/plugin.go pkg/plugin/proxy_cache/plugin_test.go
git commit -m "fix(proxy-cache): keep HEAD misses out of GET cache"
```

### Task 4: Final scoped verification

**Files:**
- Verify only; no planned file changes.

**Interfaces:**
- Consumes: the three preceding commits.
- Produces: reusable focused verification evidence for this plan.

- [ ] **Step 1: Run the affected packages together**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/chaitin_waf ./pkg/plugin/request_validation ./pkg/plugin/proxy_cache -count=1'`

Expected: PASS.

- [ ] **Step 2: Run lint, build, and diff checks**

Run:

```bash
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build'
git diff --check
```

Expected: all PASS. Run `bash -lc 'source .envrc && make clean'` after the build if the binary is not needed.
