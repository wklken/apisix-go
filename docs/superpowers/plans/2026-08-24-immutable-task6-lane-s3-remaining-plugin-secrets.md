# Immutable Task 6 Lane S3 Remaining Plugin Secrets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** close the eighteen-factory gap left by Lanes S1 and S2 without disguising six phantom compatibility declarations as plugin-owned runtime secrets, then migrate the twelve real secret-consuming plugin packages to exact attempt-scoped materialization.

**Architecture:** S3-0 first extends the manifest declaration contract with an explicit runtime target, marks six schema-absent compatibility declarations as compiler-discard, restores two missing session-fallback declarations, and adds a compiler-owned raw-document preparer that resolves and immediately drops compiler-discard values before any plugin is decoded. Twelve file-exclusive leaf packages then implement real scoped and transitional legacy materializers, retain only `secret.Value`, redacted `secret.Descriptor`, or derived attempt-owned objects, and retire those objects with the generation. The final S3 gate enumerates all 41 manifest factories with `plugin_config` declarations and accepts each only through one visible owner: compiler-discard or `base.ScopedSecretMaterializer`.

**Tech Stack:** Go 1.26, capability manifest/catalog, immutable generation publications, `compiler.PreparationAttempt`, `base.ScopedSecretAccess`, `secret.Value` and `secret.Descriptor`, existing Store compatibility resolver, focused unit/race tests.

**Spec:** [`docs/superpowers/plans/2026-08-24-immutable-task6-c6.4-plugin-runtime.md`](2026-08-24-immutable-task6-c6.4-plugin-runtime.md), invariants 1-8 and Tasks 4-6; [`docs/superpowers/plans/2026-08-24-immutable-task6-cp3-compiler-skeleton.md`](2026-08-24-immutable-task6-cp3-compiler-skeleton.md), Task 7.

## Global Constraints

- Architectural prerequisite is `40c04a26` (`feat(compiler): register refined generation attempts`) and the descriptor prerequisite is `e5b6a73e` (`feat(secret): add redacted value descriptors`). S3-0 starts from integration commit `9ebcd2b5`, which already contains A1.1's accepted compiler consumer preparer and both prerequisites. Every later leaf starts from the merged S3-0 checkpoint or a later integration descendant.
- Run Go commands from the active worktree with `source .envrc` and `GOFLAGS=-mod=readonly`.
- S3-0 is a serial integration-owner checkpoint. Leaf workers own only their assigned `pkg/plugin/<name>/**` paths and do not edit the manifest, catalog, compiler, `pkg/plugin/base`, Store, route/server code, shared `ai_auth`, shared `ai_common`, shared `function_upstream`, or another leaf package.
- Raw publication schema admission remains before registration. Compiler-discard materialization and real plugin materialization happen only after final candidate/recovery registration. No S3 path may read desired state after registration or resolve a value outside the exact final occurrence.
- The capability manifest is the sole editable declaration and runtime-target catalog. Do not create a second phantom-field table in compiler, plugin, Store, tests, or documentation.
- A present declared leaf, including a literal, is admitted through `ScopedSecretAccess.Materialize(ctx, exactManifestField, raw)`. Empty optional fields make no call. Container declarations use the canonical container path for every selected leaf, never a concrete index or map key.
- Stage all values, descriptors, derived keys/clients, and public-config replacements in locals. If the Nth value fails, install nothing. Scoped values are dropped; transitional Store owners are destroyed. Errors are fixed/redacted and never contain a raw reference, environment name, Vault path, ciphertext, plaintext, provider body, cookie secret, private key, token, map key, or array index.
- Scoped public config retains only `secret.Value.Descriptor(capability.SecretPluginConfig).String()`. The transitional legacy path hashes the resolved private value and calls `secret.NewDescriptor`; it never passes raw input into that constructor. Runtime plaintext lives only in `secret.Value.Use` or a derived attempt-owned object created inside `Use`.
- The materialization phase performs every secret-dependent semantic check before installing private state. `PostInit` consumes only staged values/derived objects, applies non-secret defaults, and constructs clients/tasks; it does not resolve Store, environment, Vault, ciphertext, or raw keyrings. Direct tests/benchmarks that need secret-bearing behavior must explicitly invoke the legacy or scoped preparation phase first.
- `Stop` is idempotent. It first stops goroutines/batches, closes or releases credential-bearing clients/derived objects, then destroys transitional Store owners and drops scoped values. It must not claim Go strings were wiped.
- Every leaf proves generation N/N+1 isolation: different attempts may use the same resource key but cannot share credentials, tokens, derived keys, authenticated clients, or mutable caches. Retiring N must not change N+1.
- Leaf workers run tests and may prepare a local diff, but they do not commit, push, open a PR, cherry-pick, merge, or edit `master`. The Task 6 integration owner reviews each diff, runs its focused gates, and creates the checkpoint commit.

---

## Frozen inventory and ownership decision

At `e5b6a73e`, the manifest contains 41 distinct factories with at least one `plugin_config` declaration. S1 owns 8 and S2 owns 15 effective packages (`clickhouse-logger` is counted only in S1). S3 closes the remaining 18 factories.

### Six phantom compatibility factories

These declarations are used by Store/data-encryption compatibility but their named fields are absent from the route plugin's concrete `Config` and JSON schema. They must not be implemented as no-op plugin interfaces and must not be added as fake runtime fields to those configs.

