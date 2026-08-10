# CORS and External Authorization Input Implementation Plan

> **For agentic workers:** Execute only the bounded work unit assigned by the implementation brief. Use regression-first implementation and do not commit, push, or create a PR.

**Goal:** Close PR-026 and PR-027 by restoring APISIX-compatible CORS precedence and giving OPA/forward-auth one sanitized, versioned request-facts contract.

**Architecture:** Keep CORS on the `rs/cors` integration introduced by PR #79, fixing only local origin precedence and the final `Vary` write barrier. Add an immutable authorization-facts builder in `pkg/plugin/base`; OPA preserves its existing APISIX-compatible `input.type/request/var/route/service/consumer` envelope while sourcing request identity from those facts, and forward-auth derives reserved generated headers from the same facts.

**Validated base:** `origin/master` at `081b586a74e47395d180c46ef1485ba94d600508`.

## Global Constraints

- Preserve the `rs/cors` preflight and actual-request flow from PR #79; do not reintroduce hand-written preflight behavior.
- Preserve CORS metadata precedence. Once metadata has not admitted an origin, a non-empty `allow_origins_by_regex` is authoritative over fixed origins and `**`.
- Successful and denied origin-dependent responses must retain a single `Origin` token in `Vary`; preflight may also contain the `Access-Control-Request-*` tokens supplied by `rs/cors`.
- Authorization input schema version is integer `1`. Preserve the existing OPA envelope and query/port/timestamp fields.
- Preserve request headers as copied `map[string][]string`; never collapse repeated values to `Header.Get`.
- Client IP comes from `base.RequestVarFromNginx(r, "remote_addr")`, which honors real-ip context. Client port prefers `ctx.RemotePortKey`, then the socket peer.
- Route, service, and consumer DTOs contain only explicitly safe identity fields. Never serialize full resource/plugin/upstream objects.
- Forward-auth generated `X-Forwarded-Proto`, `Method`, `Host`, `Uri`, and `For` values are reserved and cannot be overridden by client headers, `request_headers`, or `extra_headers`.
- Run Go commands with `bash -lc 'source .envrc && ...'`.

## File Structure

- Modify `pkg/plugin/cors/plugin.go` and `plugin_test.go`.
- Create `pkg/plugin/base/authorization_input.go` and `authorization_input_test.go`.
- Modify `pkg/plugin/opa/plugin.go` and `plugin_test.go`.
- Modify `pkg/plugin/forward_auth/plugin.go` and `plugin_test.go`.

### Task 1: Make CORS regex precedence and final Vary deterministic

**Files:**
- Modify: `pkg/plugin/cors/plugin.go`
- Test: `pkg/plugin/cors/plugin_test.go`

**Interfaces:**
- Consumes: metadata, fixed origin string, compiled regex list, request Origin.
- Produces: existing `responseOrigin(origin string) (string, bool)` semantics plus a final response-header write barrier.

- [ ] **Step 1: Add the focused red tests**

Add `TestHandlerRegexOriginPrecedence`: fixed=`https://fixed.example`, regex=`^https://allowed[.]example$`; fixed-only must be denied, regex-only admitted, and `**` must not bypass a non-empty regex.

Add `TestHandlerPreflightRegexOriginPrecedenceAndVary`: credentialed allowed regex preflight returns the request origin and credentials; fixed-only is denied by missing ACAO even though `rs/cors` returns 200; both contain exactly one `Origin` token in `Vary`.

Add `TestHandlerActualResponseReassertsVaryOrigin`: downstream `Set("Vary", "Via")` cannot erase `Origin` for either an admitted or denied actual request.

- [ ] **Step 2: Run the focused failure**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/cors -run "^(TestHandlerRegexOriginPrecedence|TestHandlerPreflightRegexOriginPrecedenceAndVary|TestHandlerActualResponseReassertsVaryOrigin)$" -count=1'`

- [ ] **Step 3: Repair only the validated seams**

In `responseOrigin`, retain the existing metadata block, then evaluate a non-empty regex list before `**`, wildcard, or fixed-origin logic; return denied immediately when none match. Remove the old trailing regex loop.

In `varyResponseWriter.WriteHeader`, add `Origin` before calling the existing normalizer. Do not modify `PostInit`, `rs/cors` options, or restore the old preflight implementation.

