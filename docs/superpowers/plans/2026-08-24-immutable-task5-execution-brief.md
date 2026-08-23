# Immutable Task 5 Execution Brief

**Goal:** Make immutable compiler normalization, validation and domain-closure construction executable without Store reads, secret materialization, resource acquisition or unowned generation tasks.

**Authority:** This brief is the canonical execution contract for the resource-taxonomy prerequisite and Immutable Task 5. Where it conflicts with Task 5 or the Wave C wording in `2026-08-23-immutable-compiler-plugin-runtime.md` and `2026-08-24-journal-immutable-cutover-reorder.md`, this brief wins. Later task contracts remain unchanged except for the explicit deferrals below.

## Decisions

1. Task 5 covers every managed desired kind: `routes`, `services`, `upstreams`, `global_rules`, `plugin_configs`, `plugin_metadata`, `consumers`, `consumer_groups`, `plugins`, `protos`, `ssls`, `stream_routes` and `secrets`.
2. Resource kind/domain classification moves to `pkg/generation` before the compiler is created. Etcd and Store callers consume it in the same prerequisite change; no second editable list is added to `pkg/compiler`.
3. Task 5 resolves structural references and explicit `$secret://manager/id/...` resource references. Field-level encryption/secret declarations and plugin-metadata admission remain Task 6 responsibilities because the current manifest has no enumerable declaration contract and metadata schemas require plugin instances.
4. `WorkerCompilerFactory` moves to Task 6. A successful factory-created generation cannot exist before `PreparedGeneration.Close` owns its `TaskRegistry`.
5. Cluster acquisition moves to Task 7. Task 5 remains pure and does not call `RuntimeDependencies.Secrets`, `Resources` or `Tasks`.
6. Normalization keeps raw bytes, an exact-number generic document and a typed structural view. Typed resource values alone are not canonical because `resource.Route.UnmarshalJSON` performs nested decoding that can convert exact numbers to `float64`.
7. A missing predecessor is represented by a failed map lookup, never by a zero `generation.PublishedGeneration`.

## Dependency Order

```text
                 C0 canonical managed-resource taxonomy
                              |
Wave B RuntimeDependencies ---+---> Task 5 pure publication preparation
                                         |
Wave A4 Descriptor ---------------------->+---> Task 6 materialization + atomic worker factory
                                                   |
                                                   v
                                  Task 7 leased clusters + immutable HTTP runtime
```

## C0: Canonical Managed-Resource Taxonomy

**Files:**

- Create: `pkg/generation/resource_kinds.go`
- Create: `pkg/generation/resource_kinds_test.go`
- Modify: `pkg/etcd/watcher.go`
- Modify: focused `pkg/etcd` tests
- Modify: `pkg/config/standalone.go`
- Modify: focused `pkg/config` standalone tests
- Modify: `pkg/store/store.go`
- Modify: focused `pkg/store` tests

**Contract:**

```go
func ManagedResourceKinds() []string
func IsManagedResourceKind(string) bool
func DomainsForResourceKind(string) []Domain
```

`ManagedResourceKinds` returns a sorted defensive copy. `DomainsForResourceKind` returns a normalized defensive copy and the exact current mapping:

- stream only: `stream_routes`;
- HTTP and stream: `services`, `upstreams`, `secrets`;
- HTTP only: every other managed kind.

Unknown kinds return no domains and are not managed. Replace `isManagedEtcdBucket`, both etcd and standalone `desiredDomains` implementations, Store managed membership and the editable Store bucket list with this contract while preserving nested secret-key and singleton `/plugins` parsing.

Do not replace `Store.IsHTTPRouteReloadBucket` or `Store.IsStreamReloadBucket`. Those functions describe the legacy mutable runtime's rebuild triggers, not publication domains: consumers/groups publish in HTTP without rebuilding the route handler, and secrets currently rebuild HTTP but not stream. Preserve their existing behavior and tests until the joint cutover deletes them. Likewise keep `standaloneBuckets` as the explicit 12-kind standalone document-format subset; singleton `plugins` remains provider-managed but is not a standalone document bucket.

**Tests first:**

