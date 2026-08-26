# Immutable Task 6 Lane S1 Store Materializers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Migrate the eight Lane S1 plugin factories from process-global Store secret resolution to attempt-scoped `base.ScopedSecretAccess`, while keeping a separately implemented legacy materializer until the joint Task 9 production cutover.

**Architecture:** Each plugin gets an atomic, package-local secret preparation unit: the scoped method consumes only `base.ScopedSecretAccess`, stages every `secret.Value`, installs redacted descriptors only after all fields succeed, and never calls Store or the legacy method. Runtime code reads staged values through bounded callbacks or builds private clients/derived keys inside `secret.Value.Use`; `PostInit` only validates/defaults/builds runtime state and never resolves a secret. The eight package paths are file-exclusive and can run in parallel from the merged descriptor checkpoint.

**Tech Stack:** Go 1.26, `pkg/plugin/base.ScopedSecretAccess`, `pkg/secret.Value`, capability-manifest declarations, generation publication scopes, existing Store compatibility materializers, focused race tests.

**Spec:** [`docs/superpowers/plans/2026-08-24-immutable-task6-c6.4-plugin-runtime.md`](2026-08-24-immutable-task6-c6.4-plugin-runtime.md), Task 4; [`docs/superpowers/plans/2026-08-24-immutable-task6-cp3-compiler-skeleton.md`](2026-08-24-immutable-task6-cp3-compiler-skeleton.md), Task 7.

**Architecture prerequisite:** `40c04a26` (`feat(compiler): register refined generation attempts`) freezes the CP3.4 occurrence, registration, and scoped plugin-preparation boundary.

**Leaf implementation baseline:** `e5b6a73e` (`feat(secret): add redacted value descriptors`). Every Lane S1 leaf worktree must branch from this merged integration commit or a later integration descendant that contains it.

## Global Constraints

- Run every Go command from the active worktree after `source .envrc` with `GOFLAGS=-mod=readonly`.
- Leaf workers own only their assigned `pkg/plugin/<name>/**` directory. They do not edit `pkg/secret`, `pkg/plugin/base`, `pkg/plugin`, `pkg/compiler`, the manifest, Store, route/server code, or another leaf package.
- Leaf workers return owned-path diffs and verification evidence only; they do
  not commit. Every `git commit` command below is executed by the integration
  owner only after inspecting and accepting that worktree.
- The scoped method must not call `MaterializeSecrets`, `store.MaterializeSecret`, `BasePlugin.DataEncryption`, a package-global resolver, or a raw keyring. Legacy and scoped methods may share only pure field-selection, descriptor-installation, and runtime-value helpers.
- Raw publication schema validation and final attempt registration remain compiler-owned prerequisites. Leaf code must not construct or retain a capability, scope, generation number, attempt ID, Store pointer, or resolver.
- Call `ScopedSecretAccess.Materialize(ctx, exactManifestField, raw)` for every present declared field, including literals and strict ciphertext; skip an empty optional field. Container declarations use the manifest container field for each leaf, never `.*` or an indexed path.
- Stage all values and derived objects in locals. On any failure, install nothing and return a constant redacted error. Never include the raw input, environment name, Vault path, ciphertext, plaintext, or provider response in an error.
- Public config may retain only the `String()` of the descriptor returned by `secret.Value.Descriptor(capability.SecretPluginConfig)` (or `secret.NewDescriptor` for the legacy owner). Runtime plaintext stays in `secret.Value`, a private derived object, or a private client built inside `Value.Use`.
- `secret.Value` contains Go strings and cannot be wiped. `Stop` drops scoped values/derived objects and destroys only legacy `store.ResolvedSecret` owners; attempt registration close remains the scoped resolver lifetime owner.
- Keep the legacy `MaterializeSecrets() error` entrypoint buildable for the current Builder. C6.6 classifies its live caller; Task 9 deletes it and the final Store imports after Task 7/8 replacement paths exist.
- Do not change plugin schemas, manifest fields/strictness, APISIX-visible request behavior, metadata behavior, priorities, phases, or platform support in this lane.
- Use `apply_patch` for edits, format only touched Go files, inspect every diff, and report any baseline or narrowed verification failure exactly.

---

## Baseline Inventory and Decision Record

The eight package tests passed during the CP3.4 inventory at `40c04a26`; leaf execution repeats the same baseline gate from `e5b6a73e` before edits:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/ai_aliyun_content_moderation \
  ./pkg/plugin/ai_aws_content_moderation \
  ./pkg/plugin/ai_rag \
  ./pkg/plugin/authz_keycloak \
  ./pkg/plugin/clickhouse_logger \
  ./pkg/plugin/limit_count \
  ./pkg/plugin/oas_validator \
  ./pkg/plugin/openid_connect -count=1
