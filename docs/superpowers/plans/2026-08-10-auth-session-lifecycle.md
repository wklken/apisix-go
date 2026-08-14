# Authentication Session Lifecycle Implementation Plan

> **Execution:** Use `superpowers:executing-plans` and strict test-first cycles. This Plan is one independent PR.

**Goal:** Close PR-017 and P1 5.14 with reload/instance-independent Casdoor state and finite OpenID session lifetimes.

**Architecture:** Replace Casdoor's plugin-instance map with an AES-GCM authenticated stateless cookie. The existing required `client_secret` is the primary cookie key; optional `client_secret_fallbacks` decrypt cookies issued before a secret rotation. The cookie envelope contains version, issue/expiry times, a non-secret configuration fingerprint, and plugin payload. OpenID receives finite session defaults, one injectable clock for session lifecycle calculations, and a fail-closed JOSE algorithm allowlist matching the installed `go-oidc` verifier.

**Compatibility decisions:**

- Do not add a second required Casdoor `session_secret`; instead require the existing primary and fallback secrets to contain at least 32 characters before they can protect authentication cookies. Existing shorter secrets fail closed and must be rotated.
- The Casdoor fingerprint covers `endpoint_addr`, `client_id`, and `callback_url`, but excludes primary/fallback secrets so planned rotation remains valid.
- `client_secret_fallbacks` are decrypt-only; token exchange always uses the current `client_secret`.
- Casdoor cookies over 3800 encoded bytes fail closed. Payloads are never logged.
- OpenID defaults are idle 3600s, rolling 86400s, and absolute 604800s. Explicit positive values override; schema rejects provided non-positive values.
- Supported explicit JWT algorithms are the asymmetric algorithms supported by the installed `go-oidc`: RS256/384/512, ES256/384/512, PS256/384/512, and EdDSA. `none`, HMAC algorithms, and unknown values fail during `PostInit` and schema validation.
- This Plan does not add APISIX fields that the Go implementation cannot honor. The root OpenID schema rejects unknown fields, including the documented unsupported APISIX fields, instead of silently ignoring security-relevant settings.

## Task 1: Build the encrypted rotating session codec

- [x] Add behavior tests in `pkg/plugin/base/oauth_session_test.go` for nonce uniqueness, tamper rejection, expiry boundary, fingerprint mismatch, fallback rotation, unknown version, and the 3800-byte limit.
- [x] Add `SealOAuthSession` and `OpenOAuthSession` in `pkg/plugin/base/oauth_session.go`. Use a random 12-byte GCM nonce and SHA-256-derived AES-256 keys. The primary key encrypts; primary then fallbacks decrypt.
- [x] Run the focused base test red, then green.

## Task 2: Migrate Casdoor to stateless sessions

- [x] Add tests in `pkg/plugin/authz_casdoor/plugin_test.go` proving login-start cookie -> second plugin instance callback, authenticated cookie -> second instance, fallback-key rotation, fingerprint mismatch, expiry, tamper, and oversize-token failure.
- [x] Add `client_secret_fallbacks` to schema/config and secret-field handling; reject primary or fallback secrets shorter than 32 characters in schema and `PostInit`.
- [x] Remove the bounded session map, cleanup goroutine, and their tests. Store login state and authenticated token state in the encrypted cookie with separate expiries.
- [x] Keep current callback errors/statuses and cookie security controls. Cookie encoding failure must return a stable error without setting a partial cookie.
- [x] Run focused and full Casdoor tests, including race.

## Task 3: Enforce finite OpenID sessions and algorithms

- [x] Add tests in `pkg/plugin/openid_connect/plugin_test.go` for finite defaults, positive overrides, deprecated cookie lifetime precedence, invalid timeout schema values, supported algorithms, and `none`/HMAC/unknown rejection.
- [x] Add fake-clock cookie and Redis TTL boundary tests. All session flow/lifetime calculations use `Plugin.now`; production defaults it to `time.Now`.
- [x] Apply the three finite defaults only for non-bearer session flow. A positive deprecated `session.cookie.lifetime` continues to populate absolute timeout only when explicit `absolute_timeout` is absent.
- [x] Validate `token_signing_alg_values_expected` in schema and `PostInit`, and reject token algorithms outside the same allowlist before verifier construction.
- [x] Update `docs/plugins.md` with exact finite-session behavior, root-level unknown-field rejection, and remaining APISIX 3.17 field gaps.

## Task 4: Verify and review

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin/authz_casdoor ./pkg/plugin/openid_connect -run "(OAuthSession|CasdoorSession|SessionDefaults|SessionExpiry|SessionTTL|Algorithm)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin/authz_casdoor ./pkg/plugin/openid_connect ./pkg/route -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/base ./pkg/plugin/authz_casdoor ./pkg/plugin/openid_connect ./pkg/route -run "(OAuthSession|CasdoorSession|Session|Reload|Issuer|Algorithm)" -count=3'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/base/... ./pkg/plugin/authz_casdoor/... ./pkg/plugin/openid_connect/... ./pkg/data_encryption/...'
bash -lc 'source .envrc && make build && make clean'
```

- [x] Run `rg` for removed Casdoor map/cleanup symbols and classify any remaining matches.
- [x] Run `git diff --check`, inspect exact scope, request independent review, and remediate all confirmed High/Medium findings before delivery.
