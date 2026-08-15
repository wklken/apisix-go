# Production Readiness Remediation Ledger

Date: 2026-08-15

Base revision: `e34d59ed750d1b27d2b0e57562122e9b8040f0f5`

Implementation branch: `codex/prod-ready-remediation-wave1`

## Current verdict

The remediation in this branch closes the implementation-ready P0 findings from the 2026-08-15 review, except for the SLS credential protocol finding, which was reclassified after verification against Apache APISIX 3.17. The repository is still **not production ready** because release provenance, multi-replica state, global request-size enforcement, secret ownership, operational soak/fault-injection evidence, and several configuration-honesty items remain open.

Do not remove the README or CLI `NOT READY FOR PRODUCTION` statement from this branch.

## Closed in this branch

| Review item | Result | Acceptance evidence |
| --- | --- | --- |
| `script` / `filter_func` silently ignored | Closed by rejecting present route `script` and non-empty `filter_func` during strict route compilation. Lua execution was not approximated. | Resource decode and strict-build regression tests. |
| No HTTP liveness/readiness | Closed with exact `/livez` and `/readyz` endpoints outside the dynamic route table. Readiness requires both config stages and, in etcd mode, current etcd reachability. | Metrics/server unit and race tests; Docker healthcheck uses `/readyz`. |
| Default image/config not a production entry | Closed for the default container contract. The image uses `config-production.yaml`, has no endpoint/key fallback, and fails fast until a real etcd endpoint is supplied. | Real-file config tests, Dockerfile contract test, and direct binary non-zero startup check. |
| Default access-log headers expose credentials | Closed for the identified default-header path. Known credential headers are removed from shared and file-logger default payloads; explicit custom formats remain explicit opt-ins. New file logs use `0600`. | Shared/file logger live and detached-path tests. |
| `tcp-logger` / `syslog` unconditionally skip TLS verification | Closed with TLS 1.2 minimum, secure `ssl_verify` default, hostname/SNI handling, and explicit compatibility opt-out. | Real TLS handshake tests reject untrusted peers by default and accept only the explicit opt-out. |
| Missing upstream list weight becomes zero | Closed. Missing and negative weights are rejected; explicit zero remains APISIX-compatible when at least one final target has positive weight; final all-zero sets fail closed. | Route-build tests for list/map/duplicate-normalization cases. |
| Default `ai` plugin is a no-op | Closed by removing it from the default allowlist and making the registered placeholder fail initialization with an unsupported-control-plane error. | Plugin and default-config tests. |
| Authentication rejection can omit CORS headers | Closed by running system/global rewrites before static system/global/route CORS handlers and wrapping the authentication boundary. Consumer/consumer-group header-only CORS winners are materialized post-authentication and override route CORS request-locally; body/streaming/protocol dynamic owners remain fail-closed. | 401, route-id `_meta.filter`, preflight, success, Vary normalization, consumer and consumer-group scope tests without a buffered sibling plugin. |
| HTTP `type: chash` is silently treated as round robin | Closed by accepting only implemented `roundrobin` for HTTP-family upstreams and rejecting `chash`/other unsupported types at compile time. | Strict route/upstream tests. |
| CSRF uses `math/rand` | Closed as a P1 hardening item. The signed float64 token shape remains compatible, but its 53 random bits come from `crypto/rand`; entropy failure returns a generic HTTP 500 without a token. | Deterministic injected-reader and failure tests plus existing static-token validation. |

## Reclassified after verification

### SLS `access-key-secret` in structured data

The proposed deletion is not an implementation-safe redaction. The pinned Apache APISIX 3.17 `sls-logger` protocol also places `access-key-secret` in the RFC5424 structured data used by that transport. Removing it only in Go would break protocol authentication rather than repair an accidental default access log.

Disposition:

- `sls-logger` is not in the new production allowlist.
- Keep it disabled in production until the transport is replaced with an authenticated protocol/SDK that does not put a long-lived credential in the emitted message.
- Do not mark the current SLS protocol production-safe merely because it matches upstream behavior.

## Production container profile now enforced

