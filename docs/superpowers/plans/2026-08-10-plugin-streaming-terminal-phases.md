# Plugin Streaming and Terminal Phases Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give streaming, compression, protocol translation, hijack, and terminal-response plugins explicit ownership so they coexist with request and buffered response phases without double writes, hidden buffering, or post-commit recovery corruption.

**Architecture:** Materialized plugin bindings declare bounded response capabilities. The builder computes one immutable response plan: buffered phases, a streaming wrapper stack, and at most one exclusive protocol owner. Streaming wrappers are installed in APISIX header/body order while preserving optional writer interfaces and flush/trailer semantics. Conditional response plugins remain in the request pipeline at their declared scope/stage/priority; only a route or plugin protocol owner returns `Responded` or `Hijacked`. Incompatible protocol owners fail strict route build with plugin names and frozen resource provenance.

**Tech Stack:** Go 1.26 `net/http`, response controller/optional interfaces, compression negotiation from PR #84, buffered response phases, outer panic/outcome boundary.

## Global Constraints

- Depends on the merged buffered response phases.
- Never buffer an unbounded SSE, grpc-web, websocket, hijacked, chunked, or explicitly streaming response.
- One request has exactly one winning terminal disposition. Multiple conditional terminal-capable plugins are legal and execute by scope/request-stage/priority until one responds; upstream runs only if all continue. Cache hit, configured response, upstream proxy, and successful hijack remain mutually exclusive outcomes.
- A wrapper may expose `Flusher`, `Hijacker`, `ReaderFrom`, `Pusher`, `CloseNotifier`, full-duplex, or deadlines only when its downstream writer exposes the capability.
- Final `101/204/304`, HEAD, informational `1xx`, trailers, repeated headers, `Vary`, content negotiation, and derived-header invalidation retain the representation PR contract.
- Post-commit, post-flush, or post-hijack panic is handled only by the outer boundary: close the retained hijacked connection when applicable, run finalizers, and abort with `http.ErrAbortHandler`. No streaming plugin writes a fallback body after commit.
- Compression is negotiated once across gzip/deflate/br/identity. Stacked compression plugins must not independently choose or double-encode.
- Builder errors name both incompatible plugin capabilities and the materialized route/global/consumer provenance.
- Do not migrate loggers/tracers in this PR. The request lifecycle still guarantees their legacy cleanup until the final phase plan.
- The exact 22 HTTP identities and capabilities are the Plan 16 section of `2026-08-10-plugin-capability-manifest.md`; `mqtt-proxy` is the separately classified 23rd identity and is never installed in the HTTP executor.

---

### Task 1: Define response capabilities and build-time compatibility

**Files:**
- Create: `pkg/plugin/response_capability.go`
- Create: `pkg/plugin/response_capability_test.go`
- Modify: `pkg/plugin/request_stage_registry.go`
- Modify: `pkg/plugin/request_stage_registry_test.go`
- Modify: `pkg/plugin/init_test.go`

**Interfaces:**

```go
type ResponseModeMask uint8
const (
    ResponseModeBounded ResponseModeMask = 1 << iota
    ResponseModeStreaming
    ResponseModeHijack
)
type ResponseModeDescriptor struct { Modes ResponseModeMask }
type ResponseModeDescriber interface {
    DescribeResponseMode() (ResponseModeDescriptor, error)
}
type StreamingResponseState struct {
    Status int
    Header, Trailer http.Header
}
type StreamingHeaderFilterPlugin interface {
    RunStreamingHeaderFilter(*http.Request, *StreamingResponseState) error
}
type StreamingBodyFilterPlugin interface {
    WrapStreamingResponse(http.ResponseWriter, *http.Request) (http.ResponseWriter, error)
}
type ProtocolDisposition uint8
const (
    ProtocolResponded ProtocolDisposition = iota + 1
    ProtocolHijacked
)
type ExclusiveProtocolTerminal interface {
    RunExclusiveProtocol(http.ResponseWriter, *http.Request, http.Handler) (ProtocolDisposition, *http.Request, apisixctx.ResponseSource, error)
}
```

`ResponseCapability` is the root capability record with explicit header,
buffered-body, streaming-body, streaming-owner, compression-offer,
exclusive-protocol, and separate-subsystem fields. `ProtocolKind` is a bounded
registry (`ai`, `grpc-web`, `kafka`, `dubbo`, `http-dubbo`,
`mqtt`); arbitrary config values are not accepted.

- [ ] **Step 1: Add compatibility matrix tests**

