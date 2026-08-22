# Convergence Round 2 Implementation Plan

> For agentic workers: execute only the explicitly assigned task; no security fixes, commits, pushes, or PR actions.

**Goal:** Make one malformed configuration resource non-fatal to the rest of the configuration generation, close confirmed reliability gaps, and add the missing `t/plugin` and stream race CI coverage.

**Architecture:** Keep whole-file syntax and section-shape failures fatal, but isolate resource-scoped failures. Preserve the last-good stored value when a malformed standalone resource has a stable identity, quarantine malformed legacy store rows during snapshot construction, and keep route/global generation-wide decisions unchanged. Security and trust-boundary findings are documented only.

**Tech Stack:** Go 1.26, bbolt, chi, Viper standalone configuration, GitHub Actions, shell contract tests.

**Spec:** A malformed resource must not prevent valid sibling resources from being stored and published. If the malformed resource previously had a valid value and its identity is recoverable, that last-good value remains active. Quarantine counts must keep readiness degraded until the resource becomes valid. Whole-file parse errors, invalid section containers, global-rule policy, and stream-route last-good semantics remain outside this implementation.

**Global Constraints:** Source `.envrc` before every Go command. Write regression tests before production changes. Run one real-process `t/plugin` command at a time. Do not modify or remediate any item marked `security-deferred` in `docs/reviews/convergence-decisions.md`.

## Task 1: Reject new malformed dependent resources and quarantine legacy rows

**Files:**
- Modify: `pkg/store/getter.go`
- Test: `pkg/store/durable_apply_test.go`
- Test: `pkg/store/config_snapshot_test.go`

**Interfaces:** Extend the existing internal `validateConfigResourcePut(bucket, id, config)` dispatch; do not add a new public API.

- [ ] Add failing acknowledged-event/batch tests proving malformed `services`, `upstreams`, and `plugin_configs` are rejected before the durable write while valid last-good values remain.
- [ ] Add failing snapshot tests that seed malformed legacy rows directly in bbolt and expect them in `QuarantinedResources()` while valid sibling rows remain available.
- [ ] Run the exact new tests and capture the red failures.
- [ ] Extend write validation with the existing `ParseService`, `ParseUpstream`, and `ParsePluginConfigRule` functions.
- [ ] Change snapshot construction for those three buckets from generation-fatal decode errors to resource quarantine and continue.
- [ ] Run the exact tests, then `go test ./pkg/store -count=1`.

## Task 2: Isolate malformed standalone resources and preserve last-good values

**Files:**
- Modify: `pkg/config/standalone.go`
- Test: `pkg/config/standalone_test.go`
- Modify: `pkg/server/server.go`
- Test: `pkg/server/server_test.go` or `pkg/server/reload_test.go`

**Interfaces:** Add resource quarantine details to `StandaloneReloadResult`. Keep `Reload()` and `ReloadSnapshot()` signatures stable. Continue using `store.BatchOptions.Preserve` for last-good retention.

- [ ] Add failing YAML and JSON tests with one invalid resource and one valid sibling; assert reload succeeds and the valid sibling is applied.
- [ ] Add a failing replacement test where a malformed resource with a recoverable ID retains its previous durable value while unrelated additions/deletions converge.
- [ ] Add a failing initial-load test proving an invalid resource does not prevent startup/reload success.
- [ ] Add a failing server metric test proving provider quarantine count reflects the result and clears after a fully valid reload.
- [ ] Run the exact tests and capture the red failures.
- [ ] Parse file/container syntax fail-closed, but collect resource-level normalization failures instead of returning immediately.
- [ ] Preserve recoverable invalid identities, omit unidentified invalid resources, and retry a store-rejected authoritative batch without rejected mutations while preserving their keys.
- [ ] Publish the resulting quarantine count through the existing provider quarantine metric.
- [ ] Run the exact tests, then `go test ./pkg/config ./pkg/server ./pkg/store -count=1` and the focused race gate for these packages.

## Task 3: Prevent route-registration panics from duplicate or non-APISIX methods

**Files:**
- Modify: `pkg/route/builder.go`
- Test: `pkg/route/builder_lifecycle_test.go`

**Interfaces:** Keep `BuildWithRouteQuarantine()` unchanged; extend `validateRouteSemantics` only.

