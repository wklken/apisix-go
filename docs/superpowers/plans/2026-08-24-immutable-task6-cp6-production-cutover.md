# Immutable Task 6 CP6 Integration and Merge Gate Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Use `superpowers:test-driven-development` for behavior changes and deletion guards, `superpowers:requesting-code-review` before acceptance, and `superpowers:verification-before-completion` before the local commit.

**Goal:** Close Immutable Task 6 at an honest C6.6 integration checkpoint: reconcile every accepted Task 6 lane with CP5, prove CP5's defensive publication/metadata/consumer views and lifetime boundary compose, keep the current Store/Builder/Router production owner buildable, remove only legacy leaves whose last production callers are already gone, and hand exact remaining work to Tasks 7, 8, and 9.

**Architecture:** CP6 is an integration and merge gate, not a production prepared-generation cutover. CP5 owns one prepared-generation lifetime and exposes defensive publication, metadata, and consumer observations; its effective-binding materializer stays compiler-private. Task 7 or 8 must calculate a complete effective plugin specification before that primitive may create an instance. Production remains `cmd/root.go -> server.Server -> Store/route.Builder/stream.Router`, because no production provider currently owns a legal `generation.ApplyTicket`. Task 7 compiles immutable HTTP/TLS/effective-plugin ownership, Task 8 compiles immutable stream/MQTT ownership, and the joint Task 9 unit exclusively owns provider/coordinator/journal wiring, activation, acknowledgement, rollback, drain, and permanent legacy deletion.

**Tech Stack:** Go 1.26, `pkg/compiler`, `pkg/generation`, `pkg/runtime`, `pkg/plugin`, bbolt-backed `pkg/store`, existing `pkg/route`/`pkg/server`/`pkg/stream` owners, focused unit/race tests, import-aware AST guards, `rg` ledgers, golangci-lint, and build smoke verification.

**Spec:** `2026-08-23-immutable-compiler-plugin-runtime.md` Task 6, amended by `2026-08-24-immutable-task6-execution-brief.md` C6.6, `2026-08-24-journal-immutable-cutover-reorder.md`, and `2026-08-24-immutable-task6-total-plan.md`. The filename retains the roadmap's old `production-cutover` label for stable links; the checkpoint itself is not a production cutover.

## Global Constraints

- Start from the dynamic accepted CP5 HEAD containing reviewed S1, S2, S3, A1, M1, M2, X1, and CP5 checkpoints. Do not use `40c04a26`, `e5b6a73e`, `9ebcd2b5`, or another stale fixed baseline.
- Record the actual accepted HEAD and component SHAs before editing. Visible or unstaged work is not an accepted checkpoint. The PreparedGeneration observation surface is the subject of the no-authority rule; existing authority-bound producer hooks such as `PreparationAttempt` are separate inputs and must not be misreported as PreparedGeneration observations.
- Run Go commands after `source .envrc && export GOFLAGS=-mod=readonly`.
- Never fabricate `generation.ApplyTicket`. Candidate preparation needs the real coordinator ticket; recovery needs verified committed publications. Both production callers belong to Task 9.
- Do not create a second production path, runtime flag, prepared route constructor, prepared stream reload API, or test-only proxy described as production integration.
- CP6 consumes only accepted CP5 defensive publication, metadata, and consumer views plus close/discard behavior. It gets no registration, tasks, resources, leases, secret capability, mutable config, Store handle, raw plugins, or lifecycle callback.
- CP5's effective-binding materializer remains compiler-private. `(domain, resource, factory)` is not a sufficient effective key: route, plugin-config, service, global, not-found, consumer, metadata, and stream precedence must first be compiled by Task 7 or 8.
- `store.ConfigSnapshot` is a live legacy input, not permission to re-decrypt or construct a second CP5 runtime graph. Retain it only for the current Builder.
- Delete a legacy API only when AST/type-aware analysis and direct scans prove all production callers are gone and deletion does not require Task 7, 8, or 9.
- Do not implement Task 7 HTTP/TLS snapshots, Task 8 stream snapshots, or Task 9 supervisor/provider/journal activation.
- Workers do not commit, push, open a PR, merge, or edit another lane. Only the integration owner creates the reviewed local commit.
- Never put decrypted secrets, keyring material, credentials, resolver handles, or decoded snapshots into logs or ledgers.

