# apisix-go Production Readiness 深度审查报告

日期：2026-08-17（同步 production-readiness-hardening 实现）

审查基线：`5f2df72628ef9b3f6ac6f8c0517ba3a6d9dfc397`（`master`）；当前账本同步
本轮 hardening 实现改动，发布证据仍未产生。

审查类型：基于当前源码、配置、工作流和本轮 focused verification 的交叉复核；
保留 2026-08-15 ledger 与随后变更作为历史背景。

结论：**当前仓库仍不是 production ready。** 8 月 15 日剩下的实现型 P0（001–009）在代码里已关闭。仓库现在是可运行的 **`http-data-plane-v1` 候选 HTTP 数据面**，还不能替换 Apache APISIX，也不能摘掉 README / CLI 的 “not production ready” 声明。

剩下的阻断：

1. **代码 P0（001–003 + 本轮 hardening）**：**CLOSED**。路由 ACL/脚本
   字段、候选 profile 的认证与上游 TLS 策略、正的 body timeout，以及插件
   状态选择器的独立工作流均已落地并有 focused verification。
2. **发布资格 P0（004）**：**OPEN**。没有针对本 HEAD 的已签名 digest、RC/final
   证据和可回滚旧 digest；外部 ingress 请求日志与仓库/环境策略也未完成资格
   证明。

## 1. 执行摘要

#115–#120 以及本轮 hardening 把对应实现缺口收掉了：`script`/`filter_func`
fail-closed、`/livez`/`/readyz`、生产镜像入口、默认日志头脱敏、logger TLS
校验、缺 weight 拒绝、`ai` 占位失败、认证 401 带 CORS、HTTP `chash` 拒绝、全局
413、密钥物化、认证默认值、精确六插件 profile，以及 singular `host`、
`remote_addr`/`script_id` fail-closed、候选认证/TLS admission、正的 body
timeout 和独立 plugin-status CI。

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

### 本轮 production-readiness-hardening

| 项 | 状态 | 当前实现与边界 |
| --- | --- | --- |
| singular `host` / `host` 与 `hosts` 冲突 | **CLOSED** | singular `host` 进入与单元素 `hosts` 相同的 exact/wildcard dispatcher；冲突或空值在 `BuildStrict` 拒绝，错误保留 route ID。 |
| `remote_addr` / `script_id` | **CLOSED** | 任意配置的 `remote_addr` 与 non-null `script_id` 在 route compile fail-closed；`remote_addrs`、`vars`、`script`、非空 `filter_func` 同样不再静默忽略。 |
| 候选认证与上游 TLS admission | **CLOSED** | `http-data-plane-v1` 对 effective route/plugin-config/service winners 与 global rules 强制认证凭据隐藏、JWT `exp`、已解析 HTTPS/gRPCS 的 `tls.verify: true`；空 profile 保持兼容默认。 |
| body read timeout | **CLOSED** | 候选 profile 要求正的 `client_body_timeout`；`conf/config-production.yaml` 固定为 60s，并映射为 header/body combined `ReadTimeout`。 |
| plugin status source-of-truth CI | **CLOSED** | 独立 workflow 在每个 PR 创建 required-compatible check 并运行精确 `TestSupportedPluginManifestSelection`；`master` push 仅监控 `docs/plugins.md`、manifest、selector test 和自身，不替代发布资格。 |

上述 CLOSED 是代码/工作流闭环，不等于外部部署或发布资格。本轮 focused
tests、lint、build、workflow contract 和 `git diff --check` 的结果不能替代
真实 RC/final、ingress、容量、恢复或回滚证据。

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

工作流已有 keyless cosign、`attest-build-provenance`、RC operational gates、etcd
recovery harness，以及独立的 plugin-status selector gate。本提交没有 `v*` tag、
没有 GHCR 已签名 digest、没有保留的 SBOM/Trivy/soak/recovery 证据包。首次发布
没有旧 digest，回滚无法验收。`master` 保护、CI/security/plugin-status required
checks、`production-release` 环境的 `v*` tag policy（保留 reviewer / wait timer）
和 no-self-approval 仍需由仓库/环境管理员验证，不能由本 PR 代替。

**验收**：按 `docs/runbooks/production-release.md` 对同一 source revision 跑通一次
post-merge RC 和一次独立 final；
保留可验证签名、attestation、SBOM/Trivy、soak JSON、etcd 恢复记录、外部 ingress
请求日志 evidence；确认 protected `master` 和 `production-release` policy；文档化
回滚 bootstrap 并实际回滚到 distinct older digest。在此之前不要摘警告横幅。

## 5. 已关闭、不应再当实现 P0 的项

8 月 15 日 001–009，以及 #116/#117/#119/#120 关闭的 body 上限、密钥物化、认证默认值、profile 门禁。详见第 3 节。

`consumer-restriction.allowed_by_methods` 未列出用户放行仍是 APISIX 同款行为，不是 Go 绕过。

`sls-logger` 把 `access-key-secret` 放进 RFC5424 SD 与 APISIX 3.17 协议一致。不要只在 Go 里删掉该字段。生产 profile 已排除该插件；兼容 allowlist 仍包含它。

兼容模式仍保留官方默认：jwt-auth 无 `exp` 的 token 不过期、上游 HTTPS 默认不校
验证证书、`hide_credentials` 默认 false。候选 `http-data-plane-v1` 已在 route
compile 前 fail-closed：jwt 必须含 literal `exp`，HTTPS/gRPCS 必须
`tls.verify: true`，key/jwt/basic 必须 `hide_credentials: true`；disabled auth
config 不触发该要求。

## 6. P1：production candidate 前处理

