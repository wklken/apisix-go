# Outstanding Backlog（待办清单）

> Status: 2026-08-08（已复核）。本文档是 docs 目录全部审计/整改结论的唯一待办落点。
>
> 本轮最初以 `master`（`8478c9a`）、实际生产代码/测试、`docs/plugins.md`，以及 Apache APISIX `3.17.0` tag（`9ef2ecab67f652d38365049613610ef649bb4ad0`）为依据；开始实施前已在当前 `master`（`ccde2ae`）重新核对目标路径。未勾选不再等于“应当实施”：只有“确认要做”中的复选框属于执行队列；其余项目分别记录外部前置条件、测量门槛或驳回原因。
>
> 审计期间工作区并发出现了 3 份尚未合入的 proxy runtime 计划（`proxy-safety-correctness`、`proxy-performance-acceptance`、`upstream-cluster-runtime`）。它们不属于本 backlog 原始条目，本轮未把其中的新增架构工作并入“确认要做”；执行到 traffic-split timeout 或 route performance 前，应先确认两套计划的依赖顺序与文件所有权。

## 确认要做

### P0 — 请求/响应完整性

- [ ] **chaitin-waf 只把截断后的请求体恢复给上游** — `pkg/plugin/chaitin_waf/plugin.go` 的 `askWAF` 通过 `io.LimitReader` 读取后用截断副本替换 `r.Body`。应完整读取并恢复原 body，只把 `req_body_size` 范围内的副本发给 WAF。计划：[`2026-08-08-data-integrity-and-cache-correctness.md`](superpowers/plans/2026-08-08-data-integrity-and-cache-correctness.md)。
- [ ] **request-validation 规范化 JSON 时丢失大整数精度** — `parseJSON` 解码到 `float64` 后重新序列化；应使用 `json.Decoder.UseNumber()`，继续保留“校验对象与上游对象一致”的规范化安全语义。计划：[`2026-08-08-data-integrity-and-cache-correctness.md`](superpowers/plans/2026-08-08-data-integrity-and-cache-correctness.md)。
- [ ] **proxy-cache 的 HEAD MISS 会写入与 GET 共用的空 body 缓存项** — 默认 cache method 同时包含 GET/HEAD，而 cache key 不区分方法。保留 HEAD 命中既有 GET 缓存的能力，但 HEAD MISS 不得创建/覆盖共享缓存项。计划：[`2026-08-08-data-integrity-and-cache-correctness.md`](superpowers/plans/2026-08-08-data-integrity-and-cache-correctness.md)。

### P1 — APISIX 3.17 行为与测试语料对齐

- [ ] **grpc-transcode fractional `deadline` 生成无效 `grpc-timeout`** — schema 接受小数，当前直接拼成 `1.5m`；将 schema 收紧为非负整数毫秒，并覆盖小数拒绝与整数 wire 值。上游 3.17 也存在这个 schema/wire 缺陷，本项是 wire correctness hardening，不宣称上游差异。计划：[`2026-08-08-protocol-parity-and-fixtures.md`](superpowers/plans/2026-08-08-protocol-parity-and-fixtures.md)。
- [ ] **grpc-web 非 POST 返回 400，而 3.17 返回 405** — 改为 405 并更新单元/route parity 断言。计划：[`2026-08-08-protocol-parity-and-fixtures.md`](superpowers/plans/2026-08-08-protocol-parity-and-fixtures.md)。
- [ ] **proxy-rewrite 到 before-proxy 才修改 `r.URL` / method** — 3.17 在 rewrite phase 调用 `ngx.req.set_uri` / `ngx.req.set_method`，后续插件能观察新值；Go 侧应在 proxy-rewrite handler 内完成 URI/method finalize，同时保留 host/scheme 的上游目标阶段处理。计划：[`2026-08-08-protocol-parity-and-fixtures.md`](superpowers/plans/2026-08-08-protocol-parity-and-fixtures.md)。
- [ ] **limit-count TEST 16/17 没有表示“更新同一路由并禁用插件”** — `t/plugin/limit-count.yaml` 当前把 TEST 16 配成仍启用插件，TEST 17 又重新执行限流；应使用一个场景先建立限流状态，再以空 `plugins` 更新同一路由，并断言后续 4 次请求均为 200。计划：[`2026-08-08-protocol-parity-and-fixtures.md`](superpowers/plans/2026-08-08-protocol-parity-and-fixtures.md)。
- [ ] **traffic-split2 TEST 16 丢失两个 `match` 规则** — pinned 3.17 TEST 16 明确以 `arg_id == 1/2` 绑定两个 `upstream_id`；当前 manifest 将其弱化为无条件权重轮询。补回规则，TEST 17 继续分别断言 route/first/second。计划：[`2026-08-08-protocol-parity-and-fixtures.md`](superpowers/plans/2026-08-08-protocol-parity-and-fixtures.md)。

