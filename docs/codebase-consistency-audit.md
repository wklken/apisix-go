# 代码一致性 & 重复逻辑 & 造轮子 全量审计

> Status: 全量只读扫描完成,2026-08-07,基于 `master` (bea4ea8)
> 扫描范围:`cmd/`、`pkg/`、`t/` 全部 Go 文件(排除 `vendor/`、`.worktrees/`、`*_test.go`)
> 方法:5 个并行只读 agent,分别覆盖 JSON 处理、字符串处理、列表/循环处理、Map 处理、重复逻辑/造轮子;所有结论均经 rg/grep/diff 实扫验证。
>
> 本文档是审计结论与整改建议的落点。修一处,更新一处;全部整改完成后归档。

## 结论总览

| 维度 | 评级 | 一句话结论 |
|---|---|---|
| JSON 处理 | ✅ 良好 | 单一 codec 边界已建立并有 AST 守卫防回归,生产代码 0 处直接 `encoding/json` |
| 字符串处理 | ⚠️ 中上 | strconv 解析一致;但 fmt/strconv 数字转换混用 ~40+ 处,其中 15+ 处每请求热路径 |
| 列表/循环处理 | ✅ 良好 | slices/maps/lo/迭代器已大量使用;遗留 1 处调试输出、1 处 map 顺序隐患 |
| Map 处理 | ⚠️ 中等偏下 | 2 处裸断言可导致进程级 panic;三种取值风格并存无统一约定 |
| 重复定义 | ⚠️ 中等偏高 | 集中在成对克隆插件族(proxy_cache/graphql_proxy_cache 等),含逐字克隆 |
| 造轮子 | ✅ 低 | 仅 request_id 手写 UUIDv7 明确可替换(依赖库已有 `NewV7()`) |

## 1. JSON 处理

### 现状(已统一,防回归守卫在)

- `pkg/json/types.go` 封装 `goccy/go-json`(别名 `gojson`),导出 `Marshal/Unmarshal/MarshalIndent/NewDecoder/NewEncoder` 及 `RawMessage`/`Number` 类型别名。
- 生产代码直接 import `"encoding/json"` 数量:**0**;绕过封装直用 `github.com/goccy/go-json`:**0**。
- `pkg/json/imports_test.go` 的 AST 守卫(`TestProductionCodeUsesProjectJSON`)扫描 `cmd/`+`pkg/` 并显式豁免 `t/`;allowlist 为空。
- `t/plugin/case.go:7` 使用 `encoding/json` 是有意保留的"独立兼容性 oracle"。
- `util.Parse`(pkg/util/parse.go)约 30 处,配置进入路径(etc/consumer → 插件 config)统一走它。
- `MarshalIndent` 封装已导出但生产代码 0 调用(死封装,仅信息)。

### 遗留问题

| # | 位置 | 问题 | 建议 |
|---|---|---|---|
| J1 | `pkg/plugin/base/request.go:35-37` | `WriteJSONMessage` 用 `fmt.Fprintf(w, '{"message":%q}', ...)` 手工拼 JSON,与 `util.BuildMessageResponse` 构造相同 wire 形状但 escaping 不同(`%q` 不转义 `<`/`>`/`&`);Content-Type 也不同 | 统一为单一响应 helper |
| J2 | 全仓 4 条 HTTP JSON 响应路径并存 | `base.WriteJSONMessage` / `util.BuildMessageResponse`+`http.Error` / `json.NewEncoder(w).Encode`(node_status、server_info、csrf、batch_requests、wolf_rbac)/ `util.WriteJSON`(仅 route/builder.go:1408,2026);escaping、trailing newline、Content-Type 细节不一致 | 收敛到 `util.WriteJSON` / `base.WriteJSONMessage` 二选一 |
| J3 | `pkg/plugin/tcp_logger/plugin.go:359` vs `pkg/plugin/udp_logger/plugin.go:311` | `encodeBatch` 逐行相同(仅错误串 "tcp"/"udp" 不同);kafka_logger:485 / rocketmq_logger:404 同构,且 `originLogEntries` 各有相同副本 | 抽共享包 |
| J4 | `pkg/store/getter.go:35-39` | 加密路径手写 `json.Marshal` + `json.Unmarshal`,与 `util.Parse` 完全等价 | 换 `util.Parse(metadata, v)` |
| J5 | Decoder vs ReadAll+Unmarshal 混用 | 16 处 `json.NewDecoder(...).Decode` vs 2 处 `io.ReadAll`+`Unmarshal`(store/consumer_secret.go:218 有 1MiB 限制、ai_rag/plugin.go:276 为通用 helper),各有充分理由 | 不修,仅记录 |
| J6 | logger 插件 batch 编码 | 4 份近似重复(见 J3) | 见 J3 |

### 保持现状(不要改)

- map + `json.Marshal` 构造拒绝响应体(map key 排序确定性,无依赖书写顺序的代码)
- SSE 帧拼接 `"event: ...\ndata: " + ...`(`ai_protocols/protocol.go:269-274`)——协议格式拼装,非 JSON 拼接
- `UseNumber` 仅在 decode 到 `map[string]any` 处使用——行为正确

## 2. 字符串处理

### 现状(干净项)

- str→数字解析全部走 strconv(~130 处),无手写数字解析、无 `fmt.Sscanf`
- 无自写 contains/trim/lower/prefix 重实现(全走 `strings` 包,~600+ 处)
- 无循环内 `+=` 字符串拼接反模式;`strings.Join` 59 处;循环拼接都用 Builder
- 无手写 hex/base64/JSON 转义/URL 编码(hex 全走 `encoding/hex`,URL 全走 `net/url`)
- bytesconv(`pkg/util/bytesconv.go`)23 处使用,逐处核查生命周期均安全(bbolt key、一次性 w.Write、独占 marshal 缓冲区)
- Builder/Buffer 约 50/50 混用,但按用途(二进制/io vs 纯字符串)区分基本合理

