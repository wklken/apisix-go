# APISIX-Go 综合代码与安全审查报告

日期：2026-08-18
审查基线：`master@54f09952fe290014f72da519d2557a80a5b543f0`

## 1. 报告来源与合并规则

本报告合并以下两份只读审查结果：

1. Codex Security 标准扫描：17 个 finding，6 High、11 Medium。
2. OpenCode 深度审查：`docs/opencode-deep-review-2026-08-18.md`，10 个原始 finding。

OpenCode finding 已逐项回到当前 revision 的实现、调用链、配置、测试和反证进行复核。原始 ID、顺序和严重度保持不变；重复项只补充到既有 finding，不重复计数。

去重后共 23 个仓库问题：

- Codex Security 原有 17 个。
- OpenCode 新增 6 个。
- OpenCode 的 `SEC-001`、`BUG-003`、`SEC-003` 分别补强既有 OIDC audience、请求体上限和 AES-CBC finding。
- OpenCode 的 `BUG-001` 只命中了当前 checkout 的 ignored `vendor/` 陈旧状态；它不属于仓库或 CI finding，因此不计入上述总数。

上一轮 Codex Security 的 `scan-manifest.json`、`findings.json`、`coverage.json` 和生成的 `report.md` 是已经封存的同一组 canonical artifacts。本文件是面向人工审阅的综合报告；没有修改封存产物，避免破坏其摘要和 SARIF 一致性。

## 2. 综合结论

### 2.1 被排除的本地环境候选项

OpenCode 的 `BUG-001` 准确复现了当前 checkout 中默认 Go 命令因 `inconsistent vendoring` 失败的现象，但把它归因于 `master` 和 CI 是错误的：

- `.gitignore:23` 明确忽略 `vendor/`。
- `git ls-files vendor` 为空，`HEAD:vendor/modules.txt` 不存在，历史中也没有已跟踪 vendor 树。
- 因此干净 checkout 和 CI 不会从 Git 得到该目录，而是使用 module mode；失败只由当前工作区残留的陈旧 ignored `vendor/` 触发。
- `source .envrc && go mod vendor` 已修复本地依赖树；第二次生成摘要一致，且没有 `go.mod`、`go.sum` 或其他 tracked diff。

处置：拒绝把它作为仓库/CI 阻断项，也不应 force-add 79MB 的 ignored vendor 树。需要 vendor 的本地工作区可重新生成或删除该目录。

### 2.2 OpenCode 新增的高价值问题

| ID | 原级别 | 结论 | 问题 |
| --- | --- | --- | --- |
| SEC-002 | P1 | Correct | Loki/SLS/Splunk 默认 payload 外发敏感请求和响应头 |
| BUG-002 | P1 | Correct | memory proxy-cache zone 不执行 `memory_size` 容量限制 |
| BUG-004 | P1 | Correct | 永久非法 SSL/consumer 让 etcd watcher 无限 snapshot recovery |
| BUG-005 | P2 | Correct | 单个不可解码 route/global rule 阻断全部后续 HTTP 路由发布 |
| BUG-006 | P2 | Correct | HTTPS/grpcs map-form node 省略端口时错误固化为 80 |
| BUG-007 | P2 | Correct | `limit-count` delayed-sync 按高基数 key 永久保留状态 |

### 2.3 OpenCode 对既有 finding 的补强

| ID | 原级别 | 结论 | 合并结果 |
| --- | --- | --- | --- |
| SEC-001 | P1 | Correct | 既有 OIDC wrong-audience finding 扩展为：静态 key 路径在 discovery 失败/无 issuer 时还会跳过 issuer |
| BUG-003 | P1 | Correct | 与既有“默认请求体和读取时间无上限”合并，并补充完整读体插件清单 |
| SEC-003 | P2 | Correct | 与既有确定性、无认证 AES-CBC finding 合并，保留版本化 AEAD 迁移要求 |