Cover buffered+buffered, gzip+brotli shared negotiation, grpc-web+compression, multiple conditional terminals followed by upstream, two exclusive protocol owners, buffered+SSE, cache+websocket, and hijack+body transform. Multiple conditional terminals must build successfully; every actual rejection asserts both plugin names and `ResourceProvenance`. Add `key-auth + mocking`: key-auth runs once at access, mocking runs once at before-proxy only if auth continues, and neither identity may appear in both request and terminal executors.

- [ ] **Step 2: Run focused tests and record compile-red**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "^TestBuildResponsePlan|^TestResponseCapability" -count=1'
```

- [ ] **Step 3: Implement the exact matrix and completeness gate**

Use a static registry for legacy capability declarations until each plugin implements the interface. Do not infer from Handler type assertions. The exact conditional-terminal stage table lives in `2026-08-10-plugin-capability-manifest.md`; the stage registry rejects missing, duplicate, or extra declarations. An identity already owned by `RequestPhasePlugin` executes only there and is never installed again as `TerminalPlugin`; the latter is only for terminal-only and route-owned protocol owners. A registry test enumerates every plugin that wraps a response writer, flushes, hijacks, buffers, compresses, or terminates and requires an explicit capability.

- [ ] **Step 4: Run plugin tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin -run "(BuildResponsePlan|ResponseCapability|RegistryCompleteness)" -count=1'
```

### Task 2: Implement capability-preserving streaming wrappers

**Files:**
- Create: `pkg/plugin/base/streaming_phase.go`
- Create: `pkg/plugin/base/streaming_phase_test.go`
- Create: `pkg/plugin/streaming_executor.go`
- Create: `pkg/plugin/streaming_executor_test.go`

**Interfaces:**

The executor uses `StreamingBodyFilterPlugin` and `ExclusiveProtocolTerminal`
above. The normal-upstream continuation is passed to protocol owners so
grpc-web/transcode can frame it while terminal-only owners can ignore it.

The executor receives full `Binding` values so every response wrapper retains `Scope`, effective priority, and provenance. Route-owned and plugin-owned protocol candidates also retain frozen provenance. The executor updates the lifecycle final request and response source after every protocol result.

- [ ] **Step 1: Add writer and phase-order regressions**

Use real connections where required. Cover `103 -> 200`, flush-first streaming, trailers, `io.Copy`/ReaderFrom, successful and failed hijack, full-duplex capability parity, terminal stop, terminal error before commit, and panic after flush/hijack.

- [ ] **Step 2: Run tests and capture red failures**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin -run "(StreamingPhase|StreamingExecutor|TerminalPlugin)" -count=1'
```

- [ ] **Step 3: Implement wrappers with exact capability preservation**

Use the existing `httpsnoop`/ResponseController-compatible patterns. Build wrapper order from explicit scope and phase priority, not legacy middleware unwind. Conditional response plugins stay in their declared request stage and are never duplicated in the response executor. An exclusive protocol owner either responds or hijacks; the ordinary continuation reaches upstream once when no exclusive owner is selected. Unknown dispositions fail closed before commit.

- [ ] **Step 4: Run focused race and real-connection tests**

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/base ./pkg/plugin -run "(StreamingPhase|StreamingExecutor|TerminalPlugin)" -count=3'
```

### Task 3: Consolidate compression and protocol streaming owners

**Files:**
- Modify: `pkg/plugin/gzip/plugin.go`
- Modify: `pkg/plugin/gzip/plugin_test.go`
- Modify: `pkg/plugin/brotli/plugin.go`
- Modify: `pkg/plugin/brotli/plugin_test.go`
- Modify: `pkg/plugin/grpc_web/plugin.go`
- Modify: `pkg/plugin/grpc_web/plugin_test.go`
- Modify: `pkg/plugin/grpc_transcode/plugin.go`
- Modify: `pkg/plugin/grpc_transcode/plugin_test.go`
- Modify: `pkg/plugin/cors/plugin.go`
- Modify: `pkg/plugin/cors/plugin_test.go`

- [ ] **Step 1: Add combined negotiation and streaming regressions**

Cover one Accept-Encoding negotiation for gzip/deflate/br/identity; pre-encoded pass-through; identity forbidden; cap fallback; `101/204/304`; grpc-web binary/text framing, flush and final trailers; grpc-transcode request/response translation; CORS preflight terminal response and actual streaming headers.