### 遗留问题(按热度排序)

| # | 位置 | 问题 | 建议 |
|---|---|---|---|
| S1 | `pkg/plugin/base/logging.go:171-173` | `matchCondition` 每请求对 `[]any` 元素 `fmt.Sprint`,常见元素即 string | type-switch 免分配 |
| S2 | `pkg/plugin/base/logging.go:190,193` | 每请求 `regexp.MatchString` 动态编译(`~`/`!~` 过滤条件,被 syslog/skywalking/loki 等 8+ logger 插件每请求调用);`pkg/plugin/expr/expression.go:153` 同为表达式引擎却预编译——同项目两种做法 | 预编译(包级 `regexp.MustCompile`) |
| S3 | `pkg/observability/metrics/prometheus.go:396,401` | `fmt.Sprint(entry.Status)`(int)每请求打 label | `strconv.Itoa` |
| S4 | `pkg/plugin/ai_proxy/plugin.go:500`、`ai_proxy_multi/plugin.go:971` | `fmt.Sprintf("%d", status)` 注册 `$upstream_status`,每请求 | `strconv.Itoa` |
| S5 | `pkg/plugin/grpc_web/plugin.go:166`、`base/request.go:32`、`body_transformer/plugin.go:245`、`exit_transformer/plugin.go:259`、`error_page/plugin.go:82` | `fmt.Sprint(len(...))` 设 Content-Length,5 处每请求 | `strconv.Itoa` |
| S6 | `pkg/plugin/limit_conn/plugin.go:1020`、`loki_logger/plugin.go:540`、`ai_aliyun_content_moderation/plugin.go:824`、`graphql_proxy_cache/plugin.go:723` | `fmt.Sprintf("%d", ...)` 每请求生成 key/时间戳/Age 头 | `strconv.FormatInt` |
| S7 | `pkg/plugin/brotli/plugin.go:203`、`gzip/plugin.go:200` | `fmt.Sprintf("%d.%d", ProtoMajor, ProtoMinor)` 同一逻辑双实现,每请求 | 同 S 系列 + 合并公共 helper |
| S8 | host:port 三种写法并存 | ① `net.JoinHostPort(host, strconv.Itoa(port))` 约 10 处(正确);② `net.JoinHostPort(host, fmt.Sprint(port))` 8 处(syslog:282、error_log_logger:460,629、tcp_logger:237、udp_logger:204、sls_logger:208、datadog:313、redirect:162);③ `fmt.Sprintf("%s:%d", host, port)` 9 处(server:809、route/builder.go:1511、base/redis.go:23、limit_count:989、ai_rate_limiting:306,321、graphql_limit_count:735、loggly:388、example_plugin:98) | 统一写法 ①,写法 ③ 对 IPv6 host 拼出歧义地址 |
| S9 | `pkg/plugin/acl/plugin.go:246` | 每请求 `regexp.Compile(\s*(?:sep)\s*)` 做 split | 缓存编译结果 |
| S10 | `pkg/plugin/zipkin/plugin.go:64` | 正则 `^[0-9a-fA-F]+$` 校验十六进制 | `hex.DecodeString` 即可 |
| S11 | `pkg/plugin/data_mask/plugin.go:256-290` | `maxArgs<=0` 分支对同一 raw 调两次 `url.ParseQuery` | 复用解析结果 |
| S12 | `pkg/plugin/ai_stream/anthropic.go:161`、`sse.go:80`、`aws_eventstream.go:74` | `usage.Text += text` 流式逐块累加 | 影响有限,可 Builder,低优先级 |
| S13 | `pkg/plugin/authz_keycloak/plugin.go:615,717`、`ai_rate_limiting/plugin.go:446` | `fmt.Sprintf("%x", sha256.Sum256(...))` vs 项目 8+ 处 `hex.EncodeToString` | 统一 hex.EncodeToString |
| S14 | `pkg/server/server.go:211` vs `pkg/config/types.go:388` | 监听地址拼接同构逻辑,一个 `fmt.Sprintf` 一个 `strconv.Itoa` | 合并 |

### 保持现状(不要改)

- `echo/plugin.go:101,105` 的 `append([]byte(...), body...)` 大 body 重复拷贝、`basic_auth/plugin.go:193` 手写 Basic 解析、`mocking/plugin.go:225` 手写 content-type 解析、`real_ip/plugin.go:224` 自写 parseAddr fallback——均为边界场景,可接受
- 手写解析器(expr 语言、JSONPath 子集、模板语法)均无标准库替代,合理
- `== ""`(649)vs `len(s)==0`(213)混用——等价,不强制统一

## 3. 列表 / 循环处理

### 现状(干净项)

- 现代 API 已为主力:`slices.Contains` 21 处、`maps.Copy` 14 处、`lo.Filter/Map/FilterMap` 5 处、`strings.SplitSeq` 12 个文件、`slices.Backward` 3 处
- 无 slice 删除模式、无循环内删除 index 跳过 bug、无手写排序算法、无 append 预分配热点缺失
- 校验型去重(带错误信息)保留手写,合理

### 遗留问题

