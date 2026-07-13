# APISIX 3.17 Design Notes

> Consolidated design documents for the Go-native APISIX 3.17 implementation.
>
> The design baseline is APISIX `release/3.17`; these notes describe intentional Go-native boundaries and separate-subsystem decisions.

## APISIX 3.17 Protocol Bridge Design

> Status: design baseline plus bounded Dubbo/Kafka slices and a TCP/MQTT stream owner, 2026-07-12
> This document is the contract for implementing the protocol plugins in
> [`plugins.md`](plugins.md).

The current Go runtime has an HTTP `http.Handler` pipeline and now also owns a
bounded TCP stream listener/route snapshot with cancellation and result/log
callbacks. It does not yet expose a general stream-variable/plugin-chain API,
active health probes, TLS/UDP stream owner, or Kafka-specific stream
binding. Protocol-owned bounded transport and stream boundaries therefore have
different integration states:

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
facade. The official Go scope is therefore:

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
remains unchanged.

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

---

## APISIX 3.17 `proxy-cache`：磁盘 Zone 与 Stale 行为设计

> 状态：设计完成；P2 磁盘读写首片、PURGE、跨实例加载、访问时过期清理、按 `disk_size` 的写入后配额驱逐、每分钟一次的流量触发过期扫描、生命周期绑定的后台过期清理、配置 memory zone 的跨实例共享、route-builder refresh 生命周期、变更定义后的 memory-zone 代际隔离、`graphql-proxy-cache` 的共享 memory/disk zone 存储、zone registry 基础校验与 route replacement 前的完整静态 registry 预检、进程内动态 zone registry 原子刷新、identity-aware `cache_control`/strategy-specific `cache_set_cookie` 规则和跨插件 stale 策略审计已实现（2026-07-12）
>
> 相关实现：[`pkg/plugin/proxy_cache/plugin.go`](../pkg/plugin/proxy_cache/plugin.go)
>
> 上游参考：[`disk_handler.lua`](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/proxy-cache/disk_handler.lua)、[`memory_handler.lua`](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/proxy-cache/memory_handler.lua)

### 1. 当前事实与边界

- 插件已经支持 `cache_key`、方法/状态过滤、绕过与 no-cache、memory zone 的 `Cache-Control` TTL/请求 freshness（含 identity-bearing `cache_key` 时按 APISIX 规则关闭该行为）、disk zone 对 `cache_control` 的忽略、memory-only `cache_set_cookie`、`Vary`、`PURGE`、消费者隔离和 `Apisix-Cache-Status`。
- `pkg/config/types.go` 能读取 `apisix.proxy_cache.cache_ttl` 和 `zones`；插件初始化时会对已配置 registry 做基础校验（重复/空名称、size/path、cache_levels、未知引用和 cache strategy/zone 存储类型匹配），并把声明的 memory zone 接入共享存储。严格 cache 初始化错误会阻止 replacement route handler 安装，避免刷新后静默丢失缓存插件。
- 配置了绝对 `disk_path` 的 `cache_strategy = "disk"` 会使用版本化磁盘 envelope，并在插件实例间按摘要路径重新加载；未配置 zone 时仍保留进程内 memory fallback。
- 访问发现条目已过期时，会同时删除对应的内存副本和磁盘文件；写入磁盘条目后会按 zone 的 `disk_size` 删除过期文件和最旧文件；磁盘 lookup 最多每分钟触发一次受界扫描，配置的 disk zone 另有绑定插件生命周期、可停止的后台过期清理线程。
- 现有 `lookup` 保留过期条目并返回 `EXPIRED`；请求侧 `max-age`、`max-stale`、`min-fresh` 不满足时返回 `STALE`，随后重新请求上游。当前没有 stale-if-error 或过期内容兜底响应。
- `graphql-proxy-cache` 复用相同的 zone 存储 envelope 和过期生命周期；磁盘策略按上游 `Cache-Control: s-maxage/max-age` 或 `Expires` 计算 TTL、无响应头时回退到插件 `cache_ttl`，并与 memory 策略一样始终拒绝 `private`/`no-store`/`no-cache` 响应，而其公开 purge 路径、缓存键格式和 GraphQL mutation bypass 必须保持兼容。
- `RefreshConfiguredZones` 是进程内配置刷新边界：它先校验完整 zone snapshot，再原子替换配置指针；无效 snapshot 不会覆盖最后一个有效配置，已有插件实例继续持有旧代际并通过引用计数独立排空。读取 zone registry 的内部路径会复制当前 snapshot，未声明 zone 仍保持兼容性的进程内 memory fallback。

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