- sorted defensive managed-kind copy;
- exact 13-kind membership;
- exact domain mapping and defensive copies;
- `TestStandaloneBucketsExcludeSingletonPlugins` locks the standalone document subset at 12 kinds while the canonical managed set still includes singleton `plugins`;
- `TestRouteReloadBucketSemantics` remains green and proves C0 did not equate publication domains with legacy reload impact;
- existing etcd key normalization, desired-domain, standalone provider and standalone Store snapshot tests remain green;
- direct absence scan rejects a second provider-domain classifier or managed-membership list in etcd, standalone config and Store while allowing the documented standalone-format subset and legacy reload-impact functions.

**Verification:**

```bash
bash -lc 'source .envrc && GOFLAGS=-mod=readonly go test -race ./pkg/generation ./pkg/etcd ./pkg/config ./pkg/store -run "^(TestManagedResourceKinds.*|TestDomainsForResourceKind.*|TestDesiredBatchFromEtcd.*|TestCanonicalEtcdPrefixAndManagedKeyShapes|TestDesiredBatchFromStandaloneUsesContentDigestCursor|TestDesiredBatchFromStandaloneSortsMutationsAndConservativelyRequiresDomains|TestStandaloneBucketsExcludeSingletonPlugins|TestStandaloneBaselineSnapshot.*|TestRouteReloadBucketSemantics)$" -count=1'
bash -lc 'source .envrc && GOFLAGS=-mod=readonly make build'
git diff --check
```

**Commit:** `refactor(generation): centralize managed resource taxonomy`

## Task 5: Pure Compiler Phases

**Files:**

- Create: `pkg/compiler/types.go`
- Create: `pkg/compiler/compiler.go`
- Create: `pkg/compiler/compiler_test.go`
- Create: `pkg/compiler/normalize.go`
- Create: `pkg/compiler/normalize_test.go`
- Create: `pkg/compiler/validate.go`
- Create: `pkg/compiler/validate_test.go`
- Create: `pkg/compiler/closure.go`
- Create: `pkg/compiler/closure_test.go`

Do not create `worker_factory.go`, modify `pkg/resource`, call Store parsing methods, use `store.PublishedView`, initialize plugins, materialize secrets, acquire resources or start tasks.

### Step 1: Lock input and journal contracts with failing tests

Add tests for ticket/snapshot revision and digest mismatch, a non-empty required-domain ticket missing a candidate, a valid empty required-domain ticket producing an empty set, invalid dependencies rejected before any runtime dependency call, deterministic candidate ordering and a produced candidate accepted by the real journal Stage contract.

The compiler consumes:

```go
func New(*capability.Manifest, runtime.RuntimeDependencies) (*Compiler, error)
func (c *Compiler) PreparePublication(
    context.Context,
    generation.ApplyTicket,
    generation.Snapshot,
    map[generation.Domain]generation.PublishedGeneration,
) (generation.PublicationSet, error)
```

`New` validates and stores the manifest/dependencies, but Task 5 `Prepare` treats `Secrets`, `Resources` and `Tasks` as forbidden capabilities. Spy implementations must prove zero calls.

### Step 2: Normalize every managed kind without side effects

For every desired resource:

1. clone and retain raw bytes;
2. decode an exact generic document with `pkg/json.Decoder.UseNumber()`;
3. decode only the typed structural view needed for IDs and dependency fields;
4. reject duplicate typed IDs, kind/embedded-ID mismatches, malformed singleton `plugins`, malformed nested secret IDs and unsupported kinds;
5. retain issues as stable `(ResourceKey, code, error)` values instead of stopping unrelated resource normalization;
6. sort resources, tombstones and issues canonically.

Do not feed the typed view back into publication bytes. Candidate values are the original cloned raw bytes.

### Step 3: Validate structural dependencies

Build stable graph edges for:

- route to effective service/upstream/plugin config;
- service to upstream;
- stream route to service/upstream;
- consumer to consumer group;
- upstream and traffic-split inline upstream to client SSL;
- traffic-split weighted upstream references;
- grpc-transcode to proto;
- explicit `$secret://manager/id/...` to `secrets/manager/id`.

Preserve current upstream precedence:

```text
route inline > route upstream_id > service inline > service upstream_id
```

