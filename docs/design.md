# APISIX 3.17 Design Notes

> Consolidated design documents for the Go-native APISIX 3.17 implementation.
>
> The design baseline is APISIX `release/3.17`; these notes describe intentional Go-native boundaries and separate-subsystem decisions.

## Current architecture and Task 1-11 implementation status

This section is the current cross-package design record. Source and focused
tests are the final authority for implementation detail. Files under
`docs/superpowers/plans/` record intent and execution history; they do not prove
that a planned package or feature exists.

### Compatibility and capability source of truth

Observable APISIX `3.17.0` compatibility is the default product direction.
Go-native extensions may intentionally diverge when they are declared and
evidenced; inventory parity by itself is not qualification or production
readiness.

`pkg/capability/manifest.yaml` is the only editable source for the pinned APISIX
source/image, capability factories and aliases, phases and priorities, behavior
status, evidence, qualification profiles, platform gaps, accepted divergences,
and secret declarations. It is parsed with strict YAML fields and generates:

```text
pkg/capability/manifest.yaml
  -> pkg/plugin/registry_gen.go
  -> docs/plugins.md
  -> README.md / README.zh-CN.md generated summaries
```

The generated registry and documentation projections must not be hand-edited.
Behavior support and qualification evidence are independent dimensions: a
registered or implemented factory is not automatically qualified.

### Bootstrap and immutable configuration

The production bootstrap order is:

```text
cmd/root.go
  -> capability.Load
  -> config.LoadEffective
  -> capability.NewSecretDeclarationCatalog
  -> data_encryption.NewService
  -> secret.NewGenerationSecretResolver
  -> store.OpenJournal
  -> Journal.Recover
  -> server.NewServer
  -> compiler.NewWorkerCompilerFactory
  -> server.NewGenerationEngine
  -> generation.NewCoordinator
  -> GenerationEngine.InstallRecovery
  -> listeners
  -> etcd or standalone provider
```

Journal recovery completes before listeners and providers start. Providers do
not mutate handlers, routes, plugins, or runtime resources; they submit
`generation.DesiredBatch` values through `generation.DesiredApplier`.
`generation.Coordinator` is the single in-process desired-to-published writer.

Static configuration uses one presence-aware precedence chain:
`builtin defaults -> default file -> optional override -> recognized
APISIXGO_* -> repeatable --set`. Maps merge recursively, sequences replace,
and absent, null, false, zero, and empty string remain distinct. Each effective
field retains provenance. Compatibility, security, and qualification are
orthogonal selection axes; the removed combined profile selector must not be
restored under another name. `EffectiveConfig` is defensively owned by the
compiler factory and is never published as mutable global state.

### Durable desired-to-published transaction

`pkg/generation` owns immutable values, validation, journal interfaces, and the
coordinator. `pkg/store` is the bbolt implementation of the durable generation
journal; it is not a mutable runtime resource store. The journal resides at
`<EffectiveConfig.Paths.DataDir>/apisix-go-store.db` and records desired
revisions, content-addressed artifacts, staged transactions, independent HTTP
and stream published heads, provider cursor/authority, acknowledgements, and
per-resource decisions with integrity metadata.

The only production publication order is:

```text
ApplyDesired
  -> LoadDesired and predecessor publications
  -> Engine.Prepare
  -> Journal.Stage
  -> Engine.Activate
  -> Journal.Commit (persist acknowledgement)
  -> Engine.FinalizeActivation
  -> return acknowledgement to provider
```

`Activate` may expose the candidate in memory before durable commit, so an
activation or commit failure must restore the exact predecessor bundle and then
abort the stage. Predecessor retirement begins only after commit. Finalization
transfers ownership and queues retirement without synchronously waiting for
drain. A committed cursor replay does not apply or compile again; it confirms
the exact active fence. Recovery compiles only committed published state, never
the desired head. An empty journal opens the recovery barrier without inventing
a serving generation.

`last-good` may copy only the exact bytes for the same key and domain from a
valid published predecessor. With no predecessor, invalid first-generation
input fails closed. Explicit tombstones remain deletion and never fall back.
Quarantined or fail-closed resources cannot enter a candidate publication.
HTTP and stream may have independent revisions, while the required domains of a
single commit are updated atomically.

### Pure planning, materialization, and prepared-generation ownership

`compiler.Compiler.PreparePublication` normalizes and validates resources,
builds dependencies and domain closure, refines disposition, and validates the
final publication set before any registration, client, resource, task, or
secret side effect. HTTP and stream planning chooses exact winning occurrences
before materialization; disabled and losing occurrences acquire nothing.

`WorkerCompilerFactory` defensively owns the manifest, effective config, and
trusted CA material and shares one `runtime.ResourceRegistry`. Candidate and
recovery compilation use the same ownership-transfer path. A
`PreparedGeneration` owns the entire attempt: publication, immutable metadata
and consumers, exact secret authority, generation task registry, resource
leases, HTTP/TLS snapshot, stream snapshot, and cleanup ledger. Its exported
views are defensive and carry no registration, task, resource, secret, or
cleanup authority.

Cleanup has three ordered phases, each executed in reverse registration order:

```text
quiesce tasks and plugin admission
  -> finalize runtime resources
  -> release consumer, secret, and registration authorities
```

A deadline, cancellation, task residual, or incomplete resource finalization
retains ownership and retry authority. It must not detach the generation or
advance to a later phase. Terminal cleanup revokes snapshots and detaches the
generation exactly once.

### Atomic serving bundle and leases

`server.GenerationEngine` publishes one atomic bundle with independent HTTP and
stream domain slots. Updating one slot must preserve the other slot's revision
and owner. A prepared generation retires only after it owns no active domain and
all generation leases have drained.

Every HTTP request pins the exact HTTP generation. Batch/subrequest work may
retain from a live parent lease; a successful hijack transfers a retained lease
to a wrapped connection and releases it on `Close`. TLS selection temporarily
pins the same HTTP generation. Every accepted stream connection pins its exact
router generation for the complete connection. Rollback and later activation
therefore affect new acquisition, not already-owned work.

`pkg/route` and `pkg/stream` compile detached immutable snapshots. They do not
read the journal, instantiate plugins, acquire shared resources, start tasks, or
activate listeners. `pkg/compiler` owns those effects; `pkg/server` owns atomic
activation; the persistent stream runtime owns listeners and per-connection
router acquisition.

### Panic, task, and resource ownership

Generation/shared background work uses `runtime.TaskOwner`; request and
connection child work uses `runtime.RequestTaskGroup`. Plugins receive a
compiler-created owner and provide only a fixed component. The canonical plugin
owner prefix is `plugin/<sanitized-factory>/<sha256(instance-key)>`; raw route
or resource identifiers do not enter task residuals. Shared resources use
resource-local core owners instead of borrowing a generation registry.

