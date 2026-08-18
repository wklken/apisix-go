# Verified Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remediate every valid finding in `docs/report.md`, preserve the rejected local-vendor finding as evidence only, and deliver one reviewable PR whose findings remain separated by commit.

**Architecture:** Apply fixes in dependency order. Each behavior change starts with a focused failing regression, changes only its owning subsystem, updates the remediation ledger, passes impact-scoped verification, and becomes one independent commit. Permanent configuration validation failures use typed quarantine while transient store failures continue to retry; encryption writes move to a versioned AEAD envelope while legacy CBC remains read-only.

**Tech Stack:** Go 1.26, `net/http`, bbolt, etcd client v3, Prometheus client, AES-GCM, existing package tests and `t/plugin` manifests.

**Spec:** `docs/report.md`

## Global Constraints

- Source `.envrc` before every Go, lint, build, or test command.
- Do not run repository-wide `go test ./...`, `go test ./pkg/...`, or `make test`; use exact affected packages.
- Preserve unrelated untracked and ignored files, including the locally regenerated ignored `vendor/` tree.
- `BUG-001` is rejected as a repository finding and gets no empty commit.
- One finding per implementation commit; the consolidated report is a separate documentation commit.
- No dependency additions.
- Final publication is one `codex/remediate-audit-findings` PR.

---

### Task 1: Publish the verified source report and execution plan

**Files:**
- Create: `docs/report.md`
- Create: `docs/superpowers/plans/2026-08-18-review-remediation.md`

**Interfaces:**
- Consumes: verified findings at `master@54f09952fe290014f72da519d2557a80a5b543f0`.
- Produces: immutable finding IDs and the commit/test contract followed below.

- [x] **Step 1: Validate the document structure and finding inventory**

Run:

```bash
for id in BUG-001 SEC-001 SEC-002 BUG-002 BUG-003 BUG-004 BUG-005 BUG-006 BUG-007 SEC-003; do
  rg -q "$id" docs/report.md
done
rg -n '[[:blank:]]+$' docs/report.md docs/superpowers/plans/2026-08-18-review-remediation.md
```

Expected: every ID is present; the trailing-whitespace search prints nothing.

- [x] **Step 2: Commit the documentation baseline**

```bash
git add docs/report.md docs/superpowers/plans/2026-08-18-review-remediation.md
git commit -m "docs: consolidate verified review findings"
```

### Task 2: SEC-001 — bind locally verified OIDC JWTs to issuer and audience

**Files:**
- Modify: `pkg/plugin/openid_connect/verify.go`
- Modify: `pkg/plugin/openid_connect/plugin.go`
- Modify: `pkg/plugin/openid_connect/plugin_test.go`
- Modify: `t/plugin/openid-connect.yaml`
- Modify: `docs/plugins.md`
- Create: `docs/review-remediation-ledger-2026-08-18.md`

**Interfaces:**
- Consumes: issuer allowlist/discovery, `client_id`, and JWT/introspection claims.
- Produces: `claim_validator.audience.valid_audiences`; local JWTs require a trusted issuer and expected audience.

- [ ] **Step 1: Retain the observed RED/GREEN evidence in the ledger**
- [ ] **Step 2: Run scoped OIDC lint, package tests, manifest selection, the 28 affected integration cases, and build**
- [ ] **Step 3: Commit only the OIDC implementation, tests, fixture, plugin status, and ledger**

Commit:

```bash
git commit -m "fix(openid-connect): require trusted JWT issuer and audience"
```

### Task 3: SEC-002 — redact sensitive default logger headers

**Files:**
- Modify: `pkg/plugin/loki_logger/plugin.go`
- Modify: `pkg/plugin/loki_logger/plugin_test.go`
- Modify: `pkg/plugin/sls_logger/plugin.go`
- Modify: `pkg/plugin/sls_logger/plugin_test.go`
- Modify: `pkg/plugin/splunk_hec_logging/plugin.go`
- Modify: `pkg/plugin/splunk_hec_logging/plugin_test.go`
- Modify: `docs/review-remediation-ledger-2026-08-18.md`

**Interfaces:**
- Consumes: `base.CollapseAccessLogHeaderValues(http.Header)`.
- Produces: default snapshot and legacy payloads that omit request credentials and response cookies; explicit custom `log_format` remains caller-owned.

- [ ] **Step 1: Add failing tests for Authorization/Cookie/Set-Cookie removal and benign-header retention in snapshot and legacy builders**
- [ ] **Step 2: Run the named tests and observe exposed sensitive fields**
- [ ] **Step 3: Replace default header normalization/cloning with `CollapseAccessLogHeaderValues`; keep query strings and custom formats unchanged**
- [ ] **Step 4: Run all three logger packages, scoped lint, update the ledger, and commit**

Commit:

```bash
git commit -m "fix(logging): redact sensitive default headers"
```

### Task 4: BUG-002 — enforce configured memory cache-zone capacity

