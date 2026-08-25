# Immutable Task 11 Request and Connection Concurrency Ownership Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Use `superpowers:test-driven-development` for each behavior change and `superpowers:verification-before-completion` before each commit.

**Goal:** Move every request- or connection-owned child goroutine in the selected bridge and plugin paths under `runtime.RequestTaskGroup`, so its owner joins all accepted children before returning, preserves Task 10 panic identity/classification, and retains generation dispatch leases through externally visible timeouts.

**Architecture:** Contract C Task 1 supplies the unchanged `NewRequestTaskGroup`, `Go`, and `Wait` method set with first-raw-panic caching and join-before-repanic semantics. Protocol owners construct one group from the exact request or connection context, admit all child work through it, and call `Wait` after cleanup but before the request/connection owner releases its lifecycle or generation lease. Result channels carry completion state, never recovered panic values; an incomplete child result drives the owner to `Wait`, which re-panics the original raw value only after siblings finish.

**Tech Stack:** Go 1.26 standard library (`context`, `errors`, `io`, `net`, `net/http`, `sync`, `time`), existing `runtime.RequestTaskGroup`, request lifecycle finalizers, generation dispatch leases, focused/race Go tests, golangci-lint, and repository build targets.

**Spec:** Task 11 in `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`; Contract C in `docs/superpowers/plans/2026-08-26-immutable-task11-runtime-task-contract-integration.md`; Task 10 batch fatality contract in `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`.

## Global Constraints

- Start from the reviewed, integrated Contract C Task 1 head, not bare base `b0220dcebd64a1d2d687be84d1f14ab501dfffd0`. Contract C owns `pkg/runtime/request_tasks.go`, its tests, and the repository-wide goroutine AST gate.
- Consume `runtime.NewRequestTaskGroup(parent, owner)`, `(*RequestTaskGroup).Go`, and `(*RequestTaskGroup).Wait` unchanged. Do not add `Cancel`, `WaitContext`, `Active`, `Residuals`, a panic channel, or a second task-group abstraction.
- Rely on Contract C's accepted semantics: cache the first raw panic value by identity, close admission when `Wait` begins, join every accepted child, and only then re-panic that same value. With no panic, `Wait` returns the existing `errors.Join` result.
- Do not modify `pkg/runtime/goroutine_contract_test.go`. Contract C owns the final repository-wide AST gate. This plan adds package-specific executable behavior oracles only.
- Construct every group from the exact request/connection parent context. Do not use `context.Background()` to detach request work. The sole exception is replaced in MCP by `context.WithoutCancel(r.Context())`, which preserves request values while making session cancellation explicit.
- Unknown raw panic values must never be formatted, wrapped, logged as ordinary plugin failure, sent over a channel, or serialized as a response. Task groups recover only to join, then re-panic the exact value from their owning stack.
- Preserve Task 10 classification: exact `http.ErrAbortHandler` in a batch worker becomes the existing per-item 502; every other recovered value, including a pointer-valued core panic and `*plugin.PanicError`, remains raw and is re-panicked by the request owner after all accepted batch children finish.
- If an externally visible timeout wins first, decide and write the timeout result, but do not return from the handler or release its generation/dispatch lease until the task group finishes. A successful `ResponseWriter.Write` proves commitment to the server writer, not delivery to a remote socket.
- Preserve current buffer capacities, admission limits, copy direction labels, half-close behavior, deadline refresh, cancellation causes, response ordering, and first-error precedence unless a task explicitly changes it.
- Every cleanup path must attempt all owned cleanup before `Wait`; cleanup errors may join ordinary task errors, but must not replace a cached raw panic.
- Run all Go commands as `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && ...'` from the active worktree. Do not run `go test ./...`, `go test ./pkg/...`, `make test`, or broad integration suites.
- Each behavior task is one independently reviewable local commit. No push or PR is authorized by this plan.

## Current-Source Concurrency Inventory

The frozen-base inventory is 15 raw production `go` statements plus one `sync.WaitGroup.Go` admission across the eight production areas below.

| Owner/path | Spawn sites | Timeout, cancellation, channel, copy, and wait behavior at base | Required owner boundary |
| --- | ---: | --- | --- |
| `pkg/stream/bridge/bridge.go` | 3 raw `go` | One context watcher closes both connections; two copy directions send into a capacity-2 result channel. First EOF half-closes its destination and waits for reverse; first hard error closes both and waits for reverse. A child panic skips its send and can strand the receiver. | One `connection/stream-bridge` group; watcher and both directions admitted before `Wait`; every direction sends a completion marker from `defer`. |
| `pkg/plugin/batch_requests/plugin.go` | 1 raw `go` | Sequential items; optional per-item timeout; dispatcher capacity gate; capacity-1 late result send; worker owns dispatcher release and optional child generation lease. Timeout returns a 504 item before worker finishes. Exact abort becomes 502; other panic currently re-panics from a detached goroutine. | One `request/batch-requests` group for the handler; worker remains responsible for dispatcher/child lease; response is written before final `Wait`, then the request owner joins/re-panics. |
| `pkg/plugin/kafka_proxy/websocket.go` | 3 raw `go` | Two copy directions report to capacity-2 channel; watcher closes Kafka and WebSocket on context cancellation. First result cancels/closes and waits for second. | One `connection/kafka-proxy` group with deferred direction completion, cleanup, then `Wait`. |
| `pkg/plugin/kafka_proxy/transport.go` | 1 raw `go` | `closeOnContextDone` watches context and returns a stopper used by WebSocket and the consumer call path. | Preserve the helper signature; internally own watcher with a group and make stop cancel/join idempotently. |
| `pkg/plugin/proxy_mirror/plugin.go` | 1 raw `go` | Plugin-wide background context and `WaitGroup`; capacity-16 admission; request body copied before asynchronous send; `Stop` cancels/joins all requests across generations. | One request group registered as a request lifecycle finalizer before launch; plugin keeps only shared admission/client resources. |
| `pkg/plugin/mqtt_proxy/stream.go` | 2 raw `go` + 1 `WaitGroup.Go` | Listener and stream cancellation watchers close connections; accepted connections join only after accept exits; bridge handles duplex copy. | Listener watcher and accepted connections use explicit connection groups; stream watchers use idempotent group-backed stoppers; bridge task must land first. |
| `pkg/plugin/mcp_bridge/plugin.go` | 3 raw `go` | `startSession(context.Background())`; stdout/stderr scanners feed capacity-16 events; reaper waits local scanners, calls `cmd.Wait`, removes session, closes `events` and `done`. `closeSession` cancels but does not join. | Session owns a `connection/mcp-bridge` group derived from `context.WithoutCancel(r.Context())`; scanners plus coordinator join on close. |
| `pkg/plugin/ai_stream/flush_writer.go` | 1 raw `go` | Every periodic `FlushWriter` starts a ticker loop detached from its HTTP streaming response. `Close` closes `stop` and waits on `done`; timer flush and response writes share `mu`, but a writer panic while manually locked can strand shutdown. Both `ai_proxy` call sites close on multiple branches instead of one guaranteed defer. | One `request/ai-stream` group from the exact HTTP request context; `Close` stops and joins the loop with cached panic replay; both callers pass `r.Context()` and defer one close immediately. |