- [ ] **Step 4: Verify CORS**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/cors -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/cors/...'
```

### Task 2: Add stable, sanitized authorization facts

**Files:**
- Create: `pkg/plugin/base/authorization_input.go`
- Test: `pkg/plugin/base/authorization_input_test.go`

**Interfaces:**
- Consumes: `*http.Request`, optional server address, safe route/service identities.
- Produces: `CaptureAuthorizationFacts(r *http.Request, serverAddr string, route, service AuthorizationResource) AuthorizationFacts`.

- [ ] **Step 1: Define versioned DTOs**

Define `AuthorizationResource` with only `id`, `name`, and `uri`, and `AuthorizationFacts` with version, scheme, method, host, path, raw query, copied canonical headers, client IP/port, server address/port, route, and service. Use `omitempty` on optional values without serializing full resources.

- [ ] **Step 2: Implement capture rules**

Clone every header value slice. Derive client IP from `RequestVarFromNginx`; prefer `ctx.RemotePortKey` for client port and fall back to `net.SplitHostPort(r.RemoteAddr)`. Split the provided server address safely. Derive scheme from TLS/URL without trusting an arbitrary client `X-Forwarded-Proto` value.

- [ ] **Step 3: Test real IP, repeated headers, immutable copies, and secret exclusion**

Assert a real-ip context value overrides the socket address, its paired context port is used, two `X-Role` values survive, mutating the request after capture does not mutate facts, and serialized facts contain only the explicit DTO fields.

- [ ] **Step 4: Verify base DTO**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/base -run "^TestCaptureAuthorizationFacts" -count=1'`

### Task 3: Preserve OPA compatibility while sanitizing optional resources

**Files:**
- Modify: `pkg/plugin/opa/plugin.go`
- Test: `pkg/plugin/opa/plugin_test.go`

**Interfaces:**
- Consumes: `base.AuthorizationFacts` and a safe consumer identity.
- Produces: the existing OPA `input` envelope with `version=1`, repeated headers retained, and optional resources restricted to safe fields.

- [ ] **Step 1: Add policy-input shape and exclusion tests**

Assert `input.version=1`; preserve `type=http`, `request.port/query`, and `var.timestamp`; verify two header values remain an array; real-ip populates `var.remote_addr`; route/service contain only id/name/uri; consumer contains only username/group_id. Seed route/service upstream and consumer plugin secrets and assert their keys/values are absent from JSON.

- [ ] **Step 2: Source the existing envelope from facts**

Add `Version int` to `opaInput`. Change request headers to `map[string][]string`. Build safe resource DTOs from configured resource context or the existing local route/service identity fallback. Build a small safe consumer DTO from the attached `resource.Consumer`. Do not return full `resource.Route`, `resource.Service`, or `resource.Consumer` from OPA helpers.

Retain query normalization, request port, `type`, timestamp, OPA decision handling, and response/upstream-header behavior.

- [ ] **Step 3: Verify OPA**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/opa -count=1'`

### Task 4: Derive forward-auth generated headers from the same facts

**Files:**
- Modify: `pkg/plugin/forward_auth/plugin.go`
- Test: `pkg/plugin/forward_auth/plugin_test.go`

**Interfaces:**
- Consumes: `base.CaptureAuthorizationFacts`.
- Produces: authoritative generated `X-Forwarded-*` headers and lossless configured non-reserved headers.

- [ ] **Step 1: Add real-ip, reserved-header, and repeated-header tests**

Set a socket peer and different real-ip context. Assert `X-Forwarded-For` equals the normalized real IP. Attempt to override every generated header through both `request_headers` and `extra_headers`; generated facts must win. Assert a configured non-generated header preserves all request values.

- [ ] **Step 2: Apply configured headers before authoritative generated headers**

Copy configured request header values with `Header.Values` into cloned slices. Keep extra-header interpolation unchanged for non-reserved names. Apply generated headers last from captured facts so neither input source can override them.

- [ ] **Step 3: Verify forward-auth**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/forward_auth -count=1'`

### Task 5: Combined acceptance and independent PR delivery

- [ ] **Step 1: Run combined gates**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin/cors ./pkg/plugin/opa ./pkg/plugin/forward_auth -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/base/... ./pkg/plugin/cors/... ./pkg/plugin/opa/... ./pkg/plugin/forward_auth/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 2: Commit the exact independent PR scope**

```bash
git add docs/superpowers/plans/2026-08-10-cors-external-authorization-input.md \
  pkg/plugin/base/authorization_input.go pkg/plugin/base/authorization_input_test.go \
  pkg/plugin/cors/plugin.go pkg/plugin/cors/plugin_test.go \
  pkg/plugin/opa/plugin.go pkg/plugin/opa/plugin_test.go \
  pkg/plugin/forward_auth/plugin.go pkg/plugin/forward_auth/plugin_test.go
git commit -m "fix(authz): stabilize external authorization facts"
```
