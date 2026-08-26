# Configuration compatibility

`apisix-go` accepts the documented scalar and mapping listener forms and a
bounded subset of the YAML shape in the official Apache APISIX
[`conf/config.yaml.example`](https://github.com/apache/apisix/blob/master/conf/config.yaml.example).
Recognition is a compatibility boundary, not an activation guarantee. In
compatibility mode, unknown static fields remain only as provenance and are
reported by opaque handles in the redacted effective-config output; they are
not decoded into the typed `Config`. Strict security rejects unknown static
fields. YAML anchors, aliases, and merge keys fail closed, and LuaJIT hex-float
template retyping is not qualified. Explicitly unsupported runtime activations
listed below also fail closed when configured.

## Effective static configuration

### Precedence, presence, and provenance

The precedence order is built-in defaults, the default file, the selected
override file, `APISIXGO_*`, and repeatable CLI `--set` overrides. The default
file is `conf/config-default.yaml`; `-c`/`--config` selects the override file.
APISIX template expansion happens inside each parsed file layer. It is not a
separate overlay after the files.

Within a merge, mappings merge recursively and a sequence replaces the lower
sequence. A field absent from an upper layer inherits the lower value. An
explicit `null` replaces the lower value and remains present in provenance;
explicit `false`, zero, an empty string, an empty mapping, and an empty sequence
are likewise distinct from absence. Configuration integers are retained
exactly rather than being decoded through `float64`.

Every winning field records one of the provenance source kinds `builtin`,
`default_file`, `override_file`, `apisix_env`, `apisixgo_env`, or `cli`, plus an
approved origin and whether the source was explicit. Under
`security_profile: strict`, any unknown static field fails configuration load.
Under `security_profile: compat`, unknown fields remain available only for
provenance and ignored-field diagnostics; their raw keys and values are not
rendered.

### APISIX templates and APISIXGO overrides

APISIX-compatible file templates use `${{NAME}}` or
`${{NAME:=fallback}}`, including substitutions in YAML keys. A missing variable
without a fallback is an error. The variable is expanded while its owning file
is parsed and is recorded as the winning `apisix_env` source.

`APISIXGO_*` is a separate, Go-specific namespace for typed static overrides.
It is applied after both files, accepts only schema-derived aliases, and rejects
unknown aliases. For example, `APISIXGO_DEPLOYMENT_ETCD_HOST` supplies the etcd
endpoint list and accepts comma-separated endpoints. A repeatable CLI override
uses the complete typed path, for example:

```bash
apisix -c conf/config-example.yaml \
  --set proxy.max_in_flight=2048 \
  --set apisix_go.runtime_paths.log_dir=relative-logs
```

The former `deployment.profile` field has been removed from files, environment
overrides, and CLI overrides. Use `compatibility_target`, `security_profile`,
and `qualification_profile` independently. `APISIXGO_DEPLOYMENT_PROFILE` is
rejected by the same migration boundary.

### Runtime paths

The four Go-owned runtime paths are:

| Typed path | Environment alias |
| --- | --- |
| `apisix_go.runtime_paths.data_dir` | `APISIXGO_RUNTIME_PATHS_DATA_DIR` |
| `apisix_go.runtime_paths.runtime_dir` | `APISIXGO_RUNTIME_PATHS_RUNTIME_DIR` |
| `apisix_go.runtime_paths.log_dir` | `APISIXGO_RUNTIME_PATHS_LOG_DIR` |
| `apisix_go.runtime_paths.temp_dir` | `APISIXGO_RUNTIME_PATHS_TEMP_DIR` |

The short aliases intentionally omit the repeated `APISIX_GO` segment; aliases
such as `APISIXGO_APISIX_GO_RUNTIME_PATHS_DATA_DIR` do not exist. Bootstrap
derives platform defaults from Go's user configuration, user cache, and
temporary-directory APIs and injects them as built-in defaults. Those defaults
are not a fixed Linux `/var` layout.

`data_dir` must always resolve to a non-empty absolute path. Selecting a
qualification profile requires all four paths to resolve to non-empty absolute
paths. A relative value in a file resolves against that file's directory. A
relative `APISIXGO_*` or CLI value resolves against the selected override file's
directory, or the default file's directory when there is no override. The
durable journal is always `data_dir/apisix-go-store.db`.

### Static inspection commands

Use the compatibility example for successful inspection:

```bash
apisix config test -c conf/config-example.yaml
apisix config dump --effective --redacted -c conf/config-example.yaml
apisix config test -c conf/config-example.yaml --set proxy.max_in_flight=2048
apisix config dump --effective --redacted -c conf/config-example.yaml --set apisix_go.runtime_paths.log_dir=relative-logs
```

`apisix config test` validates only static read/merge/decode/profile contracts.
On success it prints exactly `configuration is valid`. It does not create/check
directory permissions, open/migrate the journal, bind ports, contact
etcd/providers, configure logging, or prove runtime readiness. It also does not
start a provider, server, or background goroutine.

`apisix config dump` requires both `--effective` and `--redacted`; there is no
unredacted mode. The registered secret contract covers the encryption keyring,
admin keys, the etcd password, sanitized etcd URL userinfo, all plugin-attribute
values, and discovery-provider configuration values. Unknown paths omit their
original keys and values; one opaque correlation handle links provenance to the
ignored-field list. Known `apisix_env` provenance paths use opaque handles but
are not treated as ignored fields. When such a path contains a dynamic mapping
key, the same handle can correlate its safe config key with provenance. Known
non-secret configuration values remain visible in the typed config output.
`AdminSSLCertKey` and `EtcdTLS.Key` remain visible as file paths, not as inline
private-key contents.

The dump also retains approved operational metadata such as profiles, file
paths, provider and plugin names, environment variable names, and sanitized
hosts. Treat it as a sensitive diagnostic artifact even though secret values
are redacted.

### Journal relocation and rollback

When upgrading from the cwd-relative journal, stop the old process, back up the
cwd `apisix-go-store.db`, create and permission the selected `data_dir`, and
copy/verify the database as `data_dir/apisix-go-store.db`, then start exactly one
instance and validate the published resource generation before increasing the
replica count, and retain the backup for rollback. Starting without moving the
old journal presents an empty local state until providers repopulate it.

## Production container configuration

The image loads `/usr/local/apisix/conf/config-default.yaml` and layers
`/usr/local/apisix/conf/config-production.yaml` over it. The production override
has an empty `deployment.etcd.host` and no `apisix_go.runtime_paths` overlay.
The repository snapshot is currently unqualified and its production
configuration is expected to fail closed until an operator supplies a real
etcd endpoint and the central manifest records complete qualification evidence.
The endpoint can be supplied through `APISIXGO_DEPLOYMENT_ETCD_HOST`
(comma-separated for multiple endpoints) or an operator-managed override; the
image does not fall back to a local etcd address.

The following is an operator-owned overlay example, not the checked-in
production file or the image's filesystem contract:

```yaml
compatibility_target: apisix-3.17
security_profile: strict
qualification_profile: http-data-plane-v1

apisix_go:
  runtime_paths:
    data_dir: /var/lib/apisix-go
    runtime_dir: /run/apisix-go
    log_dir: /var/log/apisix-go
    temp_dir: /var/tmp/apisix-go
```

The operator must create or mount all four directories and set their ownership
and permissions before startup. The current Dockerfile creates only
`/usr/local/apisix/conf`, `/usr/local/apisix/logs`, and
`/usr/local/apisix/data`; it does not create the `/var` paths above. The
checked-in `conf/config-production.yaml` intentionally omits this runtime-path
overlay, and this documentation does not change either file.

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
be explicitly `true` under strict security. The effective HTTP plugin sequence
must exactly equal the manifest `required_plugins` sequence, including order.
Qualification derives from that ordered sequence and its required evidence.
Under strict security,
enabled effective `key-auth`, `basic-auth`, and `jwt-auth` configurations must
set `hide_credentials: true`; `jwt-auth` must also include literal `exp` in
`claims_to_verify`. Effective HTTPS and gRPCS upstreams must set
`tls.verify: true` after inline, ID, or service resolution. Disabled auth
configurations remain inert. `security_profile: compat` retains the
APISIX-compatible defaults for these dynamic fields. Qualification selection
does not disable the Kafka compatibility owner. `security_profile: strict`
permits plaintext Kafka. When Kafka TLS is configured,
`security_profile: strict` requires `tls.verify: true`;
`security_profile: compat` permits `tls.verify: false`. Kafka remains outside
the HTTP qualification evidence claim.

NGINX HTTP and stream process access-log settings are unsupported in every
profile, not only in `http-data-plane-v1`: any explicitly non-zero boolean or
numeric value, or non-empty string value, fails configuration load. Route/plugin
loggers are the supported compatibility/general-plugin request-logging
mechanism, whose output is owned by the Go request pipeline. Qualification
selection makes no request-logging egress claim; consult the generated status
for the current `required_plugins` sequence.

The separate status listener is configured by `apisix.status.ip` and
`apisix.status.port`, defaulting to `127.0.0.1:7085`. `/status` returns HTTP 200
while that listener is serving. `/status/ready` returns HTTP 503 until a
serviceable configuration has committed, then returns HTTP 200. The image does
not define a Docker healthcheck. Orchestrators should use the status listener
for HTTP readiness; `/livez` and `/readyz` on data-plane listeners are ordinary
user route paths.

The production release contract requires a bounded periodic etcd reachability
probe in addition to the watch loop. During a verified recovery test, etcd loss
must leave `/status/ready` at 200 while the last successfully committed route
continues serving; the reachability metric must report the loss, recovery must
restore it, and a newer revision must apply. See the
[production release runbook](runbooks/production-release.md) for the evidence
and operator-supplied deployment step.

Deterministically invalid route, global-rule, consumer, and SSL payloads are
rejected before replacing their last-good store value. During HTTP generation
build, a semantic failure scoped to one route quarantines that complete route;
the remaining valid routes still publish. A malformed route or global-rule row
left by an older database is likewise omitted from the immutable build
snapshot. Every omission keeps the no-label
`config_apply_quarantined_resources` gauge non-zero and `/status/ready` at 503.
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

Selection derives the ordered `required_plugins` sequence and evidence result
from the manifest and fails closed on any sequence mismatch or blocked
requirement.

## Applied by the Go runtime

| Configuration | Go behavior |
| --- | --- |
| `apisix.node_listen` | Opens every configured TCP HTTP listener. Both `9080` and `{port: 9080, ip: ...}` forms are accepted. |
| `compatibility_target` | Selects the pinned observable compatibility contract. The current accepted value is `apisix-3.17`; other values fail startup. |
| `security_profile` | Selects `compat` or `strict` security behavior independently from compatibility and qualification. |
| `qualification_profile` | Empty makes no qualification claim. `http-data-plane-v1` is selectable only when its manifest `required_plugins` sequence exactly matches the effective plugins, including order, and every entry has complete required evidence; otherwise startup fails closed. |
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
- `qualification_profile: http-data-plane-v1` makes no Kafka evidence claim,
  but does not disable upstreams with `scheme: kafka` or the Kafka PubSub
  compatibility owner. Security is orthogonal: strict permits plaintext Kafka
  and requires `tls.verify: true` only when Kafka TLS is configured; compat
  permits `tls.verify: false`.
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
- General stream-plugin chaining and stream metrics. Startup failures are
  surfaced through the process return, and `/status/ready` remains unavailable
  until a serviceable configuration has committed.
- The APISIX Admin API, control API, admin UI, admin CORS/IP
  restrictions, and admin mTLS. The current Go admin router is not a complete
  APISIX Admin API implementation.
- Lua external plugins, WASM plugins, XRPC protocol plugins, and the official
  discovery providers (`dns`, Eureka, Nacos, Consul, and Kubernetes). Top-level
  discovery activation fails startup; route/upstream discovery compatibility
  fields are preserved and rejected at route compilation.
- Exact APISIX/OpenResty etcd watch resync semantics. The production
  qualification profile exposes bounded provider reachability as metrics and
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