## Verified Planning-Snapshot Call Chain

Re-run this table at accepted CP5 HEAD; line numbers may move.

| Current owner | Verified call | Why CP6 retains it | Final owner |
| --- | --- | --- | --- |
| `cmd/root.go:startWithOptions` | constructs `data_encryption.Service`, passes it to `server.NewServer`, starts server | server still owns Store ingestion and reload | Task 9 removes permanent Store/raw-keyring resolver seam |
| `pkg/etcd/watcher.go`, `pkg/store/store.go`, `pkg/server/server.go` | `ConfigClient.sendBatch` emits acknowledged Store events; durable Store apply/hooks trigger registered HTTP/stream reload callbacks | desired-batch helpers and `generation.NewCoordinator` have no production caller | Task 9 replaces provider-to-reload delivery with provider/coordinator/journal activation and acknowledgement |
| `pkg/server/server.go`, `reload.go` | construct `route.Builder` with Store, clusters, config, encryption resolver | no legal prepared activation exists | Task 7 compiles; Task 9 installs/deletes |
| `pkg/route/builder.go` | reads one `GetConfigSnapshot` for routes/global rules/plugin metadata/services/upstreams/plugin-configs/SSL; constructs/materializes plugins; authenticated requests still read consumer groups through package-global Store access | complete HTTP effective specs do not exist and consumers are not snapshot-owned | Task 7 replaces construction and binds consumers; Task 9 removes callers |
| `pkg/server/tls.go` | SNI callbacks read package-global Store certificate/config selectors | immutable TLS selector does not exist | Task 7 compiles; Task 9 activates/deletes |
| `pkg/server/server.go` | prepares stream routes, reads upstream/service, reloads stream, commits last-good | immutable stream snapshot is absent | Task 8 compiles; Task 9 activates/deletes |
| `pkg/stream/runtime.go`, `router.go` | mutable reload creates MQTT state and uses legacy materialization | stream instance/connection lifetime is not generation-owned | Task 8 constructs; Task 9 installs/retires |
| `pkg/server/route_handler.go` | retires/swaps the HTTP handler, closes hijacked connections, waits asynchronously for old requests to drain, then calls the old stop callback | HTTP snapshot and prepared lifetime are not one transaction | Task 7 defines lifetime; Task 9 swaps/drains |

## Boundary Classification

### A. CP6 may complete

- Reconcile accepted lane/CP5 types, cleanup order, defensive copying, closed-view behavior, and redaction.
- Verify CP5's public observation surface and keep lifecycle authority internal.
- Preserve one Store/Builder/Router production path and fix only reproduced integration defects.
- Delete exact zero-production-caller leaves and their proxy-only debris.
- Produce residual/deletion ledgers, focused/race/lint/build evidence, independent review, and one local commit.

### B. CP6 must retain

- Builder plugin construction and `ConfigSnapshot` plaintext path until Task 7 compiles HTTP state and Task 9 activates it.
- Store metadata/consumer/SSL/service/upstream/plugin-config/global getters still used in production.
- MQTT legacy materialization and mutable reload until Task 8 compiles stream state and Task 9 activates it.
- Current route/stream replacement and retirement because no coordinated activation exists.
- Existing encryption service/resolver needed by the old path, without extending it into new compiler/runtime code.

### C. Task 9 is the only permanent deletion gate

Task 9 may delete those paths only when the same reviewed unit proves:

1. etcd/standalone providers call `Coordinator.Apply` rather than legacy events;
2. the coordinator supplies legal candidate tickets and recovery uses verified committed publications;
3. Task 7 HTTP/TLS and Task 8 stream/MQTT snapshots are generation-owned;
4. N remains active through N+1 journal commit/finalize and every failure restores N;
5. HTTP, TLS, consumers, metadata, secrets, plugins, resources, and stream advance atomically;
6. cursor/ack advances only after commit;
7. permanent scoped resolution no longer needs Store keyring/global fallback; and
8. AST and direct absence guards prove old builders, reloads, plaintext materializers/buckets, and facades have no callers.

