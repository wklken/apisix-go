# Benchmark 数据分析与性能瓶颈报告（2026-08-07）

> 数据来源：PR #26 合入 master 的可复现 benchmark 基础设施，merge commit `834eff7`
> （原 PR head `f4b9aba`）。本报告先记录原始测量与定位，再记录
> `codex/optimize-route-dispatch` 按 AGENTS.md 「Performance Benchmarking」契约完成的
> identical-corpus baseline → current → compare 验收。

## 1. 测量环境与方法

- 主机：Apple M2 Pro（macOS darwin/arm64），go1.26.4（toolchain go1.26.4）
- 命令：`scripts/benchmark.sh run` 默认参数 `-benchtime=1s -count=10 -cpu=1,4 -p=1`
- 汇总工具：pinned `benchstat v0.0.0-20260709024250-82a0b07e230d`
- 数据均取 `-cpu=1` 行（GOMAXPROCS=1），`B/op`/`allocs/op` 为 -benchmem 实测
- 辅助证据：`profile-cpu` / `profile-mem`（`-benchtime=5s -count=1 -cpu=1`），
  heap 审查固定 `go tool pprof -sample_index=alloc_space`
- 全部产物仅存在于忽略目录 `.cache/bench/`，未提交

## 2. 数据

### 2.1 pkg/json（goccy/go-json 封装）

| Benchmark | 1KiB | 32KiB | 256KiB | B/op @256KiB | allocs @256KiB |
|---|---|---|---|---|---|
| MarshalTyped | 2.91µ | 14.7µ | 84.4µ | 265.1Ki | 11 |
| UnmarshalTyped | 3.64µ | 28.5µ | 190.6µ | 267.1Ki | 52 |
| MarshalDynamic | 5.84µ | 18.4µ | 93.7µ | 266.0Ki | 15 |
| UnmarshalDynamic | 5.95µ | 30.6µ | 190.0µ | 269.4Ki | 119 |
| Encoder | 2.66µ | 10.2µ | 61.8µ | 1.154Ki | 11 |
| Decoder（UseNumber） | 7.75µ | 56.3µ | 362µ | **1.255Mi** | 150 |

- 吞吐：Marshal ~3GB/s（256KiB 档），Unmarshal ~1.3-1.4GB/s，Decoder ~700MB/s。
- Decoder 是全部 18 行中 B/op 放大最严重的：256KiB payload → 1.255MiB/op（约 5 倍），
  heap profile 显示 99% 以上分配集中在 goccy streaming decoder 的 buffer 读取与
  empty-interface decode 内部。

### 2.2 pkg/plugin/base（logger 请求/响应体捕获）

| Benchmark | 1KiB | 1MiB | B/op @1MiB | allocs @1MiB |
|---|---|---|---|---|
| ReadAndRestoreRequestBody（首次捕获） | 600n | 229.3µ | 3.125Mi | 27 |
| ReadSharedRequestBody loggers=1 / 3 | 6.04n / 16.9n | 6.04n / 16.8n | ~0 | ~0 |
| SharedResponseRecorderWrite | 201n | 120.6µ | 1.000Mi | 2 |
| SharedResponseRecorderBody cold / cached | 151n / 2.1n | 91.3µ / 2.1n | 1.000Mi / 0 | 1 / 0 |
| Decoded gzip cold / cached | 4.64µ / 10.5n | 683.1µ / 10.5n | 3.164Mi / 0 | 31 / 0 |
| Decoded br cold / cached | 9.28µ / 9.4n | ~1.9ms / 9.4n | 4.19Mi / 0 | 39 / 0 |

- 缓存命中路径全部为个位数 ns 且 0 分配：shared capture 的复用设计（1/3 个 logger）完全生效。
- 冷路径成本集中在：`io.ReadAll` 缓冲增长 + `string()` 拷贝（body），以及
  `decodeResponseBody` 内 `io.ReadAll` 解压分配（gzip 3.16Mi B/op @1MiB）。

### 2.3 pkg/proxy

| Benchmark | 1KiB | 64KiB | 1MiB | B/op | allocs |
|---|---|---|---|---|---|
| ProxyBufferPool serial / parallel | 7.82n / 7.84n | — | — | 0 | 0 |
| ReverseProxyServeHTTP | 1.103µ | 1.780µ | 20.05µ | **1.672Ki（恒定）** | 15（恒定） |

- 32KB 缓冲池完全生效：1MiB 响应下 B/op 仍恒为 1.67KiB，无分配放大。
- `1.672Ki` 常量来自 15 次固定分配的元数据结构，与 payload 大小无关。

### 2.4 pkg/route（优化前基线）

