# Immutable Task 6 CP3 Compiler Skeleton Implementation Plan

> **Execution rule:** execute each checkpoint on `codex/apisix-go-immutable-task6` descendants. A later worktree starts only from the SHA produced by the preceding merged checkpoint. Intermediate commits must build. No push or PR is authorized.

**Goal:** establish one schema source of truth, perform side-effect-free raw admission and per-resource refinement, validate the exact final publication set, and only then register a candidate/recovery attempt and expose additive preparation hooks.

**Baseline:** `176b218d` (`feat(runtime): add immutable consumer bindings`).

**Architecture:** CP3 has four serial checkpoints: consumer schema ownership, plugin schema witnesses, pure raw admission/refinement, and final-attempt orchestration. CP3 freezes hook contracts but does not implement metadata/consumer/plugin leaf materialization, current Builder integration, or final `PreparedGeneration` activation.

## Corrected evidence boundaries

1. Regular plugin `GetSchema()` describes route/service/global/plugin-config use. It does not describe consumer credentials.
2. Seven consumer credential schemas and JWE validation are currently private to `pkg/store/consumer_kv.go`; copying them into plugin would create two editable truth sources, while Store importing plugin would create the wrong dependency direction.
3. The target canonical owner is a new low-level `pkg/consumer` registry. Store and compiler consume it; runtime bindings hold only already-validated values; plugin does not own or mirror it.
4. Current predecessor checks do not validate closure/decisions completely. Full `generation.ValidatePublishedGeneration` is required before last-good reuse.
5. Current `PreparePublication` does not call `generation.ValidatePublicationSet` before returning. Registration-time validation is too late to prove the pure phase.
6. Recovery may contain independent HTTP/stream revisions. Candidate `ValidatePublicationSet` cannot validate recovery; recovery uses exact revision coverage plus `ValidatePublishedGeneration` for each domain.
7. Raw schema admission must explicitly admit recognized secret envelopes at declared fields, then the resolved value must pass the original unmodified schema after materialization. This is a deliberate compatibility envelope, not a general schema bypass.

## Canonical-source ledger

| Field | Decision |
| --- | --- |
| Change intent | validate consumer credentials in both legacy Store and new compiler without duplicate schemas |
| Current candidate | private constants/types/validators in `pkg/store/consumer_kv.go` |
| Canonical target | `pkg/consumer` definition registry |
| Consumers | legacy Store snapshot/index and CP3/CP5 compiler preparation |
| Derived mirrors | none; plugin must not copy schema strings |
| Intentional divergence | `wolf-rbac` admits both historical `wolf_url` and runtime `server` until a separate compatibility decision |
| Verification | exact legacy test parity, registry factory/field parity with capability `consumer_config` declarations, zero private Store schema definitions |
| Confidence | high |

## Dependency graph

```text
CP2 contracts @ 176b218d
  -> CP3.1 consumer schema/lookup registry
  -> CP3.2 schema-only plugin witnesses
  -> CP3.3 raw admission + refinement + final set validation
  -> CP3.4 final-attempt registration + additive hooks
  -> leaf migration worktrees
```

## CP3.1 — Move consumer schema and lookup ownership

**Checkpoint commit:** `refactor(consumer): own credential schemas and lookup keys`

**Exclusive files:**

- Create: `pkg/consumer/registry.go`
- Create: `pkg/consumer/registry_test.go`
- Modify: `pkg/store/consumer_kv.go`
- Modify: focused Store consumer tests
- Modify only for parity: capability/data-encryption catalog parity tests

### Contract

The package exports immutable definitions through behavior, not mutable schema maps:

```go
type Registry struct { /* immutable compiled definitions */ }

func NewRegistry() (*Registry, error)
func (r *Registry) Factories() []string
func (r *Registry) ValidateResolved(factory string, config any) error
func (r *Registry) LookupKey(factory string, config any) (string, error)
```

