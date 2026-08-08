# Outstanding Backlog（待办清单）

> Status: 2026-08-08。本文档是 docs 目录全部审计/整改文档合并后的唯一待办落点。
>
> 来源审计（已归档删除，结论已合并到本文档）：
> - `docs/bug-report-2026-08-06.md`（14 高危 / 67 中危 / 79 低危）
> - `docs/code-quality-scan-report-20260807.md`（H1-H21 / M01-M62）
> - `docs/perf-data-structure-review-report-20260805.md`（R1-R5 / P1-P8）
> - `docs/TEST_CONVERSION_REVIEW.md`（21 WRONG / ~130 WEAKENED）
> - `docs/plugins.md` remaining-jobs 列（具体 Go 侧 TODO）
> - `docs/todo_route-performance-followups-20260807.md`（P1/P2/P3）
> - `docs/external-dependency-review-20260808.md`（依赖简化建议）
> - `docs/reinventing-the-wheel-review-20260806.md`（造轮子建议）
> - `docs/code-quality-remediation-suite-closure-20260808.md`（关闭说明）
> - `docs/codebase-consistency-audit.md`（整改台账）
> - `docs/superpowers/plans/*`（执行计划）

## P0 —— 安全/数据完整性（建议优先修复）

### 安全绕过

- [ ] **wolf_rbac 只校验状态码，不校验响应体 `ok` 字段** — `pkg/plugin/wolf_rbac/plugin.go:263-267` `checkPermission` 解码了 `body.OK` 但丢弃；Handler 仅凭 `status != 200` 拒绝（`:161`）。wolf server 以 2xx+`ok:false` 拒绝时请求被放行（权限绕过）。
- [ ] **ai_prompt_guard 最后一条非 user 消息可整体绕过** — `pkg/plugin/ai_prompt_guard/plugin.go:142-151` 会话最后一条是 assistant/tool/system 时 `userMessages` 结果为空 → 直接放行，deny 正则不执行。修复：最后一条非 user 消息时回退扫描最后一条 user 消息。
- [ ] **ai_aliyun_content_moderation 审核服务故障时 fail-open** — `pkg/plugin/ai_aliyun_content_moderation/plugin.go:631-634` `checkSingleContent` 已返回 error，但 `moderateContent` 仍 `if err != nil { return 0, "", "" }`，调用方 code==0 放行。修复：审核 API 失败按 deny 处理。
- [ ] **authz_casbin 用户身份取自客户端可控 header** — `pkg/plugin/authz_casbin/plugin.go:192-197` `username()` 直接 `r.Header.Get(p.config.Username)`，客户端可伪造匹配任意 policy subject。修复：绑定到已认证变量（`$consumer_name`）。

### 数据完整性

- [ ] **chaitin_waf 截断并丢弃请求体后转发** — `pkg/plugin/chaitin_waf/plugin.go:437-441` `askWAF` 用 `io.LimitReader` 读后直接把 `r.Body` 替换为截断内容，放行后上游收到截断 body + 原 Content-Length。修复：完整读 body 发截断副本给 WAF，r.Body 恢复完整内容。
- [ ] **request_validation 校验后改写 body，大整数精度丢失** — `pkg/plugin/request_validation/plugin.go:259-263` 仍 `json.Unmarshal` 无 `UseNumber`，`normalizeJSONBody` 重序列化替换 r.Body。>2^53 整数丢失精度。修复：校验用 `json.Number` 或校验后不改写 body。
- [ ] **proxy_cache HEAD 响应污染 GET 缓存** — `pkg/plugin/proxy_cache/plugin.go:606-622` `cacheKey` 无方法维度，`cacheableMethod` 默认含 HEAD（`:233`）。HEAD MISS 缓存空 body + Content-Length，后续 GET 命中回放截断响应。修复：HEAD 只写 header 不入 body 缓存，或 key 区分方法。

## P1 —— 文档声明的具体 Go 侧 TODO（来自 plugins.md remaining-jobs）

