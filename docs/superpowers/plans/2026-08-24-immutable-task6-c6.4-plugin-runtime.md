# Immutable Task 6 C6.4-C6.6 Plugin Runtime Execution Plan

> **Execution rule:** implement this plan checkpoint by checkpoint. Every child worktree starts from the named merged checkpoint on `codex/apisix-go-immutable-task6`; child commits return to that integration branch, never directly to `master`. Push and PR publication are outside the current authority.

**Goal:** remove production plugin dependence on package-global Store secret/metadata/consumer lookups, bind every sensitive value and plugin instance to one final publication attempt, and hand the current pre-Task-9 Builder an exact immutable dependency bundle without creating a second activation path.

**Architecture:** C6.4 is split into pure/additive contracts and leaf migrations. C6.5 owns final publication refinement, attempt registration, metadata/consumer materialization, plugin preparation, and reverse-order cleanup. C6.6 is the only current-production integration owner. It passes the exact final views and registrations into the existing Builder/server lifetime; it does not reconstruct them from Store.

**Baseline:** `720df206` (`feat(store): isolate secret attempt views`).

**Tech stack:** Go 1.26, capability manifest, immutable generation publications, attempt-scoped secret materializer, immutable runtime views, focused race tests, AST boundary guards.

## Non-negotiable invariants

1. Raw publication schema validation happens before secret access. The exact order is:

   ```text
   prepare raw publication
   -> build schema witnesses
   -> validate/refine desired metadata and plugin documents
   -> produce final PublicationSet
   -> register final candidate/recovery AttemptID
   -> build GenerationCapability
   -> materialize metadata and consumer credentials
   -> prepare plugins/resources/tasks
   -> expose PreparedGeneration
   ```

2. No new path may fall back to package-global Store, `data_encryption.Resolver`, an environment lookup outside `GenerationCapability`, or a raw keyring.
3. Intermediate commits compile. Shared API changes are additive until the compiler and current production caller use the new path; legacy methods are deleted atomically at the C6.6 boundary.
4. A descriptor or error may retain a source class and digest, never a raw reference, environment variable name, Vault path, ciphertext, or plaintext.
5. `secret.Value` contains Go strings that cannot be reliably zeroed. Code must drop references and close attempt-owned resolver state; it must not claim that Go strings were wiped.
6. Plugin instances, metadata views, consumer bindings, tasks, and leases share one attempt lifetime. Reusable generation-neutral clients use separate digest-keyed leases and retain none of those objects.
7. Same resource bytes in two attempts still produce different plugin instance keys.
8. The final registration is closed only after the installed handler generation retires. Prepare failure closes acquired objects once in reverse order.
9. Task 6 does not invent Task 7 HTTP snapshots, Task 8 stream snapshots, or Task 9 activation. C6.6 adapts the current Builder/server owner only.
10. Full Store plaintext deletion is a completion condition only if C6.6 can inject and retain the final C6.5 bundle without a dual activation path. Otherwise the remaining decoded snapshot is explicitly deferred to Task 9 and Task 6 is not described as eliminating all process plaintext.

## Source-of-truth amendments

The capability manifest remains the only editable secret declaration catalog.

### Existing plugin-config compatibility additions

Add these twelve optional (`strict: false`) declarations without changing existing strict entries:

| Factory | Field |
| --- | --- |
| `ai-aliyun-content-moderation` | `access_key_id` |
| `ai-aws-content-moderation` | `comprehend.access_key_id` |
| `ai-aws-content-moderation` | `comprehend.session_token` |
| `clickhouse-logger` | `user` |
| `limit-count` | `key` |
| `limit-count` | `redis_host` |
| `limit-count` | `redis_config.redis_host` |
| `limit-count` | `redis_cluster_nodes` |
| `limit-count` | `redis_cluster_config.redis_cluster_nodes` |
| `oas-validator` | `spec` |
| `oas-validator` | `spec_url_request_headers` |
| `openid-connect` | `public_key` |

Container declarations name the container; do not add terminal `.*`.

### Consumer credential source

Add `consumer_config` as a distinct `SecretDeclarationSource`. It participates in declaration validation, catalog lookup/digest, attempt scope validation, and secret materialization. It does **not** participate in plugin-config at-rest encryption enumeration; this preserves the current boundary instead of silently expanding `data_encryption` semantics.

Compatibility declarations cover every currently resolved string in the supported consumer schemas:

| Factory | Fields |
| --- | --- |
| `basic-auth` | `username`, `password` |
| `key-auth` | `key` |
| `jwt-auth` | `key`, `secret`, `public_key`, `private_key`, `algorithm` |
| `hmac-auth` | `key_id`, `secret_key` |
| `ldap-auth` | `user_dn` |
| `jwe-decrypt` | `key`, `secret` when the value is a string |
| `wolf-rbac` | `appid`, `header_prefix`, `server`, `wolf_url` |

`wolf-rbac` is intentionally dual-declared during convergence: Store validates and currently resolves the historical `wolf_url`, but the plugin consumes `server`. This is a recorded compatibility divergence, not a naming decision hidden inside the migration.

## Worktree and merge topology

```text
T6 integration @ 720df206
  -> CP1 catalog compatibility
  -> CP2 additive plugin/runtime contracts
  -> CP3 C6.5 pure factory skeleton and final-attempt boundary
      -> lane S1 Store-materializer plugins
      -> lane S2 raw-resolver plugins
      -> lane M1 ordinary metadata consumers
      -> lane A1 auth consumer bindings
  -> CP4 merge leaf lanes
      -> lane X1 composites: workflow + multi-auth
      -> lane M2 special metadata: batch/error-log/dynamic readers
  -> CP5 C6.5 prepared ownership integration
  -> CP6 C6.6 current-production integration and legacy deletion
  -> Task 6 review/gates
  -> local master only after acceptance
```

Dependent worktrees always branch from the latest merged checkpoint on the Task 6 integration branch. They do not branch from stale `master`, and they do not merge directly to `master`.

Shared-file ownership is serial:

- catalog owner: `pkg/capability/**`, parity tests;
- contract owner: `pkg/runtime/**`, `pkg/plugin/base/**`, `pkg/plugin/types.go`, `pkg/plugin/init.go`, `pkg/plugin/instance.go`, common guards/fixtures;
- compiler owner: `pkg/compiler/**`;
- integration owner: `pkg/route/**`, `pkg/server/**`, `cmd/**`, shared Store deletion;
- leaf workers own only their named plugin package directories and tests.

## Task 1: CP1 — Extend catalog compatibility

**Files:**

- Modify: `pkg/capability/types.go`
- Modify: `pkg/capability/load.go`
- Modify: `pkg/capability/manifest.yaml`
- Modify: `pkg/capability/declaration_catalog_test.go`
- Modify: `pkg/data_encryption/declaration_catalog_parity_test.go`
- Modify: `pkg/secret/materializer.go`
- Modify: `pkg/store/secret_broker.go`
- Modify: focused secret materializer tests
- Modify: focused Store broker tests

- [ ] Extend the parity oracle with the twelve `plugin_config` fields and prove the test fails before manifest edits.
- [ ] Add `SecretConsumerConfig = "consumer_config"`; accept it in manifest/catalog and secret scope validation.
- [ ] Accept the source in the temporary Store broker's scope admission; catalog, materializer, and broker must agree on the same source set.
- [ ] Add the consumer matrix above, including both `server` and `wolf_url` for `wolf-rbac`.
- [ ] Prove catalog digest changes with source/field/strict changes and remains deterministic.
- [ ] Prove `EncryptPluginConfigs`, `DecryptPluginConfigs`, and metadata enumeration remain unchanged and ignore `consumer_config`.
- [ ] Prove container declarations resolve each string element/value through one canonical field declaration.
- [ ] Run:

  ```bash
  source .envrc
  GOFLAGS=-mod=readonly go test -race ./pkg/capability ./pkg/data_encryption ./pkg/secret \
    -run '(Declaration|Catalog|Parity|ConsumerConfig|Collection|Scope)' -count=1
  GOFLAGS=-mod=readonly go run ./cmd/capability-gen -check
  git diff --check
  ```

**Checkpoint commit:** `feat(capability): declare scoped plugin and consumer secrets`

## Task 2: CP2 — Freeze additive attempt-bound contracts

**Files:**

- Modify: `pkg/runtime/dependencies.go`
- Create/modify: runtime consumer-view files and tests
- Modify: `pkg/plugin/base/types.go`, `secrets.go`, `metadata.go` and tests
- Modify: `pkg/plugin/types.go`, `init.go`, `instance.go`, `executor.go` and focused tests
- Create only if repeated fixtures require it: a focused scoped-secret test helper