- [ ] **Step 2: Run package tests before production edits**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{gzip,brotli,grpc_web,grpc_transcode,cors} -run "(Streaming|Negotiation|Trailer|Preflight|Bodyless)" -count=1'
```

- [ ] **Step 3: Move response ownership into streaming interfaces**

Reuse the shared compression negotiation package; do not reintroduce plugin-specific parsers. Request decoding remains request-phase behavior. CORS preflight implements terminal response; actual-response headers implement header filter and do not wrap the body.

- [ ] **Step 4: Run complete affected packages and focused races**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{gzip,brotli,grpc_web,grpc_transcode,cors} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/{gzip,brotli,grpc_web} -run "(Streaming|Flush|Trailer|Hijack|Negotiation)" -count=3'
```

### Task 4: Migrate buffering, AI streaming, and hijack owners

**Files:**
- Modify: `pkg/plugin/proxy_buffering/plugin.go`
- Modify: `pkg/plugin/proxy_buffering/plugin_test.go`
- Modify: `pkg/plugin/ai_rate_limiting/plugin.go`
- Modify: `pkg/plugin/ai_rate_limiting/plugin_test.go`
- Modify: `pkg/plugin/ai_aliyun_content_moderation/plugin.go`
- Modify: `pkg/plugin/ai_aliyun_content_moderation/plugin_test.go`
- Modify: `pkg/plugin/ai_proxy/plugin.go`
- Modify: `pkg/plugin/ai_proxy/plugin_test.go`
- Modify: `pkg/plugin/ai_proxy_multi/plugin.go`
- Modify: `pkg/plugin/ai_proxy_multi/plugin_test.go`
- Modify: `pkg/plugin/kafka_proxy/plugin.go`
- Modify: `pkg/plugin/kafka_proxy/plugin_test.go`

- [ ] **Step 1: Add bounded/streaming mode regressions**

Cover proxy-buffering cap and spill/pass-through decision; AI proxy/multi and moderation SSE first chunk, final packet, invalid event and moderation denial; rate-limit accounting on streaming completion/abort; kafka proxy upgrade success/failure and no second response after hijack.

- [ ] **Step 2: Run focused package tests before edits**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{proxy_buffering,ai_rate_limiting,ai_aliyun_content_moderation,ai_proxy,ai_proxy_multi,kafka_proxy} -run "(Streaming|SSE|Hijack|Buffer|Abort)" -count=1'
```

- [ ] **Step 3: Implement explicit capability owners**

Every AI streaming parser validates protocol event shape before forwarding. Terminal moderation denial before commit returns its configured response; after any forwarded byte, failure aborts and records the existing bounded safety metric. Kafka successful hijack returns `TerminalHijacked` and hands the connection to the outer outcome owner.

- [ ] **Step 4: Run complete packages and real-socket races**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{proxy_buffering,ai_rate_limiting,ai_aliyun_content_moderation,ai_proxy,ai_proxy_multi,kafka_proxy} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/{ai_aliyun_content_moderation,ai_proxy,ai_proxy_multi,kafka_proxy} -run "(Streaming|Hijack|Abort)" -count=3'
```

### Task 5: Migrate terminal response and FaaS owners

**Files:**
- Modify: `pkg/plugin/mocking/plugin.go`
- Modify: `pkg/plugin/mocking/plugin_test.go`
- Modify: `pkg/plugin/redirect/plugin.go`
- Modify: `pkg/plugin/redirect/plugin_test.go`
- Modify: `pkg/plugin/fault_injection/plugin.go`
- Modify: `pkg/plugin/fault_injection/plugin_test.go`
- Modify: `pkg/plugin/aws_lambda/plugin.go`
- Modify: `pkg/plugin/aws_lambda/plugin_test.go`
- Modify: `pkg/plugin/azure_functions/plugin.go`
- Modify: `pkg/plugin/azure_functions/plugin_test.go`
- Modify: `pkg/plugin/openwhisk/plugin.go`
- Modify: `pkg/plugin/openwhisk/plugin_test.go`
- Modify: `pkg/plugin/openfunction/plugin.go`
- Modify: `pkg/plugin/openfunction/plugin_test.go`
- Modify: `pkg/plugin/mcp_bridge/plugin.go`
- Modify: `pkg/plugin/mcp_bridge/plugin_test.go`
- Modify: `pkg/plugin/public_api/plugin.go`
- Modify: `pkg/plugin/public_api/plugin_test.go`

- [ ] **Step 1: Add exactly-once terminal tests**

Assert each success/deny/error disposition calls either configured terminal logic or upstream, never both; response phases run once; cancellation and panic propagate through the outer lifecycle. Include MCP SSE first flush and public-api direct dispatch.