- [ ] **grpc-transcode `deadline` 接受小数，产生 `1.5m` 无效值** — `pkg/plugin/grpc_transcode/plugin.go:68-71` schema `type: number`、`plugin.go:457-458` `fmt.Sprintf("%gm", ...)`。修复：限制/归一化为整数 wire 格式。
- [ ] **grpc-web 非 POST 返回 400，官方为 405** — `pkg/plugin/grpc_web/plugin.go:86-89`。与 pinned 3.17 源（`return exit(ctx, 405)`）矛盾。修复：对齐 405 并更新测试。
- [ ] **GM 插件 SSL marker 校验未接入生产加载路径** — `pkg/plugin/gm/plugin.go:60-71` `ValidateSSLConfig` 仅测试引用，`pkg/server` 的 SSL 加载路径未调用。修复：wire 进 SSL 加载。
- [ ] **ai-aws-content-moderation 缺 `check_request` / `deny_message` schema 字段** — `pkg/plugin/ai_aws_content_moderation/plugin.go:35-100` schema/config 均无这两个官方字段。
- [ ] **rocketmq-logger `use_tls` 仍硬拒** — `pkg/plugin/rocketmq_logger/plugin.go:213-216` PostInit hard-reject。仅当有 approved client transport 契约时实现。
- [ ] **ai-proxy-multi 无 per-IP DNS 健康状态** — `pkg/plugin/ai_proxy_multi/health.go:59-84` 按 instance index 而非 per-IP node 维护健康。需新 transport/health 契约。
- [ ] **kafka-proxy 无外部 broker smoke 测试** — `pkg/plugin/kafka_proxy/` 仅 loopback fixtures。需外部集成环境 + 凭据安全 CI 契约。

## P2 —— Route 性能后续（来自 todo_route-performance-followups-20260807.md）

- [ ] **同 suffix 候选碰撞仍 O(n)** — `pkg/route/builder.go:171` `embeddedSuffixes [3][2]map[string][]int` 按 host rank 而非 exact host 值索引；`matchEmbeddedRoute`（`:339-354`）对同 suffix 候选反向线性扫描，1000 条同 suffix 不同 exact host 仍 O(n)。修复：exact host/method 纳入候选索引，host 每请求只解析一次，保留 bucket slice 注册顺序真源与既有优先级语义。
- [ ] **补同 suffix + 多 exact host / 多 wildcard host / 多 method 语义测试** — 现有测试不同 suffix；`BenchmarkRouteDispatch` 无同 suffix 语料、无 host/method 维度。
- [ ] **补正式 suffix-collision benchmark（10/100/1000，first/last/mismatch/miss）** — 用 identical-corpus baseline/current + pinned benchstat 验收。
- [ ] **补完整 route reload benchmark** — `BenchmarkRouteBuildIndexes`（pkg/route/benchmark_test.go:177）只覆盖 100/1000 条、无 atomic swap、无插件组合、无 10k。目标覆盖 100/1000/10000、无插件/典型插件链/consumer+service+global 组合、CPU/alloc/wall profile。

## P3 —— 集成测试覆盖缺口（来自 TEST_CONVERSION_REVIEW）

- [ ] **limit-count 禁用插件场景未表示** — `t/plugin/limit-count.yaml:10185,10230` `limit-count-disable-plugin-test-16/17` 仍插件启用断言 200+503，无 `"plugins": {}` disable case（上游断言 4×200 无限流）。
- [ ] **traffic-split2-16 match 分发仍弱化** — `t/plugin/traffic-split.yaml` 2-16 仍单条无 match 规则，upstream_id 变体断言仍为 `^(first|second)$`。修复：与 2-12/13/17 一致补 `match: arg_id==1/==2`。
- [ ] **`security-warning.t` / `security-warning2.t` 38 个 TEST 块仍 pending** — 跨插件 TLS 警告族，需评估是否纳入 scope。

## P4 —— 审计残留（中等严重度）

