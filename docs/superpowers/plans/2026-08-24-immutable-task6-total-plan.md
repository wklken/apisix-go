# Immutable Task 6 Total Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** finish Immutable Compiler Task 6 as one dependency-ordered local integration: migrate plugin secrets, metadata, and consumers to exact registered-attempt contracts, complete base `PreparedGeneration` ownership and its private effective-spec materializer, audit the unchanged production boundary, pass the Task 6 gate, and only then merge to local `master` and begin Immutable Task 7.

**Architecture:** one integration owner advances `codex/apisix-go-immutable-task6` through serial shared checkpoints and accepts file-exclusive leaf diffs from isolated worktrees. Fixed commits identify accepted architecture and value contracts; future worktrees branch from a rolling integration baseline containing every prerequisite. Task 6 ends with detached dependency/lifetime authority and no new production activation path; Task 7/8 computes effective HTTP/stream bindings and snapshots, and the joint Task 9 performs the sole cutover.

**Tech Stack:** Go 1.26, capability manifest and generated registry, immutable generation publications, attempt-scoped secret materialization, `runtime.MetadataView`, `runtime.ConsumerBindings`, generation-owned task/resource leases, Git worktrees, focused unit/race tests, `golangci-lint`, and import-aware AST guards.

**Spec:** [`docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`](2026-08-23-immutable-compiler-plugin-runtime.md), Task 6, amended by [`docs/superpowers/plans/2026-08-24-immutable-task6-execution-brief.md`](2026-08-24-immutable-task6-execution-brief.md) and [`docs/superpowers/plans/2026-08-24-immutable-task6-c6.4-plugin-runtime.md`](2026-08-24-immutable-task6-c6.4-plugin-runtime.md).

## Global Constraints

- Run Go and Make commands from the active worktree after `source .envrc` with `GOFLAGS=-mod=readonly`.
- Use impact-scoped tests. Do not routinely run `go test ./...`, `go test ./pkg/...`, `make test`, or the whole `t/plugin` suite.
- `40c04a26` is the fixed CP3.4 architecture boundary, `e5b6a73e` the fixed descriptor dependency, and `9ebcd2b5` the fixed A1.1 consumer-preparer checkpoint. They are landmarks, not universal future branch points.
- The rolling implementation baseline is the current accepted integration HEAD after all declared prerequisites merge. Record that SHA in every worker handoff and acceptance record.
- Workers own only assigned files, return an owned-path diff plus command evidence, and do not commit, push, open a PR, merge, or modify another lane. The integration owner reads every diff, reruns the credible focused gate, and creates accepted commits.
- Shared manifest/catalog, compiler, runtime, plugin base/registry, Store, route/server/stream, and repository guard files have one serial integration owner. Parallel workers may not edit them.
- Keep the current Builder compatibility path buildable during leaf migration, but never let a new scoped path fall back to Store, `data_encryption`, environment lookup, a raw keyring, or a package-global resolver.
- Raw publication schema admission happens before attempt registration and secret access. Resolved validation happens after attempt-scoped materialization and before binding or instance publication.
- `plugin_metadata` belongs only to the HTTP publication domain in Task 6. The generic metadata preparer must not merge HTTP and stream resources with the same key.
- Errors/descriptors may expose stable factory/source/field class and digest only, never raw documents, references, environment names, Vault paths, ciphertext, credentials, lookup keys, or plaintext.
- Task 6 does not implement Task 7 HTTP snapshots, Task 8 stream snapshots, or Task 9 supervisor activation. C6.6 only audits compatibility and removes proven zero-caller leaves; it does not adapt or inject a prepared production path.
- Do not touch the four user-owned untracked `docs/reviews/` reports in the main checkout.
- Push, PR publication, and remote merge are outside current authority. Local `master` remains unchanged until complete Task 6 acceptance and independent review pass.

---

## Worktree and Integration Protocol

1. The integration owner records `git rev-parse HEAD`, `git status --short`, prerequisite checkpoint SHAs, and exclusive paths before dispatch.
2. A leaf worktree branches from that recorded integration HEAD only after every incoming dependency edge in the DAG is accepted. A later dependent worktree is recreated from the newer integration HEAD; it is not rebased onto a guessed historical landmark.
3. A worker writes/tests only its exclusive paths and returns the binary-safe diff for those exact assigned paths, `git status --short`, and exact command output. It does not create a commit.
4. The integration owner verifies the returned base SHA and owned-path boundary, reads the full diff, applies the accepted patch to the integration worktree, formats only touched Go files, reruns focused gates, and creates one checkpoint commit.
5. Shared-file changes are authored serially in the integration worktree. A leaf that discovers a required shared seam stops and reports the exact interface; the integration owner lands that seam first and recreates dependent worktrees.
6. After each accepted checkpoint, update the rolling-baseline ledger and the ready/blocked matrix. Never let a stale worktree silently absorb newer prerequisite behavior through conflict resolution.
7. No partial lane merges to local `master`. The entire clean Task 6 branch merges locally only after Task 10; no push or PR occurs.

### Task 1: Freeze and Accept the Remaining Child Plans

