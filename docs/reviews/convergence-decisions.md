# Convergence Review Decisions

This ledger records confirmed findings that are intentionally not remediated in the convergence PR. Security and network trust-boundary items are record-only by explicit project-owner direction. Architectural items require a separate behavior decision because a local patch could fail open, delete last-good state, or change a core compatibility contract.

## Security-deferred findings

### SEC-01: OpenID Connect code flow forwards an unverified ID token

- Severity: P1
- Evidence: the OpenID Connect code-flow and refresh paths parse the JWT payload and persist/forward `X-ID-Token` without invoking the provider verifier on that token.
- Risk: downstream components that treat the forwarded token as authenticated identity may trust attacker-controlled claims.
- Decision: record only; no authentication or token-handling change in this PR.

### SEC-02: AI provider requests can forward credential and hop-by-hop headers

- Severity: P1
- Evidence: `ai_common.CopyForwardHeaders` removes only `Host`, `Content-Length`, and `Accept-Encoding`; cookies, proxy credentials, client authorization, and connection-scoped headers can reach configured AI providers.
- Risk: credentials or transport-specific headers can cross a configured provider trust boundary.
- Decision: record only; no header policy change in this PR.

### SEC-03: OPA policy responses can supply an invalid HTTP status code

- Severity: P2
- Evidence: the OPA decision `status_code` is passed to `WriteHeader` without an HTTP-range check.
- Risk: a malicious or compromised policy endpoint can cause a request-path panic and denial of service.
- Decision: record only because the value crosses a network trust boundary; no OPA behavior change in this PR.

### SEC-04: Kafka logger collapses per-broker SASL identity

- Severity: P2
- Evidence: configuration accepts broker-specific SASL settings, but writer construction applies the first selected credentials across the broker set.
- Risk: credentials can be sent to a broker for which they were not configured, and mixed-identity clusters cannot be represented safely.
- Decision: record only; no authentication topology change in this PR.

### SEC-05: CSRF documentation overstates the hashing construction

- Severity: P2
- Evidence: documentation describes HMAC-SHA256 while the implementation and pinned upstream behavior use the existing keyed/plain SHA256 construction.
- Risk: operators may infer a stronger cryptographic contract than the runtime provides.
- Decision: record only; neither documentation nor implementation is changed in this PR.

### SEC-06: Invalid host entries can broaden route exposure

- Severity: P2
- Evidence: invalid `hosts` elements are skipped; an empty effective host list is treated as hostless and mixed valid/invalid lists can partially publish.
- Risk: a malformed host restriction can produce a broader request match than intended.
- Decision: record only; no route-host trust-boundary behavior change in this PR.

### SEC-07: Shared client configuration IDs are structurally ambiguous

- Severity: P2
- Evidence: shared IDs concatenate formatted values with `:` delimiters, so differently segmented inputs can produce the same identity string.
- Risk: callers can reuse a client carrying the wrong authentication or destination configuration.
- Decision: record only; no shared-client identity migration in this PR.

### SEC-08: ClickHouse logger environment credentials conflict with secret ownership enforcement

- Severity: P2
- Evidence: `clickhouse-logger.user=$ENV://...` is resolved in `PostInit`, but the plugin does not declare the generic secret-materialization ownership contract. The route builder rejects the reference before `PostInit`, and the pinned `t/plugin/clickhouse-logger.yaml` environment case therefore cannot publish its route.
- Risk: an operator relying on environment-backed ClickHouse credentials sees the affected route quarantined after the ownership guard is enabled.
- Decision: record only because the repair changes credential ownership/materialization; the non-security harness environment test uses the already supported consumer-secret path instead.

### SEC-09: Malformed legacy SSL rows remain generation-fatal

- Severity: P2
- Status: remediating / fixed
- Evidence: new SSL writes are certificate-validated before commit, but `ConfigSnapshot` previously returned a generation error when a malformed SSL row already existed in the durable store.
- Risk: legacy corruption can block startup; silently skipping it can also remove a TLS identity and change network security behavior.
- Decision: last-good-or-fail-closed. Snapshot decode failure keeps the previous SSL for that ID when one exists and publishes the rest; first startup with no last-good fails the generation. SSL is never omitted.

### SEC-10: Active HTTPS health checks do not enforce their certificate-verification contract

- Severity: P2
- Evidence: the active health-check parser ignores APISIX `checks.active.https_verify_certificate`, whose upstream default is `true`; probes reuse the upstream transport, so an omitted or disabled upstream `tls.verify` can also disable certificate verification for an HTTPS health check.
- Risk: an untrusted network peer with an invalid certificate can influence upstream health state and traffic selection.
- Decision: record only; no TLS transport, schema, or probe behavior change in this PR.

### SEC-11: Network-provided terminal status values are not bounded before response writing

- Severity: P2
- Evidence: Chaitin WAF JSON `status`, OpenWhisk JSON `statusCode`, and Dubbo upstream HTTP status values can reach `WriteHeader` without a Go-terminal `200..599` contract; the first two paths can also receive values outside the HTTP range from a remote peer.
- Risk: a malicious or compromised upstream can trigger a request-path panic with an invalid status, while informational statuses can be emitted as interim responses followed by an unintended implicit 200.
- Decision: record only because these values cross network trust boundaries; no plugin schema, transport, or response-writing change in this PR.

## Architecture decisions required

### ARCH-01: Invalid global-rule isolation versus fail-closed behavior

- Status: remediating / fixed
- Current behavior: a malformed global-rule row keeps the previously published version of that ID when one exists; if there is no last-good (including first startup), the generation fails closed and the previously installed handler remains.
- Tradeoff: skipping one invalid global rule may silently remove a security or compliance control and fail open.
- Decision: last-good-or-fail-closed. Global rules are never omitted from a published generation.

### ARCH-02: Per-resource last-good semantics for stream routes

- Status: remediating / fixed
- Current behavior: stream-route PUTs retain last-good on decode failure. List/reload keeps the previous version of a corrupted ID when one exists; first startup with no last-good fails closed. Explicit delete drops last-good. Conflicting listen addresses are generation-fatal.
- Tradeoff: isolation requires stable per-route ownership, delete semantics, listener reconciliation, readiness metrics, and a definition for conflicting listen addresses.
- Decision: last-good-or-fail-closed with per-route ownership. Stream routes are never omitted to keep the process up.

### ARCH-03: Central APISIX core route schema enforcement

- Current behavior: route validation is distributed and does not enforce the complete pinned APISIX 3.17 schema before materialization.
- Tradeoff: importing the full upstream contract would reject currently accepted bare or compatibility routes and is broader than a panic-prevention patch.
- Decision needed: choose strict upstream schema parity or retain a documented compatibility subset with explicit deviations.

### ARCH-04: HTTP fixture observation window

- Current behavior: exact HTTP fixture assertions run after child shutdown so already-issued and shutdown-flush requests are observable without adding a delay to every case. Bounded count ranges remain pre-shutdown checks.
- Remaining decision: if producers can legitimately outlive child shutdown, define an explicit per-fixture settling window rather than a global sleep.

### ARCH-05: Malformed legacy dynamic plugin list

- Status: remediating / fixed
- Current behavior: a malformed durable `plugins` entry keeps the previously published `httpPlugins` when one exists; first startup with no last-good fails the generation. Reload never falls back to `config.GlobalConfig.Plugins`.
- Tradeoff: skipping it falls back to another plugin allowlist and can unexpectedly enable or disable behavior across every route.
- Decision: last-good-or-fail-closed. The dynamic plugin list is never omitted and never replaced by the static config allowlist.