### Frozen `RequestTaskGroup` dependency

This plan does not own the runtime implementation. Contract C Task 1 must be integrated and green first with the exact production method set:

```go
func NewRequestTaskGroup(parent context.Context, owner string) *RequestTaskGroup
func (g *RequestTaskGroup) Go(run func(context.Context) error) error
func (g *RequestTaskGroup) Wait() error
```

Its required observable behavior is:

```go
func TestRequestTaskGroupWaitJoinsBeforeRepanic(t *testing.T) {
	want := &struct{ marker string }{marker: "core"}
	release := make(chan struct{})
	group := NewRequestTaskGroup(context.Background(), "request/test")
	if err := group.Go(func(context.Context) error { panic(want) }); err != nil { t.Fatal(err) }
	if err := group.Go(func(context.Context) error { <-release; return nil }); err != nil { t.Fatal(err) }

	got := make(chan any, 1)
	go func() {
		defer func() { got <- recover() }()
		_ = group.Wait()
	}()
	select {
	case value := <-got:
		t.Fatalf("Wait repanicked before join: %#v", value)
	default:
	}
	close(release)
	if value := <-got; value != want { t.Fatalf("panic = %#v", value) }
}
```

Contract C additionally proves concurrent and repeated callers observe the same cached identity, ordinary errors still join, and `Go` rejects admission after `Wait` begins. If any of those tests fail on the integration head, stop this plan and repair Contract C rather than adding a call-site workaround.

## File Responsibility and Dependency Order

| Task | Exclusive production files | Focused test ownership | Depends on |
| --- | --- | --- | --- |
| 1. Stream bridge | `pkg/stream/bridge/bridge.go` | `pkg/stream/bridge/bridge_test.go` | Contract C Task 1 |
| 2. Batch requests and generation lease | `pkg/plugin/batch_requests/plugin.go` | `pkg/plugin/batch_requests/plugin_test.go`; generation-lease oracles only in `pkg/server/route_handler_test.go` | Contract C Task 1 |
| 3. Kafka connection paths | `pkg/plugin/kafka_proxy/websocket.go`, `transport.go` | `websocket_test.go`, `transport_test.go`, and affected `consumer_test.go` | Contract C Task 1 |
| 4. Proxy mirror request lifetime | `pkg/plugin/proxy_mirror/plugin.go` | `pkg/plugin/proxy_mirror/plugin_test.go` | Contract C Task 1 and Task 10 request lifecycle |
| 5. MQTT connection paths | `pkg/plugin/mqtt_proxy/stream.go` | `pkg/plugin/mqtt_proxy/stream_test.go` | Task 1 and Contract C Task 1 |
| 6. MCP session lifetime | `pkg/plugin/mcp_bridge/plugin.go` | `pkg/plugin/mcp_bridge/plugin_test.go` | Contract C Task 1 |
| 7. AI streaming flush lifetime | `pkg/plugin/ai_stream/flush_writer.go`; constructor call sites in `pkg/plugin/ai_proxy/plugin.go`, `pkg/plugin/ai_proxy_multi/plugin.go` | `flush_writer_test.go`; focused existing streaming tests in both caller packages | Contract C Task 1 and the reviewed generation-plan AI-health Task 5 head |
| 8. Integration verification | no production files | package-specific inventory and combined gates only; no duplicate AST gate | Tasks 1-7 and Contract C final gate |

After Contract C Task 1, Tasks 1, 2, 3, 4, and 6 have independent write sets and may form a frozen-base wave. Task 5 starts from Task 1's reviewed head because MQTT consumes bridge behavior. Task 7 must start from the reviewed generation-plan AI-health Task 5 head because both tasks modify `pkg/plugin/ai_proxy_multi/plugin.go` and `plugin_test.go`; they are sequential owners of that shared call site, never parallel frozen-base writers. Integrate in the deterministic order: Contract C Task 1, Task 1, Task 2, Task 3, Task 4, Task 5, Task 6, generation AI-health Task 5, Task 7, Task 8. Rebase or regenerate each later verification result after integration; do not reuse race evidence from an obsolete base.

---

### Task 1: Join Both Stream-Bridge Directions Before Returning or Re-panicking

**Files:**

- Modify: `pkg/stream/bridge/bridge.go`
- Modify: `pkg/stream/bridge/bridge_test.go`

**Consumes:** Contract C Task 1 `RequestTaskGroup`; current `Pump`, `copyDirection`, `closeWrite`, idle-deadline, and connection-close behavior.

**Produces:** One joined `connection/stream-bridge` owner for the cancellation watcher and both copy directions; exact raw panic returns to the `Pump` caller only after the peer direction and both connection closes finish.

- [ ] **Step 1: Add a subprocess RED oracle for raw direction panic ownership**

Use a subprocess because the frozen implementation panics in a detached goroutine and would otherwise terminate the package test process. Add a helper mode that supplies a `net.Conn` whose `Read` panics with a package-level pointer sentinel, wraps `Pump` with `recover`, and prints markers only after the opposite direction observes close.

```go
func TestPumpRawDirectionPanicReturnsFromOwnerAfterPeerCleanup(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestPumpRawDirectionPanicHelper$")
	cmd.Env = append(os.Environ(), "APISIX_GO_BRIDGE_PANIC_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil { t.Fatalf("helper exited before owner recovery: %v\n%s", err, out) }
	if !bytes.Contains(out, []byte("bridge-owner-recovered")) ||
		!bytes.Contains(out, []byte("bridge-peer-closed")) {
		t.Fatalf("missing ownership markers: %s", out)
	}
}
```

The helper must compare `recover() == bridgeRawPanic` and exit non-zero on a different value. RED on the frozen implementation: the child process exits non-zero from the raw direction goroutine before printing `bridge-owner-recovered`.

- [ ] **Step 2: Add normal precedence and join oracles**

Extend the existing half-close and hard-error tests to assert:

