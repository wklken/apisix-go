# Authentication and Ingress Trust Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task in an isolated worktree. This plan does not authorize subagents.

**Goal:** Close the remaining authentication defaults and request-trust leaks: verified Wolf TLS, secure OIDC cookies, constant-time byte-exact Basic Auth, credential cleanup on anonymous fallback, LDAP `hide_credentials`, authoritative consumer identity headers, and query-free proxy failure logs.

**Architecture:** Keep each authentication plugin as its policy owner, but apply two cross-cutting rules at the outer route boundary: inbound clients never own `X-Consumer-Username`, and internal proxy error logs never include raw query strings. Authentication plugins may set the consumer header only after successful authentication state exists.

**Tech Stack:** Go 1.26, `crypto/subtle`, `crypto/tls`, `net/http`, real `httptest` TLS handshakes, existing request-phase authentication state.

## Frozen contracts

- `wolf-rbac.ssl_verify` defaults to true at route and consumer scope. `false` remains an explicit compatibility opt-out. Plain `http://` retains its warning.
- OIDC code-flow cookies default to `Secure=true`; an explicit `cookie_secure: false` remains available for loopback development. Bearer-only mode creates no session cookie.
- Basic usernames and passwords are compared byte-for-byte after Base64 decode; no whitespace removal or normalization occurs. Password comparison uses `subtle.ConstantTimeCompare`.
- When `hide_credentials=true` and an Authorization header is present, it is removed before any success, failure, or anonymous continuation after the plugin takes ownership of it.
- LDAP adds APISIX-compatible `hide_credentials` with default false and removes a successfully parsed Basic header before downstream execution when enabled.
- The route boundary deletes client-supplied `X-Consumer-Username`; only authenticated consumer attachment may set it later.
- Proxy failure logs include method and escaped path only. Query, fragment, userinfo, and raw request URI never enter the error message.

### Task 1: Verify Wolf TLS by default

**Files:**
- Modify: `pkg/plugin/wolf_rbac/plugin.go`
- Modify: `pkg/plugin/wolf_rbac/plugin_test.go`

- [ ] **Step 1: Add a real TLS default/opt-out matrix**

Use `httptest.NewTLSServer`. The default/nil `SSLVerify` row must reject the untrusted server; `ssl_verify:false` must reach it; a trusted transport injected through `p.client` must succeed with verification enabled. Cover consumer-scope nil inheriting the route default.

- [ ] **Step 2: Run the focused red tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/wolf_rbac -run "(SSLVerify|TLS|ClientForConfig)" -count=1'
```

Expected before implementation: nil/default selects `p.insecureClient`.

- [ ] **Step 3: Default both config layers to verified TLS**

In `PostInit`, set a nil plugin default to true. In `consumerConfig.applyDefaults`, inherit the plugin pointer, which is now non-nil. Keep the request selector explicit:

```go
func (p *Plugin) clientForConfig(cfg consumerConfig) *http.Client {
	if cfg.SSLVerify == nil || *cfg.SSLVerify {
		return p.client
	}
	return p.insecureClient
}
```

Do not silently rewrite `http://` URLs or remove the warning.

### Task 2: Make OIDC session cookies secure by default

**Files:**
- Modify: `pkg/plugin/openid_connect/plugin.go`
- Modify: `pkg/plugin/openid_connect/session.go`
- Modify: `pkg/plugin/openid_connect/plugin_test.go`
- Create: `pkg/plugin/openid_connect/session_test.go`

- [ ] **Step 1: Add omitted/explicit cookie tests**

Cover omitted, explicit true, and explicit false values for both write and clear cookies. Omitted must produce `Secure`; explicit false must not. Preserve `HttpOnly` and SameSite behavior.

- [ ] **Step 2: Make omission distinguishable**

Change `SessionConfig.CookieSecure` from `bool` to `*bool`, update schema default to true, and apply the default in `PostInit`:

```go
if p.config.Session.CookieSecure == nil {
	secure := true
	p.config.Session.CookieSecure = &secure
}
```

Use `*p.config.Session.CookieSecure` only after code-flow defaults have run. Update direct test fixtures that bypass `PostInit` to provide a pointer or initialize through the existing helper.

### Task 3: Make Basic Auth byte-exact and constant-time

**Files:**
- Modify: `pkg/plugin/basic_auth/plugin.go`
- Modify: `pkg/plugin/basic_auth/plugin_test.go`

- [ ] **Step 1: Replace normalization tests with exact-byte tests**

Add rows for internal spaces, leading/trailing spaces, empty password, same-length wrong password, and different-length wrong password. A consumer named `user name` with password `sec ret` succeeds only for those exact bytes. `user name` must not authenticate as `username`.

- [ ] **Step 2: Add malformed anonymous cleanup coverage**

With `hide_credentials=true` and a valid anonymous consumer, send `Authorization: Bearer attacker` and invalid Basic Base64. Assert anonymous continuation and an empty downstream Authorization header.