## 3. OpenCode finding 验证 ledger

Scope：`github.com/wklken/apisix-go`，当前 `master@54f09952`；不是 PR/diff 审查，因此 `Introduced by current PR` 均为 `Not applicable`。

| Finding ID | 原始级别 | Verdict | Introduced by current PR | 建议评估 | Disposition |
| --- | --- | --- | --- | --- | --- |
| BUG-001 | P1 | Partially correct | Not applicable | replace | Reject：仅为 ignored 本地状态，不是仓库 finding |
| SEC-001 | P1 | Correct | Not applicable | adjust | Follow-up |
| SEC-002 | P1 | Correct | Not applicable | as-is | Follow-up |
| BUG-002 | P1 | Correct | Not applicable | adjust | Fixed in `fix(proxy-cache): enforce memory zone capacity` |
| BUG-003 | P1 | Correct | Not applicable | adjust | Fixed in `fix(config): bound request bodies by default` |
| BUG-004 | P1 | Correct | Not applicable | replace | Follow-up，需配置面语义设计 |
| BUG-005 | P2 | Correct | Not applicable | replace | Follow-up，需 fail-closed/last-good 决策 |
| BUG-006 | P2 | Correct | Not applicable | as-is | Follow-up |
| BUG-007 | P2 | Correct | Not applicable | adjust | Follow-up |
| SEC-003 | P2 | Correct | Not applicable | as-is，要求版本迁移 | Follow-up |

## 4. OpenCode 新增 finding 详情

### BUG-001：本地 ignored vendor 状态陈旧

- Normalized claim：当前 checkout 残留的 ignored vendor 树描述旧依赖图，使默认 Go vendor mode 失败；该状态不在 `HEAD` 中，也不会进入干净 CI checkout。
- Verdict：Partially correct。现象与本地影响正确，仓库和 CI 归因错误。
- Evidence：修复前可稳定复现 `inconsistent vendoring`；但 `.gitignore:23` 忽略 `vendor/`，`git ls-files vendor` 为空，`HEAD:vendor/modules.txt` 不存在。重新生成后默认聚焦测试和 `make build` 均通过，tracked diff 为空。
- Proposed fix assessment：replace。
- Best-fit solution：本地运行 `source .envrc && go mod vendor` 或删除残留 vendor 树；不要修改 `.gitignore`，也不要 force-add vendor。该项从仓库 finding 清单中排除。

### SEC-002：默认外部日志 payload 泄露敏感头

- Normalized claim：未配置自定义 `log_format` 时，Loki、SLS、Splunk 会把业务请求的 `Authorization`/`Cookie` 或响应的 `Set-Cookie` 等头放入外发 event。
- Verdict：Correct。
- Evidence：
  - `pkg/plugin/base/access_log.go:223-238` 已定义 `CollapseAccessLogHeaderValues` 和敏感头集合。
  - `pkg/plugin/loki_logger/plugin.go:451-463,559-571` 使用不脱敏的 `CollapseHeaderValues`。
  - `pkg/plugin/sls_logger/plugin.go:327-345` 同样使用不脱敏 helper。
  - `pkg/plugin/splunk_hec_logging/plugin.go:342-368,386-417` 直接保存/克隆完整 Header。
- Counterevidence：插件 endpoint 自身的认证头测试不等于业务 payload 已脱敏；现有测试没有覆盖默认 event 中的业务凭据。
- Proposed fix assessment：as-is。
- Best-fit solution：三类默认 builder 的 snapshot/legacy 路径统一使用 `CollapseAccessLogHeaderValues`，并对 request/response 敏感头分别加回归测试。

### BUG-002：memory cache zone 不执行 `memory_size`