1. EOF in one direction performs only that direction's `CloseWrite`, then waits for the reverse direction.
2. A hard error closes both endpoints and still waits for the second direction.
3. Parent cancellation closes both endpoints and `Pump` does not return until the two direction callbacks and watcher have exited.
4. The first existing copy error remains the returned error when cleanup/second-direction completion also reports an ordinary error.

Capture RED with:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/stream/bridge -run "^(TestPumpRawDirectionPanicReturnsFromOwnerAfterPeerCleanup|TestPump.*Waits|TestPump.*Precedence)$" -count=1'
```

- [ ] **Step 3: Replace all three raw spawns with one request task group**

Derive `pumpCtx, cancel := context.WithCancel(ctx)`, construct `runtime.NewRequestTaskGroup(pumpCtx, "connection/stream-bridge")`, and admit the watcher plus both directions before beginning result selection. Extend the private result only with completion state:

```go
type directionResult struct {
	direction string
	err       error
	completed bool
}
```

Each direction callback owns exactly one deferred send into the existing capacity-2 channel. It sets `completed=true` only after `copyDirection` returns. If the copy panics, its defer sends `completed=false`; Contract C catches and caches the raw panic after that defer runs. The owner closes/cancels as required, receives the second direction result, calls `cancel`, attempts both connection closes through per-close recovery, and finally calls `tasks.Wait`. Capture cleanup panic only long enough to attempt the other close; a panic replayed by `Wait` has primary-child precedence, otherwise re-panic the first cleanup value. Never place a recovered value in `directionResult`.

If fixed pre-wait admission unexpectedly fails, cancel, close both endpoints, join every already accepted callback, and return the admission error. Do not retry after `ErrTaskGroupWaiting`.

- [ ] **Step 4: GREEN, package, race, lint, and build**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/stream/bridge -run "^(TestPumpRawDirectionPanicReturnsFromOwnerAfterPeerCleanup|TestPump)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/stream/bridge -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/stream/bridge -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/stream/bridge && make build'
git diff --check
```

- [ ] **Step 5: Dead-spawn scan, independent review, and commit**

Run `rg -n '\bgo[[:space:]]+|sync\.WaitGroup|\.Go\(' pkg/stream/bridge --glob '*.go'`. The only `.Go(` in production must be `RequestTaskGroup.Go`; subprocess/test goroutines remain test-owned. Request a read-only review focused on half-close, direction-error precedence, cancellation, and raw panic identity. Then commit:

```bash
git add pkg/stream/bridge/bridge.go pkg/stream/bridge/bridge_test.go
git commit -m "refactor(stream): own bridge connection tasks"
```

---

### Task 2: Own Batch Workers Through Response Commitment and Generation Drain

**Files:**

- Modify: `pkg/plugin/batch_requests/plugin.go`
- Modify: `pkg/plugin/batch_requests/plugin_test.go`
- Modify tests only: `pkg/server/route_handler_test.go`

**Consumes:** Contract C Task 1; Task 10 exact-abort 502/raw-core fatality; current dispatcher admission and `GenerationDispatchLease` factory attached to the request context.

**Produces:** One `request/batch-requests` group per outer handler, timeout response commitment before join, exact raw panic returned to the request owner, and generation drain held until every accepted timed-out worker releases its child dispatch lease.

- [ ] **Step 1: Convert the Task 10 subprocess oracle into a safe RED for request-owner recovery**

Do not run a direct frozen-base raw panic test. In the subprocess helper, wrap the outer batch handler invocation in `recover`, use a pointer-valued sentinel from the upstream subrequest, and print success only when the recovered value is the same pointer, the normal batch body was not written, and the worker's dispatcher/child release marker ran.

```go
func TestBatchCorePanicReturnsToRequestOwnerInSubprocess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestBatchCorePanicOwnerHelper$")
	cmd.Env = append(os.Environ(), "APISIX_GO_BATCH_OWNER_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil { t.Fatalf("detached worker escaped request owner: %v\n%s", err, out) }
	for _, marker := range [][]byte{[]byte("batch-owner-recovered"), []byte("batch-worker-released")} {
		if !bytes.Contains(out, marker) { t.Fatalf("missing %q in %s", marker, out) }
	}
}
```

RED on the frozen implementation: non-zero subprocess exit because the worker re-panics from a detached goroutine. Retain and strengthen the existing exact `http.ErrAbortHandler` test: one 502 item, no outer panic, and ordinary JSON response.

- [ ] **Step 2: Add timeout commitment and child-lease RED oracles**

Add a response-writer wrapper whose first `Write` closes a channel. Run `ServeHTTP` in a test goroutine with one blocked subrequest and a short item timeout. Assert in order:

1. the committed body contains the existing 504 item;
2. the handler has not returned after the write;
3. dispatcher release and child generation release have not happened while the worker is blocked;
4. releasing the worker lets the handler return exactly once and balances both releases.

Extend `TestRouteHandlerBatchTimeoutKeepsRetiredGenerationUntilWorkerExit` to run the route handler asynchronously. After the timeout body is written, retire/swap the generation and assert `Drain` remains blocked and the child count is still one. Release the worker, then assert child release once, parent release once, active generations zero, and `Drain` completes.

Capture both RED families:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/batch_requests -run "^(TestBatchCorePanicReturnsToRequestOwnerInSubprocess|TestHandlerConvertsAbort|TestBatchTimeoutWritesBeforeJoiningWorker)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^TestRouteHandlerBatchTimeoutKeepsRetiredGenerationUntilWorkerExit$" -count=1'
```

Expected timeout RED: the frozen handler returns immediately after 504 and the parent generation lease can retire while its detached child continues.

- [ ] **Step 3: Thread one outer group through batch item dispatch**

Create the group once in `serveBatchRequest` from `r.Context()`. Pass it through the private item/dispatch helpers; do not create one group per item. Extend the private worker result with `completed bool`. The task callback uses the existing item-derived timeout context, defers dispatcher release and optional child lease release, and defers one send to the capacity-1 result channel. It sets `completed=true` only after the existing recovery/classification logic returns normally.

The worker recovery remains local and exact:

- `recovered == http.ErrAbortHandler`: construct the existing abort 502 item and complete normally.
- every other recovered value: `panic(recovered)` unchanged; the deferred result announces `completed=false`, then Contract C caches the raw value.

When the item owner receives `completed=false`, call `tasks.Wait` immediately; this closes further admission and re-panics before any batch body is encoded. When timeout wins, append the existing 504 item and continue; the capacity-1 result channel permits late completion without blocking. After all ordinary items, encode/write the complete batch response first, then call `tasks.Wait` before `serveBatchRequest` returns. Propagate an ordinary `Wait` error through the existing handler error path.

On `tasks.Go` admission failure, release any dispatcher and child lease acquired before admission, cancel the item context, join prior accepted work, and return the admission error. No result channel may carry the panic value.

- [ ] **Step 4: GREEN and directly affected gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/batch_requests -run "^(TestBatch|TestSubrequest|TestHandler.*Panic|TestHandlerConvertsAbort|TestBatchTimeout)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/batch_requests -run "^(TestBatch|TestHandler.*Panic|TestBatchTimeout)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestRouteHandlerBatch|TestRouteHandlerDrainWaitsForRetainedBatchChild)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^(TestRouteHandlerBatch|TestRouteHandlerDrainWaitsForRetainedBatchChild)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/batch_requests -count=1 && golangci-lint run ./pkg/plugin/batch_requests ./pkg/server && make build'
git diff --check
```

