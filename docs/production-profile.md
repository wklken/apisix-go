# Candidate HTTP data-plane profile

`http-data-plane-v1` is a conservative operator contract for running
`apisix-go` as an HTTP data plane behind a separately managed control plane and
ingress boundary. It is a candidate awaiting release and operations
qualification. The repository-wide warning still applies: apisix-go is under
active development and is not ready for production use. The executable
operator procedure is the [production release runbook](runbooks/production-release.md);
it does not turn a workflow definition or local test into qualification
evidence.

## Selection

Select the compatibility target, security policy, and qualification contract
independently in the merged runtime configuration:

```yaml
compatibility_target: apisix-3.17
security_profile: strict
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

`compatibility_target` currently accepts only `apisix-3.17`.
`security_profile` accepts `compat` or `strict`; `strict` enables the transport,
credential, and trusted-address requirements below without selecting a
qualification claim. `qualification_profile` accepts the empty value or
`http-data-plane-v1`; the empty value makes no qualification claim. Unknown
axis values fail startup, and explicitly activated unsupported runtime features
still fail closed in every selection.

## Exact profile requirements

The checked-in production reference selects both `strict` security and
`http-data-plane-v1` qualification. Its merged configuration must satisfy all
of the following:

- Under `strict`, `debug: false`.
- Under `http-data-plane-v1`, `deployment.role: data_plane` and the effective
  provider is `etcd`.
- Every `deployment.etcd.host` endpoint uses the `https://` scheme and
  `deployment.etcd.tls.verify` is explicitly `true` under `strict` when etcd is
  the effective provider.
- Under `http-data-plane-v1`, `apisix.proxy_mode: http`; `apisix.stream_proxy.tcp` and
  `apisix.stream_proxy.udp` are empty; `stream_plugins` is empty.
- `apisix.enable_admin: false`.
- Under `strict`, `apisix.trusted_addresses` contains at least one syntactically
  valid CIDR.
- `nginx_config.http.client_max_body_size` is positive and
  `nginx_config.http.client_body_timeout` is mandatory and positive. The checked-in
  `conf/config-production.yaml` value for `client_body_timeout` is 60 seconds;
  Go applies it together with the header timeout as `net/http`'s combined
  `ReadTimeout`, because `net/http` has no body-only server deadline.
- The ordered HTTP plugin list is exactly:

  ```yaml
  plugins:
    - basic-auth
    - cors
    - jwt-auth
    - key-auth
    - prometheus
    - request-id
  ```

- Process access-log settings remain unset: HTTP and stream access-log enable
  flags are false, paths and formats are empty, and access-log buffering is
  zero.
- Process-level NGINX HTTP/stream access-log settings are rejected when
  explicitly non-zero or non-empty in every profile. The compatibility/general
  plugin contract uses route/plugin loggers; Elasticsearch, ClickHouse, and
  Tencent CLS require a non-empty effective flat-string `log_format` from
  effective resource/plugin configuration or plugin metadata before creating a
  client or batch processor; effective resource/plugin configuration wins over
  plugin metadata. The exact six-plugin candidate allowlist contains no
  request logger and makes no request-logging egress claim.
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
- Kafka PubSub and upstreams with `scheme: kafka` are outside the HTTP
  qualification contract and carry no `http-data-plane-v1` evidence claim.
  The APISIX 3.17 compatibility owner remains available regardless of the
  independently selected security or qualification axis.

The checked-in `conf/config-production.yaml` is the reference shape. It leaves
the etcd endpoint empty so an operator must supply a real endpoint through an
override or `APISIXGO_DEPLOYMENT_ETCD_HOST`; it must not be replaced with a
plaintext endpoint when selecting this profile.

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

## External ingress request-log qualification

The exact six-plugin allowlist contains no in-process request logger. A
deployment that relies on an external TLS-terminating ingress must therefore
provide a redacted request-log evidence bundle before this profile can be
qualified. The bundle must demonstrate a request ID, method, normalized path
without query-string secrets, status, latency, upstream identity, retention
owner, and trace correlation for representative successful, rejected, and
failed requests. These are ingress evidence requirements; the Go runtime does
not claim to emit those fields, and this document does not mutate or verify the
external logging system.

## State ownership

The profile deliberately separates shared inputs from per-replica observations
and excludes stateful cross-replica features:

| State family | Source or owner | Profile contract |
| --- | --- | --- |
| Configuration | etcd/config snapshots | Shared desired configuration is the input to every replica; this profile does not add a consensus or configuration-write owner. |
| Consumer state | etcd consumer snapshots | Consumer resources are shared inputs. Request authentication is evaluated locally from the snapshot and request credentials. |
| Request authentication | Each request, route/plugin config, consumer snapshot | Stateless per-request processing; no cross-replica authentication session is assumed. |
| Rate limiting | Excluded stateful rate-limit families | No cross-replica quota is provided by this profile. A per-replica counter must not be treated as a global quota. |
| Sessions | Excluded stateful session families | Session-backed authentication and login flows require a separately qualified shared-session design. |
| Cache | Excluded stateful cache families | Stateful cache contents and invalidation are not a profile contract. |
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
qualification contract also excludes general stream-plugin chaining, stream
metrics, Lua/OpenResty runtime behavior, Kafka PubSub/upstream `scheme: kafka`,
external plugin runners, and process access-log claims. Exclusion from
qualification does not disable the Kafka compatibility owner.

The bounded observability contract is strict: Zipkin is v2-only. OTel rejects
`set_ngx_var: true` and any non-zero `inactive_timeout`; collector
`request_timeout` remains supported. SkyWalking is not in the profile
allowlist, and its multi-span behavior is not production-qualified. Stream
metrics are excluded because the profile rejects stream activation; registered
stream capabilities remain outside this candidate contract.

## WebSocket boundary

An HTTP WebSocket upgrade is admitted only when the effective route or service
sets `enable_websocket: true`. Every WebSocket upgrade attempt skips response
callbacks; request, authentication, access, before-proxy, and log phases still
run. For this profile, successful HTTP reverse-proxy tunnels remain subject to
cluster admission and timeout limits; Kafka PubSub compatibility tunnels are
outside the profile contract.
When a route generation retires, its WebSocket connections are closed as part
of generation shutdown.

## Readiness and reload behavior

- `/livez` returns HTTP 200 while the process is alive.
- `/readyz` remains HTTP 503 until configuration has been applied and the
  configured etcd provider is reachable; it then returns HTTP 200 with the
  config-apply and provider-reachability state.
- The container image does not define a Docker healthcheck. Orchestrators must
  configure liveness on `/livez` and readiness on `/readyz` explicitly.
- A generation-wide configuration or route startup failure is returned to the
  process entrypoint. An invalid individual HTTP route is quarantined instead:
  valid routes start, the invalid route receives 404, and readiness remains 503
  until the quarantine is cleared.
- `SIGHUP` performs graceful shutdown and returns an unsupported-reload error.
  It is not an in-process reload; start a new process with the new merged
  configuration after the old generation has drained.

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
Do not enable request loggers, `sls-logger`, stream, or `gm` under this profile.
Strip unsupported route fields before migration. Keep `status: 0` only for
routes that are intentionally disabled; those routes are accepted but omitted
from the HTTP route table. The data plane quarantines unsupported route
semantics instead of approximating them or blocking unrelated valid routes.

## Qualification status

This profile is a conservative candidate, not a production-readiness claim. It
still requires the later release and operations qualification work: provenance
and artifact verification, real deployment/ingress validation, operational
runbooks, rollback evidence, and environment-specific capacity and failure
testing. Until those gates are complete, keep the global not-ready warning and
do not advertise `http-data-plane-v1` as production qualified. The first
release also has no previous immutable digest, so rollback qualification cannot
be claimed until a distinct older published digest exists and is exercised.
The independent plugin-status workflow must create a check on every pull
request so it can remain required without path-filtered PRs staying pending.
Its exact selector pass is not release qualification evidence; `master` push
triggers remain scoped to the status matrix, manifests, selector test, and
workflow paths.
The final workflow must retain the protected `production-release` reviewers and
wait timer; the current environment's protected-branch-only tag policy must be
updated by an operator to permit the intended `v*` tag before publication.

The current capability manifest intentionally prevents selecting
`qualification_profile: http-data-plane-v1`: none of its six required plugins
satisfies every required evidence claim. Startup fails closed until the
manifest records all of the following gaps as passing evidence:

| Required plugin | Current non-passing required evidence |
| --- | --- |
| `basic-auth` | `converted_upstream=stale`; `differential`, `failure`, `real_dependency`, and `recovery` missing |
| `cors` | `converted_upstream=stale`; `differential`, `failure`, `real_dependency`, and `recovery` missing |
| `jwt-auth` | `converted_upstream=stale`; `differential`, `failure`, `real_dependency`, and `recovery` missing |
| `key-auth` | `converted_upstream=stale`; `differential`, `failure`, `real_dependency`, and `recovery` missing |
| `prometheus` | `converted_upstream=deferred`; `differential`, `failure`, `real_dependency`, and `recovery` missing |
| `request-id` | `converted_upstream=stale`; `differential` and `failure` missing |