**Files:**
- Create/accept: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-s3-remaining-plugin-secrets.md`
- Create/accept: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-m2-special-metadata.md`
- Create/accept: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-x1-composites.md`
- Create/accept: `docs/superpowers/plans/2026-08-24-immutable-task6-cp5-prepared-generation.md`
- Create/accept: `docs/superpowers/plans/2026-08-24-immutable-task6-cp6-production-cutover.md`
- Review: all nine child plans linked in the Source-of-Truth Hierarchy

**Interfaces:**
- Consumes: accepted CP3.4, descriptor, A1.1, manifest, and live production-call inventories.
- Produces: non-overlapping file ownership, exact rolling-baseline rules, package TDD steps, and focused gates for every leaf.

- [ ] **Step 1: Validate every inventory against current source**

```bash
source .envrc
export GOFLAGS=-mod=readonly
rg -n --glob '*.go' --glob '!**/*_test.go' \
  'MaterializeSecrets|MaterializeScopedSecrets|DataEncryption\(|LoadPluginMetadata|GetPluginMetadataRaw|GetValidatedPluginMetadata|GetPluginMetadata\(|GetConsumer|ConsumerLookup' \
  pkg/plugin pkg/compiler pkg/runtime
git status --short
```

Expected: every production match has exactly one S1, S2, S3, M1, M2, A1, X1, CP5, or C6.6 owner; overlaps have explicit ordering rather than concurrent writers.

- [ ] **Step 2: Review the S3 and M2 blockers**

S3-0 must own: a scoped-support disposition for all 41 manifest factories with `plugin_config` declarations; compiler-owned raw-before-decode materialize-and-discard for `basic-auth`, `key-auth`, `jwt-auth`, `hmac-auth`, `ldap-auth`, and `jwe-decrypt`; and manifest parity for `dingtalk-auth.secret_fallbacks` and `saml-auth.secret_fallbacks` with ordered fallback behavior intact. A leaf no-op may not bypass this gate.

M2-C0 must own one concrete `MetadataPreparer` from final HTTP candidate through strict `plugin_metadata` materialization to `runtime.MetadataView`. Per-resource last-good/fail-closed decisions remain compiler-owned tests, not leaf caches.

- [ ] **Step 3: Run child-plan self-review**

Read every step in the nine child plans and reject generic or deferred instructions: each implementation step must name the exact package/file, API, expected behavior, RED/GREEN command, and integration owner. Then run `git diff --check -- docs/superpowers/plans/2026-08-24-immutable-task6-*.md`. Expected: every named type/function exists now or is produced by an earlier task, and the documentation diff is clean.

**Acceptance:** every remaining production file has one worker or serial owner, with exact dependency and gate.

### Task 2: Land S3-0 and M2-C0, the Serial Compiler Blockers

**Files:**
- Execute exact files/tests from: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-s3-remaining-plugin-secrets.md`, Task S3-0
- Execute after S3-0: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-m2-special-metadata.md`, Task M2-C0 only
- Serial ownership: `pkg/capability/**`, required parity tests, shared `pkg/compiler/**`, and shared scoped-support guards only

**Interfaces:**
- Consumes: `PreparationAttempt`, declaration catalog, `9ebcd2b5` consumer preparer, raw-before-decode admission.
- Produces: compiler-owned discard materialization for six phantom auth groups, complete 41-factory support classification, and the concrete HTTP-only `MetadataPreparer`/`runtime.MetadataView` required by M1 and M2 leaves.

- [ ] **Step 1: Add RED parity and support tests**

Tests fail for the six phantom groups, missing DingTalk/SAML fallback fields, and any declared factory with neither a real scoped materializer nor the exact compiler discard disposition.

- [ ] **Step 2: Run RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/capability ./pkg/compiler ./pkg/plugin \
  -run 'S3|ScopedSupport|CompatibilityDiscard|SecretFallback|DeclarationParity' -count=1
```

Expected: FAIL because S3-0 catalog/compiler support is absent.

- [ ] **Step 3: Implement only the shared adapter**

Materialize declared raw auth fields through the exact attempt occurrence before route-plugin decode, then discard without exposing plaintext. Keep A1 ownership of auth package files. Add both fallback containers to manifest parity. Do not add a leaf no-op.

- [ ] **Step 4: Run GREEN and integrate**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/capability ./pkg/compiler ./pkg/plugin \
  -run 'S3|ScopedSupport|CompatibilityDiscard|SecretFallback|DeclarationParity' -count=1
go test -race ./pkg/compiler ./pkg/plugin \
  -run 'ScopedSupport|CompatibilityDiscard|SecretFallback' -count=1
go run ./cmd/capability-gen -repo-root . -check
golangci-lint run ./pkg/capability/... ./pkg/compiler/... ./pkg/plugin/...
make build
git diff --check
```

Expected: all 41 factories are classified; six phantom groups resolve/discard only through attempt authority; fallback ordering passes.

**Acceptance:** integration owner creates one S3-0 checkpoint and records its SHA as the minimum A1/S3 leaf baseline.

- [ ] **Step 5: Land M2-C0 from the accepted S3-0 descendant**

Without allowing a second compiler writer, implement final HTTP candidate
enumeration, raw-schema ownership, exact `plugin_metadata` occurrences,
strict/optional attempt materialization, resolved validation, and
`runtime.NewMetadataView`. Prove HTTP-only behavior, candidate/recovery
identity, Azure/error-log declaration parity, and fail-before-secret schema
admission with the exact M2-C0 RED/GREEN/race/generator/lint/build gates.

**Acceptance:** the integration owner records a separate M2-C0 checkpoint.
Immediately after this point, non-overlapping M1 package workers and the four
ordinary M2 leaves may run alongside the remaining secret/auth leaves; overlap
integration still waits for its named S1/S2/S3 prerequisites.

### Task 3: Execute Secret Leaf Lanes S1, S2, and S3

**Files:**
- Execute: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-s1-store-materializers.md`
- Execute: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-s2-raw-resolvers.md`
- Execute: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-s3-remaining-plugin-secrets.md`

**Interfaces:**
- Consumes: descriptor, final-attempt occurrences, S3-0 for S3 leaves, package-local `base.ScopedSecretAccess`.
- Produces: every real plugin-config secret owned by an attempt-scoped implementation; named Builder legacy entrypoints remain explicitly ledgered through the joint Task 9 cutover.

- [ ] **Step 1: Dispatch file-exclusive leaves from dependency-complete rolling baselines**

S1 has eight packages. S2 has fifteen; `clickhouse_logger` remains S1-owned. S3 has twelve real leaves: three AI, four function, and five session-auth. S3 workers do not edit shared `ai_auth`, `ai_common`, or `function_upstream`; if a shared seam is proven necessary, the integration owner lands it serially first. Accept S3-FN1 `azure_functions` route-secret work before M1 adds Azure metadata.

- [ ] **Step 2: Require one RED/GREEN packet per leaf**

Each packet contains exact RED command/output, owned-path diff, passing focused package test, focused race evidence for retained mutable/task/client state, and a scan proving the scoped method does not call Store, `DataEncryption`, the legacy materializer, or a raw resolver.

- [ ] **Step 3: Review and integrate one leaf at a time**

Run the exact owned-package `git diff --check`, `go test`, and applicable `go test -race` commands printed in that leaf's S1/S2/S3 child-plan task. The integration owner inspects public-config redaction, failure atomicity, `Stop`, and exact manifest field spelling before committing. A command from a neighboring leaf is not substitute evidence.

- [ ] **Step 4: Run lane integration gates**

Use the exact package matrices in the S1/S2/S3 plans, then run:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin -run 'ScopedPreparation|ScopedSupport|SecretMaterializationGuard' -count=1
go run ./cmd/capability-gen -repo-root . -check
make build
git diff --check
```

Expected: all 41 declared factories satisfy scoped support and no new scoped implementation reaches a legacy backend.

**Acceptance:** S1, S2, and S3 each have an integration-owner checkpoint and zero-unowned-factory inventory. S2-E1 hands remaining `error_log_logger` metadata/observer/task ownership to M2.

### Task 4: Complete A1 Immutable Auth Consumers

**Files:**
- Execute: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-a1-consumers.md`, A1.2-A1.3
- Exclusive packages: `basic_auth`, `key_auth`, `jwt_auth`, `hmac_auth`, `ldap_auth`, `jwe_decrypt`, `wolf_rbac`

**Interfaces:**
- Consumes: `9ebcd2b5`, accepted S3-0, `base.ConsumerLookup`.
- Produces: seven auth packages whose non-nil immutable lookup is authoritative; only explicit nil-lookup Builder compatibility remains.

- [ ] **Step 1: Branch three non-overlapping groups after S3-0**

Group A owns basic/key, Group B JWT/HMAC, Group C LDAP/JWE/Wolf. S3 owns no concurrent auth-package edit.

- [ ] **Step 2: Require poison fallback and N/N+1 tests**

Each package proves a non-nil lookup miss never reaches Store, N remains stable while N+1 prepares, and closing N after retirement does not invalidate N+1. Preserve anonymous behavior, constant-time checks, authentication state, headers/status/body, and Wolf's `server`/`wolf_url` record.

- [ ] **Step 3: Run A1.3 acceptance**

Run the exact A1.3 commands, then verify remaining production Store calls are confined to named nil-lookup compatibility functions. Scoped lint, build, and diff check pass.

**Acceptance:** the integration owner commits A, B, and C in reviewed order; no auth package can type-assert the lookup back to closeable concrete bindings.

### Task 5: Complete M1 Ordinary Immutable Metadata

**Files:**
- Execute: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-m1-metadata.md`
- Own: the 23 frozen ordinary packages and final ordinary-access guard

**Interfaces:**
- Consumes: accepted Task 2 M2-C0 `MetadataPreparer`/`runtime.MetadataView`, then accepted S1/S2/S3 changes for M1 overlaps.
- Produces: one metadata decode per generation-owned ordinary plugin instance; no ordinary request/log/body path reads Store metadata.

- [ ] **Step 1: Land non-overlapping groups from the newest accepted baseline**

M1-A2 may write package-local changes while M2-C0 is being integrated. M1-A1 packages other than `azure_functions` may do the same. `azure_functions` is an ordered overlap: first accept S3-FN1 scoped-secret work from the S3-0 descendant and M2-C0, then branch M1 Azure from the integration descendant containing both and add metadata without overwriting secret dependencies.

- [ ] **Step 2: Land S1/S2 overlap groups from merged dependency baselines**

M1-B starts after S1 and preserves scoped dependencies in `clickhouse_logger`, `limit_count`, and `oas_validator`. M1-C/D start after S2 and preserve scoped dependencies across logger packages. Never replay an old metadata diff mechanically over newer secret code.

- [ ] **Step 3: Remove only the ordinary metadata fallback**

After all 23 packages migrate, delete `pkg/plugin/base/metadata.go` and its obsolete test. Add the import-aware guard with a temporary allowlist containing only `authz_casbin`, `batch_requests`, `chaitin_waf`, `error_log_logger`, and `otel` until M2 removes them.

- [ ] **Step 4: Run the M1 acceptance gate**

Run all 23 package tests, the 23-package N/N+1 race corpus, schema/compiler tests, generator check, scoped lint, build, source scans, and diff check from the child plan. Confirm `graphql_limit_count` reads `limit-count` and no generic OTel alias fallback exists.

**Acceptance:** M1 has no unresolved review finding; the five M2 directories remain M2-only.

### Task 6: Complete the M2 Special Metadata Owners

**Files:**
- Execute: `docs/superpowers/plans/2026-08-24-immutable-task6-lane-m2-special-metadata.md`
- Exclusive leaves: `chaitin_waf`, `authz_casbin`, `batch_requests`, `otel`, `error_log_logger`

**Interfaces:**
- Consumes: accepted M2-C0/final `runtime.MetadataView`, S2-E1 for error-log, generation task registry.
- Produces: five generation-owned special metadata lifecycles.

- [ ] **Step 1: Dispatch four independent special leaves**

After M2-C0, `chaitin_waf`, `authz_casbin`, `batch_requests`, and `otel` may run in parallel. Preserve generation-local Chaitin config, one Casbin enforcer per generation, compiler-owned batch last-good behavior, and `plugin_metadata > plugin_attr > defaults` for both OTel keys.

- [ ] **Step 2: Dispatch error-log from both prerequisites**

Start `error_log_logger` only from a baseline containing M2-C0 and S2-E1. M2 owns metadata materialization, metadata-versus-route selection, observer/task startup, `TaskRegistry`/`Stop` retirement, overlap, and invalid-desired/last-good tests; preserve S2 private plugin-config secret installation.

- [ ] **Step 3: Integrate and remove the final metadata allowlist**

After all five leaves pass focused/race tests, update the import-aware guard so no production plugin may call legacy Store metadata APIs. Run the exact M2 compiler/five-package gate, generator, scoped lint, build, and diff check.

**Acceptance:** one final `runtime.MetadataView` is authoritative for ordinary and special owners; no plugin polls or reconstructs mutable global metadata.

### Task 7: Integrate CP4 and X1 Composite Ownership

**Child plan:**
`docs/superpowers/plans/2026-08-24-immutable-task6-lane-x1-composites.md`

**Files:**
- Modify: `pkg/plugin/workflow/**`
- Modify: `pkg/plugin/multi_auth/**`
- Shared tests/guards: serial integration owner only

**Interfaces:**
- Consumes: S1 `limit_count`, A1 immutable consumers, applicable S3 child preparation, `ScopedSecretAccess.Child`, attempt-bound instance key.
- Produces: composite children retaining outer resource provenance and exact attempt ownership.

- [x] **Step 1: Write RED ownership and partial-failure tests**

Prove sibling factories cannot collide, child scopes preserve outer resource/attempt, and failure in child three stops children two and one exactly once in reverse order.

- [x] **Step 2: Migrate workflow and multi-auth**

Move child construction/materialization out of `PostInit`, pass the same immutable dependencies and consumer lookup, and remove direct legacy `base.MaterializePluginSecrets` only after both composites use scoped child access. X1 produces the shared child primitive; Task 7/8 later supplies the effective outer scope, provenance, and HTTP/stream context before invoking it.

- [x] **Step 3: Run focused gates**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/workflow ./pkg/plugin/multi_auth -count=1
go test -race ./pkg/plugin/workflow ./pkg/plugin/multi_auth \
  -run 'Scoped|Attempt|Child|Cleanup|Consumer' -count=1
golangci-lint run ./pkg/plugin/workflow/... ./pkg/plugin/multi_auth/...
make build
git diff --check
```

**Acceptance:** X1 checkpoint
`b31d2a6d59c3e4f39b375b4def5706d0867a36d2` retains the named legacy-only
Store seam for pre-Task-9 Builder compatibility; scoped paths have no Store or
global fallback. Child keys include factory, outer provenance, config digest,
and attempt identity.

### Task 8: Complete CP5 `PreparedGeneration` Ownership

**Child plan:**
`docs/superpowers/plans/2026-08-24-immutable-task6-cp5-prepared-generation.md`

**Files:**
- Modify/create according to current source: `pkg/compiler/**`
- Modify focused ownership tests under `pkg/runtime/**` only if the interface requires it

**Interfaces:**
- Consumes: all accepted secret, consumer, metadata, composite, task, and resource preparation hooks.
- Produces: atomic `WorkerCompilerFactory.PrepareGeneration`, `PrepareRecovery`, and closeable `PreparedGeneration`.

- [ ] **Step 1: Add RED transaction and cleanup tests**

Assert this base order:

```text
final publication set
-> attempt registration/capability
-> task registry
-> consumer bindings
-> final metadata view
-> PreparedGeneration
```

Cover registration/metadata/consumer failure, recovery mismatch, factory-close race, concurrent exact discard, and mismatched-discard-does-not-close. Separately pass three complete fake effective specs to the compiler-private materializer and prove third-plugin reverse cleanup without selecting HTTP or stream winners.

- [ ] **Step 2: Implement one reverse-order cleanup stack**

Append base ownership immediately after acquisition and transfer only after base preparation. Task 7/8 later appends each effective plugin/resource lease immediately through the same private materialization gate. `Close`/`DiscardPrepared` are concurrent and idempotent, expose defensive publication/metadata/consumer accessors only, and never expose Store, broker, resolver, raw keyring, closeable consumer bindings, plugin bindings, instances, leases, or the materializer.

- [ ] **Step 3: Run CP5 gates**

Run the exact mapped compiler/runtime/plugin ordinary and race gates, lint,
canonical generator, build, complete-range diff/API/stale scans, and independent
review in the CP5 child plan's Task 8. The child gate is authoritative because
it first proves every package regex has real matches; do not reuse the earlier
cross-package `FactoryClose` regex that selected zero runtime tests.

Expected: recovery never reads desired state or computes disposition; raw source occurrences are never claimed as effective binding inventory; partial base or supplied-effective-spec preparation closes every acquired owner exactly once in reverse order.

**Acceptance:** integration owner creates CP5 only after independent ownership/cleanup review.

### Task 9: Execute CP6/C6.6 Integration and Compatibility Audit

**Current status at accepted CP5 `8cae3365`: accepted.** CP6 completed the compatibility audit and independent review without changing the legacy production owner, installing a prepared generation, or deleting a legacy API.

**Child plan:**
`docs/superpowers/plans/2026-08-24-immutable-task6-cp6-production-cutover.md`

**Files:**
- Inspect: `pkg/route/**`, `pkg/server/**`, `pkg/stream/**`, `cmd/**`
- Delete only proven-dead compatibility accessors under `pkg/store/**`, `pkg/plugin/base/**`, and shared wrappers

**Interfaces:**
- Consumes: accepted CP5 base bundle contract, all completed leaf migrations,
  and the two deferred Task 7 HTTP / Task 8 stream effective-materializer call
  paths.
- Produces: no-forged-ticket guard, exact live-caller/deletion ledger, buildable legacy production owner, and removal only of zero-production-caller leaves.

- [ ] **Step 1: Prove the activation gap and forbid a second path**

Add an AST guard proving pre-Task-9 production has no
provider-to-ticket-to-`PrepareGeneration`/`PrepareRecovery` path. Allow only
the existing journal-owned `pkg/store/journal_apply.go` ticket construction;
reject compiler prepare calls and any new ticket construction outside that
exact allowlist. Do not add a prepared Builder, prepared MQTT reload,
cross-domain retirement latch, feature flag, or test-only production proxy.

- [ ] **Step 2: Build the live caller and residual-risk ledger**

Classify Builder/`ConfigSnapshot`, global consumer lookup, TLS, mutable stream/MQTT construction, registration retirement, and Store resolver ownership. Assign HTTP/TLS removal to Task 7 plus Task 9, stream removal to Task 8 plus Task 9, and provider/activation/rollback/acknowledgement/final deletion to the joint Task 9 cutover.
Record both deferred effective-materializer call paths without adding a
prepared production adapter in C6.6. Raw occurrences remain source authority,
not a temporary binding inventory.

- [ ] **Step 3: Delete only zero-caller paths**

```bash
rg -n --glob '*.go' --glob '!**/*_test.go' \
  'DataEncryption\(|MaterializePluginSecrets|MaterializeSecrets\(|ResolveSecretReference|GetPluginMetadataRaw|GetValidatedPluginMetadata|GetPluginMetadata\(|GetConsumer|GetConsumerGroup' \
  cmd pkg
```

Classify every remaining match. Delete only APIs whose production callers were removed by the accepted Task 6 leaves. If the pre-Task-9 owner still needs decoded Store plaintext or legacy materialization, retain it with its exact Task 7/8/9 deletion owner; do not claim Task 6 eliminated it.

- [ ] **Step 4: Run compatibility-boundary gates**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./cmd ./pkg/store ./pkg/compiler ./pkg/route ./pkg/server ./pkg/stream \
  -run 'Prepared|Generation|Secret|Metadata|Consumer|MQTT|ProductionDoesNotForge' -count=1
go test -race ./pkg/store ./pkg/compiler ./pkg/route ./pkg/server ./pkg/stream \
  -run 'Prepared|Generation|Close|Secret|Metadata|Consumer|ProductionDoesNotForge' -count=1
golangci-lint run ./cmd/... ./pkg/store/... ./pkg/compiler/... ./pkg/route/... ./pkg/server/... ./pkg/stream/...
make build
git diff --check
```

**Acceptance:** Task 6 adds no production activation path; every deleted API has zero production callers, and every retained pre-Task-9 seam has an exact caller, risk, and Task 7/8/9 deletion owner.

### Task 10: Run Task 6 Acceptance and Independent Review

**Files:**
- Verify all paths changed since the Task 6 base
- Update source-of-truth docs only when implementation status or a Task 9 deferral changed

**Interfaces:**
- Consumes: accepted CP1-CP6 integration history.
- Produces: clean, review-approved Task 6 branch ready for local master.

- [ ] **Step 1: Freeze evidence identity**

Record final integration HEAD, base SHA, `git status --short`, changed-file list, and checkpoint SHAs. No unreviewed worker diff may be counted complete.

- [ ] **Step 2: Run final contract guards**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go run ./cmd/capability-gen -repo-root . -check
go test ./pkg/plugin \
  -run 'ScopedPreparation|ScopedSupport|SecretMaterializationGuard|MetadataDependencyGuard' -count=1
rg -n --glob '*.go' --glob '!**/*_test.go' \
  'ResolveSecretReference|LoadPluginMetadata|GetPluginMetadataRaw|GetValidatedPluginMetadata' pkg/plugin
git diff --check
```

Expected: generator and guards pass; source scans contain no prohibited production plugin call. Allowed compatibility symbols are exact and C6.6/Task-9-owned.

- [ ] **Step 3: Run final impact-scoped tests/races**

Build batches from final changed packages and child acceptance lists. At minimum include `pkg/capability`, `pkg/generation`, `pkg/secret`, `pkg/data_encryption`, `pkg/store`, `pkg/runtime`, `pkg/compiler`, shared `pkg/plugin`, every changed leaf, and changed route/server/stream/cmd packages. Run real-process `t/plugin` only for an exact affected behavior lacking a narrower seam, one command at a time.

- [ ] **Step 4: Run final scoped lint and build**

Run the exact Task 6 package scopes; the final changed-file ledger may narrow this list only by proving an entire package subtree is untouched:

```bash
source .envrc
export GOFLAGS=-mod=readonly
golangci-lint run ./cmd/... ./pkg/capability/... ./pkg/generation/... ./pkg/secret/... \
  ./pkg/data_encryption/... ./pkg/store/... ./pkg/runtime/... ./pkg/compiler/... \
  ./pkg/plugin/... ./pkg/route/... ./pkg/server/... ./pkg/stream/...
make build
git diff --check
```

- [ ] **Step 5: Request one independent merge-level review**

Review declaration completeness, raw-before-decode order, final-attempt identity, redaction, N/N+1 isolation, composites, reverse cleanup, recovery purity, the no-forged-production-path guard, dead fallbacks, and Task 7/8/9 boundaries. Resolve every high/medium finding and rerun invalidated gates.

**Acceptance:** clean branch, fresh evidence, separated baseline failures, no unresolved high/medium finding, and every residual seam explicitly assigned.

### Task 11: Merge Locally and Hand Off to Immutable Task 7

**Files:**
- Git history only; no product edit
- Next plan: `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`, Task 7

**Interfaces:**
- Consumes: accepted clean Task 6 integration HEAD.
- Produces: local `master` containing Task 6 and the baseline for immutable HTTP snapshot compilation.

- [ ] **Step 1: Recheck identities**

Confirm integration is clean, local `master` has not advanced unexpectedly, and intended Task 6 commits are reachable from the reviewed branch. Do not reset a dirty checkout.

- [ ] **Step 2: Merge the complete branch to local master**

Perform one local merge after Task 10. No push, PR, remote merge, or release is implied. Verify new local master contains final Task 6 HEAD and unrelated user files are unchanged.

- [ ] **Step 3: Reuse or invalidate evidence correctly**

If merge is content-identical, record unchanged diff/runtime/commands. If conflict resolution changes any file, rerun every gate affected by that file.

- [ ] **Step 4: Branch Immutable Task 7 from new local master**

Task 7 here means **Immutable Compiler Task 7, “Compile an Immutable HTTP Snapshot”**, not Program Boundary 7 runtime safety. Its worktrees consume Task 6 `PreparedGeneration` and the new master SHA; they do not branch from `40c04a26`, `e5b6a73e`, `9ebcd2b5`, or stale master.

**Acceptance:** Task 6 is on local master with preserved evidence; Task 7 begins from that exact SHA and no remote publication occurred.

---

## Checkpoint Acceptance Matrix

| Checkpoint | Must be true | Minimum gate |
| --- | --- | --- |
| S3-0 | six phantom groups use compiler discard; DingTalk/SAML fallbacks manifest-owned; 41 factories classified | capability/compiler/plugin tests, race, generator, lint, build |
| S1 | eight Store-materializer packages use scoped access and private redacted values | eight package tests, relevant race, guard, build |
| S2 | fifteen raw-resolver packages scoped; clickhouse S1-owned; error-log handoff explicit | package tests, async races, zero scoped `DataEncryption`, build |
| S3 | twelve real leaves pass; Azure order preserved | child package gates, 41-factory gate, generator, build |
| A1 | seven auth packages treat non-nil lookup as authoritative | poison fallback, N/N+1 race, lint, build |
| M1 | 23 ordinary packages decode once; only M2 allowlist remains | 23 tests/overlap races, schema/generator/guard/lint/build |
| M2 | concrete metadata preparer plus five special owners; no allowlist remains | compiler/five-package tests, races, guard/generator/lint/build |
| X1 | children preserve outer scope/attempt and reverse cleanup; effective outer context deferred | workflow/multi-auth tests/race, lint/build |
| CP5 | base transaction plus private effective-spec materializer; no public binding view | compiler/runtime/plugin focused race, AST guard, lint/build |
| C6.6 | no forged ticket/second path; live callers classified; only zero-caller leaves removed | cmd/store/compiler/route/server/stream tests/race, AST/rg/lint/build |
| Task 6 final | clean branch, fresh evidence, no unresolved high/medium review | generator, impact tests/races, scoped lint, build, diff, review |
| local master | complete accepted Task 6 only | identity/status; rerun after content-changing conflict resolution |

## Explicit Deferrals and Non-Goals

- Immutable HTTP route/upstream/TLS snapshot construction is Task 7.
- Detached stream router snapshot construction is Task 8.
- Durable-journal/immutable activation and supervisor/worker cutover is Task 9.
- Do not choose between `wolf-rbac.server` and `wolf_url` in Task 6.
- Do not add a general plugin framework, external runner compatibility, Lua/OpenResty runtime, or unrelated behavior cleanup.
- Do not claim full Store plaintext elimination if pre-Task-9 production retains a proven seam; record it and delete it in Task 9.
- Do not push or create a PR without later project-owner authority.

## Source-of-Truth Hierarchy

This total plan owns ordering, readiness, and integration gates; package details remain in child plans.

| Priority | Source | Authority |
| --- | --- | --- |
| 1 | [`2026-08-23-apisix-go-convergence-program-spec.md`](2026-08-23-apisix-go-convergence-program-spec.md) | product, security, compatibility, state, and delivery contract |
| 2 | [`2026-08-24-journal-immutable-cutover-reorder.md`](2026-08-24-journal-immutable-cutover-reorder.md) | durable-journal/immutable-runtime cutover ordering |
| 3 | [`2026-08-24-immutable-task6-execution-brief.md`](2026-08-24-immutable-task6-execution-brief.md) | corrected C6.1-C6.6 contract plus authoritative Task 7/8 effective-spec input amendment |
| 4 | [`2026-08-23-immutable-compiler-plugin-runtime.md`](2026-08-23-immutable-compiler-plugin-runtime.md) | stable interfaces except where the higher-priority execution amendments supersede provisional Task 6/7/8 inputs |
| 5 | [`2026-08-24-immutable-task6-c6.4-plugin-runtime.md`](2026-08-24-immutable-task6-c6.4-plugin-runtime.md) | CP1-CP6 topology and global invariants |
| 6 | [`CP3`](2026-08-24-immutable-task6-cp3-compiler-skeleton.md) and [`CP2.1`](2026-08-24-immutable-task6-cp2.1-secret-descriptor.md) | final-attempt boundary and descriptor correction |
| 7 | [`S1`](2026-08-24-immutable-task6-lane-s1-store-materializers.md), [`S2`](2026-08-24-immutable-task6-lane-s2-raw-resolvers.md), [`S3`](2026-08-24-immutable-task6-lane-s3-remaining-plugin-secrets.md), [`M1`](2026-08-24-immutable-task6-lane-m1-metadata.md), [`M2`](2026-08-24-immutable-task6-lane-m2-special-metadata.md), [`A1`](2026-08-24-immutable-task6-lane-a1-consumers.md), [`X1`](2026-08-24-immutable-task6-lane-x1-composites.md), [`CP5`](2026-08-24-immutable-task6-cp5-prepared-generation.md), [`CP6`](2026-08-24-immutable-task6-cp6-production-cutover.md) | inventories, file ownership, TDD steps, integration and lifecycle gates |
| 8 | This document | dependency DAG, checkpoint status, integration, final acceptance, Task 7 handoff |

The S3 and M2 filenames are frozen here. Those plans become executable only after the integration owner verifies their concrete inventories; no competing plan should be created.

## Accepted Checkpoint Ledger

These commits are present in the current Task 6 integration history. Final gates are rerun against final HEAD because later changes can invalidate old command evidence.

| Boundary | Accepted implementation commits | Result |
| --- | --- | --- |
| C6.1 | `820176b4 feat(capability): own secret declaration catalog` | manifest-owned declaration catalog |
| C6.2 | `9c7fcb98 feat(data-encryption): consume manifest secret catalog` | encryption service consumes catalog identity |
| C6.2b | `d09c1c33 refactor(generation): share publication validation` | shared generation structural validation |
| C6.3 | `e2b34324 feat(secret): bind canonical attempt identities`; `4e9ab703 feat(runtime): add immutable metadata view`; `729ed7fc feat(secret): bind materialization to attempts`; `720df206 feat(store): isolate secret attempt views` | attempt identity, metadata, materializer, temporary broker |
| CP1 | `c30bd147 feat(capability): declare scoped plugin and consumer secrets` | plugin/consumer declaration compatibility |
| CP2 | `5b3b582c feat(plugin): add attempt-scoped preparation contracts`; `176b218d feat(runtime): add immutable consumer bindings` | scoped plugin seam and immutable consumer lookup |
| CP3.1-CP3.3 | `dc958046 refactor(consumer): centralize resolved registry`; `93916292 refactor(plugin): expose schema-only factory witnesses`; `d9b74dc3 refactor(consumer): expose credential schema witnesses`; `c77acdc4 refactor(capability): centralize declared field traversal`; `9b3748bb feat(compiler): admit raw plugin schemas`; `18d53718 refactor(compiler): refine validated predecessors`; `476d8885 refactor(compiler): validate final publication sets` | resolved registries, schema witnesses, raw admission, predecessor refinement, final-set validation |
| CP3.4 | `40c04a26 feat(compiler): register refined generation attempts` | exact final candidate/recovery registration and bounded hooks |
| CP2.1 | `e5b6a73e feat(secret): add redacted value descriptors` | source-class plus digest descriptor |
| A1.1 | `9ebcd2b5 feat(compiler): prepare immutable consumer bindings` | final HTTP candidate becomes attempt-local consumer/group bindings |

Supporting plan-only commits are sequencing evidence, not implementation completion: `7b30c19d`, `5f3b7407`, `c7bd484a`, `ac5c3ca2`, `56690bf1`, `63775cf1`, `62b2bfc1`, `4573f15a`, and `dcd04fff`.

## Dependency DAG

```text
C6.1 -> C6.2 -> C6.2b -> C6.3 -> CP1 -> CP2 -> CP3.1..CP3.4
                                      \             \
                                       -> CP2.1      -> A1.1
                                            \             \
                                             +-------------+--> S3-0

S1 (8 leaves) -----------------------------------------------------+
S2 (15 leaves) ----------------------------------------------------+
S3-0 -> S3 (12 real leaves) --------------------------------------+--> 41-factory gate
S3-0 -> A1 (3 groups, 7 packages) --------------------------------+
S3-FN1 + M2-C0 -> M1-Azure ---------------------------------------+
M2-C0 -> remaining M1-A1/A2 integration --------------------------+
S1 -> M1-B --------------------------------------------------------+
S2 -> M1-C/D ------------------------------------------------------+
M2-C0 -> chaitin | casbin | batch | otel -------------------------+
M2-C0 + S2-E1 -> error-log ---------------------------------------+
                                                                    v
                         CP4 reviewed leaf integration
                              |                 |
                              v                 v
                     X1 workflow/multi-auth   M2 final integration
                              \                 /
                               +------ CP5 -----+
                                      |
                           CP6/C6.6 compatibility audit
                                      |
                             Task 6 review and gates
                                      |
                              merge to local master
                                      |
                         Immutable Task 7 HTTP snapshot
```

Conflict edges:

- S3-0 and M2-C0 both touch shared compiler ownership, so execute serially even though both logical prerequisites exist.
- Accept S3-FN1 `azure_functions` first. M1 Azure then branches from an integration descendant containing both S3-FN1 and M2-C0.
- M1-B waits for S1; M1-C/D wait for S2; M2 `error_log_logger` waits for M2-C0 and S2-E1.
- X1 `workflow` waits for S1 `limit_count`; X1 `multi_auth` waits for A1 and applicable S3 child support.

## Ready/Blocked Matrix at accepted CP5 `8cae3365`

Refresh this snapshot whenever integration HEAD changes.

| Work unit | State | Branch rule / blocker |
| --- | --- | --- |
| S3-0 | **accepted** | ancestor `a5c866e5` |
| S1 eight leaves | **accepted** | last lane-owned leaf `8e499178` |
| S2 fifteen leaves | **accepted** | acceptance guard `723e092c` |
| M1 | **accepted** | final `ec301a7a` |
| M2 | **accepted** | final `077e19d8` |
| A1 Groups A/B/C | **accepted** | final `397583e9` |
| S3 real leaves | **accepted** | last lane-owned leaf `1a3d4ae8` |
| X1 | **accepted** | product checkpoint `b31d2a6d` |
| CP5 | **accepted** | final acceptance `8cae3365` |
| CP6/C6.6 | **accepted** | integration/caller/boundary audit accepted from CP5 `8cae3365` |
| local `master` merge | **ready after CP6 commit** | fast-forward only after the accepted staged diff is committed and rechecked |
| Immutable Task 7 worktrees | **blocked** | branch only from post-Task-6 local master |

---

## Self-Review Record

- Spec coverage: C6.1-C6.3 accepted foundations, CP1-CP6, S1/S2/S3/M1/M2/A1/X1, audited Task 7/8 binding deferral, final review, local-master merge, and Immutable Task 7 handoff each have an owner and gate.
- Placeholder scan: execution steps name concrete child tasks, packages, interfaces, commands, and expected results; no unresolved implementation placeholder remains.
- Type/term consistency: fixed landmarks are `40c04a26`, `e5b6a73e`, and `9ebcd2b5`; future work uses a recorded rolling integration HEAD. Task 7 is explicitly disambiguated.
- Ownership consistency: workers return diffs/evidence only; commits, shared edits, conflict resolution, review remediation, and local-master merge remain with the integration owner. CP5 never publishes raw bindings, and C6.6 never creates a second production path.