### 6.1 兼容模式（空 profile）

- 默认 access-log denylist 不含 key-auth 默认头 `apikey`，也不打码 query token。[Security](bb976cde-403e-4b6a-8efb-9fb9441ad9cb) P1-1。profile 六插件无 logger，故候选 profile 不触发。
- `gm` 仍是 pass-through，`PostInit` 成功。[Parity](2dd4ed4e-e8ff-4862-aada-37078782e5b1)。profile 进不去。
- CLI 默认 `-c conf/config-default.yaml`：`debug: true`、loopback etcd、完整 allowlist。镜像 CMD 已改生产文件。
- 镜像仍 COPY 兼容配置；覆盖 `CMD`/`-c` 会离开生产 profile。

### 6.2 候选 profile 上的官方 footgun

- 候选 profile 已把这些默认值变成 admission requirement：jwt-auth 的
  `claims_to_verify` 必须含 literal `exp`；上游 `https`/`grpcs` 必须
  `tls.verify: true`；三个 allowlist 认证插件必须
  `hide_credentials: true`。不满足时 `BuildStrict` 拒绝并保留来源/route
  identity。
- 空 profile 仍保留上述 APISIX 兼容默认，不把兼容配置误称为候选 profile。
- basic-auth 成功时 Info 打 consumer 名。

### 6.3 运维

- Docker HEALTHCHECK 打 `/livez`；编排应 `/livez` 存活、`/readyz` 就绪。etcd
  抖动会把 readiness 标为 unhealthy，但不应把 Docker liveness probe 当作
  readiness。
- Soak 测的是 commit 上的 `go test ./pkg/route`，不是已发布容器。
- RC `publish-image: false`，没有签名 RC digest。
- `SIGHUP` 是优雅退出 + 非零，不是 reload。
- `docs/plugins.md` 的 status/manifest 选择由独立 plugin-status workflow
  触发并运行精确 selector；GitHub-hosted check 结果仍需在发布前取得。
- `AGENTS.md` 仍写监听 `:8080`，实际默认 `9080`。

### 6.4 外部 ingress request-log evidence

六插件候选 profile 没有进程内 request logger。依赖外部 TLS-terminating
ingress 的部署，必须在 RC/final qualification 前提供脱敏 request-log bundle，
覆盖成功、拒绝、失败请求，并证明 redacted request ID、method、normalized path
（不含 query secrets）、status、latency、upstream identity、retention owner 和
trace correlation。该 bundle 是外部 ingress 的资格证据，不是 Go runtime 发出
这些字段的声明；本 PR 不修改或验证外部日志系统。

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
3. 迁移配置先剥掉 `remote_addr` / `remote_addrs` / `vars` / `script` /
   `script_id` / 非空 `filter_func`；singular `host` 可用，但不能与 `hosts`
   同时配置。仅为有意停用的路由保留 `status: 0`，此类路由被接受但不会注册
   到 HTTP 路由表。数据面会拒绝不支持的语义，而不是静默忽略。
4. jwt 配 `claims_to_verify: ["exp"]`；上游 HTTPS/gRPCS 配
   `tls.verify: true`；认证插件配 `hide_credentials: true`；生产 profile 的
   `client_body_timeout` 保持正值（镜像参考值 60s）。
5. 不要启用 logger / sls-logger / stream / `gm`；为外部 ingress 留下脱敏
   request-log evidence。
6. K8s：liveness `/livez`，readiness `/readyz`。不要把 Docker HEALTHCHECK 当
   readiness。
7. 发布只认 `security-release-gates` 签过的 digest；不要把 RC 或 `master` 镜像当生产。

## 9. 判定 production ready 的条件

1. 路由 singular `host` 正确限制匹配，`host`/`hosts` 冲突、`remote_addr`、
   `script_id`、`vars`、`remote_addrs`、`script`、`filter_func` 按当前支持边界
   fail-closed（或跳过 status 0）并有负向测试。**已满足**（001–003 CLOSED）。
2. 候选 profile 对 effective auth/JWT/TLS 配置 fail-closed，并要求正的
   `client_body_timeout`（参考值 60s）。**已满足**；空 profile
   兼容默认未改变。
3. 独立 plugin-status workflow 对 status matrix/manifest selector 保持精确
   gate。**代码已满足**；GitHub-hosted required-check 结果仍需
   在发布前取得。
4. 按 runbook 留下本版本的签名、attestation、SBOM/Trivy、soak、etcd 恢复、
   外部 ingress request-log 证据；回滚路径可执行并回滚到 distinct older digest。
   **未满足**（004 OPEN）。
5. `master` protection、CI/security/plugin-status required checks、
   `production-release` reviewer/wait 与允许的 `v*` tag policy、no-self-approval
   由管理员保持并验证；本 PR 不修改这些外部状态。
6. 只保留一份当前 ledger；8 月 9 日 / 8 月 15 日报告作为历史。
7. README / CLI 警告在第 4 项完成前保持。

## 10. 本轮未跑的检查

没有跑 `go test ./...`、`make test`、整包 `t/plugin`、本机 Docker 构建、live
cosign、etcd chaos 或 30 分钟 soak。本轮已按 impact-scoped 范围完成 focused
tests、lint、build、workflow contract 和 `git diff --check`；GitHub-hosted
required-check、RC/final、外部 ingress、容量、etcd chaos 与 rollback 仍需在
发布/部署环境中取得证据；本轮未修改外部状态。

本文件是当前唯一 production-readiness 排序账本；`docs/production-readiness-review-2026-08-15.md`
与 `docs/production-readiness-remediation-2026-08-15.md` 保留为历史。
