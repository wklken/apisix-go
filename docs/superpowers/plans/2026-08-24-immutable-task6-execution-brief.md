# Immutable Task 6 Execution Brief

> **Owner:** Immutable compiler Task 6. Execute this brief inside one integration worktree based on `2477f048`; keep intermediate commits off `master` until the complete Task 6 build and review gates pass.

**Goal:** Replace package-global secret and plugin-metadata lookups with manifest-owned declarations, publication-scoped attempt capabilities, immutable metadata views, generation-local plugin instances, and atomic `WorkerCompilerFactory` ownership.

**Why this brief exists:** The source at `2477f048` exposes four ordering gaps that the parent plan did not make executable: the catalog must land before the encryption-service cutover; HTTP and stream can publish different bytes for the same resource key; plugin metadata has no immutable dependency channel; and a factory cannot construct generation dependencies until pure publication preparation has selected the exact set. This brief fixes those gaps without moving Task 7 HTTP compilation, Task 8 stream compilation, or Task 9 activation into Task 6.

## Frozen Contract Amendments

1. `secret.Scope` includes mandatory `Domain generation.Domain`. Attempt grants retain closure indexes by `(Domain, ResourceKey)`. Candidate and recovery materialization reject an empty, unknown, or cross-domain scope before resolver access.
2. `runtime.MetadataView` is a concrete immutable value containing defensive canonical metadata bytes keyed by factory. `NewMetadataView(map[string][]byte)` clones and validates its input. `Decode(factory string, target any) (bool, error)` returns `(false, nil)` when metadata is absent and never exposes backing bytes. `base.Dependencies` contains this view; production plugin packages do not read Store metadata.
3. Plugin instances are generation-local because they retain an attempt capability and a generation task registry. Their registry key includes the attempt ID plus the APISIX instance scope/provenance/config digest. Generation-neutral clients and clusters remain separately digest-keyed resources and may be shared across generations.
4. Task 6 does not add `compileHTTPFunc` or `compileStreamFunc`; `route.CompileInput` belongs to Task 7 and detached stream input belongs to Task 8. Task 6 `PreparedGeneration` may contain no HTTP/stream snapshot while it proves registration, task and lease ownership. Tasks 7 and 8 extend it with their real snapshot types.
5. Extract package-private pure `preparePublication(ctx, manifest, ticket, desired, previous)`. Existing `Compiler.PreparePublication` delegates to it. Candidate factory flow calls it once, performs pure catalog/schema admission and decision refinement, registers the final exact set, then materializes. Recovery validates committed publication structure and goes directly to materialization; it never runs desired-state disposition.
6. Move the pure `PublicationCandidate` structural validator to `pkg/generation` before recovery uses it. Store staging and compiler recovery consume the same validator; neither package keeps a private duplicate.

## Dependency Order

### C6.1 — Manifest-owned declaration catalog

**Exclusive files:** `pkg/capability/types.go`, `pkg/capability/load.go`, `pkg/capability/manifest.yaml`, focused capability tests, and a migration-only parity test under `pkg/data_encryption`.

**Implementation:**

- Add `SecretDeclarationSource`, `SecretDeclaration`, `PluginCapability.SecretDeclarations`, and `SecretDeclarationCatalog` exactly as specified by the parent plan.
- Parse rejects unknown sources, blank or non-canonical paths, declarations whose factory is not owned by the same capability, duplicate factory/source/field tuples, and conflicting strict policies.
- Catalog enumeration and lookup are defensive and deterministic. Digest uses a versioned canonical encoding sorted by factory/source/field/strict, never map iteration or string concatenation.
- Add every declaration from the four legacy data-encryption tables to the owning manifest capability.
- Add a parity test proving exact equality for config/metadata and strict/optional sets. Do not delete or change the legacy tables or `NewService` in this commit.

**Gate:**

```bash
source .envrc
GOFLAGS=-mod=readonly go test -race ./pkg/capability ./pkg/data_encryption -run '(Declaration|Manifest|Strict|Optional|Parity)' -count=1
GOFLAGS=-mod=readonly go run ./cmd/capability-gen -repo-root . -check
```

### C6.2 — Encryption service/catalog cutover

**Depends on:** C6.1.

**Exclusive owner:** `pkg/data_encryption/**` plus every `NewService` fixture/caller required for the atomic signature migration.

**Implementation:** Make catalog non-optional in `NewService`, include its digest in `SameConfiguration`, add declaration validation/resolution methods, make low-level config/metadata helpers catalog-driven, migrate all constructors, and only then delete the four legacy tables and global query helpers. Keep every intermediate commit on the Task 6 branch until this atomic lane builds.

**Gate:** focused declaration/encryption/metadata/rotation tests plus a repository build smoke check.

