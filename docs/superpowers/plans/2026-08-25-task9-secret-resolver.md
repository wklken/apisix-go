# Task 9 Generation Secret Resolver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move generation-scoped environment/Vault secret resolution out of `pkg/store` into an owned `secret.GenerationSecretResolver` that admits only exact candidate or verified published recovery closures, preserves candidate/recovery isolation at the same desired revision, and closes every secret view safely.

**Architecture:** `GenerationSecretResolver` is the sole in-process implementation of `secret.AttemptResolverFactory`. It owns one immutable data-encryption service, one resolver-owned HTTP client, and independent attempt objects whose resource indexes, Vault caches, lifecycle gates, and cleanup state are never shared across candidate or recovery views. Candidate and recovery open calls validate their complete generation input, recompute the domain-separated `AttemptID`, clone only published/last-good closure resources, and return an `AttemptResolver`; `ResolveScoped` performs a second exact authority check before decrypting or contacting a backend.

**Tech Stack:** Go 1.26; standard `context`, `crypto/subtle`, `crypto/sha256`, `encoding` through the repository `pkg/json`, `net/http`, `net/url`, `os`, `path`, `strings`, `sync`, and `time`; existing `pkg/capability`, `pkg/data_encryption`, `pkg/generation`, and `pkg/secret` interfaces. Do not add a dependency.

**Spec:** `docs/superpowers/plans/2026-08-25-immutable-task9-joint-cutover.md`, Task 9/T9-1 and Frozen cross-lane contract 6; the corrected ordering in `docs/superpowers/plans/2026-08-24-journal-immutable-cutover-reorder.md`; the parent Task 9 section in `docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`.

## Global Constraints

- Base is local `master` `16b28bb54162da6e94560d772c46640c719cdde6` or the parent Task 9 integration base containing the same frozen contracts; do not rebuild Tasks 1-8.
- The only public production boundary is `secret.AttemptResolverFactory`; keep `AttemptResolver`, `Scope`, `Materializer`, and attempt-ID encodings compatible with `pkg/secret/materializer.go` and `pkg/secret/attempt_id.go`.
- `OpenCandidate` receives only `AttemptID`, `generation.ApplyTicket`, and `generation.PublicationSet`; `OpenRecovery` receives only `AttemptID`, `generation.RevisionSet`, and verified `map[generation.Domain]generation.PublishedGeneration`.
- Recovery code never accepts or consults `generation.RecoveryState.Desired`; the caller must pass only `RecoveryState.Revisions` and `RecoveryState.Published` after journal verification.
- Candidate and recovery views with the same desired revision must be concurrently valid but must have different domain-separated IDs and independent indexes.
- Do not import `pkg/store`, call a package-global Store getter, read bbolt, inspect an event, or use a mutable/last-good Store cache from the new resolver.
- Do not modify `pkg/store/secret_broker.go`, `pkg/store/consumer_secret.go`, `pkg/store/store.go`, `pkg/secret/materializer.go`, or any server/provider file in this lane. The transitional Store broker remains until the joint T9-9 deletion checkpoint.
- Every sensitive byte copied into an attempt or cache is zeroed on eviction, expiry, revoke, failed open cleanup, factory close, or any other terminal attempt cleanup. Errors never contain raw references, tokens, response bodies, or resolved values.
- Run Go commands only as `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/secret -count=1'` or the other exact commands below; use impact-scoped `pkg/secret` tests and `git diff --check`, not the repository-wide test aggregation.
- This child lane writes no commit, push, PR, or merge. The parent integration owner reviews the plan and creates the implementation/integration commits after the child plans are accepted.

## Current State and Invariants

The following facts are frozen from the current source and are prerequisites for implementation:

- `pkg/secret/materializer.go` already defines `AttemptResolverFactory`, `AttemptResolver`, `AttemptRegistration`, and the `Materializer` registration lifecycle. `NewMaterializer` reserves the exact ID before calling `OpenCandidate`/`OpenRecovery`, and `AttemptRegistration.Close` invokes the resolver close callback before releasing or quarantining its own registry entry.
- `pkg/secret/attempt_id.go` already provides `CandidateAttemptID(ticket, set)` and `RecoveryAttemptID(revisions, published)`. Both encodings sort domains/resources/decisions and include the full snapshot/closure identity. Candidate and recovery use different encoding domains.
- `generation.ValidatePublicationSet` validates a candidate's artifact, snapshot digest, closure, decisions, and required-domain set. `generation.ValidateRecoverySet` validates the exact non-zero published domain revisions and does not accept a desired-only serving set.
- `pkg/store/secret_broker.go` currently holds the transitional implementation. It retains candidate resources from `candidate.Snapshot` and recovery resources from `published.Snapshot`, checks attempt/generation/domain/resource scope before resolving, supports `$ENV://` and `$secret://vault/<id>/<path>`, uses per-attempt bounded caches, and drains in-flight resolutions before zeroing.
- `pkg/store/consumer_secret.go` currently owns the constants, Vault resource configuration type, HTTP client creation, environment traversal, and legacy Store-backed `secrets` bucket lookup. Only the pure backend logic moves; the new implementation must receive the `secrets/vault/<id>` bytes from the immutable closure and never call `Store.GetFromBucket`.
- `data_encryption.Service` validates the manifest-declared `(plugin, source, field)` and resolves optional/strict ciphertext before a managed reference is handled. The new attempt must call it only after exact scope admission.
- `generation.Snapshot.Lookup` returns a caller-owned byte clone. Resource indexes must copy only keys admitted by the candidate/recovery decisions and clear those clones during cleanup; retaining a whole `Snapshot` is not sufficient because it preserves unrelated bytes and hides ownership.

