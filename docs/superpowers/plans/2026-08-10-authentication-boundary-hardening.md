# Authentication Boundary Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close PR-005, PR-006, and PR-025 by failing closed on constrained OpenID issuers and preventing client credentials or identity headers from crossing authentication boundaries.

**Architecture:** Each authentication plugin remains its own policy owner. OpenID marks tokens inactive when a configured issuer constraint is missing or mismatched; Wolf clears protected headers before remote verification and requires a complete identity; Basic Auth removes credentials immediately after parsing whenever hiding is enabled.

**Tech Stack:** Go 1.26, `net/http`, go-oidc, existing consumer store and plugin tests.

## Global Constraints

- Do not change behavior when OpenID has no configured issuer/discovery issuer constraint.
- Never trust inbound `X-UserId`, `X-Username`, or `X-Nickname` under the active Wolf prefix.
- A Wolf 200 response without non-empty `id` and `username` is an authentication failure, not anonymous success.
- `hide_credentials=true` removes `Authorization` before every success, anonymous fallback, and failure response after parsing.
- Run Go commands with `bash -lc 'source .envrc && ...'`.

---

## File Structure

- Modify `pkg/plugin/openid_connect/verify.go` and tests.
- Modify `pkg/plugin/wolf_rbac/plugin.go` and tests.
- Modify `pkg/plugin/basic_auth/plugin.go` and tests.

### Task 1: Enforce OpenID issuer constraints

**Files:**
- Modify: `pkg/plugin/openid_connect/verify.go:178-239`
- Test: `pkg/plugin/openid_connect/plugin_test.go`

**Interfaces:**
- Consumes: configured issuer list, discovery document issuer, decoded claims.
- Produces: `validateIssuer(payload map[string]any)` setting `active=false` for missing or mismatched constrained issuers.

- [ ] **Step 1: Add configured/discovery issuer matrix**

Extend `TestHandlerValidatesIssuerAgainstConfiguredIssuers` or add `TestValidateIssuerRequiresConfiguredIssuerClaim` with rows: configured list + missing `iss` rejected; configured list + matching accepted; configured list + mismatch rejected; discovery issuer + missing rejected; no constraint + missing accepted.

```go
payload := map[string]any{"active": true}
plugin.validateIssuer(payload)
if payload["active"] != false {
	t.Fatalf("active = %#v, want false", payload["active"])
}
```

- [ ] **Step 2: Run the focused failing test**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/openid_connect -run "(ValidatesIssuer|RequiresConfiguredIssuerClaim)" -count=1'`

Expected before implementation: constrained missing-issuer rows remain active.

- [ ] **Step 3: Make constraint presence explicit**

Resolve configured issuers first, then discovery issuer. When either produces a non-empty constraint, require a non-empty claim and exact membership/equality. Preserve no-constraint compatibility.

```go
if len(configured) > 0 {
	if issuer == "" || !slices.Contains(configured, issuer) {
		payload["active"] = false
	}
	return
}
if discovery.Issuer != "" && issuer != discovery.Issuer {
	payload["active"] = false
}
```

- [ ] **Step 4: Verify OpenID tests**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/openid_connect -count=1'`

### Task 2: Clear and require Wolf identity headers

**Files:**
- Modify: `pkg/plugin/wolf_rbac/plugin.go:130-171,306-337`
- Test: `pkg/plugin/wolf_rbac/plugin_test.go`

**Interfaces:**
- Consumes: configured Wolf header prefix and remote `userInfo`.
- Produces: sanitized request/response identity headers only after a complete successful identity response.

- [ ] **Step 1: Add forged-header and empty-identity regressions**

Create `TestHandlerClearsForgedIdentityHeaders` and `TestHandlerRejectsEmptyUserInfo`. Seed both request and response recorder headers with attacker values and configure the fake Wolf server to return status 200 with `{}`.

```go
for _, name := range []string{"X-UserId", "X-Username", "X-Nickname"} {
	request.Header.Set(name, "attacker")
}
```

Expected result: empty identity returns 500 or the plugin's stable authentication failure status, `next` is not called, and none of the protected request headers remains attacker-controlled.

- [ ] **Step 2: Run the focused failures**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/wolf_rbac -run "^(TestHandlerClearsForgedIdentityHeaders|TestHandlerRejectsEmptyUserInfo)$" -count=1'`

- [ ] **Step 3: Add one protected-header helper**

```go
func clearUserHeaders(r *http.Request, prefix string) {
	for _, suffix := range []string{"UserId", "Username", "Nickname"} {
		r.Header.Del(prefix + suffix)
	}
}
```

Call it before token/permission processing. Change `setUserHeaders` so empty `userInfo`, empty normalized `id`, or empty `username` returns an error. Only then set request and response headers.

- [ ] **Step 4: Verify Wolf behavior**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/wolf_rbac -count=1'`

### Task 3: Hide Basic credentials on anonymous fallback

**Files:**
- Modify: `pkg/plugin/basic_auth/plugin.go:110-174`
- Test: `pkg/plugin/basic_auth/plugin_test.go`

**Interfaces:**
- Consumes: parsed Basic header and `HideCredentials`.
- Produces: local username/password values for authentication while the request header is removed once parsed.

- [ ] **Step 1: Add the anonymous credential matrix**

Add `TestHandlerHideCredentialsOnAnonymousFallback` covering unknown user, wrong password, malformed consumer config, and valid anonymous consumer. In the downstream handler assert:

```go
if got := r.Header.Get("Authorization"); got != "" {
	t.Fatalf("Authorization = %q, want removed", got)
}
```

Also assert missing anonymous consumer fails without forwarding credentials.

- [ ] **Step 2: Run the focused test and observe leakage**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/basic_auth -run "(HideCredentials|Anonymous)" -count=1'`

- [ ] **Step 3: Delete the header after successful parse**

Move the deletion to immediately after `parseBasicAuthorization` succeeds:

```go
user, pass, err := parseBasicAuthorization(r.Header.Get("Authorization"))
if err != nil { /* existing response */ }
if *p.config.HideCredentials {
	r.Header.Del("Authorization")
}
```

Remove the later success-only deletion.

- [ ] **Step 4: Verify, lint, and build**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/openid_connect ./pkg/plugin/wolf_rbac ./pkg/plugin/basic_auth -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/openid_connect/... ./pkg/plugin/wolf_rbac/... ./pkg/plugin/basic_auth/...'
bash -lc 'source .envrc && make build'
```

- [ ] **Step 5: Commit the independent PR scope**

```bash
git add docs/superpowers/plans/2026-08-10-authentication-boundary-hardening.md \
  pkg/plugin/openid_connect/verify.go pkg/plugin/openid_connect/plugin_test.go \
  pkg/plugin/wolf_rbac/plugin.go pkg/plugin/wolf_rbac/plugin_test.go \
  pkg/plugin/basic_auth/plugin.go pkg/plugin/basic_auth/plugin_test.go
git commit -m "fix(auth): harden identity and credential boundaries"
```