Do not create an edge to a lower-precedence reference that cannot be selected. Sort and compact adjacency lists; detect cycles with deterministic three-color DFS and stable `dependency-cycle` decisions.

### Step 4: Decide disposition with predecessor presence

For each required domain, distinguish `(previous, found)` from a zero published value. Apply:

- explicit tombstone -> `deleted`, never last-good;
- valid resource -> `published`;
- invalid security-sensitive resource with same-domain predecessor value -> `last-good`;
- first invalid security-sensitive resource -> `fail-closed`;
- other invalid resource -> `quarantined`.

After the first disposition pass, construct each domain's effective view from the exact bytes selected for `published` and `last-good`. Re-decode those final bytes, rebuild the structural graph and recompute every route/stream-route closure from that effective view. If a predecessor value selected for last-good cannot be decoded, fail closed. A required unavailable dependency quarantines the owner with `dependency-unavailable`; no candidate may retain a reference to an absent required value or to a dependency that existed only in rejected desired bytes.

Task 5 classifies `routes`, `services`, `global_rules`, `plugin_configs`, `plugin_metadata`, `plugins`, `ssls`, `secrets`, `consumers`, `consumer_groups` and `stream_routes` as resource-level security-sensitive. `upstreams` and `protos` are quarantinable unless the owning closure becomes unavailable. This is the closure-aware compiler form of the already accepted policy; do not reintroduce the superseded temporary `securitySensitiveResource` production helper. Task 6 adds field-level secret and metadata decisions before materialization.

### Step 5: Build canonical domain candidates

Create exactly one candidate for every `ticket.RequiredDomains` entry. Each candidate must satisfy the existing journal validator:

- artifact revision, digest and snapshot ID equal the domain candidate snapshot;
- closure contains every domain-relevant key exactly once;
- every closure key has exactly one matching decision;
- published/last-good values appear in the candidate snapshot;
- deleted values appear only as tombstones;
- quarantined/fail-closed values do not appear in the candidate snapshot.

`ticket.DesiredDigest` identifies the full desired snapshot and is not reused as a domain candidate digest.

### Step 6: Verification and review

```bash
bash -lc 'source .envrc && GOFLAGS=-mod=readonly go test -race ./pkg/compiler -run "^(TestNormalize|TestValidate|TestDependencyClosure|TestDisposition|TestCompiler)" -count=1'
bash -lc 'source .envrc && GOFLAGS=-mod=readonly go test ./pkg/generation ./pkg/store -run "^(TestNewSnapshotCanonicalizesResourcesAndTombstones|TestSnapshotDigestIsIndependentOfInputOrder|TestJournalStageRejectsTicketDomainCandidateAndClosureViolations|TestJournalStageCanonicalizesOrderAndDefensivelyCopies|TestJournalPolicyLastGoodRequiresSameDomainPredecessor|TestJournalPolicyDispositionShapes|TestJournalPolicyRejectsUnknownMixedAndCrossDomainStateAtomically)$" -count=1'
bash -lc 'source .envrc && GOFLAGS=-mod=readonly make build'
git diff --check
```

Independent review must prove there is no Store import/read, no runtime side effect, exact-number bytes are preserved, all managed kinds receive deterministic decisions, predecessor absence is explicit and the real journal accepts the produced candidate.

**Commit:** `feat(compiler): resolve immutable domain closures`

## Explicit Deferrals

- Task 6 owns `WorkerCompilerFactory`, `PreparedGeneration`, generation task shutdown, descriptor-based plugin admission, field-level secret declarations and plugin-metadata requirements. Its factory exposes an atomic `PrepareGeneration(ctx, ticket, snapshot, previous, onFailure) (*PreparedGeneration, error)`: it creates the generation task registry, materializes and prepares, transfers ownership only on success, and stops the registry on every failure. It does not expose a `NewGeneration` method returning an unowned `*Compiler`.
- Task 7 owns `runtime.ResourceRegistry.Acquire` for clusters and every immutable HTTP/TLS/consumer/proto runtime owner.
- The joint cutover deletes the remaining legacy reload/event/bucket APIs after their callers move; it no longer creates the canonical kind/domain contract from scratch.
