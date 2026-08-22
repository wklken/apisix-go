# Convergence Round 3 Implementation Plan

Base revision: `dc98d351ead73063627db6d1e51a4aaeea2c83fd`

Goal: close the remaining confirmed non-security P1/P2 correctness gaps without changing any network-security or trust-boundary behavior. Security findings remain record-only in `docs/reviews/convergence-decisions.md`.

## Task 1: Reject unknown standalone sections and close the watcher registration gap

Files:

- `pkg/config/standalone.go`
- `pkg/config/standalone_test.go`
- `pkg/server/server.go`

Steps:

1. Add a regression test that loads a valid route, replaces the authoritative file with a misspelled root section such as `routs`, and proves the reload fails while the last-good route remains in Store.
2. Add YAML and JSON decoder cases proving unknown root sections are rejected deterministically; keep `{}` as the explicit valid empty authoritative snapshot.
3. Validate every root key against `standaloneBuckets` before building mutations.
4. Add a watcher lifecycle API that registers fsnotify and then synchronously reconciles once. Keep `Start()` registration-only for existing cancellation and blocked-send tests.
5. Convert the existing pre-registration update test to exercise the new API and change production Server startup to use it after the acknowledged callback is installed.
6. Run:
   - `source .envrc && go test ./pkg/config -run 'TestStandalone(RejectsUnknownRootSectionWithoutDeletingLastGood|StartAndReconcileClosesRegistrationGap|SnapshotDecodeFailures)' -count=1`
   - `source .envrc && go test ./pkg/config ./pkg/server -count=1`

## Task 2: Preserve stream upstream weight semantics and Kafka IPv6 addresses

Files:

- `pkg/stream/router.go`
- `pkg/stream/router_test.go`
- `pkg/route/builder.go`
- the existing focused Kafka route test file

Steps:

1. Add stream router tests proving an explicitly configured zero-weight node is never selected, a negative weight is rejected, and an all-zero upstream is rejected for both RR/chash construction paths where relevant.
2. Apply the same weight contract already used by HTTP upstreams: omitted weight defaults to one, explicit zero disables the node, negative values fail, and at least one positive node is required.
3. Add a Kafka PubSub builder test that captures an IPv6 broker and expects `kafka://[::1]:9092`.
4. Build Kafka broker URLs through `net.JoinHostPort`/the existing normalized upstream host helper.
5. Run:
   - `source .envrc && go test ./pkg/stream -run 'Test.*Weight' -count=1`
   - `source .envrc && go test ./pkg/route -run 'Test.*Kafka.*IPv6' -count=1`

## Task 3: Align Dubbo/http-dubbo retry target selection with HTTP

Files:

- `pkg/route/builder.go`
- `pkg/route/dubbo_proxy_test.go`
- `pkg/route/http_dubbo_test.go`

Steps:

1. Add red tests for the default `nodes-1` retry count, priority fallback through request-aware target selection, and traffic-split retry advancement.
2. Store `httpRetryCount(upstream)` in both Dubbo terminal descriptors instead of the raw unset value.
3. Select ordinary retry targets through `pxy.NextTarget` so per-request priority state advances.
4. For a traffic-split override, use its retry budget and advance through `Override.NextRetry` after the first failed attempt; preserve health reporter and selected-target attribution.
5. Keep the existing safe retry boundary: only failures before request bytes are written may retry.
6. Run:
   - `source .envrc && go test ./pkg/route -run 'TestServe(Dubbo|HTTPDubbo).*Retry' -count=1`
   - `source .envrc && go test ./pkg/route ./pkg/proxy -count=1`

## Task 4: Contain batch panics and validate plugin response statuses

Files:

- `pkg/plugin/batch_requests/plugin.go`
- `pkg/plugin/batch_requests/plugin_test.go`
- `pkg/plugin/mocking/plugin.go` and focused tests
- `pkg/plugin/uri_blocker/plugin.go` and focused tests
- `pkg/plugin/acl/plugin.go` and focused tests
- `pkg/plugin/consumer_restriction/plugin.go` and focused tests
- `pkg/plugin/redirect/plugin.go` and focused tests
- `pkg/plugin/fault_injection/plugin.go` and focused tests
- `pkg/plugin/serverless/plugin.go` and focused tests
- `pkg/plugin/exit_transformer/plugin.go` and focused tests

Steps:

1. Add a subprocess regression proving a normal panic in a batch subrequest does not terminate the process and produces a bounded 502 pipeline response.
2. Recover every batch worker panic locally, log the stack, and convert it to the existing abort response; never re-panic from the detached worker goroutine.
3. Add `maximum: 599` to all six static status-code schemas and focused schema tests rejecting `1000`.
4. Add `minItems: 1` to `uri-blocker.block_rules` and make `PostInit` reject an empty list so direct construction cannot compile the empty regexp.
5. Make the serverless and exit-transformer Lua status converters accept only integral values in `100..599`; invalid numeric/string values must not replace the previous/default status.
6. Run the focused package tests, then:
   - `source .envrc && go test ./pkg/plugin/batch_requests ./pkg/plugin/mocking ./pkg/plugin/uri_blocker ./pkg/plugin/acl ./pkg/plugin/consumer_restriction ./pkg/plugin/redirect ./pkg/plugin/fault_injection ./pkg/plugin/serverless ./pkg/plugin/exit_transformer -count=1`

## Task 5: Make Release/RC execute the harness and fail closed on missing smoke cases

Files:

- `Makefile`
- `t/plugin/runner_test.go`
- focused `t/plugin` harness tests
- `.github/workflows/release.yml`
- `.github/workflows/release-candidate.yml`
- `scripts/release_gate_test.sh`
- `docs/reviews/convergence-decisions.md`

Steps:

1. Add an exact smoke selector owned by `TestPluginIntegration`: when `APISIX_GO_PLUGIN_SMOKE_CASE` is set, run exactly the matching `<plugin>/<case>` and fail if the manifest case does not exist.
2. Add `make test-plugin-smoke PLUGIN_SMOKE_CASE=...`, using an exact top-level `-run` expression and rejecting an empty selector.
3. Change Release and RC smoke matrices to use that target, and run `make test-plugin-harness` in both build-and-unit jobs.
4. Extend `release_gate_test.sh` so removal of either harness gate or fail-closed smoke command turns the contract test red.
5. Add `make test-plugin-harness` and the exact smoke commands to release qualification evidence.
6. Update `ARCH-04` to describe the already-implemented post-shutdown exact HTTP assertion; retain only the residual settling-window decision.
7. Run:
   - `source .envrc && make test-plugin-harness`
   - `source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE=uri-blocker/one-rule-blocks-query`
   - a missing-case invocation and confirm it fails
   - `bash scripts/release_gate_test.sh`

## Task 6: Final verification and delivery

1. Inspect every worker diff and independently verify each finding against the final call chain.
2. Run `golangci-lint fmt` only on touched Go files and discard unrelated formatting.
3. Run focused tests, the affected package race gate, `make test-plugin-harness`, `make lint`, `make build`, `make test`, `make test-cover`, and `git diff --check`.
4. Request one independent read-only merge review.
5. Commit, push, open a PR against `master`, follow GitHub Actions with `$gh-fix-ci`, repair only confirmed in-scope failures, and squash-merge after all required checks pass.
