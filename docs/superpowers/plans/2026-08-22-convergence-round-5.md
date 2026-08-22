# Convergence Round 5 Implementation Plan

## Baseline and scope

- Base revision: `820d2ecffa1a27776d043bdd37d7e580143921f7` (`origin/master` after PR #152).
- Goal: remediate every newly confirmed non-security P2 from the clean merged-master review and repeat the delivery/CI/review loop.
- Security boundary: network-security and remote trust-boundary findings are recorded only. This round does not change Chaitin WAF, OpenWhisk, Dubbo, TLS, certificate, or network-response trust behavior.
- No dependency changes or unrelated refactors.

## Confirmed findings and acceptance contracts

### R5-P2-01: active probe results are not bound to the probe-start generation

Capture the target health generation immediately before each active probe. Apply the result only if that generation is still current. The final healthy/unhealthy mark must compare the expected generation under the load balancer lock so a passive transition between validation and marking cannot admit a stale result. A discarded stale result resets active counters and emits no observer transition.

Regression proof: block an active failure in the transport, perform passive quarantine then passive recovery, release the stale failure, and assert that the recovered target stays healthy with no second unhealthy notification.

### R5-P2-02: exhausted traffic-split retries double-report one transport failure

When a traffic-split retry callback has no `NextRetry` or the callback returns no next override, clear the request's selected health target before returning false. This prevents the ReverseProxy terminal error handler from reporting the already-recorded final attempt again.

Regression proof: a one-node traffic-split override with one configured retry and an always-failing transport reports exactly one TCP failure after the terminal error handler runs.

### R5-P2-03: workflow accepts informational status codes as terminal responses

Validate workflow `return.code` as an integral HTTP terminal status from 200 through 599. Update unit coverage for 100, 103, 199, 200, and 599, and update the exact integration diagnostic from `100 and 599` to `200 and 599`.

Regression proof: 1xx values fail `PostInit`; 200 and 599 remain accepted; the focused workflow manifest case observes the updated validation message.

### R5-P2-04: CI contract scripts have no CI caller

Run `scripts/release_gate_test.sh` from the CI build/unit job and `scripts/plugin_status_gate_test.sh` from the plugin-status job. Extend both scripts so they assert their own workflow invocation, making removal fail closed. Keep workflow triggers aligned with the scripts and modified workflow files.

Regression proof: both contract scripts and `actionlint` pass; removing either workflow invocation makes its owning contract script fail.

## Record-only security item

Add a security-deferred ledger entry for network-provided terminal status values in Chaitin WAF, OpenWhisk, and Dubbo. Do not change plugin validation, transports, response writing, or schemas for those paths.

## Work units and ownership

1. Health owner: `pkg/proxy/active_health.go`, `pkg/proxy/active_health_test.go`, `pkg/proxy/health.go`, `pkg/proxy/health_test.go`.
2. Route owner: `pkg/route/builder.go` and focused route retry/health tests.
3. Workflow owner: `pkg/plugin/workflow/plugin.go`, `pkg/plugin/workflow/plugin_test.go`, `t/plugin/workflow.yaml`.
4. Main owner: `.github/workflows/unit-test.yml`, `.github/workflows/plugin-status.yml`, `scripts/release_gate_test.sh`, `scripts/plugin_status_gate_test.sh`, `docs/reviews/convergence-decisions.md`, and this plan.

Workers may implement and run focused tests only. They must not commit, push, open a PR, modify dependencies, or touch another owner's files.

## Verification gates

1. Red-then-green exact regression tests for all four findings.
2. Affected package tests and focused race tests for proxy and route.
3. Exact workflow plugin smoke, plugin harness, plugin-status target, and absent-selector fail-closed checks.
4. Both contract scripts and `actionlint` for modified workflows.
5. `make lint`, `make build`, impact-scoped tests, `make test`, `make test-cover`, and `git diff --check`.
6. Dead-code/proxy-only call-site scan for new helpers and conditional mark methods.
7. Independent merge-level review; repair confirmed non-security findings only.
8. Commit, push, PR, GitHub Actions monitoring, merge, then another clean merged-master review.

## Verification-discovered test repair

- `workflow/workflow-schema-diagnostics` used the invalid route itself as the reload probe. The first invalid update could satisfy the probe from the prior generation before the new generation removed the quarantined route, causing every later step to wait for a stale 201 and fail. Each invalid update now includes a uniquely addressed valid sibling route; the probe waits for that sibling's 202, while `/schema` must be 404 with the expected validation log. This directly verifies that one invalid route does not block valid sibling publication.