- [ ] Add a concrete immutable `runtime.ConsumerBindings` value, constructor, and indexes. `runtime` must not import plugin/base or Store.
- [ ] Define only the minimal read interface in `pkg/plugin/base`: credential-key lookup, anonymous-consumer lookup, and consumer-group lookup. Put that interface in `base.Dependencies`; the concrete runtime value satisfies it without creating a reverse dependency.
- [ ] Make the read API request-time lookup only and return defensive copies. Construction/materialization stays in compiler preparation code and accepts the manifest catalog plus final capability; preparation errors never become request-time lookup errors.
- [ ] Add `Consumers` to the additive dependency bundle alongside Config, Secrets, Metadata, and Tasks.
- [ ] Introduce scoped secret materialization with explicit `context.Context`, immutable base `secret.Scope`, and `secret.GenerationCapability`. Plugin code may change only `Field`.
- [ ] Add `Attempt` to `plugin.InstanceKey`; reject zero attempts and include it in equality/string identity.
- [ ] Keep legacy constructors/materializers callable only for existing production while leaf work proceeds. Mark them transitional; do not let new compiler code use them.
- [ ] Define a redacted `SecretDescriptor` containing source class plus digest only. It must not accept/store raw input.
- [ ] Add tests for cross-attempt key separation, capability/scope mismatch rejection before resolver access, redacted failures, and immutable consumer lookup copies.
- [ ] Run focused plugin/base/runtime/secret tests plus `make build`.

**Checkpoint commit:** `refactor(plugin): add attempt-bound runtime dependencies`

## Task 3: CP3 — Build the pure C6.5 factory skeleton

**Files:** `pkg/compiler/**` and focused tests only.

- [ ] Extract/consume pure `preparePublication` and build a schema-witness set without secret access or Store reads.
- [ ] A schema witness may call only the lifecycle necessary to expose schemas. If a factory `Init` needs runtime configuration, add a narrow schema-provider seam; do not construct a fake empty dependency bundle and hope all plugins initialize.
- [ ] Implement pure per-resource metadata/plugin admission and last-good/fail-closed refinement. Validate a predecessor before reuse.
- [ ] Rebuild and validate the exact final `PublicationSet` after refinement.
- [ ] Register only that final set, then create the capability. Tests must show invalid desired bytes are never registered and resolver calls remain zero during refinement.
- [ ] Add additive hooks for metadata materialization, consumer binding preparation, and plugin preparation after registration; leaf lanes may target these frozen interfaces.
- [ ] Recovery validates committed publication structure and schema, registers recovery, then materializes. It never consults desired state or computes a new disposition.
- [ ] Run focused compiler/generation/secret race tests and `make build`.

**Checkpoint commit:** `feat(compiler): register refined generation attempts`

## Task 4: Lane S1 — Replace Store secret materializers

**Exclusive packages:**

- `ai_aliyun_content_moderation`
- `ai_aws_content_moderation`
- `ai_rag`
- `authz_keycloak`
- `clickhouse_logger`
- `limit_count`
- `oas_validator`
- `openid_connect`

- [ ] Convert existing `$ENV` tests to the scoped capability and add at least one managed-reference case per behavior family.
- [ ] Materialize only declared fields. Empty optional fields do not call the capability.
- [ ] Preserve limit-count root/nested alias provenance before normalization and use container declarations for cluster nodes.
- [ ] Remove PostInit self-materialization. Compiler admission is the sole materialization phase.
- [ ] Retain `secret.Value` or build provider clients inside `Value.Use`; never copy plaintext back into public config. On stop/failure, drop references and rely on attempt close for owned-byte clearing.
- [ ] Prove config/error output contains descriptor/digest only.
- [ ] Run each affected package test; add race only to packages with mutable retained client/secret state.

## Task 5: Lane S2 — Replace raw encryption resolvers

**Exclusive packages:**

- `ai_rate_limiting`, `csrf`, `kafka_proxy`, `response_rewrite`
- `elasticsearch_logger`, `error_log_logger`, `google_cloud_logging`, `http_logger`, `kafka_logger`
- `lago`, `loggly`, `rocketmq_logger`, `sls_logger`, `splunk_hec_logging`, `tencent_cloud_cls`

