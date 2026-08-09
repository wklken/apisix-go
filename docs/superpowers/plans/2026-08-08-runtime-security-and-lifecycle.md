# Runtime Security and Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fail closed when Casdoor randomness is unavailable, release shared limiter registries with plugin owners, remove repeated OpenID static-key parsing, remove an invalid traffic-split deadline approximation, and clear MQTT preread deadlines.

**Architecture:** Use existing Builder `Stop()` ownership as the lifecycle boundary. Shared limiter entries gain reference counts so a new route generation can acquire before the old generation releases; immutable OpenID key material moves to `PostInit`; timeout fixes remove or clear state at the narrowest owner.

**Tech Stack:** Go 1.26, `crypto/rand`, `sync`, `net/http`, `net.Conn`, existing plugin lifecycle and unit tests.

## File Structure

- Modify `pkg/plugin/authz_casdoor/plugin.go` and its tests: make entropy failure explicit and fail closed.
- Modify `pkg/plugin/limit_req`, `pkg/plugin/limit_count`, and `pkg/plugin/graphql_limit_count`: add acquire/release ownership to shared registries.
- Modify `pkg/plugin/openid_connect/plugin.go` and its tests: parse static public keys during initialization.
- Modify `pkg/plugin/traffic_split/plugin.go` and its tests: remove the unsafe whole-request deadline approximation.
- Modify `pkg/plugin/mqtt_proxy/plugin.go` and its tests: clear the CONNECT preread deadline before tunneling.

## Global Constraints

- Run every Go command as `bash -lc 'source .envrc && ...'` from the repository root.
- Do not add dependencies or change public plugin schemas.
- All `Stop()` methods must remain idempotent.
- Shared limiter state must survive overlapping old/new Builder generations: acquire the new reference before releasing the old one.
- Do not replace phase-specific connect/send/read semantics with another whole-request approximation.
- Use impact-scoped tests, focused race tests for shared registries, then `make build`; do not run `go test ./...` or `make test`.
- Subagents require explicit user authorization under `AGENTS.md`; inline execution is the default.

---

### Task 1: Fail closed on authz-casdoor random-source errors

**Files:**
- Modify: `pkg/plugin/authz_casdoor/plugin.go`
- Test: `pkg/plugin/authz_casdoor/plugin_test.go`

**Interfaces:**
- Consumes: `io.Reader`, defaulting to `crypto/rand.Reader`.
- Produces: `randomState(reader io.Reader) (string, error)` and `Plugin.newState func() (string, error)`; redirect returns 500 without storing a session or cookie on failure.

- [ ] **Step 1: Add a deterministic failing reader and helper test**

```go
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

func TestRandomStateFailsClosed(t *testing.T) {
	state, err := randomState(failingReader{})
	if err == nil || state != "" {
		t.Fatalf("randomState() = %q, %v; want empty state and error", state, err)
	}
}
```

- [ ] **Step 2: Add a handler-level failure test**

Construct a valid test plugin, set `p.newState = func() (string, error) { return "", errors.New("entropy unavailable") }`, request a protected URL, and assert status 500, no `Set-Cookie`, and `p.sessions.Len() == 0`.

- [ ] **Step 3: Verify both tests fail with the current fallback contract**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/authz_casdoor -run "TestRandomStateFailsClosed|TestHandlerRejectsRandomStateFailure" -count=1'`

Expected: FAIL because `randomState` cannot return an error and the handler always creates a redirect session.

- [ ] **Step 4: Change the random contract and redirect flow**

Implement:

```go
func randomState(reader io.Reader) (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", fmt.Errorf("generate random state: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
```

Initialize `p.newState` with `func() (string, error) { return randomState(rand.Reader) }`. In `redirectToAuthorize`, obtain both `sessionID` and `state` through `p.newState`; on either error, log it and return `http.StatusInternalServerError` before `saveSession` or `setSessionCookie`. Update existing test lambdas from `func() string` to `func() (string, error)`.

- [ ] **Step 5: Run package tests and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/authz_casdoor -count=1'`

Expected: PASS.

```bash
git add pkg/plugin/authz_casdoor/plugin.go pkg/plugin/authz_casdoor/plugin_test.go
git commit -m "fix(authz-casdoor): fail closed without secure randomness"
```