`TaskPlugin` error or panic fails and closes admission only for the exact plugin
owner and reports a bounded failure. `TaskCore` errors are reported without
poisoning the owner; `TaskCore` panic is not recovered by the task runtime.
`RequestTaskGroup.Wait` joins every accepted child before re-panicking the first
panic with the same identity; ordinary errors are joined. A response timeout
does not permit the handler or connection owner to return and release its
generation lease while accepted children still run.

`TaskRegistry.Stop` rejects admission, cancels, and joins accepted work. A
deadline returns a sorted, deduplicated `TaskResidualError`; it is incomplete
cleanup and a later `Stop` may finish. `runtime.ResourceRegistry` shares only an
exact `ResourceKey{Kind, Scope, Digest}` with the same Go type. Final-reference
close is retryable; while incomplete, the identity and resource remain owned
and cannot be replaced.

The production goroutine gate rejects raw `go` and type-resolved
`sync.WaitGroup.Go` only under `pkg/plugin`, `pkg/proxy`, `pkg/route`, and
`pkg/stream`. It is not a repository-wide claim; process owners outside those
roots must still have explicit lifecycle and joins.

### Shutdown phase fence

Server shutdown is an ordered, retryable state machine:

```text
stop and join the config producer
  -> reject listeners and new route leases; initiate stream close
  -> drain HTTP, generation leases, and the stream runtime
  -> close the generation engine
  -> close the generation secret resolver
  -> close the journal
  -> close metrics expiration/export, then tracing
```

An incomplete phase that still owns runtime authority, including a task
residual or deadline, must not advance to a later phase. Terminal errors are
recorded once according to the owning phase; a later `Shutdown` continues from
the first incomplete phase instead of repeating completed releases.

### Readiness and bounded observability

Readiness is internal bounded state, not a Prometheus scrape read-back.
`config_apply_ready` requires an observed, healthy provider and a successful
HTTP stage; it requires a successful stream stage only when stream is
configured. Any quarantine blocks readiness. `/livez` is independent;
`/readyz` additionally requires provider reachability in etcd mode, so a
recovered generation may keep serving while readiness is false.

Untrusted provider, stage, resource, route, backend, owner, or panic values do
not become metric labels. Dynamic HTTP/LLM/upstream series use hard-cap trackers
and the canonical `__overflow__` label. In-flight series are not evicted before
release, and stream route metric references remain until the last overlapping
generation that can emit a terminal observation retires.

Plugin callback panics are classified as bounded `plugin.PanicError` values;
the public error omits the raw panic value. Exact `http.ErrAbortHandler` retains
its sentinel semantics, downstream/core panic is not attributed to a plugin,
and all lifecycle finalizers run before the selected response or re-panic.
Unknown core panic currently escapes the route handler after cleanup but is
still recovered by the standard `net/http.Server` connection boundary.

### Secret authority

Secret declarations in the capability manifest are runtime authority, not
documentation metadata. The catalog digest binds encryption, materialization,
and compiler construction. Each candidate or recovery attempt receives a
distinct identity and scopes access by generation, attempt, domain, factory,
resource kind/id, declaration source, and canonical field.

The resolver can read only defensive bytes in the exact publication closure;
it has no journal or global Store lookup. Cross-generation, cross-attempt,
cross-domain, cross-resource, and undeclared access fails before contacting a
backend. Decryption precedes `$ENV://` or `$secret://vault/...` resolution, and
Vault configuration must come from the same domain closure. Plaintext is
temporarily exposed only through `secret.Value.Use`; persistent observation is
limited to redacted digest/descriptor identity. Cache eviction, expiry, and
close zero retained bytes. An incomplete close preserves attempt identity and
authority for retry.

### Implemented versus planned-only boundary

The immutable generation compiler, in-process `Server + GenerationEngine`,
lease-aware retirement, and retryable task/resource cleanup are implemented.
The external supervisor/exec-worker architecture, IPC activation protocol,
listener inheritance, worker probation/restart policy, and cross-platform
lifecycle packages described by the supervisor child plan are not implemented.
There are currently no `pkg/supervisor`, `pkg/worker`, `pkg/lifecycle`, or
`pkg/platform` packages.

The repository has existing security/release workflows and amd64 image, SBOM,
Trivy, signing, attestation, and operational evidence gates. The proposed
next-generation qualification subsystem, APISIX-oracle evidence bundle,
multi-architecture OCI promotion contract, and native macOS/Windows artifacts
remain planned-only; there is currently no `pkg/qualification`,
`qualification/policy.json`, or `.github/workflows/qualification.yml`.

## Configuration, container, and release gates

Runtime configuration has one presence-aware precedence chain:
`builtin defaults -> default file -> override file -> APISIXGO_* -> repeatable --set path=value`.
The base file is always `conf/config-default.yaml`; `-c/--config` selects an
optional override. APISIX `${{NAME}}` and `${{NAME:=fallback}}` templates expand
inside each parsed file layer before that layer enters the merge. Maps merge
recursively, lists/sequences replace, and an explicit `null` replaces the lower
layer; absent, `null`, `false`, zero, and empty string remain distinct presence
states. Only recognized `APISIXGO_*` aliases are applied as static overlays, and
repeatable `--set path=value` flags are applied last.

`config.LoadEffective` validates the result and returns one immutable
`config.EffectiveConfig`; bootstrap performs explicit dependency injection along
`EffectiveConfig -> data_encryption.Service -> explicit resolver dependency`
before constructing the server and plugins. Configuration is not published as
process-global state. The HTTP plugin allowlist and listener set must be
non-empty, listeners must be valid, proxy connection limits must be positive,
and the effective `etcd` provider must have endpoints and a prefix. The startup
plugin list comes from this validated effective configuration rather than
mutable package-global state. See the
[static-configuration program specification](superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md)
and the [central capability manifest](../pkg/capability/manifest.yaml).

Startup logs only the bounded `config.CapabilitySummary`: debug mode, bounded
role/provider values, listener and plugin counts, protocol-mode booleans, the
etcd endpoint count, and whether proxy limits are configured. It excludes
addresses, credentials, keys, certificates, tokens, and secret references.

The runtime image is pinned to Alpine 3.24.1, contains the CA bundle without a
`curl` dependency or built-in Docker healthcheck, and runs as UID/GID 10001.
Its default command uses `/usr/local/apisix/conf/config-production.yaml`, which
intentionally fails closed until an operator supplies a real etcd endpoint. The executable
`scripts/container_smoke.sh` mounts a generated standalone
`/usr/local/apisix/conf/config.yaml` and explicitly passes
`-c /usr/local/apisix/conf/config.yaml` after the image name, overriding the image
default for the smoke. This keeps the smoke on the same merge rules as local
execution while testing its generated configuration. The smoke serializes
real-process runs, starts an isolated upstream and standalone route, waits for
a proxied response, verifies the runtime UID, sends `TERM`, and
requires exit code zero. Run its static contract locally with `bash
scripts/container_smoke_test.sh`; run the real smoke only on a host with
Docker.