## Checkpoint Meaning

The checkpoint is **C6.6 Task 6 integration gate**. It does not mean a generation was installed, the existing journal ticket reaches compiler preparation, route/stream lifetime is generation-bound, N/N+1 rollback is wired, Store plaintext is eliminated, or provider acknowledgement uses the immutable runtime.

**Execution status:** accepted after independent merge-level review. The deletion set is empty; the unchanged Store/Builder/Router owner remains the sole production path, and no prepared generation was installed.

## File Structure at Execution

- Create `pkg/compiler/c6_production_boundary_test.go` as the single import-aware pre-Task-9 authority/second-path guard.
- Create `pkg/compiler/c6_integration_test.go` only when a reproduced cross-lane contract failure has no existing focused test home.
- Create `pkg/compiler/c6_legacy_leaf_guard_test.go` only when the final caller ledger identifies at least one real zero-production-caller leaf.
- Modify `pkg/compiler/**`, `pkg/runtime/**`, or one exact plugin leaf only for a reproduced integration defect; do not use CP6 to restructure a passing subsystem.
- Modify a legacy declaration owner only when Task 4 proves its last production caller is gone; otherwise leave production code untouched and record the deferral.
- Modify `2026-08-24-immutable-task6-total-plan.md`, `2026-08-24-immutable-task6-c6.4-plugin-runtime.md`, and `2026-08-24-immutable-task6-execution-brief.md` so their C6.6 status matches this audited boundary.
- Do not modify `cmd/root.go`, route/server/stream production code merely to demonstrate prepared injection. Those files change in CP6 only if an accepted-lane integration defect breaks the existing owner.

---

### Task 1: Freeze the Accepted Baseline and CP5 Surface

**Files:** Inspect every lane plan, `2026-08-24-immutable-task6-cp5-prepared-generation.md`, `pkg/compiler/**`, `pkg/runtime/**`, and `pkg/plugin/base/**`. Create ignored `.cache/telemetry/c6.6-baseline-ledger.txt`.

**Interfaces:** Consumes accepted lane/CP5 commits and their compiled public surface. Produces `C6_CP5_HEAD`, the exact checkpoint/API ledger, and no source mutation.

- [ ] **Step 1: Prove worktree ancestry and accepted checkpoints**

```bash
git status --short
git branch --show-current
git rev-parse HEAD
git log --oneline --decorate -20
export C6_CP5_HEAD
C6_CP5_HEAD="$(git rev-parse HEAD)"
test -n "$C6_CP5_HEAD"
for checkpoint_sha in \
  a5c866e5 fcad5b38 077e19d8 1a3d4ae8 8e499178 397583e9 \
  723e092c ec301a7a b31d2a6d 042decf6 51960ab6 7bfc3941 \
  b67e3340 c669ce19 c98649f4 dbd87d0a 2918b735 faabe39c; do
  git merge-base --is-ancestor "$checkpoint_sha" "$C6_CP5_HEAD"
done
```

Stop if a lane/CP5 checkpoint is missing, unreviewed, or only an unstaged foreign diff. `git log -20` is orientation only and does not replace the explicit ancestor checks. Record S1/S3 as their last lane-owned leaf because their tracked plans do not name separate aggregate acceptance commits.

- [ ] **Step 2: Record CP5's exact accepted API from source**

```bash
rg -n 'type PreparedGeneration|func \([^)]*PreparedGeneration|type WorkerCompilerFactory|func \([^)]*WorkerCompilerFactory' pkg/compiler --glob '*.go'
rg -n 'PublicationSet|MetadataView|ConsumerLookup|Registration|TaskRegistry|ResourceRegistry|GenerationCapability|Bindings\(' pkg/compiler --glob '*.go'
go doc ./pkg/compiler.PreparedGeneration
go doc ./pkg/compiler.WorkerCompilerFactory
go doc ./pkg/compiler.PreparationAttempt
go doc ./pkg/compiler.FactoryOccurrence
```

