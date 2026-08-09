# Outstanding Backlog（唯一待办清单）

> Status: 2026-08-09（代码库复核后收敛）。本文档是 docs 目录 PR 过程文档的唯一待办落点。
>
> 常设说明文档 `configuration.md`、`design.md`、`plugins.md` 保留不动；`superpowers/plans/2026-08-08-proxy-performance-acceptance.md` 因另一个 agent 正在使用，暂不合并，也不作为本文待办来源。
>
> 本次已将其余 PR 过程文档中仍未落地的事项归并到本文；已在代码中确认落地、或仅保留执行步骤而不再提供额外待办信息的文档已回收删除。

## 确认要做

### P0 — 请求/响应完整性

- [ ] **chaitin-waf 只把截断后的请求体恢复给上游** — `pkg/plugin/chaitin_waf/plugin.go` 的 `askWAF` 通过 `io.LimitReader` 读取后用截断副本替换 `r.Body`。应完整读取并恢复原 body，只把 `req_body_size` 范围内的副本发给 WAF。
- [ ] **request-validation 规范化 JSON 时丢失大整数精度** — `parseJSON` 解码到 `float64` 后重新序列化；应使用 `json.Decoder.UseNumber()`，继续保留“校验对象与上游对象一致”的规范化安全语义。
- [ ] **proxy-cache 的 HEAD MISS 会写入与 GET 共用的空 body 缓存项** — 默认 cache method 同时包含 GET/HEAD，而 cache key 不区分方法。保留 HEAD 命中既有 GET 缓存的能力，但 HEAD MISS 不得创建/覆盖共享缓存项。
- [ ] **response-rewrite 遇到不支持或无法解码的编码时破坏 HTTP 表示元数据** — 当前会保留原压缩 body，却删除 `Content-Encoding`，导致下游按明文解释压缩内容。应跳过 body filter，但保留原始 body、`Content-Encoding` 与 `Content-Length`。
- [ ] **brotli 在判断是否超 `max_response_size` 前先完整缓冲响应，且透传时清掉原始 `Content-Length`** — 应在缓冲超过上限后单向切换为透传，并在未压缩场景保留上游长度元数据。
- [ ] **请求体读取失败后被缓存为“成功读取的部分 body”** — `pkg/apisix/ctx/context.go` 第二次读取会拿到截断 body 且 `err == nil`。应把 bytes 和 read error 一起缓存，重复读取返回同样的错误语义。
- [ ] **data-mask 把空 body 当成非法 JSON，且本地 reader 不关闭原 body** — 空 body 应 no-op 放行；读取路径应复用已有安全 reader/replace 语义。
- [ ] **request-validation 在配置 `body_schema` 且请求 body 为空时先报 JSON 解析错误** — 应先让 schema 决定“空值是否合法”，而不是被通用 JSON 解析提前短路。
- [ ] **batch-requests 对非法 path 可能 panic，timeout 后仍会阻塞等待 worker** — 应改为 error-returning 请求构造，并在超时路径避免无界等待。

### P1 — 身份、认证与请求来源

- [ ] **request-id 的 `range_id` 返回别名池缓冲的字符串** — `pkg/plugin/request_id/plugin.go` 目前返回 `util.BytesToString(id)`，在 `Put` 回池后仍别名该切片；应在返回边界复制拥有权。
- [ ] **basic-auth 错误消息与 multi-auth 诊断会泄露原始或解码后的凭据** — 应把 malformed Basic 认证错误收敛为常量错误文案，不包含攻击者提供的 token 或 `user:pass`。
- [ ] **DingTalk / Feishu / Casdoor 手工登录会话 cookie 缺少安全默认值** — 默认应 `Secure=true`、`HttpOnly=true`、`SameSite=Lax`，并显式暴露受约束的配置项；`SameSite=None` 必须要求 `Secure=true`。
- [ ] **authz-casdoor 随机源失败时退回可预测时间字符串** — session ID 与 OAuth state 都必须在 `crypto/rand` 失败时 fail closed，不得生成弱随机值。
- [ ] **real-ip 只写 context，不让 `$remote_addr` 跟随可信 IP** — 现状会导致 limit-count/req/conn 默认 key 仍按代理 IP 计数；`$remote_addr` 应优先读取 `ctx.RemoteAddrKey`。
- [ ] **`RegisterRequestVar` 对 nil map 直接写入** — 新调用路径可 panic；应与 `RegisterApisixVar` 一样 nil-safe。
- [ ] **openid-connect 每次 bearer 校验都重新解析静态公钥** — 在 `PostInit` 解析已完成 secret resolution 的 `public_key`，请求路径只按 token algorithm 构造 verifier。

