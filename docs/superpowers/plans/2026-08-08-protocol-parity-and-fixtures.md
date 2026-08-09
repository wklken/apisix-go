# Protocol Parity and Fixture Fidelity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close two gRPC wire/status gaps, make proxy-rewrite visible at the correct phase, and restore the exact APISIX 3.17 limit-count and traffic-split fixture semantics.

**Architecture:** Keep protocol fixes in their plugin handlers and schemas. Treat `t/plugin` manifests as executable mirrors of the pinned upstream TEST blocks: use step-level config reload for the limit-count disable sequence and copy the exact `arg_id` match rules for traffic-split2 TEST 16.

**Tech Stack:** Go 1.26, JSON Schema, `net/http`, APISIX plugin integration runner YAML.

## File Structure

- Modify `pkg/plugin/grpc_transcode/plugin.go` and its tests: make `deadline` integer milliseconds in both the public config and custom JSON decoder.
- Modify `pkg/plugin/grpc_web/plugin.go` and its tests: align non-POST status with APISIX 3.17.
- Modify `pkg/plugin/proxy_rewrite/plugin.go` and its tests: finalize request-visible URI/method during the plugin phase.
- Modify `t/plugin/limit-count.yaml` and `t/plugin/traffic-split.yaml`: restore the pinned upstream fixture sequences.

## Global Constraints

- Canonical upstream behavior is Apache APISIX `3.17.0` tag `9ef2ecab67f652d38365049613610ef649bb4ad0`.
- Run every Go command as `bash -lc 'source .envrc && ...'` from the repository root.
- `grpc-transcode.deadline` remains milliseconds; reject fractions instead of inventing a different unit conversion.
- Proxy rewrite must change URI/method before lower-priority plugins, but host/scheme remain final upstream-target concerns.
- Run only one real-process `t/plugin` command at a time.
- Use impact-scoped tests and `make build`; do not run the full integration package or `make test`.
- Subagents require explicit user authorization under `AGENTS.md`; inline execution is the default.

---

### Task 1: Make grpc-transcode deadline wire-safe

**Files:**
- Modify: `pkg/plugin/grpc_transcode/plugin.go`
- Test: `pkg/plugin/grpc_transcode/plugin_test.go`

**Interfaces:**
- Consumes: `Config.Deadline float64` as populated by the existing schema parser.
- Produces: schema accepts only integer values `>= 0`; positive values produce `<integer>m`.

- [ ] **Step 1: Add a schema regression test**

```go
func TestSchemaRejectsFractionalDeadline(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Validate(map[string]any{
		"proto_id": "echo",
		"service":  "echo.EchoService",
		"method":   "Echo",
		"deadline": 1.5,
	}, p.GetSchema()); err == nil {
		t.Fatal("fractional deadline validation error = nil")
	}
}
```

- [ ] **Step 2: Verify the test fails**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/grpc_transcode -run "^TestSchemaRejectsFractionalDeadline$" -count=1'`

Expected: FAIL because the current schema uses `"type": "number"`.

- [ ] **Step 3: Tighten the schema and Go type**

Change the schema field to:

```json
"deadline": {
  "type": "integer",
  "minimum": 0,
  "default": 0
}
```

Change both `Config.Deadline` and the `raw.Deadline` field inside `Config.UnmarshalJSON` from `float64` to `int`, and replace the header formatting with:

```go
r.Header.Set("grpc-timeout", strconv.Itoa(p.config.Deadline)+"m")
```

- [ ] **Step 4: Run grpc-transcode tests**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/grpc_transcode -count=1'`

Expected: PASS, including the existing `250m` assertion.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/grpc_transcode/plugin.go pkg/plugin/grpc_transcode/plugin_test.go
git commit -m "fix(grpc-transcode): require integer deadlines"
```

### Task 2: Return 405 for non-POST grpc-web requests

**Files:**
- Modify: `pkg/plugin/grpc_web/plugin.go`
- Test: `pkg/plugin/grpc_web/plugin_test.go`
- Test: `pkg/route/plugin_parity_test.go`

**Interfaces:**
- Consumes: request method in `Plugin.Handler`.
- Produces: OPTIONS remains 204; non-POST/non-OPTIONS returns 405; invalid POST content type remains 400.

- [ ] **Step 1: Change both existing assertions before production code**

In `TestHandlerRejectsInvalidRequest`, set the `non-post` row to:

```go
wantStatus: http.StatusMethodNotAllowed,
```

In `TestGRPCWebRouteChainRejectsUnsupportedMethod`, assert:

```go
if res.Code != http.StatusMethodNotAllowed {
	t.Fatalf("status = %d, want %d", res.Code, http.StatusMethodNotAllowed)
}
```

- [ ] **Step 2: Verify the focused failures**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/grpc_web -run "^TestHandlerRejectsInvalidRequest$" -count=1'
bash -lc 'source .envrc && go test ./pkg/route -run "^TestGRPCWebRouteChainRejectsUnsupportedMethod$" -count=1'
```