`.github/workflows/security-release-gates.yml` runs the focused race suite and
the pinned `govulncheck` scanner. Its read-only `container-evidence` job builds
and loads one `linux/amd64` image, reuses it for the non-root container smoke,
CycloneDX SBOM, and fail-closed Trivy scan, then uploads
`sbom.cdx.json`, `trivy.json`, `apisix-image.tar.gz`, and
`rollback-metadata.json`. A separate guarded `publish-image` job downloads and
checks that exact archive; only it has registry, OIDC, and attestation write
permissions, references the protected `production-release` environment, and
pushes/signs/attests the captured immutable registry digest. The reusable
workflow and its final caller both grant `attestations: write` where required;
PR/master and RC paths remain read-only.

Each qualification workflow resolves its selected ref once and all jobs use
that immutable commit. RC and final runs build separate, digest-bound artifacts
and each reruns the complete gate set; RC evidence is not relabeled or mixed
with final-release evidence. The final workflow verifies all evidence identities,
creates a checksum manifest, and attaches the evidence bundle and checksum to
the GitHub release so the qualification record does not expire with Actions
artifact retention.

The rollback file binds the selected source ref and actual `git rev-parse HEAD`
to the immutable image identity and artifact checksums. Before rollback,
operators download the evidence bundle, verify every recorded checksum, run
`gzip -dc apisix-image.tar.gz | docker load`, confirm the loaded image ID, and
redeploy the previous digest through the existing deployment process. The
metadata is evidence, not a new deployment controller. The canonical soak
stores real JSON from `go test -json ... | tee
.cache/release-evidence/proxy-soak.json`; `.cache/telemetry` is optional and is
not evidence unless a producer populated it. Local deterministic checks are:

```bash
bash scripts/container_smoke_test.sh
bash scripts/release_metadata_test.sh
bash scripts/release_gate_test.sh
```

### Candidate HTTP data-plane profile and lifecycle

#### Historical behavior before convergence: candidate profile

`deployment.profile` accepts either the empty compatibility value or the
strict `http-data-plane-v1` candidate. The strict profile is an ordered,
HTTP-only contract: it uses the six-plugin allowlist
`request-id`, `cors`, `key-auth`, `jwt-auth`, `basic-auth`, `prometheus`,
disables Admin and stream listeners/plugins, requires data-plane etcd over
verified HTTPS, a trusted source CIDR, a positive request-body limit, and no
process access-log claims. It excludes Kafka PubSub and upstreams with
`scheme: kafka`; the Kafka owner remains available in empty compatibility mode.
Its full operator contract is in
[`production-profile.md`](production-profile.md) and the
[`production release runbook`](runbooks/production-release.md); it is awaiting
post-merge RC/final release and operations evidence and does not change the
repository-wide not-ready status. The first release has no previous immutable
digest, so rollback qualification remains open until a distinct older
published digest is exercised.

> **Superseded 2026-08-23:** the governing design has three independent
> selection axes and manifest-derived qualification. See the
> [program specification](superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md),
> [compatibility contract](architecture/compatibility-contract.md), and
> [legacy conflict ledger](architecture/legacy-conflicts.md). The text above is
> retained only as historical evidence of the pre-convergence candidate.

NGINX HTTP and stream process access-log settings are unsupported in both the
compatibility and candidate profiles: any explicitly non-zero boolean or
numeric value, or non-empty string value, fails during configuration load.
Route/plugin loggers are the supported compatibility/general-plugin request-
logging mechanism. The exact six-plugin `http-data-plane-v1` allowlist contains
no request logger and makes no request-logging egress claim. Elasticsearch,
ClickHouse, and Tencent CLS loggers require a non-empty effective flat-string
`log_format` from effective resource/plugin configuration or plugin metadata
before acquiring clients or creating processors; effective resource/plugin
configuration wins over plugin metadata.

Prometheus admits HTTP `http_status`, `http_latency`, and `bandwidth` tuples
through three independent budgets. `plugin_attr.prometheus.max_http_series`
defaults to `10000` and accepts integers from `100` through `100000`. Existing
tuples remain unchanged across route reloads; after a family reaches its limit,
unseen tuples retain bounded family labels (`code` for status, `type` for
latency/bandwidth, plus `request_type` and status `response_source`) and
replace dynamic labels with `__overflow__`, incrementing
`<metric_prefix>http_metric_series_overflow_total{metric}`. With the default
`metric_prefix: apisix_`, this is
`apisix_http_metric_series_overflow_total{metric}`. Budgets and their admitted
series are initialized once for the process and are not reset by route reload.

The loader retains recognized compatibility fields, but explicit activation of
unsupported Admin, top-level discovery, external-plugin commands, WASM, XRPC,
QUIC, or HTTP/3 fails closed. Route/upstream discovery fields remain decodable
for migration diagnostics and are rejected by HTTP or stream compilation when
they would require discovery runtime behavior. Frontend HTTPS serving is part
of the implemented TLS boundary; direct Internet exposure still requires that
frontend TLS boundary or a trusted TLS-terminating ingress whose source CIDRs
are configured.

#### Current lifecycle and publication

WebSocket upgrades are admitted only when the effective route or service sets
`enable_websocket: true`. They skip response callbacks while the request,
authentication, access, before-proxy, and log phases run. A successful hijack
retains the exact HTTP generation through the wrapped connection, so replacing
or rolling back a serving bundle affects only new acquisitions. Cluster
admission and timeout limits continue to apply. `SIGHUP` remains unsupported as
a process-reload mechanism; normal provider changes use the in-process durable
generation transaction described above.

Publication no longer mutates runtime rows and invokes a route builder. The
provider submits raw desired resources, the journal persists the desired
revision, and the compiler determines a complete candidate with exact
per-resource decisions. Malformed route-scoped resources may be quarantined
according to policy; resources that fail closed never enter the candidate.
Stream candidate errors and conflicting listen addresses fail preparation for
that domain. A failed prepare, stage, activation, or commit retains or restores
the prior active bundle. Explicit deletion is a tombstone and cannot reuse
last-good. URI registration and plugin materialization are attempt-owned and
roll back with the prepared generation.

#### Historical behavior before convergence: lifecycle

