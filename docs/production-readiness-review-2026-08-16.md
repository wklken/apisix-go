# apisix-go Production Readiness 深度审查报告

日期：2026-08-16

审查基线：`0b47cca4b39573f03793616e8ef3d5ee52474d07`（`master`）

审查类型：6 个只读 subagent 并行审查 + 主会话源码交叉复核。对照 2026-08-15 ledger 与随后合入的 #112–#120、可观测性上限、发布门禁。

结论：**当前仓库仍不是 production ready。** 8 月 15 日剩下的实现型 P0（001–009）在代码里已关闭。仓库现在是可运行的 **`http-data-plane-v1` 候选 HTTP 数据面**，还不能替换 Apache APISIX，也不能摘掉 README / CLI 的 “not production ready” 声明。

剩下的阻断：

1. **代码 P0（001–003）**：**CLOSED**。路由 `vars` / `remote_addrs` 在 `validateRouteSemantics` 编译期 fail-closed；显式 `status: 0` 由 `Disabled()` 从 HTTP 路由表跳过。
2. **发布资格 P0（004）**：**OPEN**。没有针对本 HEAD 的已签名 digest、RC/final 证据和可回滚旧 digest。

## 1. 执行摘要

#115–#120 把 8 月 15 日那批实现缺口收掉了：`script`/`filter_func` fail-closed、`/livez`/`/readyz`、生产镜像入口、默认日志头脱敏、logger TLS 校验、缺 weight 拒绝、`ai` 占位失败、认证 401 带 CORS、HTTP `chash` 拒绝、全局 413、密钥物化、认证默认值、精确六插件 profile。

`http-data-plane-v1` 的 allowlist 与运行时一致。它仍是候选 profile，不是已合格发布。兼容模式（空 profile + `config-default.yaml`）比候选 profile 宽，不能当生产入口。

## 2. 审查范围与方法

| 领域 | 状态 | 产出 |
| --- | --- | --- |
| 8 月 15 日 P0 001–010 + PR-015/016 | 完成 | [P0 ledger](e669411e-85fe-481e-a394-e715ec4de2cc)：9 CLOSED / 3 PARTIAL / 0 OPEN |
| 8 月 15 日 P1 残留 + #116–#120 | 完成 | [P1 leftovers](b774b925-3fa0-4ce4-838e-17cc194a1e74) |
| 安全残留 | 完成 | [Security](bb976cde-403e-4b6a-8efb-9fb9441ad9cb)：无 P0 认证绕过 |
| 运维 / 发布 / HA | 完成 | [Ops](a23c2722-86ae-4d74-9b4f-adfd5c3fcac3)：代码/profile 就绪，证据未就绪 |
| 正确性 / 静默失效 | 完成 | [Correctness](b7801cb1-746c-4e71-8d17-785c4a361449) |
| 插件/profile 诚实性 | 完成 | [Parity](2dd4ed4e-e8ff-4862-aada-37078782e5b1) |

分级：

- **P0（代码）**：**CLOSED**。解析后忽略的路由 ACL/停用字段已改为编译拒绝或跳过，不再静默当普通反向代理。
- **P0（发布）**：**OPEN**。没有可验证的签名镜像与 RC/final 证据，不能宣称已合格。
- **P1**：兼容模式泄漏、APISIX 同款 footgun、文档漂移。
- **延期**：stream / ext-plugin / inspect / WASM / Admin / 通用 Secret。profile 禁用即可。

## 3. 8 月 15 日账本（→ 当前 HEAD）

### 原剩余 P0 001–010

| ID | 状态 |
| --- | --- |
| 001 `script` / `filter_func` | **CLOSED** |
| 002 `/livez` `/readyz` | **CLOSED** |
| 003 默认镜像/配置 | **CLOSED**（镜像走 `config-production.yaml`；CLI 默认仍是兼容文件） |
| 004 默认 access-log 敏感头 | **CLOSED**（denylist，不是白名单） |
| 005 tcp-logger / syslog TLS | **CLOSED**（默认校验，显式 `ssl_verify: false` 可关） |
| 006 缺 weight → 0 | **CLOSED** |
| 007 `ai` no-op | **CLOSED**（不在 allowlist；配置则 fail-closed） |
| 008 认证 401 缺 CORS | **CLOSED** |
| 009 HTTP `type: chash` | **CLOSED**（拒绝，未实现 ketama） |
| 010 `sls-logger` SD 含密钥 | **PARTIAL** |

### 原 PARTIAL

