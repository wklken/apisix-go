# PR #162 Pending Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan. Repository policy overrides this template: execute inline in the current task and do not delegate.

**Goal:** Implement every PR #162 adversarial-review item explicitly accepted for remediation, preserve deliberate APISIX-compatible behavior, and record deferred or accepted risks so later reviews can distinguish known decisions from regressions.

**Architecture:** Keep publication and generation ownership unchanged. Extend the existing immutable stream compiler, separate management status traffic from the data plane, apply strict-profile validation inside affected plugins, retain secret authority in generation-scoped runtime objects, and replace eager request-body duplication with one replayable snapshot. Every change is test-first and limited to its owning package.

**Tech Stack:** Go 1.26, net/http, GopherLua, repository generation/runtime APIs, capability manifest generator, focused Go tests.

## Global constraints

- Work only on `codex/pr162-review-remediation`, the existing PR #163 branch.
- Do not change the generation publication order or add direct mutable-store access.
- Do not hand-edit generated capability projections.
- Use `source .envrc && scripts/go_cache.sh run -- ...` for Go commands.
- Keep compat behavior where the decision ledger says compat is intentional; add strict-only enforcement at plugin initialization.

### Task 1: Deterministic stream matching

**Files:**
- Modify: `pkg/stream/router.go`
- Modify: `pkg/stream/router_test.go`

1. Add failing tests showing that exact IP beats CIDR, longer CIDR beats shorter CIDR independent of resource order, disjoint predicates coexist, and overlapping incomparable predicates fail compilation with both route IDs.
2. Run `go test ./pkg/stream -run 'Test(CompileRouter|Router)' -count=1` and capture the expected failures.
3. Model listener and remote predicates as sets. Sort comparable overlapping routes from narrower to broader; reject overlap when neither set contains the other.
4. Rerun the focused package tests and a focused race test.

```go
func compareStreamSpecificity(left, right compiledRoute) (specificityOrder, bool) {
	// Return narrower/broader/equal only when the predicates overlap and
	// containment is provable in every dimension.
}
```

### Task 2: Separate status listener and forwarded-header trust boundary

**Files:**
- Modify: `pkg/config/defaults.go`
- Modify: `pkg/config/validation.go`
- Modify: `pkg/config/validation_test.go`
- Modify: `pkg/server/server.go`
- Modify: `pkg/server/server_test.go`

1. Add failing config tests for the default `127.0.0.1:7085` status endpoint and invalid explicit status addresses.
2. Add failing server tests proving `/livez` and `/readyz` are ordinary data-plane paths, `/status` is live on a separate handler, `/status/ready` depends on committed serviceable config, and losing etcd after last-good publication does not make readiness false.
3. Add a failing test proving an untrusted peer's inbound X-Forwarded-For is cleared even when no trusted CIDRs are configured.
4. Implement a separately owned status `http.Server`, start and stop it with server lifecycle, and base readiness on committed config readiness rather than current etcd connectivity.
5. Make untrusted forwarding behavior unconditional; preserve the received chain only for an explicitly trusted peer.
6. Run focused config/server tests and server race tests.

```go
func newStatusHandler(readiness func() bool) http.Handler

// GET /status       -> 200 while the status listener is serving
// GET /status/ready -> 200 only after a committed serviceable generation
```

### Task 3: Strict-profile plugin boundaries and SLS wire authentication

**Files:**
- Modify: `pkg/plugin/chaitin_waf/plugin.go`
- Modify: `pkg/plugin/chaitin_waf/plugin_test.go`
- Modify: `pkg/plugin/csrf/plugin.go`
- Modify: `pkg/plugin/csrf/plugin_test.go`
- Modify: `pkg/plugin/referer_restriction/plugin.go`
- Modify: `pkg/plugin/referer_restriction/plugin_test.go`
- Modify: `pkg/plugin/sls_logger/plugin.go`
- Modify: `pkg/plugin/sls_logger/plugin_test.go`

1. Add failing tests for strict rejection of Chaitin debug response headers, strict CSRF Secure cookies, and strict rejection of ambiguous `*example.com` referer patterns; add compat tests preserving existing behavior.
2. Implement only the strict-profile validation or cookie attribute required by those tests.
3. Change the SLS test first so RFC5424 structured data must carry the configured access key ID and generation-private secret over the existing verified-TLS transport.
4. Retain the SLS secret in the plugin-owned secret value, expose it only while constructing a message, and clear the authority during `Stop`; keep diagnostics and exported configuration redacted.
5. Run each affected plugin package test and focused race tests for SLS lifecycle handling.

```go
func (p *Plugin) withAccessKeySecret(use func(string) error) error
```

### Task 4: Interruptible bounded serverless Lua

**Files:**
- Modify: `pkg/plugin/serverless/plugin.go`
- Modify: `pkg/plugin/serverless/plugin_test.go`