| Factory | Existing phantom fields | Real runtime owner |
| --- | --- | --- |
| `basic-auth` | `password` | A1 owns `consumer_config.username/password`; compiler discards only a present raw route compatibility value |
| `key-auth` | `key` | A1 owns `consumer_config.key`; compiler discards the raw route compatibility value |
| `jwt-auth` | `secret`, `private_key` | A1 owns actual consumer keys/algorithms; compiler discards raw route compatibility values |
| `hmac-auth` | `secret` | A1 owns `consumer_config.key_id/secret_key`; compiler discards the raw route compatibility value |
| `ldap-auth` | `user_dn` | A1 owns the actual consumer `user_dn`; compiler discards the raw route compatibility value |
| `jwe-decrypt` | `key`, `secret` | A1 owns string consumer `key/secret`; compiler discards raw route compatibility values |

S3-0 gives these declarations an explicit manifest-owned compiler-discard target. The six auth packages remain exclusively available to A1; S3 makes no edits under their package directories.

### Twelve real leaf packages

| Group | Factory | Manifest fields after S3-0 | Current plaintext use and required private owner |
| --- | --- | --- | --- |
| AI | `ai-proxy` | `auth.header`, `auth.query`, `auth.gcp.service_account_json`, `auth.aws.secret_access_key`, `auth.aws.session_token` | request headers/query, GCP token exchange, AWS signing; private per-instance auth values |
| AI | `ai-proxy-multi` | same paths under `instances.*` | selected requests plus active-health headers/query; values aligned to immutable instance indexes |
| AI | `ai-request-rewrite` | same five non-array paths | rewrite provider request auth; private per-plugin values |
| function | `azure-functions` | `authorization.apikey` | route key injection; M1 separately owns metadata `master_apikey` |
| function | `openfunction` | `authorization.service_token` | Basic header construction |
| function | `openwhisk` | `service_token` | Basic header for every action request |
| function | `aws-lambda` | `authorization.apikey`, `authorization.iam.accesskey`, `authorization.iam.secretkey` | API-key header or SigV4 credentials |
| session | `authz-casdoor` | `client_secret`, `client_secret_fallbacks` | OAuth exchange plus AEAD session sealing/opening |
| session | `cas-auth` | `cookie.secret` | initiation-cookie HMAC |
| session | `dingtalk-auth` | `app_secret`, `secret`, **new `secret_fallbacks`** | token request, OAuth state/session signing, fallback verification and token cache identity |
| session | `feishu-auth` | `app_secret`, `secret`, `secret_fallbacks` | token request and state/session signing/verification |
| session | `saml-auth` | `sp_private_key`, `secret`, **new `secret_fallbacks`** | parsed SP signer and signed request/logout/session cookies |

## Dependency and worktree graph

```text
40c04a26 architecture
  -> e5b6a73e descriptor
      -> 9ebcd2b5 A1.1 compiler consumer preparer
          -> S3-0 manifest target + fallback parity + compiler discard preparer
              -> AI-1 ai_proxy ---------------------+
              -> AI-2 ai_proxy_multi ---------------+
              -> AI-3 ai_request_rewrite -----------+
              -> FN-1 azure_functions --------------+
              -> FN-2 openfunction -----------------+
              -> FN-3 openwhisk --------------------+
              -> FN-4 aws_lambda -------------------+--> S3 integration + all-41 gate
              -> AU-1 authz_casdoor ----------------+
              -> AU-2 cas_auth ---------------------+
              -> AU-3 dingtalk_auth ----------------+
              -> AU-4 feishu_auth ------------------+
              -> AU-5 saml_auth --------------------+

S3-FN1 azure route secret accepted -> M1-Azure metadata worktree
A1 auth package leaves ------------------------------> X1 composites
S1 + S2 + S3 accepted -------------------------------> CP5 / C6.6
```

All twelve real leaves may run in parallel after S3-0. `azure_functions` shares `plugin.go` and `plugin_test.go` with M1, so S3-FN1 lands first from the S3-0 checkpoint; the M1-Azure metadata worktree must branch from the accepted S3-FN1 commit and preserve its route-secret state and Stop ordering. The three AI workers do not edit `pkg/plugin/ai_auth` or `pkg/plugin/ai_common`; they keep small package-local private auth holders. The four function workers do not edit `pkg/plugin/function_upstream`; embedded `Stop` is called from the leaf's overriding `Stop`. If review proves a shared seam unavoidable, the integration owner stops the affected workers, lands one additive shared seam serially, and recreates those worktrees from that merged commit.

## Required package-local scoped fixture

Each real leaf test exercises the public boundary rather than forging `ScopedSecretAccess`. It loads the real manifest/catalog, builds one final HTTP publication containing the factory, registers it through `secret.NewScopedMaterializer`, constructs a `GenerationCapability`, and calls:

```go
if err := base.MaterializeScopedPluginSecrets(
    context.Background(), scope, capabilityValue, plugin,
); err != nil {
    t.Fatal(err)
}
```

The fake `secret.ScopedAttemptBroker` records complete `secret.Scope` values and returns configured plaintext. `t.Cleanup` closes the registration. An N/N+1 test creates two separate registrations and plugin instances; it does not reuse a capability, `secret.Value`, derived client, token cache, or plugin instance.

For every leaf, RED means the package does not yet implement `base.ScopedSecretMaterializer`. GREEN includes exact field scopes, optional absence, literal, `$ENV`, `$secret`, contextual/rotated ciphertext where current at-rest compatibility applies, Nth-value atomic failure, descriptor-only config, redacted errors, N/N+1 isolation, idempotent `Stop`, and a focused race gate over retained mutable state.

---

### Task 1: S3-0A — Classify runtime ownership and restore fallback declarations