| Benchmark | 10 | 100 | 1000 | B/op @1000 | 备注 |
|---|---|---|---|---|---|
| ConvertURI param / wildcard | 300.8n / 47.8n | — | — | 116B / 0 | |
| RegisterRoutes static | 111.0µ | 10.55ms | **1.092s** | **754.9Mi** | 超线性（~n²） |
| RegisterRoutes embedded | 24.79µ | 242.0µ | 2.378ms | 1.698Mi | 近线性 |
| Dispatch static match / miss | 314n / 429n | 329n / 435n | 357n / 457n | 368B / 416B | 与路由数无关 |
| Dispatch embedded match-first | 667.6n | 1.786µ | **12.76µ** | 704B | 线性扫描 |
| Dispatch embedded match-last | 548.0n | 547.4n | 540.8n | 704B | 反向扫描第一轮命中 |
| Dispatch embedded miss | 892.1n | 2.001µ | **13.05µ** | 752B | 线性扫描 |

## 3. 瓶颈分析（按严重度排序）

### P1：静态路由注册 O(n²)，1000 条耗时约 1.1s、分配 755MiB/op（已修复）

- 数据：`RegisterRoutes/kind=static` 10→111µ、100→10.55ms（×95）、1000→1.09s（×103）。
- 根因（heap profile 定位，99.4% 分配）：基线 commit `834eff7` 的
  `registerRouteWithHosts`（pkg/route/builder.go:134）
  对**不含 `:` 参数的路由**一律走 `registerWildcardRoute` → `existingWildcardDispatcher`
  （builder.go:242），而后者每次注册都调用 `mux.Routes()` 全树遍历
  （`chi.(*node).routes.func1` 每次重建全部 Route 切片）。静态路由每条都是独立 pattern，
  chi 树随注册增长，于是每次注册遍历整棵树 → O(n²)。
- 影响边界：1.1s 是 `registerRoute` 阶段的隔离测量，不是包含 store 解码、handler/plugin
  构建、日志和 router 替换的端到端 etcd reload 数据。1 万条的百秒级结果只是 O(n²)
  外推，未做实测。
- 修复：每次 `Builder.Build` 创建一个 `routeRegistrar`，用
  `map[convertedPattern]*wildcardDispatcher` 持有该 mux 的 dispatcher，删除
  `mux.Routes()` 扫描。单条 dispatcher 查找由 O(n) 变为 O(1)，整批注册由 O(n²)
  降为近线性。

### P2：embedded wildcard 最坏命中和 miss 都是 O(n) 扫描（已修复）

- 数据：1000 条时 `match-first` 12.76µ、`miss` 13.05µ；`match-last` 只有 540.8ns。
- 根因：基线 commit `834eff7` 的 `wildcardDispatcher.ServeHTTP`（builder.go:256）
  对 miss 要 `slices.Backward`
  扫完当前 bucket 的全部路由；profile 显示 `matchesWildcardRoute`（43.1% cum）+
  `runtime.memequal` / `IndexByteString`（37.5%）占主要 CPU。
- 原 benchmark 的 `match` 只测最后注册路由，是反向扫描的最佳情况；较早注册的命中同样
  接近全量扫描，不能概括为所有 match 都与路由数无关。
- 修复：保留 bucket slice 作为注册顺序真源，并为每个 host/method bucket 建立
  `suffix -> bucket indexes`。请求只枚举 path 中可能的 literal suffix，再从候选中选择
  最大 bucket index；复杂度为 O(path segments + matching candidates)，不是对任意 suffix
  承诺 O(1)。

### P3：JSON streaming Decoder 的分配放大（测量成立，生产瓶颈未确认）

- 数据：`Decoder+UseNumber` 256KiB → 362µ、1.255MiB/op、150 allocs；独立的
  `UnmarshalDynamic` 行是 190µ、269.4KiB/op、119 allocs。
- 根因：`map[string]any` + `UseNumber` 逐节点分配（interface/string/Number 对象），
  heap profile 99% 落在 goccy unmarshal 内部。
- 影响边界：两行使用不同 API，不能把 Decoder 的 5 倍 B/op 与 UnmarshalDynamic 的
  119 allocs 合并成一个结论。256KiB 时 typed/dynamic Unmarshal 延迟基本相同
  （190.6µs vs 190.0µs），也不支持“typed 快约 30%”。生产 `Decoder+UseNumber` 用于
  standalone resource ID、`forward-auth` 的 `$post_arg`，以及 `grpc-transcode` 的
  int64/hash JSON 归一化；缺少这些路径的流量与 payload 分布证据，本 PR 不做
  speculative codec 优化。

### P4：logger 冷路径成本（合理设计成本，但有固定放大）