WebSocket upgrades are admitted only when the effective route or service sets
`enable_websocket: true`. Every WebSocket upgrade attempt skips response
callbacks while request, authentication, access, before-proxy, and log phases
run. For the candidate profile, a successful profile-allowed HTTP
reverse-proxy tunnel remains subject to cluster admission and timeout limits;
Kafka PubSub compatibility tunnels are outside that contract. Retiring a route
generation closes its WebSocket connections. `SIGHUP` drains the server and
returns an unsupported-reload error, so configuration changes require a new
process rather than an in-process reload. Zipkin is v2-only, and OTel rejects
`set_ngx_var` and non-zero `inactive_timeout` while retaining collector
`request_timeout`.

> **Superseded 2026-08-23:** route retirement and `SIGHUP` above describe the
> pre-convergence implementation limitation, not the governing target. The
> later [supervisor-generation child plan](superpowers/plans/2026-08-23-supervisor-worker-platform.md)
> targets generation handoff that preserves ordinary hijacked connections.
> See the [program specification](superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md),
> [compatibility contract](architecture/compatibility-contract.md), and
> [legacy conflict ledger](architecture/legacy-conflicts.md).

#### Historical behavior before convergence: route schema

Route publication uses one pre-materialization entrypoint,
`validateRouteCompatibility`, for the Go data-plane compatibility subset.
It does not import the full pinned APISIX 3.17 route schema. The subset
accepts bare routes (`uri` without methods, plugins, or upstream) and
empty `vars` / `remote_addrs`. Explicit deviations: `script`,
`script_id`, `filter_func`, non-empty `vars`, `remote_addr`, and
non-empty `remote_addrs` are rejected. Empty `hosts` fail closed, and
invalid wildcard host patterns are rejected before publication. Plugin
materialization, secret ownership, and upstream resolution stay on the
existing post-entrypoint path.

> **Superseded 2026-08-23:** the governing route target is the pinned observable
> APISIX contract with explicit gap accounting, not preservation of this subset
> as the final compatibility boundary. See the
> [program specification](superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md),
> [compatibility contract](architecture/compatibility-contract.md), and
> [legacy conflict ledger](architecture/legacy-conflicts.md). The text above is
> retained as historical implementation evidence until the later HTTP
> compatibility child plan converges it.

## APISIX 3.17 Protocol Bridge Design

> Status: design baseline plus bounded Dubbo/Kafka slices and a TCP/MQTT stream owner, 2026-07-12
> This document is the contract for implementing the protocol plugins in
> [`plugins.md`](plugins.md).

The current Go runtime has an HTTP `http.Handler` pipeline and also owns a
bounded raw-TCP stream listener/route snapshot with cancellation and result/log
callbacks. Stream startup is fail-closed: stream mode requires at least one
TCP listener, and unsupported UDP, TLS, PROXY protocol, unresolved upstream,
and unsupported plugin configuration is rejected before the server begins
serving. HTTP route-scoped failures follow the quarantine contract above;
invalid stream generation preparation or activation is rejected without
replacing the last-good stream runtime. It does not yet expose a general
stream-variable/plugin-chain API, stream-specific active health/discovery,
TLS/UDP stream owner, or Kafka-specific stream binding. HTTP cluster active
health is implemented separately under a resource-local task owner. The runtime
exposes `/livez` and `/readyz`; startup failures are
returned to the process entrypoint, and readiness remains unavailable until
configuration and the configured provider are ready. Protocol-owned bounded
transport and stream boundaries therefore have different integration states:

| Plugin | Current local behavior | Design consequence |
|---|---|---|
| `kafka-proxy` | Stores SASL/PLAIN settings in request context, owns the official PubSub protobuf WebSocket command loop for list-offset/fetch, and uses a bounded `kafka-go` consumer abstraction with upstream TLS verification plus inline or local SSL-resource client certificates; an in-process TLS wire fixture verifies the actual PLAIN handshake and broker auth error; the raw-frame WebSocket bridge remains a compatibility extension. | Keep external broker smoke coverage optional; the raw bridge must not be counted as APISIX 3.17 Kafka parity. |
| `dubbo-proxy` | Stores service name/version/method in request context and now has a Hessian2 HTTP-to-Dubbo terminal with route upstream selection, bounded connect-only retries, and passive health outcome reporting through `pkg/proxy`. | Keep persistent shared-connection multiplexing, response-ID matching, retry after request write, active probes, and native health lifecycle separate from the bounded per-target gate. |
| `http-dubbo` | Builds a Dubbo 2.x fastjson request and calls a selected TCP upstream, with bounded connect-only retries and passive health outcome reporting through `pkg/proxy`. | Keep this fastjson adapter separate from the hessian2 adapter used by `dubbo-proxy`; never retry after request bytes are written or invent active probe state in the codec. |
| `mqtt-proxy` | Validates config, exposes a bounded MQTT 3.1.1/5.0 CONNECT parser, provides plugin-owned `ServeStream`/`ServeListener`, and is now bound to the main TCP stream-route owner with `server_addr`/`server_port`/`remote_addr` matching, bounded cancellation/backpressure, and `StreamInfo` result callbacks; the HTTP handler remains a compatibility no-op. | Retain TLS/mTLS, UDP, and general stream-plugin-chain behavior as separate scope. |

The official APISIX documentation confirms these boundaries: `kafka-proxy`
configures a `kafka` upstream and currently supports SASL/PLAIN, `dubbo-proxy`
uses hessian2 with `Map<String, Object>` request/response data, `http-dubbo`
uses fastjson for Dubbo 2.x, and `mqtt-proxy` is an L4 stream plugin that
exposes `mqtt_client_id` for consistent hashing.