### P2 — 生命周期与安全残留

- [ ] **authz-casdoor 随机源失败时退回可预测时间字符串** — session ID 与 OAuth state 都必须在 `crypto/rand` 失败时 fail closed，不得生成弱随机值。计划：[`2026-08-08-runtime-security-and-lifecycle.md`](superpowers/plans/2026-08-08-runtime-security-and-lifecycle.md)。
- [ ] **三个限流外层共享注册表没有 release 生命周期** — `limit_req.consumerBucketStores`、`limit_count.limitCountGroups.entries`、`graphql_limit_count.graphqlLimitCountGroups.entries` 的内层状态虽有容量/TTL，外层 config/group key 仍只增不删。应增加引用计数，并在 plugin `Stop()` 时释放最后一个 owner。计划：[`2026-08-08-runtime-security-and-lifecycle.md`](superpowers/plans/2026-08-08-runtime-security-and-lifecycle.md)。
- [ ] **openid-connect 每次 bearer 校验都重新解析静态公钥** — 在 `PostInit` 解析已完成 secret resolution 的 `public_key`，请求路径只按 token algorithm 构造 verifier。计划：[`2026-08-08-runtime-security-and-lifecycle.md`](superpowers/plans/2026-08-08-runtime-security-and-lifecycle.md)。
- [ ] **traffic-split 把 connect/send/read 的最小值当成整个请求 deadline** — 该近似会在合法 read timeout 之前取消请求。先删除错误的整体 deadline 承诺与实现；精确的 phase-specific transport timeout 仍作为独立 transport contract 延期。计划：[`2026-08-08-runtime-security-and-lifecycle.md`](superpowers/plans/2026-08-08-runtime-security-and-lifecycle.md)。
- [ ] **mqtt-proxy CONNECT 预读 deadline 没有清除** — `decodeConnect` 成功后立即 `SetReadDeadline(time.Time{})`，避免后续双向复制继承 5 秒预读期限。计划：[`2026-08-08-runtime-security-and-lifecycle.md`](superpowers/plans/2026-08-08-runtime-security-and-lifecycle.md)。

### P3 — Route suffix collision 性能

- [ ] **同 suffix + 多 exact host 候选仍反向线性扫描** — 先补 exact/wildcard host 与 method 的语义测试，以及 10/100/1000 的 collision benchmark；在同一 benchmark corpus 上记录 baseline 后，为 exact host 建立值级索引。只有 1000-route exact-host `match-first`/`host-mismatch` 达到预声明的实用阈值才接受优化，wildcard host 只要求语义不回归。计划：[`2026-08-08-route-suffix-collision-performance.md`](superpowers/plans/2026-08-08-route-suffix-collision-performance.md)。

## 有条件延期（当前不进入实施队列）

