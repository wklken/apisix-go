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

The production HTTP allowlist is deliberately limited to `request-id`, `cors`,
`key-auth`, `jwt-auth`, `basic-auth`, and `prometheus`. Stream proxy mode and
stream plugins are disabled, the deployment uses the data-plane etcd provider,
etcd TLS verification is enabled, and no admin or data-encryption key material
is embedded in the image defaults. The selected `deployment.profile:
http-data-plane-v1` is a conservative candidate profile; it still awaits the
release and operations qualification described in
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
be explicitly `true`. The HTTP plugin list must be exactly this ordered list:
`request-id`, `cors`, `key-auth`, `jwt-auth`, `basic-auth`, `prometheus`.
In this candidate profile, enabled effective `key-auth`, `basic-auth`, and
`jwt-auth` configurations must set `hide_credentials: true`; `jwt-auth` must
also include literal `exp` in `claims_to_verify`. Effective HTTPS and gRPCS
upstreams must set `tls.verify: true` after inline, ID, or service resolution.
Disabled auth configurations remain inert. The empty compatibility profile
retains the APISIX-compatible defaults for these dynamic fields.
The profile also excludes Kafka PubSub and upstreams with `scheme: kafka`;
those remain available only in the empty compatibility profile.

NGINX HTTP and stream process access-log settings are unsupported in every
profile, not only in `http-data-plane-v1`: any explicitly non-zero boolean or
numeric value, or non-empty string value, fails configuration load. Route/plugin
loggers are the supported compatibility/general-plugin request-logging
mechanism, whose output is owned by the Go request pipeline. The exact
six-plugin `http-data-plane-v1` allowlist contains no request logger and makes
no request-logging egress claim.

`/livez` returns HTTP 200 while the process is alive. `/readyz` returns HTTP
503 until configuration has been applied and the configured etcd provider is
reachable, then returns HTTP 200 with the config-apply and etcd-reachability
state. The image HEALTHCHECK uses `/livez` (process liveness). Orchestrators must
probe `/readyz` separately for config-apply and etcd reachability; do not use
the Docker HEALTHCHECK as Kubernetes liveness *and* readiness.

The production release contract requires a bounded periodic etcd reachability
probe in addition to the watch loop. During a verified recovery test, etcd loss
must make `/readyz` return 503 while `/livez` and the last successfully applied
route continue serving; readiness must return to 200 after recovery and a
newer revision must apply. See the [production release runbook](runbooks/production-release.md)
for the evidence and operator-supplied deployment step.

Plugin support status is verified by a separate read-only `Plugin Status
Contract` workflow. It creates the same check on every pull request so it can
be required without path-filtered PRs remaining pending, and runs the exact
`TestSupportedPluginManifestSelection` gate. Pushes to `master` are limited to
changes in `docs/plugins.md`, plugin manifests, the selector test, or the
workflow itself. This keeps the status matrix independent from the broad CI
path, which intentionally ignores Markdown-only changes.

## Applied by the Go runtime

| Configuration | Go behavior |
| --- | --- |
| `apisix.node_listen` | Opens every configured TCP HTTP listener. Both `9080` and `{port: 9080, ip: ...}` forms are accepted. |
| `deployment.profile` | Empty selects compatibility mode; `http-data-plane-v1` enables the strict candidate HTTP data-plane contract documented in [`production-profile.md`](production-profile.md). Other values are rejected. |
| `apisix.proxy_mode` and `apisix.stream_proxy.tcp` | `http` leaves stream settings unused. When `proxy_mode` contains `stream`, the bounded raw-TCP/MQTT stream runtime requires at least one TCP listener and starts only after routes, upstream references, listener binds, and supported flags validate successfully. |
| `plugins`, `stream_plugins`, and `plugin_attr` | Control plugin registration, stream plugin selection, and plugin-specific settings. The Prometheus lifetime and cardinality contract is documented below. |
| `graphql.max_size` | Applies to the GraphQL limit and GraphQL proxy-cache plugins. |
| `apisix.data_encryption` | Configures encrypted resource-field handling. |
| `nginx_config.http.keepalive_timeout` | Maps to `http.Server.IdleTimeout`. |
| `nginx_config.http.client_header_timeout` and `client_body_timeout` | Map to the corresponding Go read timeouts; the body timeout uses the combined header/body deadline because `net/http` has no body-only server timeout. `http-data-plane-v1` requires `client_body_timeout` to be positive; the checked-in production value is 60 seconds. |
| `nginx_config.http.send_timeout` | Must remain zero. A non-zero value fails startup because Go `net/http` cannot reproduce NGINX write-idle timeout semantics without imposing an absolute response deadline. |
| `deployment.etcd.host`, `prefix`, `user`, `password`, `timeout`, `startup_retry`, and `tls` | Configure the etcd client endpoints, prefix, credentials, dial/request timeout, startup retries, client certificate, verification, and SNI. |
| `deployment.etcd.health_check_timeout` | Sets the interval in seconds between independent etcd reachability probes. It defaults to 10 seconds when omitted or non-positive. Each probe is separately bounded by `deployment.etcd.timeout`; this field is an interval, not a request deadline. |
| `deployment.role: data_plane` with `role_data_plane.config_provider: yaml` or `json` | Loads resource snapshots from `conf/apisix.yaml` or `conf/apisix.json`, watches the file, and applies additions, updates, and removals through the local store. |
| `proxy.max_idle_conns` | Global maximum number of idle (keep-alive) connections kept open across all upstream hosts. Default 1024; zero selects the default. |
| `proxy.max_idle_conns_per_host` | Maximum number of idle connections kept open per upstream host. Default 250; zero selects the default. |
| `proxy.max_conns_per_host` | Maximum number of concurrent connections per upstream host. Default 1024; zero selects the default. |
| `proxy.max_in_flight` | Maximum number of concurrently active upstream response bodies per cluster. Default 1024; zero selects the default. Negative values are rejected at configuration load. |