- **PR-015 stream**：**PARTIAL**。启动 fail-closed；无官方 stream ACL / limit-conn / syslog / traffic-split / mTLS。`http-data-plane-v1` 强制 HTTP-only。
- **PR-016 前端 TLS**：**PARTIAL**。全局协议/密码/tickets/client CA 已应用。SSL 资源仍无 per-SNI protocol 或 client-mTLS。

010 / PR-015 / PR-016 被生产 profile 排除，不是实现关闭。兼容 allowlist 仍含 `sls-logger`。

### 原 P1 运行时残留

[P1 leftovers](b774b925-3fa0-4ce4-838e-17cc194a1e74) 在 `http-data-plane-v1` 上关闭：全局 413、强制密钥物化、wolf-rbac/OIDC/basic-auth/ldap hide、discovery/`enable_websocket`/Admin/QUIC/WASM fail-closed、SIGHUP 诚实、consumer 头剥离、代理错误不打 query、process access log 拒绝、HTTP 指标基数预算、es/clickhouse/cls 强制 `log_format`、OTel 不再把 `inactive_timeout` 映射成 exporter timeout。

仍 OPEN/延期：stream 指标、SkyWalking 单 span、进程内 limit/session/cache（profile 不允许这些插件）。

5.6 签名/provenance：**机制已有，证据没有。**

## 4. P0：进入生产前必须处理

### PR-2026-08-16-001：路由 `vars` 被解析后忽略 — **CLOSED**

**来源**：[Correctness](b7801cb1-746c-4e71-8d17-785c4a361449)，主会话复核。

`resource.Route.Vars` 现为 raw JSON。`pkg/route.validateRouteSemantics` 对非空路由 `vars` fail-closed（`null` / `[]` 仍接受）。插件级 `vars`（traffic-split 等）不受影响。`BuildStrict` 失败时保留 last-good mux。

**验收**：compile 对非空路由 `vars` fail closed；standalone/etcd 加载失败并保留 last-good。负向测试在 `pkg/route/unsupported_semantics_test.go`。

### PR-2026-08-16-002：`remote_addrs` 被解析后忽略 — **CLOSED**

`validateRouteSemantics` 拒绝任何非空白 `remote_addrs` 项。不实现 CIDR ACL。

**验收**：非空 `remote_addrs` fail closed；负向测试与 `vars` 同表。

### PR-2026-08-16-003：`status: 0` 不停用路由 — **CLOSED**

SSL `status == 0` 仍由 `pkg/store/getter.go` 跳过。路由用 `StatusConfigured()` / `Disabled()` 区分缺省与显式 0。`BuildStrict` 对 `Disabled()` 跳过注册；非法显式 status（非 0/1）由 `validateRouteSemantics` 拒绝。

**验收**：显式 `status: 0` 不进入路由表；缺省与 `status: 1` 仍启用。回归在 `pkg/route/route_status_test.go`。

### PR-2026-08-16-004：本 HEAD 没有合格发布证据 — **OPEN**

**来源**：[Ops](a23c2722-86ae-4d74-9b4f-adfd5c3fcac3)、[P1 leftovers](b774b925-3fa0-4ce4-838e-17cc194a1e74)。

工作流已有 keyless cosign、`attest-build-provenance`、RC operational gates、etcd recovery harness。本提交没有 `v*` tag、没有 GHCR 已签名 digest、没有保留的 SBOM/Trivy/soak/recovery 证据包。首次发布没有旧 digest，回滚无法验收。`production-release` 环境仍需放行 `v*` tag（保留 reviewer / wait timer）。

**验收**：按 `docs/runbooks/production-release.md` 跑通一次 RC 和一次独立 final；保留可验证签名、attestation、soak JSON、etcd 恢复记录；文档化回滚 bootstrap。在此之前不要摘警告横幅。

## 5. 已关闭、不应再当实现 P0 的项

8 月 15 日 001–009，以及 #116/#117/#119/#120 关闭的 body 上限、密钥物化、认证默认值、profile 门禁。详见第 3 节。

`consumer-restriction.allowed_by_methods` 未列出用户放行仍是 APISIX 同款行为，不是 Go 绕过。

`sls-logger` 把 `access-key-secret` 放进 RFC5424 SD 与 APISIX 3.17 协议一致。不要只在 Go 里删掉该字段。生产 profile 已排除该插件；兼容 allowlist 仍包含它。

jwt-auth 无 `exp` 不过期、上游 HTTPS 默认不校验证书、`hide_credentials` 默认 false：官方默认，且 jwt/key/basic 在六插件 allowlist 里。不当作 Go P0；生产操作手册必须写明要显式打开。

