# APISIX-Go Architecture

APISIX-Go is a single-process Go implementation of the Apache APISIX 3.17 data
plane. It is still under development and is not production ready. This document
describes the current cross-package architecture; source code and focused tests
remain authoritative for implementation details.

## Reading map

| Question | Source |
| --- | --- |
| What is implemented and verified for each plugin? | Generated [plugin status](plugins.md) |
| How is static configuration loaded? | [Configuration](configuration.md) |
| What can an APISIX HTTP user expect? | [HTTP data-plane compatibility](http-data-plane.md) |
| How is a candidate qualified or released? | [Production release runbook](runbooks/production-release.md) |
| Why does behavior intentionally differ from APISIX? | [Architecture decisions](#architecture-decisions) |

## Compatibility model

Observable APISIX 3.17 behavior is the default target. Go-native extensions or
security boundaries may differ only when they are declared and evidenced.
Registration and inventory counts do not prove compatibility or production
readiness.

`pkg/capability/manifest.yaml` is the editable source for plugin factories,
aliases, behavior status, evidence, gaps, divergences, platforms, and secret
declarations. It generates the runtime registry and human status projections:

```text
pkg/capability/manifest.yaml
  -> pkg/plugin/registry_gen.go
  -> docs/plugins.md
  -> generated README summaries
```

Generated files must not be edited directly. Validation and release evidence
describe what was tested; they never change runtime behavior.

## Runtime overview

```text
static configuration
  -> journal recovery
  -> HTTP/TLS and stream listeners
  -> etcd or standalone provider
  -> generation coordinator
  -> durable journal
  -> compiler and prepared generation
  -> atomic generation activation
```

The process has one `Server + GenerationEngine`. Providers submit immutable
desired batches; they do not mutate routes, plugins, listeners, or runtime
resources. `generation.Coordinator` is the single desired-to-published writer.

Startup recovers the committed journal before listeners and providers begin.
The production construction path is:

```text
cmd/root.go
  -> capability and effective configuration
  -> encryption and generation-scoped secret resolver
  -> durable journal recovery
  -> server, compiler factory, and generation engine
  -> listeners
  -> provider
```

## Configuration

Static configuration uses one presence-aware order:

```text
built-in defaults
  -> conf/config-default.yaml
  -> optional -c/--config file
  -> recognized APISIXGO_* variables
  -> repeatable --set values
```

Maps merge recursively and lists replace earlier lists. Absent, null, false,
zero, and empty string remain distinct. The effective result retains provenance
and is passed explicitly into runtime construction; it is not mutable global
state. See [Configuration](configuration.md) for operator-facing details.

## Desired-to-published transaction

`pkg/store` is the bbolt implementation of the generation journal, not a
mutable runtime resource store. The journal records desired revisions, staged
transactions, committed HTTP and stream heads, decisions, provider cursors,
and acknowledgements under the configured data directory.

The only publication order is:

```text
ApplyDesired
  -> Prepare
  -> Stage
  -> Activate
  -> Commit acknowledgement
  -> FinalizeActivation
  -> acknowledge the provider
```

Activation may temporarily expose a candidate before durable commit. If
activation or commit fails, the engine restores the exact predecessor before
aborting the stage. Recovery serves only committed published state, never the
newest desired state.

Invalid first-generation input fails closed. `last-good` may reuse only the
exact same-domain resource from a committed predecessor. A tombstone is a
deletion and never falls back. HTTP and stream can advance independently, but
all domains required by one commit change atomically.

## Compilation and generation ownership

Planning completes before any plugin, secret, client, resource, task, or
registration side effect. It selects the exact winning resource and plugin
occurrences; disabled or losing occurrences acquire nothing.

A prepared generation owns one complete attempt:

- immutable publication and metadata;
- consumer and secret authority;
- background task ownership and shared-resource leases;
- HTTP/TLS and stream snapshots; and
- the cleanup ledger.

Cleanup is retryable and ordered: stop work, finalize resources, then release
authority. A timeout or residual task retains ownership for a later retry; it
does not permit the generation to be detached early.

## Serving and leases

`GenerationEngine` publishes one atomic bundle with independent HTTP and stream
slots. Updating one slot preserves the other. A generation retires only after
it owns no active domain and every lease has drained.

Each HTTP request, TLS certificate selection, hijacked connection, and stream
connection pins the exact generation it uses. Activation and rollback affect
new acquisitions, not already admitted work.

`pkg/route` and `pkg/stream` compile detached immutable snapshots. They do not
read the journal or own plugin materialization. `pkg/compiler` owns side
effects, and `pkg/server` owns activation and listener lifecycle.

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
  -> close journal
  -> close metrics and tracing
```

An incomplete phase cannot release a later dependency. A subsequent shutdown
continues from the first incomplete phase.

The status listener defaults to `127.0.0.1:7085`. `/status` is liveness;
`/status/ready` reports whether a committed serviceable configuration is
active. Etcd loss does not withdraw readiness while the committed last-good
generation remains serviceable.

Metrics use bounded label sets. Untrusted provider, route, resource, owner, or
panic values cannot become labels. Plugin panics are attributed without
exposing the raw panic value, and cleanup still runs before the response or
re-panic decision.

## HTTP, TLS, and stream boundaries

| Area | Current boundary |
| --- | --- |
| HTTP | APISIX-compatible route, service, consumer, upstream, and plugin pipeline with explicit gaps in [plugin status](plugins.md). |
| Frontend TLS | TLS 1.2/1.3, exact/wildcard/fallback SNI certificates, session tickets, and the configured client-CA policy. |
| Stream | Raw TCP with immutable route snapshots and at most one `mqtt-proxy` protocol binding. |
| Not implemented | UDP, stream TLS/mTLS, PROXY protocol, service discovery, general stream-plugin chaining, external plugin runners, WASM, XRPC, QUIC, and HTTP/3. |

WebSocket upgrades require `enable_websocket: true` and retain their HTTP
generation until the connection closes. `SIGHUP` is not an in-process reload
mechanism; provider updates use the durable generation transaction.

Protocol-specific transports remain plugin owned:

| Plugin | Implemented scope | Explicit boundary |
| --- | --- | --- |
| `kafka-proxy` | APISIX PubSub WebSocket list-offset/fetch with SASL/PLAIN and TLS options. | The raw Kafka frame bridge is an extension; external broker qualification is separate. |
| `dubbo-proxy` | Hessian2 HTTP-to-Dubbo terminal with bounded connect-only retry. | No shared persistent multiplexing or retry after request bytes are written. |
| `http-dubbo` | Dubbo 2.x fastjson adapter. | It remains distinct from the Hessian2 adapter. |
| `mqtt-proxy` | MQTT CONNECT preread and TCP stream binding. | No UDP, stream TLS/mTLS, or general stream-plugin chain. |

## Secret authority

Secret declarations in the capability manifest are runtime authority. Each
candidate or recovery attempt gets a distinct identity scoped by generation,
domain, plugin factory, resource, source, and field.

The resolver can read only the exact publication closure. Cross-generation,
cross-domain, cross-resource, and undeclared access fail before backend use.
Plaintext exists only inside `secret.Value.Use`; logs, metrics, status, errors,
and persistent records expose only redacted identity. Cache eviction and close
zero retained bytes, and incomplete close keeps the attempt reserved for retry.

## Process and release boundary

One APISIX-Go process runs per replica. Kubernetes, systemd, or another service
manager owns abnormal-exit restart, replica replacement, and rollout
availability. APISIX-Go owns journal recovery, activation, readiness,
connection draining, and graceful termination. There is no internal
supervisor/worker or listener-inheritance protocol.

The container runs as UID/GID `10001:10001` and defaults to
`/usr/local/apisix/conf/config.yaml`. `conf/config-production.yaml` is an
explicit production example used by operational validation. GoReleaser builds
Linux amd64 and arm64 archives; the qualified container workflow currently
builds Linux amd64. See the [release runbook](runbooks/production-release.md)
for evidence and publication rules.

## Architecture decisions

ADRs record intentional compatibility or security differences. Proposed ADRs
describe implemented candidate behavior but are not accepted production
divergences until owner approval is recorded in the manifest.

| ADR | Status | Decision |
| --- | --- | --- |
| [0001](architecture/adr/0001-compatibility-governance.md) | accepted | Separate APISIX compatibility from Go-native extensions. |
| [0003](architecture/adr/0003-platform-support.md) | accepted | Publish only explicitly qualified platform artifacts. |
| [0004](architecture/adr/0004-runtime-safety-boundaries.md) | accepted | Bound ambiguous stream routing and embedded Lua execution. |
| [0005](architecture/adr/0005-credential-log-redaction.md) | accepted | Redact credential material from authentication logs. |
| [0006](architecture/adr/0006-opa-resource-context-redaction.md) | accepted | Limit OPA resource context to non-secret identity fields. |
| [0007](architecture/adr/0007-bounded-acl-jsonpath.md) | proposed | Bound ACL JSONPath evaluation and evaluate every match. |
| [0008](architecture/adr/0008-request-validation-secret-schema-safety.md) | proposed | Bound secret-backed schema compilation and lifetime. |
| [0009](architecture/adr/0009-rocketmq-client-safety.md) | proposed | Patch the pinned RocketMQ client for cancellation and lifecycle safety. |
| [0010](architecture/adr/0010-bounded-ai-proxy-multi-dns.md) | proposed | Bound DNS and active-health work for `ai-proxy-multi`. |