### Prometheus metric lifetime and cardinality

`plugin_attr.prometheus.max_http_series` limits each of `http_status`,
`http_latency`, and `bandwidth` independently.
`plugin_attr.prometheus.max_llm_series` independently limits each of
`llm_latency`, `llm_prompt_tokens`, `llm_completion_tokens`, and
`llm_active_connections`. Both settings default to `10000`, accept only
integers from `100` through `100000`, and fail startup when an explicit value
is invalid.

The seven families accept an APISIX-compatible
`plugin_attr.prometheus.metrics.<family>.expire` value. It is a non-negative
whole number of idle seconds; missing or `0` disables expiration for that
family. Activity means a successful metric observation, not a Prometheus
scrape. The cleanup interval is half the smallest enabled expiration, clamped
between one second and one minute. Without an expired backlog, deletion normally
occurs by the next scan after the configured idle period. Cleanup deletes at
most 256 entries from each family per scan, so larger backlogs drain over later
scan intervals.

Each family stores at most its configured number of exact label tuples. Once
full, an unseen tuple is written to one synthetic child with every label set to
`__overflow__`; the vector therefore has at most `limit + 1` children from this
subsystem. HTTP overflow observations increment
`<metric_prefix>http_metric_series_overflow_total{metric}`, and LLM overflow
observations increment
`<metric_prefix>llm_metric_series_overflow_total{metric}`. With the default
prefix these names begin with `apisix_`.

Expiration deletes both the exported vector child and its capacity entry.
Recreated counters and histograms start again from zero. An
`llm_active_connections` child remains pinned while any request using it is
active and becomes eligible only after the last release plus another idle
period. Expiration and capacity configuration is startup-only; restart the
process to apply changes.

## HTTP upstream proxy behavior

- A route timeout overrides the corresponding upstream timeout; a non-positive
  field inherits from the upstream. After that overlay, any still omitted or
  non-positive `connect` / `send` / `read` becomes 60 seconds (APISIX/NGINX
  omitted default). There is no unlimited timeout via `0`.
- `connect` limits TCP connection establishment. `send` and `read` are
  inactivity limits which reset when body I/O makes progress. `read` also
  limits the wait for response headers. Long-idle WebSocket or gRPC streams
  must set an explicit `timeout` larger than 60s, same as APISIX.
- `tls.verify: true` validates an HTTPS upstream certificate. In the empty
  compatibility profile, omitted or false preserves APISIX-compatible insecure
  verification behavior; `http-data-plane-v1` rejects an effective HTTPS or
  gRPCS upstream unless `tls.verify: true`.
- Automatic retries require a replayable body. POST and PATCH additionally require `Idempotency-Key` or `X-Idempotency-Key`.
- `proxy-control` buffers at most 8 MiB in memory. A larger buffered request is rejected with HTTP 413.
- An invalid initial route generation stops startup. An invalid reload retains the last successfully published generation.
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
- `http-data-plane-v1` rejects `scheme: kafka` upstreams because Kafka PubSub is a separate compatibility subsystem; the empty compatibility profile retains the Kafka owner.
- Without explicit HTTP timeout settings, request headers are limited to 10 seconds and idle keep-alive connections to 90 seconds. Total read/write timeouts remain disabled for streaming compatibility.
- Each upstream is served by a reusable cluster that owns one connection pool, one retry/progress wrapper chain, and one load balancer. Clusters are interned by their complete effective configuration, so unchanged upstreams keep their connection pools across unrelated route reloads, while changed upstreams receive new clusters. Route generations hold reference-counted leases and release them only after in-flight requests drain.
- When a cluster reaches its in-flight limit, the next request is rejected with HTTP 503. Overload is fail-fast and never queued.
- The supported `checks.active` HTTP/HTTPS probe subset (`type`, `http_path`, `host`, `timeout`, `concurrency`, `healthy.interval`/`successes`/`http_statuses`, and `unhealthy.interval`/`http_failures`/`tcp_failures`/`http_statuses`) recovers and quarantines targets. When every target is unhealthy the pool fails open and keeps forwarding, with the state exposed through metrics and logs.
- `apisix.disable_upstream_healthcheck: true` omits active probes from cluster configuration while retaining ordinary weighted selection.
- Route generations are retired asynchronously: publishing a new handler never blocks behind a long-lived request on the previous generation.

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
- Exact APISIX/OpenResty etcd watch resync and lifecycle semantics. The
  production profile uses its bounded reachability probe for readiness and
  does not claim OpenResty timing parity.
- WebSocket upgrades require effective route or service
  `enable_websocket: true`. Every WebSocket upgrade attempt skips response
  callbacks; request, authentication, access, before-proxy, and log phases
  still run. For `http-data-plane-v1`, the admission and timeout guarantee
  applies only to profile-allowed HTTP reverse-proxy tunnels; retired route
  generations close them during generation shutdown. Kafka PubSub compatibility
  routes are outside this profile contract.
- Zipkin is v2-only. OTel rejects `set_ngx_var: true` and any non-zero
  `inactive_timeout`; collector `request_timeout` remains supported.
- `SIGHUP` performs graceful shutdown and returns an unsupported-reload error;
  it is not an in-process configuration reload.

No placeholder implementation is added for these native or separate-runtime
features. They should be treated as unsupported when deploying an official
configuration file with `apisix-go`.
