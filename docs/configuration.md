# Configuration compatibility

`apisix-go` accepts the YAML shape of the official Apache APISIX
[`conf/config.yaml.example`](https://github.com/apache/apisix/blob/master/conf/config.yaml.example),
including its scalar and mapping forms for listeners. The Go loader keeps
configuration that has no direct Go equivalent in the typed configuration
object so an official file can be loaded without being rewritten.

## Applied by the Go runtime

| Configuration | Go behavior |
| --- | --- |
| `apisix.node_listen` | Opens every configured TCP HTTP listener. Both `9080` and `{port: 9080, ip: ...}` forms are accepted. |
| `apisix.proxy_mode` and `apisix.stream_proxy.tcp` | `http` leaves stream settings unused. When `proxy_mode` contains `stream`, the bounded raw-TCP/MQTT stream runtime requires at least one TCP listener and starts only after routes, upstream references, listener binds, and supported flags validate successfully. |
| `plugins`, `stream_plugins`, and `plugin_attr` | Control the existing plugin registration, stream plugin selection, and plugin-specific settings. |
| `graphql.max_size` | Applies to the GraphQL limit and GraphQL proxy-cache plugins. |
| `apisix.data_encryption` | Configures encrypted resource-field handling. |
| `nginx_config.http.keepalive_timeout` | Maps to `http.Server.IdleTimeout`. |
| `nginx_config.http.client_header_timeout` and `client_body_timeout` | Map to the corresponding Go read timeouts; the body timeout uses the combined header/body deadline because `net/http` has no body-only server timeout. |
| `nginx_config.http.send_timeout` | Maps to `http.Server.WriteTimeout`. |
| `deployment.etcd.host`, `prefix`, `user`, `password`, `timeout`, `startup_retry`, and `tls` | Configure the etcd client endpoints, prefix, credentials, dial/request timeout, startup retries, client certificate, verification, and SNI. |
| `deployment.role: data_plane` with `role_data_plane.config_provider: yaml` or `json` | Loads resource snapshots from `conf/apisix.yaml` or `conf/apisix.json`, watches the file, and applies additions, updates, and removals through the local store. |
| `proxy.max_idle_conns` | Global maximum number of idle (keep-alive) connections kept open across all upstream hosts. Default 1024; zero selects the default. |
| `proxy.max_idle_conns_per_host` | Maximum number of idle connections kept open per upstream host. Default 250; zero selects the default. |
| `proxy.max_conns_per_host` | Maximum number of concurrent connections per upstream host. Default 1024; zero selects the default. |
| `proxy.max_in_flight` | Maximum number of concurrently active upstream response bodies per cluster. Default 1024; zero selects the default. Negative values are rejected at configuration load. |

## HTTP upstream proxy behavior

- A route timeout overrides the corresponding upstream timeout; zero inherits from the upstream.
- `connect` limits TCP connection establishment. `send` and `read` are inactivity limits which reset when body I/O makes progress. `read` also limits the wait for response headers.
- `tls.verify: true` validates an HTTPS upstream certificate. Omitted or false preserves APISIX-compatible insecure verification behavior.
- Automatic retries require a replayable body. POST and PATCH additionally require `Idempotency-Key` or `X-Idempotency-Key`.
- `proxy-control` buffers at most 8 MiB in memory. A larger buffered request is rejected with HTTP 413.
- An invalid initial route generation stops startup. An invalid reload retains the last successfully published generation.
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
plugin attributes. Recognition means the file is accepted and values are
retained; it does not imply that a native NGINX/Lua subsystem exists in the Go
runtime.

## Intentionally unsupported

These settings remain parsed but have no effect unless they map to a behavior
listed above:

- OpenResty/NGINX worker directives, Lua module paths/hooks, Lua shared-dict
  sizing, NGINX configuration snippets, access-log formatting, and NGINX
  variable/real-IP directives.
- Dynamic HTTPS listener serving, HTTP/3/QUIC, stream TLS/mTLS, PROXY protocol,
  and UDP stream proxying. In stream mode, empty TCP listener sets, listener or
  upstream TLS/PROXY protocol flags, top-level TCP PROXY protocol flags,
  unresolved upstream references, unsupported stream plugins, invalid listener
  addresses, and bind failures are rejected at startup rather than ignored.
  HTTPS certificate selection is a dynamic APISIX resource concern, not
  represented by the official listener-only config fields.
- General stream-plugin chaining, stream metrics, and a dynamic readiness
  endpoint. The repository has no readiness endpoint; startup failures are
  surfaced through the process return and dynamic health publication belongs to
  a later observability contract.
- The APISIX Admin API, control API, status server, admin UI, admin CORS/IP
  restrictions, and admin mTLS. The current Go admin router is not a complete
  APISIX Admin API implementation.
- Lua external plugins, WASM plugins, XRPC protocol plugins, and the official
  discovery providers (`dns`, Eureka, Nacos, Consul, and Kubernetes).
- etcd watch resync/health-check timing and exact APISIX/OpenResty lifecycle
  semantics.

No placeholder implementation is added for these native or separate-runtime
features. They should be treated as unsupported when deploying an official
configuration file with `apisix-go`.