- **wolf-rbac `ok:false`**：pinned 3.17 的 `/wolf/rbac/access_check` 同样只按 HTTP status 判定；在没有 Wolf server 契约或可复现的 2xx+`ok:false` 拒绝响应前，不把它定为权限绕过。
- **ai-prompt-guard 最后一条非 user 消息**：当前顺序与 3.17 完全一致（先取最后一条，再按 role 过滤）。改为“回退最后一条 user”属于产品语义变化，需要单独确认，不作为 parity 修复。
- **ai-aliyun-content-moderation 故障 fail-open**：3.17 明确记录错误后继续请求。若要 fail-closed，应先增加显式 `fail_mode` 契约，不能无条件 deny。
- **GM SSL marker**：当前 `resource.SSL` 不含 GM 双证书字段，Go TLS 也不提供 Tongsuo/NTLS serving；只接一个校验 helper 不会形成可用生产路径，保持 native/runtime deferred。
- **rocketmq-logger TLS**：等待 approved RocketMQ client transport hook；当前 hard reject 比静默忽略安全。
- **ai-proxy-multi per-IP DNS health**：需要新的 DNS snapshot、transport dial 和 health ownership 契约，保持 separate subsystem。
- **kafka-proxy 外部 broker smoke**：仅在有外部环境与 credential-safe CI 契约后增加；loopback TLS/SASL fixture 继续作为当前可重复 gate。
- **`security-warning*.t` 38 块**：这些是 19 个插件的 HTTP/TLS 告警日志族，不是功能路径缺失；另立跨插件 observability 项目时再纳入，不混入当前 correctness 批次。
- **exit-transformer Lua VM 池化**：`LState` 清理/复用有状态泄漏风险；先用正式 benchmark/profile 证明 `lua.NewState` 是实际瓶颈，再决定是否池化。JSON decode 是官方向 Lua table 传值所需，不单独删除。
- **完整 route reload benchmark**：没有已声明的 reload 性能目标或已复现回归；需要 reload SLO、真实插件组合和 atomic swap owner 后再建语料。

## 审计后移除（不应实施）

- **authz-casbin 改绑 `$consumer_name`**：驳回。3.17 schema 的 `username` 就是 header 名，官方实现直接读取该 header；强制 consumer 变量会破坏独立 authz-casbin 用法。
- **ai-aws-content-moderation 增加 `check_request` / `deny_message`**：驳回。两个字段不存在于 APISIX 3.17 schema；`docs/plugins.md` 的旧声明是错误镜像。
- **node-status 改为 `Dec()`**：驳回。`atomic.Uint64` 没有 `Dec` API，`Add(^uint64(0))` 是无锁减一；手写 `stringUint` 也没有已证明的正确性或性能问题。
- **CORS 只拦真正 preflight**：驳回。3.17 rewrite 明确对所有 OPTIONS 返回 200，当前代码和测试与之相符。
- **P5 依赖删除 Phase 1-4**：从缺陷 backlog 移除。当前依赖均仍有生产调用；“零风险”、统一 writer 或替换 limiter/balancer 都未经行为测试与收益测量。以后只能以具体依赖升级、漏洞、二进制体积或 profile 证据单独立项。
- **手写协议层整体替换**：从缺陷 backlog 移除。JWE/HMAC/SSE/TC3/B3/sw8/Dubbo/Kafka 项必须由具体互操作失败、CVE 或维护成本证据触发；`request-id` UUIDv7/KSUID 继续有意保留。

## 已完成并验证（无需处理）

- etcd watch 重连、bbolt Clone、otel init shutdown、LB 空节点 panic、splunk 换行、google_cloud_logging 超时、key_auth fail-closed、jwt_auth 缺失 claim、sls TLS 校验、error_log_logger kafka close、prometheus 优雅关闭、globalClients refcount、zipkin 超时、casdoor 会话过期、batch_requests 取消、proxy_cache Vary 变体、echo before/after 拼接、proxy-cache no_cache→MISS、acl 字段拼写、graphql 空文档消息、kafka-logger 反向断言、limit-conn key 日志、RS256 2048-bit。
- consistency-audit 台账 60 项抽验全部属实（cacheutil/luautil/dubbo/limitbase/log_batch/base 系列/ai_common 系列/pluginRegistry）。
- 当前 `master` 已合入的历史计划不再重复列为待办；本轮审计生成的 4 份计划只覆盖上述“确认要做”队列。
- client-control 非尺寸读错已修（`TestClientControlMaxBytesReadFailureReturns500`）。
