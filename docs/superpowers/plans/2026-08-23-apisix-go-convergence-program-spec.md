# APISIX-Go Architecture Convergence Program Specification

## Purpose

This specification converts the 200 decisions confirmed in the 2026-08-22
architecture review into one implementation contract. It supersedes conflicting
parts of `docs/design.md`, `docs/plugins.md`, and
`docs/reviews/convergence-decisions.md`; those documents remain historical
evidence until the governance plan updates them explicitly.

The implementation baseline is `master` at
`99f18c53b8a61e734ef8e8f078665fb16d98f26d`, matching `origin/master` when this
specification was written. The compatibility oracle is Apache APISIX 3.17.0 at
`9ef2ecab67f652d38365049613610ef649bb4ad0`.

## Product Contract

- Preserve APISIX 3.17 configuration and externally observable behavior for
  Go-applicable HTTP data-plane features. Equivalent Go internals are preferred;
  OpenResty or NGINX implementation identity is not required.
- Keep the APISIX namespace compatibility-pure. Go-native extensions use a
  separately versioned namespace and may intentionally diverge only through an
  explicit ADR approved by the project owner.
- Finish and qualify the APISIX 3.17 HTTP data plane before broad stream parity.
  HTTP/3, QUIC, Lua `filter_func`/script execution, external plugin runner
  compatibility, and `inspect` remain explicit gaps rather than `Full` or `N/A`.
- Freeze new non-compatibility extensions until the APISIX 3.17 convergence
  gaps and evidence model are closed.
- Do not claim production readiness until the exact release artifact passes the
  compatibility, capacity, dependency, recovery, security, platform, upgrade,
  rollback, and provenance gates in this program.

## Configuration and State Contract

- Replace monolithic `deployment.profile` semantics with three orthogonal
  dimensions: `compatibility_target=apisix-3.17`,
  `security_profile=compat|strict`, and
  `qualification_profile=http-data-plane-v1`.
- Use a presence-aware merge pipeline that records the source of every effective
  value. Distinguish absent, `null`, `false`, zero, and empty string. Preserve
  exact integers and `json.Number`; do not round-trip identifiers or revisions
  through `float64`.
- Implement upstream APISIX environment semantics in the APISIX namespace.
  Keep `APISIXGO_*` as an explicit Go extension layer with documented empty and
  missing value behavior.
- Remove mutable process globals such as `config.GlobalConfig` and global data
  encryption configuration. Pass immutable runtime dependencies explicitly.
- The supervisor is the single writer for desired and published state. Persist
  schema version, migrations, integrity metadata, desired revision, published
  HTTP revision, published stream revision, and artifact identity in bbolt.
- Persist last-good published state across restart. Offline startup may serve it,
  but readiness remains degraded or unavailable until the authoritative provider
  is healthy.
- Publication is a reversible two-boundary transition: prepare owns the new
  generation without exposing it, activation swaps the in-memory active target
  reversibly, and only a successful journal commit makes it published. A failed
  stage discards the unexposed prepared generation; a failed activation or
  journal commit restores the old active generation and closes the new one. Only
  post-commit finalization may mark the old generation retiring; fallible drain
  and termination run afterward in the supervisor loop.
- Explicit delete is authoritative and never restored from last-good. Invalid
  security-sensitive resources use per-resource last-good only when a valid
  predecessor exists; first startup without one fails closed. Publish dependency
  closures atomically.
- HTTP and stream own independent published revisions, readiness, and rollback
  state. A control-plane update is acknowledged only after durable desired state
  and each required domain publish or explicit quarantine/last-good decision.

## Runtime and Process Contract

- Use one long-lived supervisor and normally one active Go worker generation.
  Go concurrency uses goroutines and `GOMAXPROCS`; do not copy NGINX's
  CPU-count worker topology without measured evidence.