- [ ] **exit_transformer 每响应新建 Lua VM + 全量 JSON Unmarshal** — `pkg/plugin/exit_transformer/plugin.go:137-143,306` Lua 源已预编译为共享 proto，但 `lua.NewState` 仍每响应新建、响应体全量 `json.Unmarshal`。修复：LState 池化/复用。
- [ ] **限流外层注册表只增不删** — `limit_req` `consumerBucketStores`（`plugin.go:219,485`）、`limit_count` `limitCountGroups.entries`（`:65-68,690`）、`graphql_limit_count` `graphqlLimitCountGroups.entries`（`:265-268,464`）永不淘汰（内部状态已用 `BoundedTTLMap` 约束，外层仍泄漏）。
- [ ] **authz_casdoor 弱随机回退仍在** — `pkg/plugin/authz_casdoor/plugin.go:323-329` `randomState()` 在 `rand.Read` 失败时回退 `time.Now()` 可预测值（saml_auth 已修，此处漏）。
- [ ] **openid_connect 每请求重解析 RSA 公钥** — `pkg/plugin/openid_connect/verify.go:186-198` `staticKeyVerifier` 每请求 PEM 解码 + x509 解析，无 verifier 缓存字段。
- [ ] **node_status 手写 uint→string + `^uint64(0)` 补码减一** — `pkg/plugin/node_status/plugin.go:92-105` `stringUint` 手写循环，`:67` `activeRequests.Add(^uint64(0))`。替换为 `strconv.FormatUint` / `Dec()`。
- [ ] **traffic_split `upstreamTimeout` 取 min 作为整体请求 deadline** — `pkg/plugin/traffic_split/plugin.go:671-686`（`:435-439` 应用）。connect=3s、read=60s 时 3s 后整体取消。修复：用 read（或 max）作为 deadline。
- [ ] **mqtt_proxy 预读 deadline 未清除** — `pkg/plugin/mqtt_proxy/stream.go:157` `readConnectFromStream` 设 5s read deadline 从不重置，后续 `io.Copy` 仍受过期 deadline 约束，空闲连接约 5s 断开。修复：预读后 `SetReadDeadline(time.Time{})`。
- [ ] **cors 所有 OPTIONS 短路为 200** — `pkg/plugin/cors/plugin.go:257-260` 任何 OPTIONS（含非 preflight）都被网关吞掉。修复：仅对带 Origin + Access-Control-Request-Method 的 preflight 短路（当前注释自述 mirror APISIX，需确认）。
- [ ] **proxy_rewrite URI 改写延迟到 before-proxy 生效** — `pkg/plugin/proxy_rewrite/plugin.go:203-213` 存 `ProxyRewriteKey`，`FinalizeProxyRewrite` 在 director 时（`pkg/route/builder.go:1491,2019-2031`）才应用，后续插件读到旧 `$uri`。

## P5 —— 依赖简化 / 造轮子（可选，低优先级）

> 来自 `external-dependency-review-20260808.md`（建议清单，未实施）与 `reinventing-the-wheel-review-20260806.md`。全部仍未实施，按阶段排序：

- [ ] **Phase 1（S，零风险）**：删 `samber/lo`、`spf13/cast`、`gofrs/uuid`、`go-nanoid`、`bpool`（ID 三件套合并为一个 `pkg/util` helper）。
- [ ] **Phase 2（M）**：`resty` → pkg/shared net/http helper；`alice` → 手写 Chain；`rs/cors` → 插件内 40 行；`httpsnoop` → base recorder（顺带统一 15 处 writer）。
- [ ] **Phase 3（M，先写测试）**：`ulule/limiter`、`smallnest/weighted`（先补选点分布序列测试）。
- [ ] **Phase 4（可选，需基准）**：`goccy/go-json` → `encoding/json`（benchstat 对比后再定）。
- [ ] **仍手写的协议层**（reinventing-the-wheel 未替换项）：jwe_decrypt compact JWE、hmac_auth signature、ai_stream SSE、tencent_cloud_cls TC3/protobuf、zipkin/skywalking B3/sw8、dubbo 帧层、kafka_proxy pubsub protobuf。request_id UUIDv7/KSUID 为有意保留（#53），不在替换范围。

## 已完成并验证（无需处理，仅记录）

- etcd watch 重连、bbolt Clone、otel init shutdown、LB 空节点 panic、splunk 换行、google_cloud_logging 超时、key_auth fail-closed、jwt_auth 缺失 claim、sls TLS 校验、error_log_logger kafka close、prometheus 优雅关闭、globalClients refcount、zipkin 超时、casdoor 会话过期、batch_requests 取消、proxy_cache Vary 变体、echo before/after 拼接、proxy-cache no_cache→MISS、acl 字段拼写、graphql 空文档消息、kafka-logger 反向断言、limit-conn key 日志、RS256 2048-bit。
- consistency-audit 台账 60 项抽验全部属实（cacheutil/luautil/dubbo/limitbase/log_batch/base 系列/ai_common 系列/pluginRegistry）。
- `docs/superpowers/plans/` 30 个计划全部执行并合入；仅 `security-warning*` 38 块 pending（见 P3）。
- client-control 非尺寸读错已修（`TestClientControlMaxBytesReadFailureReturns500`）。