- Normalized claim：memory zone 解析并 fingerprint `memory_size`，但 entry 写入没有总字节计数、容量门禁或淘汰，因此容量声明不生效。
- Verdict：Correct。
- Evidence：`pkg/plugin/proxy_cache/zones.go:28-35` 的 zone 只有 map；`:368-381` 仅把 `MemorySize` 加入 fingerprint；`:409-426` 只验证字符串；`pkg/plugin/proxy_cache/response_phase.go:287-313` 无条件写入 `entries`。GraphQL cache 复用同一实现。
- Counterevidence：TTL 只控制命中有效期，当前没有周期性 aggregate-capacity eviction；引用计数只在 plugin 生命周期结束时释放整个 zone。
- Proposed fix assessment：adjust。容量计算不能只看 body，需要把 header、vary index、覆盖旧 entry 和并发写入纳入同一锁内的 accounting。
- Best-fit solution：zone 保存解析后的 capacity/current bytes，并采用明确的 LRU/最旧淘汰合同；同时为 proxy-cache 和 graphql-proxy-cache 加共享 zone 容量测试。
- Remediation：已在共享 memory zone 锁内核算 key、body、header、entry metadata、Vary index 与 loaded metadata；写入后按 `storedAt` 淘汰最旧 entry，单条超限不会保留，覆盖写入会重新计算占用。proxy-cache 的 Vary entry 淘汰会同步清理索引签名，graphql-proxy-cache 通过相同 zone store 获得同一容量边界。
- Verification：修复前新增测试分别观察到最旧 entry 未淘汰、GraphQL 第三次请求错误 HIT、Vary 第一变体第三次请求错误 HIT；修复后 `go test ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache -count=1`、相同包的 `-race` 运行及 scoped `golangci-lint` 全部通过。

### BUG-004：永久非法资源阻断 etcd watcher 恢复

- Normalized claim：SSL/consumer acknowledged apply 失败会退出 watch；snapshot recovery 重新读取同一非法资源并再次失败，`knownKeys`/`lastRevision` 永远无法推进。
- Verdict：Correct。
- Evidence：`pkg/etcd/watcher.go:248-276` 在 apply 失败后进入循环 recovery；`:294-332` 仅在全部 snapshot event 成功后提交 revision；`pkg/store/store.go:396-413` 会在写入前拒绝非法 SSL/consumer。
- Counterevidence：现有恢复测试的下一份 snapshot 会移除坏资源，因此没有覆盖“坏资源永久存在”。
- Proposed fix assessment：replace。不能简单忽略所有验证错误并宣告 snapshot 成功，否则会弱化 fail-closed。
- Best-fit solution：定义 per-resource last-good/quarantine：按资源 ID 和 mod revision 记录拒绝状态，保留该资源上一份有效状态，同时允许无关资源推进；readiness、指标和错误日志必须暴露被隔离资源。控制面入口也应阻止非法资源写入。

### BUG-005：单个畸形 route/global rule 冻结全部 HTTP 发布

- Normalized claim：只要 store 中保留一个不可解码 route/global rule，任何后续 reload 都会在构建全量 snapshot 时失败并继续使用旧 handler。
- Verdict：Correct。
- Evidence：`pkg/store/getter.go:684-718` 在首个 decode error 直接返回；`pkg/server/reload.go:145-177` 的 `BuildStrict` 失败保留旧 handler；`TestStoreListersFailOnUndecodableEntries` 明确编码了当前严格行为。
- Counterevidence：这是已有 fail-closed 合同，不是无意的局部异常。因此 OpenCode 建议的“直接 skip 并发布有效子集”不能原样采用。
- Proposed fix assessment：replace。
- Best-fit solution：优先在写入/同步边界验证并拒绝坏对象；若数据面必须容忍外部 etcd 污染，应设计 per-resource last-good/quarantine，而不是静默跳过或发布未经明确批准的部分配置。

### BUG-006：HTTPS/grpcs map-form node 缺省端口错误