## Frozen Files and Interfaces

### Files owned by this child plan

- Create: `pkg/secret/generation_resolver.go` — resolver factory, attempt lifecycle, closure indexing, environment/Vault resolution, bounded zeroing cache, and close-once cleanup.
- Create: `pkg/secret/generation_resolver_test.go` — resolver-specific unit, security, concurrency, cleanup, and redaction tests. Reuse existing same-package fixtures from `pkg/secret/materializer_test.go` where suitable; do not alter that existing test file.

### Files intentionally left for the parent/integration lanes

- Preserve until T9-9: `pkg/store/secret_broker.go`, `pkg/store/consumer_secret.go`, and Store's `secretBroker`, Vault client, and legacy resolution fields.
- Parent bootstrap owns the call from `cmd/root.go`/`pkg/server`: construct one resolver after the validated `data_encryption.Service` exists, transfer it into compiler/materializer/engine ownership, and close it after the generation engine has revoked registrations but before the journal closes.
- Parent cutover owns replacing the transitional Store resolver and deleting Store secret broker fields. This child must not make a compatibility adapter or route both implementations in production.

### Exact public boundary

Add the following concrete methods. Keep the public constructor minimal; package-local tests inject a counting HTTP client through an unexported constructor helper.

```go
type GenerationSecretResolver struct{}

func NewGenerationSecretResolver(
	encryption data_encryption.Service,
) (*GenerationSecretResolver, error)

func (resolver *GenerationSecretResolver) OpenCandidate(
	context.Context,
	AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) (AttemptResolver, error)

func (resolver *GenerationSecretResolver) OpenRecovery(
	context.Context,
	AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) (AttemptResolver, error)

func (resolver *GenerationSecretResolver) Close(context.Context) error
```

The implementation must include these compile-time checks in the production file:

```go
var _ AttemptResolverFactory = (*GenerationSecretResolver)(nil)
var _ AttemptResolver = (*generationSecretAttempt)(nil)
```

The public constructor allocates one resolver-owned `*http.Client`. A private `newGenerationSecretResolver(encryption, client)` helper may accept a package-test client and treats it as transferred ownership, calling `CloseIdleConnections` during resolver close. Do not retain a caller-owned mutable cache, Store, snapshot, or config pointer.

## Exact Data and Security Contract

### Candidate and recovery closure

For each open call, perform validation in this order, before reserving the attempt in the resolver map or making any backend request:

1. Normalize a nil context to `context.Background()` and return its cancellation before any ID or input work.
2. For a candidate, require `generation.ValidatePublicationSet(ticket, set) == nil`, require non-empty `ticket.RequiredDomains`, and require `set.DesiredRevision == ticket.DesiredRevision` through that validator.
3. For recovery, require `generation.ValidateRecoverySet(revisions, published) == nil`. This rejects desired-only recovery and rejects a published map whose domain revision does not equal the supplied fence.
4. Recompute `want := CandidateAttemptID(ticket, set)` or `want := RecoveryAttemptID(revisions, published)`. A zero result is invalid. Compare the 32-byte IDs with `subtle.ConstantTimeCompare(id[:], want[:]) == 1`; a mismatch returns `ErrInvalidCapability` without opening a view.
5. Clone the input ticket/revisions and all candidate/published maps, snapshots, closure slices, decision slices, and resource bytes before releasing the caller's ownership. Never retain caller-owned slices or maps.
6. Build one domain-indexed resource map. For each decision with `DispositionPublished` or `DispositionLastGood`, call the snapshot's `Lookup(decision.Key)` and retain that clone. Do not index quarantined, fail-closed, or deleted resources. The index is limited to resources present in the validated closure; no non-closure snapshot key is retained.
7. Store the attempt generation as `ticket.DesiredRevision` or `revisions.Desired`, and store the exact domain map. The recovery attempt's serving bytes come only from `published`; the desired snapshot is not an argument and cannot be read.

`OpenCandidate` and `OpenRecovery` use the same private constructor after their mode-specific validation. An attempt ID present in either the live map or a draining/quarantined map is rejected with `ErrAttemptAlreadyRegistered`; an ID is reusable only after successful complete cleanup. Candidate and recovery attempts at desired revision 42 therefore coexist because their canonical IDs include different encoding domains and their resource maps are separate.

### Scope and reference checks

`ResolveScoped` must execute all checks below before decrypting or contacting Vault:

```go
func (attempt *generationSecretAttempt) ResolveScoped(
	ctx context.Context,
	scope Scope,
	raw string,
) (string, error)
```

