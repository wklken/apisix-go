# Review Remediation Ledger

## Scope

- Verification report: `docs/report.md`
- Base: `master@54f09952fe290014f72da519d2557a80a5b543f0`
- Verified head: `54f09952fe290014f72da519d2557a80a5b543f0`
- Remediated head/diff: working tree
- Authorized IDs: `BUG-001`, `SEC-001`, `SEC-002`, `BUG-002`, `BUG-003`, `BUG-004`, `BUG-005`, `BUG-006`, `BUG-007`, `SEC-003`

## Status

| Finding ID | Verification verdict | Introduced by current PR | Scope | Status | Change/test evidence | Reason or next action |
| --- | --- | --- | --- | --- | --- | --- |
| BUG-001 | Partially correct | Not applicable | Authorized verified subclaim | fixed | Local ignored vendor state was regenerated and verified separately; tracked diff remained empty | No repository finding exists and no empty commit is created; retained for source-ledger completeness |
| SEC-001 | Correct | Not applicable | Authorized | fixed | Local JWT and introspection regression tests, manifest validation, 28 affected integration cases, scoped lint, package tests, and build passed | Locally verified JWTs now require a verifiable issuer and expected audience; configured audiences also apply to introspection responses |
| SEC-002 | Correct | Not applicable | Authorized | fixed | Default snapshot and legacy payload tests failed before the change, then all three logger packages and scoped lint passed | Loki, SLS, and Splunk default payloads omit sensitive request/response headers while preserving ordinary headers |
| BUG-002 | Correct | Not applicable | Authorized | pending | None | Queued: cache-capacity commit |
| BUG-003 | Correct | Not applicable | Authorized | pending | None | Queued: request-boundary commit |
| BUG-004 | Correct | Not applicable | Authorized | pending | None | Queued: typed resource-quarantine commit |
| BUG-005 | Correct | Not applicable | Authorized | pending | None | Queued after BUG-004: malformed route/global-rule isolation commit |
| BUG-006 | Correct | Not applicable | Authorized | pending | None | Queued: upstream-node default-port commit |
| BUG-007 | Correct | Not applicable | Authorized | pending | None | Queued: delayed-sync lifecycle commit |
| SEC-003 | Correct | Not applicable | Authorized | pending | None | Queued last: versioned AEAD migration commit |

## Changed Files

- `SEC-001`: `pkg/plugin/openid_connect/verify.go`
  - Fails closed when discovery is unavailable or omits its issuer.
  - Requires locally verified JWTs to match `client_id` by default.
  - Allows an explicit resource audience through `claim_validator.audience.valid_audiences`.
- `SEC-001`: `pkg/plugin/openid_connect/plugin.go`
  - Adds schema validation for non-empty `valid_audiences` arrays.
- `SEC-001`: `pkg/plugin/openid_connect/plugin_test.go`
  - Covers discovery failure, missing discovery issuer, wrong default audience, an accepted explicit resource audience, and configured audience validation for introspection responses.
- `SEC-001`: `t/plugin/openid-connect.yaml`
  - Gives static-key fixtures an explicit trusted issuer and refreshes affected signed tokens so the integration corpus exercises the new issuer/audience contract.
- `SEC-001`: `docs/plugins.md`
  - Documents the issuer and audience trust requirements for locally verified JWTs.
- `SEC-002`: `pkg/plugin/loki_logger/plugin.go`, `pkg/plugin/sls_logger/plugin.go`, `pkg/plugin/splunk_hec_logging/plugin.go`
  - Uses the shared access-log header sanitizer in each default snapshot and legacy payload builder; explicit custom log formats remain unchanged.
- `SEC-002`: the corresponding three `plugin_test.go` files
  - Covers request `Authorization`/`Cookie`, response `Set-Cookie`, and benign-header retention in both execution paths.

## Verification

- RED: the three fail-closed tests returned 204 before the fix; expected 401.
- RED: the explicit resource-audience test returned 401 before `valid_audiences`; expected 204.
- GREEN: `source .envrc && go test ./pkg/plugin/openid_connect -run '^TestHandlerRejectsStaticJWT(WhenDiscoveryUnavailable|WhenDiscoveryHasNoIssuer|ForWrongAudience)$' -count=1` passed.
- GREEN: `source .envrc && go test ./pkg/plugin/openid_connect -run '^TestHandlerAcceptsStaticJWTForConfiguredAudience$' -count=1` passed.
- RED/GREEN: temporarily removing `valid_audiences` enforcement made the introspection wrong/missing-audience cases return 204 instead of 403; after restoring the implementation, `source .envrc && go test ./pkg/plugin/openid_connect -run '^TestHandlerValidatesIntrospectedTokenAgainstConfiguredAudiences$' -count=1` passed.
- Package: `source .envrc && go test ./pkg/plugin/openid_connect -count=1` passed.
- Lint: `source .envrc && golangci-lint run ./pkg/plugin/openid_connect/...` passed with 0 issues.
- Manifest: `source .envrc && go test ./t/plugin -run '^(TestManifestCorpusValidates|TestSupportedPluginManifestSelection)$' -count=1` passed.
- Integration: the 28 static-key cases whose original fixtures used unreachable discovery without an issuer anchor passed as one selected `TestPluginIntegration/openid-connect/(...)` run.
- Build: `source .envrc && make build && make clean` passed.
- Independent review: the first pass found the 28 affected integration fixtures and missing introspection coverage; both were corrected and included in the final verification above. The follow-up review reported no Critical, Important, or Minor findings and concluded `Ready: yes`.
- RED: `TestDefaultLogFieldsRedactSensitiveHeaders`, `TestDefaultAccessLogFieldsRedactSensitiveHeaders`, and `TestDefaultEventsRedactSensitiveHeaders` exposed `authorization` in the default payloads before SEC-002.
- GREEN: `source .envrc && go test ./pkg/plugin/loki_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging -count=1` passed.
- Lint: `source .envrc && golangci-lint run ./pkg/plugin/loki_logger/... ./pkg/plugin/sls_logger/... ./pkg/plugin/splunk_hec_logging/...` passed with 0 issues.
- Not run: repository-wide tests, the full `t/plugin` integration suite, race tests, or a real external OIDC provider. The changed behavior has focused unit and selected real-process integration coverage and does not alter a concurrency path.

## Remaining Pending Work

`BUG-002`, `BUG-003`, `BUG-004`, `BUG-005`, `BUG-006`, `BUG-007`, and `SEC-003` remain pending in the dependency-ordered implementation plan. `BUG-001`, `SEC-001`, and `SEC-002` have no remaining implementation blocker.