**Files:**
- Modify: `pkg/capability/types.go`
- Modify: `pkg/capability/load.go`
- Modify: `pkg/capability/manifest.yaml`
- Modify: `pkg/capability/declaration_catalog_test.go`
- Modify: `pkg/data_encryption/declaration_catalog_parity_test.go`
- Modify only for target-aware traversal tests: `pkg/capability/secret_field_walk.go`, `pkg/capability/secret_field_walk_test.go`

**Interfaces:**
- Consumes: existing `SecretDeclaration{Factory, Source, Field, Strict}` and v1 catalog digest.
- Produces: validated `SecretMaterializationTarget`, `SecretDeclaration.EffectiveTarget()`, target-aware catalog traversal, and a v2 digest including the effective target.

- [ ] **Step 1: Write failing catalog tests**

Add tests named `TestSecretDeclarationCatalogClassifiesCompilerDiscardTargets`, `TestSecretDeclarationCatalogRejectsInvalidRuntimeTargets`, `TestSecretDeclarationCatalogDigestIncludesRuntimeTarget`, and parity cases for DingTalk/SAML fallback arrays. Assert exactly six factories have compiler-discard `plugin_config` declarations and the fallback fields are `plugin` target.

- [ ] **Step 2: Run RED**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/capability ./pkg/data_encryption -run "(RuntimeTarget|CompilerDiscard|Fallback|Parity|CatalogDigest)" -count=1'
```

Expected: compilation/test failure because runtime target and the two declarations do not exist.

- [ ] **Step 3: Add the manifest-owned target**

Implement one enum with only these effective values:

```go
type SecretMaterializationTarget string

const (
    SecretMaterializationPlugin          SecretMaterializationTarget = "plugin"
    SecretMaterializationCompilerDiscard SecretMaterializationTarget = "compiler_discard"
)

type SecretDeclaration struct {
    Factory string                       `yaml:"factory"`
    Source  SecretDeclarationSource      `yaml:"source"`
    Field   string                       `yaml:"field"`
    Strict  bool                         `yaml:"strict"`
    Target  SecretMaterializationTarget  `yaml:"target,omitempty"`
}

func (d SecretDeclaration) EffectiveTarget() SecretMaterializationTarget
```

An omitted target means `plugin`. Reject unknown targets and reject `compiler_discard` for any source other than `plugin_config`. Include the effective target in deterministic sort/digest encoding and bump the digest domain string to `apisix-go/secret-declarations/v2`.

Mark only the eight phantom field declarations owned by the six factories as `target: "compiler_discard"`. Add optional plugin-target declarations `dingtalk-auth.secret_fallbacks` and `saml-auth.secret_fallbacks`. Do not change strictness, consumer declarations, schemas, or at-rest encryption enumeration.

- [ ] **Step 4: Add target-aware traversal without a second table**

Add a catalog method whose target parameter is validated and whose traversal retains the same deterministic wildcard/container behavior as `TransformDeclaredFields`:

```go
func (c *SecretDeclarationCatalog) TransformDeclaredFieldsForTarget(
    factory string,
    source SecretDeclarationSource,
    target SecretMaterializationTarget,
    document any,
    transform SecretFieldTransform,
) error
```

Existing `TransformDeclaredFields` still visits all declarations and preserves data-encryption behavior. Target-aware traversal filters only by the manifest value.

- [ ] **Step 5: Run GREEN**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/capability ./pkg/data_encryption -run "(Declaration|RuntimeTarget|CompilerDiscard|Fallback|Parity|CatalogDigest|SecretField)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go run ./cmd/capability-gen -check'
git diff --check
```

Expected: PASS; data-encryption still sees all plugin-config declarations and its compatibility behavior is unchanged.

### Task 2: S3-0B — Materialize and discard phantom raw fields in compiler

**Files:**
- Create: `pkg/compiler/discarded_secret_preparer.go`
- Create: `pkg/compiler/discarded_secret_preparer_test.go`
- Modify: `pkg/compiler/occurrence.go`
- Modify: `pkg/compiler/factory.go`
- Modify: `pkg/compiler/factory_test.go`

**Interfaces:**
- Consumes: S3-0A target-aware catalog, exact `PreparationAttempt`, raw final candidate snapshots, `PreparationAttempt.MaterializeSecret`.
- Produces: package-private `prepareCompilerDiscardSecrets(context.Context, PreparationAttempt, *capability.SecretDeclarationCatalog) error`; target-aware support validation.

- [ ] **Step 1: Write failing exact-attempt tests**

Add `TestPrepareCompilerDiscardSecretsUsesExactRawFinalOccurrences`, `TestPrepareCompilerDiscardSecretsRejectsNonStringAndRedactsFailure`, `TestPrepareCompilerDiscardSecretsIsAtomicAcrossAttempts`, and `TestValidateScopedSecretSupportDoesNotAcceptPhantomNoOps`. Cover all six factories, missing fields, two values in one JWT/JWE config, candidate and recovery, cross-domain/foreign occurrence rejection, N/N+1, and resolver failure text containing a raw environment reference and plaintext.
The support test also proves an unowned real declaration fails before candidate
or recovery registration and leaves both registration and resolver call counts
at zero.

