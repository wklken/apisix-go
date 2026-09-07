# APISIX-Go Architecture

APISIX-Go is a single-process Go implementation of the Apache APISIX 3.17 data
plane. This document describes the current cross-package architecture; source
code and focused tests remain authoritative for implementation details.

## Reading map

| Question | Source |
| --- | --- |
| Where is plugin behavior implemented and tested? | `pkg/plugin/<plugin>` and `t/plugin` |
| How is static configuration loaded? | [Configuration](configuration.md) |
| What can an APISIX HTTP user expect? | [HTTP data-plane compatibility](http-data-plane.md) |
| How is an HTTP candidate qualified? | [HTTP candidate qualification](runbooks/http-candidate-qualification.md) |
| Where is plugin behavior tested? | [Plugin testing](plugin-testing.md) |
| Why does behavior intentionally differ from APISIX? | [Architecture decisions](#architecture-decisions) |

## Compatibility model

Observable Apache APISIX 3.17 HTTP data-plane behavior is the product target.
The official APISIX 3.17 source and tests define compatibility; validation
artifacts do not participate in runtime behavior.

`pkg/plugin/registry.go` directly owns implemented factories, aliases, phases,
scopes, and domains. Plugin names, priorities, and schemas come from each
initialized implementation. `pkg/capability/declarations.go` independently
owns the encrypted fields that plugins may materialize. Neither file is a
behavior, evidence, readiness, divergence, or platform-status ledger.

```text
plugin implementation Init() -- name, priority, schemas
pkg/plugin/registry.go       -- constructor and execution placement
pkg/capability/declarations.go -- encrypted fields
```

Registration and inventory counts do not prove compatibility or production
readiness.

Plugin verification has separate owners: package tests cover plugin-local
logic, and `t/plugin` covers candidate-only real-process behavior. Comparisons
with official APISIX source are development work; every discovered regression
must land in one of those two maintained layers. The product repository does
not keep an oracle runner, aggregate compatibility catalog, or a second
regression-test implementation. See [Plugin testing](plugin-testing.md).

## Runtime overview

```text
static configuration
  -> etcd or standalone initial sync
  -> generation coordinator
  -> in-memory desired state
  -> compiler and prepared generation
  -> atomic generation activation
  -> HTTP/TLS and stream listeners
```

The process has one `Server + GenerationEngine`. Providers submit immutable
desired batches; they do not mutate routes, plugins, listeners, or runtime
resources. `generation.Coordinator` is the single desired-to-published writer.

Startup requires the provider's initial desired-state submission before opening
listeners. The production construction path is:

```text
cmd/root.go
  -> effective configuration and encrypted-field catalog
  -> encryption and generation-scoped secret resolver
  -> server, compiler factory, and generation engine
  -> provider initial sync and atomic publication
  -> listeners
```

## Configuration

Static configuration uses one presence-aware order:

```text
built-in defaults
  -> conf/config-default.yaml
  -> optional -c/--config file
  -> APISIX 3.17 reserved environment overrides
```

Maps merge recursively and lists replace earlier lists. Absent, null, false,
zero, and empty string remain distinct; null deletes a mapping key before
schema defaults are applied. YAML anchors, aliases, and merge keys are supported.
The effective result is passed
explicitly into runtime construction; it is not mutable global state. See
[Configuration](configuration.md) for operator-facing details.

## Desired-to-published publication

`generation.Coordinator` serializes provider updates and owns the in-memory
desired snapshot, provider cursor, published domain heads, and last
acknowledgement. The publication order is:

```text
Apply desired batch in memory
  -> compile and prepare required domains
  -> atomically replace the active generation bundle
  -> commit coordinator state
  -> acknowledge the provider
```

Compilation or activation failure leaves both the active bundle and coordinator
state unchanged. Exact same-cursor replay returns the in-memory acknowledgement
without recompilation; cursor reuse with different content fails. After process
restart, etcd or standalone supplies a fresh authoritative snapshot before the
data plane accepts traffic.

Invalid first-generation input fails closed. `last-good` may reuse only the
exact same-domain resource from the active predecessor. A tombstone is a
deletion and never falls back. HTTP and stream can advance independently, but
all domains required by one commit change atomically.

## Compilation and generation ownership

Planning completes before any plugin, secret, client, resource, task, or
materialization side effect. It selects the exact winning resource and plugin
occurrences; disabled or losing occurrences acquire nothing.

A prepared generation owns one complete runtime generation:

- immutable publication and metadata;
- consumers and generation-scoped secret materialization;
- background task ownership and shared-resource leases;
- HTTP/TLS and stream snapshots; and
- the cleanup ledger.

Cleanup is retryable and ordered: stop work, finalize resources, then release
generation secrets. A timeout or residual task retains ownership for a later retry; it
does not permit the generation to be detached early.

Process-log rotation holds HTTP publication authority for the complete filesystem
operation. The serving engine revokes the predecessor's authority before swapping
the HTTP bundle and waits for an admitted rotation to finish; failed publication
restores the predecessor's authority. Prepared and draining generations cannot
start rotation. The compiler supplies only the scoped execution callback to plugins,
without exposing task registries or resource handles.

Local rate-limit counters and MCP session lookup are shared through compiler-owned
resource leases. Unrelated HTTP publication does not reset an active quota or
disconnect an existing MCP session. The SSE request still owns its subprocess
and request tasks; sharing lookup does not transfer that request to a successor
generation. The final resource lease closes the shared registry.

## Serving and leases

`GenerationEngine` publishes one atomic bundle with independent HTTP and stream
slots. Updating one slot preserves the other. A generation retires only after
it owns no active domain and every lease has drained.

Each HTTP request, TLS certificate selection, hijacked connection, and stream
connection pins the exact generation it uses. Activation and rollback affect
new acquisitions, not already admitted work.

`pkg/route` and `pkg/stream` compile detached immutable snapshots. They do not
own provider state or plugin materialization. `pkg/compiler` owns side
effects, and `pkg/server` owns activation and listener lifecycle.

The compiler resolves the process node identity, then the HTTP route core
initializes APISIX route, service, node, and matched-route variables before the
plugin pipeline runs. These variables are request infrastructure, not a
configurable system plugin.

Background work uses the runtime task APIs. Request and connection children
must be joined before their generation lease is released. Shared resources are
identified by their complete kind, scope, digest, and Go type; final close is
retryable.

## Shutdown and readiness

Shutdown proceeds through ownership boundaries:

```text
stop provider
  -> reject new listener and lease admission
  -> drain HTTP, stream, and generation leases
  -> close generation engine and secret resolver
  -> close metrics and tracing
```

An incomplete phase cannot release a later dependency. A subsequent shutdown
continues from the first incomplete phase.

The status listener defaults to `127.0.0.1:7085`. `/status` is liveness;
`/status/ready` reports whether a serviceable configuration is active. Etcd
loss does not withdraw readiness while the current last-good
generation remains serviceable.

Metrics use bounded label sets. Untrusted provider, route, resource, owner, or
panic values cannot become labels. Plugin panics are attributed without
exposing the raw panic value, and cleanup still runs before the response or
re-panic decision.

## HTTP, TLS, and stream boundaries

| Area | Current boundary |
| --- | --- |
| HTTP | APISIX-compatible route, service, consumer, upstream, and plugin pipeline. |
| Frontend TLS | Explicit TLS 1.1 with compatible ciphers, TLS 1.2/1.3, exact/wildcard/fallback SNI certificates, session tickets, and the configured client-CA policy. |
| Stream | Raw TCP with immutable route snapshots and at most one `mqtt-proxy` protocol binding. |
| Not implemented | UDP, stream TLS/mTLS, PROXY protocol, service discovery, general stream-plugin chaining, external plugin runners, WASM, XRPC, QUIC, and HTTP/3. |

Frontend certificate and key references are materialized through the generation
secret catalog before TLS compilation. SSL private-key decryption follows the
SSL keyring independently of the plugin data-encryption enable flag.
URI-conditional mTLS stores its verification result and skip patterns in the
connection context at handshake time. Each TLS connection also retains the
effective SNI (including fallback) and an immutable index of protected hosts;
requests for an mTLS-protected host must match that SNI. Requests retain this
policy across later generation changes without retaining other hosts' key material.

HTTP upstream upgrades require `enable_websocket: true` to forward the upgrade
headers; disabled routes still proxy ordinary HTTP. Protocol-owned upgrades such
as Kafka PubSub use their own handshake. Upgraded connections retain their HTTP
generation until the connection closes. `SIGHUP` re-reads static configuration
but may change only `nginx_config.error_log_level`; invalid input or any other
static difference leaves the process unchanged. Provider updates use atomic
in-memory publication.

HTTP upstream selection supports weighted round robin, consistent hashing,
least connections, and EWMA within health and priority groups. Request-owned
attempts release their load counters on completion, error, and retry; latency
samples require an actual upstream response. Generation-owned clusters retain
health probes and connection pools until their leases drain.

HTTP/1 upstream pools apply static defaults and resource `keepalive_pool`
overrides for idle capacity, idle expiry, and requests per connection. The
request cap retires connections without interrupting response bodies or accepted
upgrades. HTTP/2 and gRPC transports do not yet implement this request-count cap.

Protocol-specific transports remain plugin owned:

| Plugin | Implemented scope | Explicit boundary |
| --- | --- | --- |
| `kafka-proxy` | APISIX PubSub WebSocket list-offset/fetch with SASL/PLAIN and TLS options. | The raw Kafka frame bridge is an extension; external broker qualification is separate. |
| `dubbo-proxy` | Hessian2 HTTP-to-Dubbo terminal with bounded connect-only retry. | No shared persistent multiplexing or retry after request bytes are written. |
| `http-dubbo` | Dubbo 2.x fastjson adapter. | It remains distinct from the Hessian2 adapter. |
| `mqtt-proxy` | MQTT CONNECT preread and TCP stream binding. | No UDP, stream TLS/mTLS, or general stream-plugin chain. |

## Secret authority

Secret declarations in `pkg/capability/declarations.go` are runtime authority. Each
prepared generation gets a read-only view scoped by generation, domain, plugin
factory, resource, source, and field.

Managed references dispatch to Vault, AWS Secrets Manager, or GCP Secret Manager
using the secret resources supplied in the publication closure. Backend
credentials, OAuth tokens, and resolved-value caches remain owned by that
generation; remote error bodies are not exposed in resolver diagnostics.

The resolver can read only the exact publication closure. Cross-generation,
cross-domain, cross-resource, and undeclared access fail before backend use.
Plaintext exists only inside `secret.Value.Use`; logs, metrics, status, errors,
and persistent records expose only redacted identity. Cache eviction and close
zero retained bytes. Closing the generation invalidates every derived view.

## Process and candidate boundary

One APISIX-Go process runs per replica. Kubernetes, systemd, or another service
manager owns abnormal-exit restart, replica replacement, and rollout
availability. APISIX-Go owns activation, readiness,
connection draining, and graceful termination. There is no internal
supervisor/worker or listener-inheritance protocol.

The container runs as UID/GID `10001:10001` and defaults to
`/usr/local/apisix/conf/config.yaml`. Candidate qualification builds an
ephemeral Linux amd64 image for container and stability evidence; the
repository does not publish that image. See
[HTTP candidate qualification](runbooks/http-candidate-qualification.md).

## Architecture decisions

ADRs record unavoidable compatibility differences that remain in the current
implementation. They are architecture documentation, not runtime approval or
qualification state.

| ADR | Status | Decision |
| --- | --- | --- |
| [0004](architecture/adr/0004-runtime-safety-boundaries.md) | accepted | Bound ambiguous stream routing and embedded Lua execution. |
| [0005](architecture/adr/0005-credential-log-redaction.md) | accepted | Redact credential material from authentication logs. |
| [0006](architecture/adr/0006-frontend-tls-cipher-boundary.md) | accepted | Preserve the Go TLS cipher boundary, including unsupported finite-field DHE. |