- [ ] Add failing tests for duplicate parameterized URIs, duplicate methods, lowercase methods, and unsupported `QUERY`; assert the bad route is quarantined and a valid sibling remains published.
- [ ] Run the exact tests and capture the red failure or panic.
- [ ] Enforce APISIX 3.17's exact uppercase method set and uniqueness of `methods` and effective `uris` before chi registration.
- [ ] Run the exact tests, then `go test ./pkg/route -count=1`.

## Task 4: Keep cluster observer identity aligned with the registered upstream

**Files:**
- Modify: `pkg/proxy/cluster.go`
- Test: `pkg/proxy/cluster_test.go`
- Test: `pkg/route/upstream_options_test.go`

**Interfaces:** Add `Name` to the private `clusterKeyIdentity`; no registry API changes.

- [ ] Replace the existing label-only expectation with a failing test that distinct names produce distinct keys.
- [ ] Add or extend a registry test proving separately named but otherwise identical clusters retain the correct observer label.
- [ ] Run the exact tests and capture the red failure.
- [ ] Include `ClusterConfig.Name` in the deterministic key identity and update comments.
- [ ] Run the exact tests, then `go test ./pkg/proxy ./pkg/route -count=1`.

## Task 5: Bound AI moderation denial status codes

**Files:**
- Modify: `pkg/plugin/ai_aws_content_moderation/plugin.go`
- Modify: `pkg/plugin/ai_aliyun_content_moderation/plugin.go`
- Test: the existing schema tests in each package

**Interfaces:** No Go API change; restrict `deny_code` to integer HTTP status codes `100..599`.

- [ ] Add failing schema tests for `99`, `600`, and a fractional Aliyun value.
- [ ] Run the exact tests and capture the red failures.
- [ ] Add `minimum`, `maximum`, and integer type constraints to both schemas.
- [ ] Run both plugin package tests.

## Task 6: Make exact HTTP fixture assertions observe shutdown-time extras

**Files:**
- Modify: `t/plugin/runner_test.go`
- Test: `t/plugin/fixture_network_test.go` or the existing runner tests

**Interfaces:** Extend the private `fixtureAssertionAfterShutdown` policy; manifest format remains unchanged.

- [ ] Add a failing test proving exact HTTP expectations and explicit zero HTTP expectations are classified for post-shutdown assertion.
- [ ] Run the exact test and capture the red failure.
- [ ] Move exact HTTP fixture assertions to the existing post-shutdown phase; keep count-range assertions on their bounded observation window.
- [ ] Run focused harness tests covering exact, zero, unordered, and count-range fixtures.

## Task 7: Add non-integration `t/plugin` CI coverage and stream race coverage

**Files:**
- Modify: `t/plugin/runner_test.go`
- Modify: `Makefile`
- Modify: `.github/workflows/unit-test.yml`
- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `scripts/release_gate_test.sh`
- Add or modify: focused workflow/Makefile contract test if an existing owner exists

**Interfaces:** Add `APISIX_GO_SKIP_PLUGIN_INTEGRATION=1` as a test-only switch and `make test-plugin-harness` as the supported local/CI entrypoint.

- [ ] Add a failing test or shell contract proving the CI workflow invokes the new harness target and the release race gate includes `./pkg/stream`.
- [ ] Run the focused contract tests and capture the red failures.
- [ ] Make `TestPluginIntegration` skip only when the explicit test-only environment variable is set.
- [ ] Add `test-plugin-harness` and run it from the unit workflow without replacing the four real-process smoke cases.
- [ ] Add `./pkg/stream` to the focused release race command and update its exact shell contract.
- [ ] Run `make test-plugin-harness`, the release gate contract test, and `go test -race ./pkg/stream -count=1`.

## Task 8: Consolidated verification and delivery

**Files:** All files changed by Tasks 1-7 plus `docs/reviews/convergence-decisions.md`.

- [ ] Format only touched Go files and inspect the resulting diff for unrelated rewrites.
- [ ] Run all exact regression tests, then impacted package tests and focused race gates.
- [ ] Run `source .envrc && make lint`, `source .envrc && make build`, and the repository-wide unit gate authorized by the convergence request.
- [ ] Run `git diff --check` and inspect every changed line against this plan.
- [ ] Obtain one independent read-only merge-level review; fix only validated non-security findings.
- [ ] Commit one coherent change, push the `codex/` branch, open a PR, wait for all GitHub Actions checks, and merge only after green CI.
- [ ] Refresh `origin/master` and begin the next review round.
