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
- Evidence: new SSL writes are certificate-validated before commit, but `ConfigSnapshot` still returns a generation error when a malformed SSL row already exists in the durable store.
- Risk: legacy corruption can block startup; silently skipping it can also remove a TLS identity and change network security behavior.
- Decision: record only; define fail-closed or last-good SSL recovery before changing startup semantics.

## Architecture decisions required

### ARCH-01: Invalid global-rule isolation versus fail-closed behavior

- Current behavior: a semantically invalid global rule remains generation-fatal, including initial startup.
- Tradeoff: skipping one invalid global rule may silently remove a security or compliance control and fail open.
- Decision needed: define whether last-good global-rule retention, generation rejection, or explicit per-rule quarantine is the required contract.

### ARCH-02: Per-resource last-good semantics for stream routes

- Current behavior: one invalid stream route rejects the stream generation during initial startup or reload.
- Tradeoff: isolation requires stable per-route ownership, delete semantics, listener reconciliation, readiness metrics, and a definition for conflicting listen addresses.
- Decision needed: design stream-route last-good/quarantine behavior before implementation.

### ARCH-03: Central APISIX core route schema enforcement

- Current behavior: route validation is distributed and does not enforce the complete pinned APISIX 3.17 schema before materialization.
- Tradeoff: importing the full upstream contract would reject currently accepted bare or compatibility routes and is broader than a panic-prevention patch.
- Decision needed: choose strict upstream schema parity or retain a documented compatibility subset with explicit deviations.

### ARCH-04: HTTP fixture observation window

- Current behavior: exact HTTP fixture assertions are made before child shutdown and check for an extra request only non-blockingly.
- Planned narrow fix: assert exact HTTP fixtures after child shutdown so already-issued and shutdown-flush requests are observable without adding a delay to every case.
- Remaining decision: if producers can legitimately outlive child shutdown, define an explicit per-fixture settling window rather than a global sleep.

### ARCH-05: Malformed legacy dynamic plugin list

- Current behavior: a malformed or structurally inconsistent durable `plugins` entry is generation-fatal.
- Tradeoff: skipping it falls back to another plugin allowlist and can unexpectedly enable or disable behavior across every route.
- Decision needed: define last-good ownership and recovery for the generation-wide plugin list before isolating it.