1. Add failing tests for infinite loops during `PostInit` and request execution, caller cancellation, unavailable OS/IO libraries, and invalid operator timeout settings.
2. Read an operator-only nonzero `plugin_attr.<factory>.execution_timeout_ms`, using a bounded default and hard ceiling; route configuration cannot override it.
3. Create Lua states with only the libraries required by the supported compatibility surface.
4. Attach a deadline context before both chunk evaluation and function invocation. Return only after GopherLua reports interruption, so the generation lease remains pinned until execution has actually stopped.
5. Run focused tests and race tests for both serverless plugin factories.

```go
const defaultExecutionTimeout = time.Second
const maxExecutionTimeout = 10 * time.Second
```

### Task 5: Factory-scoped plugin metadata secret authority

**Files:**
- Modify: `pkg/runtime/metadata.go`
- Modify: `pkg/runtime/metadata_test.go`
- Modify: `pkg/compiler/metadata_preparer.go`
- Modify: `pkg/compiler/metadata_preparer_test.go`
- Modify: `pkg/compiler/effective_binding_materializer.go`
- Modify: focused compiler tests covering dependency injection and cleanup

1. Add failing runtime tests proving a plugin can decode only its own metadata and that closing the generation metadata authority revokes all scoped views.
2. Add failing compiler tests proving materialized secrets are not stored in shared JSON and metadata closes during aborted/retired generation cleanup.
3. Store descriptors in canonical metadata JSON and keep resolved values in a generation-owned map keyed by factory and concrete JSON pointer.
4. Scope the metadata view before injecting dependencies into each plugin factory. Insert the secret only into that factory's decode copy, then release it after all plugin instances stop.
5. Run focused runtime/compiler tests and race tests.

```go
type MetadataDocument struct {
	Document []byte
	Secrets  map[string]secret.Value
}

func (v MetadataView) ForFactory(factory string) MetadataView
func (v MetadataView) Close()
```

### Task 6: One replayable HMAC request-body snapshot

**Files:**
- Add: `pkg/plugin/base/request_body_snapshot.go`
- Add: `pkg/plugin/base/request_body_snapshot_test.go`
- Modify: `pkg/plugin/hmac_auth/plugin.go`
- Modify: `pkg/plugin/hmac_auth/plugin_test.go`
- Modify: `pkg/plugin/multi_auth/plugin.go`
- Modify: `pkg/plugin/multi_auth/plugin_test.go`

1. Add failing snapshot tests for streaming SHA-256, replay, memory-to-secure-temp-file spill, limit rejection, repeated readers, and close/removal.
2. Add failing HMAC/multi-auth tests showing one captured snapshot is reused across probes and downstream, and the effective cap is the lower of plugin and ingress limits.
3. Implement a request-scoped snapshot that hashes during capture, retains only a small prefix in memory, spills above the threshold to a mode-0600 temporary file, and deletes it from the request lifecycle finalizer.
4. Make HMAC validate the cached digest and restore the body from the snapshot. Make multi-auth reuse the same snapshot instead of `TeeReader` plus a second `ReadAll` allocation.
5. Preserve `max_req_body_size` defaults and error behavior. Run focused base/HMAC/multi-auth tests and race tests.

```go
type RequestBodySnapshot struct {
	// immutable size, digest and replay backing; cleanup is idempotent
}
```

### Task 7: Durable review decisions and capability truth

**Files:**
- Add: `docs/reviews/2026-08-26-pr162-adversarial-review-decisions.md`
- Add: `docs/architecture/adr/0004-runtime-safety-boundaries.md`
- Modify: `docs/design.md`
- Modify: `docs/production-profile.md`
- Modify: `pkg/capability/manifest.yaml`
- Generate: capability projections owned by the manifest generator

1. Record every reviewed item with one of: fixed here, strict-only hardening, accepted compat behavior, or deferred structural work.
2. Explicitly record deferred consumer plaintext redesign and context-aware wolf-rbac retry, plus the accepted CSRF double-submit/compat token format and APISIX-compatible referer/Chaitin behavior.
3. Document the status listener/readiness contract, deterministic stream routing, bounded serverless runtime, SLS wire-auth behavior, metadata authority, and body snapshot ownership.
4. Add the accepted Go-native runtime-safety divergence and update affected manifest entries. Generate and check projections.

### Task 8: Completion gates and delivery

1. Format only touched Go files and inspect the resulting diff.
2. Run all focused package tests collected above, relevant race gates, capability generation check, `make lint`, and `make build`.
3. Run `git diff --check`, search moved/added APIs for unexpected call sites, and confirm no unrelated changes.
4. Review the full diff against the decision ledger, commit with a specific Conventional Commit message, push the existing branch, and verify PR #163 head/check state.
