# Convergence Round 6 Implementation Plan

## Baseline and scope

- Base revision: `456186e88e308fa1742cfc72266ffe11c0873cef` (`origin/master` after PR #153).
- Goal: remediate the two newly confirmed non-security P2 findings from the clean merged-master review, then repeat focused verification, independent review, PR CI, merge, and post-merge review.
- Security boundary: network-security and remote trust-boundary findings remain record-only. This round does not change TLS, certificate, upstream-response trust, schemas, or network validation.
- Architecture boundary: ARCH-01, ARCH-02, and ARCH-05 remain explicit design decisions because changing them can fail open generation-wide controls or requires stream ownership/delete semantics. They are not modified in this narrow P2 round.
- No dependency changes or unrelated refactors.

## Confirmed findings and acceptance contracts

### R6-P2-01: active health observer notifications can be published out of state order

The active checker currently changes target health under the load-balancer lock, releases the lock, and then calls `SetHealth`. A passive transition can publish the opposite state in that gap, after which the delayed active notification overwrites metrics with stale state.

Move the active generation-checked transition and its observer notification into one load-balancer critical section. Preserve generation validation and transition-only notification behavior. Do not broaden observer or public `MarkHealthy` / `MarkUnhealthy` contracts.

Regression proof: block the active transition's observer callback, concurrently attempt the opposite passive transition, and prove that the passive transition cannot publish until the active notification completes; final observer state must equal the load balancer state for both healthy and unhealthy directions.

### R6-P2-02: proxy-mirror generation retirement leaves idle transports open

`proxy-mirror.Stop` cancels and waits for admitted requests but does not close idle connections owned by its HTTP and h2c clients. The ordinary transport retains them until its timeout; the h2c HTTP/2 transport can retain them indefinitely.

After `mirrorWG.Wait` and before signalling `mirrorStopDone`, call `CloseIdleConnections` on both clients when present. Preserve idempotent concurrent `Stop` semantics and do not close transports before in-flight requests finish.

Regression proof: install close-recording transports for both clients, call `Stop` twice, and assert each transport is closed exactly once after outstanding work has drained.

## Work units and ownership

1. Health owner: `pkg/proxy/active_health.go`, `pkg/proxy/active_health_test.go`, `pkg/proxy/health.go`, and only directly required health tests.
2. Mirror owner: `pkg/plugin/proxy_mirror/plugin.go` and `pkg/plugin/proxy_mirror/plugin_test.go`.
3. Main owner: this plan, integration review, broad gates, delivery, CI monitoring, merge, and post-merge review.

Workers may implement test-first and run focused tests only. They must not touch another owner's files, modify dependencies, commit, push, open or merge a PR, or change record-only security/architecture behavior.

## Verification gates

1. Deterministic red-then-green regression for each finding.
2. `go test ./pkg/proxy -count=1` and `go test -race ./pkg/proxy -count=1`.
3. `go test ./pkg/plugin/proxy_mirror -count=1` and its focused race test.
4. Call-site/dead-code scan for changed private helpers; verify public health APIs remain intact.
5. `make lint`, `make build`, `make test`, `make test-cover`, and `git diff --check`.
6. Independent merge-level review; repair every confirmed non-security P0/P1/P2.
7. Commit, push, PR, GitHub Actions monitoring, merge, then another clean merged-master three-domain review.
