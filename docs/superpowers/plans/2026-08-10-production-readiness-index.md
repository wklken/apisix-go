# Production Readiness Remediation Plan Index

**Original audit baseline:** `bb7fab01e7b46964bfe6cd543959c981d28b49a5`

**Planning refresh baseline:** `6c94583a70d9260f54c13ba76da510a49f244f80` after Plans 01–10 merged.

**Source ledger:** `docs/production-readiness-review-2026-08-09.md`

**Phase capability source:** `docs/superpowers/plans/2026-08-10-plugin-capability-manifest.md` covers all 115 registered HTTP factory keys and 114 implementation identities consumed by Plans 12–17.

**Delivery contract:** Each row below is one independently reviewed pull request. Implementation starts from the then-current `master`; no PR silently depends on an unmerged sibling. If an earlier PR changes an interface consumed by a later plan, refresh that later plan against the new `master` before dispatch.

## Global execution rules

- Run Go commands from the repository root with `bash -lc 'source .envrc && ...'`.
- Add the failing behavior test before production changes and record the expected failure.
- Use the smallest credible affected-package tests. Run `make build` for code changes. Run focused race tests for concurrency or lifecycle changes.
- Run repository lint only after focused tests pass; do not substitute `go test ./...`, `make test`, or the whole `t/plugin` suite.
- Preserve unrelated untracked reports in the main checkout. Each implementation uses a dedicated worktree and a `codex/` branch.
- An implementation worker may edit only its assigned paths and run focused verification. It may not commit, push, open a PR, or delegate.
- The main agent inspects the actual diff and evidence and performs delivery. Plan 11 is the coordination-document owner and publishes this index, the phase umbrella, the capability manifest, and Plans 11–17 together. Each later implementation PR includes or refreshes only its own plan when needed; no later PR silently edits a sibling plan.

## Plan and finding coverage

| Order | Plan / PR | Findings | Primary paths | Depends on |
| --- | --- | --- | --- | --- |
| 01 | `2026-08-10-http-route-rewrite-expression-correctness.md` | PR-001, PR-002, PR-019 | `pkg/route`, `proxy_rewrite`, `expr` | none |
| 02 | `2026-08-10-authentication-boundary-hardening.md` | PR-005, PR-006, PR-025 | `openid_connect`, `wolf_rbac`, `basic_auth` | none |
| 03 | `2026-08-10-cors-external-authorization-input.md` | PR-026, PR-027 | `cors`, `opa`, `forward_auth`, request context | none |
| 04 | `2026-08-10-ai-safety-fail-closed.md` | PR-007 | Aliyun moderation, prompt guard | none |
| 05 | `2026-08-10-http-representation-integrity.md` | PR-008, PR-009, P1 5.4 | cache, gzip, body transforms, base helper | none |
| 06 | `2026-08-10-stream-forwarding-and-startup.md` | PR-003, PR-004, PR-015 | MQTT, stream router/runtime, server | none |
| 07 | `2026-08-10-store-snapshot-and-etcd-ack.md` | PR-010, PR-018 | store, etcd watcher, server sync | none |
| 08 | `2026-08-10-standalone-store-lifecycle.md` | PR-011 | standalone watcher, server shutdown, store | none |
| 09 | `2026-08-10-logger-batch-resource-bounds.md` | PR-012 | logger batch, base defaults, metrics | none |
| 10 | `2026-08-10-http-plugin-allowlist.md` | PR-013 | config, route builder, plugin factory | none |
| 11 | `2026-08-10-plugin-panic-outcome-foundation.md` | P1 5.5 foundation | request lifecycle, outcome writer, route panic boundary | 10 |
| 12 | `2026-08-10-plugin-request-phase-bridge.md` | PR-014 foundation | mixed request executor, request-context/id, limit-conn | 11 |
| 13 | `2026-08-10-plugin-scoped-rewrite-executor.md` | PR-014 partial | system/global/route rewrite ordering | 12 |
| 14 | `2026-08-10-plugin-consumer-access-executor.md` | PR-014 partial | auth handoff, consumer merge/rewrite, access | 13 |
| 15 | `2026-08-10-plugin-buffered-response-phases.md` | PR-014 partial | bounded header/body phases, cache store | 14 |
| 16 | `2026-08-10-plugin-streaming-terminal-phases.md` | PR-014 partial | compression, streaming, protocol, terminal/hijack | 15 |
| 17 | `2026-08-10-plugin-log-finalizer-phases.md` | PR-014, P1 5.5 closure | log/tracer finalizers, capability completeness | 16 |
| 18 | `2026-08-10-frontend-and-upstream-tls.md` | PR-016, PR-022, P1 5.10 | config, frontend TLS, cluster transport | none |
| 19 | `2026-08-10-auth-session-lifecycle.md` | PR-017, P1 5.14 | Casdoor, OpenID sessions, shared session owner | 02 |
| 20 | `2026-08-10-request-data-contract.md` | PR-020, PR-021 | data-mask, logging view, request-validation | 17 |
| 21 | `2026-08-10-reachable-vulnerability-upgrades.md` | PR-023 | modules, vendor, SAML/gRPC/text tests | none |
| 22 | `2026-08-10-config-container-release-gates.md` | PR-024, P1 5.6 | config merge, Dockerfile, CI, docs | 10, 18 |
| 23 | `2026-08-10-rate-limit-state-correctness.md` | P1 5.1 | limit-req/conn/count, AI rate limiting | none |
| 24 | `2026-08-10-body-and-task-resource-bounds.md` | P1 5.2, P1 5.13 | shared body limit, mirror/split/MCP, batch requests | 09 |
| 25 | `2026-08-10-ai-streaming-and-provider-parity.md` | P1 5.3, P1 5.12 | AI multi, endpoint, balancer, AWS moderation | 04 |
| 26 | `2026-08-10-ua-restriction-config-validation.md` | P1 5.7 | ua-restriction | none |
| 27 | `2026-08-10-production-metrics-and-request-logging.md` | P1 5.8, P1 5.15 | metrics, node-status, logger/trace snapshots | 09, 17 |
| 28 | `2026-08-10-oas-and-secret-resolution-boundaries.md` | P1 5.9, P1 5.18 | OAS loader, secret materialization, affected plugins | 10 |
| 29 | `2026-08-10-grpc-transport-and-transcode-contract.md` | P1 5.11, P1 5.16 | cluster HTTP/2 transport, grpc-transcode | 18 |
| 30 | `2026-08-10-faas-progress-timeout.md` | P1 5.17 | function upstream and FaaS owners | 18 |

## Coverage check

- P0 PR-001 through PR-027 are each assigned to one closure owner. PR-014 is deliberately delivered through Plans 12–17 and is marked closed only by Plan 17.
- P1 5.1 through 5.18 each have one closure owner. P1 5.5 has a foundation in Plan 11 and is marked closed only by Plan 17; P1 5.4, 5.6 and 5.10 join the PR that owns the same protocol invariant.
- Historical false positives, dependency-removal candidates, and benchmark-gated route suffix optimization remain excluded because the source ledger explicitly classifies them outside the production blocker scope.

## Dispatch order

Plans 01–10 are merged. The default next sequence is the strict Plan 11→17 phase chain. Plans 18, 21, 23, and 26 have no dependency on that chain and may be preflighted in parallel, but only one plan is in delivery at a time. Up to three `implementation-worker` agents may work within one active plan when their paths are exclusive and fixed interfaces are already accepted.