启动或配置刷新时必须完成以下校验：

1. zone 名称非空、唯一，并且只允许插件 `cache_zone` 引用已声明的 zone。
2. `memory_size`、`disk_size` 使用明确的字节单位；溢出、零值和负值拒绝启动。
3. disk zone 必须有绝对 `disk_path`；路径由本地配置提供，不能来自请求或 route/plugin 配置。
4. `cache_levels` 只允许正整数层级（例如 `1:2`），并限制总层数和单层宽度。
5. 目录创建、权限和磁盘可写性在首次使用前检查；失败时返回明确错误，不静默切换到磁盘之外的路径。

已声明的 `memory` zone 按 zone 名称和配置代际共享 entries/vary index，并通过引用计数在最后一个插件实例停止时释放；未声明 zone 仍使用兼容性的进程内 fallback。配置重载时定义发生变化会创建新代际，旧代际不能在仍被请求引用时提前释放。

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
- [x] 将 `apisix.proxy_cache.zones` 做成基础严格校验的配置 registry，覆盖重复/空名称、size/path/cache_levels 格式和未知 zone 引用；route replacement 会先预检完整静态 registry，`RefreshConfiguredZones` 负责动态刷新时的完整 snapshot 校验与原子替换。
- [x] 拒绝 plugin `cache_strategy` 与 zone `disk_path` 不匹配的配置，并拒绝 `$request_method` cache key；`graphql-proxy-cache` 复用相同的 strategy/zone 校验。
- [x] route builder 对 proxy-cache/graphql-proxy-cache 的严格初始化错误停止 replacement handler 构建；普通插件的历史 skip-on-error 行为保持不变。
- 保持当前 route/plugin 行为和 `PURGE` 结果不变。

#### P2：disk 读写

- 已实现版本化 envelope、摘要路径、原子写入、跨实例加载、PURGE、访问时过期清理、写入后的 `disk_size` 超限驱逐、流量触发扫描和生命周期绑定的后台过期清理；声明的 memory zone 也有共享生命周期和引用计数。配置刷新边界、跨插件一致性和超限验收均已有对应实现或测试。
- 覆盖重启后命中、损坏文件按 MISS、并发写入、目录不可写、超限驱逐和 `PURGE`。
- 通过临时目录测试；测试结束清理文件，不依赖 `/tmp` 中的固定目录或用户 home。

#### P3：stale 与跨插件一致性

- [x] 让 `proxy-cache` 与 `graphql-proxy-cache` 共用已声明 zone 的 memory registry、disk envelope 和过期清理生命周期；两个插件仍保留各自的缓存键、PURGE 路径和请求策略。
- [x] 覆盖 `Vary` 变体、过期 index、配置 TTL、`only-if-cached`、上游错误不返回 stale body 的回归测试；官方 `graphql-proxy-cache` 不暴露 `cache_control`，不增加跨插件隐式 stale-if-error。
- 对 route/service/consumer 缓存键做跨插件隔离测试。

`RefreshConfiguredZones` 只承诺进程内、已校验 snapshot 的配置替换；不能据此声称完整 NGINX cache-manager 或跨 worker runtime parity。跨插件 stale-if-error 仍不会被隐式开启。

---

## APISIX 3.17 Secret Resolution Design

> Status: resolver API/field registry implemented; migrated logger credentials and the explicit `response-rewrite.body_secret` extension use strict plugin-boundary resolution, while ordinary `response-rewrite.body` remains compatibility-oriented, 2026-07-12

### Existing contract

APISIX data-encryption values are base64-encoded AES-128-CBC ciphertext. The
configured `apisix.data_encryption.keyring` is ordered newest-first; existing
route/consumer/plugin-metadata parsing already tries every key and replaces a
registered encrypted field with plaintext before the plugin is initialized.

The Go implementation keeps that wire/storage format and does not add a new
ciphertext prefix. This avoids rewriting existing etcd data and keeps rotation
compatible with APISIX.

### Resolver API