`Registry` owns exactly: `basic-auth`, `key-auth`, `jwt-auth`, `hmac-auth`, `ldap-auth`, `jwe-decrypt`, and `wolf-rbac`. This checkpoint owns resolved schema/lookup behavior only. CP3.3 adds raw secret-envelope admission using the manifest catalog; secret-capable paths are never copied into this registry. Returned errors are redacted at compiler boundaries; legacy Store may wrap them with its existing factory context.

### Steps

- [ ] Write parity tests that enumerate current valid/invalid Store fixtures through the proposed registry and fail because the registry does not exist.
- [ ] Move, do not copy, the six JSON schemas and JWE custom validation into `pkg/consumer`.
- [ ] Define `wolf-rbac` with both `server` and `wolf_url`; retain `appid` as lookup key.
- [ ] Move lookup-key extraction/type validation into the registry so CP5 does not reimplement Store structs/switches.
- [ ] Make Store snapshot preparation consume registry `ValidateResolved` and `LookupKey` while preserving duplicate-key/reference indexing behavior.
- [ ] Keep raw-reference resolution order unchanged in Store during this checkpoint.
- [ ] Delete private Store compiled schemas, schema structs used only for validation/lookup, and `mustCompileConsumerSchema` after zero call-site proof.
- [ ] Add a relationship test proving registry factories and fields equal the manifest `consumer_config` declarations.
- [ ] Run:

  ```bash
  source .envrc
  GOFLAGS=-mod=readonly go test -race ./pkg/consumer ./pkg/store \
    -run '(Consumer|Credential|JWE|Wolf|LookupKey|Schema)' -count=1
  GOFLAGS=-mod=readonly go test ./pkg/capability ./pkg/data_encryption \
    -run '(Declaration|Parity|ConsumerConfig)' -count=1
  GOFLAGS=-mod=readonly make build
  git diff --check
  ```

## CP3.2 — Expose schema-only plugin witnesses

**Checkpoint commit:** `refactor(plugin): expose schema-only factory witnesses`

**Exclusive files:**

- Create: `pkg/plugin/schema_witness.go`
- Create: `pkg/plugin/schema_witness_test.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/manifest_contract_test.go`
- Modify only if required by a confirmed schema owner: focused plugin package files/tests

### Contract

```go
type SchemaWitness struct {
    Factory  string
    Config   string
    Metadata string
}

func SchemaWitnessForFactory(factory string) (SchemaWitness, error)
```

### Steps

- [ ] Write RED tests for unknown factory, every manifest factory, copied immutable strings, and Init-only execution.
- [ ] Construct through the raw generated `pluginRegistry` factory, not public `plugin.New` and not fake empty runtime dependencies.
- [ ] Call only `Init`; never call PostInit, handlers, Store, secret materialization, task/resource registration, or consumer lookup.
- [ ] Add an AST contract over registered `Init` methods that rejects runtime dependency access and side-effect owners. If an existing Init violates it, stop and isolate an explicit schema provider rather than weakening the guard.
- [ ] Compile every non-empty config/metadata schema once in the test.
- [ ] Keep consumer schemas out of `BasePlugin` and plugin packages; CP3.1 is their sole owner.
- [ ] Run focused plugin witness/manifest tests and `make build`.

## CP3.3 — Pure raw admission and exact refinement

**Checkpoint commit:** `refactor(compiler): validate refined publication sets`

### Buildable prerequisites discovered during CP3.1/CP3.2

CP3.3 starts with two independent, merge-before-compiler checkpoints. The
original plan assumed both seams already existed; current source proves they do
not.

1. `refactor(consumer): expose credential schema witnesses`
   - `pkg/consumer` remains the sole owner of consumer schemas and resolved JWE
     validation.
   - Expose copied schema strings for raw admission without exposing validators,
     secret paths, or mutable registry state.
   - Add a structural JWE raw schema while preserving its existing resolved
     length/base64 validator and diagnostics.
