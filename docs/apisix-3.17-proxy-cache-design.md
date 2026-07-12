# APISIX 3.17 `proxy-cache`：磁盘 Zone 与 Stale 行为设计

> 状态：设计完成；P2 磁盘读写首片、PURGE、跨实例加载、访问时过期清理、按 `disk_size` 的写入后配额驱逐、每分钟一次的流量触发过期扫描、生命周期绑定的后台过期清理、配置 memory zone 的跨实例共享、route-builder refresh 生命周期、变更定义后的 memory-zone 代际隔离、`graphql-proxy-cache` 的共享 memory/disk zone 存储、zone registry 基础校验与 route replacement 前的完整静态 registry 预检、进程内动态 zone registry 原子刷新、identity-aware `cache_control`/strategy-specific `cache_set_cookie` 规则和跨插件 stale 策略审计已实现（2026-07-12）
>
> 相关实现：[`pkg/plugin/proxy_cache/plugin.go`](../pkg/plugin/proxy_cache/plugin.go)
>
> 上游参考：[`disk_handler.lua`](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/proxy-cache/disk_handler.lua)、[`memory_handler.lua`](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/proxy-cache/memory_handler.lua)

## 1. 当前事实与边界

- 插件已经支持 `cache_key`、方法/状态过滤、绕过与 no-cache、memory zone 的 `Cache-Control` TTL/请求 freshness（含 identity-bearing `cache_key` 时按 APISIX 规则关闭该行为）、disk zone 对 `cache_control` 的忽略、memory-only `cache_set_cookie`、`Vary`、`PURGE`、消费者隔离和 `Apisix-Cache-Status`。
- `pkg/config/types.go` 能读取 `apisix.proxy_cache.cache_ttl` 和 `zones`；插件初始化时会对已配置 registry 做基础校验（重复/空名称、size/path、cache_levels、未知引用和 cache strategy/zone 存储类型匹配），并把声明的 memory zone 接入共享存储。严格 cache 初始化错误会阻止 replacement route handler 安装，避免刷新后静默丢失缓存插件。
- 配置了绝对 `disk_path` 的 `cache_strategy = "disk"` 会使用版本化磁盘 envelope，并在插件实例间按摘要路径重新加载；未配置 zone 时仍保留进程内 memory fallback。
- 访问发现条目已过期时，会同时删除对应的内存副本和磁盘文件；写入磁盘条目后会按 zone 的 `disk_size` 删除过期文件和最旧文件；磁盘 lookup 最多每分钟触发一次受界扫描，配置的 disk zone 另有绑定插件生命周期、可停止的后台过期清理线程。
- 现有 `lookup` 保留过期条目并返回 `EXPIRED`；请求侧 `max-age`、`max-stale`、`min-fresh` 不满足时返回 `STALE`，随后重新请求上游。当前没有 stale-if-error 或过期内容兜底响应。
- `graphql-proxy-cache` 复用相同的 zone 存储 envelope 和过期生命周期；磁盘策略按上游 `Cache-Control: s-maxage/max-age` 或 `Expires` 计算 TTL、无响应头时回退到插件 `cache_ttl`，并与 memory 策略一样始终拒绝 `private`/`no-store`/`no-cache` 响应，而其公开 purge 路径、缓存键格式和 GraphQL mutation bypass 必须保持兼容。
- `RefreshConfiguredZones` 是进程内配置刷新边界：它先校验完整 zone snapshot，再原子替换配置指针；无效 snapshot 不会覆盖最后一个有效配置，已有插件实例继续持有旧代际并通过引用计数独立排空。读取 zone registry 的内部路径会复制当前 snapshot，未声明 zone 仍保持兼容性的进程内 memory fallback。

本设计只覆盖 Go HTTP proxy 能够稳定表达的共享缓存行为，不复刻 OpenResty shared-dict、NGINX cache manager 或跨 worker 的内部生命周期。

## 2. Zone 配置契约

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

## 3. 存储抽象与磁盘格式

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

## 4. Fresh / Expired / Stale 语义

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

## 5. 分阶段实现与验收

### P1：zone 注册与 memory 共享

- [x] 为已声明 `memory` zone 提供线程安全的共享 registry、entries/vary index 和引用计数生命周期；配置定义变化时按代际隔离 entries，旧代际独立排空。
- [x] 将 `apisix.proxy_cache.zones` 做成基础严格校验的配置 registry，覆盖重复/空名称、size/path/cache_levels 格式和未知 zone 引用；route replacement 会先预检完整静态 registry，`RefreshConfiguredZones` 负责动态刷新时的完整 snapshot 校验与原子替换。
- [x] 拒绝 plugin `cache_strategy` 与 zone `disk_path` 不匹配的配置，并拒绝 `$request_method` cache key；`graphql-proxy-cache` 复用相同的 strategy/zone 校验。
- [x] route builder 对 proxy-cache/graphql-proxy-cache 的严格初始化错误停止 replacement handler 构建；普通插件的历史 skip-on-error 行为保持不变。
- 保持当前 route/plugin 行为和 `PURGE` 结果不变。

### P2：disk 读写

- 已实现版本化 envelope、摘要路径、原子写入、跨实例加载、PURGE、访问时过期清理、写入后的 `disk_size` 超限驱逐、流量触发扫描和生命周期绑定的后台过期清理；声明的 memory zone 也有共享生命周期和引用计数。配置刷新边界、跨插件一致性和超限验收均已有对应实现或测试。
- 覆盖重启后命中、损坏文件按 MISS、并发写入、目录不可写、超限驱逐和 `PURGE`。
- 通过临时目录测试；测试结束清理文件，不依赖 `/tmp` 中的固定目录或用户 home。

### P3：stale 与跨插件一致性

- [x] 让 `proxy-cache` 与 `graphql-proxy-cache` 共用已声明 zone 的 memory registry、disk envelope 和过期清理生命周期；两个插件仍保留各自的缓存键、PURGE 路径和请求策略。
- [x] 覆盖 `Vary` 变体、过期 index、配置 TTL、`only-if-cached`、上游错误不返回 stale body 的回归测试；官方 `graphql-proxy-cache` 不暴露 `cache_control`，不增加跨插件隐式 stale-if-error。
- 对 route/service/consumer 缓存键做跨插件隔离测试。

`RefreshConfiguredZones` 只承诺进程内、已校验 snapshot 的配置替换；不能据此声称完整 NGINX cache-manager 或跨 worker runtime parity。跨插件 stale-if-error 仍不会被隐式开启。