```

| Factory | Manifest-owned fields at baseline | Current owner and lifecycle coupling | Runtime use that must remain private |
| --- | --- | --- | --- |
| `ai-aliyun-content-moderation` | `access_key_id`, `access_key_secret` | `plugin.go` retains two `*store.ResolvedSecret`; `PostInit` self-calls `MaterializeSecrets` | HMAC request parameters and signing key |
| `ai-aws-content-moderation` | `comprehend.access_key_id`, `comprehend.secret_access_key`, optional `comprehend.session_token` | `plugin.go` retains three Store owners; explicit pre-`PostInit` materialization already exists in the main test helper | AWS SigV4 credentials |
| `ai-rag` | `embeddings_provider.azure_openai.api_key`, `vector_search_provider.azure_ai_search.api_key` | `plugin.go` retains two Store owners; `PostInit` self-calls `MaterializeSecrets` | `api-key` headers for both provider calls |
| `authz-keycloak` | optional `client_secret` | `plugin.go` retains one Store owner | token/refresh forms and the shared-cache identity digest |
| `clickhouse-logger` | strict `password`, optional `user` | optional Store references are handled by `MaterializeSecrets`, but strict password decryption and resolver presence are hidden in `PostInit` | ClickHouse request headers; batch/client ownership stays unchanged |
| `limit-count` | `key`, root/nested Redis host aliases, root/nested Redis cluster-node containers | `plugin.go` normalizes aliases before Store materialization, then retains key/host/node Store owners | request key, Redis options, digest-keyed backend identity |
| `oas-validator` | optional `spec`, container `spec_url_request_headers` | `plugin.go` retains inline/header Store owners; `PostInit` self-calls `MaterializeSecrets` | inline compilation and remote/external-reference request headers across refreshes |
| `openid-connect` | `client_secret`, `client_rsa_private_key`, `public_key`, `session.secret`, `session.redis.password` | existing Store materializer handles only `public_key`; other four declared fields remain public-config plaintext | OAuth forms/assertions/provider, parsed RSA/public keys, session key, Redis client |

Additional baseline facts that constrain the work:

- `base.MaterializeScopedPluginSecrets` never falls back to legacy materialization and rescans config after the scoped owner returns.
- `compiler.PreparationAttempt.PrepareScopedPluginSecrets` binds a `plugin.FactoryInstance` to the exact factory occurrence and registered generation attempt.
- `limit-count` declarations intentionally distinguish root and nested aliases. Normalization must not erase the declaration that authorized the raw value.
- `oas-validator` and `clickhouse-logger` still use Store-backed metadata. That is Lane M1 scope and must remain untouched here.
- The current Builder calls `plugin.MaterializePluginSecrets` before `PostInit`; the new compiler will call the scoped method. These are two preparation entrypoints during the additive window, not two runtime activation paths.

## File Ownership Map

Each leaf worker may create `secrets.go` and `scoped_secrets_test.go` inside its package and modify only the listed existing files.

| Owner | Production files | Test/setup files |
| --- | --- | --- |
| S1-Aliyun | `pkg/plugin/ai_aliyun_content_moderation/plugin.go`; create `secrets.go` | `plugin_test.go`, `benchmark_test.go`, `response_phase_test.go`; create `scoped_secrets_test.go` |
| S1-AWS | `pkg/plugin/ai_aws_content_moderation/plugin.go`; create `secrets.go` | `plugin_test.go`; create `scoped_secrets_test.go` |
| S1-RAG | `pkg/plugin/ai_rag/plugin.go`; create `secrets.go` | `plugin_test.go`; create `scoped_secrets_test.go` |
| S1-Keycloak | `pkg/plugin/authz_keycloak/plugin.go`; create `secrets.go` | `plugin_test.go`; create `scoped_secrets_test.go` |
| S1-ClickHouse | `pkg/plugin/clickhouse_logger/plugin.go`; create `secrets.go` | `plugin_test.go`; create `scoped_secrets_test.go` |
| S1-Limit | `pkg/plugin/limit_count/plugin.go`, `redis.go`; create `secrets.go` | `plugin_test.go`; create `scoped_secrets_test.go`; `manifest_test.go` only if an existing assertion must be extended |
| S1-OAS | `pkg/plugin/oas_validator/plugin.go`; create `secrets.go` | `plugin_test.go`, `benchmark_test.go`; create `scoped_secrets_test.go` |
| S1-OIDC | `pkg/plugin/openid_connect/plugin.go`, `flow.go`, `provider.go`, `session.go`; create `secrets.go` | `plugin_test.go`, `session_test.go`, `flow_redirect_test.go`; create `scoped_secrets_test.go`; `manifest_test.go` only if an existing assertion must be extended |

The serial integration owner alone may modify `pkg/plugin/scoped_preparation_test.go` after all eight leaves are accepted. No leaf worker touches that shared file.

## Dependency and Parallel Execution Graph

```text
40c04a26 CP3.4 architecture boundary
  -> e5b6a73e merged secret.Descriptor contract
      -> Group P1, eight file-exclusive leaf worktrees in parallel
         Aliyun | AWS | RAG | Keycloak | ClickHouse | Limit | OAS | OIDC
      -> Task 9 serial support/parity integration test
      -> Task 10 Lane S1 gates and reviewed checkpoint
      -> CP4 composites/M1 and then CP5/C6.6
```

No leaf depends on another leaf. Composite `workflow` depends on the accepted `limit-count` leaf, and final C6.6 Store deletion depends on all eight accepted leaves.

## Shared Scoped-Test Harness Contract

Every leaf keeps its fixture in its own `scoped_secrets_test.go`; there is no cross-package helper file for parallel workers to contend over. The fixture must use the real `secret.NewScopedMaterializer` so tests receive real `secret.Value` plaintext and descriptor digests rather than forging the type.

The package-local fixture has this exact shape (rename only the helper prefix if the package already owns one of these identifiers):

```go
type scopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type scopedSecretBroker struct {
	values map[string]string
	fail   map[string]error
	calls  []scopedSecretCall
}

func (*scopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*scopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this leaf fixture")
}