| # | 位置 | 问题 | 建议 |
|---|---|---|---|
| L1 | `pkg/plugin/init.go:377` | **生产路径遗留 `fmt.Println("plugin name:", ...)` 调试输出**(BuildPluginChain 循环内每插件打印) | 直接删除 |
| L2 | `pkg/plugin/traffic_split/plugin.go:236-247`、`pkg/plugin/resource/route.go:90-93` | map("nodes")→slice 顺序不确定:`selectHashedNode`(plugin.go:541-562)依赖 slice 顺序做 offset 减法,进程重启/多实例间 chash 选点漂移;`pkg/proxy/loadbalance.go:60-73` 明确注释"sort keys"实践正确,此处未排序,不一致 | 按 key 排序后 append |
| L3 | `pkg/plugin/openid_connect/plugin.go:1636-1645`、`ai_proxy_multi/plugin.go:1333-1344` | `[]any` 分支手写线性查找,同函数 `[]string` 分支用 `slices.Contains`——不对称 | `slices.Contains` 直接可用 |
| L4 | `pkg/plugin/proxy_cache/plugin.go:1577` + `graphql_proxy_cache/plugin.go:775` | `sameStringSlice` 两份逐字重复 | `slices.Equal` |
| L5 | `pkg/plugin/api_breaker/plugin.go:259` + `ai_proxy_multi/health.go:311` | `containsStatus` 两份相同(函数体即 `slices.Contains(statuses, status)`) | 直接 `slices.Contains` |
| L6 | `pkg/plugin/proxy_cache/plugin.go:1503-1512` | `cacheControlValueDirective` 命中后不 break,继续空转 | break |
| L7 | `pkg/plugin/proxy_cache/plugin.go:1549-1560` + `graphql_proxy_cache/plugin.go:744-764` | `parseVaryHeader` 两份几乎相同(仅 map 初始化写法差异) | 合并 + `lo.Uniq` |
| L8 | `pkg/plugin/limit_count/delayed_sync.go:253-262` | `uniqueStrings` 教科书式手写去重 | `lo.Uniq` |
| L9 | `pkg/server/server.go:301` | `for range` 线性扫描 + 副作用;若列表含重复项会重复 `metrics.Init()` | `slices.Contains` 包裹 |
| L10 | `pkg/plugin/proxy_rewrite/plugin.go:164-172` | 循环体内 `p.config.RegexURI[i]`/`[i+1]` 各重复索引 3 次 | 缓存到局部变量 |
| L11 | `pkg/plugin/proxy_cache/plugin.go:1294-1299`、`pkg/plugin/init.go:364-366`、`ai_auth/aws.go:247-251`、`stream/router.go:83-85` | `sort.Slice`/`sort.SliceStable` less 中重复索引 | `slices.SortFunc` / `slices.SortStableFunc` |

### 保持现状(不要改)

- `grpc_transcode/plugin.go:1034` `enumAsValue` 顺序敏感,不能换 `slices.Contains`
- 校验+错误返回的循环、解析+append、注册副作用等业务循环,不换 lo
- `proxy_cache/plugin.go:1319` range map 中 delete——Go 语义允许,安全
- `proxy_cache/plugin.go:1328-1329` 头部弹出(`slices[1:]`)——受 `maxVaryVariants` 上限约束,规模小,可接受

## 4. Map 处理

### 遗留问题(按危险度排序)

| # | 位置 | 问题 | 建议 |
|---|---|---|---|
| M1 | `pkg/plugin/ai_protocols/anthropic_openai.go:244` | **对不可信外部响应(OpenAI API)做链式裸断言** `contentParts[0].(map[string]any)["text"]`,结构变化即 HTTP 处理中进程级 panic——全项目 panic 风险最高处 | 改 `, ok :=` 双值断言或 `ai_common.AsAnyMap` |
| M2 | `pkg/plugin/elasticsearch_logger/plugin.go:448` | `action["index"].(map[string]any)["_type"] = "_doc"` 链式裸断言写入(当前由 443 行构造保证安全,风格危险) | 同上 |
| M3 | 三种取值风格并存 | 裸断言 2 处 / `, _ :=` 忽略 ok 48 处(AI 插件族:ai_proxy 19、anthropic_openai 14、oas_validator 10、ai_proxy_multi 10、ai_protocols 10、ai_request_rewrite 8)/ `, ok :=` 64 处;且同文件混排(如 `ai_proxy/plugin.go:1144-1150` 连续 3 次忽略 ok,同文件 1200/1215/1233 又用检查型) | 引入统一安全取值 helper;至少同文件统一 |
| M4 | `pkg/plugin/wolf_rbac/plugin.go:297-301` | `fmt.Sprint(userInfo["id"])` 等对 JSON 字段做 `%v` 格式化,类型不对时输出 `map[...]` 而非报错 | 双值断言 + 类型断言 |
| M5 | `pkg/plugin/otel/plugin.go:448`、`body_transformer/plugin.go:1072`、`lago/plugin.go:456` | map 取值 `fmt.Sprint(value)` 转字符串(慢且易错) | 类型断言 |
| M6 | `pkg/server/server.go:784-801` | 同一取值路径内手写断言(`attr["enable_export_server"].(bool)`)与 `cast.ToInt` 混用 | 同一函数统一一种风格 |

### 保持现状(不要改)

- `util.Parse` 走配置进入路径、手写逐字段断言走 plugin_attr/自定义字段——两条路径各自一致
- 深拷贝实现(`ai_common.CloneJSONValue`、`consumer_secret.go:84-92`、`data_encryption.go:327-332`)合理,不换 `maps.Clone`
- 锁模式整体一致(consumerMu RWMutex、stream/router.go:37、proxy/health.go:25 等);全局注册表统一 sync.Map
- 无 nil map 写入风险;`m[k] != nil` 存在性检查仅用于 json.RawMessage 场景,文件内一致
- key 存在性命名(ok 624 / exists ~20 / found 5)跨文件不统一——仅风格,不强制
- 默认值三种写法(ok-check 42 / nil-check 41 / 空串哨兵)——语义等价场景,不强制统一

## 5. 重复定义(重点)

### Top 榜单(按重复份数 × 代码量)