- [ ] **Step 5: Ownership scan, independent review, and commit**

Run `rg -n '\bgo[[:space:]]+|sync\.WaitGroup|context\.Background\(' pkg/plugin/batch_requests --glob '*.go'` and verify production has no result. Request a read-only review focused on response-write-before-join timing, exact abort versus raw pointer identity, no serialization on incomplete result, dispatcher/lease exactly-once release, and route generation drain. Commit only owned files:

```bash
git add pkg/plugin/batch_requests/plugin.go pkg/plugin/batch_requests/plugin_test.go pkg/server/route_handler_test.go
git commit -m "refactor(batch): own subrequest workers"
```

---

### Task 3: Join Kafka Cancellation Watchers and WebSocket Copy Directions

**Files:**

- Modify: `pkg/plugin/kafka_proxy/transport.go`
- Modify: `pkg/plugin/kafka_proxy/transport_test.go`
- Modify: `pkg/plugin/kafka_proxy/websocket.go`
- Modify: `pkg/plugin/kafka_proxy/websocket_test.go`
- Modify tests only if an assertion changes: `pkg/plugin/kafka_proxy/consumer_test.go`

**Consumes:** Contract C Task 1; existing `closeOnContextDone` consumer/transport helper signature; WebSocket close-code and `websocketProxyError` precedence.

**Produces:** Joined watcher stoppers and two-direction bridge ownership under `connection/kafka-proxy`, without changing consumer call sites or WebSocket protocol mapping.

- [ ] **Step 1: Add RED tests for idempotent watcher join and raw direction ownership**

For `closeOnContextDone`, use a connection whose `Close` blocks. Cancel the parent, invoke the returned stopper in another goroutine, and assert the stopper does not return until `Close` is released; then call the stopper again and assert it returns without a second close. Repeat for normal stop-before-cancel and assert the connection is not closed.

For WebSocket copy panic, use a subprocess helper like Task 1. One bridge direction panics with a pointer sentinel; the other blocks until connection close. Wrap `ServeWebSocket` in `recover` and print markers only after exact identity and peer exit. RED is a non-zero helper exit from the frozen detached direction.

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/kafka_proxy -run "^(TestCloseOnContextDoneStopJoinsClose|TestCloseOnContextDoneStopIsIdempotent|TestServeWebSocketRawDirectionPanicReturnsAfterPeerCleanup)$" -count=1'
```

- [ ] **Step 2: Preserve helper signatures while moving the watcher behind a group**

Keep this public-to-package shape because both transport and consumer use it:

```go
func closeOnContextDone(ctx context.Context, conn net.Conn) func()
func closeConnectionsOnCancel(ctx context.Context, connections ...net.Conn) func()
```

Each helper constructs a fixed-owner task group from `ctx`, admits one watcher callback, and returns an idempotent stopper guarded by `sync.Once`. The watcher selects between `ctx.Done()` and a private `stop` channel. Parent cancellation attempts every connection close through per-close recovery and re-panics the first close value only after all connections were attempted; ordinary stopper close exits without closing a live connection. The stopper closes `stop` and invokes `Wait` inside a recovery closure that stores `{panicked, value}`; after `Once.Do` every caller separately re-panics the stored exact value. Never put the `Wait` call itself directly in a panicking `Once.Do` closure, because only the first caller would then observe the panic. Do not call `cancel()` merely to stop the watcher because that would make a normal operation close its connection.

With the fixed valid owner and non-nil callback, pre-wait watcher admission failure is a synchronous core invariant: panic with that exact error before returning a stopper. Do not silently continue without cancellation ownership.

- [ ] **Step 3: Move WebSocket copy results to completion-state ownership**

Construct one `connection/kafka-proxy` group from the WebSocket request context. Extend the private direction result to `{err error; completed bool}`. Each direction sends exactly once from `defer`, setting `completed=true` only after `clientToKafka` or `kafkaToClient` returns. After the first result, preserve existing cancellation, close-frame, backend close, and normal-close classification; always receive the second result and call both watcher stopper and `tasks.Wait` before returning.

An incomplete result triggers cleanup followed by `tasks.Wait`, which re-panics the original raw value after the peer exits. Never turn it into `websocketProxyError`. Keep `ServePubSubWebSocket` on the same group-backed cancellation helper even though it has no duplex direction spawns.

- [ ] **Step 4: GREEN and affected package gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/kafka_proxy -run "^(TestCloseOnContextDone|TestServeWebSocket|TestServePubSubWebSocket|TestTransport|TestKafkaConsumer)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/kafka_proxy -run "^(TestCloseOnContextDone|TestServeWebSocket|TestServePubSubWebSocket|TestTransport|TestKafkaConsumer)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/kafka_proxy -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/kafka_proxy && make build'
git diff --check
```

- [ ] **Step 5: Scan, review, and commit**

Run `rg -n '\bgo[[:space:]]+|sync\.WaitGroup|context\.Background\(' pkg/plugin/kafka_proxy --glob '*.go'`. Request a read-only review of watcher stop/cancel races, consumer compatibility, both-direction join, exact panic identity, and WebSocket close/error precedence. Commit:

```bash
git add pkg/plugin/kafka_proxy/transport.go pkg/plugin/kafka_proxy/transport_test.go pkg/plugin/kafka_proxy/websocket.go pkg/plugin/kafka_proxy/websocket_test.go pkg/plugin/kafka_proxy/consumer_test.go
git commit -m "refactor(kafka): own connection tasks"
```

---

### Task 4: Bind Proxy-Mirror Delivery to the Request Lifecycle

**Files:**

- Modify: `pkg/plugin/proxy_mirror/plugin.go`
- Modify: `pkg/plugin/proxy_mirror/plugin_test.go`

**Consumes:** Contract C Task 1; Task 10 `apisixctx.RequestLifecycle.AddFinalizer`; existing capacity-16 mirror admission and HTTP clients.