- [ ] **Step 2: Run RED**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^(TestPrepareCompilerDiscardSecrets|TestValidateScopedSecretSupportDoesNotAcceptPhantomNoOps)" -count=1'
```

Expected: compilation failure because the compiler-owned preparer does not exist and current support validation requires a plugin interface for the phantom factories.

- [ ] **Step 3: Implement raw-before-decode preparation**

Walk only `attempt.Occurrences(SecretPluginConfig)`. For each occurrence, clone/read its exact candidate and normalized resource, locate the exact factory config, and call `TransformDeclaredFieldsForTarget(..., SecretMaterializationCompilerDiscard, ...)`. Each present leaf must be a string; materialize it with the occurrence and canonical declaration field, call `Value.Use(func(string) error { return nil })`, retain no descriptor/plaintext/value, and leave the candidate clone unchanged. Missing fields make no call. Any type, authority, normalize, or resolver error maps to one fixed redacted compiler error.

Do not decode a plugin, instantiate a factory, mutate publication bytes, add the phantom fields to plugin structs, or retain a compiler cache.

- [ ] **Step 4: Put the pure support gate before registration**

Change `validateScopedSecretSupport` so a factory with at least one
`plugin`-target declaration must implement `base.ScopedSecretMaterializer`; a
factory whose declarations are all compiler-discard is owned by the compiler
preparer and must not need or gain a no-op plugin method. Candidate and recovery
flows run this pure validation immediately after exact occurrence enumeration
and before `RegisterCandidate` or `RegisterRecovery`; a missing owner produces
zero registration/resolver calls. After successful registration and
`PreparationAttempt` construction, `prepareRegisteredAttempt` runs
compiler-discard materialization, then metadata, consumers, and real plugin
preparation. A discard failure uses existing reverse cleanup and closes the
registration once.

- [ ] **Step 5: Run GREEN and lifecycle gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^(TestPrepareCompilerDiscardSecrets|TestValidateScopedSecretSupport|TestAttemptFactory)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler -run "^(TestPrepareCompilerDiscardSecrets|TestAttemptFactory)" -count=1'
golangci-lint run ./pkg/capability/... ./pkg/compiler/...
git diff --check
```

**Integration checkpoint:** `feat(compiler): own phantom secret compatibility`.

---

### Task 3: S3-AI1 — Migrate `ai_proxy`

**Files:**
- Modify: `pkg/plugin/ai_proxy/plugin.go`
- Modify: `pkg/plugin/ai_proxy/plugin_test.go`
- Modify: `pkg/plugin/ai_proxy/benchmark_test.go`
- Modify: `pkg/plugin/ai_proxy/provider_parity_test.go`
- Modify: `pkg/plugin/ai_proxy/request_phase_test.go`

**Interfaces:** five manifest fields; produces package-local private header/query values plus optional GCP/AWS values. It does not modify `pkg/plugin/ai_auth/**` or `pkg/plugin/ai_common/**`.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsAIProxyAuth` with two header values, two query values, GCP JSON, AWS secret and session token. Assert map leaves use `auth.header`/`auth.query`, not names; optional nil providers make zero calls; the public maps/config contain descriptors only.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_proxy -run "^TestMaterializeScopedSecretsOwnsAIProxyAuth$" -count=1'`.
- [ ] Implement separate scoped and legacy materializers. Stage `map[string]secret.Value` for headers/query and private values for GCP/AWS. Request construction opens only the needed values inside nested `Use` callbacks and creates a request-local `ai_auth.AWSConfig`/`GCPConfig`; no plaintext returns to `Config` or a shared transport. Preserve provider selection, sensitive-query registration, SigV4, token TTL behavior, and error/status mapping.
- [ ] Add Nth-map-value rollback, redaction, N/N+1 concurrent request, and idempotent `Stop` tests. `Stop` closes idle connections, drops the plugin-owned GCP token source/cache reference after in-flight requests retire, destroys legacy owners, then drops scoped maps/values.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_proxy -run "(MaterializeScopedSecrets|Auth|Bedrock|GCP|SensitiveQuery|Stop)" -count=1 && go test -race ./pkg/plugin/ai_proxy -run "(MaterializeScopedSecrets|Handler|GCP|Stop)" -count=1'`.

### Task 4: S3-AI2 — Migrate `ai_proxy_multi`

**Files:**
- Modify: `pkg/plugin/ai_proxy_multi/plugin.go`
- Modify: `pkg/plugin/ai_proxy_multi/health.go`
- Modify: `pkg/plugin/ai_proxy_multi/plugin_test.go`
- Modify: `pkg/plugin/ai_proxy_multi/provider_parity_test.go`
- Modify: `pkg/plugin/ai_proxy_multi/benchmark_test.go`

**Interfaces:** five `instances.*` container paths; produces one immutable private auth holder aligned with every original instance index and shared by request/health paths.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsEveryAIProxyMultiInstanceAuth` using three instances, repeated names, disabled weight, two map leaves, GCP, AWS, and an instance without auth. Assert canonical wildcard fields, stable index alignment, and no materialization based on weight/health eligibility.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_proxy_multi -run "^TestMaterializeScopedSecretsOwnsEveryAIProxyMultiInstanceAuth$" -count=1'`.
- [ ] Implement staged scoped/legacy materializers and a private `[]instanceAuthValues` aligned before `PostInit` sorting/index maps. Both `requestInstance` and `runHealthProbe`/`healthURL` read header/query values through `Use`; selected GCP/AWS paths build request-local configs. Health/client cache keys contain only structural config and descriptor digests, never plaintext.
- [ ] Extend the existing `Stop` in `health.go`: stop/join health work and close health/main clients first, then destroy/drop auth owners. Prove a blocked health probe cannot access values after retirement, failed materialization starts no health loop, and N/N+1 health/request traffic stays isolated.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_proxy_multi -run "(MaterializeScopedSecrets|Health|Bedrock|GCP|Stop)" -count=1 && go test -race ./pkg/plugin/ai_proxy_multi -run "(MaterializeScopedSecrets|Health|Request|Stop)" -count=1'`.