| 排名 | 模式 | 重复数 | 估计代码量 | 最佳归宿 |
|---|---|---|---|---|
| 1 | proxy_cache ↔ graphql_proxy_cache 缓存族(`cloneHeader`/`parseVaryHeader`/`varySignature`/`sameStringSlice`/Cache-Control TTL 解析) | 2 | ~120 行逐字 | 抽 `pkg/plugin/cacheutil` 或并入 `pkg/util` |
| 2 | exit_transformer ↔ serverless Lua 转换(`luaTableToHeader`/`goValueToLua`/`luaValueToGo`/`luaTableToGo`) | 2 | 96 行逐字节相同(diff IDENTICAL) | 抽 `pkg/plugin/luautil` |
| 3 | dubbo_proxy ↔ http_dubbo 传输层(`ServeDubboWithRetries`、`buildDubboRequest`、`serveDubboAttempt`、`writeDubboResponse`,~400 行同构;`WithConfig`/`GetConfig` 双份) | 2 | ~400 行同构 | http_dubbo 复用 dubbo_proxy 传输包 |
| 4 | 限流三件套(limit_req/limit_conn/limit_count + graphql_limit_count):`redisInt` 3 份逐字(limit_req:581、limit_conn:948、graphql_limit_count:826)、`resolveLimitValue`/`numericLimitValue`/`parseLimitInt`/`varPattern` 正则同构、quotaHeaders 默认头同构 | 3-4 | ~200 行 | 抽 `pkg/plugin/limitbase` |
| 5 | `transport()`(DefaultTransport.Clone + Keepalive/SSLVerify)AI 族 6 份(ai_proxy:1277、ai_proxy_multi:1626、ai_request_rewrite:440 逐字;feishu_auth:324、ai_aws_content_moderation:374、ai_rag:299 简版)+ 全仓 Clone 模式 15 处 | 6 | ~60 行 | `pkg/shared` 提供 transport 构建器 |
| 6 | `writeJSON` 6 处(ua_restriction:198、referer_restriction:191、consumer_restriction:218、authz_keycloak:844、wolf_rbac:357、batch_requests:442)+ basic_auth 7 处手写 `{"message":...}` 字面量 + ip/referer/ua 三处 `json.Marshal(map{"message"})`(应已存在 `util.BuildMessageResponse`) | 6+7+3 | ~80 行 | 统一 `util.WriteJSON`/`base.WriteJSONMessage` |
| 7 | `resolveValue` 变量插值(`$[A-Za-z0-9_]+` 正则 + ReplaceAllStringFunc):mocking:292、fault_injection:224、traffic_label:230(后两者逐字)、response_rewrite:492、api_breaker:263、forward_auth:292;正则常量 5 处重复 | 6 | ~60 行 | base 提供字符串插值 helper |
| 8 | 会话签名 3 套:base/oauth_session.go 已有 `SignSessionValue`/`VerifySessionValue`,cas_auth:404-437、saml_auth:707-737 各自重写 HMAC-SHA256 + base64url;feishu_auth、dingtalk_auth 已正确用 base 版本 | 3 | ~90 行 | 全部改用 base 版本 |
| 9 | `readBody` 3 处(graphql_proxy_cache:601、graphql_limit_count:934 逐字、data_mask:613)+ `base.ReadRequestBody` 已存在 | 3 | ~40 行 | 复用 base |
| 10 | `requestSize` 3 处逐字(datadog:474、request_context:137、google_cloud_logging:631) | 3 | ~30 行 | 抽 `pkg/util` |
| 11 | `callbackPath` 3 处逐字(authz_casdoor:271、cas_auth:472、saml_auth:817) | 3 | ~30 行 | 抽 base |
| 12 | 客户端 IP 提取内联 17 处(ip_restriction:138、opa:408、proxy_cache:1624、sls_logger:311、expr/request.go:43、zipkin:310 等),`base.RemoteIP` 已存在;sls_logger:310 `requestClientIP` 是 base.RemoteIP 逐字复制 | 17 | ~50 行 | 全部改用 `base.RemoteIP` |
| 13 | `sha256Hex` 3 份(authz_casdoor:290、cas_auth:527、ai_auth/aws.go:305);tencent_cloud_cls 另有 sha1Hex/hmacSHA1Hex | 3 | ~15 行 | 核心只是 `hex.EncodeToString` |
| 14 | `resolveLogFormat` 系 7 处同构(file_logger:386、http_logger:433、syslog:403、tcp_logger:319、loggly:347、loki_logger:478、splunk_hec_logging:347)+ `apisix/log/log.go:39 GetFields` 功能重叠 | 7 | ~250 行 | base 提供 log format 渲染 helper(`NestedLogMap`/`TruncateLogFormat` 可作宿主) |
| 15 | `requestVar`/nginx 变量解析 3+ 入口(proxy_cache:1605 手写 switch、graphql_limit_count:949、body_transformer:1248)+ base/apisix/variable/ctx 两层封装 | 4 | ~60 行 | 统一走 base |
| 16 | `logValues`(http.Header→map,小写 key)file_logger:452、loki_logger:444 几乎相同 | 2 | ~20 行 | 抽共享 |
| 17 | `headerValue`(response_rewrite:520)与 `requestHeaderValue`(expr/request.go:84)逐字相同 | 2 | ~15 行 | 抽共享 |
| 18 | `stringValue` 5 处(mocking:269、ai_stream/anthropic.go:347、ai_protocols/protocol.go:677、expr/expression.go:312、ctx/context.go:97)——4 处单行断言 | 5 | ~10 行 | 统一 helper |
| 19 | `numericUsage` 3 处(ai_protocols:699、ai_stream/sse.go:174、ai_rate_limiting:892)返回值 -1/0 有细微漂移 | 3 | ~30 行 | 合并(注意漂移) |
| 20 | `registerLLMRequestVars`/`registerStreamingLLMRequestVars`(ai_proxy:1086 vs ai_proxy_multi:1570)、`requestBodyOverride`/`hasProtocolRequestBodyOverride`(ai_proxy:651-675 vs ai_proxy_multi:1078-1094 逐行相同)、`apisixVarString`(prometheus:407、graphql_proxy_cache:596、splunk_hec:339)、`sameSiteMode`(cas_auth:511、saml_auth:761、openid_connect:1583)、`flatten`(otel:417 vs body_transformer:1062) | 2-3 | ~80 行 | 抽 `ai_common`/base |

### 结构性重复(非函数级)