### Task 2: Reference-count limit-count group stores

**Files:**
- Modify: `pkg/plugin/limit_count/plugin.go`
- Test: `pkg/plugin/limit_count/plugin_test.go`

**Interfaces:**
- Consumes: `Plugin.PostInit()` / `Plugin.Stop()` lifecycle.
- Produces: `limitCountGroup.refs int`, `Plugin.groupRegistered bool`, `releaseGroup()`; the final owner deletes `limitCountGroups.entries[group]`.

- [ ] **Step 1: Add lifecycle tests for one and two owners**

Add tests that reset the global map under its mutex, create two plugins with the same local `Group`, assert one entry with `refs == 2`, stop the first and assert `refs == 1`, stop the second and assert the entry is absent. Call `Stop()` a second time and assert it remains absent.

Use a config with static `Count`, `TimeWindow`, `Policy: "local"`, and `Group: "shared"` so no external client is involved.

- [ ] **Step 2: Verify the test fails because entries have no owner count**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/limit_count -run "^TestGroupRegistryReleasesLastOwner$" -count=1'`

Expected: FAIL to compile until `refs` exists, then fail until Stop releases.

- [ ] **Step 3: Add acquisition and idempotent release**

Extend the entry:

```go
type limitCountGroup struct {
	fingerprint string
	store       limiter.Store
	refs        int
}
```

On matching registration increment `refs`; initialize new entries with `refs: 1`; set `p.groupRegistered = true` only after successful registration. Implement:

```go
func (p *Plugin) releaseGroup() {
	if !p.groupRegistered || p.config.Group == "" {
		return
	}
	limitCountGroups.Lock()
	entry, ok := limitCountGroups.entries[p.config.Group]
	if ok {
		entry.refs--
		if entry.refs == 0 {
			delete(limitCountGroups.entries, p.config.Group)
		} else {
			limitCountGroups.entries[p.config.Group] = entry
		}
	}
	limitCountGroups.Unlock()
	p.groupRegistered = false
}
```

Call it from `Stop()`. If initialization after `registerGroup` fails, call `releaseGroup()` before returning the error so failed instances do not leak references.

- [ ] **Step 4: Run package and race tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/limit_count -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/limit_count -run "GroupRegistry" -count=1'
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/limit_count/plugin.go pkg/plugin/limit_count/plugin_test.go
git commit -m "fix(limit-count): release shared group stores"
```

### Task 3: Reference-count graphql-limit-count group fingerprints

**Files:**
- Modify: `pkg/plugin/graphql_limit_count/plugin.go`
- Test: `pkg/plugin/graphql_limit_count/plugin_test.go`

**Interfaces:**
- Consumes: the same plugin lifecycle as Task 2.
- Produces: `graphqlLimitCountGroup{fingerprint string, refs int}` plus final-owner deletion.

- [ ] **Step 1: Add the same two-owner lifecycle test**

Use two local-policy plugins with the same group and static count/window. Assert `refs` transitions `2 -> 1 -> absent` and double Stop is harmless.

- [ ] **Step 2: Replace the string map with an owned entry**

Use:

```go
type graphqlLimitCountGroup struct {
	fingerprint string
	refs        int
}

var graphqlLimitCountGroups = struct {
	sync.Mutex
	entries map[string]graphqlLimitCountGroup
}{entries: map[string]graphqlLimitCountGroup{}}
```

Mirror Task 2's acquire, rollback-on-PostInit-error, and `releaseGroup` rules. Keep fingerprint mismatch behavior unchanged.

- [ ] **Step 3: Run package and race tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/graphql_limit_count -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/graphql_limit_count -run "GroupRegistry" -count=1'
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/plugin/graphql_limit_count/plugin.go pkg/plugin/graphql_limit_count/plugin_test.go
git commit -m "fix(graphql-limit-count): release group registry entries"
```

### Task 4: Give limit-req consumer bucket stores explicit owners

**Files:**
- Modify: `pkg/plugin/limit_req/plugin.go`
- Test: `pkg/plugin/limit_req/plugin_test.go`

