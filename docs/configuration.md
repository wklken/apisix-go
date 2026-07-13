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
| `apisix.proxy_mode` and `apisix.stream_proxy.tcp` | Select and start the existing HTTP/TCP stream runtimes. UDP listeners are parsed but not started. |
| `plugins`, `stream_plugins`, and `plugin_attr` | Control the existing plugin registration, stream plugin selection, and plugin-specific settings. |
| `graphql.max_size` | Applies to the GraphQL limit and GraphQL proxy-cache plugins. |
| `apisix.data_encryption` | Configures encrypted resource-field handling. |
| `nginx_config.http.keepalive_timeout` | Maps to `http.Server.IdleTimeout`. |
| `nginx_config.http.client_header_timeout` and `client_body_timeout` | Map to the corresponding Go read timeouts; the body timeout uses the combined header/body deadline because `net/http` has no body-only server timeout. |
| `nginx_config.http.send_timeout` | Maps to `http.Server.WriteTimeout`. |
| `deployment.etcd.host`, `prefix`, `user`, `password`, `timeout`, `startup_retry`, and `tls` | Configure the etcd client endpoints, prefix, credentials, dial/request timeout, startup retries, client certificate, verification, and SNI. |

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
- Dynamic HTTPS listener serving, HTTP/3/QUIC, PROXY protocol, and UDP stream
  proxying. HTTPS certificate selection is a dynamic APISIX resource concern,
  not represented by the official listener-only config fields.
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