2. `refactor(capability): centralize declared field traversal`
   - Add one deterministic declaration-path walker under `pkg/capability`.
   - Migrate data-encryption field traversal to it and delete the duplicate
     encrypt/decrypt walkers before compiler consumes the seam.
   - Add a pure raw-envelope classifier; it recognizes envelopes but does not
     resolve references or validate ciphertext.

These checkpoints may be implemented in parallel because their files do not
overlap. Compiler work branches only from their merged SHA.

**Exclusive files:**

- Create: `pkg/compiler/schema.go`
- Create: `pkg/compiler/schema_test.go`
- Modify: `pkg/compiler/compiler.go`
- Modify: `pkg/compiler/types.go`
- Modify: `pkg/compiler/validate.go`
- Modify: `pkg/compiler/closure.go`
- Modify: focused compiler tests
- Modify only for a reusable pure helper: `pkg/generation/publication_validation.go` and tests

### Internal contracts

```go
type schemaSource interface {
    Plugin(factory string) (plugin.SchemaWitness, error)
}

type schemaSet struct {
    /* compiled config and metadata validators */
    /* compiled copied consumer schema witnesses */
}
```

The public package need not expose mutable compiled schema maps.

### Raw admission rules

- [ ] Validate configured plugins in routes, stream routes, services, global rules, plugin config rules, and consumer groups with the regular config schema.
- [ ] Validate `plugin_metadata/<factory>` with the factory metadata schema.
- [ ] Validate consumer auth configs through `pkg/consumer`, not regular plugin schemas.
- [ ] Treat `plugins/plugins` as singleton/name/domain enablement, not plugin config.
- [ ] Missing/invalid witnesses are bootstrap defects; resource value mismatch emits redacted `plugin-schema-invalid`, `plugin-metadata-schema-invalid`, or `consumer-schema-invalid` issues.
- [ ] At an exact manifest-declared field, a recognized `$secret://`, case-insensitive `$ENV://`, or supported encrypted envelope may satisfy raw admission even when the original value constraint would reject it. Non-declared fields never receive this relaxation.
- [ ] Use the merged `pkg/capability` declared-field walker for config, metadata, and consumer admission. Container declarations use that exact behavior; do not copy traversal or invent terminal wildcards.
- [ ] After materialization, CP5/leaf preparation validates against the original schema; raw admission never certifies the resolved value.
- [ ] Schema errors must not retain validator text that can echo a reference, ciphertext, or plaintext.

### Refinement steps

- [ ] Inject raw schema issues before secret-edge construction and closure selection; resolver/materializer/task calls remain zero.
- [ ] Replace partial predecessor checks with `generation.ValidatePublishedGeneration` and reject same/future predecessors.
- [ ] Apply existing per-resource last-good/fail-closed policy to schema issues.
- [ ] Re-run normalize, raw admission, dependencies, and closure over selected effective bytes.
- [ ] Validate predecessor bytes again; invalid predecessors fail closed.
- [ ] Rebuild each `PublicationCandidate` from final decisions/bytes.
- [ ] Call `generation.ValidatePublicationSet(ticket, finalSet)` immediately before pure `PreparePublication` returns.

### RED/GREEN evidence

- [ ] Invalid config/metadata/consumer desired bytes with valid predecessor reuse exact validated predecessor bytes.
- [ ] First startup/no valid predecessor fails closed.
- [ ] Forged predecessor closure/decision gaps and future revision are rejected.
- [ ] Declared envelope passes raw admission; unrelated enum/type error and undeclared envelope fail.
- [ ] Invalid desired and predecessor produce zero registration/resolver calls.
- [ ] Final validator catches a test-forged post-refinement set.
- [ ] Run focused compiler/generation tests, compiler race tests, and build.

## CP3.4 — Register only final attempts and freeze hooks

**Checkpoint commit:** `feat(compiler): register refined generation attempts`

**Exclusive files:**

- Create: `pkg/compiler/factory.go`
- Create: `pkg/compiler/hooks.go`
- Create: `pkg/compiler/factory_test.go`
- Create: `pkg/compiler/recovery_test.go`
- Modify: `pkg/compiler/types.go`
- Modify focused generation validation only if exact recovery coverage is centralized

