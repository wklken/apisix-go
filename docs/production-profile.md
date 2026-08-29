# Candidate HTTP data-plane profile

`http-data-plane-v1` is a conservative operator contract for running
`apisix-go` as an HTTP data plane behind a separately managed control plane and
ingress boundary. It covers the 110 plugins in the qualified APISIX 3.17
all-plugin profile that declare the HTTP domain, in the same manifest order.
The stream-only `mqtt-proxy` remains qualified by the all-plugin profile but is
outside this first HTTP production scope. The profile is a candidate awaiting
post-merge functional and runtime-stability qualification. The repository-wide
warning still applies: this bounded profile does not make the whole apisix-go
project production ready. The executable qualification procedure is the
[production release runbook](runbooks/production-release.md); a workflow
definition or an unrecorded local test is not qualification evidence.

## Selection

Select the compatibility target, security policy, and qualification contract
independently in the merged runtime configuration:

```yaml
compatibility_target: apisix-3.17
security_profile: compat
qualification_profile: http-data-plane-v1

deployment:
  role: data_plane
  role_data_plane:
    config_provider: etcd
  etcd:
    host:
      - https://etcd-1.example.invalid:2379
    prefix: /apisix
    tls:
      verify: true
```

The deployment and etcd example above remains the profile-selection shape. A
deployment may add the following separate operator-owned runtime-path overlay:

```yaml
apisix_go:
  runtime_paths:
    data_dir: /var/lib/apisix-go
    runtime_dir: /run/apisix-go
    log_dir: /var/log/apisix-go
    temp_dir: /var/tmp/apisix-go
```