- The supervisor owns provider watch, writable bbolt, lifecycle audit, stable
  health/metrics endpoints, listener descriptors, worker launch, revision fences,
  and rollback. Workers receive immutable versioned snapshots and scoped secret
  capabilities.
- Implement process replacement with `os/exec`, inherited descriptors, and a
  versioned Unix socketpair protocol. Do not use fork-without-exec.
- Activation order is prepare, load, compile, inherit, probe, catch up, READY,
  activate, and drain. A failed new generation leaves the old generation active.
  At most one generation accepts new connections.
- Ordinary dynamic updates preserve WebSocket and other hijacked connections for
  their natural lifetime. Workers explicitly register hijacked connections and
  generation-owned goroutines because `http.Server.Shutdown` does not own them.
- A worker that ignores cancellation pins its generation. When bounded drain
  expires, the supervisor terminates the old worker and reports residual owners.
- Worker crash loops use bounded restart, probation, rollback, and a terminal
  readiness failure when no healthy generation remains.
- Linux amd64/arm64 is the first production platform. Publish and natively smoke
  test macOS amd64/arm64 binaries. Keep the Windows HTTP data plane source-buildable
  as experimental, checked by nightly and release cross-builds, without an
  official Windows artifact in this program.
- Put signals, descriptor inheritance, user switching, parent-death behavior,
  atomic replacement, and local control transport behind platform-specific
  implementations. Business and plugin packages must not import OS signal
  constants.
- The container runs the supervisor as PID 1. Lifecycle commands are canonical;
  Unix signals, systemd, Kubernetes, launchd, and Windows service controls are
  adapters to the same internal operations.

## Compiler, Plugin, and Resource Contract

- Replace mutable route construction with explicit immutable compiler phases:
  normalize, validate, resolve dependency closure, materialize secrets, prepare
  resources, compile HTTP/stream handlers, probe, and publish.
- Define plugin phase, priority, scope, instance identity, lifecycle, and resource
  sharing in a central declarative manifest. Generate registration and status
  facts only; keep behavior in ordinary Go code.
- Share clients, loggers, limiters, caches, and health schedulers only by
  effective configuration and APISIX scope. Equal reloads preserve state;
  different effective identities never share mutable state.
- Every production goroutine belongs to a request, plugin instance, generation,
  or supervisor and has an owner name, context, count, and join path. Do not
  start unowned fire-and-forget goroutines.
- Recover plugin panics at explicit plugin boundaries. Unknown core invariant
  panics terminate the worker. After response commit, flush, or hijack, abort the
  affected connection instead of attempting a second response. Finalizers run
  exactly once.
- According to decision 196C, do not retain temporary legacy adapters. Each
  subsystem cutover must add equivalence tests, install the replacement, remove
  the old path and proxy-only facades, and pass its focused gate in one atomic
  implementation unit.

## HTTP Compatibility Contract

- Define and test the downstream/upstream protocol matrix for HTTP/1.1, HTTP/2,
  h2c, gRPC, WebSocket, trailers, informational responses, cancellation, and
  connection reuse. HTTP/3 remains an explicit subsequent milestone.
- Implement listener-local HTTP/2, TLS, PROXY protocol, timeout, real-IP, and
  trust settings; never combine per-listener booleans through a global OR.
- Match APISIX radixtree URI/host semantics, route conflict and priority rules,
  plugin phase ordering, consumer merge semantics, variable semantics, error
  responses, retry boundaries, DNS behavior, load-balancer algorithms, and
  upstream authority/Host/SNI separation.
- Stream request and response bodies by default. Buffer only when the compiled
  body plan requires it, using memory plus bounded temporary spool mapped to
  upstream settings in compatibility mode and stricter quotas in strict mode.
- Preserve flush, chunking, trailers, informational responses, and cancellation.
  Do not parse a configuration field and silently ignore its behavior.
- Compatibility mode retains pinned upstream defaults and bugs unless an explicit
  versioned security/reliability divergence says otherwise. Strict mode may add
  trusted CIDRs, verified TLS, origin policy, quotas, and secret protections.