### Task 5: S3-AI3 — Migrate `ai_request_rewrite`

**Files:**
- Modify: `pkg/plugin/ai_request_rewrite/plugin.go`
- Modify: `pkg/plugin/ai_request_rewrite/plugin_test.go`

**Interfaces:** five non-array AI auth paths; produces private request-time header/query/GCP/AWS values without a shared-package edit.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsAIRewriteAuth` covering multi-entry maps, absent providers, `$ENV`, `$secret`, contextual ciphertext, exact scopes, and descriptor-only config.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_request_rewrite -run "^TestMaterializeScopedSecretsOwnsAIRewriteAuth$" -count=1'`.
- [ ] Implement atomic scoped/legacy materializers and request-local auth reconstruction inside `Value.Use`. Preserve prompt/body replacement, provider endpoint validation, SigV4/GCP behavior, warnings, and error status text. Add `Stop` to close the client and drop token/auth state.
- [ ] Add third-value failure, redaction, N/N+1 overlapping rewrite, and Stop-after-request tests.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_request_rewrite -run "(MaterializeScopedSecrets|Rewrite|AWS|GCP|Stop)" -count=1 && go test -race ./pkg/plugin/ai_request_rewrite -run "(MaterializeScopedSecrets|Handler|Stop)" -count=1'`.

---

### Task 6: S3-FN1 — Migrate `azure_functions` before M1-Azure

**Files:**
- Modify: `pkg/plugin/azure_functions/plugin.go`
- Modify: `pkg/plugin/azure_functions/plugin_test.go`

**Interfaces:** plugin-config `authorization.apikey`; preserves the existing client-header > route > metadata precedence and produces the route-secret state that M1-Azure must retain while adding immutable `master_apikey` metadata.

- [ ] Branch directly from the merged S3-0 checkpoint. Add failing `TestMaterializeScopedSecretsOwnsAzureRouteAPIKey` proving a route key uses plugin-config scope, absent route auth makes no plugin-config call, the existing metadata fallback remains untouched for M1, and neither route nor metadata values overwrite a client-supplied header.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/azure_functions -run "^TestMaterializeScopedSecretsOwnsAzureRouteAPIKey$" -count=1'`.
- [ ] Implement separate scoped/legacy route-key materializers. Keep the route value private and use it inside `processRequest`; public config holds the descriptor. Override `Stop` only to call embedded `function_upstream.Plugin.Stop`, then destroy/drop route values. Leave `loadMetadata` and the existing metadata field untouched so M1 can layer its owner afterward.
- [ ] Add route-materialization failure before `PostInit`, current metadata fallback, N/N+1 route-key rotation, and idempotent Stop tests. Record the private route-value fields and Stop ordering in the M1 handoff.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/azure_functions -run "(MaterializeScopedSecrets|Metadata|Authorization|Precedence|Stop)" -count=1'`.

### Task 7: S3-FN2 — Migrate `openfunction`

**Files:**
- Modify: `pkg/plugin/openfunction/plugin.go`
- Modify: `pkg/plugin/openfunction/plugin_test.go`
- Modify: `pkg/plugin/openfunction/benchmark_test.go`

**Interfaces:** optional `authorization.service_token`; produces a private value used only to build the per-request Basic header.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsOpenFunctionServiceToken` covering absent authorization, empty token, literal, `$ENV`, `$secret`, exact field, descriptor-only config, and redacted failure.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/openfunction -run "^TestMaterializeScopedSecretsOwnsOpenFunctionServiceToken$" -count=1'`.
- [ ] Implement atomic scoped/legacy methods. Build `Authorization: Basic ...` inside `Value.Use` in `processRequest`; do not precompute/store the plaintext or encoded credential in the shared `function_upstream` client. Override `Stop` to release the embedded client before values.
- [ ] Update direct tests/benchmark to prepare literal credentials explicitly. Add N/N+1 header isolation and Stop tests.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/openfunction -run "(MaterializeScopedSecrets|Authorization|Handler|Stop)" -count=1'`.

### Task 8: S3-FN3 — Migrate `openwhisk`

**Files:**
- Modify: `pkg/plugin/openwhisk/plugin.go`
- Modify: `pkg/plugin/openwhisk/plugin_test.go`
- Modify: `pkg/plugin/openwhisk/benchmark_test.go`

**Interfaces:** required `service_token`; produces a private value used during action-request construction.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsOpenWhiskServiceToken` covering resolved empty rejection, literal/reference/ciphertext, exact scope, descriptor-only config, and no client construction on failure.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/openwhisk -run "^TestMaterializeScopedSecretsOwnsOpenWhiskServiceToken$" -count=1'`.
- [ ] Implement scoped/legacy materializers and reject an empty resolved token before installation. `buildActionRequest` creates the Basic header inside `Value.Use`; `PostInit` only builds the neutral HTTP transport. Add idempotent `Stop` that closes idle connections, destroys legacy owner, and drops the scoped value.
- [ ] Update direct tests/benchmark to prepare first. Add N/N+1 action header isolation, failed third-party response, concurrent requests, and post-retirement tests.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/openwhisk -run "(MaterializeScopedSecrets|ActionRequest|Authorization|Stop)" -count=1 && go test -race ./pkg/plugin/openwhisk -run "(MaterializeScopedSecrets|Handler|Stop)" -count=1'`.