- `debug: false`.
- HTTP-only proxy mode, no stream listeners, and no stream plugins.
- Exact default HTTP allowlist: `request-id`, `cors`, `key-auth`, `jwt-auth`, `basic-auth`, `prometheus`.
- Data-plane etcd provider with TLS verification enabled.
- No default etcd endpoint; `APISIXGO_DEPLOYMENT_ETCD_HOST` or an operator-managed config is mandatory.
- Loopback-only trusted proxy CIDRs by default.
- No embedded admin or data-encryption keys.
- `/livez` for liveness and `/readyz` for config/provider readiness.

This is a safe starting contract, not a declaration that every allowlisted plugin or every deployment topology is production-qualified.

## Remaining production-readiness work

### P0/P1 architecture and runtime safety

1. Implement a global streaming-safe request body limit with a stable HTTP 413 contract for content-length and chunked bodies. Do not map an upstream `MaxBytesError` to 502 or buffer every request in memory.
2. Make secret materialization mandatory at plugin boundaries. Stop storing resolved plaintext back into long-lived plugin configuration, including the AWS content-moderation path.
3. Close remaining authentication defaults: `wolf-rbac` TLS verification, OIDC session-cookie `Secure`, constant-time basic-auth comparison without APISIX-incompatible trimming, malformed Authorization cleanup on anonymous fallback, and `ldap-auth.hide_credentials`.
4. Fail closed or implement declared-but-unused configuration fields, including discovery upstream fields, route websocket behavior, selected Zipkin/OTel fields, Admin/discovery/QUIC/WASM sections, and SIGHUP reload semantics. CORS metadata origins were rechecked against code and tests and are implemented.
5. Define safe ownership for client-supplied `X-Consumer-Username` on unauthenticated routes and redact query-bearing `RequestURI` values from proxy failure logs.

### Observability

1. Add a process-level access log contract or explicitly reject the parsed NGINX access-log settings.
2. Give Elasticsearch, ClickHouse, and Tencent CLS meaningful safe defaults or require explicit `log_format`.
3. Finish stream metrics and establish bounded HTTP status/latency label lifecycle budgets.
4. Resolve SkyWalking single-span and OTel `inactive_timeout` semantic gaps.

### High availability

Process-local state still diverges across replicas: local rate limiting, CAS/DingTalk/Feishu sessions, proxy cache, active/passive health state, secret cache, and MCP sessions. A production topology must either move each state owner to a shared backend or explicitly disable the affected feature. OIDC and supported rate limiters should use Redis where available.

### Release supply chain and operations

1. Add image signing and verifiable provenance (for example, Sigstore/cosign plus a documented identity policy).
2. Run security release gates for release candidates, not only selected CI paths.
3. Add reproducible container build/run smoke, SBOM/vulnerability evidence, etcd disconnect/recovery fault injection, soak testing, and a standalone rollback/runbook contract.
4. Verify the production image in a clean Docker environment. Docker was unavailable in the implementation environment for this branch.

### Explicit production exclusions

- Keep `proxy_mode: http`; no generic stream plugin chain or stream mTLS.
- Do not depend on per-SNI protocol/client-mTLS configuration until the SSL resource contract supports it.
- Keep Lua/serverless, ext-plugin, inspect, GM, MCP bridge, SLS logger, local session plugins, unsupported discovery, Admin API, WASM, XRPC, QUIC, and HTTP/3 outside the production profile.

## Verification completed

- Impact-scoped package tests for config, metrics, server, plugin executor/base/file logger, AI, CSRF, TCP/syslog loggers, resource, and route.
- Focused `-race` tests for metrics and server readiness state.
- `make lint`: zero issues.
- `make build`: passed with Go 1.26.6.
- `git diff --check`: passed.
- Direct production-config startup without an etcd override: expected non-zero exit with `deployment.etcd.host must contain at least one endpoint`.

Not run:

- Docker build/runtime smoke because the Docker CLI is not installed in the implementation environment.
- Broad `make test`, `go test ./...`, and the full real-process `t/plugin` suite, per the repository's impact-scoped verification policy.
- Release signing/provenance, external vulnerability scanning, etcd chaos, or soak gates; these remain open work rather than implied evidence.