- 首次 body 捕获 1MiB：229µ + 3.125Mi B/op（`io.ReadAll` 缓冲增长 + `string()` 拷贝，~3 倍放大）。
- gzip 冷解码 1MiB：683µ + 3.164Mi B/op（解压本身 1.43GB/s 为 CPU 主成本，分配来自
  `decodeResponseBody` 的 `io.ReadAll`）。
- br 冷解码 1MiB：~1.9ms，三类中最贵。
- 说明：这些是启用相关、且实际捕获/解压对应大 body 时每请求一次的成本；相邻 logger
  的缓存命中已是最优（10.5ns、0 分配），复用设计正确，不建议为省分配牺牲共享语义。

## 4. 已确认无问题的区域

- proxy 缓冲池：1MiB 响应 B/op 恒 1.67KiB、15 allocs，无放大。
- shared capture 缓存命中：request/response/decoded 均为个位数 ns、0 分配。
- static dispatch 与路由数无关；优化前 embedded `match-last` 是最佳情况，不能代表所有命中。
- ConvertURI wildcard：47.8ns、0 分配（param 变体 300.8ns、116B，来自 split+regex，量级可接受）。

## 5. Route 优化验收

预声明 practical thresholds：

- static 1000 routes 注册 ≤10ms/op 且 ≤5MiB/op；
- embedded 1000 routes `match-first`、`miss` ≤2µs/op；
- `match-last` 回退不超过 20%；
- dispatch `B/op`、`allocs/op` 不增加。

运行器接受 baseline-warm/current-latest 的 metadata 与 corpus 指纹，pinned benchstat 得到：

| 1000 routes row | Baseline | Current | Delta | 判定 |
|---|---:|---:|---:|---|
| Register static time | 1.105227s | 2.040ms | -99.82% | PASS |
| Register static B/op | 754.937MiB | 1.920MiB | -99.75% | PASS |
| Register static allocs/op | 5.02777M | 22.80k | -99.55% | PASS |
| Register embedded time | 2.4594ms | 476.4µs | -80.63% | PASS |
| Register embedded B/op | 1738.8KiB | 450.5KiB | -74.09% | PASS |
| Register embedded allocs/op | 14.027k | 4.062k | -71.04% | PASS |
| Dispatch embedded match-first | 12.7800µs | 642.8ns | -94.97% | PASS |
| Dispatch embedded match-last | 553.3ns | 574.8ns | 无显著差异（p=0.394） | PASS |
| Dispatch embedded miss | 13.2360µs | 789.0ns | -94.04% | PASS |
| Dispatch embedded B/op | 704 / 704 / 752B | 704 / 704 / 752B | unchanged | PASS |
| Dispatch embedded allocs/op | 4 / 4 / 7 | 4 / 4 / 7 | unchanged | PASS |

全部预声明阈值通过。当前 route benchmark corpus 中已无随路由数增长的已确认请求分发瓶颈；
完整 reload 仍需独立端到端 benchmark 才能判断 handler/plugin 构建等剩余成本。

## 6. 复现命令

```bash
# 原始完整数据
make init-bench
BENCH_DIR=.cache/bench/analysis make benchmark-current
bash -lc 'source .envrc && .cache/bin/benchstat .cache/bench/analysis/current.txt'

# 本次 route identical-corpus 比较
BENCH_DIR=.cache/bench/route-dispatch-formatted-corpus \
  BENCH_PACKAGES=./pkg/route \
  BENCH_CORPUS_FILES=pkg/route/benchmark_test.go \
  BENCH_REGEX='^Benchmark(RegisterRoutes|RouteDispatch)/' \
  BENCH_TIME=250ms BENCH_COUNT=6 BENCH_CPU=1 BENCH_P=1 \
  bash scripts/benchmark.sh compare baseline-warm current-latest

# 关键 profile
BENCH_DIR=.cache/bench/analysis make benchmark-profile-mem PROFILE_PACKAGE=./pkg/route \
  PROFILE_BENCH='^BenchmarkRegisterRoutes/kind=static/routes=1000$$'
BENCH_DIR=.cache/bench/analysis make benchmark-profile-cpu PROFILE_PACKAGE=./pkg/route \
  PROFILE_BENCH='^BenchmarkRouteDispatch/kind=embedded-wildcard/result=miss/routes=1000$$'
BENCH_DIR=.cache/bench/analysis make benchmark-profile-mem PROFILE_PACKAGE=./pkg/json \
  PROFILE_BENCH='^BenchmarkJSONDecoder/size=256KiB$$'
```

> 注：make 命令行传入 `PROFILE_BENCH` 时正则中的 `$` 需写成 `$$`（make 解析会吞掉单 `$`）。
