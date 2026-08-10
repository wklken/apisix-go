# Stream Forwarding and Startup Implementation Plan

> **Execution owner:** `$fast-plan-impl` with bounded implementation workers. Implement the work units in the dependency order below; workers may not commit, push, or create PRs.

**Goal:** Close PR-003 and PR-004, and close the startup-safety portion of PR-015 for the currently supported TCP/raw and MQTT stream scope. Successful MQTT preread must not poison the long-lived stream, half-close must preserve the response direction, and an explicitly enabled stream runtime must fail startup when its requested configuration cannot be honored.

**Architecture:** Put the bidirectional pump in the leaf package `pkg/stream/bridge` so both `pkg/stream` and `pkg/plugin/mqtt_proxy` can import it without a cycle. The pump treats clean EOF as a half-close, waits for both directions, and uses context cancellation or a hard copy error to close both connections. The server validates the supported stream subset before binding, returns route/compile/listen errors, publishes a runtime only after complete construction, and closes it if a later startup step fails.

**Tech Stack:** Go 1.26, `net.Conn`, `io.Reader`, TCP half-close, context cancellation, stream router/runtime, server startup tests.

## Acceptance contract

- After a successful MQTT CONNECT replay, the upstream write deadline is reset to `time.Time{}`. Reset failure is a setup error and forwarding does not begin.
- A clean EOF in either direction calls `CloseWrite` on the destination when supported and does not close the opposite read direction. The pump waits for the remaining direction.
- Raw TCP and MQTT use the route upstream read timeout as their per-direction idle timeout; an unset value uses 60 seconds.
- `ServeStream` remains the compatibility entry point. `ServeStreamWithIdle` is the route-owned entry point used by `pkg/stream`.
- Stream mode is required when `proxy_mode` contains `stream`. It fails startup for no TCP listeners, any UDP listener, TLS, listener/upstream PROXY protocol flags, unresolved upstreams, unsupported plugins, invalid addresses, or bind failures.
- HTTP-only mode remains valid and ignores unused stream configuration.
- Runtime construction is transactional across listeners: a later bind failure closes all earlier listeners and no runtime is published.
- Invalid route reloads retain the last-good router generation. This PR does not invent an external readiness endpoint; none exists in the repository. Startup errors are deployment-visible through the process return, while dynamic config-health publication remains owned by the later production metrics/readiness plan.
- General stream plugin chaining, stream TLS/mTLS, UDP forwarding, PROXY protocol support, and stream metrics remain explicitly unsupported; this PR must document and reject them rather than silently degrade.
- Real socket tests are not parallel and use bounded deadlines.

## Fixed interfaces

```go
// pkg/stream/bridge
func Pump(
    ctx context.Context,
    left net.Conn,
    right net.Conn,
    leftReader io.Reader,
    idle time.Duration,
) error

// pkg/plugin/mqtt_proxy
func (p *Plugin) ServeStreamWithIdle(
    ctx context.Context,
    client net.Conn,
    peer string,
    dial StreamDialer,
    idle time.Duration,
) (StreamInfo, error)
```

`leftReader == nil` means read directly from `left`. The separate reader exists only so MQTT can forward bytes already buffered after CONNECT while deadlines remain attached to the underlying client connection.

## Work units and ownership

### WU-01: Shared pump and raw TCP half-close

**Exclusive files:**

- `pkg/stream/bridge/bridge.go` (new)
- `pkg/stream/bridge/bridge_test.go` (new)
- `pkg/stream/router.go`
- `pkg/stream/router_test.go`

**Steps:**

- [ ] Add real TCP regression tests first: the client writes a request, calls `CloseWrite`, the upstream observes EOF, delays, then returns a complete response. Confirm the current raw bridge truncates it.
- [ ] Implement `bridge.Pump` with two directional results. Refresh source read and destination write deadlines on progress.
- [ ] On clean EOF, call destination `CloseWrite` when available and continue waiting. On context cancellation or a non-normalized hard error, close both sides to unblock the peer direction.
- [ ] Replace the router-local bridge/copy helpers with the shared package; keep raw TCP's current 60-second default and route read-timeout override.
- [ ] Preserve existing result normalization and close ownership; do not add retry, logging, PROXY protocol, or metrics behavior.

**Regression command:**

```bash
bash -lc 'source .envrc && go test ./pkg/stream -run "^(TestStreamBridgePreservesHalfClose|TestPump)" -count=1'
```

**Focused verification:**

```bash
bash -lc 'source .envrc && go test -race ./pkg/stream ./pkg/stream/bridge -run "^(TestStreamBridgePreservesHalfClose|TestPump|TestRouter)" -count=1'
```

### WU-02: MQTT replay deadline, half-close, and route idle timeout

**Depends on:** WU-01 fixed `bridge.Pump` API.

**Exclusive files:**