- [ ] Add failing tests showing declared strict ciphertext, optional literal, `$ENV`, and `$secret` behavior where applicable.
- [ ] Replace `BasePlugin.DataEncryption()` with scoped capability materialization.
- [ ] Preserve the current field path and strict/optional behavior from the manifest catalog; do not add a private table in plugin code.
- [ ] Preserve logger client/batch ownership but move long-lived tasks/leases under generation ownership where the plugin already starts them.
- [ ] Remove every production import of `pkg/data_encryption` from these packages.
- [ ] Run affected package tests and focused race tests for async logger paths.

## Task 6: Lane M1 — Migrate ordinary metadata consumers

**Scope discovery gate:** freeze the live list with an AST/import-aware inventory before edits; expected count at baseline is 27 production consumers.

- [ ] Replace `base.LoadPluginMetadata`, direct `store.GetPluginMetadata*`, and raw Store metadata access with `runtime.MetadataView.Decode`.
- [ ] Decode once during generation/plugin preparation; request handlers read instance-owned immutable values.
- [ ] Preserve aliases explicitly, including `graphql-limit-count` reading the `limit-count` metadata document.
- [ ] Preserve precedence explicitly, including OpenTelemetry `plugin_metadata > plugin_attr > defaults`.
- [ ] Validate raw schema before any metadata secret materialization; materialize declared metadata fields only after final attempt registration.
- [ ] Run focused package tests proving generation N remains unchanged after N+1 is prepared.

## Task 7: Lane A1 — Build immutable consumer bindings

**Primary packages:** compiler/runtime construction plus auth plugin leaf packages. Shared compiler files remain with the compiler owner; auth workers modify only plugin packages/tests.

- [ ] Build bindings from the final HTTP candidate after attempt registration. Each consumer plugin config is schema-validated, then only declared string fields are materialized with source `consumer_config` and resource owner `consumers/<id>`.
- [ ] Index exact request-time lookup keys currently used by `consumerPluginLookupKey`; reject duplicates deterministically.
- [ ] Preserve anonymous-consumer lookup and consumer-group data needed by workflow without Store calls.
- [ ] Migrate `basic_auth`, `key_auth`, `jwt_auth`, `hmac_auth`, `ldap_auth`, `jwe_decrypt`, and `wolf_rbac` to immutable bindings.
- [ ] For JWE `any` values, materialize only string values; preserve non-string schema behavior.
- [ ] Prove two prepared generations use different bindings even when requests overlap; retirement of N does not invalidate N+1.
- [ ] Prove errors contain consumer resource identity/field digest only, not credentials or raw references.

## Task 8: CP4 composites — Preserve child ownership

**Depends on:** Tasks 4 and 7 merged.

**Exclusive packages:** `pkg/plugin/workflow/**`, `pkg/plugin/multi_auth/**`.

- [ ] `workflow` constructs `limit-count` children through the frozen scoped preparation path with the outer resource owner and same attempt.
- [ ] `multi-auth` constructs auth children through the same dependency bundle and same consumer bindings.
- [ ] Child `InstanceKey` includes child factory plus outer provenance plus attempt; siblings cannot collide.
- [ ] Partial child failure stops already-prepared children exactly once in reverse order.
- [ ] Delete direct calls to legacy `base.MaterializePluginSecrets` only after both composites use the scoped path.

## Task 9: CP4 special metadata owners

**Depends on:** Task 6 ordinary view contract and CP3 final-attempt boundary.

- [ ] `chaitin_waf`: remove request-time polling/cache and bind effective config once per generation.
- [ ] `authz_casbin`: construct the metadata enforcer once; request handling never reads Store.
- [ ] `batch_requests`: replace Store last-good/cache with limits decoded from the final view. C6.5 owns last-good/fail-closed selection.
- [ ] `error_log_logger`: make global metadata use its explicit schema owner, materialize strict metadata after registration, and bind observer/task cleanup to the prepared generation.
- [ ] `otel`: preserve metadata-over-attribute precedence with focused tests.
- [ ] Add generation N/N+1 overlap and invalid-desired/valid-predecessor tests at the narrowest owning layer.

## Task 10: CP5 — Complete PreparedGeneration ownership

**Files:** `pkg/compiler/**`, common runtime types/tests.

- [ ] Implement candidate and recovery construction in this order:

  ```text
  final set
  -> registration/capability
  -> task registry
  -> consumer bindings
  -> final metadata view
  -> plugin/resource leases
  -> PreparedGeneration
  ```