**Files:**
- Modify: `pkg/plugin/proxy_cache/zones.go`
- Modify: `pkg/plugin/proxy_cache/plugin.go`
- Modify: `pkg/plugin/proxy_cache/response_phase.go`
- Modify: `pkg/plugin/proxy_cache/plugin_test.go`
- Modify: `pkg/plugin/graphql_proxy_cache/response_phase.go`
- Modify: `pkg/plugin/graphql_proxy_cache/plugin_test.go`
- Modify: `docs/review-remediation-ledger-2026-08-18.md`

**Interfaces:**
- Consumes: parsed `Zone.MemorySize` and the shared `memoryZone` lock.
- Produces: atomic byte accounting and oldest-entry eviction shared by proxy-cache and graphql-proxy-cache; an entry larger than the zone is not retained.

- [ ] **Step 1: Add failing tests for multiple unique entries, overwrite accounting, one oversized entry, Vary storage, and GraphQL shared storage**
- [ ] **Step 2: Run the named capacity tests and observe unbounded retention**
- [ ] **Step 3: Store parsed capacity; under the zone lock account for key/body/header/metadata/Vary bytes and evict oldest entries until within capacity**
- [ ] **Step 4: Run both packages normally and with race, scoped lint, update the ledger, and commit**

Commit:

```bash
git commit -m "fix(proxy-cache): enforce memory zone capacity"
```

### Task 5: BUG-003 — make request body and read-time bounds default

**Files:**
- Modify: `pkg/config/init.go`
- Modify: `pkg/config/init_test.go`
- Modify: `pkg/plugin/function_upstream/plugin.go`
- Modify: `pkg/plugin/function_upstream/plugin_test.go`
- Modify: `docs/configuration.md`
- Modify: `docs/review-remediation-ledger-2026-08-18.md`

**Interfaces:**
- Consumes: existing `server.limitRequestBody`, `client_body_timeout`, and `base.ReadRequestBody`.
- Produces: omitted settings default to 10 MiB and 60 seconds; explicitly non-positive settings fail load; function-upstream uses the canonical body cache.

- [ ] **Step 1: Add failing tests for omitted defaults, explicit zero rejection, and function-upstream propagation of `http.MaxBytesError`**
- [ ] **Step 2: Run the focused tests and observe zero/unbounded behavior**
- [ ] **Step 3: Install Viper defaults before decode, validate both values are positive for every profile, and replace raw `io.ReadAll(r.Body)` with `base.ReadRequestBody(r)`**
- [ ] **Step 4: Run config/server/function-upstream tests, scoped lint, update docs/ledger, and commit**

Commit:

```bash
git commit -m "fix(config): bound request bodies by default"
```

### Task 6: BUG-004 — quarantine permanent etcd resource validation failures

**Files:**
- Modify: `pkg/store/event.go`
- Modify: `pkg/store/store.go`
- Modify: `pkg/store/store_test.go`
- Modify: `pkg/etcd/watcher.go`
- Modify: `pkg/etcd/watcher_test.go`
- Modify: `pkg/observability/metrics/runtime_lifecycle.go`
- Modify: `pkg/observability/metrics/runtime_lifecycle_test.go`
- Modify: `pkg/observability/metrics/prometheus.go`
- Modify: `docs/review-remediation-ledger-2026-08-18.md`

**Interfaces:**
- Produces: `store.ResourceValidationError`, watcher quarantine keyed by full etcd key and mod revision, and a bounded quarantine gauge.
- Preserves: bbolt/I/O failures remain retryable and cannot advance watcher revision.

- [ ] **Step 1: Add failing tests with a permanent invalid SSL/consumer plus an unrelated valid route, and a separate transient persistence failure**
- [ ] **Step 2: Verify the permanent invalid resource loops recovery and the transient error is indistinguishable**
- [ ] **Step 3: Wrap only deterministic validation errors; skip only that typed error, retain last-good state, continue other events, advance revision, and clear quarantine on valid replacement/delete**
- [ ] **Step 4: Keep readiness false while quarantine exists; run etcd/store/metrics tests and races, update the ledger, and commit**

Commit:

```bash
git commit -m "fix(etcd): quarantine invalid resource updates"
```

### Task 7: BUG-005 — isolate malformed HTTP route resources

**Files:**
- Modify: `pkg/store/store.go`
- Modify: `pkg/store/getter.go`
- Modify: `pkg/store/getter_test.go`
- Modify: `pkg/route/builder.go`
- Modify: `pkg/route/builder_test.go`
- Modify: `pkg/server/reload.go`
- Modify: `pkg/server/reload_test.go`
- Modify: `docs/design.md`
- Modify: `docs/review-remediation-ledger-2026-08-18.md`

**Interfaces:**
- Consumes: BUG-004 typed validation/quarantine contract.
- Produces: write-boundary route/global-rule validation and explicit legacy-store quarantine metadata; valid routes publish while readiness remains degraded.