### P2 — 路由、代理与生命周期

- [ ] **mqtt-proxy CONNECT 建链后仍遗留绝对 deadline** — 当前只清了 client preread read deadline，upstream replay write deadline 仍会把活跃会话在约 5 秒后打断；两侧临时 deadline 都应在 phase 成功后清零。
- [ ] **traffic-split 丢弃了重新赋值的 request 指针** — `WithHealthReporter` 返回的 clone 没有被拷回调用方，导致 `Host` 重写和被动健康追踪上下文丢失；应在原请求指针上保留 enriched state。
- [ ] **ai-proxy-multi 的健康检查 goroutine 生命周期发布存在竞态** — `Stop()` 可能在字段尚未初始化时返回，导致 probe goroutine 泄漏；应在 `PostInit` 完成生命周期对象初始化。
- [ ] **route priority 已解码但未参与匹配** — 当前匹配结果取决于注册顺序；应保证更高 `resource.Route.Priority` 的路由获胜，等优先级保持既有稳定语义。
- [ ] **重复 URI 参数名仍可通过 `convertURI` 并在注册阶段触发 panic** — 例如 `/user/:id/:id`；应在 URI 转换阶段 fail closed。
- [ ] **standalone 配置对含 `/` 的字符串 ID 缺少验证，文件 watcher 也缺少显式 Stop 生命周期** — 应收紧 ID 语法，并让阻塞 send / Watch 在停止时可退出。
- [ ] **zipkin / lago 的响应 writer wrapper 不暴露 `Unwrap`** — `http.ResponseController` 无法透传 flush/hijack 等能力，影响流式与升级响应。

### P3 — 限流、AI、日志与缓存可靠性

- [ ] **本地 fixed-window limit-count 实际表现为滑动过期窗口** — 每次请求都会刷新 `resetAt` 与 TTL；应锚定到窗口首次命中时间。
- [ ] **delayed-sync flush 与 inline expiry flush 可能重复提交同一 delta** — 需要对 in-flight delta 做预留和 completion 同步，避免后端重复计数。
- [ ] **ai-proxy / ai-proxy-multi 用 `http.Client.Timeout` 截断正常长流** — 现状是总时长超时，不是 inactivity timeout；应改成 connect/header/send/read progress timeout，并保留显式总时长控制项。
- [ ] **ai-request-rewrite 对 Anthropic 仍生成 OpenAI Chat 请求并访问 `/v1/chat/completions`** — provider 为 `anthropic` 时应切到 Messages 协议与 `/v1/messages`。
- [ ] **ai-rag provider 调用没有请求总超时** — 普通非流式上游请求可无限等待；应提供毫秒级配置并给客户端加总超时。
- [ ] **Elasticsearch bulk API 只看 HTTP 200，不解析 item 级失败** — 应识别 `errors:true` 与每条 item 的失败状态，返回首个失败项位置。
- [ ] **error-log-logger / sls-logger 缺少写超时和有界 pending queue** — blocked write 会卡住 sender/Stop；应补写 deadline，并为 `max_pending_entries` 设明确默认值和配置面。
- [ ] **日志字段解析对运行时动态注册变量缺少 fallback** — `$balancer_ip`、`$upstream_addr` 等已写入 context 的变量可能因静态白名单漏项而输出空串；应在静态解析失败后回退到 live var map 精确取值。
- [ ] **syslog 在部分写失败后保留后缀，后续可能把半帧和新帧拼接** — 可接受 at-least-once 重复，但不能拼接孤儿 suffix；部分写失败后应丢弃当前批次缓冲。
- [ ] **proxy-cache 并发 MISS 没有 single-flight/coalescing** — 相同 key 的并发 MISS 会同时打上游并竞争写回；应增加每 key 的引用计数锁，并在持锁后二次查 cache。

