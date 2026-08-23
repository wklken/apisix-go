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
GOFLAGS=-mod=readonly go run ./cmd/capability-gen -check
```

### C6.2 — Encryption service/catalog cutover

**Depends on:** C6.1.

**Exclusive owner:** `pkg/data_encryption/**` plus every `NewService` fixture/caller required for the atomic signature migration.

**Implementation:** Make catalog non-optional in `NewService`, include its digest in `SameConfiguration`, add declaration validation/resolution methods, make low-level config/metadata helpers catalog-driven, migrate all constructors, and only then delete the four legacy tables and global query helpers. Keep every intermediate commit on the Task 6 branch until this atomic lane builds.

**Gate:** focused declaration/encryption/metadata/rotation tests plus a repository build smoke check.

### C6.3 — Attempt registration and temporary Store broker

**Depends on:** C6.2 and the domain amendment.

Split after interfaces are frozen:

- `pkg/secret/**` and `pkg/runtime/dependencies*`: attempt IDs, duplicate-live registry, registration-bound materializers, generation capability, domain/catalog admission, close quarantine, and immutable metadata view.
- `pkg/store/store.go`, `consumer_secret.go`, `resolved_secret.go`, focused broker tests: candidate/recovery authorization, exact per-domain retained bytes, cancellation-aware resolution, cache identity including secret-config digest, revoke/close race, and no package-global fallback.

`Store.Stop` closes the broker before bbolt. A view is removed from live lookup before waiting for in-flight resolutions; retained sensitive bytes are cleared after the wait. Backend revoke failure never revalidates the local capability.

### C6.4 — Plugin dependency and metadata migration

**Depends on:** C6.3.

**Exclusive owner:** `pkg/plugin/**`.

- Reduce the universal plugin interface to the parent-plan contract and inject explicit config, `GenerationCapability`, `MetadataView`, and generation tasks.
- Build one generation-owned schema witness per factory to obtain `GetMetadataSchema`; validate present immutable metadata before any secret access, materialize declared metadata fields, then build the final immutable view used by route/service/consumer instances.
- Preserve admission order: init, metadata schema, config schema, decode, pre-materialization validation, scoped secret materialization, post-init, bind.
- Replace every `base.LoadPluginMetadata -> store.GetPluginMetadata`, direct Store metadata lookup, raw encryption resolver, and `store.MaterializeSecret` path. Extend the AST guard to prohibit production plugin imports of `pkg/data_encryption` and Store secret/metadata access.
- Plugin-instance keys are attempt-scoped. Reusable clients remain separate resources and never retain generation capabilities or task registries.

### C6.5 — Pure refinement, factory and prepared ownership

**Depends on:** C6.3 and C6.4.

**Exclusive owner:** `pkg/compiler/**` plus the shared pure candidate validator in `pkg/generation` and its Store caller migration.

- Extract pure publication preparation from the current compiler constructor dependency cycle.
- Refine structural candidates with catalog/schema admission before computing the final attempt ID. Invalid resource/plugin admission follows the established per-resource last-good/fail-closed disposition; the final registered set is the exact refined set.
- Implement `PrepareGeneration`, `PrepareRecovery`, `PreparedGeneration`, deterministic cleanup, defensive accessors and concurrent idempotent discard/close.
- Task 6 owns no HTTP cluster or stream router construction. Snapshot hooks are added only by Tasks 7 and 8 with their real input types.

### C6.6 — Integration and merge gate

**Depends on:** C6.2–C6.5.

Adapt `cmd/root.go`, standalone ingestion, current Builder/server/stream callers only as needed to keep the pre-Task-9 production owner buildable. Do not add a global Store or raw-keyring fallback. Run the parent Task 6 focused race commands, capability generator check, scoped lint, build, dead-code/call-site scans, and an independent merge-level review. Merge the complete Task 6 branch into local `master` only after all gates pass.

## Parallelism and Ownership

- C6.1 and C6.2 are sequential single-owner foundation work.
- After C6.2 freezes signatures, secret/runtime, Store broker, and plugin leaf migration may use separate worktrees only where files are exclusive.
- Compiler factory work starts only after the attempt and plugin contracts compile.
- One integration owner resolves live-caller fallout. Workers may implement and run focused tests locally but do not commit, push, merge, or modify another lane's files.

## Completion Evidence

- Exact catalog parity and deterministic digest.
- Candidate/recovery overlap, cross-attempt rejection, cross-domain same-key rejection, revoke/close race and redaction.
- Metadata absent/schema-invalid/valid-materialized ordering with no Store access.
- Third-plugin failure stops tasks and releases the first two leases exactly once in reverse order.
- Recovery rejects revision/domain/candidate mismatches without desired-state access or disposition.
- Concurrent exact discard closes once; mismatched publication leaves the candidate open.
- No production plugin imports `pkg/data_encryption` or calls Store secret/metadata globals.
- Focused race gates, `cmd/capability-gen -check`, scoped lint, `make build`, `git diff --check`, and independent review pass.