- [ ] **Step 3: Run the red tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/basic_auth -run "(ConstantTime|ExactCredential|Malformed.*Anonymous|HideCredentials)" -count=1'
```

- [ ] **Step 4: Remove normalization and compare in constant time**

Delete `normalizeCredential`. Once a non-empty header is owned, remove it before parsing when hiding is enabled. Compare decoded password bytes without logging them:

```go
if *p.config.HideCredentials {
	r.Header.Del("Authorization")
}
user, pass, err := parseBasicAuthorization(authHeader)
// existing generic error/anonymous handling
if subtle.ConstantTimeCompare([]byte(pass), []byte(ba.Password)) != 1 {
	// existing invalid-user path
}
```

Do not normalize the username before `GetConsumerByPluginKey`.

### Task 4: Add LDAP credential hiding

**Files:**
- Modify: `pkg/plugin/ldap_auth/plugin.go`
- Modify: `pkg/plugin/ldap_auth/plugin_test.go`
- Modify: `docs/plugins.md`

- [ ] **Step 1: Add schema and behavior red tests**

Prove schema accepts `hide_credentials`, default false preserves the header, explicit true removes it before successful downstream execution, and malformed/failed LDAP authentication never invokes downstream.

- [ ] **Step 2: Implement the field without changing LDAP credential or TLS policy**

Add `HideCredentials *bool` to `Config`, default it to false in `PostInit`, and delete Authorization immediately after successful `extractBasicUser` when enabled. Keep the existing LDAP whitespace behavior unchanged: this PR closes credential hiding, while LDAP credential-byte parity remains outside the qualified allowlist and needs separate evidence before behavior changes.

- [ ] **Step 3: Run focused auth packages**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/wolf_rbac ./pkg/plugin/openid_connect ./pkg/plugin/basic_auth ./pkg/plugin/ldap_auth -count=1'
```

### Task 5: Own consumer identity and redact proxy failure targets

**Files:**
- Modify: `pkg/server/route_handler.go:59-64`
- Modify: `pkg/server/route_handler_test.go`
- Modify: `pkg/route/consumer_access_test.go`
- Modify: `pkg/route/builder.go:3394-3453`
- Modify: `pkg/route/coverage_helpers_test.go`

- [ ] **Step 1: Add forged identity tests**

At `serveRouteRequest` entry, seed `X-Consumer-Username: attacker`. For an unauthenticated route, upstream must see no header. For Basic/Key/JWT authenticated routes, upstream must see only the selected consumer username. Also cover a failed auth response so the forged value cannot appear in detached log state.

- [ ] **Step 2: Delete the ingress-owned identity once**

At the start of `serveRouteRequest`, before lifecycle creation and plugin execution:

```go
r.Header.Del("X-Consumer-Username")
request, lifecycle := apisixctx.EnsureRequestLifecycle(r, time.Now())
```

Do not add a trusted-client exception. Trusted proxies own forwarding metadata, not authenticated consumer identity.

- [ ] **Step 3: Add a query-secret proxy log regression**

Invoke `newErrorHandler` with a request like `/pay?access_token=secret&code=123`. Capture logger output and assert it contains the method/path and error class, but not `access_token`, `secret`, `code=`, or the full `RequestURI`.

- [ ] **Step 4: Log only the escaped path**

Add a private helper that returns `/` for a nil/empty URL and otherwise `r.URL.EscapedPath()`. Replace the current `r.URL.RequestURI()` argument:

```go
logger.Errorf("proxy request %s %s failed: %v", r.Method, proxyFailureLogPath(r), err)
```

Keep the client response generic.

### Task 6: Verify and commit

- [ ] **Step 1: Run normal, race, lint, and build gates**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/wolf_rbac ./pkg/plugin/openid_connect ./pkg/plugin/basic_auth ./pkg/plugin/ldap_auth ./pkg/server ./pkg/route -run "(SSLVerify|TLS|Cookie|Credential|Authorization|ConsumerUsername|ProxyErrorHandler|ProxyFailureLog)" -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/wolf_rbac ./pkg/plugin/openid_connect ./pkg/server ./pkg/route -run "(TLS|Cookie|ConsumerUsername|ProxyErrorHandler)" -count=3'
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 2: Commit the independent PR**

```bash
git add pkg/plugin/wolf_rbac pkg/plugin/openid_connect pkg/plugin/basic_auth pkg/plugin/ldap_auth \
  pkg/server/route_handler.go pkg/server/route_handler_test.go \
  pkg/route/builder.go pkg/route/coverage_helpers_test.go pkg/route/consumer_access_test.go \
  docs/plugins.md docs/superpowers/plans/2026-08-16-authentication-and-ingress-trust-hardening.md
git commit -m "fix(auth): harden credentials and ingress identity"
```