**Produces:** Request-owned mirror delivery joined by a plugin finalizer, with no plugin-global background context or cross-generation `WaitGroup`.

- [ ] **Step 1: Add RED tests for lifecycle ownership and admission timing**

Add these behavior oracles:

1. With no request lifecycle, `mirrorFinalizedRequest` fails before admission and before reading the request body; no mirror request starts.
2. A blocked mirror transport keeps lifecycle finalization blocked and retains the admission slot; releasing it completes finalization and releases exactly once.
3. A transport panic pointer is recovered by the plugin finalizer boundary after the task group joins: `FinalizeResult.Failures` contains the canonical proxy-mirror owner, `FatalPanic` is nil, and a later request finalizer still runs.
4. Saturating 16 request-owned mirrors rejects the 17th without reading its body; completing one request admits the next.
5. Concurrent `Stop` closes idle transports once but neither cancels nor waits on another request's task group; request lifecycle remains the sole join owner.

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/proxy_mirror -run "^(TestMirrorRequiresRequestLifecycleBeforeBodyRead|TestMirrorLifecycleWaitsForDelivery|TestMirrorTaskPanicIsBoundedByPluginFinalizer|TestMirrorAdmissionRemainsBoundedPerRequest|TestStopDoesNotOwnRequestTasks)$" -count=1'
```

Expected RED: current work is attached to the plugin-wide context/`mirrorWG`, lifecycle finalization returns before delivery, and `Stop` owns the join.

- [ ] **Step 2: Register the owner before launching work**

At the start of `mirrorFinalizedRequest`, after `shouldMirror` but before admission/body read, require `lifecycle := apisixctx.GetRequestLifecycle(r)`. If nil, return a stable proxy-mirror lifecycle error. Then:

1. acquire only the existing capacity token;
2. read/copy the body and build the mirror request;
3. construct `runtime.NewRequestTaskGroup(r.Context(), "request/proxy-mirror")`;
4. register `tasks.Wait` with `lifecycle.AddFinalizer(name, tasks.Wait)` before `tasks.Go`;
5. admit one callback that defers admission release and calls `sendMirror(req.WithContext(taskCtx))`.

If body/build, finalizer registration, or task admission fails, release the token exactly once and return through the existing plugin hook error path. Do not launch if finalizer registration rejects admission. The task callback returns nil after ordinary HTTP send failures because existing mirror delivery remains best-effort; a raw callback panic is cached and then classified by the plugin-owned lifecycle finalizer, not as a core fatal.

- [ ] **Step 3: Remove plugin-global request ownership**

Delete `mirrorCtx`, `mirrorCancel`, `mirrorWG`, and all wait-on-stop behavior. Retain only the bounded admission channel, stopped/idempotence state, and clients. `Stop` marks admission closed to new work and closes idle client connections exactly once. Generation/request drain guarantees all accepted request finalizers have completed before final plugin release; do not recreate a hidden second join.

- [ ] **Step 4: GREEN, race, lint, and build**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/proxy_mirror -run "^(TestMirror|TestHandler|TestStop)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/proxy_mirror -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/proxy_mirror -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/proxy_mirror && make build'
git diff --check
```

- [ ] **Step 5: Scan, independent lifecycle review, and commit**

Run `rg -n '\bgo[[:space:]]+|sync\.WaitGroup|context\.Background\(' pkg/plugin/proxy_mirror --glob '*.go'`. Request a read-only review of lifecycle registration ordering, token exactly-once release, Stop/request ownership separation, and plugin-finalizer panic classification. Commit:

```bash
git add pkg/plugin/proxy_mirror/plugin.go pkg/plugin/proxy_mirror/plugin_test.go
git commit -m "refactor(proxy-mirror): own delivery by request"
```

---

### Task 5: Own MQTT Listener, Connection, and Cancellation Work

**Files:**

- Modify: `pkg/plugin/mqtt_proxy/stream.go`
- Modify: `pkg/plugin/mqtt_proxy/stream_test.go`

**Consumes:** Reviewed Task 1 stream bridge; Contract C Task 1; existing CONNECT preread/replay and `StreamResultHandler` contracts.

**Produces:** Joined listener watcher, accepted-connection callbacks, and stream cancellation watchers with unchanged MQTT bytes, deadlines, client-ID routing, and half-close semantics.

- [ ] **Step 1: Add RED ownership tests**

Add focused tests for:

1. `closeStreamOnContextDone`: cancellation blocks its stopper until every connection `Close` completes; normal stop does not close; repeated/concurrent stop is safe.
2. `closeListenerOnContextDone`: the same join/idempotence behavior for `Listener.Close`.
3. `ServeListener`: after `Accept` fails or cancellation closes the listener, it waits for all accepted streams and invokes `onResult` once per ordinary stream before returning.
4. Raw accepted-stream panic: subprocess helper uses a dialer that signals then panics with a pointer sentinel; the parent cancels the listener context, `ServeListener` joins all accepted callbacks, and its caller recovers the same pointer. Frozen behavior exits the subprocess from `WaitGroup.Go` before owner recovery.
5. `ServeStreamWithIdle`: cancellation during preread, upstream dial, CONNECT replay, and bridge copy closes all currently owned connections and returns only after watcher/bridge cleanup.

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/mqtt_proxy -run "^(TestCloseStreamOnContextDoneJoins|TestCloseListenerOnContextDoneJoins|TestServeListenerWaitsForAcceptedStreams|TestServeListenerRawPanicReturnsFromOwner|TestServeStreamCancellationJoinsOwnedWork)$" -count=1'
```

- [ ] **Step 2: Replace cancellation watchers with group-backed idempotent stoppers**

Keep the private helper call shapes unless tests benefit from a variadic option:

```go
func closeStreamOnContextDone(ctx context.Context, conns ...net.Conn) func()
func closeListenerOnContextDone(ctx context.Context, listener net.Listener) func()
```

Each helper owns one fixed-name `RequestTaskGroup`, a private stop channel, and a `sync.Once` stopper. Cancellation attempts every close through per-close recovery, then re-panics the first exact value so the group caches it only after all closes were attempted. The stopper invokes `Wait` inside a recovery closure, stores `{panicked, value}`, and makes every concurrent/repeated caller re-panic the stored identity after `Once.Do` returns. Normal stop exits without closing. Every returned stopper must be called before its connection owner returns.

Use separate helper groups for the preread-only client watcher and the post-dial client/upstream watcher. This is intentional: calling the first stopper seals its group, while the second watcher is admitted later after the upstream exists. Do not keep a stopped group and then attempt late admission.

- [ ] **Step 3: Replace accepted `sync.WaitGroup.Go` with a connection task group**

In `ServeListener`, create `runtime.NewRequestTaskGroup(ctx, "connection/mqtt-proxy")` before the accept loop. Admit each accepted connection callback through `tasks.Go`; that callback preserves peer capture, `ServeStream`, `onResult`, and connection close ordering. If admission fails, close that just-accepted connection, stop the listener watcher, join previously admitted connections, and return the admission error.

On context-driven or ordinary `Accept` exit, first stop/join the listener watcher, then call `tasks.Wait`. If `Accept` produced an ordinary error, retain it unless `Wait` re-panics a cached raw child value; raw panic has fatal precedence after all accepted connections finish. Expected context shutdown still returns nil when no task error/panic exists.

Task 1 already makes `streambridge.Pump` join and re-panic from `ServeStreamWithIdle`; its deferred MQTT watcher stoppers and endpoint closes must execute before the accepted-connection task wrapper caches that raw value.

- [ ] **Step 4: GREEN and affected gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/mqtt_proxy -run "^(TestClose|TestServeListener|TestServeStream|TestReadConnect|TestWriteStream)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/mqtt_proxy -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/mqtt_proxy -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/stream/bridge -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/mqtt_proxy ./pkg/stream/bridge && make build'
git diff --check
```

