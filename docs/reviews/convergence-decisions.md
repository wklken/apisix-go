# Convergence Review Decisions

This ledger records confirmed findings from the convergence review and the
stacked remediations that closed them. The original convergence PR left these
items record-only; later PRs applied the locked last-good, trust-boundary,
and compatibility contracts.

## Security findings

### SEC-01: OpenID Connect code flow forwards an unverified ID token

- Severity: P1
- Status: remediating / fixed
- Evidence: the OpenID Connect code-flow and refresh paths previously parsed the JWT payload and persisted/forwarded `X-ID-Token` without invoking the provider verifier.
- Decision: verify the ID token with the existing provider verifier before persist/forward. Reject `alg: none` and failed signature/issuer/audience. Empty ID token remains allowed only when the provider omitted it. Evidence: `fix-trust-boundary-forwarding`.

### SEC-02: AI provider requests can forward credential and hop-by-hop headers

- Severity: P1
- Status: remediating / fixed
- Evidence: `ai_common.CopyForwardHeaders` previously removed only `Host`, `Content-Length`, and `Accept-Encoding`.
- Decision: expand the denylist to hop-by-hop and credential headers, reusing `pkg/plugin/base` hop-by-hop names. Provider auth stays the configured credential. Evidence: `fix-trust-boundary-forwarding`.

### SEC-03: OPA policy responses can supply an invalid HTTP status code

- Severity: P2
- Status: remediating / fixed
- Evidence: the OPA decision `status_code` was passed to `WriteHeader` without a Go-terminal bound.
- Decision: accept only `200..599`; out-of-range values fail closed on the plugin error path. Evidence: `fix-network-status-bounds`.

### SEC-04: Kafka logger collapses per-broker SASL identity

- Severity: P2
- Status: remediating / fixed
- Evidence: writer construction applied the first selected SASL credentials across the broker set.
- Decision: require all configured broker SASL identities to be identical at `PostInit`, or reject. Evidence: `fix-credential-identity`.

### SEC-05: CSRF documentation overstates the hashing construction

- Severity: P2
- Status: remediating / fixed
- Evidence: documentation described HMAC-SHA256 while `genSign` uses keyed/plain SHA256 over `{expires,random,key}`.
- Decision: correct `docs/plugins.md` to that construction. Do not change the hash. Evidence: `fix-compat-contracts`.

### SEC-06: Invalid host entries can broaden route exposure

- Severity: P2
- Status: remediating / fixed
- Evidence: invalid `hosts` elements were skipped; an empty effective host list became hostless.
- Decision: validate every `host` / `hosts` entry before registration. Any invalid entry, `hosts: []`, or empty effective list fails the route. Evidence: `fix-route-host-health-tls`.

### SEC-07: Shared client configuration IDs are structurally ambiguous

- Severity: P2
- Status: remediating / fixed
- Evidence: shared IDs concatenated formatted values with `:` delimiters.
- Decision: length-prefix each `ConfigUID` part as `<len>:<bytes>` before the existing MD5. Evidence: `fix-credential-identity`.

### SEC-08: ClickHouse logger environment credentials conflict with secret ownership enforcement

- Severity: P2
- Status: remediating / fixed
- Evidence: `clickhouse-logger.user=$ENV://...` was resolved in `PostInit` without `SecretMaterializer`.
- Decision: implement `MaterializeSecrets()` and store descriptors so environment-backed users publish after ownership. Evidence: `fix-credential-identity`.

### SEC-09: Malformed legacy SSL rows remain generation-fatal

- Severity: P2
- Status: remediating / fixed
- Evidence: new SSL writes are certificate-validated before commit, but `ConfigSnapshot` previously returned a generation error when a malformed SSL row already existed in the durable store.
- Risk: legacy corruption can block startup; silently skipping it can also remove a TLS identity and change network security behavior.
- Decision: last-good-or-fail-closed. Snapshot decode failure keeps the previous SSL for that ID when one exists and publishes the rest; first startup with no last-good fails the generation. SSL is never omitted.

### SEC-10: Active HTTPS health checks do not enforce their certificate-verification contract

- Severity: P2
- Status: remediating / fixed
- Evidence: the active health-check parser ignored `https_verify_certificate` and reused the upstream transport.
- Decision: parse the flag (default `true` for HTTPS) and build a probe-specific TLS client independent of upstream `tls.verify`. Evidence: `fix-route-host-health-tls`.

### SEC-11: Network-provided terminal status values are not bounded before response writing

- Severity: P2
- Status: remediating / fixed
- Evidence: Chaitin WAF, OpenWhisk, and Dubbo HTTP statuses could reach `WriteHeader` without a `200..599` bound.
- Decision: shared terminal-status helper; out-of-range values fail closed. Evidence: `fix-network-status-bounds`.

## Architecture findings

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

- Status: remediating / fixed
- Decision: retain a documented compatibility subset. One pre-materialization entrypoint (`validateRouteCompatibility`) runs the current checks, including host rules. Do not import the full APISIX 3.17 schema. Evidence: `fix-compat-contracts`.

### ARCH-04: HTTP fixture observation window

- Status: remediating / fixed
- Decision: optional per-fixture `settle` waits that bounded duration before the fixture assert, including the extra-request check. Default remains no extra wait. Network and exact-zero-UDP stay post-shutdown. No global sleep. Evidence: `fix-compat-contracts`.

### ARCH-05: Malformed legacy dynamic plugin list

- Status: remediating / fixed
- Current behavior: a malformed durable `plugins` entry keeps the previously published `httpPlugins` when one exists; first startup with no last-good fails the generation. Reload never falls back to `config.GlobalConfig.Plugins`.
- Tradeoff: skipping it falls back to another plugin allowlist and can unexpectedly enable or disable behavior across every route.
- Decision: last-good-or-fail-closed. The dynamic plugin list is never omitted and never replaced by the static config allowlist.