- Normalized claim：map shorthand 的裸 hostname 在解析阶段被赋值为 80，导致 scheme-aware builder 无法为 HTTPS/grpcs 应用 443。
- Verdict：Correct。
- Evidence：`pkg/resource/route.go:75-125,158-175` 在读取 scheme 前把 map node 缺省端口固化为 80；`pkg/route/builder.go:2822-2864` 只有 `Port == 0` 才按目标 scheme 补 443/80。list-form 省略 port 保留零值，因此两种合法表达不一致。
- Proposed fix assessment：as-is。
- Best-fit solution：map parser 对未提供端口返回 0，把默认端口决策集中到 builder；为 HTTP/HTTPS/gRPC/grpcs、map/list、IPv4/IPv6 增加表驱动测试。

### BUG-007：delayed-sync key 状态无回收

- Normalized claim：每个新限流 key 都永久进入 `states`；flush 只清 delta/in-flight，不删除过期且空闲的 key，攻击者可用高基数 key 线性增长内存。
- Verdict：Correct。
- Evidence：`pkg/plugin/limit_count/delayed_sync.go:35-49,81-139` 创建并保存状态；`:208-270` 只同步 delta；生产代码没有删除 `states` 的路径。队列容量不约束 `states`、`retry` 或 `retryNext` 的长期基数。
- Proposed fix assessment：adjust。只应删除已经过窗口、无 local delta、无 in-flight 且不在 retry 集合的状态，并需要整体容量上限防止窗口内攻击。
- Best-fit solution：增加安全清理和显式容量/eviction 指标；测试大量唯一 key 跨窗口后状态数回落，以及 in-flight/retry 状态不会被提前删除。

## 5. 对既有 finding 的补充

### SEC-001 → Codex Security OIDC finding

既有报告已确认 local JWT verification 默认不绑定 audience。OpenCode 进一步确认静态公钥 verifier 同时设置 `SkipIssuerCheck: true`；后置 `validateIssuer` 在没有显式 `valid_issuers` 且 discovery 失败或 issuer 为空时直接返回，`tokenActive` 又把缺失 `active` 当作 true。

合并后的修复要求：本地 JWT 验证必须同时拥有可验证的 issuer 和 expected audience/resource。expected audience 不一定等于 OAuth `client_id`，应允许显式资源 audience，但缺失约束必须失败关闭。

### BUG-003 → Codex Security request-body finding

既有报告的 gateway-wide zero limit 已经覆盖 OpenCode 主结论。OpenCode 补充了受影响的直接完整读体调用方，包括 `body_transformer`、`function_upstream`、`degraphql`、`chaitin_waf`、`request_validation` 和多类 AI 插件。

修复不能只改 production profile：普通/default profile 也应有正的全局上限；直接 `io.ReadAll` 调用方仍应使用 bounded helper，形成纵深防护。

Remediation：配置解码前为 `client_max_body_size` 和 `client_body_timeout` 安装 10 MiB/60 秒默认值，并在所有 profile 中拒绝显式非正值。`function-upstream` 改用共享 request-body helper，保留 `*http.MaxBytesError` 错误链并关闭已读取 body。修复前测试观察到省略配置解码为零、显式零被接受、body timeout 非正值被接受以及失败读体未关闭；修复后 config/server/function-upstream 全包测试、scoped lint 和 build 通过。

### SEC-003 → Codex Security AES-CBC finding

两份报告结论一致：key 被重复用作 CBC IV、密文确定性、没有认证 tag。修复必须引入版本化 AEAD envelope、随机 nonce 和字段上下文认证；legacy CBC 只能作为只读迁移入口，不能无版本原地替换。

## 6. Codex Security 原有 17 项

以下 finding 保持原判定；详细 source-to-sink、CWE 和 remediation 见封存的 Codex Security 报告。