- [ ] **Step 2: Implement terminal interfaces without changing payload semantics**

Keep transport/timeouts in their existing owners. This task changes ownership and continuation only; the separate FaaS timeout plan remains authoritative for progress/idle policy.

- [ ] **Step 3: Run terminal package tests**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/{mocking,redirect,fault_injection,aws_lambda,azure_functions,openwhisk,openfunction,mcp_bridge,public_api} -count=1'
```

### Task 6: Integrate response plans in route generations

**Files:**
- Modify: `pkg/route/builder.go`
- Create: `pkg/route/streaming_phase_test.go`
- Modify: `pkg/route/buffered_response_phase_test.go`

- [ ] **Step 1: Add strict build and runtime combinations**

Cover permitted buffered/compression, grpc-web streaming, CORS preflight, AI/MCP SSE, websocket/hijack, terminal mocking/public-api/FaaS, and rejected incompatible pairs with exact provenance. Add route-owned terminal tests for kafka-proxy websocket, dubbo-proxy, and http-dubbo so the response plan cannot miss owners absent from plugin Handler. Assert no migrated plugin remains in the legacy response remainder.

- [ ] **Step 2: Build and install exactly one response plan**

The route builder validates capabilities before publishing a handler. Dynamic reload preserves the last-good generation on incompatibility via the existing reload error path.

- [ ] **Step 3: Run route tests**

```bash
bash -lc 'source .envrc && go test ./pkg/route -run "(StreamingPhase|BufferedResponsePhase|ResponsePlan|DisabledPluginReload)" -count=1'
bash -lc 'source .envrc && go test ./pkg/route -count=1'
```

### Task 7: Verification, review, and independent PR delivery

- [ ] **Step 1: Run affected-package and race gates**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin ./pkg/route ./pkg/plugin/{gzip,brotli,grpc_web,grpc_transcode,cors,proxy_buffering,ai_rate_limiting,ai_aliyun_content_moderation,ai_proxy,ai_proxy_multi,kafka_proxy,mcp_bridge,public_api,dubbo_proxy,http_dubbo,mocking,redirect,fault_injection,aws_lambda,azure_functions,openwhisk,openfunction} -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin ./pkg/route ./pkg/plugin/{grpc_web,ai_aliyun_content_moderation,kafka_proxy} -run "(Streaming|Terminal|Hijack|Flush|ResponsePlan|Abort)" -count=3'
```

- [ ] **Step 2: Run capability/duplicate-owner scan**

```bash
rg -n 'ResponseCapability|StreamingBodyFilterPlugin|TerminalPlugin|\.Flush\(|Hijack\(' pkg/plugin pkg/route
```

- [ ] **Step 3: Run scoped lint/build/diff gates**

```bash
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/... ./pkg/route/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 4: Independent review and delivery**

Review must verify build-time compatibility, one terminal owner, capability parity, streaming first-byte behavior, negotiation, trailers, hijack transfer, panic abort, and absence of duplicate legacy work. After approval, commit:

```bash
git commit -m "refactor(plugin): execute streaming and terminal phases"
```

Open one ready PR, wait for CI, and merge before log/finalizer phases.

## Fast-plan-impl Dispatch Ownership

1. **WU-01 core capability/executors** owns `pkg/plugin/response_capability*`, `pkg/plugin/request_stage_registry*`, `pkg/plugin/base/streaming_phase*`, `pkg/plugin/streaming_executor*`, registry tests, and these two plan documents; it freezes the matrix and interfaces first.
2. **WU-02A compression and protocol wrappers** owns compression, gzip, brotli, grpc-web, grpc-transcode, CORS, and proxy-buffering directories.
3. **WU-02B AI owners** owns AI runtime/stream, proxy/multi, rate limiting, and Aliyun moderation directories.
4. **WU-02C conditional/local owners** owns FaaS, MCP, public API, mocking, redirect, fault injection, and Dubbo/Kafka request-preparation directories. WU-02A/B/C start only after WU-01 acceptance and have disjoint write paths.
5. **WU-03 route integration** owns the named `pkg/route/**` files and starts only after all WU-02 units are accepted. It installs one response plan and supplies the route-owned Kafka/Dubbo/http-Dubbo protocol candidates.

## Explicit Deferrals

- Log-phase ordering, logger/tracer panic isolation, and final registry closure.
- FaaS progress/idle timeout policy and AI provider parity beyond phase ownership.
- Process-wide body/task resource limits not already required by the response plan contract.