func (broker *scopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.calls = append(broker.calls, scopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return raw, nil
}

func (*scopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error { return nil }

func newScopedSecretHarness(
	t *testing.T, factory string, values map[string]string,
) (secret.GenerationCapability, secret.Scope, *scopedSecretBroker, func()) {
	t.Helper()
	const revision = uint64(7)
	key := generation.ResourceKey{Kind: "routes", ID: "r1"}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{}}`),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: revision,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{
			Key: key, Disposition: generation.DispositionPublished, Code: "leaf-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision, RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	set := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	broker := &scopedSecretBroker{values: maps.Clone(values), fail: make(map[string]error)}
	registration, err := secret.NewScopedMaterializer(broker, catalog).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		t.Fatal(err)
	}
	baseScope := secret.Scope{
		Generation: revision,
		Attempt: registration.AttemptID(),
		Domain: generation.DomainHTTP,
		Plugin: factory,
		Resource: key,
		Source: capability.SecretPluginConfig,
	}
	return capabilityValue, baseScope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}
```

The fixture imports `context`, `errors`, `maps`, and `testing` plus `pkg/capability`, `pkg/generation`, `pkg/plugin/base`, and `pkg/secret`. A failure test sets `broker.fail[raw] = errors.New("test broker unavailable")` before invoking the scoped wrapper; the production wrapper must redact that error.

Each scoped test calls:

```go
capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, values)
defer closeAttempt()
if err := base.MaterializeScopedPluginSecrets(
	context.Background(), scope, capabilityValue, plugin,
); err != nil {
	t.Fatal(err)
}
```

Assertions compare `broker.calls` with exact manifest fields and raw inputs. They also assert config strings equal `plugin_config#sha256:<64 lowercase hex>` and contain none of the raw reference, environment name, managed path, ciphertext, or plaintext.

---

### Task 0: Consume the Landed `secret.Descriptor` Contract

**Status:** satisfied by integration commit `e5b6a73e`; this task is a read-only branch prerequisite, not leaf implementation work.

**Owner:** `pkg/secret` shared-contract owner. Lane S1 workers must not modify these files.

**Landed files:**

- `pkg/secret/descriptor.go`
- `pkg/secret/descriptor_test.go`
- `docs/superpowers/plans/2026-08-24-immutable-task6-cp2.1-secret-descriptor.md`

**Exact consumed API:**

```go
type Descriptor struct {
    source capability.SecretDeclarationSource
    digest [32]byte
}

func NewDescriptor(
    source capability.SecretDeclarationSource,
    digest [32]byte,
) (Descriptor, error)

func (value Value) Descriptor(
    source capability.SecretDeclarationSource,
) (Descriptor, error)

func (descriptor Descriptor) Source() capability.SecretDeclarationSource
func (descriptor Descriptor) Digest() [32]byte
func (descriptor Descriptor) String() string
```

`String` is exactly `<source>#sha256:<64 lowercase hex>`. Constructors accept only a declaration source and digest; no raw reference, environment name, Vault path, ciphertext, or plaintext can be passed or retained. Fields are private and the landed tests cover all admitted sources, zero/invalid identity, deterministic formatting, and no exported fields.

- [ ] **Step 1: Prove the leaf branch contains the prerequisite**

```bash
git merge-base --is-ancestor e5b6a73e HEAD
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/secret -run '^TestDescriptor' -count=1
```

Expected: both commands succeed before any leaf edit.

- [ ] **Step 2: Freeze consumption rules**

Scoped paths call `value.Descriptor(capability.SecretPluginConfig)`. Legacy paths compute the resolved-value SHA-256 and call `secret.NewDescriptor(capability.SecretPluginConfig, digest)`. No leaf duplicates descriptor formatting or restores `store.ResolvedSecret.Descriptor()` into public config.

---

### Task 1: Migrate `ai-aliyun-content-moderation`

**Exclusive owner:** `pkg/plugin/ai_aliyun_content_moderation/**`.

**Files:**

- Create: `pkg/plugin/ai_aliyun_content_moderation/secrets.go`
- Create: `pkg/plugin/ai_aliyun_content_moderation/scoped_secrets_test.go`
- Modify: `pkg/plugin/ai_aliyun_content_moderation/plugin.go`
- Modify: `pkg/plugin/ai_aliyun_content_moderation/plugin_test.go`
- Modify setup only: `benchmark_test.go`, `response_phase_test.go`

**Interfaces:**

- Consumes: `base.ScopedSecretMaterializer`, `ScopedSecretAccess.Materialize`, `secret.Value.Descriptor`.
- Produces: `func (*Plugin) MaterializeScopedSecrets(context.Context, base.ScopedSecretAccess) error`; package-private credential callbacks for `access_key_id` and `access_key_secret`; legacy `MaterializeSecrets` remains separate.

- [ ] **Step 1: Add RED tests for exact fields, managed resolution, and no fallback**

Add:

```go
func TestScopedSecretsMaterializeAliyunCredentialsAtomically(t *testing.T)
func TestScopedSecretsResolveManagedAliyunCredential(t *testing.T)
func TestPostInitDoesNotSelfMaterializeAliyunCredentials(t *testing.T)
```

The first test uses `$ENV://ALIYUN_ACCESS_ID` and `$ENV://ALIYUN_ACCESS_SECRET`, asserts exactly two broker calls in manifest order, verifies descriptor-only config, runs `PostInit`, and observes a signed moderation request containing the resolved access ID but no descriptor. The managed case resolves `$secret://vault/aliyun/access-key-secret`. The failure subtest makes the second broker value fail and proves neither credential nor descriptor is installed. The `PostInit` test starts with raw references and proves a redacted credential-unavailable error without a broker/Store lookup.

- [ ] **Step 2: Verify RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/ai_aliyun_content_moderation \
  -run '^TestScopedSecrets|^TestPostInitDoesNotSelfMaterializeAliyunCredentials$' -count=1
```

Expected: FAIL because the plugin does not implement `ScopedSecretMaterializer`, and current `PostInit` still self-materializes.

- [ ] **Step 3: Implement atomic dual-path credentials**

Move secret ownership out of `plugin.go` into `secrets.go`. The scoped method always materializes the two required exact fields, obtains `Descriptor(capability.SecretPluginConfig)`, stages both values/descriptors, then installs both. Its implementation contains no Store symbol and does not call `MaterializeSecrets`.

Keep the legacy Store path in `MaterializeSecrets`, but make its public config descriptor with `secret.NewDescriptor(capability.SecretPluginConfig, sha256.Sum256(resolvedBytes))` instead of `ResolvedSecret.Descriptor`, so the environment name/path is not retained. A package-private `useAliyunCredentials(func(id, secret string) error)` nests scoped `Value.Use` callbacks or clones/clears legacy bytes. `sendModerationRequest` signs entirely inside that callback. `Stop` destroys legacy owners, replaces scoped values with zero values, and nils mode flags.

Remove the `PostInit -> MaterializeSecrets` branch. `PostInit` returns the existing constant redacted unavailable message when credentials were not explicitly prepared. Update all direct setup/benchmark helpers to call `base.MaterializePluginSecrets` before `PostInit` during the additive window.

- [ ] **Step 4: Verify GREEN and behavior**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/ai_aliyun_content_moderation \
  -run '(ScopedSecrets|MaterializeSecrets|PostInit|Signature|Request|Response)' -count=1
go test -race ./pkg/plugin/ai_aliyun_content_moderation \
  -run '(ScopedSecrets|MaterializeSecrets|Signature)' -count=1
```

- [ ] **Step 5: Leaf checkpoint**

After diff review by the integration owner:

```bash
git add pkg/plugin/ai_aliyun_content_moderation
git commit -m "refactor(ai-aliyun): scope moderation credentials"
```

---

### Task 2: Migrate `ai-aws-content-moderation`

**Exclusive owner:** `pkg/plugin/ai_aws_content_moderation/**`.

**Files:**

- Create: `pkg/plugin/ai_aws_content_moderation/secrets.go`
- Create: `pkg/plugin/ai_aws_content_moderation/scoped_secrets_test.go`
- Modify: `pkg/plugin/ai_aws_content_moderation/plugin.go`
- Modify: `pkg/plugin/ai_aws_content_moderation/plugin_test.go`

**Interfaces:**

- Consumes: scoped access and shared descriptor contract.
- Produces: scoped materializer for two required AWS credentials and the optional session token; one bounded SigV4 credential callback.

- [ ] **Step 1: Write RED tests**

Add:

```go
func TestScopedSecretsMaterializeAWSComprehendCredentials(t *testing.T)
func TestScopedSecretsSkipEmptyAWSSessionToken(t *testing.T)
func TestScopedSecretsResolveManagedAWSSessionToken(t *testing.T)
func TestScopedSecretsAWSFailureInstallsNothing(t *testing.T)
```

Assert exact calls to `comprehend.access_key_id`, `comprehend.secret_access_key`, and only when non-empty `comprehend.session_token`. Exercise `$ENV` for required fields and `$secret://vault/aws/session-token` for the managed case. Sign a request and verify resolved credentials reach SigV4 while config/errors expose only descriptors.

- [ ] **Step 2: Verify RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/ai_aws_content_moderation -run '^TestScopedSecrets' -count=1
```

Expected: FAIL on missing scoped ownership/unowned references.

- [ ] **Step 3: Implement minimal scoped and legacy paths**

Create three package-private credential slots. Stage required values first, conditionally stage the session token, then atomically install values and `plugin_config` descriptors. Preserve the legacy Store method independently for Builder tests. Replace the byte-clone code in SigV4 setup with `useAWSCredentials(func(accessID, secret, token string) error)`; optional empty token passes `""` without a capability call. `Stop` drops scoped values and destroys legacy owners.

Keep `PostInit` defaults/client construction unchanged; it must not gain secret resolution.

- [ ] **Step 4: Verify GREEN**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/ai_aws_content_moderation \
  -run '(ScopedSecrets|MaterializeSecrets|SigV4|SessionToken|PostInit)' -count=1
go test -race ./pkg/plugin/ai_aws_content_moderation \
  -run '(ScopedSecrets|SigV4)' -count=1
```

- [ ] **Step 5: Leaf checkpoint**

```bash
git add pkg/plugin/ai_aws_content_moderation
git commit -m "refactor(ai-aws): scope comprehend credentials"
```

---

### Task 3: Migrate `ai-rag`

**Exclusive owner:** `pkg/plugin/ai_rag/**`.

**Files:**

- Create: `pkg/plugin/ai_rag/secrets.go`
- Create: `pkg/plugin/ai_rag/scoped_secrets_test.go`
- Modify: `pkg/plugin/ai_rag/plugin.go`
- Modify: `pkg/plugin/ai_rag/plugin_test.go`

**Interfaces:**

- Consumes: scoped access and descriptor prerequisite.
- Produces: scoped materializer for both provider API keys; bounded provider request callbacks; explicit pre-`PostInit` preparation.

- [ ] **Step 1: Write RED tests**

Add:

```go
func TestScopedSecretsMaterializeRAGProviderKeys(t *testing.T)
func TestScopedSecretsResolveManagedRAGSearchKey(t *testing.T)
func TestScopedSecretsRAGSecondKeyFailureIsAtomic(t *testing.T)
func TestPostInitDoesNotSelfMaterializeRAGKeys(t *testing.T)
```

Use exact fields `embeddings_provider.azure_openai.api_key` and `vector_search_provider.azure_ai_search.api_key`. Run both provider mock servers after scoped preparation and prove each receives its resolved `api-key`. The managed case uses `$secret://vault/ai-rag/search-key`. Assert raw references/plaintext never appear in config/errors.

- [ ] **Step 2: Verify RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/ai_rag \
  -run '^TestScopedSecrets|^TestPostInitDoesNotSelfMaterializeRAGKeys$' -count=1
```

Expected: FAIL because scoped ownership is missing and `PostInit` still invokes Store materialization.

- [ ] **Step 3: Implement scoped values and remove self-fallback**

Create independent embedding/search credential slots and an atomic two-value installer. Replace `resolvedSecretString(*store.ResolvedSecret)` with `withEmbeddingKey` and `withSearchKey`; build and execute each provider request inside the corresponding callback. Keep legacy materialization separate and descriptor-safe. Remove the self-materializing branch from `PostInit`; return the constant provider-credential-unavailable error until an explicit preparation path has succeeded. Update the common `newTestPlugin` helper to materialize before `PostInit`.

- [ ] **Step 4: Verify GREEN**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/ai_rag \
  -run '(ScopedSecrets|MaterializedAPIKeys|Provider|PostInit)' -count=1
go test -race ./pkg/plugin/ai_rag \
  -run '(ScopedSecrets|MaterializedAPIKeys)' -count=1
```

- [ ] **Step 5: Leaf checkpoint**

```bash
git add pkg/plugin/ai_rag
git commit -m "refactor(ai-rag): scope provider credentials"
```

---

### Task 4: Migrate `authz-keycloak`

**Exclusive owner:** `pkg/plugin/authz_keycloak/**`.

**Files:**

- Create: `pkg/plugin/authz_keycloak/secrets.go`
- Create: `pkg/plugin/authz_keycloak/scoped_secrets_test.go`
- Modify: `pkg/plugin/authz_keycloak/plugin.go`
- Modify: `pkg/plugin/authz_keycloak/plugin_test.go`

**Interfaces:**

- Consumes: optional scoped field `client_secret`.
- Produces: `withClientSecret(func(string) error)`, descriptor digest for `serviceAccountCacheKey`, independent legacy and scoped materializers.

- [ ] **Step 1: Write RED tests**

Add:

```go
func TestScopedSecretsMaterializeKeycloakClientSecret(t *testing.T)
func TestScopedSecretsSkipEmptyKeycloakClientSecret(t *testing.T)
func TestScopedSecretsResolveManagedKeycloakClientSecret(t *testing.T)
func TestScopedKeycloakCacheIdentityUsesDigestNotCredential(t *testing.T)
```

The managed reference is `$secret://vault/keycloak/client-secret`. Verify token and refresh forms contain the resolved value, optional empty config makes zero broker calls, cache identity changes with value digest, and neither identity/config/error includes raw or plaintext.

- [ ] **Step 2: Verify RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/authz_keycloak -run '^TestScoped' -count=1
```

Expected: FAIL because only `MaterializeSecrets` exists.

- [ ] **Step 3: Implement one optional credential slot**

For an empty client secret, mark preparation complete without calling access and let `withClientSecret` invoke the callback with `""`. For a present value, materialize exact field `client_secret`, stage its descriptor/digest, and install atomically. Replace direct `ResolvedSecret.Fingerprint` use in `serviceAccountCacheKey` with the credential slot digest; replace Store byte access with the bounded callback. Keep the Resty client generation-neutral: it must not retain the secret, while request forms are created inside the callback. `Stop` releases the client, destroys legacy bytes, drops scoped values, and clears the cached digest.

- [ ] **Step 4: Verify GREEN**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/authz_keycloak \
  -run '(Scoped|MaterializeSecrets|ClientSecret|ServiceAccount|Refresh)' -count=1
go test -race ./pkg/plugin/authz_keycloak \
  -run '(Scoped|ServiceAccount|Refresh)' -count=1
```

- [ ] **Step 5: Leaf checkpoint**

```bash
git add pkg/plugin/authz_keycloak
git commit -m "refactor(authz-keycloak): scope client secret"
```

---

### Task 5: Migrate `clickhouse-logger` and Remove Hidden `PostInit` Resolution

**Exclusive owner:** `pkg/plugin/clickhouse_logger/**`.

**Files:**

- Create: `pkg/plugin/clickhouse_logger/secrets.go`
- Create: `pkg/plugin/clickhouse_logger/scoped_secrets_test.go`
- Modify: `pkg/plugin/clickhouse_logger/plugin.go`
- Modify: `pkg/plugin/clickhouse_logger/plugin_test.go`

**Interfaces:**

- Consumes: optional `user`, strict `password`.
- Produces: explicit legacy strict-decryption phase before `PostInit`; scoped user/password callbacks; private descriptor-based client identity.

- [ ] **Step 1: Write RED tests**

Add:

```go
func TestScopedSecretsMaterializeClickHouseUserAndStrictPassword(t *testing.T)
func TestScopedSecretsResolveManagedClickHouseUser(t *testing.T)
func TestScopedSecretsClickHousePasswordFailureIsAtomic(t *testing.T)
func TestPostInitNeverCallsClickHouseDataEncryption(t *testing.T)
```

The scoped test must call `password` even when its raw value is a literal, because catalog strictness is materializer-owned. It calls `user` only when non-empty. Use `$secret://vault/clickhouse/user` for the managed case and assert `SendBatch` emits resolved `X-ClickHouse-User`/`X-ClickHouse-Key` headers. The PostInit test supplies a poison/missing resolver after explicit preparation and proves PostInit does not inspect it.

Move the existing `TestPostInitRejectsInvalidEncryptedPassword` and rotated-key test expectations to the explicit legacy `base.MaterializePluginSecrets` call. Their RED state is that strict decryption currently occurs only in `PostInit`.

- [ ] **Step 2: Verify RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/clickhouse_logger \
  -run '(ScopedSecrets|PostInitNeverCallsClickHouseDataEncryption|EncryptedPassword)' -count=1
```

- [ ] **Step 3: Separate strict legacy and scoped preparation**

In legacy `MaterializeSecrets`, require the existing DataEncryption resolver, run `ResolveForContext(rawPassword, "clickhouse-logger.password")`, immediately wrap the resolved password in a legacy owner, and install only a shared descriptor. Optional user references continue through Store; literals may remain private config values only on the legacy path if Store was never asked to own them. All resolver presence/decrypt logic leaves `PostInit`.

In scoped `MaterializeScopedSecrets`, materialize exact `user` when present and always exact `password`; stage both values/descriptors before installation. `resolvedUser` and `resolvedPassword` become callback-based helpers so `SendBatch` constructs and sends headers inside nested callbacks. Client UID uses descriptors/digests, never plaintext. Do not alter metadata/log-format precedence, batch processor setup, or logger stop ordering.

- [ ] **Step 4: Verify GREEN and async ownership**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/clickhouse_logger \
  -run '(ScopedSecrets|MaterializeSecrets|EncryptedPassword|SendBatch|PostInit)' -count=1
go test -race ./pkg/plugin/clickhouse_logger \
  -run '(ScopedSecrets|SendBatch|Stop)' -count=1
```

- [ ] **Step 5: Leaf checkpoint**

```bash
git add pkg/plugin/clickhouse_logger
git commit -m "refactor(clickhouse-logger): scope logger credentials"
```

---

### Task 6: Migrate `limit-count` Without Losing Alias Provenance

**Exclusive owner:** `pkg/plugin/limit_count/**`.

**Files:**

- Create: `pkg/plugin/limit_count/secrets.go`
- Create: `pkg/plugin/limit_count/scoped_secrets_test.go`
- Modify: `pkg/plugin/limit_count/plugin.go`
- Modify: `pkg/plugin/limit_count/redis.go`
- Modify: `pkg/plugin/limit_count/plugin_test.go`
- Modify only if required by an existing assertion: `pkg/plugin/limit_count/manifest_test.go`

**Interfaces:**

- Consumes: exact declarations `key`, `redis_host`, `redis_config.redis_host`, `redis_cluster_nodes`, `redis_cluster_config.redis_cluster_nodes`.
- Produces: package-private provenance enum/value containing the exact admitted field; key/host/node scoped callbacks; digest-only Redis backend identity.

- [ ] **Step 1: Write RED provenance and container tests**

Add:

```go
func TestScopedSecretsPreserveRootRedisHostDeclaration(t *testing.T)
func TestScopedSecretsPreserveNestedRedisHostDeclaration(t *testing.T)
func TestScopedSecretsPreserveRootClusterContainerDeclaration(t *testing.T)
func TestScopedSecretsPreserveNestedClusterContainerDeclaration(t *testing.T)
func TestScopedSecretsResolveManagedLimitCountKey(t *testing.T)
func TestScopedSecretsSkipEmptyLimitCountOptionalFields(t *testing.T)
func TestScopedSecretsLimitCountNodeFailureIsAtomic(t *testing.T)
```

Root tests must observe only root field names; nested tests must observe only nested field names. Every cluster element uses the same container field, in slice order, with no index or `.*`. The managed key is `$secret://vault/limit-count/key`. Failure on node N installs no key/host/node values and reports only the bounded node index.

- [ ] **Step 2: Verify RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/limit_count -run '^TestScopedSecrets' -count=1
```

Expected: FAIL; current `MaterializeSecrets` normalizes root aliases before selecting authority and no scoped method exists.

- [ ] **Step 3: Capture authority before normalization**

Before calling `applyRootRedisConfig` or `applyRootRedisClusterConfig`, select exactly one source for each effective value:

```go
type secretFieldSelection struct {
	field string
	raw   string
}
```

For Redis host, nested wins when explicitly non-empty; otherwise root is selected. For cluster nodes, a non-empty nested slice wins; otherwise clone the root slice. Record `redis_config.redis_host` versus `redis_host`, and `redis_cluster_config.redis_cluster_nodes` versus `redis_cluster_nodes`, before normalization. Never infer authority from the normalized copy.

- [ ] **Step 4: Implement atomic scoped materialization and runtime use**

Materialize `key` only when non-empty, host only when selected, and each selected node with its container field. Stage all `secret.Value`, descriptors, and field provenance before changing config. Write the descriptor to the originally supplied field and to the normalized runtime mirror, while retaining the original field in the private selection for audit/error context.

Replace `ResolvedSecret.Bytes/Fingerprint` in `resolveKey` and `redisBackendClientLocked` with bounded value callbacks and descriptor digests. Build Redis options/client inside the callback that supplies host/node plaintext; shared-client keys contain policy plus descriptor digests. Keep password/sentinel fields unchanged because they are not Lane S1 declarations. Legacy Store materialization remains separate and adopts the same pre-normalization selection logic.

- [ ] **Step 5: Verify GREEN, alias behavior, and concurrency**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/limit_count \
  -run '(ScopedSecrets|MaterializeSecrets|RootRedis|RedisCluster|ResolveKey|Backend|Group)' -count=1
go test -race ./pkg/plugin/limit_count \
  -run '(ScopedSecrets|RedisCluster|Backend|Stop)' -count=1
```

- [ ] **Step 6: Leaf checkpoint**

```bash
git add pkg/plugin/limit_count
git commit -m "refactor(limit-count): scope backend secret fields"
```

---

### Task 7: Migrate `oas-validator` With Refresh-Safe Secret Use

**Exclusive owner:** `pkg/plugin/oas_validator/**`.

**Files:**

- Create: `pkg/plugin/oas_validator/secrets.go`
- Create: `pkg/plugin/oas_validator/scoped_secrets_test.go`
- Modify: `pkg/plugin/oas_validator/plugin.go`
- Modify: `pkg/plugin/oas_validator/plugin_test.go`
- Modify setup only: `pkg/plugin/oas_validator/benchmark_test.go`

**Interfaces:**

- Consumes: optional `spec`; terminal-container declaration `spec_url_request_headers` for each map value.
- Produces: `withInlineSpec(func(string) error)` and `withRequestHeaders(func(map[string]string) error)`; explicit preparation before PostInit; deterministic header materialization.

- [ ] **Step 1: Write RED tests**

Add:

```go
func TestScopedSecretsMaterializeOASInlineSpec(t *testing.T)
func TestScopedSecretsMaterializeOASHeaderContainerDeterministically(t *testing.T)
func TestScopedSecretsResolveManagedOASHeader(t *testing.T)
func TestScopedSecretsOASHeaderFailureIsAtomic(t *testing.T)
func TestPostInitDoesNotSelfMaterializeOASSecrets(t *testing.T)
```

Sort header names before capability calls, but pass `spec_url_request_headers` for every value. Use `$secret://vault/oas/authorization` as the managed value. Prove inline spec compiles and remote/external-reference requests receive resolved headers, while config retains only descriptors. The failure test proves already staged inline/header values do not become visible. Direct PostInit with raw references must fail redacted without materialization.

- [ ] **Step 2: Verify RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/oas_validator \
  -run '^TestScopedSecrets|^TestPostInitDoesNotSelfMaterializeOASSecrets$' -count=1
```

- [ ] **Step 3: Implement scoped values and bounded refresh callbacks**

Materialize non-empty `spec`; sort map keys and materialize every header value with the terminal container field. Stage everything, then install descriptor copies in a newly allocated config map. `compileSpec` runs inline parsing/compilation inside `withInlineSpec`. Remote and external-reference fetches run inside `withRequestHeaders`; recursively nest `Value.Use` calls and invoke the fetch callback before unwinding so the plaintext map is not retained on the plugin.

Remove the `PostInit -> MaterializeSecrets` branch. PostInit requires explicit preparation only when declared fields are present, then applies defaults/metadata and validates inline bytes through the bounded callback. Preserve refresh last-good behavior, cancellation, metadata TTL, SSRF policy, and Stop joining. Stop drops scoped values after the refresh goroutine has joined, then destroys legacy owners.

- [ ] **Step 4: Verify GREEN and refresh race**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/oas_validator \
  -run '(ScopedSecrets|MaterializeSecrets|InlineSpec|Headers|Refresh|ExternalReference|PostInit)' -count=1
go test -race ./pkg/plugin/oas_validator \
  -run '(ScopedSecrets|Refresh|Stop|ExternalReference)' -count=1
```

- [ ] **Step 5: Leaf checkpoint**

```bash
git add pkg/plugin/oas_validator
git commit -m "refactor(oas-validator): scope document secrets"
```

---

### Task 8: Migrate All Five Declared `openid-connect` Secrets

**Exclusive owner:** `pkg/plugin/openid_connect/**`.

**Files:**

- Create: `pkg/plugin/openid_connect/secrets.go`
- Create: `pkg/plugin/openid_connect/scoped_secrets_test.go`
- Modify: `pkg/plugin/openid_connect/plugin.go`
- Modify: `pkg/plugin/openid_connect/flow.go`
- Modify: `pkg/plugin/openid_connect/provider.go`
- Modify: `pkg/plugin/openid_connect/session.go`
- Modify: `pkg/plugin/openid_connect/plugin_test.go`
- Modify focused setup/assertions: `session_test.go`, `flow_redirect_test.go`
- Modify only if required by an existing assertion: `manifest_test.go`

**Interfaces:**

- Consumes: optional `client_secret`, `client_rsa_private_key`, `public_key`, `session.secret`, `session.redis.password`.
- Produces: bounded client-secret callback, parsed RSA/private/public keys, derived `[32]byte` session key, Redis client built inside password use, provider construction with an explicit private client-secret argument.

- [ ] **Step 1: Write five-field RED coverage**

Add:

```go
func TestScopedSecretsMaterializeAllOIDCFields(t *testing.T)
func TestScopedSecretsSkipAbsentOIDCFieldsForBearerOnly(t *testing.T)
func TestScopedSecretsResolveManagedOIDCClientSecret(t *testing.T)
func TestScopedSecretsRejectInvalidOIDCPublicKeyAtomically(t *testing.T)
func TestScopedSecretsOIDCConfigAndErrorsAreRedacted(t *testing.T)
```

The full test uses a valid generated RSA private/public key, a code-flow session secret, and Redis password; it asserts exact field order above and no undeclared fields. The managed case uses `$secret://vault/oidc/client-secret`. The bearer-only case proves all empty optional fields make zero calls. Invalid public key after earlier successful fields installs no client secret/private key/session key/Redis password.

- [ ] **Step 2: Add RED behavior tests for every private consumer**

Extend focused tests to prove:

- `client_secret_basic`, `client_secret_post`, and `client_secret_jwt` use the scoped value;
- `private_key_jwt` uses the parsed private key after config contains only a descriptor;
- static JWT verification uses the parsed public key;
- session cookie seal/open uses the derived private session key, not the descriptor;
- Redis options receive the resolved password while client identity/config do not expose it;
- provider/OAuth2 construction gets the resolved client secret without restoring it to `Config`.

- [ ] **Step 3: Verify RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/openid_connect \
  -run '(ScopedSecrets|ClientSecretPost|ClientSecretJWT|PrivateKeyJWT|PublicKey|Session|Redis)' -count=1
```

Expected: FAIL because current scoped ownership is absent and the existing materializer handles only `public_key`.

- [ ] **Step 4: Stage and derive all five values atomically**

In `secrets.go`, materialize each non-empty field in the fixed order. Parse private/public keys inside their `Value.Use` callbacks into local derived objects. Derive `sha256(sessionSecret)` into a local `[32]byte`; retain no session plaintext. Retain client-secret and Redis-password `secret.Value` for bounded runtime/client construction. Only after all parsing succeeds, install values/derived objects, presence flags, and descriptors.

The legacy method must cover the same five manifest fields for the current Builder, not just `public_key`. It remains independent and uses Store/DataEncryption compatibility mechanisms already supplied to the Builder; public config still receives only the shared descriptor format.

- [ ] **Step 5: Remove all runtime reads of secret-bearing config fields**

Make these exact structural changes:

```go
func (p *Plugin) withClientSecret(use func(string) error) error
func (p *Plugin) withRedisPassword(use func(string) error) error
func newProviderClient(
	ctx context.Context,
	doc discoveryData,
	cfg Config,
	clientSecret string,
	httpClient *http.Client,
) *providerClient
```

`authenticatedFormRequest`, HMAC `clientAssertion`, and provider construction execute inside `withClientSecret`. `providerClient` may retain its private OAuth2 config until plugin Stop, but `Config.ClientSecret` remains a descriptor. `PostInit` uses presence flags and the already parsed private key; it never parses descriptor text.

Replace `sessionKey()` hashing of `Config.Session.Secret` with a clone of the pre-derived `[32]byte`. Build Redis options/client inside `withRedisPassword`; the client is closed/released before the scoped value is dropped. Static public-key verification uses the pre-parsed object. `Stop` clears derived key arrays/pointers, provider/session client pointers, legacy owners, and scoped value structs after client release.

- [ ] **Step 6: Verify GREEN and concurrent flows**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/openid_connect \
  -run '(ScopedSecrets|MaterializeSecrets|ClientSecret|PrivateKeyJWT|PublicKey|Session|Redis|Provider)' -count=1
go test -race ./pkg/plugin/openid_connect \
  -run '(ScopedSecrets|ClientSecret|Session|Redis|Provider|Stop)' -count=1
```

- [ ] **Step 7: Leaf checkpoint**

```bash
git add pkg/plugin/openid_connect
git commit -m "refactor(openid-connect): scope oidc credentials"
```

---

### Task 9: Serial Lane Support and Boundary Integration

**Owner:** Task 6 integration owner after all eight leaf checkpoints are reviewed and merged.

**Files:**

- Modify: `pkg/plugin/scoped_preparation_test.go`
- Read-only scan: all eight Lane S1 package directories

**Interfaces:**

- Consumes: all eight `ScopedSecretMaterializer` implementations.
- Produces: a factory-registry support gate used by CP5 compiler preparation.

- [ ] **Step 1: Add the registry support test**

Add:

```go
func TestLaneS1FactoriesSupportScopedSecretMaterialization(t *testing.T) {
	factories := []string{
		"ai-aliyun-content-moderation",
		"ai-aws-content-moderation",
		"ai-rag",
		"authz-keycloak",
		"clickhouse-logger",
		"limit-count",
		"oas-validator",
		"openid-connect",
	}
	for _, factory := range factories {
		t.Run(factory, func(t *testing.T) {
			supported, err := SupportsScopedSecretMaterialization(factory)
			if err != nil || !supported {
				t.Fatalf("SupportsScopedSecretMaterialization(%q) = %v, %v", factory, supported, err)
			}
		})
	}
}
```

- [ ] **Step 2: Run support and compiler fail-closed tests**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin -run '(LaneS1Factories|ScopedSecretMaterialization|FactoryInstance)' -count=1
go test ./pkg/compiler -run '(ScopedSecretSupport|FactoryOccurrence|PrepareScopedPluginSecrets)' -count=1
```

Expected: PASS; all declared Lane S1 factories are admitted without invoking Init/PostInit/materialization during the support witness.

- [ ] **Step 3: Enforce the additive-window boundary by source scan**

```bash
rg -n 'func \(p \*Plugin\) MaterializeScopedSecrets' \
  pkg/plugin/{ai_aliyun_content_moderation,ai_aws_content_moderation,ai_rag,authz_keycloak,clickhouse_logger,limit_count,oas_validator,openid_connect}
rg -n 'store\.MaterializeSecret|DataEncryption\(\)|ResolvedSecret|pkg/store' \
  pkg/plugin/{ai_aliyun_content_moderation,ai_aws_content_moderation,ai_rag,authz_keycloak,clickhouse_logger,limit_count,oas_validator,openid_connect} \
  --glob '*.go' --glob '!**/*_test.go'
rg -n 'MaterializeSecrets\(' \
  pkg/plugin/{ai_aliyun_content_moderation,ai_aws_content_moderation,ai_rag,authz_keycloak,clickhouse_logger,limit_count,oas_validator,openid_connect} \
  --glob '*.go'
```

Review every match. At this checkpoint, Store/DataEncryption matches are allowed only in clearly named legacy preparation helpers. They must not appear in any scoped method, runtime request callback, or PostInit body. `MaterializeSecrets(` must not appear inside `MaterializeScopedSecrets`.

- [ ] **Step 4: Shared test checkpoint**

```bash
git add pkg/plugin/scoped_preparation_test.go
git commit -m "test(plugin): require scoped lane s1 factories"
```

---

### Task 10: Lane S1 Integration Acceptance

**Owner:** Task 6 integration owner.

**Files:** read-only verification of the complete Lane S1 diff against the descriptor-prerequisite parent.

- [ ] **Step 1: Run every leaf package test together**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/ai_aliyun_content_moderation \
  ./pkg/plugin/ai_aws_content_moderation \
  ./pkg/plugin/ai_rag \
  ./pkg/plugin/authz_keycloak \
  ./pkg/plugin/clickhouse_logger \
  ./pkg/plugin/limit_count \
  ./pkg/plugin/oas_validator \
  ./pkg/plugin/openid_connect -count=1
```

- [ ] **Step 2: Run retained-state race coverage**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test -race ./pkg/plugin/ai_aliyun_content_moderation \
  ./pkg/plugin/ai_aws_content_moderation \
  ./pkg/plugin/ai_rag \
  ./pkg/plugin/authz_keycloak \
  ./pkg/plugin/clickhouse_logger \
  ./pkg/plugin/limit_count \
  ./pkg/plugin/oas_validator \
  ./pkg/plugin/openid_connect -count=1
```

- [ ] **Step 3: Run declaration/compiler/build gates**

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/secret ./pkg/plugin/base ./pkg/plugin ./pkg/compiler \
  -run '(Descriptor|Scoped|Factory|Occurrence|Materialize)' -count=1
go run ./cmd/capability-gen -check
golangci-lint run \
  ./pkg/secret/... \
  ./pkg/plugin/ai_aliyun_content_moderation/... \
  ./pkg/plugin/ai_aws_content_moderation/... \
  ./pkg/plugin/ai_rag/... \
  ./pkg/plugin/authz_keycloak/... \
  ./pkg/plugin/clickhouse_logger/... \
  ./pkg/plugin/limit_count/... \
  ./pkg/plugin/oas_validator/... \
  ./pkg/plugin/openid_connect/...
make build
git diff --check
```

Do not expand to `go test ./...` or the integration corpus unless a Lane S1 change has no narrower proof.

- [ ] **Step 4: Perform the merge review checklist**

For each factory verify:

- every manifest field has exactly one scoped call path;
- empty optional values make no call;
- strict fields are still passed to the capability even when literal/ciphertext;
- container leaves use the container declaration;
- scoped failure is atomic and redacted;
- `PostInit` has no resolver/self-materialization;
- public config has source class plus digest only;
- request/provider/client code reads no secret-bearing public config field;
- Stop orders client/task shutdown before dropping scoped values and destroys legacy owners once;
- scoped method has no legacy/Store/DataEncryption call edge;
- no schema, priority, phase, metadata, or unrelated behavior changed.

- [ ] **Step 5: Lane integration checkpoint**

After independent review accepts all leaf commits and the shared support test, record the integration head as the Lane S1 checkpoint. Do not squash unrelated CP3 history and do not merge to local `master` until the complete Task 6 acceptance gate passes.

## Known Compatibility Boundaries After Lane S1

1. The current Builder still calls `MaterializeSecrets`; therefore Store/DataEncryption compatibility imports remain intentionally reachable only from the explicit legacy preparation functions. C6.6 records that residual caller, and the joint Task 9 cutover removes the methods/imports after Task 7/8 replaces production construction.
2. Scoped descriptor strings intentionally stop exposing the original `$ENV` name or `$secret` path. This changes only diagnostic/effective-config representation, not APISIX request behavior.
3. `clickhouse-logger` and `oas-validator` still read plugin metadata from Store in this checkpoint. Lane M1 replaces that separate source; S1 must not mix the two changes.
4. OAS refresh workers, ClickHouse batch workers, shared clients, Redis clients, and limiter groups retain their current owners. CP5 supplies private lifecycle authority, Tasks 7/8 attach effective owners, and Task 9 binds installed retirement; S1 only makes their secret inputs attempt-scoped.
5. OIDC's private OAuth2/Redis clients may retain strings internally after being built inside `Value.Use`; public config never does. Stop/release and attempt close are the lifetime boundary, and the code must not claim Go strings were zeroed.
6. `limit-count` root and nested aliases remain accepted. The exact originally supplied declaration authorizes materialization; normalized mirrors carry only the same descriptor and never create a second capability call.
7. Managed-reference tests use the real scoped materializer with a package-local fake broker. They validate scope/catalog/field behavior without requiring a live Vault service.
8. All eight factories remain HTTP-only and keep their manifest-supported Linux, Darwin, and Windows platform declarations. No OS-specific implementation is introduced.

## Self-Review Record

- Spec coverage: every Lane S1 bullet has an owning task; PostInit self-fallback removal is explicit for Aliyun, RAG, and OAS, and ClickHouse's hidden strict resolver is also moved to explicit preparation.
- Source-of-truth coverage: every manifest declaration is named; OIDC covers all five rather than preserving the current one-field gap; limit-count container/alias rules are explicit.
- Ownership coverage: eight leaf paths are disjoint; the descriptor and shared registry test have serial owners; no leaf is instructed to edit shared files.
- Type consistency: all leaf tasks consume the same `base.ScopedSecretAccess`, `secret.Value`, and Task 0 descriptor signatures; new compiler authority types do not enter leaf code.
- Placeholder scan: the plan contains no unspecified implementation or test steps; all deferred boundaries name their owning later checkpoint.
