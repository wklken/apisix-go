# Configuration compatibility

`apisix-go` accepts the YAML shape of the official Apache APISIX
[`conf/config.yaml.example`](https://github.com/apache/apisix/blob/master/conf/config.yaml.example),
including its scalar and mapping forms for listeners. The Go loader keeps
configuration that has no direct Go equivalent in the typed configuration
object so an official file can be loaded without being rewritten. Recognition
is a compatibility boundary, not an activation guarantee: compatibility-only
fields may be retained, while the explicitly unsupported runtime activations
listed below fail closed when configured.

## Production container configuration

The image starts with `conf/config-production.yaml` layered over
`conf/config-default.yaml`. This production override intentionally contains no
etcd endpoint, so the image fails configuration validation until an operator
provides `APISIXGO_DEPLOYMENT_ETCD_HOST` (comma-separated for multiple
endpoints), or mounts an operator-managed configuration override. The endpoint
must be supplied explicitly; the image does not fall back to a local etcd
address.

The checked-in production override selects three independent axes:

```yaml
compatibility_target: apisix-3.17
security_profile: strict
qualification_profile: http-data-plane-v1
```

`compatibility_target` selects the pinned observable APISIX contract.
`security_profile` selects compatibility-preserving or versioned strict
security controls. `qualification_profile` selects an evidence-backed
operating contract. Strict security is independent of qualification. An empty
qualification profile makes no qualification claim. Selecting
`http-data-plane-v1` fails closed when any required manifest evidence is
blocked; current required plugins and blocking evidence are shown only in the
[generated plugin capability status](plugins.md).

The production override is a conservative candidate configuration; it still
awaits the release and operations qualification described in
[`production-profile.md`](production-profile.md) and the
[`production release runbook`](runbooks/production-release.md). The release
workflow and metadata checks define a qualification path; they do not by
themselves qualify an image or deployment.

The profile requires `debug: false`, an HTTP-only `apisix.proxy_mode`, empty
TCP/UDP stream listeners and `stream_plugins`, at least one valid
`apisix.trusted_addresses` CIDR, positive
`nginx_config.http.client_max_body_size` and `client_body_timeout` values, and
no process access-log settings. The checked-in production override uses a
60-second `client_body_timeout`.
Every etcd endpoint must use `https://` and `deployment.etcd.tls.verify` must
be explicitly `true` under strict security. The qualification profile's
required plugin set comes from `pkg/capability/manifest.yaml`, not this prose;
the effective HTTP plugin list must match that set. Under strict security,
enabled effective `key-auth`, `basic-auth`, and `jwt-auth` configurations must
set `hide_credentials: true`; `jwt-auth` must also include literal `exp` in
`claims_to_verify`. Effective HTTPS and gRPCS upstreams must set
`tls.verify: true` after inline, ID, or service resolution. Disabled auth
configurations remain inert. `security_profile: compat` retains the
APISIX-compatible defaults for these dynamic fields. Kafka PubSub and upstreams
with `scheme: kafka` remain APISIX compatibility owners but are outside the
HTTP qualification contract, independent of security selection.

NGINX HTTP and stream process access-log settings are unsupported in every
profile, not only in `http-data-plane-v1`: any explicitly non-zero boolean or
numeric value, or non-empty string value, fails configuration load. Route/plugin
loggers are the supported compatibility/general-plugin request-logging
mechanism, whose output is owned by the Go request pipeline. Qualification
selection makes no request-logging egress claim; consult the generated status
for the current required plugin set.

`/livez` returns HTTP 200 while the process is alive. `/readyz` returns HTTP
503 until configuration has been applied and the configured etcd provider is
reachable, then returns HTTP 200 with the config-apply and etcd-reachability
state. The image does not define a Docker healthcheck. Orchestrators must
configure `/livez` for process liveness and `/readyz` for config-apply and etcd
reachability.

The production release contract requires a bounded periodic etcd reachability
probe in addition to the watch loop. During a verified recovery test, etcd loss
must make `/readyz` return 503 while `/livez` and the last successfully applied
route continue serving; readiness must return to 200 after recovery and a
newer revision must apply. See the [production release runbook](runbooks/production-release.md)
for the evidence and operator-supplied deployment step.

Deterministically invalid route, global-rule, consumer, and SSL payloads are
rejected before replacing their last-good store value. During HTTP generation
build, a semantic failure scoped to one route quarantines that complete route;
the remaining valid routes still publish. A malformed route or global-rule row
left by an older database is likewise omitted from the immutable build
snapshot. Every omission keeps the no-label
`config_apply_quarantined_resources` gauge non-zero and `/readyz` at 503.
Global-rule and shared generation setup failures remain fail-closed.
Provider-side and build-snapshot quarantine counts are aggregated
independently, so clearing one source cannot hide the other.

`pkg/capability/manifest.yaml` is the only editable plugin, behavior, evidence,
qualification, platform, gap, and divergence ledger. `docs/plugins.md` is its
generated projection and must not be edited as an independent matrix. Verify
the manifest/ADR/generated-document contract without writing files:

```bash
go run ./cmd/capability-gen -repo-root . -check
```

Selection derives the required set and evidence result from the manifest and
fails closed on any mismatch or blocked requirement.

## Applied by the Go runtime

| Configuration | Go behavior |
| --- | --- |
| `apisix.node_listen` | Opens every configured TCP HTTP listener. Both `9080` and `{port: 9080, ip: ...}` forms are accepted. |
| `compatibility_target` | Selects the pinned observable compatibility contract. The current accepted value is `apisix-3.17`; other values fail startup. |
| `security_profile` | Selects `compat` or `strict` security behavior independently from compatibility and qualification. |
| `qualification_profile` | Empty makes no qualification claim. `http-data-plane-v1` is selectable only when its manifest-owned required set has complete required evidence; otherwise startup fails closed. |
| `apisix.proxy_mode` and `apisix.stream_proxy.tcp` | `http` leaves stream settings unused. When `proxy_mode` contains `stream`, the bounded raw-TCP/MQTT stream runtime requires at least one TCP listener and starts only after routes, upstream references, listener binds, and supported flags validate successfully. |
| `plugins`, `stream_plugins`, and `plugin_attr` | Control plugin registration, stream plugin selection, and plugin-specific settings. The Prometheus lifetime and cardinality contract is documented below. |
| `graphql.max_size` | Applies to the GraphQL limit and GraphQL proxy-cache plugins. |
| `apisix.data_encryption` | Configures encrypted resource-field handling. New writes use explicit `$encrypted://v2:` AES-GCM envelopes with a random 12-byte nonce and the canonical `plugin-name.field-path` as authenticated context. Bare `v2:` values remain plaintext. Unversioned AES-CBC remains decrypt-only for migration and an explicit legacy envelope is rewritten as v2 when it passes through the write path. Keep older keys after the newest key until legacy values have been rewritten. |
| `nginx_config.http.keepalive_timeout` | Maps to `http.Server.IdleTimeout`. |
| `nginx_config.http.client_header_timeout` and `client_body_timeout` | Map to the corresponding Go read timeouts; the body timeout uses the combined header/body deadline because `net/http` has no body-only server timeout. `client_body_timeout` defaults to 60 seconds and must be positive in every profile. |
| `nginx_config.http.client_max_body_size` | Bounds ingress request bodies before route/plugin processing. It defaults to 10 MiB and must be positive in every profile; explicitly setting zero no longer selects an unlimited body. |
| `nginx_config.http.send_timeout` | Must remain zero. A non-zero value fails startup because Go `net/http` cannot reproduce NGINX write-idle timeout semantics without imposing an absolute response deadline. |
| `deployment.etcd.host`, `prefix`, `user`, `password`, `timeout`, `startup_retry`, and `tls` | Configure the etcd client endpoints, prefix, credentials, dial/request timeout, startup retries, client certificate, verification, and SNI. |
| `deployment.etcd.health_check_timeout` | Sets the interval in seconds between independent etcd reachability probes. It defaults to 10 seconds when omitted or non-positive. Each probe is separately bounded by `deployment.etcd.timeout`; this field is an interval, not a request deadline. |
| `deployment.role: data_plane` with `role_data_plane.config_provider: yaml` or `json` | Loads resource snapshots from `conf/apisix.yaml` or `conf/apisix.json`, watches the file, and applies additions, updates, and removals through the local store. |
| `proxy.max_idle_conns` | Global maximum number of idle (keep-alive) connections kept open across all upstream hosts. Default 1024; zero selects the default. |
| `proxy.max_idle_conns_per_host` | Maximum number of idle connections kept open per upstream host. Default 250; zero selects the default. |
| `proxy.max_conns_per_host` | Maximum number of concurrent connections per upstream host. Default 1024; zero selects the default. |
| `proxy.max_in_flight` | Maximum number of concurrently active upstream response bodies per cluster. Default 1024; zero selects the default. Negative values are rejected at configuration load. |

### APISIX 3.17 parity details

The following resource and wire contracts are part of the supported
compatibility profile:

- The dynamic HTTP plugin allowlist is the single `/apisix/plugins` resource.
  Its value is a JSON array of `{name: string, stream?: boolean}` objects;
  entries with `stream: true` are excluded from the HTTP allowlist. A valid
  dynamic list overrides the startup `plugins` list. A malformed update is
  rejected and the last valid generation remains active; deleting the resource
  removes the dynamic override and restores the startup list.
- A route with neither `host` nor `hosts` configured inherits `service.hosts`.
  Route plugin and upstream fields take precedence over service fields. An
  explicitly configured `enable_websocket: false` overrides a service value;
  an omitted route field inherits the service value. A plugin name present in
  more than one global rule is removed from all of those global bindings,
  matching APISIX's duplicate elimination.
- Upstream node `priority` values are retained in the cluster configuration.
  The highest-priority group is selected while it has a selectable target;
  transport retries exhaust that group before trying a lower group, and
  zero-weight nodes are not selectable. If every group is unavailable, the
  existing fail-open behavior applies.
- Frontend SSL resources default an omitted `status` to enabled and accept
  singular `sni` as well as `snis`. A `*.example.com` wildcard matches exactly
  one label. An SSL resource `client.ca` enables client-certificate
  verification for that SNI (with omitted `client.depth` defaulting to `1`);
  when no resource client CA is configured, the global trusted client CA
  remains in effect. Unsupported resource depth and URI-skip forms are
  rejected rather than silently weakening mTLS.
- Stream routes resolve a route-level `upstream_id` first. Only when it is
  absent does `service_id` supply plugins and an upstream, with route values
  taking precedence. Service updates trigger stream reloads, and matching uses
  the published resource order (the first matching route wins), not specificity
  sorting.
- `apisix.delete_uri_tail_slash: true` removes one trailing slash before HTTP
  route matching but preserves `/`. `apisix.show_upstream_status_in_response_header:
  true` emits `X-APISIX-Upstream-Status` for every upstream status; `false`
  emits it only when the final upstream result is 5xx. Transport retries retain
  the comma-separated attempt status chain. Local route/director failures do
  not synthesize an upstream status. `apisix.enable_server_tokens: true`
  forces `Server: APISIX/<version>` and `false` forces `Server: APISIX` on local,
  upstream, and streaming responses.

### Prometheus metric lifetime and cardinality

Prometheus collection is enabled only when `prometheus` is present in the
top-level `plugins` allowlist. When it is absent, APISIX-Go does not initialize
the collectors, expiration scanner, export listener, connection observer, or
request metric callbacks. The process-level `http_requests_total` counts every
HTTP request while collection is enabled. Status/latency/bandwidth and
completed LLM metrics are recorded only for requests whose effective route,
service, `plugin_config`, or global rule contains the `prometheus` plugin. A
local binding takes precedence over a global binding, matching the official
`prefer_route` run policy, so one request is never counted twice.

The dedicated export server is enabled by default and listens at
`127.0.0.1:9091`. `export_addr.ip` must be a literal IPv4 or IPv6 address and
`export_addr.port` must be an integer from `1` through `65535`. `export_uri`
must be a literal absolute path. When `enable_export_server` is `false`, the
same URI is registered through the `public-api` plugin instead; it is not also
published on the data-plane router while the dedicated server is enabled.
Explicit wrong types and invalid values for these fields fail startup instead
of falling back to another address or an ephemeral port.

`metric_prefix`, histogram buckets, request-family `extra_labels`, series
limits, and expiration values are also validated before collectors are registered.
Buckets must be finite positive numbers in strictly increasing order. Extra
label names must be valid, unique, and must not collide with the family's base
labels or Prometheus's reserved `le` label; variable values must begin with
`$`. An invalid explicit value fails startup and cannot be deferred into a
panic on the first request.

`plugin_attr.prometheus.max_http_series` limits each of `http_status`,
`http_latency`, `bandwidth`, and `upstream_status` independently, and also
caps the current route index for `stream_connection_total`.
`plugin_attr.prometheus.max_llm_series` independently limits each of
`llm_latency`, `llm_prompt_tokens`, `llm_completion_tokens`, and
`llm_active_connections`. Both settings default to `10000`, accept only
integers from `100` through `100000`, and fail startup when an explicit value
is invalid.

These eight families accept an APISIX-compatible
`plugin_attr.prometheus.metrics.<family>.expire` value. It is a non-negative
whole number of idle seconds; missing or `0` disables expiration for that
family. Activity means a successful metric observation, not a Prometheus
scrape. The cleanup interval is half the smallest enabled expiration, clamped
between one second and one minute. Without an expired backlog, deletion normally
occurs by the next scan after the configured idle period. Cleanup deletes at
most 256 entries from each family per scan, so larger backlogs drain over later
scan intervals.

`extra_labels` is supported only by the seven request/LLM families because
their values come from a request context. `upstream_status` supports `expire`
but rejects `extra_labels` instead of silently ignoring them.

Each family stores at most its configured number of exact label tuples. Once
full, an unseen tuple is written to one synthetic child with every label set to
`__overflow__`; the vector therefore has at most `limit + 1` children from this
subsystem. HTTP overflow observations increment
`<metric_prefix>http_metric_series_overflow_total{metric}`, and LLM overflow
observations increment
`<metric_prefix>llm_metric_series_overflow_total{metric}`. With the default
prefix these names begin with `apisix_`. For the bounded
`stream_connection_total` metric index only, stream routes are sorted before
the index is built; configured routes beyond the cap share
`stream_connection_total{route="__overflow__"}` and route reloads delete
children that are no longer indexed.

Expiration deletes both the exported vector child and its capacity entry.
Recreated counters and histograms start again from zero. An
`llm_active_connections` child remains pinned while any request using it is
active and becomes eligible only after the last release plus another idle
period. Expiration and capacity configuration is startup-only; restart the
process to apply changes.

### Prometheus APISIX 3.17 compatibility

APISIX-Go exports the APISIX 3.17 family names and labels that have honest
Go-native runtime owners: `nginx_http_current_connections{state}`,
`http_requests_total`, `node_info{hostname,version}`,
`etcd_modify_indexes{key}`, `upstream_status{name,ip,port}`,
`stream_connection_total{route}`, the HTTP status/latency/bandwidth families,
and the four LLM families. APISIX-Go-specific readiness, proxy, logger, panic,
and cardinality metrics remain additional families.

Go's `net/http` connection callback can identify accepted/handled, active, and
waiting connections, but it cannot separate NGINX's header-reading and
response-writing FFI phases. The official `reading` and `writing` state series
therefore remain zero. Likewise, APISIX-Go has no NGINX shared dictionaries or
XRPC runtime, so `shared_dict_capacity_bytes`,
`shared_dict_free_space_bytes`, and XRPC-only families are intentionally not
emitted.

The `bandwidth` family preserves the official labels but its byte semantics
follow available Go ownership: ingress is the non-negative request
`Content-Length`, and egress is the response body bytes written. These values
do not include request/response start lines and headers as NGINX's
`$request_length` and `$bytes_sent` do.

## HTTP upstream proxy behavior

- A route timeout overrides the corresponding upstream timeout; a non-positive
  field inherits from the upstream. After that overlay, any still omitted or
  non-positive `connect` / `send` / `read` becomes 60 seconds (APISIX/NGINX
  omitted default). There is no unlimited timeout via `0`.
- `connect` limits TCP connection establishment. `send` and `read` are
  inactivity limits which reset when body I/O makes progress. `read` also
  limits the wait for response headers. Long-idle WebSocket or gRPC streams
  must set an explicit `timeout` larger than 60s, same as APISIX.
- `tls.verify: true` validates an HTTPS upstream certificate. Under
  `security_profile: compat`, omitted or false preserves APISIX-compatible
  insecure verification behavior; `security_profile: strict` rejects an
  effective HTTPS or gRPCS upstream unless `tls.verify: true`, independently
  of qualification selection.
- Automatic retries require a replayable body. POST and PATCH additionally require `Idempotency-Key` or `X-Idempotency-Key`.
- `proxy-control` buffers at most 8 MiB in memory. A larger buffered request is rejected with HTTP 413.
- An invalid individual route is quarantined as a unit at initial build and
  reload: it receives 404 while valid routes publish. A generation-wide error
  stops initial startup or retains the last successfully published reload.
- A singular route `host` uses the same exact and wildcard dispatcher as a
  one-element `hosts` list. Simultaneous `host` and `hosts`, or a blank
  singular `host`, is rejected; a request with the wrong host receives 404.
- Route `remote_addr` (when configured, including an explicit blank), non-empty
  `remote_addrs`, non-empty `vars`, non-null `script_id`, `script`, and
  non-empty `filter_func` are rejected during route compilation because the Go
  data plane does not implement those APISIX ACL or Lua semantics. They are
  never silently discarded. Empty `vars` (`[]` or `null`) and empty
  `remote_addrs` remain accepted. The Go matcher uses URI, host, and method
  only.
- Explicit route `status: 0` is omitted from the HTTP route table. Omitted
  `status` and `status: 1` stay enabled. Any other explicit `status` fails
  compilation. This is independent of SSL `status`, which already skips
  `status == 0`.
- HTTP-family upstreams accept only the implemented `roundrobin` type. `chash` and other unsupported types are rejected during route compilation instead of silently falling back to weighted round robin.
- `qualification_profile: http-data-plane-v1` excludes `scheme: kafka`
  upstreams because Kafka PubSub is a separate compatibility subsystem. The
  Kafka owner remains available under the selected compatibility target,
  independent of the security and qualification axes.
- Without explicit HTTP timeout settings, request headers are limited to 10 seconds and idle keep-alive connections to 90 seconds. Total read/write timeouts remain disabled for streaming compatibility.
- Each upstream is served by a reusable cluster that owns one connection pool, one retry/progress wrapper chain, and one load balancer. Clusters are interned by their complete effective configuration, so unchanged upstreams keep their connection pools across unrelated route reloads, while changed upstreams receive new clusters. Route generations hold reference-counted leases and release them only after in-flight requests drain.
- When a cluster reaches its in-flight limit, the next request is rejected with HTTP 503. Overload is fail-fast and never queued.
- The supported `checks.active` HTTP/HTTPS probe subset (`type`, `http_path`, `host`, `timeout`, `concurrency`, `healthy.interval`/`successes`/`http_statuses`, and `unhealthy.interval`/`http_failures`/`tcp_failures`/`timeouts`/`http_statuses`) recovers and quarantines targets. Active defaults are healthy statuses `{200,302}` and HTTP/TCP/timeout failure thresholds `5/2/3`; the passive status defaults remain separate. When every target is unhealthy the pool fails open and keeps forwarding, with the state exposed through metrics and logs.
- `apisix.disable_upstream_healthcheck: true` omits active probes from cluster configuration while retaining ordinary weighted selection.
- Route generations are retired asynchronously: publishing a new handler never blocks behind a long-lived request on the previous generation.

The last statement describes the current pre-convergence implementation. Its
retirement path may close hijacked connections, and `SIGHUP` currently drains
and exits with an unsupported-reload error. It is not the governing lifecycle
target. The later
[supervisor-generation plan](superpowers/plans/2026-08-23-supervisor-worker-platform.md)
targets generation handoff that preserves ordinary hijacked connections; that
replacement is not implemented yet. See the
[legacy conflict ledger](architecture/legacy-conflicts.md).

## Standalone file-driven mode

Set the deployment role and provider in `conf/config.yaml`:

```yaml
deployment:
  role: data_plane
  role_data_plane:
    config_provider: yaml
```

The YAML provider reads `conf/apisix.yaml` and requires the file to end with
`#END`. The JSON provider reads `conf/apisix.json` and does not require the
marker. Both providers use the existing APISIX resource shapes for routes,
upstreams, services, plugin metadata, SSLs, stream routes, consumers, consumer
groups, global rules, plugin configs, and protos.

Every standalone reload applies the complete file as the authoritative managed
Store snapshot in one validated replacement batch. This includes an empty
snapshot, so a resource committed by a candidate whose runtime publication
later failed cannot survive after it is removed from the file.

The loader also recognizes the remaining official top-level sections and
nested fields, including `nginx_config`, `ext-plugin`, `wasm`, `xrpc`, `events`,
`lru`, status/trusted-address settings, deployment roles, admin settings, and
plugin attributes. Recognition retains values for compatibility and diagnostics;
it does not imply that a native NGINX/Lua subsystem exists in the Go runtime.
Explicit activation of Admin, top-level discovery, `ext-plugin.cmd`, WASM,
XRPC, QUIC, or HTTP/3 fails startup. Route and upstream discovery fields are
retained by the resource decoder but rejected during HTTP or stream route
compilation when they would require an unsupported runtime.

## Intentionally unsupported

These settings remain outside the Go runtime. Compatibility-only fields may be
retained for migration diagnostics; explicit activation of the unsupported
runtime features called out below is rejected rather than silently ignored:

- OpenResty/NGINX worker directives, Lua module paths/hooks, Lua shared-dict
  sizing, NGINX configuration snippets, access-log formatting, and NGINX
  variable/real-IP directives. The candidate profile also forbids process
  access-log claims; use the documented request/metrics logging boundaries.
- Frontend HTTPS listener serving is supported by the implemented Go TLS
  listener. HTTP/3/QUIC, stream TLS/mTLS, PROXY protocol, and UDP stream
  proxying remain unsupported. In stream mode, empty TCP listener sets, listener
  or upstream TLS/PROXY protocol flags, top-level TCP PROXY protocol flags,
  unresolved upstream references, unsupported stream plugins, invalid listener
  addresses, and bind failures are rejected at startup. HTTPS certificate
  selection uses the implemented frontend TLS and APISIX SSL resource path; a
  listener-only field does not create a certificate.
- General stream-plugin chaining and stream metrics. Liveness and readiness are
  exposed through `/livez` and `/readyz`; startup failures are surfaced through
  the process return, and `/readyz` remains unavailable until configuration and
  the configured etcd provider are ready.
- The APISIX Admin API, control API, status server, admin UI, admin CORS/IP
  restrictions, and admin mTLS. The current Go admin router is not a complete
  APISIX Admin API implementation.
- Lua external plugins, WASM plugins, XRPC protocol plugins, and the official
  discovery providers (`dns`, Eureka, Nacos, Consul, and Kubernetes). Top-level
  discovery activation fails startup; route/upstream discovery compatibility
  fields are preserved and rejected at route compilation.
- Exact APISIX/OpenResty etcd watch resync semantics. The production
  qualification profile uses its bounded reachability probe for readiness and
  does not claim OpenResty timing parity.
- WebSocket upgrades require effective route or service
  `enable_websocket: true`. Every WebSocket upgrade attempt skips response
  callbacks; request, authentication, access, before-proxy, and log phases
  still run. For `qualification_profile: http-data-plane-v1`, the admission and
  timeout guarantee applies only to qualification-allowed HTTP reverse-proxy
  tunnels. In the current pre-convergence implementation, retired generations
  may close those tunnels during shutdown. Kafka PubSub compatibility routes
  are outside this qualification contract.
- Zipkin is v2-only. OTel rejects `set_ngx_var: true` and any non-zero
  `inactive_timeout`; collector `request_timeout` remains supported.
- The current pre-convergence `SIGHUP` implementation performs graceful
  shutdown and returns an unsupported-reload error; it is not an in-process
  configuration reload. The governing supervisor-generation target hands a
  validated generation to a replacement process while preserving ordinary
  hijacked connections. That target belongs to a later child plan and is not
  yet delivered.

No placeholder implementation is added for these native or separate-runtime
features. They should be treated as unsupported when deploying an official
configuration file with `apisix-go`.