### Task 9: S3-FN4 — Migrate `aws_lambda`

**Files:**
- Modify: `pkg/plugin/aws_lambda/plugin.go`
- Modify: `pkg/plugin/aws_lambda/plugin_test.go`

**Interfaces:** API key and IAM access/secret keys; preserves API-key precedence and exact SigV4 canonicalization.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsAWSLambdaCredentials` with API-only, IAM-only, all-fields, missing IAM member, `$ENV`, `$secret`, and Nth-field failure. Assert exact three field names and descriptor-only public structs.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/aws_lambda -run "^TestMaterializeScopedSecretsOwnsAWSLambdaCredentials$" -count=1'`.
- [ ] Implement atomic scoped/legacy materializers. Keep private API/IAM values; `processRequest` opens API key or both IAM values, constructs a request-local `ai_auth.AWSConfig`, and invokes the existing signer options. Do not modify `ai_auth` or put credentials into `function_upstream.Config`/client identity. Override `Stop` to call the embedded Stop first.
- [ ] Add resolved-required validation, client-header replacement, N/N+1 signature credential isolation, redacted signer/materializer failure, and Stop tests.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/aws_lambda -run "(MaterializeScopedSecrets|APIKey|IAM|Signature|Stop)" -count=1'`.

---

### Task 10: S3-AU1 — Migrate `authz_casdoor`

**Files:**
- Modify: `pkg/plugin/authz_casdoor/plugin.go`
- Modify: `pkg/plugin/authz_casdoor/plugin_test.go`

**Interfaces:** required `client_secret` and optional container `client_secret_fallbacks`; produces private AEAD/OAuth values.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsCasdoorSessionSecrets` with current plus two fallback values, rotated references, exact container scope, resolved length failures, descriptor-only config, and Nth-fallback rollback.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/authz_casdoor -run "^TestMaterializeScopedSecretsOwnsCasdoorSessionSecrets$" -count=1'`.
- [ ] Implement scoped/legacy methods. Validate the resolved current/fallback minimum length inside `Value.Use` before installation. Refactor OAuth form, `OpenOAuthSession`, and `SealOAuthSession` to consume private values only inside callbacks; fingerprint/cookie name remain secret-neutral.
- [ ] Add `Stop` to close idle connections, then destroy/drop current and fallback owners. Prove an N cookie remains readable only by N until retirement, N+1 accepts its configured fallback but not unrelated N values, and failures expose no session data.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/authz_casdoor -run "(MaterializeScopedSecrets|Session|Callback|Fallback|Stop)" -count=1'`.

### Task 11: S3-AU2 — Migrate `cas_auth`

**Files:**
- Modify: `pkg/plugin/cas_auth/plugin.go`
- Modify: `pkg/plugin/cas_auth/plugin_test.go`
- Modify: `pkg/plugin/cas_auth/benchmark_test.go`

**Interfaces:** required `cookie.secret`; process-global CAS session cache remains outside this secret migration and is keyed only by the existing secret-neutral route fingerprint.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsCASCookieSecret` covering literal/reference/ciphertext, resolved minimum length, exact scope, descriptor-only config, and no session/client side effects on failure.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/cas_auth -run "^TestMaterializeScopedSecretsOwnsCASCookieSecret$" -count=1'`.
- [ ] Implement scoped/legacy materializers. Wrap initiation-cookie sign/verify operations in private-value `Use`; do not include the secret/digest in `processSessions` keys or `sessionOptions`. `PostInit` retains network/CIDR validation and client construction only.
- [ ] Add Stop to close idle connections and drop owners without clearing another generation's process sessions. Update benchmark setup. Add N/N+1 cookie rejection/isolation and concurrent session/Stop tests.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/cas_auth -run "(MaterializeScopedSecrets|Cookie|Session|Logout|Stop)" -count=1'`.

### Task 12: S3-AU3 — Migrate `dingtalk_auth`

**Files:**
- Modify: `pkg/plugin/dingtalk_auth/plugin.go`
- Modify: `pkg/plugin/dingtalk_auth/plugin_test.go`

**Interfaces:** `app_secret`, `secret`, and S3-0's new container `secret_fallbacks`; produces private OAuth/session values and a descriptor-digest token-cache identity.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsDingTalkOAuthAndSessionSecrets` with two fallbacks, exact new manifest field, resolved 8-32 session-secret validation, app-secret token body, descriptor-only config, and third-value rollback.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/dingtalk_auth -run "^TestMaterializeScopedSecretsOwnsDingTalkOAuthAndSessionSecrets$" -count=1'`.
- [ ] Implement scoped/legacy materializers. OAuth-state/session sign/open and token request bodies use private values inside `Use`. Replace the plaintext `AppSecret` component of `tokenCache` keys with `secret.Value.Digest()`/descriptor digest; access tokens remain plugin-instance-owned.
- [ ] Implement idempotent Stop: prevent new work, close idle connections, clear the locked token cache and OAuth replay cache reference, then destroy/drop values. Prove N/N+1 caches and cookies are isolated and concurrent refresh/Stop passes race.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/dingtalk_auth -run "(MaterializeScopedSecrets|OAuth|Session|Fallback|TokenCache|Stop)" -count=1 && go test -race ./pkg/plugin/dingtalk_auth -run "(MaterializeScopedSecrets|TokenCache|Handler|Stop)" -count=1'`.

### Task 13: S3-AU4 — Migrate `feishu_auth`

**Files:**
- Modify: `pkg/plugin/feishu_auth/plugin.go`
- Modify: `pkg/plugin/feishu_auth/plugin_test.go`