Do not copy an in-flight plan's guessed method names. Record exact compiled accessors. Accepted `PreparedGeneration` observation is defensive publication, metadata, and consumer data plus close/discard. Existing `PreparationAttempt`, `FactoryOccurrence`, `MetadataPreparer`, and `ConsumerPreparer` are authority-bound producer hooks, not PreparedGeneration observations. If PreparedGeneration itself exposes a public registration/task/resource/secret/plugin-enumeration handle, stop CP6 and return to CP5 remediation.

- [ ] **Step 3: Prove the effective-binding primitive is private**

```bash
rg -n 'effective|materializ|binding|FactoryInstance' pkg/compiler --glob '*.go' --glob '!*_test.go'
go doc ./pkg/compiler
```

It must await a complete Task 7/8 effective spec; CP6 neither exports nor invokes it.

- [ ] **Step 4: Write ignored evidence**

Record HEAD, checkpoint SHAs, exact accessors, private materializer location, and baseline failures without secret values.

**Checkpoint:** input is proven; no code changed.

---

### Task 2: Reconcile Lane/CP5 Contracts with RED/GREEN Evidence

**Files:** Modify `pkg/compiler/**`, `pkg/runtime/**`, or the exact owning `pkg/plugin` leaf package only after reproducing a conflict. Create `pkg/compiler/c6_integration_test.go` only if no existing focused test file fits.

**Interfaces:** Consumes the exact accessors recorded by Task 1 and existing lane contracts. Produces only corrected defensive-view/lifecycle behavior proven by focused tests; it produces no route/stream adapter or new production interface.

- [ ] **Step 1: Run the contract baseline**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler ./pkg/runtime ./pkg/plugin/base \
  -run 'Publication|Prepared|Preparation|Metadata|Consumer|Cleanup|Close|Discard|Materializ|Binding' -count=1
```

Record the exact failure before editing. A green baseline does not authorize redesign.

- [ ] **Step 2: Add one failing test per real defect**

Tests may prove defensive publications, immutable metadata decode, read-only consumer lookup, concurrent idempotent close/discard, cleanup order, redaction, and absence of exported lifecycle/secret/Store/plugin-enumeration handles. Confirm RED on the defect; do not fail because an invented adapter is absent.

- [ ] **Step 3: Apply the smallest owner-side fix**

Preserve CP5 construction/cleanup order and lane APIs. Add no route/stream adapter, ticket, Store fallback, or public effective-binding view.

- [ ] **Step 4: Run focused GREEN**

Run the Step 1 command plus only exact changed leaf packages.

**Checkpoint:** Task 6 contracts compose while remaining detached from production activation.

---

### Task 3: Guard Against a Provider-to-Compiler Path and a Second Production Path

**Files:** Create `pkg/compiler/c6_production_boundary_test.go`; inspect non-test Go under `cmd`, `pkg/config`, `pkg/etcd`, `pkg/store`, `pkg/server`, `pkg/route`, and `pkg/stream`.

**Interfaces:** Consumes Go AST/type information for the production package set. Produces `TestC6ProductionBoundaryGuardRejectsForbiddenFixture` and `TestC6ProductionBoundary`; both report redacted file/symbol/reason findings and expose no production API.

- [ ] **Step 1: TDD an import-aware AST guard**

First use temporary fixture packages to prove the helper rejects new production
construction/fabrication of `generation.ApplyTicket` outside the exact existing
`pkg/store/journal_apply.go` journal owner, every pre-Task-9 call to compiler
`PrepareGeneration`/`PrepareRecovery`, a second selectable activation path, and
any pre-Task-9 direct compiler/runtime import in the inspected legacy
production roots. Resolve aliases/types; text matching is insufficient.

Before a concrete activation flag, constructor, interface, or registration site
exists, prove the "second path" boundary structurally: reject all compiler
or runtime imports in those roots and ticket construction outside the exact
journal file/function/form allowlist. This stronger pre-Task-9 rule also blocks
constructor-inferred or interface-dispatched prepare calls without pretending
to perform whole-program data-flow analysis. Do not add a keyword heuristic;
Task 7/8 must update the direct-import boundary only when they introduce a named
detached owner, and Task 9 must update it for the real activation contract.

Before Task 9, only `pkg/store` may name or transport the `ApplyTicket` type;
other inspected roots fail on any direct or aliased type use, including an
aggregate-carrier expression. Store parameters, fields, and pointer-only type
uses remain valid transport, while direct Store construction remains locked to
the journal file/function/form allowlist.

- [ ] **Step 2: Confirm fixture RED, then helper GREEN**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestC6ProductionBoundaryGuardRejectsForbiddenFixture$' -count=1
```

