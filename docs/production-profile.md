# Candidate HTTP data-plane profile

`http-data-plane-v1` is a conservative operator contract for running
`apisix-go` as an HTTP data plane behind a separately managed control plane and
ingress boundary. It is a candidate awaiting release and operations
qualification. The repository-wide warning still applies: apisix-go is under
active development and is not ready for production use.

## Selection

Set the profile in the merged runtime configuration:

```yaml
deployment:
  profile: http-data-plane-v1
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

The only accepted values are the empty compatibility value and
`http-data-plane-v1`. An unknown value fails startup. The empty value does not
claim this candidate contract; explicit activation of unsupported
runtime features still fails closed in every mode.

## Exact profile requirements

The merged configuration must satisfy all of the following:

- `debug: false`.
- `deployment.role: data_plane` and the effective provider is `etcd`.
- Every `deployment.etcd.host` endpoint uses the `https://` scheme and
  `deployment.etcd.tls.verify` is explicitly `true`.
- `apisix.proxy_mode: http`; `apisix.stream_proxy.tcp` and
  `apisix.stream_proxy.udp` are empty; `stream_plugins` is empty.
- `apisix.enable_admin: false`.
- `apisix.trusted_addresses` contains at least one syntactically valid CIDR.
- `nginx_config.http.client_max_body_size` is positive.
- The ordered HTTP plugin list is exactly:

  ```yaml
  plugins:
    - request-id
    - cors
    - key-auth
    - jwt-auth
    - basic-auth
    - prometheus
  ```

- Process access-log settings remain unset: HTTP and stream access-log enable
  flags are false, paths and formats are empty, and access-log buffering is
  zero.

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
service references that require an unsupported discovery runtime. The profile
also excludes general stream-plugin chaining, stream metrics, Lua/OpenResty
runtime behavior, external plugin runners, and process access-log claims.

The bounded observability contract is strict: Zipkin is v2-only. OTel rejects
`set_ngx_var: true` and any non-zero `inactive_timeout`; collector
`request_timeout` remains supported.

## WebSocket boundary

An HTTP WebSocket upgrade is admitted only when the effective route or service
sets `enable_websocket: true`. Every WebSocket upgrade attempt skips response
callbacks; request, authentication, access, before-proxy, and log phases still
run. Successful tunnels remain subject to cluster admission and timeout limits.
When a route generation retires, its WebSocket connections are closed as part
of generation shutdown.

## Readiness and reload behavior

- `/livez` returns HTTP 200 while the process is alive.
- `/readyz` remains HTTP 503 until configuration has been applied and the
  configured etcd provider is reachable; it then returns HTTP 200 with the
  config-apply and provider-reachability state.
- A configuration or route startup failure is returned to the process entrypoint
  instead of producing a silently degraded profile.
- `SIGHUP` performs graceful shutdown and returns an unsupported-reload error.
  It is not an in-process reload; start a new process with the new merged
  configuration after the old generation has drained.

## Qualification status

This profile is a conservative candidate, not a production-readiness claim. It
still requires the later release and operations qualification work: provenance
and artifact verification, real deployment/ingress validation, operational
runbooks, rollback evidence, and environment-specific capacity and failure
testing. Until those gates are complete, keep the global not-ready warning and
do not advertise `http-data-plane-v1` as production qualified.