## 6. P1：production candidate 前处理

### 6.1 兼容模式（空 profile）

- 默认 access-log denylist 不含 key-auth 默认头 `apikey`，也不打码 query token。[Security](bb976cde-403e-4b6a-8efb-9fb9441ad9cb) P1-1。profile 六插件无 logger，故候选 profile 不触发。
- `gm` 仍是 pass-through，`PostInit` 成功。[Parity](2dd4ed4e-e8ff-4862-aada-37078782e5b1)。profile 进不去。
- CLI 默认 `-c conf/config-default.yaml`：`debug: true`、loopback etcd、完整 allowlist。镜像 CMD 已改生产文件。
- 镜像仍 COPY 兼容配置；覆盖 `CMD`/`-c` 会离开生产 profile。

### 6.2 候选 profile 上的官方 footgun

- jwt-auth：未配 `claims_to_verify` 时缺 `exp` 的 token 不过期。
- 上游 `https`/`grpcs`：`tls.verify` 未设则 skip verify。
- 三个 allowlist 认证插件 `hide_credentials` 默认 false，凭据会到上游。
- basic-auth 成功时 Info 打 consumer 名。

### 6.3 运维

- Docker HEALTHCHECK 打 `/readyz`；编排应 `/livez` 存活、`/readyz` 就绪。etcd 抖动会把容器标 unhealthy。
- Soak 测的是 commit 上的 `go test ./pkg/route`，不是已发布容器。
- RC `publish-image: false`，没有签名 RC digest。
- `SIGHUP` 是优雅退出 + 非零，不是 reload。
- `docs/plugins.md` gzip remaining-jobs 过期（deflate / q-value 已实现）。`AGENTS.md` 仍写监听 `:8080`，实际默认 `9080`。

## 7. 有意延期（生产 profile 禁用即可）

| 项 | 处理 |
| --- | --- |
| `ext-plugin-*` / `inspect` | 未注册；路由 compile fail-closed |
| WASM / XRPC / OCSP / QUIC / HTTP/3 | 启动拒绝 |
| 通用 stream plugin chain / stream mTLS | `proxy_mode: http` |
| HTTP `chash` / ketama | compile 拒绝 |
| `sls-logger` / 本地 session / limit-* / SkyWalking | 不在六插件 allowlist |
| Admin API / discovery | 启动拒绝 |
| 完整 Lua/OpenResty | 不在 Go-native 范围 |

## 8. 建议的当前运行方式

1. 只用 `deployment.profile: http-data-plane-v1` + 镜像默认 `config-production.yaml`；不要用 `config-default.yaml` 当生产。
2. 提供 `https://` etcd 且 `tls.verify: true`。干净 `docker run` 在补 host 之前应 fail-closed。
3. 迁移配置先剥掉 `vars` / `remote_addrs` / 显式 `status: 0`；数据面现在会拒绝或跳过这些字段，而不是静默忽略。
4. jwt 配 `claims_to_verify: ["exp"]`；上游 HTTPS 配 `tls.verify: true`；认证插件配 `hide_credentials: true`。
5. 不要启用 logger / sls-logger / stream / `gm`。
6. K8s：liveness `/livez`，readiness `/readyz`。不要把 Docker HEALTHCHECK 当 liveness。
7. 发布只认 `security-release-gates` 签过的 digest；不要把 RC 或 `master` 镜像当生产。

## 9. 判定 production ready 的条件

1. 第 4 节三个路由字段 fail-closed（或真正实现）并有负向测试。**已满足**（001–003 CLOSED）。
2. 按 runbook 留下本版本的签名、attestation、soak、etcd 恢复证据；回滚路径可执行或明确 bootstrap。**未满足**（004 OPEN）。
3. 操作手册写明 jwt `exp`、上游 TLS verify、`hide_credentials`。**已写入** `docs/production-profile.md`。
4. 只保留一份当前 ledger；8 月 9 日 / 8 月 15 日报告作为历史。
5. README / CLI 警告在第 2 项完成前保持。

## 10. 本轮未跑的检查

没有跑 `go test ./...`、`make test`、整包 `t/plugin`、本机 Docker 构建、live cosign、etcd chaos 或 30 分钟 soak。各 subagent 跑了 impact-scoped `source .envrc && go test -mod=mod`。未修改生产代码。

本文件是 2026-08-16 的唯一 production-readiness 排序账本；`docs/production-readiness-review-2026-08-15.md` 与 `docs/production-readiness-remediation-2026-08-15.md` 保留为历史。