- `pkg/plugin/init.go:124-250` 约 100+ case 巨型 `switch name` 手动登记(带 FIXME),可改 map 注册表
- 每个插件重复 `name`/`priority` 常量 + `Init()` 三行赋值——琐碎但统一,可接受
- schema 常量 115 处裸 JSON 字符串,MD5 全量对比发现 3 组完全相同:`{"type":"object","properties":{}}`(ai、gm、log_rotate)、`{"type":"object"}`(batch_requests、node_status、server_info)、`{}`(error_page、request_context);metadataSchema 2 对重复(http_logger/tcp_logger、loki_logger/splunk_hec_logging)——可抽公共 schema 片段
- 114 个插件 PostInit 的 `if x == "" { 置默认 }` 模式 ~131 处,无共享默认值 helper(可 `lo.Ternary`,低优先级)
- `base.WriteJSONMessage`(base/request.go:35)与 `dubbo_proxy/transport.go:453` 逐字相同

## 6. 造轮子检查

### 明确可替换(已依赖库提供)

| # | 位置 | 手写内容 | 替代方案 |
|---|---|---|---|
| W1 | `pkg/plugin/request_id/plugin.go:198-245` | 手写完整 UUIDv7(位布局、序列号、跨毫秒 sleep、crypto/rand 填充)~50 行 | `github.com/gofrs/uuid` 的 `NewV7()`(项目已依赖 v4.4.0,vendor 中 generator.go:91) |
| W2 | `pkg/plugin/request_id/plugin.go` 同文件 | 手写 KSUID + `encodeBase62` ~30 行 | 项目无 ksuid 依赖,可加 `segmentio/ksuid`;或确认 APISIX 兼容语义后按实现保留 |
| W3 | `pkg/plugin/request_id/plugin.go` `rangeID()` | `math/rand.Intn` + 固定 charset 随机串 | 已依赖 `matoous/go-nanoid/v2` |

### 有意保留(平权/业务取舍,应加注释说明)

- `pkg/plugin/ai_auth/aws.go:70-308` 手写 AWS SigV4 ~200 行,与 `signWithSDK`(:133,封装已依赖的 aws-sdk-go-v2 `v4.NewSigner`)双轨并存——APISIX 行为平权保留
- `pkg/store/plugin_metadata_cache.go:13-76` sync.Map + 逐 key RWMutex + 版本号手写缓存——last-good/version 语义有设计理由
- `pkg/plugin/limit_conn` 自研滑动窗口限流——业务逻辑;limit_count 用 ulime/limiter/v3、limit_req 用 Redis Lua,均合规
- `pkg/config/standalone.go:243` 直读 yaml——standalone 模式合法需求(带 `#END` 校验)

### 已确认无问题

- LRU 统一走 `pkg/cache/memory/lru.go` 包装 hashicorp/golang-lru/v2,无手写山寨版
- hex 79 处 `hex.EncodeToString`(仅 3 处 `fmt.Sprintf("%x")` 风格不一致,见 S13)
- base64 全走标准库;时间处理无手写格式化;配置加载未绕过 viper

## 7. 整改建议优先级

### P0(正确性/稳定性)

1. **M1/M2**:anthropic_openai.go:244、elasticsearch_logger.go:448 裸断言 → 双值断言(进程级 panic 风险)
2. **L2**:traffic_split / resource/route.go map→slice 排序(多实例 chash 选点漂移)
3. **L1**:删除 pkg/plugin/init.go:377 生产路径 `fmt.Println`

### P1(一致性与去重,高 ROI)

1. **J1/J2**:4 条 JSON 响应路径收敛到 `util.WriteJSON`;删除 `base.WriteJSONMessage` 手工拼 JSON(注意与 `BuildMessageResponse` 统一 escaping/Content-Type 语义)
2. **Top1**:抽 `pkg/plugin/cacheutil`(proxy_cache ↔ graphql_proxy_cache,~120 行)
3. **Top2**:抽 `pkg/plugin/luautil`(exit_transformer ↔ serverless,96 行逐字)
4. **Top4**:抽 `pkg/plugin/limitbase`(限流三件套,redisInt/值解析/正则/quota 头)
5. **Top8**:会话签名统一到 `base.SignSessionValue/VerifySessionValue`(cas_auth、saml_auth)
6. **Top12**:客户端 IP 提取统一 `base.RemoteIP`
7. **W1**:request_id 换 `gofrs/uuid.NewV7()`

### P2(热路径性能)

1. **S1-S6**:fmt.Sprint/Sprintf 数字转换 → strconv/type-switch(15+ 处每请求热路径)
2. **S2**:`base/logging.go:190` 每请求 regexp 编译 → 包级预编译
3. **S8**:host:port 统一 `net.JoinHostPort`(修 IPv6 隐患)
4. **S9**:acl split 正则缓存

### P3(风格/微优化,可分批)

- **L3-L11**:slices.Contains / slices.Equal / lo.Uniq / slices.SortFunc 替换
- **S10-S13、S7**:zipkin hex 校验、data_mask 重复 ParseQuery、hex 统一、brotli/gzip 合并
- **M3-M6**:取值风格统一;M4/M5 的 fmt.Sprint → 类型断言
- **Top5-Top20**:transport 构建器、writeJSON、resolveValue、log format 渲染等公共逻辑上收
- init.go switch → map 注册表;schema 公共片段常量

### 整改落地要求

- 每个改动保持"一处修复、双处同步"意识:成对克隆插件(proxy_cache/graphql_proxy_cache、exit_transformer/serverless、dubbo_proxy/http_dubbo、limit_count/graphql_limit_count)改一个必须改另一个
- 修改前先写回归测试锁定行为(尤其 escaping 语义、chash 选点顺序、UUIDv7 格式兼容)
- 本文档随整改更新,完成后归档

---

## 整改台账 (Remediation Ledger) — 2026-08-08