**Interfaces:**
- Consumes: config UID from `shared.NewConfigUID()` and lazy consumer-override access.
- Produces: mutex-protected `consumerBucketEntry{store *bucketStore, refs int}` registry; each plugin instance acquires at most once and releases in `Stop()`.

- [ ] **Step 1: Add a lazy acquisition/release test**

Create two plugins with identical local configs, call `consumerBucketStore()` on both, assert they receive the same store and the registry has one entry with two refs, then stop each and assert `2 -> 1 -> absent`. Also assert calling `Stop()` before `consumerBucketStore()` creates no registry entry.

- [ ] **Step 2: Replace the unowned `sync.Map`**

Use:

```go
type consumerBucketEntry struct {
	store *bucketStore
	refs  int
}

var consumerBucketStores = struct {
	sync.Mutex
	entries map[string]consumerBucketEntry
}{entries: map[string]consumerBucketEntry{}}
```

Add a separate `consumerStoreMu`, `consumerStoreKey`, and `consumerStore *bucketStore` to `Plugin`. Under `consumerStoreMu`, compute the UID once; under the global mutex, reuse/create the entry and increment refs. In `Stop()`, detach the plugin fields under `consumerStoreMu`, then decrement/delete under the global mutex. Do not use `p.mu`, which guards rate buckets and would invert lock order with request processing.

- [ ] **Step 3: Run package and race tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/limit_req -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/limit_req -run "ConsumerBucketStore" -count=1'
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/plugin/limit_req/plugin.go pkg/plugin/limit_req/plugin_test.go
git commit -m "fix(limit-req): release consumer bucket stores"
```

### Task 5: Parse OpenID static public keys once

**Files:**
- Modify: `pkg/plugin/openid_connect/plugin.go`
- Modify: `pkg/plugin/openid_connect/verify.go`
- Test: `pkg/plugin/openid_connect/plugin_test.go`

**Interfaces:**
- Consumes: secret-resolved `p.config.PublicKey` during `PostInit`.
- Produces: `Plugin.staticPublicKey crypto.PublicKey`; request verification never calls PEM/x509 parsing.

- [ ] **Step 1: Add a PostInit parse test**

Create a test plugin with `PublicKey: publicKeyPEM(t, &privateKey.PublicKey)` and assert `p.staticPublicKey != nil`. Add a second case with invalid PEM and assert `PostInit()` returns `failed to parse public key`.

- [ ] **Step 2: Verify the field/test does not yet exist**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/openid_connect -run "^TestPostInitParsesStaticPublicKey$" -count=1'`

Expected: FAIL to compile before the field is added.

- [ ] **Step 3: Parse after secret resolution**

Add `staticPublicKey crypto.PublicKey` to `Plugin`. Immediately after resolving `p.config.PublicKey` in `PostInit`, parse it when non-empty:

```go
if p.config.PublicKey != "" {
	publicKey, err := parsePublicKey([]byte(p.config.PublicKey))
	if err != nil {
		return errors.New("failed to parse public key")
	}
	p.staticPublicKey = publicKey
}
```

Change `staticKeyVerifier` to use `p.staticPublicKey` and return an error if it is nil. Keep algorithm selection per token and do not add a verifier cache until profiling proves construction itself matters.

- [ ] **Step 4: Run package tests and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/openid_connect -count=1'`

Expected: PASS.

```bash
git add pkg/plugin/openid_connect/plugin.go pkg/plugin/openid_connect/verify.go pkg/plugin/openid_connect/plugin_test.go
git commit -m "perf(openid-connect): parse static public key once"
```

### Task 6: Remove traffic-split's invalid whole-request timeout

**Files:**
- Modify: `pkg/plugin/traffic_split/plugin.go`
- Test: `pkg/plugin/traffic_split/plugin_test.go`
- Modify: `docs/plugins.md`

**Interfaces:**
- Consumes: selected upstream `resource.Timeout`, which remains preserved in `Override`.
- Produces: handler does not derive a context deadline from connect/send/read; exact enforcement remains explicitly deferred.

- [ ] **Step 1: Replace the current timeout test with the correct boundary**

Rename `TestHandlerAppliesSelectedUpstreamTimeoutToRequestContext` to `TestHandlerDoesNotCollapsePhaseTimeoutsIntoOverallDeadline` and assert:

```go
performRequestWithHandler(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Context().Deadline(); ok {
		t.Fatal("selected phase timeouts must not become one whole-request deadline")
	}
	override := GetOverride(r)
	if override == nil || override.Timeout.Connect != 3 || override.Timeout.Send != 2 || override.Timeout.Read != 60 {
		t.Fatalf("override timeout = %#v, want connect=3 send=2 read=60", override)
	}
	w.WriteHeader(http.StatusNoContent)
}))
```

- [ ] **Step 2: Verify the test fails with the current minimum deadline**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/traffic_split -run "^TestHandlerDoesNotCollapsePhaseTimeoutsIntoOverallDeadline$" -count=1'`

Expected: FAIL because the context has a deadline near two seconds.

- [ ] **Step 3: Delete the approximation**

Remove the `context.WithTimeout` block from `Plugin.Handler`, delete `upstreamTimeout`, and remove the now-unused `context`/`time` imports only if no other code uses them. Preserve `Override.Timeout` for the future transport contract.

- [ ] **Step 4: Run package tests and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/traffic_split -count=1'`

Expected: PASS.

```bash
git add pkg/plugin/traffic_split/plugin.go pkg/plugin/traffic_split/plugin_test.go docs/plugins.md
git commit -m "fix(traffic-split): remove unsafe aggregate deadline"
```

### Task 7: Clear MQTT CONNECT preread deadline

**Files:**
- Modify: `pkg/plugin/mqtt_proxy/stream.go`
- Test: `pkg/plugin/mqtt_proxy/stream_test.go`

**Interfaces:**
- Consumes: `net.Conn.SetReadDeadline`.
- Produces: successful `readConnectFromStream` clears the deadline before returning raw CONNECT bytes.

- [ ] **Step 1: Add a recording connection test double**

```go
type readDeadlineConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *readDeadlineConn) SetReadDeadline(deadline time.Time) error {
	c.deadlines = append(c.deadlines, deadline)
	return c.Conn.SetReadDeadline(deadline)
}
```

Use `net.Pipe`, wrap the gateway side, write a valid CONNECT packet from the client, call `readConnectFromStream`, and assert exactly two recorded deadlines with the last `IsZero()`.

- [ ] **Step 2: Verify the test sees only the preread deadline**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/mqtt_proxy -run "^TestReadConnectClearsPrereadDeadline$" -count=1'`

Expected: FAIL because only the non-zero deadline is recorded.

- [ ] **Step 3: Clear the deadline after successful decode**

After `decodeConnect` succeeds, add:

```go
if err := conn.SetReadDeadline(time.Time{}); err != nil {
	return nil, ConnectInfo{}, fmt.Errorf("mqtt CONNECT clear read deadline: %w", err)
}
```

Do not clear on decode failure because the connection is about to be closed.

- [ ] **Step 4: Run package tests and commit**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/mqtt_proxy -count=1'`

Expected: PASS.

```bash
git add pkg/plugin/mqtt_proxy/stream.go pkg/plugin/mqtt_proxy/stream_test.go
git commit -m "fix(mqtt-proxy): clear CONNECT preread deadline"
```

### Task 8: Final lifecycle verification

**Files:**
- Verify only; no planned file changes.

**Interfaces:**
- Consumes: Tasks 1-7.
- Produces: package, race, lint, and build evidence.

- [ ] **Step 1: Run all affected packages**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/authz_casdoor ./pkg/plugin/limit_count ./pkg/plugin/graphql_limit_count ./pkg/plugin/limit_req ./pkg/plugin/openid_connect ./pkg/plugin/traffic_split ./pkg/plugin/mqtt_proxy -count=1'`

Expected: PASS.

- [ ] **Step 2: Run the shared-state race gate**

Run: `bash -lc 'source .envrc && go test -race ./pkg/plugin/limit_count ./pkg/plugin/graphql_limit_count ./pkg/plugin/limit_req -count=1'`

Expected: PASS.

- [ ] **Step 3: Run lint, build, and diff checks**

Run:

```bash
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build'
git diff --check
```

Expected: all PASS. Run `bash -lc 'source .envrc && make clean'` afterward when the binary is not needed.