- Validate the existing `Scope` shape with the same rules as `validateScope`: non-zero generation/attempt, HTTP or stream domain, non-empty plugin/resource/field, and one admitted secret source.
- Under the resolver's lookup mutex, find the exact attempt pointer by `scope.Attempt` and then release the resolver mutex; the close path removes the pointer from lookup before cleanup begins. Hold the attempt read gate while checking `closed`, generation, domain, and `scope.Resource` membership.
- Call `data_encryption.ValidateDeclaration(scope.Plugin, scope.Source, scope.Field)` only after the attempt/resource checks. An undeclared field maps to `ErrInvalidScope` through the existing Materializer path.
- For a literal or encrypted declaration, call `data_encryption.ResolveDeclared`; if the resulting value is a managed reference, continue through the same reference parser. Preserve a caller cancellation error and map all other backend/parse/decryption failures to `ErrCredentialUnavailable` without including input bytes.
- Accept `$ENV://NAME` case-insensitively for the prefix. A bare name returns the environment value; `/a/b` traverses only JSON objects and returns a string leaf. Missing variables, malformed JSON, non-object paths, or non-string leaves fail closed and redact the variable name/value from the error.
- Accept only `$secret://vault/<secret-id>/<vault-path>/<field>` with a non-empty manager, ID, path, and field. Reject every manager except `vault`, reject malformed paths, and derive `ResourceKey{Kind: "secrets", ID: "vault/" + secretID}`. Require that exact key in the same attempt and same domain's indexed map. Missing/cross-domain/cross-attempt secret resources return `ErrCapabilityScopeMismatch` before HTTP.
- Decode the retained Vault config bytes, require `uri`, `prefix`, and `token`, resolve an `$ENV://` token using the same environment helper, validate `http`/`https` plus a non-empty host, and construct `/v1/<prefix>/<vault path>` with the repository's existing `path.Join` behavior.
- Use a request context derived from the caller and a default 5-second timeout when the config timeout is non-positive. Check `ctx.Err()` before request construction, before the network call, after the response, after body read, and before returning a value.
- Limit response body reads to `1<<20+1`, zero the body buffer with `defer clear(body)`, accept only 2xx statuses, support both `data[field]` and KV-v2 `data.data[field]`, and never include the response body or plaintext in an error.

### Cache and sensitive-byte rules

Use one bounded cache per attempt so no candidate can receive a value cached under a recovery authority. The cache key must include digests of retained Vault config bytes, the resolved token, the Vault resource ID, and the requested path; do not use plaintext token or secret values as a map key. Cache entries contain a private byte slice, expiry time, sequence, and timer. On replacement, eviction, expiry, attempt close, or factory close, stop the timer and zero the byte slice before dropping the entry. Cache reads return a new string clone. The capacity remains 1024 and TTL remains 60 seconds, matching the transitional broker.

An attempt close obtains its exclusive gate after detaching the attempt from resolver lookup. This waits for every in-flight `ResolveScoped` call, marks the attempt closed, zeros all retained resource bytes, clears the attempt cache, and releases its map entry. A close error must not leave plaintext live; a failed cleanup remains quarantined in the draining map so the exact ID cannot be reopened.

## Task-by-Task TDD Plan

### Task 1: Freeze the resolver constructor and compile-time contracts

**Files:**
- Create: `pkg/secret/generation_resolver.go`
- Test: `pkg/secret/generation_resolver_test.go`

**Interfaces:**
- Consumes: existing `AttemptResolverFactory`, `AttemptResolver`, `AttemptID`, and `data_encryption.Service`.
- Produces: `GenerationSecretResolver`, `NewGenerationSecretResolver`, and the four exact methods above for parent bootstrap and `NewMaterializer`.

- [ ] **Step 1: Write the failing constructor/contract test.**

```go
func TestGenerationSecretResolverImplementsAttemptFactory(t *testing.T) {
	var factory AttemptResolverFactory = newGenerationSecretResolverForTest(t)
	if factory == nil {
		t.Fatal("factory is nil")
	}
}

func TestGenerationSecretResolverRejectsUnconfiguredEncryption(t *testing.T) {
	if _, err := NewGenerationSecretResolver(data_encryption.Service{}); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("constructor error = %v, want ErrInvalidCapability", err)
	}
}

func newGenerationSecretResolverForTest(t *testing.T) *GenerationSecretResolver {
	t.Helper()
	service, _ := testService(t, false)
	resolver, err := NewGenerationSecretResolver(service)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resolver.Close(context.Background()) })
	return resolver
}
```

- [ ] **Step 2: Run the RED test.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/secret -run "^TestGenerationSecretResolver(ImplementsAttemptFactory|RejectsUnconfiguredEncryption)$" -count=1'`

Expected: compile failure naming the missing `NewGenerationSecretResolver`, not a package-wide dependency or unrelated test failure.

- [ ] **Step 3: Add the minimal private state and constructor.**

Use private state equivalent to:

```go
type GenerationSecretResolver struct {
	mu        sync.Mutex
	encryption data_encryption.Service
	client    *http.Client
	attempts  map[AttemptID]*generationSecretAttempt
	draining  map[AttemptID]*generationSecretAttempt
	closed    bool
	closeOnce sync.Once
	closeErr  error
}
```

The constructor rejects `!encryption.Configured()`, creates fresh maps, and creates an owned `http.Client` when the option is nil. It does not open a Store or inspect any snapshot.

- [ ] **Step 4: Run the GREEN test and format only the new files.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint fmt pkg/secret/generation_resolver.go pkg/secret/generation_resolver_test.go && go test ./pkg/secret -run "^TestGenerationSecretResolver(ImplementsAttemptFactory|RejectsUnconfiguredEncryption)$" -count=1'`

Expected: PASS.

### Task 2: Implement exact candidate/recovery admission and closure indexing