References: [kafka-proxy](https://apisix.apache.org/docs/apisix/plugins/kafka-proxy/),
[dubbo-proxy](https://apisix.apache.org/docs/apisix/plugins/dubbo-proxy/),
[http-dubbo](https://apisix.apache.org/docs/apisix/3.11/plugins/http-dubbo/),
[mqtt-proxy](https://apisix.apache.org/docs/apisix/plugins/mqtt-proxy/).

### Frontend TLS and HTTP timeout boundary

The HTTPS listener builds one strict `tls.Config` before binding any listener.
Only `TLSv1.2` and `TLSv1.3` may be configured; TLS 1.2 cipher names are
limited to the six Go-enforceable ECDHE suites in `conf/config-default.yaml`.
Session tickets, dynamic exact/wildcard/fallback SNI certificate selection,
and the optional global `ssl_trusted_certificate` client-CA policy are applied
to real handshakes. Per-SNI client-auth policy, custom upstream CA bundles,
TLS 1.3 cipher selection, and stream TLS/mTLS remain outside this contract.

`nginx_config.http.send_timeout` is rejected when non-zero. Go's
`http.Server.WriteTimeout` is an absolute response deadline and cannot express
NGINX's write-idle semantics, so it is never populated from that directive.

### Shared transport contract

Protocol-owned boundaries are introduced in the plugin packages; do not make
`pkg/proxy` know about Kafka, Dubbo, or MQTT. The request/response boundary
should provide:

```text
Dial(ctx, target, options) -> Conn
Conn.RoundTrip(ctx, request) -> response       # request/response protocols
Conn.Copy(ctx, client, upstream)                # bidirectional stream protocols
Conn.Close()                                    # idempotent cleanup
```

The concrete Go API may differ, but these invariants are required:

1. Every dial, write, read, and queue wait has a bounded deadline derived from
   route/upstream timeout settings.
2. Cancellation closes both directions and releases the connection slot.
3. Protocol errors are distinguishable from transport errors so the route can
   return a deterministic client (4xx) or upstream (5xx) response.
4. Raw request bytes are replayed exactly when a preread parser is used; the
   parser may inspect bytes but must not consume data that the upstream should
   receive.
5. No plaintext credential is included in errors, logs, context dumps, or
   serialized route status.

Each protocol implementation owns its fake server/client fixture. Do not add a
real broker or external service to the default test suite.

### Kafka proxy (`kafka-proxy`)

#### Contract decision

The APISIX plugin is an upstream Kafka consumer bridge, not a REST producer
facade. The following Kafka owner is available in compatibility mode; it is
outside the `http-data-plane-v1` candidate profile. Its official Go scope is
therefore:

- support `scheme: kafka` upstream nodes and the APISIX WebSocket PubSub
  protobuf protocol;
- decode `PubSubReq` and preserve `sequence` in `PubSubResp`;
- implement `cmd_kafka_list_offset` and `cmd_kafka_fetch`, including Kafka
  message offset/timestamp/key/value fields;
- attach the resolved SASL/PLAIN credentials to the Kafka consumer dialer;
- map malformed PubSub messages, Kafka errors, timeouts, and authentication
  failures deterministically without leaking credentials;
- defer HTTP-to-Kafka REST semantics and SASL mechanisms beyond PLAIN.

The existing raw length-prefixed Kafka WebSocket bridge remains available only
as a bounded compatibility extension. It is not the official APISIX protocol
and must not be used as the parity acceptance criterion.

#### Runtime flow

```text
HTTP WebSocket route
  -> upgrade and create the APISIX PubSub session
  -> decode one bounded PubSubReq per binary WebSocket message
  -> execute list-offset/fetch against the configured Kafka brokers
  -> encode PubSubResp with the original sequence
  -> map protocol, broker, timeout, and auth errors without leaking secrets
```

The plugin must not open a new connection for every message if the route is a
long-lived stream. Connection reuse is allowed only after request IDs and
concurrent in-flight limits are explicit; otherwise use one bounded connection
per proxied exchange.

#### Acceptance tests

- [x] a fake broker receives the exact request frame and returns an exact
  response;
- [x] an oversized response frame is rejected before unbounded allocation;
- [x] context cancellation closes the transport without a goroutine leak;
- [x] a `scheme: kafka` route upgrades a WebSocket and forwards exact raw Kafka
  frames through the selected upstream as a compatibility extension;
- [x] the bounded PubSub protobuf codec round-trips command envelopes,
  sequence values, error/pong responses, and Kafka message fields;
- [x] a `scheme: kafka` route accepts a protobuf `cmd_kafka_list_offset` request
  and returns a matching `kafka_list_offset_resp`;
- [x] a `scheme: kafka` route accepts a protobuf `cmd_kafka_fetch` request and
  returns bounded Kafka messages with offset/timestamp/key/value;
- [x] a configured SASL/PLAIN consumer receives the resolved username/password
  through the consumer factory boundary;
- [x] malformed PubSub messages, fake broker/auth failures, and timeouts map
  deterministically without leaking the password;
- [x] upstream `tls.verify` and inline `client_cert/client_key` or local
  `client_cert_id` SSL resources configure the Kafka dialer, while
  mismatched/invalid pairs and missing resources fail explicitly;
- [x] an in-process TLS broker-wire fixture proves SASL/PLAIN negotiation,
  verifies the exact credentials payload, and maps a broker authentication
  error without exposing the password.

External broker smoke tests remain optional; mechanisms beyond PLAIN are not
part of the APISIX 3.17 plugin schema. The shared secret resolver is already
the credential boundary for the supported PLAIN path.

### Dubbo proxy and HTTP-Dubbo

#### Adapter decision

Use one shared invocation lifecycle with two codecs:

| Adapter | Wire codec | Input/output contract |
|---|---|---|
| `http-dubbo` | Dubbo 2.x fastjson | Existing `params_type_desc`, newline-separated fastjson values, and serialized-body escape hatch. |
| `dubbo-proxy` | Dubbo hessian2 | HTTP headers/body map to `Map<String, Object>`; response map becomes HTTP status, headers, and body. |

Do not silently switch the existing `http-dubbo` implementation to hessian2;
that would break its documented fastjson contract. A future shared package can
own framing, request IDs, deadlines, and connection lifecycle while codecs stay
independent.

#### Invocation lifecycle

1. The route selects an upstream target through the existing load balancer.
2. The adapter validates the method/service configuration and converts the HTTP
   request to a codec-specific invocation.
3. The transport assigns a request ID, writes one bounded frame, and waits for
   the matching response under connect/write/read deadlines.
4. The adapter maps a successful response to HTTP. Application exceptions,
   malformed frames, and transport failures remain distinguishable.
5. The request body is restored only when a downstream HTTP handler still needs
   it; terminal protocol handlers must not leave a consumed body in shared
   middleware state.

#### Multiplexing, retry, and health

- `upstream_multiplex_count` is currently enforced as a per-target upper bound
  on in-flight request terminals; acquisition is cancellation-aware.
- The current Go terminal opens one connection per invocation, so persistent
  shared-connection multiplexing and response-ID matching are intentionally
  deferred until a reusable connection owner exists.
- Retries are opt-in and only allowed before bytes are written, or for a
  request explicitly classified idempotent. Never duplicate an unknown Dubbo
  invocation after a partial write.
- Passive health outcomes use the shared `pkg/proxy` load-balancer abstraction:
  HTTP status, TCP failure, and timeout thresholds can quarantine observed
  nodes, and an exhausted pool fails open. Active probe cadence, cross-worker
  state, and `/v1/healthcheck` remain outside the bounded protocol terminals.
  Do not emulate NGINX/Tengine health state inside a plugin.

#### Acceptance tests

- [x] hessian2 map request/response round trip and route-terminal mapping for `dubbo-proxy`;
- existing fastjson request/response behavior remains green for `http-dubbo`;
- [x] concurrent requests stay within the configured per-target in-flight gate;
- persistent shared-connection multiplexing and response-ID matching remain
  deferred until a reusable connection owner exists;
- malformed frame, application exception, upstream close, connect timeout,
  read timeout, cancellation, and retry-after-partial-write cases;
- response headers/body/status mapping and error redaction.

The implementation uses the Apache `github.com/apache/dubbo-go-hessian2`
v1.13.1 module for the wire codec; its dependency and license are explicit in
`go.mod`. Persistent connection pooling remains a separate follow-up.

### MQTT proxy (`mqtt-proxy`)

#### Stream prerequisite

`mqtt-proxy` must not be implemented as an HTTP handler. The plugin-owned
`ServeStream`/`ServeListener` boundary accepts a TCP connection, prereads
CONNECT, selects an upstream through a dialer, and owns the connection lifetime.
The main server now supplies the listener, route selection, weighted TCP or
deterministic `chash` upstream owner, cancellation, and result/log callback.
For `key=mqtt_client_id`, the parsed client ID is the hash input and the peer
address is the fallback. General stream variables and
other stream-plugin chaining remain separate contracts. The HTTP route builder
is not involved; the detached stream snapshot is planned and materialized
through the compiler and activated independently by the generation engine.

The future general stream-plugin interfaces are:

```text
StreamRoute.Match(listener, preread) -> route/config
StreamHandler.Serve(ctx, clientConn, upstreamTarget, config)
StreamVariable.Set("mqtt_client_id", value)
```

The current plugin-owned equivalents are `Plugin.ServeStream` and
`Plugin.ServeListener`; the main runtime wraps them with the listener, route
snapshot, upstream dialer, cancellation, and `Result` callback. The general
variable/plugin-chain interfaces remain intentionally unimplemented.

#### CONNECT preread

The protocol-owned parser in `pkg/plugin/mqtt_proxy/connect.go` now validates the
fixed header, remaining-length encoding, protocol name/level, CONNECT flags,
MQTT 5 properties, UTF-8 fields, and payload boundaries. It returns the exact
CONNECT packet length and exposes `ClientIDOrPeer` for the stream dialer. The
parser is deliberately independent of `net.Conn`, so it can be unit-tested
without a broker and reused by the plugin-owned preread boundary.

The MQTT stream owner reads only a bounded prefix (enough for the fixed header,
remaining-length field, protocol name/level, and client ID), validates:

- protocol name (default `MQTT`);
- protocol level (`4` for MQTT 3.1.x or `5` for MQTT 5.0);
- remaining-length encoding and CONNECT flags;
- non-empty client ID when the protocol requires one.

The inspected bytes are replayed to the upstream unchanged. A missing client ID
falls back to the peer address for consistent hashing, matching the official
plugin behavior. `mqtt_client_id` is available to the stream load balancer and
stream log context, not to ordinary HTTP request variables.

#### Connection lifecycle and errors

- connect/read/write deadlines are bounded by stream route settings;
- both `io.Copy` directions stop on client close, upstream close, or context
  cancellation;
- malformed CONNECT is rejected and the connection is closed without dialing an
  upstream;
- upstream selection failure and upstream disconnect are recorded as stream
  errors, with no HTTP status invented for a raw TCP client;
- protocol-level 4 and 5 are validated, but MQTT payloads are not decoded or
  rewritten by the gateway.

#### Main-server startup boundary

The main stream owner currently supports raw TCP routes and the
`mqtt-proxy` route plugin only. The configured upstream `read` timeout bounds
each forwarding direction, with a 60-second default when it is unset. Stream
mode fails before HTTP serving when the TCP listener set is empty, a UDP
listener is configured, a listener or upstream requests TLS or PROXY protocol,
an upstream reference cannot be resolved, a route uses an unsupported stream
plugin, or a listener cannot bind. Runtime construction is transactional across
listeners, and a later Prometheus or HTTP startup error closes and clears the
stream runtime created by that startup attempt. General stream plugin chaining,
stream TLS/mTLS, UDP forwarding, and PROXY protocol remain outside this bounded
contract. Stream-stage readiness and generation-aware stream route/connection
metrics are implemented.

#### Acceptance tests

- [x] MQTT 3.1.x and 5.0 CONNECT preread with byte-for-byte replay in the
  plugin-owned stream boundary;
- [x] malformed fixed header, invalid remaining length, wrong protocol level,
  and invalid flags are rejected before upstream dialing;
- [x] client ID is exposed to consistent hashing; absent ID uses peer address;
- [x] bidirectional payload forwarding, client/upstream close, and cancellation
  in the plugin-owned stream boundary;
- [x] main-server stream-route selection and MQTT `StreamInfo` result/log
  context;
- [x] explicit bounded-backpressure assertion: a large client write to a
  non-reading upstream is released by runtime cancellation;
- [x] no HTTP middleware or request-body assumptions are involved.

### Delivery order

- [x] Add fake network fixtures and the smallest shared deadline/cancellation
  helpers.
- [x] Complete the existing `http-dubbo` error/timeout branches without changing
  its fastjson wire contract.
- [x] Add the hessian2 `dubbo-proxy` adapter and route terminal integration.
- [x] Add the plugin-owned bounded Kafka raw-frame transport and fake-broker
  coverage as a compatibility extension; it is not the official PubSub owner.
- [x] Add the official PubSub protobuf WebSocket owner, Kafka list-offset/fetch
  consumer, SASL/PLAIN dialer, and deterministic error mapping.
- [x] Add the plugin-owned MQTT preread/consistent-hash dialer boundary and
  listener lifecycle.
- [x] Add a main-server TCP stream-route owner for listener configuration,
  route matching, upstream selection, MQTT binding, cancellation, and result
  callbacks; Kafka uses the separate HTTP WebSocket owner above.
- [ ] Only then consider multiplexing optimizations, provider-response retries,
  and optional real broker integration tests; extra Kafka SASL mechanisms are
  outside the APISIX 3.17 plugin schema, while the bounded `http-dubbo`
  connect-only retry is already part of the route terminal.

No plugin should be marked complete while it only accepts schema, stores
context metadata, or runs an HTTP no-op.

### HTTP plugin phase closure

The HTTP route runtime now uses explicit request, buffered response, streaming,
protocol, log, and lifecycle owners. Every registered factory key is mapped to
one audited capability identity; `otel` is the only factory alias. Production
route materialization rejects an unknown or legacy request owner instead of
installing a generic post-`next` handler.

One outer route response capture records the final outcome and optional bounded
body. The request pipeline prepares the union of effective static and resolved
consumer log policies before the terminal, then seals one detached request and
registers one composite lifecycle finalizer. Log callbacks receive private
snapshot copies in global-then-merged priority order. Tracer completion and
request metrics use lifecycle finalizers, while generation and separate-system
owners remain outside per-request execution. The server publishes the final
outcome and completion time before finalization and recycles pooled request
variables only after every finalizer returns.

---

## APISIX 3.17 `proxy-cache`：磁盘 Zone 与 Stale 行为设计

> 状态：版本化磁盘存储、PURGE、跨实例加载、过期/配额清理、memory-zone 共享与代际隔离、`graphql-proxy-cache` 共享 zone、严格 zone 校验，以及 identity-aware `cache_control`/`cache_set_cookie` 已实现。生产配置由 immutable generation materialization 持有；`RefreshConfiguredZones` 仅是 direct-package/test compatibility seam，不是生产 reload owner。（2026-08-26）
>
> 相关实现：[`pkg/plugin/proxy_cache/plugin.go`](../pkg/plugin/proxy_cache/plugin.go)
>
> 上游参考：[`disk_handler.lua`](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/proxy-cache/disk_handler.lua)、[`memory_handler.lua`](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/proxy-cache/memory_handler.lua)

### 1. 当前事实与边界

- 插件已经支持 `cache_key`、方法/状态过滤、绕过与 no-cache、memory zone 的 `Cache-Control` TTL/请求 freshness（含 identity-bearing `cache_key` 时按 APISIX 规则关闭该行为）、disk zone 对 `cache_control` 的忽略、memory-only `cache_set_cookie`、`Vary`、`PURGE`、消费者隔离和 `Apisix-Cache-Status`。
- `pkg/config/types.go` 能读取 `apisix.proxy_cache.cache_ttl` 和 `zones`；compiler materialization 会对 generation-owned 配置做基础校验（重复/空名称、size/path、cache_levels、未知引用和 cache strategy/zone 存储类型匹配），并把声明的 memory zone 接入共享存储。严格 cache 初始化错误会使 owning route 按 publication policy quarantine/fail closed，不能静默丢失缓存插件。
- 配置了绝对 `disk_path` 的 `cache_strategy = "disk"` 会使用版本化磁盘 envelope，并在插件实例间按摘要路径重新加载；未配置 zone 时仍保留进程内 memory fallback。
- 访问发现条目已过期时，会同时删除对应的内存副本和磁盘文件；写入磁盘条目后会按 zone 的 `disk_size` 删除过期文件和最旧文件；磁盘 lookup 最多每分钟触发一次受界扫描，配置的 disk zone 另有绑定插件生命周期、可停止的后台过期清理线程。
- 现有 `lookup` 保留过期条目并返回 `EXPIRED`；请求侧 `max-age`、`max-stale`、`min-fresh` 不满足时返回 `STALE`，随后重新请求上游。当前没有 stale-if-error 或过期内容兜底响应。
- `graphql-proxy-cache` 复用相同的 zone 存储 envelope 和过期生命周期；磁盘策略按上游 `Cache-Control: s-maxage/max-age` 或 `Expires` 计算 TTL、无响应头时回退到插件 `cache_ttl`，并与 memory 策略一样始终拒绝 `private`/`no-store`/`no-cache` 响应，而其公开 purge 路径、缓存键格式和 GraphQL mutation bypass 必须保持兼容。
- 生产 runtime context 从 immutable generation 的 `EffectiveConfig` 取得完整 zone snapshot；已有插件实例持有自己的代际并通过引用计数独立排空。`RefreshConfiguredZones` 保留为 direct-package/test compatibility seam：它能校验并原子替换测试用 snapshot，但没有 production 调用点，不能作为动态 reload 契约。未声明 zone 仍保持兼容性的进程内 memory fallback。

本设计只覆盖 Go HTTP proxy 能够稳定表达的共享缓存行为，不复刻 OpenResty shared-dict、NGINX cache manager 或跨 worker 的内部生命周期。

### 2. Zone 配置契约

沿用 `conf/config-default.yaml` 的配置形状：

```yaml
apisix:
  proxy_cache:
    cache_ttl: 10s
    zones:
      - name: disk_cache_one
        memory_size: 50m
        disk_size: 1G
        disk_path: /var/cache/apisix/disk_cache_one
        cache_levels: 1:2
      - name: memory_cache
        memory_size: 50m
```

启动或 generation preparation 时必须完成以下校验：

1. zone 名称非空、唯一，并且只允许插件 `cache_zone` 引用已声明的 zone。
2. `memory_size`、`disk_size` 使用明确的字节单位；溢出、零值和负值拒绝启动。
3. disk zone 必须有绝对 `disk_path`；路径由本地配置提供，不能来自请求或 route/plugin 配置。
4. `cache_levels` 只允许正整数层级（例如 `1:2`），并限制总层数和单层宽度。
5. 目录创建、权限和磁盘可写性在首次使用前检查；失败时返回明确错误，不静默切换到磁盘之外的路径。

已声明的 `memory` zone 按 zone 名称和配置代际共享 entries/vary index，并通过引用计数在最后一个插件实例停止时释放；未声明 zone 仍使用兼容性的进程内 fallback。后续 generation 的定义发生变化会创建新代际，旧代际不能在仍被请求引用时提前释放。

### 3. 存储抽象与磁盘格式

先把当前 map 封装成同一接口，再增加磁盘实现，避免在插件 handler 中分叉两套缓存判断：

```text
Lookup(key, request) -> entry/status
Store(key, entry, ttl)
Purge(key)
PurgeVariants(key)
Close()
```

磁盘条目使用版本化 envelope，至少包含：`version`、状态码、响应头、响应体、写入时间、TTL、过期时间和 `Vary` 信息。文件名只由缓存 key 的摘要生成，目录层级由已校验的 `cache_levels` 生成；不得把原始 URL、header 或 consumer 名称拼入路径。

写入流程：

1. 在 zone 目录内创建临时文件，并使用受限权限写入完整 envelope。
2. `fsync` 后原子 rename 到最终文件名；rename 失败时保留旧条目并报告错误。
3. 索引只记录文件摘要、大小和过期时间；索引损坏或版本不匹配按 MISS 处理并删除孤儿临时文件。
4. 通过 per-key 锁避免同一 key 的并发写入；读取不持有全局锁。

驱逐和清理必须受 `disk_size`、条目数量、单条目最大 body 大小约束。清理线程只处理已过期或超限条目，不能在请求 goroutine 中递归扫描整个 zone。

### 4. Fresh / Expired / Stale 语义

状态机保持现有 APISIX 可见状态：

| 条件 | 状态 | 行为 |
| --- | --- | --- |
| 条目不存在 | `MISS` | 请求上游；满足条件时写入缓存 |
| 条目在 TTL 内 | `HIT` | 直接返回缓存响应并设置 `Age` |
| TTL 已过期 | `EXPIRED` | 不返回过期 body；请求上游，成功后替换条目 |
| `Cache-Control: max-age` 不满足 | `STALE` | 不返回旧 body；请求上游 |
| `max-stale` 超过允许窗口或 `min-fresh` 不满足 | `STALE` | 不返回旧 body；请求上游 |
| `only-if-cached` 且无可用 fresh 条目 | `MISS` + 504 | 不访问上游 |

除非另行定义并测试 `stale-if-error`，上游错误时不能把过期 body 当作成功响应返回。若未来增加 stale-if-error，必须新增配置/响应状态、最大 stale 窗口和上游错误白名单，不能通过隐式 fallback 开启。

响应头规则继续沿用现有实现：`Set-Cookie` 默认不缓存，memory zone 可显式启用 `cache_set_cookie`，disk zone 始终不缓存 `Set-Cookie`；`private`/`no-store`/`no-cache` 不缓存，`Vary: *` 不缓存；`hide_cache_headers` 只影响返回给客户端的缓存控制头。

### 5. 分阶段实现与验收

#### P1：zone 注册与 memory 共享

- [x] 为已声明 `memory` zone 提供线程安全的共享 registry、entries/vary index 和引用计数生命周期；配置定义变化时按代际隔离 entries，旧代际独立排空。
- [x] 将 `apisix.proxy_cache.zones` 做成基础严格校验的配置 registry，覆盖重复/空名称、size/path/cache_levels 格式和未知 zone 引用；generation preparation 会预检完整 snapshot，`RefreshConfiguredZones` 仅保留 direct-package/test compatibility。
- [x] 拒绝 plugin `cache_strategy` 与 zone `disk_path` 不匹配的配置，并拒绝 `$request_method` cache key；`graphql-proxy-cache` 复用相同的 strategy/zone 校验。
- [x] compiler materialization 对 proxy-cache/graphql-proxy-cache 的严格初始化错误停止 candidate snapshot 构建；route-scoped disposition 由 generation publication policy 决定。
- 保持当前 route/plugin 行为和 `PURGE` 结果不变。

#### P2：disk 读写

- 已实现版本化 envelope、摘要路径、原子写入、跨实例加载、PURGE、访问时过期清理、写入后的 `disk_size` 超限驱逐、流量触发扫描和生命周期绑定的后台过期清理；声明的 memory zone 也有共享生命周期和引用计数。generation snapshot、跨插件一致性和超限验收均已有对应实现或测试。
- 覆盖重启后命中、损坏文件按 MISS、并发写入、目录不可写、超限驱逐和 `PURGE`。
- 通过临时目录测试；测试结束清理文件，不依赖 `/tmp` 中的固定目录或用户 home。

#### P3：stale 与跨插件一致性

- [x] 让 `proxy-cache` 与 `graphql-proxy-cache` 共用已声明 zone 的 memory registry、disk envelope 和过期清理生命周期；两个插件仍保留各自的缓存键、PURGE 路径和请求策略。
- [x] 覆盖 `Vary` 变体、过期 index、配置 TTL、`only-if-cached`、上游错误不返回 stale body 的回归测试；官方 `graphql-proxy-cache` 不暴露 `cache_control`，不增加跨插件隐式 stale-if-error。
- 对 route/service/consumer 缓存键做跨插件隔离测试。

`RefreshConfiguredZones` 只承诺 direct-package/test compatibility 下的已校验 snapshot 替换；它不是 production reload owner，也不能据此声称完整 NGINX cache-manager 或跨 worker runtime parity。跨插件 stale-if-error 仍不会被隐式开启。

---

## APISIX 3.17 Secret Resolution Design

The current authority model is manifest-declared and generation-attempt scoped.
The former shared `pluginFields` registry and Store-wide optional resolver are
not production authority boundaries.

New writes use an explicit `$encrypted://v2:` AES-GCM envelope and authenticate
the canonical declared field context. Legacy APISIX AES-128-CBC remains
decrypt-only for migration. The keyring is newest-first; new writes never emit
the legacy format.

The capability manifest declares the factory, source (`plugin config`, `plugin
metadata`, or `consumer config`), canonical field, strictness, and target. Its
catalog digest must match the encryption service, generation materializer, and
compiler. The attempt-scoped resolver reads only the exact HTTP or stream
publication closure and rejects scope mismatch before decrypting or contacting
a backend. Secret materialization and dependency injection occur before plugin
`PostInit`; failure rolls back the complete prepared generation.

After decryption, supported environment or Vault references resolve within the
same attempt and domain. Attempt caching is bounded and time-limited; eviction,
expiry, and close zero retained bytes. Close waits for in-flight use, and an
incomplete close keeps the attempt identity reserved for retry.

Plaintext is available only inside `secret.Value.Use`. Errors, logs, metric
labels, status, cleanup ledgers, and persistent descriptors may expose only a
stable redacted category or digest. They must never contain plaintext,
ciphertext, a backend reference, or key material. Adding a secret-bearing field
therefore requires a manifest declaration plus scope-rejection, valid/invalid
ciphertext, missing-key, rotation, zeroization, and redaction coverage.

## Logger Batch Resource Ownership

Network/broker sinks that use the shared batch processor inherit a central
resource contract. `file-logger` uses a separate byte-aware processor described
below. An unset `max_pending_entries` is bounded at
10,000, and a new entry is rejected when the number of buffered, queued, active,
and retrying entries is already at that limit. The detached production
`RunLogPhase` path never waits for a delivery worker; rejection is the overload
policy. The legacy direct `Handler` compatibility path is the explicit
exception: it flushes and waits briefly so callers can read the file when
`ServeHTTP` returns.

Flushes are consumed by one delivery worker per processor by default rather
than starting one goroutine per batch. Each delivery attempt has a 10-second
context deadline, retry delay is cancellable, and sinks attach that context to
HTTP, broker, dial, and socket operations. The internal worker limit can be
overridden only by trusted constructors and is capped at eight; it is not a new
route-schema field.

The processor owns fixed `batch-scheduler`, `batch-worker`, and
`batch-shutdown` task components. Shutdown refuses later pushes, seals the
current buffer, and allows at most 15 seconds for workers to drain before
canceling delivery. A timeout returns an exact residual and retains sink/client
authority. Shared clients, senders, and retained connections are released only
after every admitted callback has actually returned; a later cleanup retry
finishes the close.

`batch_process_entries` retains its APISIX-compatible buffered-entry meaning.
`logger_batch_pending_entries` reports all accepted nonterminal entries, while
`logger_batch_events_total` records only validated capacity, stopped, delivery,
and shutdown outcomes. Dynamic gauges are refcounted across overlapping route
generations and are deleted only after the final processor owner closes; event
counters use the stable plugin identifier and a bounded outcome label.

`file-logger` owns one `file-log-writer` task and enforces both entry and payload
byte bounds. Generations sharing a canonical path lease one 64 KiB buffered
writer. The process-local writer epoch owns
`core/file-writer-registry/signal-watch`; final lease release stops and joins
that watcher before flushing and closing the writer. `SIGUSR1` reopen is
serialized as flush old descriptor, reopen, then allow later writes. A process
crash can still lose bytes in the one-second application buffer.