**Interfaces:** `app_secret`, `secret`, `secret_fallbacks`; produces private OAuth/session values and a neutral HTTP client.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsFeishuOAuthAndSessionSecrets` covering two fallbacks, exact container scope, resolved 8-32 session-secret validation, token body, descriptor-only config, and atomic failure.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/feishu_auth -run "^TestMaterializeScopedSecretsOwnsFeishuOAuthAndSessionSecrets$" -count=1'`.
- [ ] Implement separate scoped/legacy methods. Refactor OAuth state/session signing and token body construction to open only private values. Keep redirect/userinfo behavior and `ai_common` response limits unchanged; do not modify the shared package.
- [ ] Add Stop to close the neutral client and drop owners. Prove N/N+1 cookie/fallback isolation, blocked request retirement, redacted failures, and concurrent handler reads.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/feishu_auth -run "(MaterializeScopedSecrets|OAuth|Session|Fallback|Handler|Stop)" -count=1'`.

### Task 14: S3-AU5 — Migrate `saml_auth`

**Files:**
- Modify: `pkg/plugin/saml_auth/plugin.go`
- Modify: `pkg/plugin/saml_auth/plugin_test.go`
- Modify: `pkg/plugin/saml_auth/benchmark_test.go`
- Modify: `pkg/plugin/saml_auth/signature_security_test.go`

**Interfaces:** `sp_private_key`, `secret`, and S3-0's new `secret_fallbacks`; produces a parsed attempt-owned `crypto.Signer` plus private cookie values.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsSAMLPrivateAndSessionKeys` with a valid key, current/two fallback cookie secrets, invalid resolved PEM, exact fallback container scope, descriptor-only config, and atomic rollback.
- [ ] Run RED: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/saml_auth -run "^TestMaterializeScopedSecretsOwnsSAMLPrivateAndSessionKeys$" -count=1'`.
- [ ] Implement scoped/legacy materializers. Validate resolved cookie lengths. Parse `SPCert` plus resolved private key inside `Value.Use` and install only the derived `crypto.Signer`/certificate after every value succeeds. `PostInit` consumes the staged key pair and builds IDP metadata; it never reads the descriptor as PEM. Every request/logout/session cookie opens current/fallback values inside callbacks.
- [ ] Implement idempotent Stop to drop the derived signer/metadata after handlers retire, destroy legacy owners, and drop scoped values. Update direct setup. Prove N/N+1 signatures/cookies cannot cross, fallback rotation works only when configured, and concurrent service-provider reads/Stop pass race.
- [ ] Run GREEN/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/saml_auth -run "(MaterializeScopedSecrets|KeyPair|ServiceProvider|Session|Fallback|Signature|Stop)" -count=1 && go test -race ./pkg/plugin/saml_auth -run "(MaterializeScopedSecrets|ServiceProvider|Session|Stop)" -count=1'`.

---

## Leaf review checkpoints

Each worker stops after the named focused/race gate and hands the integration owner: its complete diff, RED command/output, GREEN command/output, the N/N+1 and Stop test names, and any narrowed or pre-existing failure. The worker does not create the checkpoint commit. After rejecting out-of-scope files and rerunning the package gate, the integration owner creates exactly one reviewable checkpoint per row.

| Lane | Exclusive package ownership | Integration-owner checkpoint |
| --- | --- | --- |
| S3-AI1 | `pkg/plugin/ai_proxy/**` | `feat(ai-proxy): scope plugin secrets` |
| S3-AI2 | `pkg/plugin/ai_proxy_multi/**` | `feat(ai-proxy-multi): scope instance secrets` |
| S3-AI3 | `pkg/plugin/ai_request_rewrite/**` | `feat(ai-request-rewrite): scope provider secrets` |
| S3-FN1 | `pkg/plugin/azure_functions/**` | `feat(azure-functions): scope route api key` |
| S3-FN2 | `pkg/plugin/openfunction/**` | `feat(openfunction): scope service token` |
| S3-FN3 | `pkg/plugin/openwhisk/**` | `feat(openwhisk): scope service token` |
| S3-FN4 | `pkg/plugin/aws_lambda/**` | `feat(aws-lambda): scope invocation credentials` |
| S3-AU1 | `pkg/plugin/authz_casdoor/**` | `feat(authz-casdoor): scope oauth session secrets` |
| S3-AU2 | `pkg/plugin/cas_auth/**` | `feat(cas-auth): scope cookie secret` |
| S3-AU3 | `pkg/plugin/dingtalk_auth/**` | `feat(dingtalk-auth): scope oauth session secrets` |
| S3-AU4 | `pkg/plugin/feishu_auth/**` | `feat(feishu-auth): scope oauth session secrets` |
| S3-AU5 | `pkg/plugin/saml_auth/**` | `feat(saml-auth): scope signing and session secrets` |

### Task 15: Merge S3 leaves and prove all 41 factories have one owner

**Files:**
- Modify: `pkg/compiler/occurrence_test.go`
- Modify only for an AST/source guard: `pkg/plugin/scoped_preparation_test.go`
- Review-only: all S1, S2, S3 and A1 package diffs

**Interfaces:** consumes target-aware declarations plus registered plugin factories; produces the final Task 6 scoped-support ledger.

- [ ] **Step 1: Integrate in fixed order**

The integration owner reviews and commits S3-0 first, then AI1/AI2/AI3, FN1/FN2/FN3/FN4, and AU1-AU5. Before each commit, inspect package ownership, rerun that package's focused tests, and reject edits outside the assigned files. Publish the accepted FN1 commit as M1-Azure's required branch point. Do not merge a worker branch or commit directly to `master`.