### C6.2b — Shared publication structural validator

**Depends on:** C6.2 only for branch sequencing; its files are independent.

**Exclusive owner:** `pkg/generation/**` plus the structural validation call site in `pkg/store/journal_publish.go` and focused Store tests.

Move the pure `PublicationSet`/`PublicationCandidate` structural validator from Store into `pkg/generation`. Preserve the existing error taxonomy and validation semantics exactly. Store staging delegates to the shared validator, and no package-private duplicate remains. This lands before C6.3 so candidate and recovery attempt registration can reject malformed publication closures before retaining bytes or opening a resolver.

**Gate:** focused generation validator tests, existing Store publication-validation tests, and a build smoke check.

### C6.3 — Attempt registration and temporary Store broker

**Depends on:** C6.2b and the domain amendment.

Split after interfaces are frozen:

- `pkg/secret/**` and `pkg/runtime/dependencies*`: attempt IDs, duplicate-live registry, registration-bound materializers, generation capability, domain/catalog admission, close quarantine, and immutable metadata view.
- `pkg/store/store.go`, `consumer_secret.go`, `resolved_secret.go`, focused broker tests: candidate/recovery authorization, exact per-domain retained bytes, cancellation-aware resolution, cache identity including secret-config digest, revoke/close race, and no package-global fallback.

Attempt IDs use versioned, domain-separated, length-delimited canonical binary encodings of the complete candidate or recovery identity. Public hash helpers return the zero ID on an impossible encoding failure; registration uses checked private helpers and rejects the sentinel. Candidate registration rejects an empty required-domain set; recovery registration rejects an empty verified published map. Authorization indexes only published/last-good resources by `(Domain, ResourceKey)`, never a cross-domain union.

Materialization validates the exact catalog tuple before examining the value. A raw reference goes directly to the attempt resolver; a non-reference first follows the declaration's strict/optional at-rest policy, and any reference produced by decryption then goes to the resolver. This preserves strict encrypted literals without rejecting an explicitly supported reference as malformed ciphertext.

The duplicate-live registry linearizes at reserve. Successful close releases the ID; backend revoke failure leaves it quarantined for the process/backend-owner lifetime. `Store.Stop` closes the broker before bbolt. A view is removed from live lookup before waiting for in-flight resolutions; retained sensitive bytes are cleared after the wait. Backend revoke failure never revalidates the local capability.

`MetadataView` accepts only one JSON object per factory, uses `pkg/json` with `UseNumber`, stores compact defensive bytes, exposes only decode, and returns redacted errors because its documents may already contain plaintext.

### C6.4 — Plugin dependency and metadata migration

**Depends on:** C6.3.

**Detailed execution plan:** [`2026-08-24-immutable-task6-c6.4-plugin-runtime.md`](2026-08-24-immutable-task6-c6.4-plugin-runtime.md).

C6.4 is no longer treated as one `pkg/plugin/**`-exclusive serial block. It first lands catalog compatibility and additive runtime/plugin contracts, then freezes the pure C6.5 final-attempt boundary. Plugin secret, metadata, and consumer leaf packages may branch from that merged checkpoint with exclusive package ownership. Shared runtime/plugin types, compiler files, route/server callers, and legacy deletion remain single-owner integration files.

The manifest adds a distinct `consumer_config` declaration source without expanding plugin-config at-rest encryption. Current `wolf-rbac` compatibility preserves both the Store schema's `wolf_url` and the plugin's `server` until a separate compatibility decision resolves the divergence.

### C6.5 — Pure refinement, factory and prepared ownership

**Depends on:** C6.3 and the C6.4 additive catalog/contract checkpoint. Its pure refinement/final-attempt skeleton lands before C6.4 leaf migrations; final prepared ownership lands after those migrations.

**Exclusive owner:** `pkg/compiler/**`.

- Extract pure publication preparation from the current compiler constructor dependency cycle and freeze the final-attempt hooks needed by C6.4 leaf work.
- Refine structural candidates with catalog/schema admission before computing the final attempt ID. Invalid resource/plugin admission follows the established per-resource last-good/fail-closed disposition; the final registered set is the exact refined set.
- Implement `PrepareGeneration`, `PrepareRecovery`, `PreparedGeneration`, deterministic cleanup, defensive accessors and concurrent idempotent discard/close.
- Define one compiler-private effective-binding materializer. C6.5 validates exact
  attempt/source authority and owns every lease it acquires, but it does not
  select HTTP/stream winners or expose a public plugin-binding view. Tasks 7
  and 8 compute the complete effective specs before invoking this primitive.
- Task 6 owns no HTTP cluster or stream router construction. Snapshot hooks are added only by Tasks 7 and 8 with their real input types.

