# UA Restriction Configuration Validation Implementation Plan

**Goal:** Close P1 5.7 by rejecting invalid allow/deny regular expressions during route compilation.

**Refresh baseline:** `71a2d564154c350bcda1d93cacf84b5bf678eaeb` after Plan 25 merged.

**Architecture:** Compile every configured expression in `PostInit` and return the first field/index-qualified error. Handler sees only fully compiled immutable slices.

**Tech Stack:** Go 1.26 regexp and existing plugin/route strict-build tests.

## Global Constraints

- One invalid expression rejects the whole plugin config; no warning-and-skip behavior remains.
- Error text includes `allowlist` or `denylist`, array index, and original pattern.
- Last-good route generation remains active after a bad reload.

### Task 1: Add failing configuration tests

- [x] Add allow/deny invalid regex tests, valid mixed rules, and strict Builder reload preservation.
- [x] Run `bash -lc 'source .envrc && go test ./pkg/plugin/ua_restriction ./pkg/route -run "UARestriction.*Regex" -count=1'` and observe the old warning-and-success behavior fail the regressions.

### Task 2: Return compile errors and verify

- [x] Replace warning/continue branches with field/index/pattern-qualified errors.
- [x] Run package tests, lint, build, then deliver:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/ua_restriction ./pkg/route -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/ua_restriction/... ./pkg/route/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [x] Review the final diff; delivery follows after the final staged-diff check.