- [ ] **Step 3: Apply the proven helper to the real tree**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestC6ProductionBoundary' -count=1
```

Tests/compiler internals may use tickets. Production has one accepted journal
construction point in `pkg/store/journal_apply.go`, but no path from provider
translation through that ticket to compiler preparation. The guard uses an
exact package/file/symbol allowlist for that existing constructor and rejects
all compiler prepare calls. Investigate any other match instead of broadly
whitelisting a package.

- [ ] **Step 4: Corroborate directly**

```bash
rg -n 'ApplyTicket|PrepareGeneration|PrepareRecovery' cmd pkg/config pkg/etcd pkg/store pkg/server pkg/route pkg/stream --glob '*.go' --glob '!*_test.go'
rg -n 'feature.*prepared|prepared.*feature|legacy.*prepared|prepared.*legacy' cmd pkg --glob '*.go' --glob '!*_test.go'
```

Record every match and owner. Empty `rg` is corroboration, not a substitute for AST evidence.

**Checkpoint:** one legacy production owner remains; no candidate/recovery was installed.

---

### Task 4: Regenerate the Caller Ledger and Remove Only Zero-Caller Leaves

**Files:** Inspect/conditionally modify `pkg/data_encryption/**`, `pkg/plugin/base/secrets.go`, `pkg/plugin/base/metadata.go`, `pkg/plugin/types.go`, `pkg/store/getter.go`; inspect all callers under `cmd` and `pkg`. Create ignored `.cache/telemetry/c6.6-legacy-callers.tsv`; create `pkg/compiler/c6_legacy_leaf_guard_test.go` only if a leaf qualifies.

**Interfaces:** Consumes Task 3's AST/type scanner and direct declaration/caller scans. Produces the exact TSV deletion ledger and, only for qualified symbols, `TestC6DeletedLegacyLeavesAreAbsent` plus the minimal declaration deletion.

- [ ] **Step 1: Inventory direct declarations and callers**

```bash
rg -n '\bDataEncryption\(|\bMaterializePluginSecrets\(|\bMaterializeScopedPluginSecrets\(|\bMaterializeSecrets\(' cmd pkg --glob '*.go'
rg -n '\bResolveSecretReference\(|\bMaterializeSecret\(|\bLoadPluginMetadata\[' cmd pkg --glob '*.go'
rg -n '\b(GetPluginMetadataRaw|GetValidatedPluginMetadata|GetPluginMetadata|GetConsumer|GetConsumerGroup|GetConsumerByPluginKey)\(' cmd pkg --glob '*.go'
rg -n '\b(GetConfigSnapshot|PrepareStreamRoutes|CommitStreamRouteLastGood)\(' cmd pkg --glob '*.go'
```

For every declaration record symbol, declaration, non-test callers, test-only callers, lane, disposition (`delete-now`, `retain-task7`, `retain-task8`, `retain-task9`), and evidence. Test-only use is suspicious, not production compatibility.

- [ ] **Step 2: Resolve aliases/wrappers with AST/type information**

Extend Task 3's helper or add the legacy guard. Attribute selector aliases, method expressions, and proxy wrappers to the real declaration. The planning snapshot already blocks premature deletion of Builder secret/ConfigSnapshot/metadata/consumer calls, TLS Store selectors, MQTT materialization/reload, server retirement, and encryption service.

- [ ] **Step 3: Add RED absence assertions only for actual `delete-now` leaves**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestC6DeletedLegacyLeavesAreAbsent$' -count=1
```

Confirm the declaration/wrapper makes the test RED. If nothing qualifies, create no ceremonial always-green test; record zero deletions.

- [ ] **Step 4: Delete the exact leaf and debris**

Remove its declaration, newly unused import/helper, and proxy-only fixture/test. Do not delete Store buckets/migrations or reformat unrelated files.

- [ ] **Step 5: Run guard GREEN and all importing package tests**

Re-run the exact guard, declaration package, and every importer. Record an empty deletion set explicitly if applicable.

**Checkpoint:** every removal has zero-caller proof; every retained API has a live caller and Task 7/8/9 owner.

---

### Task 5: Freeze the Plaintext and Lifecycle Handoff

**Files:** Modify `docs/superpowers/plans/2026-08-24-immutable-task6-total-plan.md`, `docs/superpowers/plans/2026-08-24-immutable-task6-c6.4-plugin-runtime.md`, and `docs/superpowers/plans/2026-08-24-immutable-task6-execution-brief.md`; create ignored `.cache/telemetry/c6.6-residual-boundary.md`.

**Interfaces:** Consumes the final Task 4 caller/deletion ledger. Produces one consistent editable C6.6 status and one residual boundary assigning every live seam to Task 7, 8, or joint Task 9.

- [ ] **Step 1: Fill the current ownership matrix from Task 4 evidence**

| Residual | Task 7 | Task 8 | Joint Task 9 |
| --- | --- | --- | --- |
| Builder/effective HTTP plugin construction | compile route/plugin-config/service/global/not-found/consumer/metadata precedence | none | install snapshot, delete Builder/global reads |
| `ConfigSnapshot` decoded/plaintext routes/global rules/plugin metadata/services/upstreams/plugin-configs/SSL | remove HTTP dependence | none | delete builder/plaintext buffers/buckets after last caller |
| Package-global Store consumer-group and SNI/SSL reads | bind consumer/TLS into HTTP generation | none | atomically activate and delete old reads |
| MQTT creation and legacy materialization | none | compile exact stream specs, MQTT owner, immutable router | install/rollback, delete mutable reload/materialization |
| HTTP/stream retirement | define request lifetime | define connection lifetime | retain N through N+1 finalize, then drain once |
| registration installation | contribute HTTP owner | contribute stream owner | provider ticket, journal stage/activate/commit/finalize/ack |
| Store encryption/keyring resolver | consume scoped values | consume scoped values | permanent generation resolver/grant, delete transitional seam |
| Store events/acknowledged hooks | none | none | provider -> coordinator -> journal and cursor acknowledgement |

List only evidence types/paths, never plaintext. Distinguish encrypted publications, attempt-scoped `secret.Value`, defensive metadata/consumer values, and legacy Store-decoded configuration.

- [ ] **Step 2: Correct all editable Task 6 C6.6 claims**

In the total plan, C6.4 Task 11, and execution brief C6.6, state that CP6 is an integration gate; the allowlisted Store journal ticket has no path to compiler preparation; Tasks 7/8 own effective snapshots; Task 9 owns install/rollback/ack/retirement; and exact Store/Builder/Router paths remain. Remove the stale instruction to inject a partial prepared bundle into the current Builder. Mark none of Tasks 7-9 complete.

- [ ] **Step 3: Check source-of-truth consistency**

```bash
rg -n 'CP6|C6\.6|production|cutover|PreparedGeneration|Task 7|Task 8|Task 9|ConfigSnapshot|plaintext' \
  docs/superpowers/plans/2026-08-24-immutable-task6-total-plan.md \
  docs/superpowers/plans/2026-08-24-immutable-task6-c6.4-plugin-runtime.md \
  docs/superpowers/plans/2026-08-24-immutable-task6-execution-brief.md \
  docs/superpowers/plans/2026-08-24-journal-immutable-cutover-reorder.md
```

Preserve the joint-cutover plan's authority; do not average conflicts.

**Checkpoint:** the roadmap makes no installed-generation claim and names every future deletion owner.

---

### Task 6: Run Focused, Race, Lint, Build, and Refactor Gates

**Files:** Verify every accepted-CP5-to-CP6 changed file. Create ignored `.cache/telemetry/c6.6-verification.txt`.

**Interfaces:** Consumes the frozen final diff and Tasks 3-5 guards/ledgers. Produces generator, focused, race, lint, build, diff, and refactor-audit evidence tied to that exact diff.

- [ ] **Step 1: Freeze the final diff**

```bash
git status --short
test -n "$C6_CP5_HEAD"
git diff --stat "${C6_CP5_HEAD}...HEAD"
git diff --name-only "${C6_CP5_HEAD}...HEAD"
git diff --check "${C6_CP5_HEAD}...HEAD"
```

`C6_CP5_HEAD` is the accepted baseline SHA exported and recorded in Task 1. Include uncommitted integration changes separately.

- [ ] **Step 2: Run generator and focused correctness gates**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go run ./cmd/capability-gen -repo-root . -check
go test ./pkg/capability ./pkg/generation ./pkg/secret ./pkg/data_encryption ./pkg/store \
  ./pkg/runtime ./pkg/compiler ./pkg/plugin ./pkg/route ./pkg/server ./pkg/stream ./cmd \
  -run 'Catalog|Publication|Attempt|Materializ|Metadata|Consumer|Prepared|Cleanup|Close|Discard|C6|Builder|Reload|RouteHandler|Stream|MQTT' -count=1
```

Add exact tests when the pattern misses changed behavior. Never call a narrowed or zero-test run complete.

- [ ] **Step 3: Run concurrency-sensitive tests**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test -race ./pkg/secret ./pkg/store ./pkg/runtime ./pkg/compiler ./pkg/plugin ./pkg/route ./pkg/server ./pkg/stream \
  -run 'Attempt|Materializ|Metadata|Consumer|Prepared|Cleanup|Close|Discard|C6|Replace|Reload|Stream|MQTT' -count=1
```

- [ ] **Step 4: Run scoped lint, build, and diff checks**

```bash
source .envrc
export GOFLAGS=-mod=readonly
golangci-lint run ./pkg/capability/... ./pkg/generation/... ./pkg/secret/... ./pkg/data_encryption/... \
  ./pkg/store/... ./pkg/runtime/... ./pkg/compiler/... ./pkg/plugin/... ./pkg/route/... ./pkg/server/... ./pkg/stream/... ./cmd/...
make build
git diff --check
```

Record exact pre-existing failures; do not broaden the patch or report them passing.

- [ ] **Step 5: Perform the mandatory dead-code/refactor audit**

List changed functions/methods/types/imports/modules. For every moved, renamed, extracted, or deleted symbol, run `rg -n` with that literal identifier across `.` and `--glob '*.go'`, plus the AST guard. Remove proxy-only wrappers/tests unless the residual ledger proves compatibility. Check new Store/global/resolver imports, extra diff, lost comments, and unfixed tests.

- [ ] **Step 6: Re-run Task 4 scans**

Diff results against the final caller ledger. A new or reappearing caller blocks acceptance.

**Checkpoint:** generator, focused, race, lint, build, diff, boundary, and dead-code evidence describe the same final diff.

---

### Task 7: Independent Review and Local Integration Commit

**Files:** Review the complete accepted-CP5-to-CP6 diff; modify only confirmed finding owners; stage exact reviewed paths.

**Interfaces:** Consumes the accepted baseline SHA, final diff, residual/deletion ledgers, and verification transcript. Produces one independent review disposition and one local integration-owner checkpoint commit; it produces no push, PR, master merge, or deployment.

- [ ] **Step 1: Request independent read-only review**

Give the reviewer baseline SHA, diff, ledgers, and verification transcript. Require checks for ticket forgery, second runtime, CP5 lifecycle leakage, premature effective-binding consumption, legacy-owner buildability, zero-caller deletion proof, exact residual plaintext ownership, no route/stream retirement claim, and evidence/diff identity.

- [ ] **Step 2: Resolve findings with RED/GREEN evidence**

Reproduce valid findings, minimally fix, rerun focused and affected final gates. Reject findings only with source/call-chain evidence.

- [ ] **Step 3: Inspect exact final scope**

```bash
git status --short
git diff --check
git diff --stat
git diff --name-only
```

- [ ] **Step 4: Stage exact paths and commit locally**

Stage each path from the reviewed final inventory explicitly with `git add -- path/to/file`; issue one or more commands listing literal paths and inspect the staged result before committing. Then run:

```bash
git diff --cached --check
git diff --cached --stat
git diff --cached
git commit -m "chore(runtime): close task6 integration gate"
```

Never use `git add -A`. No push, PR, master merge, or deployment is authorized.

- [ ] **Step 5: Report truthfully**

Report the actual local commit SHA, independent-review result, count and path of zero-caller deletions, residual-ledger path, and exact verification results. State verbatim that production prepared-generation installation was not performed because no legal provider-to-journal-ticket-to-compiler-prepare path exists before Task 9; the existing journal-owned `pkg/store/journal_apply.go` ticket construction remains allowlisted and detached. HTTP/TLS belongs to Task 7 plus Task 9 activation, and stream/MQTT belongs to Task 8 plus Task 9 activation.

**Checkpoint:** one local integration commit exists; no installed generation or external delivery is claimed.

## Acceptance Checklist

- [ ] Actual accepted CP5 descendant contains every reviewed Task 6 lane.
- [ ] CP5 exposes defensive publication/metadata/consumer observations only; lifecycle authority remains internal.
- [ ] Effective materialization remains private and receives no incomplete identity surrogate.
- [ ] Production neither fabricates a ticket nor calls candidate/recovery preparation.
- [ ] No second selectable/test-only production path exists.
- [ ] Current Store/Builder/Router path builds and changes only for reproduced defects.
- [ ] `ConfigSnapshot` never rebuilds or re-decrypts a CP5 runtime graph.
- [ ] Every deletion has AST/type-aware and direct zero-caller evidence.
- [ ] Every retained materializer/getter/encryption/lifecycle seam has a Task 7/8/9 owner.
- [ ] MQTT construction is Task 8; activation/retirement is Task 9.
- [ ] HTTP/TLS construction is Task 7; activation/deletion is Task 9.
- [ ] Registration install, N/N+1 rollback, ack, finalization, and permanent Store deletion are Task 9 only.
- [ ] Generator, focused/race tests, scoped lint, build, diff, and refactor audit match the final diff.
- [ ] Independent review findings are resolved or evidence-backed rejected.
- [ ] Total plan calls C6.6 an integration gate, not installed production generations.
- [ ] Only the integration owner committed locally; no push/PR/master merge/deploy occurred.

## Self-Review Before Handoff

1. Can the allowlisted Store journal ticket reach compiler preparation? Before Task 9, all production preparation calls must remain absent.
2. Can CP6 build plugins from `ConfigSnapshot`, metadata, or consumer data a second time? If yes, reject it.
3. Does CP5 expose lifecycle authority, mutable bytes, plugin enumeration, secret capability, Store, or resolver handles? If yes, return it to CP5.
4. Did CP6 infer effective binding from an insufficient tuple instead of a Task 7/8 merged spec? If yes, delete it.
5. What AST/type evidence proves every removed symbol has zero production callers?
6. Which exact live caller retains each plaintext/materialization seam, and which task removes it?
7. Are rollback, registration installation, drain, or MQTT retirement described as complete? If yes, correct the claim.
8. Do tests exercise accepted contracts, not a proxy production never uses?
9. Does final verification match reviewed/staged diff and disclose failures/skips?
10. Does every changed line trace to integration, a reproduced defect, a guard, zero-caller deletion, or source-of-truth correction?

Any unsupported answer blocks C6.6 acceptance.