## Resource and Observability Contract

- Map APISIX/NGINX-compatible connection and request limits in compatibility
  mode. Do not impose a hidden Go `max_in_flight=1024`; strict mode may add
  explicit admission limits.
- Combine container/cgroup limits, Go memory limit, and component budgets for
  bodies, spool, caches, loggers, config compilation, and generations. Shed new
  high-cost work at a soft watermark and exit the worker at a sustained hard
  watermark.
- Async loggers use bounded queues, delivery deadlines, retries, drop metrics,
  and bounded shutdown flush. They provide at-least-attempted delivery, not
  exactly-once guarantees.
- The supervisor owns stable lifecycle/config/readiness metrics. Workers own
  request telemetry and submit bounded aggregates. Counters and histograms remain
  monotonic across generation replacement; generation-detail series have bounded
  retention.
- OpenTelemetry is disabled by default. A configured worker generation owns its
  provider, propagators, sampler, bounded exporter, and shutdown. Remove the
  implicit stdout exporter and `AlwaysSample` provider.
- Bound metric cardinality with explicit budgets and overflow reporting. Preserve
  APISIX labels within the configured budget and clean retired series according
  to gauge-immediate and counter/histogram-TTL rules.
- Expose pprof and runtime diagnostics only on a separate, explicitly enabled,
  authenticated diagnostics listener. Treat profiles as sensitive audited data.
- Health endpoints and metrics are projections of one supervisor state machine,
  not independent sources of readiness truth.

## Evidence, Delivery, and Governance Contract

- Separate behavior status from evidence maturity. A plugin's qualification is
  no stronger than its weakest required schema, unit, converted upstream,
  differential, real-dependency, failure, or recovery evidence.
- Use APISIX 3.17 at the pinned commit and immutable official image as the
  compatibility oracle. Run both data planes with the same config, input, and
  dependencies. Normalize only versioned nondeterministic fields.
- Account for every applicable pinned upstream test block as converted, N/A, or
  deferred with owner and reason. Do not hide flaky tests with retries; downgrade
  their evidence until fixed and repeated successfully.
- PR checks use hermetic fixtures and impact-scoped tests. Scheduled and release
  gates use pinned real Redis, Kafka, etcd, identity providers, TLS, cluster,
  outage, and recovery scenarios where applicable.
- Build a candidate artifact once, qualify that exact digest, then sign and
  promote the same digest. Produce an immutable evidence bundle containing source,
  upstream and dependency identities, tests, differential results, benchmarks,
  recovery, security, SBOM, signature, and provenance.
- Before 1.0, internal Go packages are not stable APIs. APISIX configuration,
  durable formats, public endpoints, and versioned Go extension contracts are
  governed now. Any active divergence requires an ADR and project-owner approval.
- Use short-lived, vertically complete PRs. Each PR updates its tests and manifest
  evidence. Do not use a long-lived rewrite branch or retain dual runtime paths.

## Program Boundaries and Dependency Order

1. Governance manifest, profile axes, status generation, and ADR controls.
2. Presence-aware static configuration and redacted effective-config tooling.
3. Durable desired/published generation journal and domain revisions.
4. Immutable compiler, plugin scopes, resource ownership, and atomic cutover.
5. Supervisor/worker lifecycle and platform adapters.
6. HTTP protocol and APISIX observable compatibility closure.
7. Runtime safety, telemetry aggregation, diagnostics, and capacity controls.
8. Differential qualification, real-dependency gates, and build-once promotion.
9. Stream subsystem convergence after the qualified HTTP milestone.

Each numbered boundary has a separate implementation plan. A boundary may begin
only when its declared consumed interfaces exist. Runtime safety work that uses
only boundaries 4 and 5 may proceed beside HTTP closure, but its body/spool
budget integration waits for boundary 6's `BodyBudget` contract. HTTP closure
and all runtime-safety tasks must complete before qualification.