`pkg/data_encryption` now exposes:

```go
resolver := data_encryption.NewResolver(enabled, keyring)
plain, err := resolver.Resolve(ciphertext)       // strict encrypted field
plain := resolver.ResolveOptional(value)         // legacy plaintext compatible
redacted := data_encryption.Redact(value)       // fixed "[REDACTED]" marker
```

Strict `Resolve` has explicit failure classes:

| Condition | Result |
|---|---|
| Encryption disabled | Return the configured value unchanged. |
| Encryption enabled with no keyring | `ErrKeyUnavailable`; do not attempt a network call. |
| No key decrypts the value | `ErrInvalidCiphertext`; the error contains no value or key. |
| Any key in the ordered ring decrypts the value | Return plaintext. |
| Empty value | Return empty value without error. |

`ResolveOptional` is retained only for compatibility with existing plaintext
configurations. It preserves the input when strict resolution fails. New
encrypted fields should use strict resolution at the boundary where the caller
can return a configuration error; the store's historical registered-field
decryption remains compatibility-oriented until each plugin is migrated.

### Key source and rotation

- Source: `apisix.data_encryption.keyring` loaded by `pkg/config.Load`.
- Read path: all configured keys are tried newest-first.
- Rotation: add the new key at index 0 and retain old keys until all stored
  values have been rewritten; no old-key deletion is performed automatically.
- Write path: this repository currently does not expose a generic encryption
  API, so plugins never log or persist newly generated ciphertext themselves.
- Startup/runtime failure: a strict resolver error is returned to the owning
  config/route boundary; it must not be downgraded to a network request with an
  empty credential.

### Redaction rules

- Never include plaintext or ciphertext in an error, log line, metric label,
  serialized status response, or `Config.String` implementation.
- Use the fixed `[REDACTED]` value for non-empty secret display fields.
- Keep secret values in local variables only for the duration of the outbound
  request/codec operation.
- Tests must assert both successful use and that diagnostic output does not
  contain the secret.

### Field registry and migration order

The shared `pluginFields` registry now includes the remaining normal-parity
logger and response fields. Store parsing uses the same resolver's optional
compatibility path for fields not yet migrated to a plugin boundary:

1. Kafka logger, RocketMQ/ClickHouse/SLS logger credentials;
2. Google Cloud, Splunk HEC, Elasticsearch, Loggly, Tencent CLS, and Lago
   credentials;
3. `error-log-logger` nested ClickHouse/Kafka credentials;
4. `csrf.key` and `response-rewrite.body` compatibility values plus the strict
   `body_secret` opt-in extension.

`csrf.key`, `http-logger.auth_header`, `kafka-logger.brokers[*].sasl_config.password`,
`clickhouse-logger.password`, `sls-logger.access_key_secret`, and
`rocketmq-logger.secret_key`, `elasticsearch-logger.auth.password`,
`loggly.customer_token`, `tencent-cloud-cls.secret_key`, `lago.token`,
`splunk-hec-logging.endpoint.token`, and
`google-cloud-logging.auth_config.private_key` and `response-rewrite.body_secret`
now stay encrypted through store parsing and are resolved in `PostInit`;
`error-log-logger` applies the same strict resolution to its nested
ClickHouse/Kafka credentials, and `kafka-proxy.sasl.password` is resolved at
its plugin boundary before request-context propagation. Invalid ciphertext or
a missing key prevents the owning client/writer/sender/producer or batch
processor from being created. The migration gate is complete for all
integrated credential-bearing boundaries. Ordinary `response-rewrite.body`
remains the one explicitly compatibility-oriented field because it is a
general-purpose response body rather than an unambiguous credential; callers
that need strict handling must use the `body_secret` extension. Each future
secret-bearing field must add valid-ciphertext, invalid-ciphertext,
missing-key, key-rotation, and redaction tests before changing the
README/plugin-matrix coverage percentage.

`response-rewrite.body` remains a compatibility field because it is a
general-purpose response body, not an unambiguous credential. The Go extension
`response-rewrite.body_secret` is the explicit opt-in contract: store parsing
leaves it encrypted, `PostInit` calls strict `Resolver.Resolve`, and invalid or
missing-key ciphertext fails before response handling. `body_secret` cannot be
combined with ordinary `body` or `filters`.