### Contracts

```go
type PreparationAttempt struct { /* unexported authority */ }

func (a PreparationAttempt) Generation() uint64
func (a PreparationAttempt) AttemptID() secret.AttemptID
func (a PreparationAttempt) Candidate(domain generation.Domain) (generation.PublicationCandidate, bool)
func (a PreparationAttempt) Secrets() secret.GenerationCapability

type MetadataPreparer interface {
    PrepareMetadata(context.Context, PreparationAttempt) (runtime.MetadataView, error)
}

type ConsumerPreparer interface {
    PrepareConsumers(context.Context, PreparationAttempt, runtime.MetadataView) (*runtime.ConsumerBindings, error)
}

type PluginPreparer interface {
    PreparePlugins(context.Context, PreparationAttempt, runtime.MetadataView, *runtime.ConsumerBindings) (PreparedPlugins, error)
}

type PreparedPlugins interface {
    Close(context.Context) error
}
```

Hooks receive immutable bound access, not Store, materializer, registration, or raw keyring handles.

### Candidate flow

- [ ] Run pure `PreparePublication` and revalidate the final set at the side-effect boundary.
- [ ] Call `RegisterCandidate(ticket, exactFinalSet)` and immediately install reverse cleanup ownership.
- [ ] Create `GenerationCapability` with the final registration and desired revision.
- [ ] Call metadata, consumer, then plugin hooks in exact order.
- [ ] Before plugin preparation, every effective factory with manifest `plugin_config` declarations must implement CP2 `ScopedSecretMaterializer`; absence fails without calling legacy code.
- [ ] Add a poison dual-interface fixture proving the new factory never calls `MaterializePluginSecrets` or legacy `MaterializeSecrets`.
- [ ] Hook failure closes prepared objects then registration exactly once using cleanup context independent of canceled request context.

### Recovery flow

- [ ] Accept only `RevisionSet` plus committed `map[Domain]PublishedGeneration`; expose no desired provider.
- [ ] Require exact non-zero revision/domain coverage and reject unknown/extra domains.
- [ ] Validate each domain with `ValidatePublishedGeneration(domain, committedRevision, value)`.
- [ ] Run raw schema admission directly against committed snapshots; any failure aborts without disposition/refinement.
- [ ] Defensively clone but do not rebuild snapshot/artifact/closure/decisions.
- [ ] Call `RegisterRecovery` with the exact verified map, create capability with `revisions.Desired`, then run the same hooks.

### Acceptance tests

- [ ] Invalid desired schema produces zero registration calls.
- [ ] Registered candidate equals the final refined set byte-for-byte/artifact-for-artifact.
- [ ] Registration precedes all hooks; hook order is metadata → consumer → plugin.
- [ ] Candidate/recovery at one desired revision have distinct AttemptIDs.
- [ ] Recovery performs zero desired reads and zero disposition rewrites.
- [ ] Invalid recovery schema produces zero `RegisterRecovery` calls.
- [ ] Partial hook failure closes acquired objects/registration once in reverse order.
- [ ] Run focused compiler/secret race tests, scoped lint, build, and diff check.

## Worktree sequencing

- CP3.1–CP3.4 are serial checkpoints because later code consumes the preceding API.
- Within a checkpoint, tests may be delegated separately only if they do not share production files.
- The first leaf-migration worktrees branch from the merged CP3.4 SHA, never from `master` or an earlier CP3 child.
- Shared compiler/plugin/consumer files remain single-owner until CP3.4 is merged.

## Explicit deferrals

- Concrete metadata materialization and all metadata-reading plugin migrations.
- Concrete consumer secret materialization and auth plugin lookup migrations.
- Scoped plugin leaf materialization and composite migration.
- Full `PreparedGeneration` task/resource/lease cleanup stack (CP5).
- Current Builder/server/stream integration and legacy deletion (CP6).