> 执行依据:`docs/superpowers/plans/2026-08-07-codebase-consistency-remediation.md`
> 与 `docs/superpowers/specs/2026-08-07-codebase-consistency-remediation-design.md`。
> 全部 60 项命名发现 + 无编号结构性观察,每项只记录一次。
> PR 状态以 2026-08-08 为准:单元 0–5 已合入 master(`ac56390`);
> 单元 6–11 的 PR 已推送待合入(head SHA 见下表,合入后状态自动翻转)。

### 汇总

| 处置 | 数量 | 说明 |
|---|---|---|
| 已修复(合入 master) | 26 | J1-J3,J6,S1-S3,S13,L1,L2,L4,L6-L8,Top1,Top4,Top6,Top8,Top11,Top13,Top14,Top16 + W 类 0 |
| 已修复(PR 待合入) | 23 | J4,S4-S9,S11,S12,L3,L5,L9-L11,M1,M2,M4,Top2,Top3,Top5,Top7,Top9,Top10,Top12,Top17-Top20 |
| 无代码处置(记录/保留) | 11 | J5,S10,M3(部分),M5,M6,Top15(部分),W1,W2,W3 + 结构性观察 3 项 |
| 合计 | 60 | 见下表逐项 |

### J 系列(JSON 处理)

| # | 原位置 | 处置 | PR / 证据 |
|---|---|---|---|
| J1 | base/request.go WriteJSONMessage | 已修复:统一 `util.WriteJSONMessage` | PR #35(单元 1,合入 `43f087a`) |
| J2 | 4 条 JSON 响应路径 | 已修复:收敛 `util.WriteJSON`/`util.WriteJSONMessage` | PR #35(单元 1,合入 `43f087a`) |
| J3 | logger encodeBatch/originLogEntries | 已修复:`base/log_batch.go` 共享批编码 | PR #38(单元 3,合入 `489162b`) |
| J4 | store/getter.go 加密路径 | 已修复:`util.Parse(metadata, v)` | PR #50(单元 10a,head `6c66c00`) |
| J5 | Decoder vs ReadAll 混用 | 无代码:16 处 Decoder 与 2 处受限 ReadAll 各有理由(consumer_secret 1MiB 限制、ai_rag 通用 helper) | 记录 |
| J6 | logger batch 编码 | 已修复:同 J3 | PR #38(单元 3,合入 `489162b`) |

### S 系列(字符串处理)

| # | 原位置 | 处置 | PR / 证据 |
|---|---|---|---|
| S1 | base/logging.go matchCondition | 已修复:string 快路径免分配 | PR #38(单元 3,合入 `489162b`) |
| S2 | base/logging.go 正则每请求编译 | 已修复:配置期预编译 | PR #38(单元 3,合入 `489162b`) |
| S3 | metrics/prometheus.go status label | 已修复:`strconv.Itoa` | PR #38(单元 3,合入 `489162b`) |
| S4 | ai_proxy/ai_proxy_multi upstream_status | 已修复:`strconv.Itoa` | PR #46(单元 7,head `1c361ae`) |
| S5 | 5 处 Content-Length `fmt.Sprint` | 已修复:`strconv.Itoa` | PR #50(单元 10a,head `6c66c00`) |
| S6 | 时间戳/Age `fmt.Sprintf("%d")` | 已修复:`strconv.FormatInt` | PR #50(单元 10a,head `6c66c00`) |
| S7 | brotli/gzip 协议版本 | 已修复:`base.ProtocolVersion` 共享 | PR #50(单元 10a,head `6c66c00`) |
| S8 | host:port 三种写法(18 处) | 已修复:统一 `net.JoinHostPort(host, strconv.Itoa(port))` | PR #45(单元 6,head `8bee5ec`) |
| S9 | acl 每请求 regexp.Compile | 已修复:按 separator 缓存编译结果 | PR #50(单元 10a,head `6c66c00`) |
| S10 | zipkin hex 校验 | 无代码:保留零分配固定长度校验(位置语义等价,`hex.DecodeString` 需先分配) | 记录 |
| S11 | data_mask 两次 url.ParseQuery | 已修复:复用解析结果 | PR #50(单元 10a,head `6c66c00`) |
| S12 | ai_stream usage.Text 累加 | 已修复:runner 基线 537,200 B/op 超过声明的 2× 阈值 → `Usage.AppendText`(strings.Builder),诊断 45,328 B/op(-92%) | PR #46(单元 7,head `1c361ae`;基线 `.cache/bench/baseline.txt`) |
| S13 | `%x` sha256 格式化 | 已修复:`hex.EncodeToString`/`base.Sha256Hex`(keycloak:397 的 `verified:file:%x` 指纹标签不在本项范围,保留) | PR #41(单元 5,合入 `4871c9b`) |
| S14 | server.go vs config/types.go 监听地址 | 已修复:两处同构拼接统一 `strconv.Itoa` | PR #45(单元 6,head `8bee5ec`) |

### L 系列(列表/循环)

| # | 原位置 | 处置 | PR / 证据 |
|---|---|---|---|
| L1 | init.go fmt.Println 调试输出 | 已修复:删除 | PR #30(单元 0,合入 `e4a1d85`) |
| L2 | traffic_split map→slice 顺序 | 已修复:按 key 排序 | PR #30(单元 0,合入 `e4a1d85`) |
| L3 | []any 手写线性查找(2 处) | 已修复:`slices.ContainsFunc`([]any 元素不可比较,不能用 Contains) | PR #52(单元 10b,head `0ca0e24`) |
| L4 | sameStringSlice 双份 | 已修复:`slices.Equal` | PR #36(单元 2,合入 `538ae92`) |
| L5 | containsStatus 双份包装 | 已修复:删除包装,直用 `slices.Contains` | PR #52(单元 10b,head `0ca0e24`) |
| L6 | cacheControlValueDirective 不 break | 已修复:命中即 break | PR #36(单元 2,合入 `538ae92`) |
| L7 | parseVaryHeader 双份 | 已修复:`cacheutil.ParseVaryHeader` 共享 | PR #36(单元 2,合入 `538ae92`) |
| L8 | uniqueStrings 手写去重 | 已修复:`lo.Uniq`(顺序稳定性由 `TestDrainQueueDeduplicatesStably` 锁定) | PR #40(单元 4,合入 `81375f4`) |
| L9 | server.go prometheus 线性扫描 | 已修复:`slices.Contains` 包裹 + 重复名测试 | PR #52(单元 10b,head `0ca0e24`) |
| L10 | proxy_rewrite 重复索引 | 已修复:缓存 pattern/replacement 局部变量 | PR #52(单元 10b,head `0ca0e24`) |
| L11 | sort.Slice 重复索引(4 处) | 已修复:`slices.SortFunc`/`SortStableFunc` + `cmp.Compare` | PR #52(单元 10b,head `0ca0e24`) |