**Files:**
- Modify: `pkg/secret/generation_resolver.go`
- Test: `pkg/secret/generation_resolver_test.go`

**Interfaces:**
- Consumes: `generation.ValidatePublicationSet`, `generation.ValidateRecoverySet`, `CandidateAttemptID`, `RecoveryAttemptID`, and `Snapshot.Lookup`.
- Produces: `OpenCandidate` and `OpenRecovery` that return independent `AttemptResolver` instances with exact generation/domain closure ownership.

- [ ] **Step 1: Write RED tests for validation, constant-time identity, cloning, and same-revision overlap.**

Add these tests with real `generation.PublicationSet` fixtures. The fixture must contain a route resource and, for Vault cases, a `secrets/vault/test1` resource in the same domain; it must construct the snapshot with `generation.NewSnapshot`, set the artifact digest to `Snapshot.Digest()`, and include one `DispositionPublished` decision per closure key:

- `TestGenerationSecretResolverOpenCandidateRequiresExactIDAndClosure`: mutate one byte of `CandidateAttemptID`, remove a required-domain candidate, and pass a duplicate closure key; each returns `ErrInvalidCapability` and the counting transport remains at zero.
- `TestGenerationSecretResolverOpenRecoveryRejectsDesiredOnlyOrMismatchedPublished`: pass an empty published map, a non-zero `RevisionSet.HTTP` without a matching HTTP artifact, and a published artifact at the wrong revision; each returns `ErrInvalidCapability`.
- `TestGenerationSecretResolverClonesCandidateInputsAndIndexesOnlyPublishedClosure`: open a candidate, mutate its input maps/slices and snapshot source, then resolve from the retained route and prove a quarantined/non-closure key is rejected.
- `TestRecoverySecretResolverUsesPublishedClosureNotDesired`: construct desired revision 42 with value A and published HTTP revision 40 with value B, pass only the published B map to `OpenRecovery`, and prove recovery resolves B while the candidate at desired revision 42 resolves A. Assert IDs differ and cross-attempt scopes fail before the counting Vault transport is called.
- `TestGenerationSecretResolverAllowsCandidateAndRecoveryAtSameDesiredRevision`: keep both registrations open and prove each attempt resolves only its own same-domain resource bytes.
- `TestGenerationSecretResolverRejectsDuplicateExactAttemptWhileDraining`: block a Vault response, close the first attempt, assert a second open with the exact ID returns `ErrAttemptAlreadyRegistered` until the first close completes, then assert a reopen succeeds.

`TestRecoverySecretResolverUsesPublishedClosureNotDesired` must construct desired revision 42 with secret config/value A, a verified committed HTTP published revision 40 with B, and call `OpenRecovery` with `RevisionSet{Desired: 42, HTTP: 40}` plus only the published B map. Resolve through the recovery scope and assert B. While it is live, open the candidate revision 42 with A, assert the IDs differ, resolve A and B independently, and assert a candidate scope sent to the recovery attempt and a recovery scope sent to the candidate attempt fail before a counting Vault transport receives a request.

- [ ] **Step 2: Run the RED tests.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/secret -run "^(TestGenerationSecretResolver(OpenCandidate|OpenRecovery|Clones|Allows|RejectsDuplicate)|TestRecoverySecretResolverUsesPublishedClosureNotDesired)$" -count=1'`

Expected: compile failure until the open methods and private attempt type exist; once the skeleton exists, behavioral failures must identify validation/indexing mismatches.

- [ ] **Step 3: Add private mode-specific validators and the index builder.**

Implement helpers with concrete responsibilities:

```go
func equalAttemptID(left, right AttemptID) bool
func cloneCandidateForAttempt(generation.PublicationCandidate) generation.PublicationCandidate
func clonePublishedForAttempt(generation.PublishedGeneration) generation.PublishedGeneration
func indexCandidateResources(generation.PublicationCandidate) (map[generation.ResourceKey][]byte, error)
func indexPublishedResources(generation.PublishedGeneration) (map[generation.ResourceKey][]byte, error)
```

`indexCandidateResources` and `indexPublishedResources` must iterate decisions, include only `published`/`last-good`, call `Snapshot.Lookup`, and clear all already-copied bytes if a later allocation or validation fails. The outer open method must clear every domain index if the attempt cannot be registered.

- [ ] **Step 4: Add the exact-ID comparison and attempt registration.**

Validate first, derive the mode-specific ID, compare with `subtle.ConstantTimeCompare`, build cloned domain indexes, then under `resolver.mu` reject `closed`, `attempts[id]`, or `draining[id]` and register one pointer. Return `ErrInvalidCapability` for malformed input/ID and `ErrAttemptAlreadyRegistered` for an occupied exact ID. Do not call Vault during open.

- [ ] **Step 5: Run the GREEN tests and race the input ownership path.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint fmt pkg/secret/generation_resolver.go pkg/secret/generation_resolver_test.go && go test ./pkg/secret -run "^(TestGenerationSecretResolver(OpenCandidate|OpenRecovery|Clones|Allows|RejectsDuplicate)|TestRecoverySecretResolverUsesPublishedClosureNotDesired)$" -count=1'`

Then run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/secret -run "^(TestGenerationSecretResolverClonesCandidateInputsAndIndexesOnlyPublishedClosure|TestRecoverySecretResolverUsesPublishedClosureNotDesired)$" -count=1'`

