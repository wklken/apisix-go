# APISIX 3.17 全插件差异测试批量流水线计划

**目标：** 将“可信差异测试工具”与“逐插件差异验证”拆成两个独立阶段；冻结工具后，以 3 个并行场景生产 lane、每轮约 24 个插件的批次推进，尽快获得 111/111 插件的可审计差异证据。

**当前基线：** 固定 APISIX 源码提交 `9ef2ecab67f652d38365049613610ef649bb4ad0`，固定 linux/amd64 oracle digest `sha256:5a8d7dfd8382aebfc0cab7bf9d24edf8dd73a6f0eed0b789d25578a373e86f64`。已有 24 个场景覆盖 14/111 插件；`real-ip`、`request-id` 场景已准备但尚未纳入共享 catalog。旧 artifact 对应旧 candidate binary，不能作为当前最终证据。

## 一、工作边界

1. Harness lane 只负责身份固定、进程隔离、请求观察、比较调度、artifact 和并行执行，不包含具体插件语义。
2. Plugin lane 只负责 APISIX 3.17 来源映射、场景配置、插件专用比较器和 focused tests，不修改 harness core。
3. Production-fix lane 只处理批量运行发现的真实实现差异；每个修复必须先有失败差异场景或聚焦回归测试。
4. Harness 问题必须由 harness 自测复现。插件场景失败不能直接作为修改 harness 的理由。
5. 通用平台恢复、supervisor/worker、stream TLS/UDP 和最终 production readiness 不进入本计划。
6. 全程清空环境代理；不提交、不推送、不创建 PR。

## 二、完成标准

### Harness freeze gate

- 支持按 plugin/case 过滤、固定并行度和稳定 shard。
- 每个并发场景拥有独立 candidate 端口、临时目录、fixture、容器名和日志路径。
- 并发失败仍收集全部场景结果，artifact 顺序稳定且不可覆盖。
- artifact 固定 candidate source commit、binary SHA-256、catalog SHA-256、oracle source commit 和 image digest。
- 严格比较不修改原始 observation；动态字段只能由命名的插件比较器处理。
- 自测覆盖代理清理、并发资源冲突、binary/catalog 篡改、部分失败 artifact、稳定 merge 和未知 normalizer 拒绝。
- 以一组 exact、动态 token、fixture 三类 golden cases 完成真实 Podman 验证后，将 schema/profile 版本冻结。

### 111-plugin breadth gate

- 111 个 required plugin 每个至少有一个 APISIX 3.17 来源绑定的可执行差异 obligation。
- 每个普通 HTTP 插件优先包含一个成功路径和一个拒绝/边界路径；无法运行的真实外部依赖必须有准确的 dependency boundary，不能伪装成本地云服务证据。
- 每个场景记录 source file、TEST label、obligation、比较策略和 fixture 类型。
- 最终 fresh candidate 的全量 artifact 覆盖 111/111，所有可运行 case 通过；未运行项必须作为显式 boundary 单列，不能计入 passed。
- manifest 的 `differential: verified` 只从最终 artifact 提升；inventory、package test 或历史 artifact 不得代替。

## 三、阶段 A：一次性建立并冻结可信工具

### A1. 拆分接口

- Harness core：oracle/candidate 生命周期、资源分配、observation、artifact、shard/merge。
- Plugin cases：`differential_cases_<plugin>.go` 和对应 focused test。
- Plugin comparators：按插件注册，只接收 observation 副本，返回比较结果；不得修改 artifact 原始观察值。
- Shared catalog：由 controller 单独集成，worker 不并发编辑。

### A2. 增加隔离并行

- 默认并行度设为 3，与当前 3 个可用 worker/本机 Podman 资源匹配。
- 不跨场景复用 APISIX 或 candidate 进程；不同插件配置保持完全隔离。
- 先实现动态端口、唯一容器名/目录和结果同步，再启用并行。
- 支持 `plugins`、`cases`、`shard-index/shard-count` 选择，便于 focused 重跑和 artifact 分片。
- 运行结束按 case name 稳定排序并生成一个批次 artifact；每个失败保留两侧日志。

### A3. Freeze 验证