### M 系列(Map 处理)

| # | 原位置 | 处置 | PR / 证据 |
|---|---|---|---|
| M1 | anthropic_openai.go 链式裸断言 | 已修复:命名 map + 本地构造 content part 提取测试(语义不变,不宣称修复外部输入 panic) | PR #51(单元 10c,head `569415a`) |
| M2 | elasticsearch_logger 链式裸断言 | 已修复:命名 map(indexAction) | PR #51(单元 10c,head `569415a`) |
| M3 | 三种取值风格并存 | 部分:共享安全 getter 落位 `ai_protocols.StringValue`(单元 7,head `1c361ae`);AI 族 ProviderConf 字符串取值站点待其合入后迁移;缺省/错误类型/空值语义完全一致 | PR #46(单元 7)+ 记录 |
| M4 | wolf_rbac fmt.Sprint 身份字段 | 已修复:标量类型断言 + 非标量明确报错;nil 字段输出空串(替代原 `<nil>` 产物) | PR #51(单元 10c,head `569415a`) |
| M5 | otel/body_transformer/lago map 取值 | 无代码:字符串化边界为有意取舍(日志字段要求任意值可序列化) | 记录 |
| M6 | server.go 断言与 cast 混用 | 无代码:comma-ok 断言与 cast.ToInt 语义不同,各自场景正确 | 记录 |

### Top 榜单(重复定义)

| # | 模式 | 处置 | PR / 证据 |
|---|---|---|---|
| Top1 | proxy_cache 缓存族 | 已修复:`pkg/plugin/cacheutil` | PR #36(单元 2,合入 `538ae92`) |
| Top2 | Lua 转换 4 函数逐字节 | 已修复:`pkg/plugin/luautil` | PR #47(单元 8,head `ec3cd42`) |
| Top3 | dubbo 传输层 | 已修复:`pkg/plugin/dubbo` 共享骨架(两实现实为不同协议方言:Hessian2 vs 文本行,帧/解码以 adapter 留在各自插件) | PR #48(单元 9,head `2a21ffa`) |
| Top4 | 限流三件套 | 已修复:`pkg/plugin/limitbase`(redisInt/正则/quota 头;值解析因 int/int64/allowZero/错误文案差异保留本地) | PR #40(单元 4,合入 `81375f4`) |
| Top5 | transport() 6 份 | 已修复:`ai_common.ApplyTransportKeepalive/ApplyTransportSSLVerify`(ai_proxy 的 DisableCompression 差异保留) | PR #46(单元 7,head `1c361ae`) |
| Top6 | writeJSON 6 处 + 手写 message | 已修复:同 J1/J2 | PR #35(单元 1,合入 `43f087a`) |
| Top7 | resolveValue 插值 6 处 | 已修复:`base.ResolveRequestVariables`(forward_auth 的 `$post_arg.` 扩展正则保留本地) | PR #45(单元 6,head `8bee5ec`) |
| Top8 | 会话签名 3 套 | 已修复:`base.SignRawSessionValue`/`VerifyRawSessionValue`(CAS/SAML 迁移,wire 格式不变;OAuth 编码格式不动) | PR #41(单元 5,合入 `4871c9b`) |
| Top9 | readBody 3 处 | 已修复:`base.ReadRequestBodyLimited`(GraphQL 两插件迁移;data_mask 无恢复语义,不同契约,保留) | PR #45(单元 6,head `8bee5ec`) |
| Top10 | requestSize 3 处逐字 | 已修复:`util.RequestSize` | PR #45(单元 6,head `8bee5ec`) |
| Top11 | callbackPath 3 处逐字 | 已修复:`base.CallbackPath` | PR #41(单元 5,合入 `4871c9b`) |
| Top12 | 客户端 IP 内联 17 处 | 已修复(兼容子集):sls_logger/opa/proxy_cache/expr 的 remote_addr 改用 `base.RemoteIP`;ip_restriction(空值回退语义不同)、zipkin(需 port)拒绝迁移 | PR #45(单元 6,head `8bee5ec`) |
| Top13 | sha256Hex 3 份 | 已修复:字符串版合入 `base.Sha256Hex`;ai_auth/aws.go 字节版(签名不同)保留 | PR #41(单元 5,合入 `4871c9b`) |
| Top14 | resolveLogFormat 系 7 处 | 已修复:`base` log format 渲染共享(见 J3) | PR #38(单元 3,合入 `489162b`) |
| Top15 | requestVar/nginx 变量 3+ 入口 | 部分:remote_addr 子路径统一 `base.RemoteIP`;proxy_cache 手写 switch、graphql_limit_count 解析器、expr 引擎的 context 覆盖/变量命名语义各异,合并会改变行为 → 保留本地 | PR #45(单元 6,head `8bee5ec`)+ 记录 |
| Top16 | logValues 小写 header map | 已修复:共享(见 J3) | PR #38(单元 3,合入 `489162b`) |
| Top17 | headerValue 双份 | 已修复:`expr.HeaderValue` 共享 | PR #45(单元 6,head `8bee5ec`) |
| Top18 | stringValue 5 处 | 已修复(AI 族 2 处):`ai_protocols.StringValue`;mocking/expr/ctx 为非本单元文件,记录 | PR #46(单元 7,head `1c361ae`) |
| Top19 | numericUsage 3 处(取整/哨兵漂移) | 已修复:`ai_protocols.NumericUsage(value, round)` 显式取整模式;sse 截断、protocols/rate-limiting 四舍五入,哨兵 -1 统一(rate-limiting 的 0 哨兵经 `<0`/`>0` 判断等价) | PR #46(单元 7,head `1c361ae`) |
| Top20 | LLM 变量注册/body override 等 | 已修复:`ai_protocols.RegisterLLMRequestVars`/`RegisterUsageContextDocumentVars`、`ai_stream.RegisterStreamingLLMRequestVars`、`ai_common.HasProtocolRequestBodyOverride`;ai_proxy 的 `$llm_has_tool_calls`/metadata 文档变量为本地扩展保留;helper 落位 ai_protocols/ai_stream 而非 ai_common 系 import 环约束,见 PR #46 说明 | PR #46(单元 7,head `1c361ae`) |