- [ ] **Step 1: Add failing tests proving malformed new events preserve prior good objects and legacy malformed bbolt entries no longer freeze valid routes**
- [ ] **Step 2: Run focused store/route/server tests and observe strict snapshot failure**
- [ ] **Step 3: Validate route/global-rule PUT values before persistence; during legacy snapshot rebuild skip malformed entries into an immutable quarantine list**
- [ ] **Step 4: Install the valid router generation but report degraded readiness; run focused packages, update design/ledger, and commit**

Commit:

```bash
git commit -m "fix(store): isolate malformed route resources"
```

### Task 8: BUG-006 — preserve scheme-aware default upstream ports

**Files:**
- Modify: `pkg/resource/route.go`
- Modify: `pkg/resource/route_test.go`
- Modify: `pkg/route/builder_test.go`
- Modify: `docs/review-remediation-ledger-2026-08-18.md`

**Interfaces:**
- Produces: map nodes without an explicit port retain `Port == 0`; the builder remains the owner of HTTP/gRPC 80 and HTTPS/gRPCS 443 defaults.

- [ ] **Step 1: Add a failing HTTP/HTTPS/gRPC/gRPCS matrix for map/list, host/IP/IPv6, explicit and omitted ports**
- [ ] **Step 2: Remove the parser's unconditional map-form port 80 assignment**
- [ ] **Step 3: Run resource/route tests and lint, update the ledger, and commit**

Commit:

```bash
git commit -m "fix(route): preserve scheme-aware node ports"
```

### Task 9: BUG-007 — reclaim and cap delayed-sync key state

**Files:**
- Modify: `pkg/plugin/limit_count/delayed_sync.go`
- Modify: `pkg/plugin/limit_count/delayed_sync_test.go`
- Modify: `pkg/plugin/limit_count/plugin_test.go`
- Modify: `docs/review-remediation-ledger-2026-08-18.md`

**Interfaces:**
- Produces: safe cleanup after window expiry only when local delta is zero, no call is in flight, and no retry owns the key; maximum 10,000 live states; new keys fail closed at capacity.

- [ ] **Step 1: Add failing cleanup, dirty/in-flight/retry preservation, and small-capacity tests**
- [ ] **Step 2: Run the named tests and observe permanent state growth**
- [ ] **Step 3: Clean under `s.mu` after flush/retry promotion and before new-state allocation; never evict live or dirty quota state**
- [ ] **Step 4: Run the package normally and with race, scoped lint, update the ledger, and commit**

Commit:

```bash
git commit -m "fix(limit-count): bound delayed-sync state"
```

### Task 10: SEC-003 — migrate encrypted writes to versioned AEAD

**Files:**
- Modify: `pkg/data_encryption/data_encryption.go`
- Modify: `pkg/data_encryption/data_encryption_test.go`
- Modify: `pkg/data_encryption/resolver.go`
- Modify: `pkg/data_encryption/resolver_test.go`
- Modify: strict encrypted-field plugin call sites that currently invoke `Resolver.Resolve`
- Modify: `docs/design.md`
- Modify: `docs/configuration.md`
- Modify: `docs/review-remediation-ledger-2026-08-18.md`

**Interfaces:**
- Produces: `v2:` AES-GCM envelopes with random 12-byte nonces and authenticated canonical field context; `ResolveForContext` for strict fields.
- Preserves: unversioned AES-CBC is decrypt-only across key rotation; all new writes are v2 and tampering fails closed.

- [ ] **Step 1: Add failing tests for nondeterminism, round-trip, tampering, wrong context, rotated keys, legacy CBC reads, and valid-v2 preservation**
- [ ] **Step 2: Run the focused cryptographic tests and observe deterministic unauthenticated CBC**
- [ ] **Step 3: Seal with AES-GCM and `crypto/rand`; encode `v2:` plus base64(nonce || ciphertext); use canonical `plugin-name.field-path` AAD**
- [ ] **Step 4: Route unversioned values only through legacy CBC; update strict plugin call sites to contextual resolution**
- [ ] **Step 5: Run data-encryption/store/config and every strict-field plugin package, build, update docs/ledger, and commit**

Commit:

```bash
git commit -m "fix(data-encryption): migrate writes to authenticated AEAD"
```

### Task 11: Final acceptance and PR delivery

**Files:**
- Modify: `docs/review-remediation-ledger-2026-08-18.md`
- Modify: `docs/superpowers/plans/2026-08-18-review-remediation.md`

**Interfaces:**
- Produces: all valid findings fixed, BUG-001 rejected with evidence, no pending IDs, one independently reviewed PR.

- [ ] **Step 1: Verify one documentation baseline plus one commit per implemented finding with `git log --oneline master..HEAD`**
- [ ] **Step 2: Run `git diff --check master...HEAD`, the final affected-package aggregate, and `make build && make clean`**
- [ ] **Step 3: Request one independent read-only review of `master...HEAD`; resolve every Critical or Important item**
- [ ] **Step 4: Push `codex/remediate-audit-findings` and create one PR against `master`**
- [ ] **Step 5: Do not merge unless separately requested**