### P4 — APISIX 3.17 兼容与性能待办

- [ ] **grpc-transcode fractional `deadline` 生成无效 `grpc-timeout`** — schema 接受小数，当前直接拼成 `1.5m`；将 schema 收紧为非负整数毫秒，并覆盖小数拒绝与整数 wire 值。上游 3.17 也存在这个 schema/wire 缺陷，本项是 wire correctness hardening，不宣称上游差异。
- [ ] **grpc-web 非 POST 返回 400，而 3.17 返回 405** — 改为 405 并更新单元/route parity 断言。
- [ ] **proxy-rewrite 到 before-proxy 才修改 `r.URL` / method** — 3.17 在 rewrite phase 调用 `ngx.req.set_uri` / `ngx.req.set_method`，后续插件能观察新值；Go 侧应在 proxy-rewrite handler 内完成 URI/method finalize，同时保留 host/scheme 的上游目标阶段处理。
- [ ] **limit-count TEST 16/17 没有表示“更新同一路由并禁用插件”** — `t/plugin/limit-count.yaml` 当前把 TEST 16 配成仍启用插件，TEST 17 又重新执行限流；应使用一个场景先建立限流状态，再以空 `plugins` 更新同一路由，并断言后续 4 次请求均为 200。
- [ ] **traffic-split2 TEST 16 丢失两个 `match` 规则** — pinned 3.17 TEST 16 明确以 `arg_id == 1/2` 绑定两个 `upstream_id`；当前 manifest 将其弱化为无条件权重轮询。补回规则，TEST 17 继续分别断言 route/first/second。
- [ ] **同 suffix + 多 exact host 候选仍反向线性扫描** — 先补 exact/wildcard host 与 method 的语义测试，以及 10/100/1000 的 collision benchmark；在同一 benchmark corpus 上记录 baseline 后，为 exact host 建立值级索引。只有 1000-route exact-host `match-first`/`host-mismatch` 达到预声明的实用阈值才接受优化，wildcard host 只要求语义不回归。

## 暂不纳入当前清理队列

- **`superpowers/plans/2026-08-08-proxy-performance-acceptance.md`**：另一个 agent 正在使用，暂不删除、不改写；待并发工作结束后再决定是否并回本文。
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

## 不做

- **authz-casbin 改绑 `$consumer_name`**：驳回。3.17 schema 的 `username` 就是 header 名，官方实现直接读取该 header；强制 consumer 变量会破坏独立 authz-casbin 用法。
- **ai-aws-content-moderation 增加 `check_request` / `deny_message`**：驳回。两个字段不存在于 APISIX 3.17 schema；`docs/plugins.md` 的旧声明是错误镜像。
- **node-status 改为 `Dec()`**：驳回。`atomic.Uint64` 没有 `Dec` API，`Add(^uint64(0))` 是无锁减一；手写 `stringUint` 也没有已证明的正确性或性能问题。
- **CORS 只拦真正 preflight**：驳回。3.17 rewrite 明确对所有 OPTIONS 返回 200，当前代码和测试与之相符。
- **依赖删除/协议整体替换类提案**：从缺陷 backlog 移除。以后只能以具体依赖升级、漏洞、二进制体积、互操作失败或 profile 证据单独立项。

## 已回收入代码，不再单独保留文档