### W 系列(造轮子)

| # | 位置 | 处置 | PR / 证据 |
|---|---|---|---|
| W1 | 手写 UUIDv7 | 无代码:保留实现,新增源码注释说明 `gofrs/uuid.NewV7` 不兼容原因(不暴露 22-bit 单调序列与回拨/溢出处理);单调/回拨/溢出/格式测试保留 | PR #53(单元 11,head `521cb38`) |
| W2 | 手写 KSUID | 无代码:保留依赖无关实现;新增长度 27/字母表/时间戳前缀/唯一性/确定性编码测试 | PR #53(单元 11,head `521cb38`) |
| W3 | rangeID math/rand | 无代码:schema(char_set minLength 6、length minimum 6)+ PostInit 默认值保证配置路径无法把生成错误送达 handler,`gonanoid.Generate` 的错误返回不可达,不满足替换条件;新增自定义字母表/长度/默认化契约测试 | PR #53(单元 11,head `521cb38`) |

### 无编号结构性观察

| 观察 | 处置 |
|---|---|
| init.go 巨型构造函数 switch | 已修复:`pluginRegistry map[string]func() Plugin`(115 key 与基线机械比对一致;别名 otel→opentelemetry、request-context→request_context 保留;serverless 工厂构造保留)— PR #49(单元 10d,head `f2db5f8`) |
| 每插件 name/priority 常量 + Init 三行赋值 | 无代码:琐碎且统一,接受 |
| schema 裸 JSON 重复(3 组相同 + 2 对 metadataSchema) | 无代码:115 处常量各自内联,抽取公共片段收益低且破坏插件自包含性,记录 |
| 114 插件 PostInit 默认值模式 ~131 处 | 无代码:单行 `if x == ""` 模式,`lo.Ternary` 收益有限,记录 |
| base.WriteJSONMessage 与 dubbo_proxy writeDubboError 逐字相同 | 已修复:dubbo 共享包统一 `dubbo.WriteError`(经 util.WriteJSONMessage)— PR #48(单元 9,head `2a21ffa`) |

### 合入状态(2026-08-08 全部合入)

全部 10 个单元 PR 已合入 master(`eee869e`):#45(单元 6)、#46(单元 7)、#47(单元 8)、#48(单元 9)、#49(单元 10d)、#50(单元 10a)、#51(单元 10c)、#52(单元 10b)、#53(单元 11)、#54(单元 12)。此前各 PR 的 head SHA 见对应 PR 记录。

### 合入后修复(2026-08-08)

| 问题 | 修复 |
|---|---|
| 合并产物:`pkg/plugin/base/request.go` 丢失 `fmt` import(单元 6 新增 `ReadRequestBodyLimited` 使用 `fmt.Errorf`,单元 10a 移除了 import;合入后 master 无法编译) | PR #56(与并行分支 `2c01168` 同款一行修复,先合先得) |
| 合并产物:`pkg/server/server.go` `startPrometheusExportServer` 重复 `return nil`(单元 10b,不可达代码) | PR #56 |
| mcp_bridge SSE flake(`unexpected EOF while reading SSE event`,CI 偶发,~1/600):会话收割 goroutine 在 stdout/stderr scanner 读完管道前调用 `cmd.Wait()`,管道关闭丢弃了快退出子进程的缓冲输出 | PR #56:先 `wg.Wait()` 再 `cmd.Wait()`;会话 ctx 与请求 ctx 解耦;请求 ctx 取消时排空缓冲事件;新增 200 次迭代回归测试(负载下 3000 次迭代验证通过) |

### 已知既有失败(非本次整改引入,基线上复现)

| 失败 | 复现证据 |
|---|---|
| t/plugin limit-conn 6 个 redis/real-ip/forwarded-for 用例(日志格式断言与 harness 期望不符) | 基线与分支一致失败(单元 4 记录) |
| t/plugin api-breaker `exponential-backoff-is-capped` | 基线与分支一致失败(单元 6 记录) |
| t/plugin exit-transformer `invalid-function-is-contained` | 基线与分支一致失败(单元 8 记录) |
| t/plugin traffic-split unreachable-node 重试用例 | 基线与分支一致失败(单元 10d 记录) |
| pkg/plugin/limit_count `TestDelayedSyncFlushRetriesDroppedStateWithoutAnotherRequest` 间歇性失败 | flushKeys 按 map 迭代顺序执行,基线上 6 次运行失败 1 次(单元 6 记录) |

### 最终验证说明

按计划约束未运行 `go test ./...` / `make test` 全量门禁;每个单元按其交付记录运行了变更包测试、受影响 manifest(-parallel=1)、`golangci-lint run --new-from-rev=origin/master`(0 issues)与 `make build`。单元 6–11 的 head SHA 验证记录见对应 PR 描述;合入 master 后如需对合流结果做一次增量 lint/build 冒烟,可依据各 PR 的验证记录执行。