- [ ] **Step 5: Scan, protocol review, and commit**

Run `rg -n '\bgo[[:space:]]+|sync\.WaitGroup|context\.Background\(' pkg/plugin/mqtt_proxy/stream.go`. Request a read-only review focused on accept/admission races, watcher stopper idempotence, raw panic precedence, exact preread replay, idle deadlines, and dependency on Task 1 half-close semantics. Commit:

```bash
git add pkg/plugin/mqtt_proxy/stream.go pkg/plugin/mqtt_proxy/stream_test.go
git commit -m "refactor(mqtt): own stream connection tasks"
```

---

### Task 6: Join MCP Scanner and Reaper Work by Session Lifetime

**Files:**

- Modify: `pkg/plugin/mcp_bridge/plugin.go`
- Modify: `pkg/plugin/mcp_bridge/plugin_test.go`

**Consumes:** Contract C Task 1; current SSE buffering/order, `exec.CommandContext`, session map, explicit close routes, and request lifecycle behavior.

**Produces:** A session-owned `connection/mcp-bridge` task group with explicit cancellation, scanner completion coordination, deterministic map/event cleanup, and no `context.Background` detachment.

- [ ] **Step 1: Add RED tests for early-return join and session cleanup**

Add or extend tests to prove:

1. An SSE writer failure calls `closeSession`, cancels the command, waits for stdout/stderr scanners and reaper, removes the session, and closes `done` before the handler returns.
2. Request cancellation still drains already-buffered events in order, then closes and joins the session; no event is sent after `events` closes.
3. Direct `closeSession` blocks while a scanner is deliberately held, then returns after release; concurrent duplicate close neither double-closes channels nor loses the join.
4. `closeAll` snapshots under the mutex, cancels and joins outside it, and leaves the map empty without deadlock.
5. A subprocess scanner-panic seam re-panics the same pointer from `handleSSE` only after the other scanner, command wait, map removal, and channel closes complete. Frozen raw goroutine behavior exits non-zero before those markers.

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/mcp_bridge -run "^(TestSSEWriterFailureJoinsSession|TestSSERequestCancelDrainsThenJoins|TestCloseSessionWaitsForScanners|TestCloseAllJoinsOutsideLock|TestScannerPanicReturnsFromSessionOwner)$" -count=1'
```

- [ ] **Step 2: Preserve request values while making session cancellation explicit**

Replace `startSession(context.Background())` with:

```go
sessionParent := context.WithoutCancel(r.Context())
sess, err := p.startSession(sessionParent)
```

`startSession` still derives `ctx, cancel := context.WithCancel(parent)` and still uses `exec.CommandContext(ctx, ...)`. `WithoutCancel` deliberately keeps request-scoped values required by the plugin while preventing an early client cancellation from racing buffered SSE drain; the handler's deferred `closeSession` remains the explicit cancellation/join point.

Add `tasks *runtime.RequestTaskGroup` to the private `session`. Do not add a process-global group or expose it from the plugin.

- [ ] **Step 3: Admit scanners and coordinator without a local WaitGroup**

Create two capacity-1 completion channels, `stdoutDone` and `stderrDone`. Admit three callbacks through the session group:

1. stdout scanner defers `stdoutDone <- struct{}{}` and calls `scanPipe`;
2. stderr scanner defers `stderrDone <- struct{}{}` and calls `scanStderr`;
3. coordinator receives both completions, calls `cmd.Wait`, removes the session, closes `events`, then closes `done`.

The completion sends must run even if a scanner panics so the coordinator cannot strand; only the task group stores the raw panic. Keep the channels capacity one so a deferred send cannot block while admission failure cleanup is running. Preserve the current requirement that both scanners consume pipes to EOF before `cmd.Wait`, because reversing that order can discard buffered child output.

If any of the three admissions fails, cancel the command, close stdin, join all already accepted callbacks, remove the session, and return the setup error. Register the session in the map only after all immutable fields exist; ensure partial failure cannot leave a visible unusable session.

- [ ] **Step 4: Make close paths the sole join owner**

Change the private close owner to accept the already-held pointer: `func (p *Plugin) closeSession(sess *session)`. `handleSSE` defers `p.closeSession(sess)`, so the coordinator removing the map entry can never make the final owner lose its `Wait`. The close function removes that exact ID under lock, then outside the lock uses a session-local `sync.Once` to close stdin, cancel, and call `sess.tasks.Wait`; its stored panic outcome is replayed to every caller. The coordinator's `removeSession` remains idempotent.

`closeAll` snapshots and clears the map under lock, then performs stdin close, cancel, and `Wait` for every snapshot outside the lock. If one session `Wait` re-panics, recover it only in a per-session cleanup wrapper, continue closing the remaining sessions, then re-panic the first exact value after all sessions finish. Do not format it or turn it into an error.

- [ ] **Step 5: GREEN and affected gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/mcp_bridge -run "^(TestSSE|TestHandle|TestSession|TestClose|TestScanner)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/mcp_bridge -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/mcp_bridge -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/mcp_bridge && make build'
git diff --check
```

- [ ] **Step 6: Scan, session-lifecycle review, and commit**

Run `rg -n '\bgo[[:space:]]+|sync\.WaitGroup|context\.Background\(' pkg/plugin/mcp_bridge --glob '*.go'`. Request a read-only review focused on scanner/coordinator ordering, channel-close ownership, partial admission cleanup, request-value preservation, map locking, and raw panic cleanup precedence. Commit:

```bash
git add pkg/plugin/mcp_bridge/plugin.go pkg/plugin/mcp_bridge/plugin_test.go
git commit -m "refactor(mcp): own session process tasks"
```

---

### Task 7: Own the AI Streaming Periodic Flush Loop by HTTP Request

**Files:**

- Modify: `pkg/plugin/ai_stream/flush_writer.go`
- Modify: `pkg/plugin/ai_stream/flush_writer_test.go`
- Modify constructor call site: `pkg/plugin/ai_proxy/plugin.go`
- Modify focused caller tests: `pkg/plugin/ai_proxy/plugin_test.go`
- Modify constructor call site: `pkg/plugin/ai_proxy_multi/plugin.go`
- Modify focused caller tests: `pkg/plugin/ai_proxy_multi/plugin_test.go`

**Consumes:** Contract C Task 1; the reviewed generation-plan AI-health Task 5 head; current synchronous/periodic flush behavior, `onFirst` callback, response status buffering, and AI stream outcome/error handling.

**Produces:** One joined `request/ai-stream` periodic loop for each HTTP streaming response, exact timer/writer panic replay from `Close`, and one guaranteed close/join defer at both production call sites.

- [ ] **Step 1: Add RED tests for loop join and raw flush panic ownership**

Change the focused constructor calls to the frozen exact replacement signature:

```go
func NewFlushWriter(
	ctx context.Context,
	writer http.ResponseWriter,
	interval time.Duration,
	onFirst func(),
) *FlushWriter
```

Add these oracles before production changes:

1. A blocking `http.Flusher.Flush` proves `Close` does not return until an in-progress timer flush exits.
2. Parent cancellation stops the periodic loop, performs the pending final flush, and a later `Close` joins without a detached tick.
3. A subprocess flusher panics on a timer tick with a pointer sentinel; wrapping `Close` recovers the same pointer only after the loop's ticker cleanup marker. The frozen raw goroutine exits the subprocess non-zero before owner recovery.
4. Concurrent/repeated `Close` calls all replay the same stored raw identity; a panic inside `sync.Once.Do` must not be visible only to its first caller.
5. A writer `Write` panic releases `FlushWriter.mu`, so the caller's deferred `Close` cannot deadlock while stopping the loop.
6. Interval zero retains synchronous `Flush`, first-write callback exactly once, deferred status write, and idempotent close.

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_stream -run "^(TestFlushWriterCloseJoinsPeriodicFlush|TestFlushWriterContextCancelStopsAndFlushes|TestFlushWriterPeriodicPanicReturnsFromCloseOwner|TestFlushWriterConcurrentCloseReplaysPanic|TestFlushWriterWritePanicDoesNotStrandClose|TestFlushWriterSupportsSynchronousAndPeriodicFlush)$" -count=1'
```

Expected RED: the new constructor does not compile against the detached implementation; the old timer panic would also terminate the subprocess instead of returning through `Close`.

- [ ] **Step 2: Replace `done` ownership with one request task group**

For `interval > 0`, construct `runtime.NewRequestTaskGroup(ctx, "request/ai-stream")` and admit exactly one callback running the ticker loop. The loop selects among ticker, `ctx.Done()`, and the existing private stop channel. Both cancellation and explicit stop perform one pending final flush before return. With `interval <= 0`, retain the no-goroutine synchronous path and do not construct a task group.

Refactor the private flush critical section so every lock has a deferred unlock. A panic from `writer.Write`, `writer.WriteHeader`, or `http.Flusher.Flush` must not leave `mu` held and strand the request's deferred close. Do not guard, convert, or relabel writer panics: a timer-loop panic is cached by Contract C, while a direct request-stack writer panic remains exact.

`Close` closes `stop` once, invokes `tasks.Wait` inside a recovery closure, and stores `{panicked, value}` separately from `sync.Once`. Every caller checks the stored outcome after `Once.Do` and re-panics the same value. For interval zero, the same outcome cache surrounds the final synchronous flush. Keep `Close`'s method signature unchanged and add no separate `Wait` method.

With the fixed owner and callback, constructor admission failure is a synchronous core invariant and panics before returning the writer. No detached fallback is allowed.

- [ ] **Step 3: Make both production request contexts and close points explicit**

Both call sites require production changes; testing `flush_writer.go` alone is insufficient.

In `pkg/plugin/ai_proxy/plugin.go` and `pkg/plugin/ai_proxy_multi/plugin.go`, pass `r.Context()` as the first constructor argument and immediately add:

```go
streamWriter := ai_stream.NewFlushWriter(r.Context(), w, flushInterval, onFirst)
defer streamWriter.Close()
```

Remove the branch-local explicit `streamWriter.Close()` calls after the defer is installed. This guarantees join on success, cancellation, terminal stream error, writer panic unwinding, and every early return while preserving all existing outcome/log/terminal-error decisions. Do not move `RecordStreamOutcome`, alter first-token timing, or change SSE/AWS EventStream forwarding.

Add focused caller assertions that the streaming handler does not return while a periodic flush callback is blocked and that request cancellation still publishes the existing `$ai_stream_outcome` before deferred close returns. Exercise both `ai-proxy` and `ai-proxy-multi`; no non-streaming path should construct a group.

- [ ] **Step 4: GREEN and caller gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_stream -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/ai_stream -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_proxy -run "^(TestHandler.*Stream|TestHandlerForwardsBedrockConverseEventStream|TestHandlerProgressingStreamSurvivesConfiguredTimeout|TestHandlerStalledStreamTimesOutConfiguredInactivity)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_proxy_multi -run "^(TestHandler.*Stream|TestHandlerForwardsSelectedBedrockEventStreamInstance)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/ai_proxy ./pkg/plugin/ai_proxy_multi -run "^(TestHandler.*Stream|TestHandlerForwards.*EventStream|TestHandlerProgressingStreamSurvivesConfiguredTimeout|TestHandlerStalledStreamTimesOutConfiguredInactivity)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/ai_stream ./pkg/plugin/ai_proxy ./pkg/plugin/ai_proxy_multi && make build'
git diff --check
```

- [ ] **Step 5: Scan, request-writer review, and commit**

Run `rg -n '\bgo[[:space:]]+|sync\.WaitGroup|context\.Background\(' pkg/plugin/ai_stream/flush_writer.go` and verify no result. Run `rg -n 'NewFlushWriter\(' pkg --glob '*.go'` and prove every production call passes its exact request context and installs a guaranteed close. Request a read-only review focused on mutex release under panic, final-flush exactly-once behavior, timer/cancel/Close races, repeated panic replay, and unchanged AI streaming outcomes. Commit:

```bash
git add pkg/plugin/ai_stream/flush_writer.go pkg/plugin/ai_stream/flush_writer_test.go pkg/plugin/ai_proxy/plugin.go pkg/plugin/ai_proxy/plugin_test.go pkg/plugin/ai_proxy_multi/plugin.go pkg/plugin/ai_proxy_multi/plugin_test.go
git commit -m "refactor(ai-stream): own periodic flush by request"
```

---

### Task 8: Integrate, Verify Package-Specific Ownership, and Review Boundaries

**Files:** No product or global gate files. Contract C alone owns `pkg/runtime/goroutine_contract_test.go`.

**Consumes:** Reviewed Tasks 1-7 integrated in dependency order and Contract C's final syntax-plus-type goroutine gate.

**Produces:** Reusable combined verification evidence and four independent boundary reviews; no duplicate syntax authority.

- [ ] **Step 1: Re-run exact package ownership inventories**

```bash
rg -n '\bgo[[:space:]]+|sync\.WaitGroup|context\.Background\(' \
  pkg/stream/bridge/bridge.go \
  pkg/plugin/batch_requests/plugin.go \
  pkg/plugin/kafka_proxy/websocket.go \
  pkg/plugin/kafka_proxy/transport.go \
  pkg/plugin/proxy_mirror/plugin.go \
  pkg/plugin/mqtt_proxy/stream.go \
  pkg/plugin/mcp_bridge/plugin.go \
  pkg/plugin/ai_stream/flush_writer.go
```

Expected: no output. Then run `rg -n '\.Go\('` on the same files and manually confirm every production match is `RequestTaskGroup.Go`, not `sync.WaitGroup.Go` or an unrelated launcher. Do not encode a second AST scan in this plan; run Contract C's authoritative gate after all Task 11 subplans integrate.

- [ ] **Step 2: Run focused normal suites from the integrated head**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/stream/bridge ./pkg/plugin/batch_requests ./pkg/plugin/kafka_proxy ./pkg/plugin/proxy_mirror ./pkg/plugin/mqtt_proxy ./pkg/plugin/mcp_bridge ./pkg/plugin/ai_stream -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/server -run "^(TestRouteHandlerBatch|TestRouteHandlerDrainWaitsForRetainedBatchChild)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_proxy -run "^(TestHandler.*Stream|TestHandlerForwardsBedrockConverseEventStream|TestHandlerProgressingStreamSurvivesConfiguredTimeout|TestHandlerStalledStreamTimesOutConfiguredInactivity)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_proxy_multi -run "^(TestHandler.*Stream|TestHandlerForwardsSelectedBedrockEventStreamInstance)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/runtime -run "^(TestRequestTaskGroup|TestProductionGoroutinesUseOwnedRuntime)$" -count=1'
```

- [ ] **Step 3: Run concurrency and consumer gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/stream/bridge ./pkg/plugin/batch_requests ./pkg/plugin/kafka_proxy ./pkg/plugin/proxy_mirror ./pkg/plugin/mqtt_proxy ./pkg/plugin/mcp_bridge ./pkg/plugin/ai_stream -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/server -run "^(TestRouteHandlerBatch|TestRouteHandlerDrainWaitsForRetainedBatchChild)$" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/ai_proxy ./pkg/plugin/ai_proxy_multi -run "^(TestHandler.*Stream|TestHandlerForwards.*EventStream|TestHandlerProgressingStreamSurvivesConfiguredTimeout|TestHandlerStalledStreamTimesOutConfiguredInactivity)$" -count=1'
```

- [ ] **Step 4: Lint, build, diff, and local artifact cleanup**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/stream/bridge ./pkg/plugin/batch_requests ./pkg/plugin/kafka_proxy ./pkg/plugin/proxy_mirror ./pkg/plugin/mqtt_proxy ./pkg/plugin/mcp_bridge ./pkg/plugin/ai_stream ./pkg/plugin/ai_proxy ./pkg/plugin/ai_proxy_multi ./pkg/server'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build && make clean'
git diff --check
git status --short
```

- [ ] **Step 5: Run four independent read-only reviews**

Review from the same integrated head with non-overlapping questions:

1. **Panic and precedence:** exact abort versus raw pointer identity, no raw value on channels/responses, join-before-repanic, ordinary error precedence.
2. **Lease and lifecycle:** batch timeout response commitment, parent/child dispatch release counts, generation drain, proxy-mirror finalizer registration and token release.
3. **Protocol and streaming preservation:** bridge/Kafka/MQTT half-close, deadlines, frame mapping, CONNECT replay, WebSocket close codes, MCP buffered event order, AI first-token/outcome/final-flush behavior.
4. **Ownership and cleanup:** no hidden background context, no detached production spawn, idempotent stopper/Stop/close paths, AI writer mutex release under panic, and no dead helper or proxy-only compatibility layer.

Any confirmed defect returns to its owning task and repeats that task's smallest RED/GREEN and affected race gates before Task 8 restarts. Contract C, not this plan, updates the final AST authority if repository inventory changes.

## Plan Self-Review Checklist

- [ ] Every one of the 15 raw `go` statements and the single `sync.WaitGroup.Go` in scope maps to exactly one task above.
- [ ] Every task starts from Contract C Task 1 and uses only the unchanged `NewRequestTaskGroup`/`Go`/`Wait` API.
- [ ] Batch raw panic RED uses a subprocess; no frozen-base test can crash the parent package runner.
- [ ] Batch timeout writes the 504 response before final join while both parent request ownership and child generation dispatch ownership remain held until completion.
- [ ] Result channels carry only bounded result/completion state and never `recover()` values.
- [ ] Bridge, Kafka, and MQTT preserve both copy directions, half-close/deadline behavior, and cleanup before raw panic replay.
- [ ] Proxy mirror requires and registers its request lifecycle finalizer before task admission and removes plugin-global request ownership.
- [ ] MCP preserves stdout/stderr drain before `cmd.Wait`, closes channels from one coordinator, and joins outside the session-map lock.
- [ ] AI streaming passes the exact request context at both constructor call sites, guarantees one deferred close/join, releases its mutex under writer panic, and preserves first-token/outcome/final-flush behavior.
- [ ] AI streaming starts from the reviewed generation AI-health head; no two worktrees write `ai_proxy_multi/plugin.go` or its tests from the same base.
- [ ] Contract C alone owns the repository AST gate; this plan contains no duplicate gate file or production runtime edit.
- [ ] Each implementation task has an exact RED command, minimal implementation steps, focused normal/race gates, scoped lint/build/diff, review boundary, and commit command.