1. 两个 exact cases 并发运行，无端口、fixture 或容器名冲突。
2. CSRF 动态 token case 证明结构、签名、Expires、未知字段和 random 范围都被校验。
3. 一个本地 upstream fixture case 证明请求观察和响应比较在并发下不串线。
4. 人为制造一项失败，证明其他 case 继续运行且 artifact 完整记录。
5. 重复同一批次，除时间/attempt identity 外，case 集合和稳定字段一致。

Freeze 后，任何 harness core 修改都暂停插件批次，并要求单独的 RED/GREEN 自测和版本说明。

## 四、阶段 B：建立 111 插件差异清单

Controller 从 `pkg/capability/manifest.yaml`、固定 APISIX 源码和现有 corpus 生成唯一矩阵，每个插件只属于一个类别：

1. `local-http`：纯请求/响应逻辑，可直接批量差异。
2. `local-fixture`：需要本地 HTTP/TCP/Kafka/collector 等协议 fixture。
3. `external-dependency`：需要真实云或第三方服务；先复用 APISIX 官方 mock/fixture，无法本地重现时记录 dependency boundary。
4. `platform/native-boundary`：行为由 NGINX/OpenResty/control-plane/stream platform 所有，不伪造为插件 differential pass。

矩阵必须包含插件、上游文件和 TEST label、首轮 obligation、预计 case 数、fixture 类型、owner、状态。先完成 breadth 清单，再写场景，避免边找来源边改 runner。

## 五、阶段 C：四轮批量场景流水线

当前运行时最多为 controller + 3 workers。每轮采用固定流水线：

1. 三个 worker 各领取约 8 个互不重叠插件，仅拥有各自的 case/comparator 文件。
2. 每个 worker一次性完成来源映射、2--5 个代表场景和 focused tests，返回 catalog rows，不编辑共享 catalog/manifest。
3. Controller 审核来源和比较策略，一次合并约 24 个插件到 catalog。
4. Harness 以 3 路隔离并发跑完整 wave，不逐插件启动人工循环。
5. 失败被归类为 `case-error`、`harness-bug`、`candidate-diff`、`dependency-blocked` 或 `declared-boundary`。
6. `candidate-diff` 进入独立 production-fix 队列；修复后只重跑失败 shard，再重跑整轮。
7. 整轮通过后才更新 ledger；manifest promotion 留到 111-plugin final artifact。

批次目标：

- Wave 1：约 24 个 local-http 插件。
- Wave 2：约 24 个 auth/rewrite/traffic 插件。
- Wave 3：约 24 个 response/compression/protocol-local 插件。
- Wave 4：剩余 AI/logger/external-dependency/platform-boundary 插件。

具体插件归属由阶段 B 的源码矩阵生成，不凭名称猜测。`real-ip` 和 `request-id` 作为 Wave 1 已准备输入。

## 六、阶段 D：最终全量证据

1. 从当前工作树构建 fresh candidate，记录新 binary SHA-256。
2. 使用冻结 harness 和最终 catalog 跑所有 shard；失败 shard 可修复重跑，但最终资格 artifact 必须是一次 fresh 全量运行。
3. 校验 artifact 覆盖、唯一性、source/image/binary/catalog identity 和每插件 obligation 统计。
4. 仅依据最终 artifact 更新 `pkg/capability/manifest.yaml`，重新生成文档并运行 drift check。
5. 独立复核 harness freeze 未被绕过、所有 normalizer 有边界测试、所有 boundary 未计入 passed。
6. 报告“APISIX 3.17 插件差异 breadth 111/111”或准确的剩余 blocker；不报告 production ready。

## 七、吞吐与风险控制

- 目标吞吐：每轮约 24 个插件，4 轮覆盖剩余 97 个；不是再执行 97 个串行小任务。
- 优先优化场景级并发，不先做跨场景进程复用。进程复用会引入配置污染、插件状态泄漏和 fixture 串线，只有测量证明并发启动仍是瓶颈时再单独设计。
- Worker 只写独占文件；catalog、manifest、生成文档和 artifact 由 controller 串行处理。
- 先完成 breadth，再扩充每插件全部相关上游 TEST label 的 depth。breadth 达标不等于所有上游用例已逐条覆盖。
- 如果某插件必须依赖未提供的真实凭证或服务，明确报告 blocker；不通过放宽比较器或把 package test 冒充 differential 来清零。