Expected: PASS, with no access to Store or desired recovery bytes.

### Task 3: Add RED coverage for exact scope and backend reference behavior

**Files:**
- Modify: `pkg/secret/generation_resolver.go`
- Test: `pkg/secret/generation_resolver_test.go`

**Interfaces:**
- Consumes: a live `generationSecretAttempt`, `Scope`, `data_encryption.Service`, and retained domain resources.
- Produces: `generationSecretAttempt.ResolveScoped` with fail-closed authority checks and bounded environment/Vault resolution.

- [ ] **Step 1: Write RED tests before implementing resolution.**

Add concrete tests. Every case uses the same `Scope` fixture and a transport with an atomic request counter:

- `TestGenerationSecretResolverRejectsScopeAuthorityBeforeVault`: alter attempt ID, generation, domain, resource, source, and field one at a time; require a scope error and zero requests for each.
- `TestGenerationSecretResolverRequiresRetainedVaultResourceInSameDomain`: omit `secrets/vault/test1` from the HTTP closure and require `ErrCapabilityScopeMismatch` before transport access.
- `TestGenerationSecretResolverResolvesEnvironmentAndNestedEnvironmentReferences`: set a plain environment value and a JSON object value, resolve `$ENV://NAME` and `$ENV://NAME/path/field`, and assert the two string leaves.
- `TestGenerationSecretResolverResolvesVaultKVv1AndKVv2Responses`: return `{"data":{"password":"v1"}}` and `{"data":{"data":{"password":"v2"}}}` from separate retained URIs and assert both values.
- `TestGenerationSecretResolverRejectsUnsupportedManagerAndMalformedPath`: use a non-Vault manager, missing ID, missing path, and missing field; require no request.
- `TestGenerationSecretResolverUsesAttemptRetainedConfigNotStoreOrGlobalState`: retain URI A, make a separate server/global fixture point at URI B, and assert only URI A receives the request. The test must not import Store; the production absence scan supplies the global proof.
- `TestGenerationSecretResolverErrorsRedactReferencesTokensBodiesAndValues`: exercise malformed environment JSON, a partial Vault body, a non-2xx body, and a missing response field; assert each error omits the raw reference, token, body bytes, and plaintext sentinel.

- [ ] **Step 2: Run the RED tests.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/secret -run "^(TestGenerationSecretResolver(RejectsScope|RequiresRetained|Resolves|RejectsUnsupported|UsesAttempt|ErrorsRedact))$" -count=1'`

Expected: compile or behavioral failures because the attempt has no resolver implementation yet. No test may be weakened to make a missing backend request pass.

### Task 4: Implement scope admission, environment resolution, and Vault access

**Files:**
- Modify: `pkg/secret/generation_resolver.go`
- Test: `pkg/secret/generation_resolver_test.go`

**Interfaces:**
- Consumes: `data_encryption.Service.ValidateDeclaration`/`ResolveDeclared`, per-domain resource indexes, caller context, and the options-owned HTTP client.
- Produces: exact `ResolveScoped` behavior described in the security contract.

- [ ] **Step 1: Implement the attempt gate and exact scope checks.**

Under `attempt.gate.RLock`, reject a closed attempt, generation mismatch, domain mismatch, or resource not present in that domain. Release the factory lookup lock before doing declaration/backend work but retain the read gate until the resolution returns so attempt close waits for the entire operation. Return `ErrCapabilityScopeMismatch` before any data-encryption or network call for authority mismatches.

- [ ] **Step 2: Implement declaration sequencing and redacted error mapping.**

Validate the declaration after authority checks; resolve literal/ciphertext via `data_encryption.ResolveDeclared`; parse the result only if it is a reference. Pass context cancellation unchanged and map all non-context errors to the existing fail-closed secret errors. Do not wrap raw values with `%q` or include reference strings in errors.

- [ ] **Step 3: Port environment reference traversal without Store access.**

Port the traversal semantics of `resolveAttemptEnvironmentSecret` from `pkg/store/secret_broker.go`: prefix match is case-insensitive, the first segment is an environment name, optional later segments walk JSON objects, and the final value must be a string. Use a temporary byte slice for JSON and `defer clear(encoded)`.

- [ ] **Step 4: Port Vault parsing and request behavior against retained bytes.**

Decode the private `vaultSecretConfig` equivalent in `pkg/secret/generation_resolver.go`; read only `attempt.resources[scope.Domain][ResourceKey{Kind: "secrets", ID: "vault/" + id}]`; resolve an environment token from that config; validate URI/host/scheme and key path; issue a bounded request using `attempt`'s resolver-owned client; zero body bytes; parse both response layouts; and cache only the returned string in the current attempt's cache.

- [ ] **Step 5: Run the GREEN behavior and race tests.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint fmt pkg/secret/generation_resolver.go pkg/secret/generation_resolver_test.go && go test ./pkg/secret -run "^(TestGenerationSecretResolver(RejectsScope|RequiresRetained|Resolves|RejectsUnsupported|UsesAttempt|ErrorsRedact))$" -count=1'`

Then run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/secret -run "^(TestGenerationSecretResolver(RejectsScope|RequiresRetained|Resolves|UsesAttempt|ErrorsRedact))$" -count=1'`