- `pkg/plugin/mqtt_proxy/stream.go`
- `pkg/plugin/mqtt_proxy/stream_test.go`
- `pkg/stream/router.go` only for the MQTT `ServeStreamWithIdle` call-site after WU-01 is accepted

**Steps:**

- [ ] Add fake-connection regression tests first that record `SetWriteDeadline(nonzero)` followed by `SetWriteDeadline(time.Time{})`; add a clear-deadline failure row. Avoid sleeping for the five-second preread deadline.
- [ ] Add a real TCP MQTT test: send a valid CONNECT plus payload, call client `CloseWrite`, have the broker observe EOF and send a delayed response, and assert the client receives the entire response.
- [ ] Change successful `writeStreamBytes` completion to clear the upstream write deadline. Return the reset error with MQTT replay context.
- [ ] Add `ServeStreamWithIdle`; keep `ServeStream` as a 60-second compatibility wrapper.
- [ ] Replace the MQTT-local copy goroutines with `bridge.Pump`, preserving the buffered client reader exactly once.
- [ ] Update the router MQTT closure to call `ServeStreamWithIdle(..., entry.streamIdleTimeout())`.

**Regression command:**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/mqtt_proxy -run "^(TestWriteStreamBytesClearsWriteDeadline|TestWriteStreamBytesReturnsClearDeadlineError|TestServeStreamPreservesHalfClose)" -count=1'
```

**Focused verification:**

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/mqtt_proxy -run "(ServeStream|WriteStreamBytes|Deadline|HalfClose)" -count=1'
```

### WU-03: Stream configuration validation, startup failure, and rollback

**Exclusive files:**

- `pkg/stream/runtime.go`
- `pkg/stream/runtime_test.go`
- `pkg/server/server.go`
- `pkg/server/stream_test.go`
- `pkg/server/server_test.go` only when the real `Start` rollback test belongs with existing startup fixtures
- `docs/design.md`
- `docs/plugins.md`
- `docs/configuration.md`

**Steps:**

- [ ] Add regression tests first for empty listener sets, UDP, TLS, listener PROXY protocol, upstream PROXY protocol, top-level TCP PROXY protocol flags, unresolved upstream, unsupported plugin, occupied address, multi-listener partial-bind rollback, valid listener, and HTTP-only mode.
- [ ] Make `NewRuntime` reject an empty spec list and every unsupported per-listener flag before publishing goroutines. Preserve its existing transactional close on any bind error.
- [ ] Change `startStreamProxy(ctx)` to return an error. Validate top-level stream configuration only when stream mode is enabled; load/resolve routes and propagate router/runtime errors; assign `s.streamRuntime` only after complete success.
- [ ] Make `Server.Start` propagate stream startup errors. If Prometheus startup or HTTP bind/serve later fails, close and clear the newly created stream runtime and join cleanup failure with the startup error.
- [ ] Add a reload regression proving an invalid new route returns an error while the previous route remains usable.
- [ ] Document the supported raw TCP/MQTT subset and fail-closed rejections. Do not claim TLS, UDP, PROXY protocol, general plugin chain, metrics, or a new readiness endpoint.

**Regression command:**

```bash
bash -lc 'source .envrc && go test ./pkg/server ./pkg/stream -run "^(TestStartStream|TestStreamStartup|TestNewRuntime|TestRuntimeReload)" -count=1'
```

**Focused verification:**

```bash
bash -lc 'source .envrc && go test -race ./pkg/server ./pkg/stream -run "(Stream|NewRuntime|RuntimeReload|StartFailure)" -count=1'
```

## Dependency order and dispatch

1. Complete and accept WU-01 first because it fixes the shared API.
2. Transfer `pkg/stream/router.go` ownership to WU-02 after WU-01 is accepted, then dispatch WU-02 and WU-03 in parallel. Their write paths are disjoint.
3. Each implementation worker gets at most three implementation/verification cycles or 20 minutes, one optional follow-up, and no commit/push/PR authority.
4. The phase owner inspects every report and the combined diff, then requests one independent merge-level review.

## Combined verification

```bash
bash -lc 'source .envrc && go test -race ./pkg/stream/bridge ./pkg/stream ./pkg/plugin/mqtt_proxy ./pkg/server -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/stream/bridge/... ./pkg/stream/... ./pkg/plugin/mqtt_proxy/... ./pkg/server/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

No broad `go test ./...` or full `t/plugin` run is required because this change has real socket unit coverage and does not modify an integration manifest.

## Delivery

- [ ] Independent reviewer reports APPROVE on the final frozen diff.
- [ ] Commit only the plan and accepted implementation paths with `git commit -m "fix(stream): preserve half-close and fail startup safely"`.
- [ ] Push `codex/prod-ready-stream-startup` and open one ready PR against `master`.
- [ ] Merge only after remote CI is green and the PR head matches the reviewed commit.