Expected: both FAIL with 400 returned instead of 405.

- [ ] **Step 3: Apply the one-line handler correction**

Replace `w.WriteHeader(http.StatusBadRequest)` in the non-POST branch with `w.WriteHeader(http.StatusMethodNotAllowed)`. Do not change the invalid-content-type branch.

- [ ] **Step 4: Run both affected packages**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/grpc_web ./pkg/route -run "TestHandlerRejectsInvalidRequest|TestGRPCWebRouteChainRejectsUnsupportedMethod" -count=1'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/grpc_web/plugin.go pkg/plugin/grpc_web/plugin_test.go pkg/route/plugin_parity_test.go
git commit -m "fix(grpc-web): reject unsupported methods with 405"
```

### Task 3: Finalize proxy-rewrite URI and method inside rewrite phase

**Files:**
- Modify: `pkg/plugin/proxy_rewrite/plugin.go`
- Test: `pkg/plugin/proxy_rewrite/plugin_test.go`
- Test: `pkg/route/proxy_rewrite_test.go`

**Interfaces:**
- Consumes: `apisixctx.ProxyRewriteKey` and `apisixctx.FinalizeProxyRewrite(*http.Request)`.
- Produces: the next plugin sees rewritten `r.URL.Path`, `RawQuery`, and `r.Method`; route director still applies host/scheme through the stored context.

- [ ] **Step 1: Add a direct phase-visibility regression test**

```go
func TestHandlerFinalizesURIAndMethodBeforeNextPlugin(t *testing.T) {
	p := newTestPlugin(t, Config{Uri: "/rewritten?fixed=1", Method: http.MethodPatch})
	req := httptest.NewRequest(http.MethodGet, "/original?incoming=1", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/rewritten" || r.URL.RawQuery != "fixed=1&incoming=1" {
			t.Fatalf("URL = %s?%s, want /rewritten?fixed=1&incoming=1", r.URL.Path, r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}
```

- [ ] **Step 2: Verify the current late-finalize behavior**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/proxy_rewrite -run "^TestHandlerFinalizesURIAndMethodBeforeNextPlugin$" -count=1'`

Expected: FAIL because the next handler sees GET and `/original`.

- [ ] **Step 3: Finalize on the request passed to `next`**

After storing the rewrite map in context, use this ordering:

```go
rewritten := r.WithContext(context.WithValue(r.Context(), apisixctx.ProxyRewriteKey, data))
apisixctx.FinalizeProxyRewrite(rewritten)
p.config.Headers.apply(rewritten, captures)
next.ServeHTTP(w, rewritten)
```

Move header application to this position so `$uri` and `$request_method` values resolve against the rewritten request, matching the upstream rewrite-phase order. Keep host and scheme only in `data`; `FinalizeProxyRewrite` intentionally does not apply them.

- [ ] **Step 4: Add a header-order assertion**

Extend the new test config with `Headers.Set: map[string]string{"X-Rewrite-View": "$request_method:$uri"}` and assert the next handler reads `PATCH:/rewritten`.

- [ ] **Step 5: Run plugin and route rewrite tests**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/proxy_rewrite ./pkg/route -run "ProxyRewrite|proxy_rewrite" -count=1'`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/plugin/proxy_rewrite/plugin.go pkg/plugin/proxy_rewrite/plugin_test.go pkg/route/proxy_rewrite_test.go
git commit -m "fix(proxy-rewrite): expose rewrite before downstream plugins"
```

### Task 4: Restore the limit-count disable sequence in the integration manifest

**Files:**
- Modify: `t/plugin/limit-count.yaml`
- Test: `t/plugin/manifest_test.go`

**Interfaces:**
- Consumes: `CaseStep.Config` plus required `ConfigProbe` reload barrier.
- Produces: one case owning pinned source tests `[16, 17]`, with the same route ID transitioning from configured `limit-count` to `plugins: {}`.

- [ ] **Step 1: Replace the two weakened cases with one stateful case**

Use this structure, retaining the existing fixture address substitution:

```yaml
- name: limit-count-disable-plugin-tests-16-17
  source:
    file: t/plugin/limit-count.t
    tests: [16, 17]
  config:
    routes:
      - id: limit-count-route
        uri: /limit-count
        plugins:
          limit-count:
            count: 2
            time_window: 60
            key: remote_addr
            rejected_code: 503
        upstream:
          type: roundrobin
          nodes: {'{{FIXTURE.primary.ADDR}}': 1}
  fixtures:
    - name: primary
      kind: http
      count: {at_least: 7, at_most: 7}
      expect: [{method: GET, path: {equals: /limit-count}}]
      respond: [{status: 200, body: limit-count upstream}]
  steps:
    - {name: enabled-request-1, input: {method: GET, path: /limit-count}, output: {status: 200}}
    - {name: enabled-request-2, input: {method: GET, path: /limit-count}, output: {status: 200}}
    - {name: enabled-request-over-limit, input: {method: GET, path: /limit-count}, output: {status: 503}}
    - name: disable-plugin-and-first-unlimited-request
      config:
        routes:
          - id: limit-count-route
            uri: /limit-count
            plugins: {}
            upstream:
              type: roundrobin
              nodes: {'{{FIXTURE.primary.ADDR}}': 1}
      config_probe:
        input: {method: GET, path: /limit-count}
        output: {status: 200, body: {equals: limit-count upstream}}
      input: {method: GET, path: /limit-count}
      output: {status: 200, body: {equals: limit-count upstream}}
      repeat: 4
```

- [ ] **Step 2: Run manifest validation**

Run: `bash -lc 'source .envrc && go test ./t/plugin -run "^TestManifestCorpusValidates$" -count=1'`

Expected: PASS without starting the real-process integration harness.

- [ ] **Step 3: Run the exact real-process case once**

Run: `bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/limit-count/limit-count-disable-plugin-tests-16-17$" -count=1'`

Expected: PASS with seven upstream requests: two before disable, one config probe, and four after disable.

- [ ] **Step 4: Commit**

```bash
git add t/plugin/limit-count.yaml
git commit -m "test(limit-count): cover plugin disable reload"
```

### Task 5: Restore traffic-split2 TEST 16 match rules

**Files:**
- Modify: `t/plugin/traffic-split.yaml`
- Test: `t/plugin/manifest_test.go`

**Interfaces:**
- Consumes: pinned TEST 16 rules `arg_id == 1` and `arg_id == 2` with upstream IDs `one` and `two`.
- Produces: TEST 16 config matches the source; TEST 17 remains the behavioral consumer of that exact shape.

- [ ] **Step 1: Replace the unconditional rule in TEST 16**

Use these two rules:

```yaml
rules:
  - match:
      - vars: [[arg_id, ==, "1"]]
    weighted_upstreams:
      - {upstream_id: one, weight: 1}
  - match:
      - vars: [[arg_id, ==, "2"]]
    weighted_upstreams:
      - {upstream_id: two, weight: 1}
```

Change the TEST 16 steps to assert `/split?id=1 -> first`, `/split?id=2 -> second`, and `/split -> route`; do not keep `^(first|second)$`.

- [ ] **Step 2: Run manifest validation**

Run: `bash -lc 'source .envrc && go test ./t/plugin -run "^TestManifestCorpusValidates$" -count=1'`

Expected: PASS.

- [ ] **Step 3: Run the exact TEST 16/17 integration cases serially**

Run:

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/traffic-split/traffic-split2-16-set-upstream-upstream-id-1-upstream-id-2-and-add-route$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/traffic-split/traffic-split2-17-hit-each-upstream-separately$" -count=1'
```

Expected: both PASS. Never run these two commands concurrently.

- [ ] **Step 4: Commit**

```bash
git add t/plugin/traffic-split.yaml
git commit -m "test(traffic-split): restore upstream-id match rules"
```

### Task 6: Final scoped verification

**Files:**
- Verify only; no planned file changes.

**Interfaces:**
- Consumes: Tasks 1-5.
- Produces: focused verification evidence for protocol and fixture parity.

- [ ] **Step 1: Run affected unit packages**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/grpc_transcode ./pkg/plugin/grpc_web ./pkg/plugin/proxy_rewrite ./pkg/route -count=1'`

Expected: PASS.

- [ ] **Step 2: Run lint, build, and diff checks**

Run:

```bash
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build'
git diff --check
```

Expected: all PASS. Run `bash -lc 'source .envrc && make clean'` when the binary is no longer needed.