| # | Severity | Finding |
| ---: | --- | --- |
| 1 | High | proxy-cache can share Authorization-dependent responses across clients |
| 2 | High | Invalid credentials can fan out into serial uncached Vault requests |
| 3 | High | openid-connect accepts locally verified JWTs for the wrong audience；已由 OpenCode SEC-001 补强 issuer 边界 |
| 4 | High | HTTPS and grpcs upstream certificate verification is disabled by default |
| 5 | High | Batch subrequests can forge forwarding headers and bypass IP allowlists |
| 6 | High | Compatibility listeners permit unbounded request bodies and read time；已与 OpenCode BUG-003 合并 |
| 7 | Medium | Several external backend responses are read without a size limit |
| 8 | Medium | Vault tokens can traverse plaintext HTTP or leak across redirects |
| 9 | Medium | jwt-auth accepts signed tokens without expiration by default |
| 10 | Medium | AI provider requests forward ingress credentials and hop-by-hop headers |
| 11 | Medium | graphql-proxy-cache exposes an unauthenticated global PURGE endpoint |
| 12 | Medium | openid-connect post-login redirect accepts protocol-relative paths |
| 13 | Medium | response-rewrite permits decompression bombs after the compressed-body cap |
| 14 | Medium | Outbound document and logging clients disable TLS verification by default |
| 15 | Medium | Client-controlled gRPC Content-Type bypasses POST idempotency requirements |
| 16 | Medium | Data encryption uses deterministic unauthenticated AES-CBC；已与 OpenCode SEC-003 合并 |
| 17 | Medium | Gateway-consumed authentication credentials are forwarded upstream by default |

## 7. 验证记录与边界

### 默认模式与本地 vendor 修复

在重新生成 ignored vendor 树后执行：

```text
source .envrc && go test \
  ./pkg/plugin/openid_connect \
  ./pkg/plugin/proxy_cache \
  ./pkg/etcd ./pkg/store ./pkg/resource \
  -count=1
source .envrc && go mod verify
source .envrc && make build && make clean
```

结果：5 个 package 全部通过，`go mod verify` 返回 `all modules verified`，构建和清理通过。第二次 `go mod vendor` 前后的完整 vendor 文件摘要均为 `677eb3029e2028255624aa045db7980f40a461b8fe4320e39fd07219e978bd91`；`go.mod`、`go.sum` 和 tracked diff 保持为空。

### 诊断性 module 模式

执行：

```text
source .envrc && GOFLAGS=-mod=mod go test \
  ./pkg/plugin/openid_connect \
  ./pkg/plugin/loki_logger \
  ./pkg/plugin/sls_logger \
  ./pkg/plugin/splunk_hec_logging \
  ./pkg/plugin/proxy_cache \
  ./pkg/plugin/limit_count \
  ./pkg/etcd ./pkg/store ./pkg/resource ./pkg/route ./pkg/data_encryption \
  -count=1
```

结果：11 个 package 全部通过。现有测试通过不反驳 findings，因为缺少的正是 discovery failure、默认 payload 敏感头、memory capacity、永久坏 snapshot、scheme + map shorthand、高基数状态回收和 AEAD/tamper 等组合。

### 未执行

- 未修改产品代码或测试。
- 未执行动态攻击 PoC、etcd/Vault/OIDC 实环境、race 或负载测试。
- 聚焦测试不代表已覆盖依赖升级的全部语义；它只证明本地 vendor 修复后的直接相关 package 与构建基线。

## 8. 建议处理顺序

1. SEC-001、SEC-002、Codex Security 的 proxy-cache Authorization 隔离及 batch trusted-header 绕过。
2. BUG-002、BUG-003、BUG-004：优先关闭远程资源耗尽和配置面永久冻结。
3. BUG-005：先确定 per-resource last-good/quarantine 与 fail-closed 合同，再实现。
4. BUG-006、BUG-007：分别以小型、独立 PR 修复。
5. SEC-003：先完成带版本的 AEAD 迁移设计，再实现和回填。

上述 P1/High finding 未关闭前，当前 revision 不应被描述为生产就绪；本地 ignored vendor 状态不构成该判断的依据。