Expected: PASS. The Vault request counter must stay zero for every rejected authority/reference case, and all redaction tests must prove that raw references, tokens, partial response bytes, and plaintext values do not occur in errors.

### Task 5: Add RED tests for cache zeroing, revoke, and in-flight close

**Files:**
- Modify: `pkg/secret/generation_resolver.go`
- Test: `pkg/secret/generation_resolver_test.go`

**Interfaces:**
- Consumes: open attempts and `AttemptResolver.Close`.
- Produces: close-safe attempt lifecycle with no new resolution after detach and no reusable ID before cleanup completion.

- [ ] **Step 1: Write lifecycle RED tests.**

Add these tests and retain references to the exact byte slices through same-package test hooks:

- `TestGenerationSecretResolverCacheZeroesEvictedExpiredAndClearedValues`: fill capacity plus one, wait for a short test expiry, and call clear; assert every evicted/expired/cleared backing slice is all zero.
- `TestGenerationSecretResolverAttemptCloseZeroesRetainedAndCachedBytes`: resolve one Vault value, retain references to the config and cache bytes, close the attempt, and assert resources, cache entries, and timers are gone and all captured bytes are zero.
- `TestGenerationSecretResolverAttemptCloseDetachesAndWaitsForInflightResolve`: block the Vault handler, start close, prove a new resolve fails and the close has not returned, release the handler, then assert the in-flight call and close finish without a race.
- `TestGenerationSecretResolverRejectsResolveAfterAttemptClose`: close an attempt and call its resolver again; require a credential-unavailable/closed error and zero additional backend requests.
- `TestGenerationSecretResolverAllowsReopenOnlyAfterSuccessfulCleanup`: close successfully, reopen the exact canonical ID, and prove the new resolver owns fresh bytes rather than the old index.
- `TestGenerationSecretResolverAttemptCloseIsIdempotent`: call the same attempt's close concurrently and repeatedly, assert its cleanup counter is one, and require a new open to remain rejected until the first cleanup has completed.

- [ ] **Step 2: Run the RED lifecycle tests.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/secret -run "^TestGenerationSecretResolver(Cache|AttemptClose|RejectsResolve|AllowsReopen)" -count=1'`

Expected: failures until the cache and attempt close lifecycle is implemented; the in-flight test must not be made non-blocking by removing the gate.

### Task 6: Implement bounded zeroing cache and attempt close

**Files:**
- Modify: `pkg/secret/generation_resolver.go`
- Test: `pkg/secret/generation_resolver_test.go`

**Interfaces:**
- Consumes: resolver maps, `generationSecretAttempt.gate`, and context cleanup semantics.
- Produces: `AttemptResolver.Close` and private cache helpers with idempotent, serialized cleanup.

- [ ] **Step 1: Implement the per-attempt cache.**

Use a private `[32]byte` key, byte-valued entries, expiry timers, and monotonically increasing sequence numbers. On `get`, remove and zero expired entries. On `set`, remove/zero expired entries, replace/zero prior values, evict the oldest sequence over capacity, and schedule a timer whose callback verifies the sequence before zeroing. Return `bytes.Clone` converted to a string; never return the cache's backing slice.

- [ ] **Step 2: Implement `generationSecretAttempt.Close`.**

Use `closeOnce` plus a stored `closeErr`. The first close detaches the exact pointer from `resolver.attempts`, moves it to `resolver.draining`, acquires `attempt.gate.Lock` to wait for readers, marks it closed, zeros all resource bytes and cache entries, then removes it from `draining` after successful cleanup. Use `context.WithoutCancel(normalizeContext(ctx))` for zeroing and map cleanup so cancellation cannot strand sensitive bytes. If a future close-owned resource reports an error, preserve it in `closeErr` and keep the ID quarantined; the current byte/client cleanup operations are deterministic and never expose the underlying token/value.

- [ ] **Step 3: Ensure all resolution paths release the read gate.**

The `ResolveScoped` method must use `defer attempt.gate.RUnlock()` immediately after acquiring the gate. It must not retain a pointer to the attempt after the gate is released. The factory map lock and attempt gate order must be consistent (`resolver.mu` first, then `attempt.gate`) to prevent close/resolve deadlocks.

- [ ] **Step 4: Run the GREEN lifecycle tests and race them.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint fmt pkg/secret/generation_resolver.go pkg/secret/generation_resolver_test.go && go test ./pkg/secret -run "^TestGenerationSecretResolver(Cache|AttemptClose|RejectsResolve|AllowsReopen)" -count=1'`

Then run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/secret -run "^TestGenerationSecretResolverAttemptCloseDetachesAndWaitsForInflightResolve$|^TestGenerationSecretResolverCacheZeroes" -count=1'`

Expected: PASS with deterministic close waiting, zeroed bytes, and no race on resolver maps/cache timers.

### Task 7: Implement factory close-once ownership and ordered cleanup

**Files:**
- Modify: `pkg/secret/generation_resolver.go`
- Test: `pkg/secret/generation_resolver_test.go`

**Interfaces:**
- Consumes: live/draining attempts and the options-owned HTTP client.
- Produces: `GenerationSecretResolver.Close(context.Context) error` for engine/server reverse ownership.

- [ ] **Step 1: Write the factory-close RED tests.**

Add these tests:

- `TestGenerationSecretResolverClosePreventsNewOpensAndClosesAttemptsOnce`: open two attempts, close the factory, assert both attempt cleanup counters and the client close counter are one, and reject a new open with `ErrCredentialUnavailable`/closed state.
- `TestGenerationSecretResolverCloseOrdersAttemptAndClientCleanup`: block an attempt cleanup, begin factory close, assert the client's idle-connection close has not run, release the attempt, and assert the client closes only after all captured bytes are zero.
- `TestGenerationSecretResolverCloseIsIdempotent`: call `Close` concurrently and repeatedly, assert all callers receive the same stored result and no cleanup counter exceeds one.
- `TestGenerationSecretResolverCloseWaitsForInflightAttemptBeforeClientClose`: block one request, begin factory close, assert client idle-connection close has not run, release the request, then assert attempt cleanup precedes client cleanup.

- [ ] **Step 2: Run the RED tests.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/secret -run "^TestGenerationSecretResolverClose" -count=1'`

Expected: failures until factory state and close-once ownership exist.

- [ ] **Step 3: Implement `GenerationSecretResolver.Close`.**

The first invocation sets `closed` under `resolver.mu`, snapshots all live and draining attempt pointers, and prevents all future opens. It then closes every attempt using a non-cancelled cleanup context, waits for any already-draining attempt, clears the resolver attempt maps, and calls `client.CloseIdleConnections()` after attempt cleanup. The current owned HTTP client has no error-returning close operation, so this method returns nil after deterministic cleanup; if a later owned resource gains an error-returning close operation, aggregate it with `errors.Join` and preserve `errors.Is` at this boundary. A repeated or concurrent call waits for the first `closeOnce` operation and returns the same stored result without closing attempts or the client twice.

- [ ] **Step 4: Run the GREEN lifecycle gate.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint fmt pkg/secret/generation_resolver.go pkg/secret/generation_resolver_test.go && go test ./pkg/secret -run "^TestGenerationSecretResolverClose" -count=1'`

Then run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/secret -run "^TestGenerationSecretResolverClose" -count=1'`

Expected: PASS. No close call may race an open, resolve, or another close; client cleanup occurs after every attempt has been revoked/zeroed.

### Task 8: Run the complete child-lane verification and absence gates

**Files:**
- Modify: `pkg/secret/generation_resolver.go`
- Test: `pkg/secret/generation_resolver_test.go`

**Interfaces:**
- Consumes: all previous tasks.
- Produces: a reviewed resolver child branch that parent can merge into the Task 9 integration base without any Store production dependency.

- [ ] **Step 1: Run the focused complete tests.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/secret -count=1'`

Expected: all existing `pkg/secret` tests and all new resolver tests pass. If an existing test fails, record its exact package/file/line and determine whether the new resolver changed a shared interface before handing off; do not report the package as passing.

- [ ] **Step 2: Run the resolver race gate.**

Run: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/secret -run "^(TestGenerationSecretResolver|TestRecoverySecretResolver|TestCandidateAndRecoverySameRevision|TestDuplicateAttempt)" -count=1'`

Expected: PASS with no race in resolver maps, attempt gates, cache expiry, or close-once state.

- [ ] **Step 3: Run formatting, lint, and absence scans.**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint fmt pkg/secret/generation_resolver.go pkg/secret/generation_resolver_test.go'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/secret/...'
! rg -n '(store|Store)[.]|GetStore|GetFromBucket|ResolveSecretReference|MaterializeSecret|last-good|streamRouteLastGood' pkg/secret/generation_resolver.go
! rg -n 'RecoveryState|[.]Desired' pkg/secret/generation_resolver.go
git diff --check
```

Expected: formatting/lint/diff checks pass; both `rg` commands print no matches. `pkg/secret` may depend on `generation` and `data_encryption`, but it must not depend on `store` or any recovery desired snapshot.

- [ ] **Step 4: Inspect the final child diff and hand off without committing.**

Run: `git status --short && git diff -- pkg/secret/generation_resolver.go pkg/secret/generation_resolver_test.go`

Expected: only the two child-owned files are changed/created; four user-owned untracked review documents in the main checkout are untouched. The parent integration owner must review the API signature and merge this lane before production bootstrap work starts.

## Store-Broker Migration Map

Use this table during implementation and review so no behavior is lost or accidentally reintroduced through Store:

| Transitional source | New owner | Required change | Explicit non-change |
| --- | --- | --- | --- |
| `storeSecretBroker` maps and `closed` | `GenerationSecretResolver.attempts`, `draining`, `closed`, `closeOnce` | Add resolver-owned lifecycle and close-once semantics | Do not retain Store lifecycle locks or `Store.stopped` checks |
| `Store.AuthorizeCandidate` | `GenerationSecretResolver.OpenCandidate` | Validate full candidate, recompute constant-time ID, clone/index closure | Do not accept an unvalidated set or read a Store bucket |
| `Store.AuthorizeRecovery` | `GenerationSecretResolver.OpenRecovery` | Validate exact revision/published map and use only published snapshots | Do not accept `RecoveryState.Desired` |
| `Store.ResolveScoped` | `generationSecretAttempt.ResolveScoped` | Exact scope/closure check, declaration resolution, reference backend | Do not look up `scope.Resource` or secret config through Store |
| `Store.RevokeAttempt` | `generationSecretAttempt.Close` | Detach, wait for readers, zero, remove/quarantine | Do not make close a no-op after the first call |
| `retainedCandidateResources`/`retainedRecoveryResources` | `indexCandidateResources`/`indexPublishedResources` | Retain only published/last-good bytes from validated closure | Do not retain quarantined/deleted/non-closure values |
| `resolveAttemptEnvironmentSecret` | private environment helper in `pkg/secret` | Preserve case-insensitive `$ENV://` and nested JSON semantics with zeroing | Do not expose env names or plaintext in errors |
| `resolveAttemptVaultSecret` | `generationSecretAttempt.resolveVault` | Use retained `secrets/vault/<id>` config, context timeout, 1 MiB body bound, KV-v1/KV-v2 parsing | Do not call `GetFromBucket`, `ResolveSecretReference`, or a global client |
| `storeAttemptSecretCache` | private per-attempt cache | Preserve 1024 capacity/60s TTL and zero on every eviction/expiry/close | Do not share plaintext cache entries between attempts |
| `Store.vaultHTTPClient`/`vaultMu` | resolver-owned client created by the public constructor or transferred to the private test constructor | Close idle connections after all attempts | Do not close the client before attempt cleanup |
| `closeSecretBroker` | `GenerationSecretResolver.Close` | Prevent opens, close attempts, then client/cache, preserve cleanup errors | Do not bypass attempt close or discard cleanup errors |