- [ ] Define one cleanup stack. Any failure closes the last acquired object first. `Discard`/`Close` is concurrent and idempotent.
- [ ] `PreparedGeneration` exposes defensive identity/view/accessor data only; it never exposes raw resolver/broker/Store handles.
- [ ] A third-plugin failure test proves the first two plugins, leases, tasks, bindings, materialized values, and registration close once in reverse order.
- [ ] A recovery test proves no desired-state access and no disposition call.
- [ ] Run focused compiler/runtime/plugin race tests and build.

**Checkpoint commit:** `feat(compiler): own prepared plugin generations`

## Task 11: CP6 — Integrate the current production owner

**Single integration owner files:** `pkg/route/**`, `pkg/server/**`, required `cmd/**`, and only the Store accessors proven dead by call-site scans.

- [ ] Pass the exact C6.5 final metadata view, consumer bindings, scoped plugin dependencies, tasks, and registration lifetime into the existing Builder/server generation.
- [ ] Do not build a temporary view from `Store.ConfigSnapshot` and do not decrypt metadata twice.
- [ ] Tie registration retirement to the installed handler generation, not merely Builder return.
- [ ] Preserve current reload rollback: failed candidate does not replace the active handler; cleanup completes before error return.
- [ ] Keep one production activation path. If exact ownership would require adding Task 9 activation, stop the deletion at the documented compatibility seam and defer that seam explicitly.
- [ ] Delete legacy plugin/global paths only after AST and `rg` scans prove zero production call sites:

  ```text
  BasePlugin.DataEncryption
  store.MaterializeSecret / ResolveSecretReference from plugins
  store.GetPluginMetadata / GetPluginMetadataRaw / GetValidatedPluginMetadata from plugins
  request-time Store consumer credential resolution from auth plugins
  transitional parameterless MaterializePluginSecrets
  ```

- [ ] If no caller needs decoded Store plugin metadata, remove the decoded cache/snapshot accessors and prove Store/journal retain raw publication bytes only. If a pre-Task-9 owner still needs them, record the exact caller and Task 9 deletion step.

**Checkpoint commit:** `feat(runtime): install prepared plugin generations`

## Task 12: Boundary guards and acceptance

- [ ] Add import-aware AST guards rejecting plugin production imports/calls for Store metadata, Store secret materialization, and `pkg/data_encryption`. Tests cover default aliases, renamed aliases, and dot imports.
- [ ] Run exact zero-call searches and classify any remaining Store import.
- [ ] Run focused race gates derived from the final diff; include compiler, secret, runtime, affected plugin batches, Store, route, and server only where directly affected.
- [ ] Run:

  ```bash
  source .envrc
  GOFLAGS=-mod=readonly go run ./cmd/capability-gen -check
  make lint
  GOFLAGS=-mod=readonly make build
  git diff --check
  ```

- [ ] Run an independent merge-level review. Resolve blockers in the Task 6 integration branch and rerun invalidated gates.
- [ ] Confirm the Task 6 branch is clean and enumerate any explicit Task 9 deferral.
- [ ] Merge the complete Task 6 branch into local `master`; only then branch dependent Task 7 worktrees from the new master SHA.

## Acceptance ledger

Task 6 is acceptable only when all applicable rows have evidence:

| Boundary | Required proof |
| --- | --- |
| Catalog | exact plugin/metadata/consumer declaration parity and deterministic digest |
| Attempt | final refined bytes registered; cross-attempt/domain/resource rejection before resolver access |
| Secrets | no raw reference/plaintext diagnostics; no package-global plugin resolver path |
| Metadata | raw schema admission before secret access; N/N+1 immutable overlap |
| Consumers | immutable prepared bindings; no request-time Store credential resolution |
| Plugins | attempt-scoped instance keys; composite children retain outer ownership |
| Cleanup | partial failure and concurrent discard close once in reverse order |
| Production | one current activation path; registration lasts through handler retirement |
| Residual | any Store plaintext/legacy seam named with exact caller and Task 9 deletion owner |
| Gates | focused race, generator drift, lint, build, diff, independent review |

## Explicit non-goals

- No Task 7 HTTP cluster/snapshot implementation.
- No Task 8 stream router implementation.
- No Task 9 supervisor/worker activation protocol.
- No new external runner or Lua/OpenResty compatibility.
- No broad plugin schema cleanup, including choosing between `wolf_url` and `server`; that divergence is recorded for a separate compatibility decision.