- 启动期 strict route build / reload last-known-good、replay-safe retry、请求 replay 容量上限、bounded transport、cluster registry / overload admission / active health、loopback benchmark 环境缓存等 proxy/runtime 基础能力，已在代码中落地，不再保留独立过程文档。
- `traffic-split` 的错误整体 request deadline 近似已删除；当前测试已固定“phase timeout 不折叠为整体 deadline”的边界。
- `limit_req`、`limit_count`、`graphql_limit_count` 的外层共享注册表已具备 `refs` 引用计数和 `Stop()` 释放路径，不再列为待办。
- `master` 已合入的历史计划与已验证问题不再重复列为待办；本次回收的过程文档只保留了它们尚未落地的高层事项。

## 执行计划

1. **Phase 1: 请求/响应完整性**
先处理 P0 全部事项：`chaitin-waf` body 恢复、`request-validation` 大整数与空 body 语义、`proxy-cache` HEAD MISS、`response-rewrite` 编码元数据、`brotli` 上限透传、请求体读取错误缓存、`data-mask` 空 body、`batch-requests` panic/timeout。每项先补回归测试，再跑所属 package；阶段收口执行 `source .envrc && make build`。

2. **Phase 2: 身份与认证安全**
处理 `request-id` 池化别名、`basic-auth` 凭据泄露、DingTalk/Feishu/Casdoor cookie 安全默认值、`authz-casdoor` 弱随机 fallback、`$remote_addr` trusted real-ip、`RegisterRequestVar` nil-safe、`openid-connect` 静态公钥预解析。阶段验证以 `source .envrc && go test -race ./pkg/plugin/request_id ./pkg/plugin/basic_auth ./pkg/plugin/dingtalk_auth ./pkg/plugin/feishu_auth ./pkg/plugin/authz_casdoor ./pkg/plugin/real_ip ./pkg/apisix/ctx ./pkg/apisix/variable -count=1` 为主，再跑 `source .envrc && make build`。

3. **Phase 3: 路由、代理与生命周期**
处理 MQTT upstream write deadline 清理、traffic-split request state 保留、ai-proxy-multi health lifecycle、route priority、重复 URI 参数、standalone watcher Stop 与 ID 校验、zipkin/lago `Unwrap`。阶段验证以 `source .envrc && go test -race ./pkg/plugin/mqtt_proxy ./pkg/plugin/ai_proxy_multi ./pkg/route ./pkg/config ./pkg/plugin/zipkin ./pkg/plugin/lago -count=1` 为主，再跑 `source .envrc && make build`。

4. **Phase 4: 限流、AI、日志与缓存可靠性**
处理 fixed-window 锚定、delayed-sync 去重、AI streaming progress timeout、Anthropic Messages 协议、ai-rag 总超时、Elasticsearch bulk item failure、error-log/sls 写超时与 pending cap、动态日志变量 fallback、syslog partial write、proxy-cache single-flight。阶段验证分两组执行：`source .envrc && go test -race ./pkg/plugin/limit_count -count=1`，以及 `source .envrc && go test ./pkg/plugin/ai_proxy ./pkg/plugin/ai_proxy_multi ./pkg/plugin/ai_request_rewrite ./pkg/plugin/ai_rag ./pkg/plugin/elasticsearch_logger ./pkg/plugin/error_log_logger ./pkg/plugin/sls_logger ./pkg/plugin/syslog ./pkg/plugin/proxy_cache ./pkg/apisix/log -count=1`，然后 `source .envrc && make build`。

5. **Phase 5: 3.17 parity 与 route suffix 性能**
最后处理 `grpc-transcode`、`grpc-web`、`proxy-rewrite`、`limit-count` manifest、`traffic-split2` manifest，以及 suffix collision 语义和 benchmark。这个阶段保持 impact-scoped：unit/package tests + 精确 `t/plugin` 用例 + suffix benchmark smoke；不运行全量 `go test ./...` 或全量 `t/plugin`。

6. **暂缓项**
`superpowers/plans/2026-08-08-proxy-performance-acceptance.md` 继续保持冻结，等另一个 agent 结束后再决定是否并回本计划；在此之前不与当前五个 phase 交叉改写同一 benchmark 语料。