### C6.6 — Integration and merge gate

**Depends on:** C6.2–C6.5, including all C6.4 leaf and composite migrations.

**Current status:** CP5 is accepted at `8cae3365`; C6.6 is accepted as an integration/compatibility gate after independent review. Production prepared-generation installation has not occurred, and the CP6 legacy deletion set is empty.

Keep `cmd/root.go`, standalone ingestion, and the current Builder/server/stream
production owner buildable without manufacturing an `ApplyTicket`, adding a
second selectable path, or exposing the compiler-private materializer. Build an
AST/`rg` caller ledger and delete only leaf compatibility APIs whose production
callers were actually removed by C6.4; retain Builder/Store snapshot and mutable
stream construction with explicit Task 7/8/9 deletion owners. Run the parent
Task 6 focused race commands, capability generator check, scoped lint, build,
dead-code/call-site scans, and an independent merge-level review. Merge the
complete Task 6 branch into local `master` only after all gates pass.

The live provider path remains `etcd.ConfigClient.sendBatch` -> acknowledged
Store event -> durable Store apply/hooks -> Server HTTP/stream reload. The
allowlisted Store journal can construct `generation.ApplyTicket`, but there is
no production edge from that ticket to `WorkerCompilerFactory.PrepareGeneration`
or `PrepareRecovery`. Task 7 owns immutable HTTP/TLS/effective-plugin snapshots,
Task 8 owns immutable stream/MQTT snapshots, and the joint Task 9 unit alone owns
provider/coordinator/journal installation, rollback, acknowledgement, retirement,
and final legacy deletion.

## Authoritative Task 7/8 Effective-Binding Input Amendment

This section supersedes the original immutable-runtime plan's provisional Task
7 phrase “consume pre-materialized bindings” and Task 8's descriptor-only
stream input. CP5 cannot determine effective runtime bindings from raw source
occurrences without duplicating the compilation owned by those tasks.

- Immutable Task 7 first computes route-over-plugin-config-over-service
  precedence, global/404/system and consumer compositions, route/service
  context, and plugin `_meta`. It pairs each effective winner with its exact
  admitted source occurrence, then calls CP5's compiler-private
  `materializeEffectiveBindings`. The returned internal bindings feed
  `route.CompileHTTP`; route code never constructs plugins or receives secret
  capability/lifecycle authority.
- Immutable Task 8 first computes stream service/route merge, protocol owner,
  effective config/scope/provenance, and stream context. It pairs the winner
  with its admitted source occurrence, calls the same private materializer,
  and feeds only the resulting internal binding into detached router
  compilation.
- The private materializer creates/injects X1's per-effective-binding child
  preparer before constructing an outer composite. It calls
  `StartObservingWithTasks` after `PostInit` for plugins that implement that M2
  lifecycle seam, before descriptor binding/lease transfer. Failures use the
  prepared-generation cleanup ledger.
- Neither Task exposes a public binding view, treats raw occurrences as runtime
  inventory, constructs a second activation path, or deletes the old production
  owner before the joint Task 9 cutover.

## Parallelism and Ownership

- C6.1 and C6.2 are sequential single-owner foundation work.
- After C6.3, catalog compatibility and additive plugin/runtime contracts land serially. The compiler's pure refinement/final-attempt skeleton then freezes the hooks consumed by leaf work.
- Plugin secret, metadata, and consumer leaf migrations may use separate worktrees only where package files are exclusive and all start from that merged checkpoint.
- Composite migration and final compiler ownership start only after their child leaf interfaces compile on the integration branch.
- One integration owner resolves live-caller fallout. Workers may implement and run focused tests locally but do not commit, push, merge, or modify another lane's files.

## Completion Evidence

- Exact catalog parity and deterministic digest.
- Candidate/recovery overlap, cross-attempt rejection, cross-domain same-key rejection, revoke/close race and redaction.
- Metadata absent/schema-invalid/valid-materialized ordering with no Store access.
- Given three Task 7/8-style effective specs, the private materializer's third-plugin failure stops tasks and releases the first two leases exactly once in reverse order.
- Recovery rejects revision/domain/candidate mismatches without desired-state access or disposition.
- Concurrent exact discard closes once; mismatched publication leaves the candidate open.
- No scoped plugin path imports or calls Store/`pkg/data_encryption` secret or
  metadata globals. Legacy `MaterializeSecrets` methods and file-level
  `pkg/data_encryption` imports required by the still-live Builder are retained
  only when the C6.6 caller ledger assigns their deletion to the joint Task 9
  cutover.
- Focused race gates, `go run ./cmd/capability-gen -repo-root . -check`, scoped lint, `make build`, `git diff --check`, and independent review pass.
