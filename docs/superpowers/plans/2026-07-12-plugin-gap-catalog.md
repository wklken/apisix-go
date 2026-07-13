# Plugin Gap Catalog Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the bounded proxy-mirror HTTP/2 request gap, fail fast on the dependency-blocked RocketMQ TLS option, and synchronize the plugin gap catalog with verified behavior.

**Architecture:** Keep `proxy-mirror` as an HTTP middleware, but give its outbound client the repository's existing HTTP/2-capable transport from `pkg/proxy`. Treat RocketMQ TLS as an explicit dependency boundary: reject `use_tls: true` during plugin initialization rather than silently using plaintext. Keep APISIX DNS resolver behavior, RocketMQ TLS transport support, and native/runtime gaps documented as deferred.

**Tech Stack:** Go 1.26.4, `net/http`, `golang.org/x/net/http2`, existing APISIX plugin/schema/test patterns, Markdown documentation.

## Global Constraints

- Keep changes surgical and limited to the catalog items audited in the current checkout.
- Use the existing `pkg/proxy.NewTransport` implementation; do not add a dependency or fork the RocketMQ client.
- Preserve the current Go-native bounded behavior and do not claim OpenResty/NGINX/Lua parity.
- Run `source .envrc` before Go commands and keep generated artifacts out of the diff.

---

### Task 1: Lock the two catalog decisions with regression tests

**Files:**
- Modify: `pkg/plugin/proxy_mirror/plugin_test.go`
- Modify: `pkg/plugin/rocketmq_logger/plugin_test.go`

**Interfaces:**
- Consumes: existing `Plugin`, `Config`, `PostInit`, and test helpers in each package.
- Produces: tests proving HTTP/2 request metadata/body preservation and explicit RocketMQ TLS rejection.

- [x] **Step 1: Add the HTTP/2 transport regression test**

After `PostInit`, assert the plugin client owns an `*http.Transport` with configured HTTP/2 support. Add a TLS `httptest.Server` integration test using the server's HTTP/2-capable client to send a unary gRPC-shaped request with `Content-Type: application/grpc`; assert the mirror observes HTTP/2, the content type, the request body, and the original handler still receives the same body.

- [x] **Step 2: Run the focused test to verify it fails**

Run: `source .envrc && go test ./pkg/plugin/proxy_mirror -run 'TestPostInitConfiguresHTTP2Transport|TestHandlerMirrorsUnaryGRPCOverHTTP2' -count=1`

Expected: FAIL because `PostInit` currently constructs a default client without an explicit HTTP/2-capable transport.

- [x] **Step 3: Add the RocketMQ TLS rejection test**

Construct a plugin with `UseTLS: true` and a capture sender so producer creation is not reached. Assert `PostInit` returns an error containing `use_tls` and `not supported`.

- [x] **Step 4: Run the focused test to verify it fails**

Run: `source .envrc && go test ./pkg/plugin/rocketmq_logger -run TestPostInitRejectsUnsupportedTLS -count=1`

Expected: FAIL because `PostInit` currently ignores `UseTLS`.

---

### Task 2: Implement the bounded transport and safety behavior

**Files:**
- Modify: `pkg/plugin/proxy_mirror/plugin.go`
- Modify: `pkg/plugin/rocketmq_logger/plugin.go`

**Interfaces:**
- Consumes: `proxy.NewTransport` and the existing plugin initialization paths.
- Produces: an HTTP/2-capable mirror client and explicit RocketMQ TLS configuration failure.

- [x] **Step 1: Configure proxy-mirror with the shared HTTP/2 transport**

Replace the default mirror client transport with the existing repository transport:

```go
transport := proxy.NewTransport((&proxy.TransportOptionBuilder{}).Build())
p.client = &http.Client{
	Timeout:   5 * time.Second,
	Transport: transport,
}
```

Keep the existing timeout and request construction behavior unchanged.

- [x] **Step 2: Reject unsupported RocketMQ TLS before producer creation**

At the start of `rocketmq_logger.Plugin.PostInit`, return:

```go
if p.config.UseTLS {
	return fmt.Errorf("rocketmq-logger use_tls is not supported by rocketmq-client-go/v2")
}
```

Leave the schema field intact so configuration remains APISIX-compatible, but ensure a true value cannot silently send over plaintext.

- [x] **Step 3: Run the focused tests**

Run:

```bash
source .envrc && go test ./pkg/plugin/proxy_mirror ./pkg/plugin/rocketmq_logger -count=1
```

Expected: PASS.

---

### Task 3: Synchronize the plugin status artifact

**Files:**
- Modify: `docs/plugins.md`

**Interfaces:**
- Consumes: verified package tests and the pinned RocketMQ dependency audit.
- Produces: one consistent matrix row and remaining-gap catalog without duplicate entries or overstated support.

- [x] **Step 1: Update the proxy-mirror matrix and catalog rows**

State that bounded HTTP/2/unary gRPC-shaped request mirroring is supported through the shared Go transport, while APISIX DNS resolver behavior and streaming-specific phase fidelity remain deferred.

- [x] **Step 2: Update the RocketMQ boundary wording**

State that `use_tls: true` fails during initialization because the pinned RocketMQ client has no TLS transport hook; keep TLS support as an explicit dependency-bound gap.

- [x] **Step 3: Recheck documentation uniqueness**

Recheck the `AI` heading and `ip-restriction` row after the edit. The earlier apparent duplicates came from overlapping inspection ranges, so leave the single live heading/row intact and do not alter counts or unrelated plugin status.

- [x] **Step 4: Review the documentation diff**

Run: `git diff --check -- docs/plugins.md`

Expected: no whitespace errors and no duplicate catalog headings/rows introduced.

---

### Task 4: Run the repository completion gates

**Files:**
- Verify: all touched files and generated artifact state.

- [x] **Step 1: Run focused package tests**

Run: `source .envrc && go test ./pkg/plugin/proxy_mirror ./pkg/plugin/rocketmq_logger -count=1`

- [x] **Step 2: Run the full repository tests**

Run: `source .envrc && go test ./... -count=1`

- [x] **Step 3: Run the build smoke check**

Run: `source .envrc && make build`

Expected: successful build; remove the ignored `apisix` binary after the check if present.

- [x] **Step 4: Run final diff checks**

Run: `git diff --check && git status --short`

Expected: only the plan, plugin/test changes, and `docs/plugins.md` are present; no generated artifact is tracked.