- [ ] **Step 2: Add the all-41 executable ledger**

Load the real manifest and enumerate distinct factories with `plugin_config` declarations. Assert the count is exactly 41. For each factory:

1. if any declaration has effective target `plugin`, `plugin.SupportsScopedSecretMaterialization(factory)` must return true;
2. if all declarations target compiler-discard, every declaration must be consumed by `prepareCompilerDiscardSecrets` and plugin support must not be required;
3. no factory may have zero effective owners, an unknown target, both a private compiler table and manifest target, or a legacy-only materializer.

Add AST guards that the twelve S3 scoped methods contain no Store/DataEncryption/global resolver call, the six A1 auth packages contain no S3 no-op scoped method, and compiler discard code never imports a plugin leaf.

- [ ] **Step 3: Run the combined S3 gate**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/capability ./pkg/data_encryption ./pkg/compiler ./pkg/plugin \
  ./pkg/plugin/ai_proxy ./pkg/plugin/ai_proxy_multi ./pkg/plugin/ai_request_rewrite \
  ./pkg/plugin/azure_functions ./pkg/plugin/openfunction ./pkg/plugin/openwhisk ./pkg/plugin/aws_lambda \
  ./pkg/plugin/authz_casdoor ./pkg/plugin/cas_auth ./pkg/plugin/dingtalk_auth \
  ./pkg/plugin/feishu_auth ./pkg/plugin/saml_auth -count=1'
```

- [ ] **Step 4: Run focused race groups serially**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race \
  ./pkg/compiler ./pkg/plugin/ai_proxy ./pkg/plugin/ai_proxy_multi ./pkg/plugin/ai_request_rewrite \
  -run "(CompilerDiscard|MaterializeScopedSecrets|Handler|Health|Stop)" -count=1'

bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race \
  ./pkg/plugin/azure_functions ./pkg/plugin/openfunction ./pkg/plugin/openwhisk ./pkg/plugin/aws_lambda \
  -run "(MaterializeScopedSecrets|Authorization|Signature|Handler|Stop)" -count=1'

bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race \
  ./pkg/plugin/authz_casdoor ./pkg/plugin/cas_auth ./pkg/plugin/dingtalk_auth \
  ./pkg/plugin/feishu_auth ./pkg/plugin/saml_auth \
  -run "(MaterializeScopedSecrets|Session|OAuth|Fallback|Signature|Stop)" -count=1'
```

- [ ] **Step 5: Run scoped completion checks**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go run ./cmd/capability-gen -check'
golangci-lint run ./pkg/capability/... ./pkg/compiler/... ./pkg/plugin/...
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
git diff --check
```

Report any pre-existing or unrelated package failure exactly. Do not describe a narrowed or skipped gate as passing.

## X1, CP5 and C6.6 handoff

- X1 starts only after A1 plus every S1/S2/S3 real leaf is merged. `workflow` and `multi-auth` use `ScopedSecretAccess.Child` for their effective child factories and must not reintroduce Store or bypass compiler-discard ownership.
- CP5 defines a compiler-private effective-binding materializer. Task 7/8 supplies complete winners/scopes/provenance/context; the primitive then calls `PreparationAttempt.PrepareScopedPluginSecrets` and attaches every acquired owner to reverse cleanup. Raw final occurrences provide secret authority, not runtime binding inventory. Compiler-discard runs before effective materialization and owns no closer.
- C6.6 records every still-live legacy `MaterializeSecrets` caller and deletes only zero-production-caller leaves. The joint Task 9 cutover deletes the remaining legacy methods, Store owners/imports, and raw DataEncryption fallback after Task 7/8 replacement compilation is ready; scoped methods and compiler-discard remain.
- Task 9 later removes the temporary in-process Store broker. S3 must not introduce a second activation path or move Task 7/8 snapshot construction into Task 6.

## Acceptance ledger

| Boundary | Required evidence |
| --- | --- |
| Source of truth | runtime target and both fallback corrections live only in manifest/catalog and digest |
| Phantom fields | six factories resolve exact raw final fields in compiler and retain nothing; no leaf no-op |
| Real fields | twelve leaf packages materialize every present declared leaf through scoped access |
| Atomicity | Nth failure installs no descriptors, values, clients, keys, caches, or tasks |
| Redaction | configs expose descriptors only; errors/logs expose no raw reference/plaintext/provider secret |
| Lifecycle | N/N+1 isolated; Stop is idempotent and retirement order is tested under race |
| Cross-lane | A1 owns consumers, M1 owns Azure metadata, S1/S2 own their packages, X1 owns composites |
| Completeness | executable manifest inventory proves exactly 41 plugin-config factories have one effective owner |
| Delivery | integration owner reviews and commits; no leaf commit/push/PR/master mutation |

## Explicit non-goals

- No fake route fields or no-op scoped materializers in `basic_auth`, `key_auth`, `jwt_auth`, `hmac_auth`, `ldap_auth`, or `jwe_decrypt`.
- No change to consumer credentials, authentication-state publication, anonymous lookup, or Store fallback; A1/C6.6 own those.
- No Azure metadata migration; S3-FN1 lands route-secret ownership first, then M1-Azure branches from that accepted checkpoint and layers metadata ownership without reverting it.
- No shared AI auth abstraction, shared function-upstream credential state, generic secret-holder framework, new dependency, Task 7/8 snapshot work, Task 9 supervisor work, or Task 11 goroutine redesign.