## Parent-Lane Handoff and Integration Dependencies

The parent may merge this child only after the following facts are recorded:

1. The compiler/materializer lane can call `NewMaterializer(encryption, resolver)` and receive `AttemptRegistration` objects whose `Close` is the only revocation path. The resolver must not be exposed as a `ScopedAttemptBroker` compatibility object.
2. The GenerationEngine lane treats `AttemptRegistration.Close` as the resource-release action for candidate discard, activation rollback, finalize retirement, and engine close. It must not call Store or call `resolver.Close` per generation.
3. The bootstrap lane owns the resolver after construction. Its failure cleanup order is engine/materializer registrations first, resolver factory second, journal third; repeated close returns the first stored cleanup result.
4. The provider/recovery lane passes only `RevisionSet` plus verified `Published` map to `RegisterRecovery`; it must not pass desired bytes or use an uncommitted/failed publication as a recovery secret view.
5. The T9-9 deletion lane removes Store's transitional broker only after all server/compiler callers use this resolver and the absence guard `! rg -n 'store[.](MaterializeSecret|ResolveSecretReference)[(]' pkg/compiler pkg/route pkg/plugin pkg/server pkg/stream --glob '*.go' --glob '!*_test.go'` passes.
6. Same-revision overlap remains a required integration property: recovery B may stay active while candidate A is prepared/rolled back, and each generation's consumers/plugins must keep the matching `AttemptID` in their `Scope`.

## Completion Criteria

The secret resolver child is complete only when:

- `GenerationSecretResolver` implements the exact frozen factory API and has no Store import or global resource lookup.
- Candidate opens use only validated `PublicationSet` closure bytes; recovery opens use only validated published generations and never desired state.
- IDs are recomputed with mode-specific canonical encodings and compared in constant time; same desired revision candidate/recovery attempts overlap without aliasing.
- Scope mismatch, missing resource, missing same-domain Vault config, malformed manager/path, cancellation, backend failure, and post-close resolution all fail closed with redacted errors and no unauthorized backend request.
- Attempt cleanup waits for in-flight resolution, zeroes retained/cache/body bytes, prevents premature ID reuse, and supports safe reopen only after successful cleanup.
- Factory close blocks new opens, closes all attempts before its client/cache, preserves any error from an owned cleanup operation, and returns the same result for repeated/concurrent close calls.
- The exact test names and focused/race commands in Tasks 1-8 pass; `golangci-lint run ./pkg/secret/...`, direct Store/desired absence scans, and `git diff --check` pass.
- The final child diff contains only `pkg/secret/generation_resolver.go` and `pkg/secret/generation_resolver_test.go`; no commit is made by this child lane.

## Self-Review Checklist

- **Spec coverage:** constructor/ownership is Task 1; exact candidate/recovery closure and same-revision overlap are Task 2; scope/reference security is Tasks 3-4; zeroing/attempt lifecycle is Tasks 5-6; factory close/error semantics are Task 7; package/race/absence gates and parent handoff are Task 8.
- **Placeholder scan:** no step depends on an unnamed helper, generic “add validation” instruction, or unspecified test. Every implementation step names a file, function/field responsibility, test name, command, and expected result.
- **Type consistency:** the constructor options and methods match the frozen `AttemptResolverFactory` signatures; `generationSecretAttempt` implements `AttemptResolver`; `AttemptRegistration` remains the Materializer-owned lifecycle wrapper; `GenerationSecretResolver.Close` is separate from per-attempt `Close`.
- **Security review:** no desired recovery snapshot enters the API; no Store/global lookup exists; cross-domain and cross-attempt secret targets are checked before HTTP; body/cache/resource bytes are zeroed; errors are redacted.
- **Ownership review:** parent engine closes registrations first, parent server closes the resolver factory next, and only then the journal. This child does not delete or alter the transitional Store bridge.

Plan complete and saved to `docs/superpowers/plans/2026-08-25-task9-secret-resolver.md`. Execution belongs to the parent Task 9 integration workflow using the frozen worktree waves and review checkpoints in `2026-08-25-immutable-task9-joint-cutover.md`.