This `/var` layout is not present in `conf/config-production.yaml` and is not an
image default. The current Dockerfile creates only `/usr/local/apisix/conf`,
`/usr/local/apisix/logs`, and `/usr/local/apisix/data`; it does not create these
four paths. Before startup, the operator must create or mount every selected
directory and set ownership and permissions for the runtime user. See
[`configuration.md`](configuration.md#runtime-paths) for platform defaults,
relative-path resolution, and environment aliases.

`compatibility_target` currently accepts only `apisix-3.17`.
`security_profile` accepts `compat` or `strict`; the checked-in production
reference uses `compat` for APISIX-compatible plugin behavior. `strict` remains
an independent optional security policy and is not required for this production
claim. `qualification_profile` accepts the empty value or
`http-data-plane-v1`; the empty value makes no qualification claim. Unknown
axis values fail startup, and explicitly activated unsupported runtime features
still fail closed in every selection.

Security policy remains independent of qualification. An empty qualification
profile makes no qualification claim. Selection of `http-data-plane-v1`
directly enforces its production transport, listener, trusted-address, and
Admin API requirements even under `security_profile: compat`, and fails closed
when any required manifest evidence is blocked. The capability manifest
owns the ordered `required_plugins` sequence. The effective HTTP plugin
sequence must exactly equal the manifest `required_plugins` sequence, including
order. Qualification derives from that ordered sequence and its required
evidence. The qualified sequence retains, in manifest order, only entries in
the APISIX namespace with full behavior, the selected domain, and every
required evidence claim verified or concretely not applicable. Any sequence
mismatch fails closed.

## Exact profile requirements

The checked-in production reference selects `compat` security behavior and
`http-data-plane-v1` qualification. The qualification owns the following
production requirements directly:

- `debug: false`.
- Under `http-data-plane-v1`, `deployment.role: data_plane` and the effective
  provider is `etcd`.
- Every `deployment.etcd.host` endpoint uses the `https://` scheme and
  `deployment.etcd.tls.verify` is explicitly `true`.
- Under `http-data-plane-v1`, `apisix.proxy_mode: http`; `apisix.stream_proxy.tcp` and
  `apisix.stream_proxy.udp` are empty; `stream_plugins` is empty.
- `apisix.enable_admin: false`.
- `apisix.trusted_addresses` contains at least one syntactically valid CIDR.
- `nginx_config.http.client_max_body_size` is positive and
  `nginx_config.http.client_body_timeout` is mandatory and positive. The checked-in
  `conf/config-production.yaml` value for `client_body_timeout` is 60 seconds;
  Go applies it together with the header timeout as `net/http`'s combined
  `ReadTimeout`, because `net/http` has no body-only server deadline.
- The effective HTTP plugin sequence exactly equals the manifest
  `required_plugins` sequence, including order: the 110 qualified HTTP-domain
  plugins from `apisix-3.17-all-plugins-v1`. This document does not maintain a
  second plugin inventory; current membership, qualification result, and
  blockers are in the [generated plugin capability status](plugins.md).
- `apisix_go.runtime_paths.data_dir`,
  `apisix_go.runtime_paths.runtime_dir`,
  `apisix_go.runtime_paths.log_dir`, and
  `apisix_go.runtime_paths.temp_dir` are all non-empty absolute paths. Static
  validation does not create the directories or verify their ownership and
  permissions. The durable journal is `data_dir/apisix-go-store.db`.

- Process access-log settings remain unset: HTTP and stream access-log enable
  flags are false, paths and formats are empty, and access-log buffering is
  zero.
- Process-level NGINX HTTP/stream access-log settings are rejected when
  explicitly non-zero or non-empty in every profile. The compatibility/general
  plugin contract uses route/plugin loggers; Elasticsearch, ClickHouse, and
  Tencent CLS require a non-empty effective flat-string `log_format` from
  effective resource/plugin configuration or plugin metadata before creating a
  client or batch processor; effective resource/plugin configuration wins over
  plugin metadata. Qualification makes no request-logging egress claim.
- Prometheus `http_status`, `http_latency`, and `bandwidth` each have an
  independent admitted-tuple budget controlled by
  `plugin_attr.prometheus.max_http_series` (default `10000`, integer range
  `100`..`100000`). Existing tuples remain unchanged; after a family is full,
  unseen tuples preserve bounded control labels, replace dynamic labels with
  `__overflow__`, and increment
  `<metric_prefix>http_metric_series_overflow_total{metric}`. With the default
  `metric_prefix: apisix_`, this is
  `apisix_http_metric_series_overflow_total{metric}`. These series are not
  reset during route reload.
- The HTTP-domain Kafka plugins are part of the 110-plugin contract with the
  behavior and dependency evidence recorded in the manifest. The selected
  `security_profile: compat` keeps APISIX-compatible Kafka transport defaults;
  operators must still qualify the real Kafka environment used by a release.

The checked-in `conf/config-production.yaml` is the reference shape. It leaves
the etcd endpoint empty and omits the runtime-path overlay, so an operator must
supply a real endpoint through an override or
`APISIXGO_DEPLOYMENT_ETCD_HOST`; it must not be replaced with a plaintext
endpoint when selecting this profile. Providing an endpoint and writable
directories does not replace release and operations qualification evidence.
The manifest's 110 HTTP plugin contracts are ready, but that does not by itself
make the repository production qualified.

## Static configuration preflight

Use the compatibility example for a successful static preflight:

```bash
apisix config test -c conf/config-example.yaml
apisix config dump --effective --redacted -c conf/config-example.yaml
```

Running `apisix config test -c conf/config-production.yaml` without an override
is expected to fail closed because the file supplies no etcd endpoint. Supplying
an HTTPS endpoint and valid runtime paths proves only static configuration.
Do not interpret that command as producing a current production JSON dump.

`config test` checks static read, merge, decode, and profile contracts only. It
does not create directories, verify permissions, open or migrate the journal,
bind listeners, contact etcd or another provider, configure logging, or prove
runtime readiness. `config dump` has no unredacted mode and its redacted output
still contains approved operational metadata; handle it as a sensitive
diagnostic artifact. The full precedence, environment-namespace, redaction,
runtime-path, and
[journal migration](configuration.md#journal-relocation-and-rollback) contract
is documented in [`configuration.md`](configuration.md).

## Topology and TLS boundary

The profile is an HTTP data plane, not a complete edge deployment. All replicas
consume the shared etcd/config and consumer snapshots. A plain HTTP listener is
acceptable only when it is reachable exclusively from a trusted TLS-terminating
ingress and that ingress source range is represented in
`apisix.trusted_addresses`. The profile cannot verify that an external ingress
or load balancer exists or is configured correctly.

Direct Internet exposure requires the implemented frontend TLS listener and its
certificate/SNI configuration. Frontend HTTPS serving is supported, while
QUIC/HTTP/3 and stream TLS/mTLS remain outside this profile. The external TLS
boundary, certificate rotation procedure, ingress policy, and Internet-facing
network controls remain operator responsibilities.

## External ingress request-log boundary

This qualification profile makes no in-process request-logging guarantee.
External ingress logging belongs to environment-specific deployment acceptance
and is outside the functional/stability milestone. A deployment may require a
redacted bundle containing request ID, method, normalized path without
query-string secrets, status, latency, upstream identity, retention owner, and
trace correlation, but the absence of that external artifact does not block
qualification of the APISIX-Go process itself.

## State ownership

The profile deliberately separates shared inputs from per-replica observations
and excludes stateful cross-replica features:

| State family | Source or owner | Profile contract |
| --- | --- | --- |
| Configuration | etcd/config snapshots | Shared desired configuration is the input to every replica; this profile does not add a consensus or configuration-write owner. |
| Consumer state | etcd consumer snapshots | Consumer resources are shared inputs. Request authentication is evaluated locally from the snapshot and request credentials. |
| Request authentication | Each request, route/plugin config, consumer snapshot | Stateless per-request processing; no cross-replica authentication session is assumed. |
| Rate limiting | Qualified HTTP rate-limit plugins and their configured backends | The manifest records each plugin's supported behavior. A per-replica counter must not be treated as a global quota; a shared quota requires the plugin's supported external store and qualification of that real dependency. |
| Sessions | Qualified HTTP authentication plugins and their configured backends | No implicit cross-replica in-process session is provided. Any externally backed session behavior is limited to the plugin contract and requires the configured dependency to be qualified. |
| Cache | Qualified HTTP cache plugins; each replica and configured backend | The plugin contract applies, but the profile does not turn per-replica cache contents into a shared cache or add cross-replica invalidation semantics. |
| Upstream health | Each replica's own connections and probes | Health describes that replica's observations and connections; it is not a cross-replica quota or a shared health authority. |
| Secret cache | Per-replica resolver/materialization boundary | Secrets come from supported config/etcd inputs and are materialized locally as needed. No cross-replica secret cache or rotation guarantee is implied. |
| Metrics | Each replica's instrumentation and scrape endpoint | Metrics are per-replica observations and must be scraped/aggregated by external operations tooling; stream metrics are excluded. |

## Runtime boundaries

Explicit activation of these unsupported features fails startup:

- Admin (`apisix.enable_admin`), top-level `discovery`, `ext-plugin.cmd`,
  `wasm.plugins`, and `xrpc.protocols`;
- `enable_quic` and `enable_http3` on SSL listeners.

Route and upstream discovery fields are retained by the resource decoder for
compatibility, but HTTP and stream compilation rejects discovery types or
service references that require an unsupported discovery runtime. The
qualification contract excludes general stream-plugin chaining, stream
metrics, Lua/OpenResty runtime behavior, external plugin runners, and process
access-log claims. Every manifest-qualified HTTP-domain plugin, including the
HTTP Kafka and request-logger families, remains in scope at its documented
behavior boundary; the profile adds no broader external-service guarantee.

The bounded observability contract is explicit: Zipkin is v2-only. OTel rejects
`set_ngx_var: true` and any non-zero `inactive_timeout`; collector
`request_timeout` remains supported. SkyWalking and the other HTTP observability
plugins are included only to the behavior and evidence boundaries recorded in
the manifest. Stream metrics are excluded because the profile rejects stream
activation; registered stream capabilities remain outside this candidate
contract.

## WebSocket boundary

An HTTP WebSocket upgrade is admitted only when the effective route or service
sets `enable_websocket: true`. Every WebSocket upgrade attempt skips response
callbacks; request, authentication, access, before-proxy, and log phases still
run. For this profile, successful HTTP reverse-proxy tunnels remain subject to
cluster admission and timeout limits; Kafka PubSub compatibility tunnels are
outside the profile contract.
In the current pre-convergence implementation, retiring a route generation may
close its WebSocket connections as part of generation shutdown. This is an
implementation limitation, not the governing lifecycle target.

## Readiness and reload behavior

- The separate status listener defaults to `127.0.0.1:7085`; `/status` returns
  HTTP 200 while it is serving.
- `/status/ready` remains HTTP 503 until a serviceable configuration has
  committed, then remains HTTP 200 while last-good continues serving. Etcd
  reachability is monitored separately and does not withdraw last-good
  readiness.
- The container image does not define a Docker healthcheck. Orchestrators must
  configure readiness on the status listener explicitly. A TCP socket probe on
  the intended listener is sufficient for process liveness.
- A generation-wide configuration or route startup failure is returned to the
  process entrypoint. An invalid individual HTTP route is quarantined instead:
  valid routes start, the invalid route receives 404, and readiness remains 503
  until the quarantine is cleared.
- The current pre-convergence `SIGHUP` path performs graceful shutdown and
  returns an unsupported-reload error; it does not hand off a live generation.

The selected architecture is one APISIX-Go process per replica. There is no
in-process supervisor/worker handoff requirement in this profile; process
restart belongs to the external service manager. APISIX-Go remains responsible
for committed-journal recovery, readiness, generation leases, and graceful
termination of the running process.

## Candidate authentication and TLS admission

When `security_profile` is `strict`, route compilation
quarantines a route with unsafe effective configuration before constructing
its handler. The policy
checks route/plugin-config/service winners and global rules; shadowed entries
do not become an independent requirement. A global-rule policy failure remains
generation-wide and fail-closed.

- Enabled `key-auth`, `jwt-auth`, and `basic-auth` configurations must set
  `hide_credentials: true` so validated credentials are not forwarded
  upstream. An explicitly disabled auth configuration (`_meta.disable: true`)
  is inert.
- `jwt-auth` must include literal `exp` in `claims_to_verify`; omitting it is
  rejected rather than accepted as a non-expiring-token default.
- After inline, ID, or service upstream resolution, HTTPS and gRPCS upstreams
  must set `tls.verify: true`; omitted or false is rejected in this profile.

`security_profile: compat` retains APISIX-compatible authentication and
upstream-TLS defaults, including omitted or false `hide_credentials` and
`tls.verify`; selecting `http-data-plane-v1` alone does not change those
security defaults.

Route compilation also quarantines an individual route configured with
`remote_addr`, non-empty
`remote_addrs` or `vars`, non-null `script_id`, `script`, and non-empty
`filter_func`. A singular `host` is supported by the same exact/wildcard
dispatcher as a one-element `hosts`; `host` and `hosts` cannot both be set.
Do not enable stream or `gm` under this profile. Strip unsupported route fields
before migration. Keep `status: 0` only for
routes that are intentionally disabled; those routes are accepted but omitted
from the HTTP route table. The data plane quarantines unsupported route
semantics instead of approximating them or blocking unrelated valid routes.

## Qualification status

The current milestone has exactly two required evidence groups:

1. HTTP functionality: the APISIX 3.17 differential corpus and selected
   110-plugin HTTP profile remain green, followed by real-process assembly
   smoke for representative authentication, rewrite, proxy-control, blocking,
   and standalone-provider paths.
2. Runtime stability: non-root container startup, an in-flight request that
   completes during graceful TERM, verified-TLS etcd last-good/recovery,
   compaction recovery, replica restart, live delete/re-add convergence, and
   the canonical 30-minute proxy soak all pass for one immutable source
   revision.

After a post-merge run records those gates against its resolved commit, the
permitted claim is: **`http-data-plane-v1` is functionally and
runtime-stability qualified within its documented HTTP plugin and dependency
boundaries.** This is not a repository-wide production-ready claim and does
not qualify stream data plane behavior.

Upgrade/rollback, registry publication, signing, release packaging, and
environment-specific Kubernetes/systemd, ingress, capacity, and observability
acceptance are deliberately deferred. They do not block this functional and
stability claim. Real external services remain conditional boundaries for the
plugins that use them; local fixture parity does not certify an operator's
Kafka, Redis, cloud-provider, or logging environment.

The manifest/ADR/generated-document contract is checked read-only with
`go run ./cmd/capability-gen -repo-root . -check`; a passing drift check alone
is not functional/stability qualification evidence. Selecting the profile in
configuration also does not create a qualification claim without the recorded
post-merge gate result.

The current selection result and every blocking evidence claim are derived from
the manifest and published in the
[generated plugin capability status](plugins.md). Do not copy its numeric
summary or per-plugin evidence rows here; selection fails closed until that
generated projection shows that the effective plugin sequence and qualified
sequence both exactly equal the manifest `required_plugins` sequence in order.
