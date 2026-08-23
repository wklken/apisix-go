# Durable Desired and Published Generation Journal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the current Store-event persistence and in-memory last-good state with one versioned bbolt journal that durably separates desired state from independently published HTTP and stream generations and acknowledges provider revisions only after an explicit publication decision has committed and runtime ownership has finalized.

**Architecture:** `pkg/generation` defines immutable snapshots, revisions, publication transactions, and the coordinator contract; `pkg/store` implements `generation.Journal` as the single writable bbolt owner and maintains decoded read views of committed published artifacts. Providers submit complete or incremental desired batches to `generation.Coordinator`, which stages a complete dependency-closed publication, reversibly activates it through the runtime engine, commits the published heads, finalizes old-owner retirement, and only then returns an acknowledgement. Existing resource buckets are imported once as desired-only state, then deleted together with the Store event/hook path so there is no dual persistence model or legacy adapter.

**Tech Stack:** Go 1.26, `context`, `crypto/sha256`, `encoding/binary`, `encoding/json`, bbolt v1.5.0, existing APISIX resource decoders, etcd client v3, fsnotify.

**Spec:** `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`

## Global Constraints

- Compatibility target is Apache APISIX 3.17.0 at commit `9ef2ecab67f652d38365049613610ef649bb4ad0`.
- Preserve the APISIX namespace; version Go-native extensions separately.
- Source `.envrc` before every Go or Make command.
- Use impact-scoped tests; do not run `go test ./...`, `go test ./pkg/...`, or `make test` unless the affected infrastructure makes narrower proof impossible.
- Run focused race tests for concurrency-sensitive changes and `source .envrc && make build` for code changes.
- Do not add dependencies when the standard library or an existing project dependency supplies the required behavior.
- The supervisor is the single writer for desired and published state; until the supervisor plan lands, `server.Server` owns the sole writable `generation.Journal` and transfers that ownership without changing the interface.
- HTTP and stream have independent published revisions, artifacts, readiness, and rollback state.
- Explicit delete is authoritative and cannot use last-good; security-sensitive invalid resources use last-good only when a published predecessor exists, otherwise that resource fails closed.
- A provider update is acknowledged only after desired state is durable, every required domain has a committed publish, last-good, quarantine, deletion, or fail-closed decision, and runtime activation has finalized predecessor ownership.
- Do not retain temporary legacy adapters, old bbolt resource buckets, Store events, acknowledged hooks, package-level stream last-good state, or proxy-only facades after the runtime cutover task.
- Keep the four existing untracked files under `docs/reviews/` outside implementation commits unless the project owner separately authorizes them.

---

## File and Responsibility Map

**Create:**

- `pkg/generation/types.go` — shared `Domain`, revisions, provider batch, publication, acknowledgement, and recovery value types.
- `pkg/generation/snapshot.go` — immutable sorted snapshots, cloning, canonical encoding, digest and resource lookup.
- `pkg/generation/journal.go` — the `generation.Journal` interface and stable journal errors.
- `pkg/generation/coordinator.go` — serialized desired → prepare → stage → reversible activate → commit → finalize → acknowledgement workflow.
- `pkg/generation/snapshot_test.go` — canonical snapshot and deletion tests.
- `pkg/generation/coordinator_test.go` — acknowledgement, partial activation rollback, commit-failure rollback, finalize and retry tests using an in-memory fake journal.
- `pkg/store/journal_schema.go` — bbolt bucket names, schema version, migrations, integrity envelopes and legacy import.
- `pkg/store/journal_apply.go` — desired batch transaction and provider-cursor idempotency.
- `pkg/store/journal_publish.go` — staged publication, policy validation, atomic published-head commit and abort.
- `pkg/store/journal_recovery.go` — restart verification and independently recoverable published domains.
- `pkg/store/journal_schema_test.go` — empty, legacy, older, newer and corrupt database cases.
- `pkg/store/journal_apply_test.go` — desired revision, replacement, explicit deletion and cursor tests.
- `pkg/store/journal_publish_test.go` — closure, domain revision and last-good policy tests.
- `pkg/store/journal_recovery_test.go` — offline restart and partial-domain integrity tests.
- `pkg/store/published_view.go` — decoded immutable HTTP/stream read views backed only by committed artifacts.
- `pkg/store/published_view_test.go` — published-view installation, isolation and restart tests.
- `pkg/server/generation_engine.go` — current runtime implementation of `generation.PublicationEngine`; the next compiler plan replaces its internals without changing the interface.
- `pkg/server/generation_engine_test.go` — prepare/activate/failure tests for HTTP and stream domains.
- `docs/architecture/durable-generation-journal.md` — durable format, transaction state machine, recovery and migration contract.

**Modify:**

- `pkg/store/store.go` — make `Store` the bbolt `generation.Journal` implementation and owner of committed read views; remove the event loop and direct resource-bucket writes.
- `pkg/store/getter.go` — read from installed domain views instead of bbolt resource buckets; remove in-memory HTTP and stream last-good ownership.
- `pkg/store/standalone_snapshot.go` — replace resource-bucket snapshotting with desired-snapshot access, then delete this file in the atomic cutover.
- `pkg/etcd/watcher.go` — translate one etcd response into one `generation.DesiredBatch` and advance the etcd cursor only after coordinator acknowledgement.
- `pkg/etcd/watcher_test.go` — assert revision, quarantine, delete, retry and acknowledgement against the journal.
- `pkg/config/standalone.go` — translate one complete file into one authoritative desired batch identified by file digest.
- `pkg/config/standalone_test.go` — assert replacement, malformed-resource decisions, explicit deletion, digest idempotency and restart behavior.
- `pkg/server/server.go` — open/recover the journal, construct the coordinator, install published views, wire the chosen provider and support offline last-good startup.
- `pkg/server/reload.go` — retain only runtime prepare/activate logic used by `generation_engine.go`; remove event debounce ownership after provider cutover.
- `pkg/server/reload_test.go` — replace Store-hook acknowledgement tests with coordinator publication tests.
- `pkg/server/stream_test.go` — replace process-memory stream last-good tests with durable domain publication and restart tests.
- `pkg/observability/metrics/config_apply.go` — project recovered/offline/provider/domain state without making metrics the state owner.
- `pkg/observability/metrics/config_apply_test.go` — verify offline last-good is serving but not ready.
- `docs/design.md` — point state ownership and reload semantics to the durable journal document.

**Delete during Task 9, in the same commit that switches both providers and the server:**

- `pkg/store/event.go`
- `pkg/store/event_ack_test.go`
- `pkg/store/durable_apply_test.go`
- `pkg/store/standalone_snapshot.go`

The cutover must also remove `NewEvent`, `NewAcknowledgedEvent`, `NewAcknowledgedBatch`, `AddEventUpdateHook`, `AddAcknowledgedEventUpdateHook`, `Sync`, `HTTPConfigGeneration`, `PrepareStreamRoutes`, `CommitStreamRouteLastGood`, `streamRouteLastGood`, `configGeneration`, and all production callers. None of these names may remain as wrappers.

## Shared Interfaces

Every task and every later child plan uses the following exact names. `GenerationArtifact` deliberately matches the program plan; `Snapshot` is the immutable payload referenced by its `Snapshot` identifier.

```go
package generation

type Domain string

const (
	DomainHTTP   Domain = "http"
	DomainStream Domain = "stream"
)

type RevisionSet struct {
	Desired uint64
	HTTP    uint64
	Stream  uint64
}

type GenerationArtifact struct {
	Domain   Domain
	Revision uint64
	Digest   [32]byte
	Snapshot string
}

type ResourceKey struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type Resource struct {
	Key   ResourceKey `json:"key"`
	Value []byte      `json:"value"`
}

type Tombstone struct {
	Key      ResourceKey `json:"key"`
	Revision uint64      `json:"revision"`
}

type Snapshot struct {
	revision   uint64
	resources  []Resource
	tombstones []Tombstone
	digest     [32]byte
}

func (s Snapshot) Revision() uint64
func (s Snapshot) Resources() []Resource
func (s Snapshot) Tombstones() []Tombstone
func (s Snapshot) Digest() [32]byte

type MutationType string

const (
	MutationPut    MutationType = "put"
	MutationDelete MutationType = "delete"
)

type Mutation struct {
	Type  MutationType
	Key   ResourceKey
	Value []byte
}

type ProviderCursor struct {
	Provider string
	Revision string
}

type DesiredBatch struct {
	Cursor          ProviderCursor
	ReplaceManaged  bool
	Mutations       []Mutation
	RequiredDomains []Domain
}

type ApplyTicket struct {
	DesiredRevision uint64
	DesiredDigest   [32]byte
	Cursor          ProviderCursor
	RequiredDomains []Domain
}

type ResourceDisposition string

const (
	DispositionPublished   ResourceDisposition = "published"
	DispositionLastGood    ResourceDisposition = "last-good"
	DispositionQuarantined ResourceDisposition = "quarantined"
	DispositionFailClosed  ResourceDisposition = "fail-closed"
	DispositionDeleted     ResourceDisposition = "deleted"
)

type ResourceDecision struct {
	Key         ResourceKey
	Disposition ResourceDisposition
	Code        string
}

type PublicationCandidate struct {
	Artifact  GenerationArtifact
	Snapshot  Snapshot
	Closure   []ResourceKey
	Decisions []ResourceDecision
}

type PublicationSet struct {
	DesiredRevision uint64
	Domains         map[Domain]PublicationCandidate
}

type PublicationToken string

type PublishedGeneration struct {
	Artifact  GenerationArtifact
	Snapshot  Snapshot
	Closure   []ResourceKey
	Decisions []ResourceDecision
}

type Acknowledgement struct {
	Cursor    ProviderCursor
	Revisions RevisionSet
	Decisions map[Domain][]ResourceDecision
}

type RecoveryFailure struct {
	Domain Domain
	Code   string
}

type RecoveryState struct {
	Revisions RevisionSet
	Desired   Snapshot
	Published map[Domain]PublishedGeneration
	Failures  []RecoveryFailure
}
```

The journal and coordinator boundary is:

```go
package generation

type Journal interface {
	ApplyDesired(context.Context, DesiredBatch) (ApplyTicket, error)
	LoadDesired(context.Context, uint64) (Snapshot, error)
	LoadPublished(context.Context, Domain) (PublishedGeneration, error)
	Stage(context.Context, ApplyTicket, PublicationSet) (PublicationToken, error)
	Commit(context.Context, PublicationToken) (Acknowledgement, error)
	Abort(context.Context, PublicationToken, string) error
	Revisions(context.Context) (RevisionSet, error)
	Recover(context.Context) (RecoveryState, error)
	Close() error
}

type PublicationEngine interface {
	Prepare(
		context.Context,
		ApplyTicket,
		Snapshot,
		map[Domain]PublishedGeneration,
	) (PublicationSet, error)
	DiscardPrepared(context.Context, PublicationSet) error
	Activate(context.Context, PublicationToken, PublicationSet) error
	RollbackActivation(context.Context, PublicationToken, PublicationSet) error
	FinalizeActivation(context.Context, PublicationToken, PublicationSet)
}

type Coordinator struct {
	journal Journal
	engine  PublicationEngine
	mu      sync.Mutex
}

func NewCoordinator(journal Journal, engine PublicationEngine) *Coordinator
func (c *Coordinator) Apply(context.Context, DesiredBatch) (Acknowledgement, error)
```

`Prepare` owns detached candidate resources immediately. If `Stage` fails before a token exists, the coordinator must call idempotent `DiscardPrepared` with `context.WithoutCancel(ctx)` and return `errors.Join(stageErr, discardErr)`; this closes only the unpublished candidate and its new leases. `Activate` may install new owners but must retain the old active owners and every old lease until the journal commit succeeds. `RollbackActivation` is idempotent: it restores the old active owners, closes the new owners and releases only new leases. `FinalizeActivation` performs only an infallible in-memory transition: it makes the new generation authoritative and transfers the predecessor into a retirement queue. It performs no IPC, drain wait, process termination, filesystem operation, or lease close that can report a recoverable failure. The supervisor main loop retires queued predecessors asynchronously; an impossible finalize invariant is a core runtime fatal condition, not a provider acknowledgement error. Plan 04's `compiler.PreparedGeneration` must implement the same prepare/discard/activate/rollback/finalize lease lifecycle.

`GenerationArtifact.Snapshot` is exactly `sha256:<lowercase hex digest>`. It is an identifier, never a filesystem path and never inline configuration. The bbolt implementation stores the canonical payload under that identifier and verifies both the identifier and `Digest` on every recovery read.

---

### Task 1: Define Canonical Generation Values and Snapshot Digests

**Files:**

- Create: `pkg/generation/types.go`
- Create: `pkg/generation/snapshot.go`
- Test: `pkg/generation/snapshot_test.go`

**Interfaces:**

- Consumes: provider resource kind/ID/value triples; no `pkg/store` types.
- Produces: all shared value types above plus `NewSnapshot`, `Revision`, `Resources`, `Tombstones`, `Digest`, `Clone`, `Lookup`, `Deleted`, `CanonicalBytes`, and `SnapshotID`.
- `Snapshot` is structurally immutable outside `pkg/generation`: every field is private, construction clones all input bytes, collection accessors return defensive copies, and no method exposes internal slice storage. Persistence readers decode into a temporary wire value and reconstruct through `NewSnapshot`; they never deserialize directly into `Snapshot`.

- [ ] **Step 1: Write failing tests for deterministic ordering, cloning and tombstones**

```go
func TestNewSnapshotCanonicalizesResourcesAndTombstones(t *testing.T) {
	snapshot, err := NewSnapshot(7, []Resource{
		{Key: ResourceKey{Kind: "routes", ID: "b"}, Value: []byte(`{"id":"b"}`)},
		{Key: ResourceKey{Kind: "routes", ID: "a"}, Value: []byte(`{"id":"a"}`)},
	}, []Tombstone{{Key: ResourceKey{Kind: "services", ID: "gone"}, Revision: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Resources()[0].Key.ID; got != "a" {
		t.Fatalf("first resource = %q, want a", got)
	}
	clone := snapshot.Clone()
	clone.resources[0].Value[0] = 'x'
	if bytes.Equal(clone.resources[0].Value, snapshot.resources[0].Value) {
		t.Fatal("Clone shared resource bytes")
	}
	exposed := snapshot.Resources()
	exposed[0].Value[0] = 'x'
	if got, _ := snapshot.Lookup(exposed[0].Key); got[0] == 'x' {
		t.Fatal("Resources exposed internal bytes")
	}
	if !snapshot.Deleted(ResourceKey{Kind: "services", ID: "gone"}) {
		t.Fatal("explicit tombstone is not visible")
	}
}

func TestSnapshotDigestIsIndependentOfInputOrder(t *testing.T) {
	left, err := NewSnapshot(4, []Resource{
		{Key: ResourceKey{Kind: "routes", ID: "b"}, Value: []byte(`{"id":"b"}`)},
		{Key: ResourceKey{Kind: "routes", ID: "a"}, Value: []byte(`{"id":"a"}`)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewSnapshot(4, []Resource{
		{Key: ResourceKey{Kind: "routes", ID: "a"}, Value: []byte(`{"id":"a"}`)},
		{Key: ResourceKey{Kind: "routes", ID: "b"}, Value: []byte(`{"id":"b"}`)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if left.Digest() != right.Digest() || left.SnapshotID() != right.SnapshotID() {
		t.Fatalf("same snapshot has different identity: %x/%x", left.Digest(), right.Digest())
	}
}
```

- [ ] **Step 2: Run the exact tests and observe the missing API failure**

Run: `bash -lc 'source .envrc && go test ./pkg/generation -run "^(TestNewSnapshot|TestSnapshotDigest)" -count=1'`

Expected: FAIL because `NewSnapshot`, `Resource`, `Tombstone`, `Clone`, `Deleted`, and `SnapshotID` do not exist.

- [ ] **Step 3: Implement the shared types and a sorted canonical snapshot**

```go
func NewSnapshot(revision uint64, resources []Resource, tombstones []Tombstone) (Snapshot, error) {
	result := Snapshot{revision: revision}
	seen := make(map[ResourceKey]struct{}, len(resources)+len(tombstones))
	for _, resource := range resources {
		if resource.Key.Kind == "" || resource.Key.ID == "" {
			return Snapshot{}, fmt.Errorf("resource key requires kind and id")
		}
		if _, exists := seen[resource.Key]; exists {
			return Snapshot{}, fmt.Errorf("duplicate resource %s/%s", resource.Key.Kind, resource.Key.ID)
		}
		seen[resource.Key] = struct{}{}
		result.resources = append(result.resources, Resource{Key: resource.Key, Value: bytes.Clone(resource.Value)})
	}
	for _, tombstone := range tombstones {
		if tombstone.Key.Kind == "" || tombstone.Key.ID == "" || tombstone.Revision == 0 {
			return Snapshot{}, fmt.Errorf("invalid tombstone")
		}
		if _, exists := seen[tombstone.Key]; exists {
			return Snapshot{}, fmt.Errorf("resource and tombstone overlap at %s/%s", tombstone.Key.Kind, tombstone.Key.ID)
		}
		seen[tombstone.Key] = struct{}{}
		result.tombstones = append(result.tombstones, tombstone)
	}
	slices.SortFunc(result.resources, compareResource)
	slices.SortFunc(result.tombstones, compareTombstone)
	encoded, err := result.CanonicalBytes()
	if err != nil {
		return Snapshot{}, err
	}
	result.digest = sha256.Sum256(encoded)
	return result, nil
}

func (s Snapshot) SnapshotID() string {
	digest := s.Digest()
	return "sha256:" + hex.EncodeToString(digest[:])
}
```

`CanonicalBytes` must encode a private struct containing `Revision()`, sorted private resources, and sorted private tombstones; it must not encode `Digest()`. `Resources`, `Tombstones`, `Lookup`, and `Clone` return defensive copies and never expose mutable internal storage. Add regression tests that mutate constructor inputs and every returned slice/byte value, then prove the original canonical bytes, digest, snapshot ID, lookup results, and tombstone membership do not change.

Use these exact ordering helpers so every writer produces the same digest:

```go
func compareResource(left, right Resource) int {
	if byKind := strings.Compare(left.Key.Kind, right.Key.Kind); byKind != 0 {
		return byKind
	}
	return strings.Compare(left.Key.ID, right.Key.ID)
}

func compareTombstone(left, right Tombstone) int {
	if byKind := strings.Compare(left.Key.Kind, right.Key.Kind); byKind != 0 {
		return byKind
	}
	return strings.Compare(left.Key.ID, right.Key.ID)
}
```

- [ ] **Step 4: Run the package tests**

Run: `bash -lc 'source .envrc && go test ./pkg/generation -run "^(TestNewSnapshot|TestSnapshotDigest|TestSnapshotClone|TestSnapshotLookup)" -count=1'`

Expected: PASS.

- [ ] **Step 5: Commit the immutable value model**

```bash
git add pkg/generation/types.go pkg/generation/snapshot.go pkg/generation/snapshot_test.go
git commit -m "feat(generation): define immutable snapshot identities"
```

---

### Task 2: Add the Versioned bbolt Journal Schema and One-Way Legacy Import

**Files:**

- Create: `pkg/generation/journal.go`
- Create: `pkg/store/journal_schema.go`
- Test: `pkg/store/journal_schema_test.go`
- Modify: `pkg/store/store.go:28-218`

**Interfaces:**

- Consumes: `generation.Snapshot`, `generation.Journal`, the exact legacy bucket-name list supplied to `OpenJournal`.
- Produces: `store.OpenJournal(path string, options JournalOptions) (*Store, error)`, idempotent `Store.Close`, schema-backed `Store.Revisions`, and a big-endian desired revision-to-artifact index used by later cursor replay. The returned store owns the only writable `*bolt.DB`; Tasks 3–6 add the remaining `generation.Journal` methods, so Task 2 must not add stub methods or a premature full-interface assertion.

- [ ] **Step 1: Write failing schema, migration and newer-version tests**

```go
func TestOpenJournalImportsLegacyBucketsAsDesiredOnly(t *testing.T) {
	path := seedLegacyDatabase(t, map[string]map[string][]byte{
		"routes": {"r1": []byte(`{"id":"r1","uri":"/"}`)},
	})
	journal, err := OpenJournal(path, JournalOptions{LegacyResourceBuckets: []string{"routes"}})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	revisions, err := journal.Revisions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if revisions != (generation.RevisionSet{Desired: 1}) {
		t.Fatalf("revisions = %+v, want desired-only revision 1", revisions)
	}
	assertBucketMissing(t, journal.db, "routes")
}

func TestOpenJournalRejectsUnknownNewerSchemaWithoutMutation(t *testing.T) {
	path := seedSchemaVersion(t, currentJournalSchemaVersion+1)
	before := readFileDigest(t, path)
	_, err := OpenJournal(path, JournalOptions{})
	if !errors.Is(err, generation.ErrNewerSchema) {
		t.Fatalf("OpenJournal() error = %v, want ErrNewerSchema", err)
	}
	if after := readFileDigest(t, path); after != before {
		t.Fatal("newer-schema database was modified")
	}
}

func seedLegacyDatabase(t *testing.T, contents map[string]map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for name, rows := range contents {
			bucket, err := tx.CreateBucketIfNotExists([]byte(name))
			if err != nil {
				return err
			}
			for key, value := range rows {
				if err := bucket.Put([]byte(key), value); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func seedSchemaVersion(t *testing.T, version uint64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucket(journalMetaBucket)
		if err != nil {
			return err
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], version)
		return bucket.Put([]byte("schema_version"), encoded[:])
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func assertBucketMissing(t *testing.T, db *bolt.DB, name string) {
	t.Helper()
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(name)) != nil {
			t.Fatalf("bucket %q still exists", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func openTestJournal(t *testing.T) *Store {
	t.Helper()
	journal, err := OpenJournal(filepath.Join(t.TempDir(), "journal.db"), JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	return journal
}
```

- [ ] **Step 2: Run the schema tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/store -run "^TestOpenJournal" -count=1'`

Expected: FAIL because `OpenJournal`, `JournalOptions`, schema metadata and `generation.ErrNewerSchema` do not exist.

- [ ] **Step 3: Define the journal interface and stable errors**

```go
var (
	ErrNotFound       = errors.New("generation state not found")
	ErrNewerSchema    = errors.New("generation journal schema is newer than this binary")
	ErrIntegrity      = errors.New("generation journal integrity check failed")
	ErrCursorConflict = errors.New("provider cursor was reused with different content")
	ErrStaleCursor    = errors.New("provider cursor is stale relative to desired head")
	ErrProviderConflict = errors.New("desired provider changed without authoritative replacement")
	ErrNoLastGood     = errors.New("published predecessor required for last-good")
	ErrInvalidClosure = errors.New("publication dependency closure is incomplete")
)
```

Add the exact `Journal` interface from Shared Interfaces to `pkg/generation/journal.go`.

- [ ] **Step 4: Implement schema creation and migration in one bbolt transaction**

```go
const currentJournalSchemaVersion uint64 = 1

var (
	journalMetaBucket         = []byte("generation_meta")
	desiredHeadBucket         = []byte("generation_desired_head")
	desiredRevisionBucket     = []byte("generation_desired_revisions")
	artifactBucket            = []byte("generation_artifacts")
	publishedHeadBucket       = []byte("generation_published_heads")
	publicationTxnBucket      = []byte("generation_publication_transactions")
	providerCursorBucket      = []byte("generation_provider_cursors")
	publicationDecisionBucket = []byte("generation_publication_decisions")
)

type JournalOptions struct {
	LegacyResourceBuckets []string
}

func OpenJournal(path string, options JournalOptions) (*Store, error) {
	if err := validateLegacyBucketNames(options.LegacyResourceBuckets); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: storeOpenTimeout})
	if err != nil {
		return nil, fmt.Errorf("open generation journal %q: %w", path, err)
	}
	storage := &Store{db: db}
	if err := storage.initializeJournal(options.LegacyResourceBuckets); err != nil {
		_ = db.Close()
		return nil, err
	}
	return storage, nil
}
```

`initializeJournal` must first inspect schema state without mutating it. A version greater than `currentJournalSchemaVersion` returns `ErrNewerSchema`; an explicit version `0`, missing required metadata/buckets for version `1`, or journal data buckets without the metadata bucket return `ErrIntegrity`. Version `1` is the first journal schema, so there is no invented version-0 upgrade path.

For a database with no journal metadata or journal data buckets, one update transaction creates every journal bucket and records `schema_version=1` and `integrity_algorithm=sha256`. A genuinely empty database with none of the caller-listed legacy buckets stays at desired/HTTP/stream revision `0` and writes no artifact. A nonempty database with no matching listed legacy bucket fails closed with `ErrIntegrity` rather than silently claiming ownership of an unrelated database. Empty or journal-reserved legacy bucket names also fail closed before opening or creating the database file. If at least one caller-listed legacy bucket exists, even when it has no rows, the same transaction reads all rows from every existing listed bucket into a revision-1 desired `generation.Snapshot`, writes its integrity envelope, revision-index entry and desired head, and deletes only those listed legacy buckets. Unlisted buckets remain untouched. `generation_desired_head` contains either zero keys or exactly the current `revision` and `artifact`; historical mappings live only in `generation_desired_revisions` under exact eight-byte big-endian keys. The index is the complete contiguous range `1..current`; startup rejects gaps, future revisions, malformed IDs and any entry whose canonical artifact revision/digest/ID does not match. Migration does not create an HTTP or stream published head because the old database cannot prove that its current rows were successfully published. Reopening either initialized form is idempotent and never repeats import. A revision argument of `0` to the private loader means the current desired head and revalidates its index mapping; a nonzero revision resolves through the revision index and may load a historical desired artifact.

- [ ] **Step 5: Add exact artifact integrity envelopes**

```go
type artifactEnvelope struct {
	Digest  [32]byte `json:"digest"`
	Size    uint64   `json:"size"`
	Payload []byte   `json:"payload"`
}

func verifyArtifact(id string, envelope artifactEnvelope) error {
	digest := sha256.Sum256(envelope.Payload)
	if uint64(len(envelope.Payload)) != envelope.Size || digest != envelope.Digest ||
		id != "sha256:"+hex.EncodeToString(digest[:]) {
		return generation.ErrIntegrity
	}
	return nil
}
```

`loadDesiredSnapshotTx`, publication staging, and recovery must decode `envelope.Payload` into a private wire struct with exported JSON fields, call `generation.NewSnapshot(wire.Revision, wire.Resources, wire.Tombstones)`, and require the rebuilt `CanonicalBytes`, `Digest()`, and `SnapshotID()` to equal the stored payload, envelope digest, and artifact ID. A direct `json.Unmarshal` into `generation.Snapshot` is forbidden because its private representation is the immutability boundary.

Use eight-byte big-endian integers for schema and revision metadata. Never decode revision data through JSON numbers or `float64`.

- [ ] **Step 6: Run schema and integrity tests**

Run: `bash -lc 'source .envrc && go test ./pkg/store -run "^(TestOpenJournal|TestJournalSchema|TestArtifactEnvelope|TestDesiredRevisionIndex)" -count=1'`

Expected: PASS, including true-empty revision 0, non-empty and empty-bucket legacy import as desired-only revision 1, unlisted-bucket preservation, invalid/reserved legacy names before file creation and nonmatching nonempty databases without mutation, current-head revision-0 and historical revision-index lookup, idempotent reopen, explicit version 0/partial-schema/orphan-artifact/unknown-head-key/malformed-index fail-closed behavior, unknown newer version without mutation and lock release, and payload/size/digest/ID/canonical artifact tampering.

- [ ] **Step 7: Commit the durable format foundation**

```bash
git add pkg/generation/journal.go pkg/store/store.go pkg/store/journal_schema.go pkg/store/journal_schema_test.go
git commit -m "feat(store): add versioned generation journal schema"
```

---

### Task 3: Persist Desired Batches, Provider Cursors and Explicit Tombstones

**Files:**

- Create: `pkg/store/journal_apply.go`
- Test: `pkg/store/journal_apply_test.go`

**Interfaces:**

- Consumes: `generation.DesiredBatch` with one authoritative `ProviderCursor` and explicit required domains.
- Produces: `Store.ApplyDesired`, `Store.LoadDesired`, monotonic `RevisionSet.Desired`, idempotent cursor replay and durable delete tombstones.

- [ ] **Step 1: Write failing tests for incremental PUT/DELETE and authoritative replacement**

```go
func TestJournalApplyDesiredPersistsExplicitDeleteAcrossRestart(t *testing.T) {
	journal := openTestJournal(t)
	ctx := context.Background()
	_, err := journal.ApplyDesired(ctx, generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "41"},
		Mutations: []generation.Mutation{{
			Type: generation.MutationPut,
			Key: generation.ResourceKey{Kind: "routes", ID: "r1"},
			Value: []byte(`{"id":"r1"}`),
		}},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := journal.ApplyDesired(ctx, generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "42"},
		Mutations: []generation.Mutation{{
			Type: generation.MutationDelete,
			Key: generation.ResourceKey{Kind: "routes", ID: "r1"},
		}},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := journal.LoadDesired(ctx, ticket.DesiredRevision)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Deleted(generation.ResourceKey{Kind: "routes", ID: "r1"}) {
		t.Fatal("delete was not persisted as a tombstone")
	}
}
```

Add tests that a complete replacement tombstones absent resources, that an identical cursor+digest returns the original ticket without incrementing Desired, and that the same cursor with different mutations returns `ErrCursorConflict`. Preserve mutation order and add same-key `PUT → DELETE` / `DELETE → PUT` tests: etcd watch order is semantic and last mutation wins. Also cover restart replay after newer desired revisions exist, collision-free cursor identity, provider switching, empty-batch rules, tombstone revision refresh, input/output defensive copies, overflow, canceled context, artifact/cursor tampering, and a coherent rollback of both the active-authority record and its keyed cursor record.

- [ ] **Step 2: Run the desired-state tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/store -run "^TestJournalApplyDesired" -count=1'`

Expected: FAIL because `ApplyDesired` and `LoadDesired` do not exist.

- [ ] **Step 3: Implement immutable desired snapshots and cursor idempotency**

```go
func (s *Store) ApplyDesired(ctx context.Context, batch generation.DesiredBatch) (generation.ApplyTicket, error) {
	batch = cloneDesiredBatch(batch)
	if err := validateDesiredBatch(batch); err != nil {
		return generation.ApplyTicket{}, err
	}
	if err := contextErr(ctx); err != nil {
		return generation.ApplyTicket{}, err
	}
	var ticket generation.ApplyTicket
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		batchDigest, err := digestDesiredBatch(batch)
		if err != nil {
			return err
		}
		current, err := loadDesiredSnapshotTx(tx, 0)
		if err != nil && !errors.Is(err, generation.ErrNotFound) {
			return err
		}
		activeProvider, err := loadActiveProviderTx(tx, current)
		if err != nil {
			return err
		}
		if replay, recordedDigest, found, err := loadCursorTx(tx, batch.Cursor); err != nil {
			return err
		} else if found {
			if recordedDigest != batchDigest {
				return generation.ErrCursorConflict
			}
			if activeProvider != batch.Cursor.Provider {
				return generation.ErrProviderConflict
			}
			if replay.DesiredRevision != current.Revision() {
				return generation.ErrStaleCursor
			}
			if replay.DesiredDigest != current.Digest() {
				return generation.ErrIntegrity
			}
			ticket = replay
			return nil
		}
		if activeProvider != "" && activeProvider != batch.Cursor.Provider && !batch.ReplaceManaged {
			return generation.ErrProviderConflict
		}
		if current.Revision() == math.MaxUint64 {
			return fmt.Errorf("desired revision overflow")
		}
		next, err := applyBatch(current, batch)
		if err != nil {
			return err
		}
		ticket = generation.ApplyTicket{
			DesiredRevision: next.Revision(),
			DesiredDigest: next.Digest(),
			Cursor: batch.Cursor,
			RequiredDomains: normalizeDomains(batch.RequiredDomains),
		}
		if err := contextErr(ctx); err != nil {
			return err
		}
		return persistDesiredTx(tx, next, ticket, batchDigest)
	})
	if err != nil {
		return generation.ApplyTicket{}, err
	}
	return cloneApplyTicket(ticket), nil
}
```

`applyBatch` must clone all input bytes and return a `NewSnapshot(current.Revision()+1, resources, tombstones)` result; it never mutates `current`. `ReplaceManaged=true` first turns every current resource absent from the authoritative input into a tombstone at the new revision, then applies mutations in their original order. `MutationDelete` removes the value and writes or refreshes a tombstone at the new revision; a later `MutationPut` clones the value and clears that key's tombstone. Existing tombstones not replaced by a PUT remain durable.

`ReplaceManaged` is global authority over the complete APISIX desired namespace because one process has exactly one configured provider. The first batch on an empty journal establishes its provider; a different provider is accepted only with `ReplaceManaged=true`, which atomically transfers authority. Every replacement requires normalized domains `[http, stream]`, including an empty replacement, so deletion of either runtime cannot remain unpublished. A non-replacement batch with mutations requires at least one domain. An empty non-replacement batch with empty domains is a legal cursor-only revision; an explicit domain on an empty non-replacement batch is a legal forced recompilation.

Provider names, cursor revisions, resource keys and domains must be non-empty valid UTF-8. Reject unknown mutation types/domains and a delete carrying non-empty value bytes. Required domains are de-duplicated into stable `http`, `stream` order. Repeated keys inside a batch are legal and remain ordered. `nil` and empty PUT values remain distinct desired bytes and receive golden digest coverage; later compiler policy decides whether either payload is valid. The schema-v1 batch digest and cursor payload must use private wire DTOs with explicit JSON tags; never marshal `generation.ProviderCursor` or `generation.ApplyTicket` directly, because unrelated changes to their exported Go fields must not alter persisted bytes. Lock both canonical payloads with readable JSON golden tests.

`providerCursorBucket` stores a fixed active-provider record plus cursor records keyed by a collision-resistant SHA-256 of the canonical provider/revision pair; every record embeds and revalidates the exact cursor, ordered batch digest and complete original ticket. The active record must equal its keyed cursor record and its ticket revision/digest must equal the already-loaded current desired snapshot, so a coherent rollback of both cursor records still fails closed. Same cursor plus the same canonical batch returns the original ticket only while that ticket is the current desired head and its provider is still active, including after restart, so a failed current publication can be retried. Once the desired head advances, replaying the older cursor returns `ErrStaleCursor` without loading, preparing or publishing the historical snapshot. A cursor from a non-active provider returns `ErrProviderConflict`; even an old replacement record cannot transfer authority again. Different mutation order, type, bytes, replacement flag or normalized domains returns `ErrCursorConflict`. Cursor persistence records ingestion/deduplication only, never provider acknowledgement: Tasks 8–9 advance etcd acknowledgement state only after `Coordinator.Apply` succeeds.

Implement the private boundary with these exact signatures; `digestDesiredBatch` canonicalizes cursor, replacement flag, mutations in original order, and normalized required domains without reading current journal state. Standalone translation is already deterministically sorted before this boundary, while etcd watch translation retains server event order:

```go
func validateDesiredBatch(generation.DesiredBatch) error
func digestDesiredBatch(generation.DesiredBatch) ([32]byte, error)
func loadActiveProviderTx(*bolt.Tx, generation.Snapshot) (string, error)
func loadCursorTx(*bolt.Tx, generation.ProviderCursor) (generation.ApplyTicket, [32]byte, bool, error)
func loadDesiredSnapshotTx(*bolt.Tx, uint64) (generation.Snapshot, error)
func applyBatch(generation.Snapshot, generation.DesiredBatch) (generation.Snapshot, error)
func normalizeDomains([]generation.Domain) []generation.Domain
func persistDesiredTx(*bolt.Tx, generation.Snapshot, generation.ApplyTicket, [32]byte) error
func contextErr(context.Context) error
func cursorRecordKey(generation.ProviderCursor) []byte
func encodeCursorRecord(cursorRecord) ([]byte, error)
func decodeCursorRecord([]byte, *generation.ProviderCursor) (cursorRecord, error)
func validateProviderCursor(generation.ProviderCursor) error
func providerCursorToWire(generation.ProviderCursor) providerCursorWire
func providerCursorFromWire(providerCursorWire) generation.ProviderCursor
func applyTicketToWire(generation.ApplyTicket) applyTicketWire
func applyTicketFromWire(applyTicketWire) generation.ApplyTicket
func cursorRecordToWire(cursorRecord) cursorRecordWire
func cursorRecordFromWire(cursorRecordWire) cursorRecord
func cloneDesiredBatch(generation.DesiredBatch) generation.DesiredBatch
func cloneApplyTicket(generation.ApplyTicket) generation.ApplyTicket
func cloneBytes([]byte) []byte
```

- [ ] **Step 4: Run desired tests including restart and race repetition**

Run: `bash -lc 'source .envrc && go test -race ./pkg/store -run "^(TestJournalApplyDesired|TestJournalCursor|TestJournalReplacement)" -count=1'`

Expected: PASS.

- [ ] **Step 5: Commit desired state persistence**

```bash
git add pkg/store/journal_apply.go pkg/store/journal_apply_test.go
git commit -m "feat(store): persist desired revisions and tombstones"
```

---

### Task 4: Stage and Atomically Commit Independent HTTP and Stream Publications

**Files:**

- Create: `pkg/store/journal_publish.go`
- Test: `pkg/store/journal_publish_test.go`
- Modify: `pkg/store/journal_schema.go`

**Interfaces:**

- Consumes: `generation.ApplyTicket` and a complete `generation.PublicationSet` produced for every required domain.
- Produces: `Store.Stage`, `Store.Commit`, `Store.Abort`, strict `Store.LoadPublished`, complete `Store.Revisions`, independent HTTP/stream revisions and one atomic dependency-closure/head transaction.

- [ ] **Step 1: Write failing independent-domain and closure tests**

```go
func TestJournalCommitAdvancesOnlyRequiredPublishedDomains(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredRoute(t, journal, "etcd", "51")
	candidate := publicationCandidate(t, generation.DomainHTTP, ticket, []generation.ResourceKey{
		{Kind: "routes", ID: "r1"},
		{Kind: "upstreams", ID: "u1"},
	})
	token, err := journal.Stage(context.Background(), ticket, generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := journal.Commit(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Revisions.HTTP != ticket.DesiredRevision || ack.Revisions.Stream != 0 {
		t.Fatalf("published revisions = %+v", ack.Revisions)
	}
}

func TestJournalStageRejectsClosureMissingSnapshotResource(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredRoute(t, journal, "etcd", "52")
	candidate := publicationCandidate(t, generation.DomainHTTP, ticket, []generation.ResourceKey{
		{Kind: "routes", ID: "r1"},
	})
	candidate.Closure = append(candidate.Closure, generation.ResourceKey{Kind: "upstreams", ID: "missing"})
	_, err := journal.Stage(context.Background(), ticket, generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains: map[generation.Domain]generation.PublicationCandidate{generation.DomainHTTP: candidate},
	})
	if !errors.Is(err, generation.ErrInvalidClosure) {
		t.Fatalf("Stage() error = %v, want ErrInvalidClosure", err)
	}
}

func applyDesiredRoute(t *testing.T, journal *Store, provider, revision string) generation.ApplyTicket {
	t.Helper()
	ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: provider, Revision: revision},
		Mutations: []generation.Mutation{
			{Type: generation.MutationPut, Key: generation.ResourceKey{Kind: "routes", ID: "r1"}, Value: []byte(`{"id":"r1","upstream_id":"u1"}`)},
			{Type: generation.MutationPut, Key: generation.ResourceKey{Kind: "upstreams", ID: "u1"}, Value: []byte(`{"id":"u1","nodes":{"127.0.0.1:80":1}}`)},
		},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func publicationCandidate(
	t *testing.T,
	domain generation.Domain,
	ticket generation.ApplyTicket,
	closure []generation.ResourceKey,
) generation.PublicationCandidate {
	t.Helper()
	resources := make([]generation.Resource, 0, len(closure))
	decisions := make([]generation.ResourceDecision, 0, len(closure))
	for _, key := range closure {
		resources = append(resources, generation.Resource{Key: key, Value: []byte(`{}`)})
		decisions = append(decisions, generation.ResourceDecision{
			Key: key, Disposition: generation.DispositionPublished, Code: "test-published",
		})
	}
	snapshot, err := generation.NewSnapshot(ticket.DesiredRevision, resources, nil)
	if err != nil {
		t.Fatal(err)
	}
	return generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: domain, Revision: ticket.DesiredRevision, Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure: append([]generation.ResourceKey(nil), closure...),
		Decisions: decisions,
	}
}
```

Also test forged and authentic-but-stale tickets, exact required-domain equality, empty-domain cursor-only publications, multiple pending tokens, stale/same-revision commit rejection, one-domain-stale failure of a two-domain commit, unknown `LoadPublished` domains, defensive copies, restart commit/abort, token collision, canonical wire goldens and tampering of every cross-linked published record. A staged token must not affect artifacts, heads, decisions or `RevisionSet` before commit.

- [ ] **Step 2: Run the publication tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/store -run "^(TestJournalCommit|TestJournalStage)" -count=1'`

Expected: FAIL because publication transactions are not implemented.

- [ ] **Step 3: Validate a complete publication set before writing a stage record**

```go
func validatePublicationSet(ticket generation.ApplyTicket, set generation.PublicationSet) error {
	if set.DesiredRevision != ticket.DesiredRevision {
		return fmt.Errorf("publication desired revision %d does not match ticket %d", set.DesiredRevision, ticket.DesiredRevision)
	}
	for _, domain := range ticket.RequiredDomains {
		candidate, ok := set.Domains[domain]
		if !ok {
			return fmt.Errorf("required domain %q has no publication candidate", domain)
		}
		if candidate.Artifact.Domain != domain || candidate.Artifact.Revision != ticket.DesiredRevision ||
			candidate.Artifact.Digest != candidate.Snapshot.Digest() ||
			candidate.Artifact.Snapshot != candidate.Snapshot.SnapshotID() {
			return generation.ErrIntegrity
		}
	}
	return nil
}
```

Inside the same read/write transaction, load the current desired snapshot, active authority and keyed cursor record. The supplied ticket must byte-for-byte match the recorded normalized ticket and must still identify the current desired revision/digest. Repeat this check during `Commit`, because a valid staged ticket can become stale after a later `ApplyDesired`.

The keys of `set.Domains` must exactly equal the normalized `ticket.RequiredDomains`; reject missing, extra and unknown domains. An empty required-domain ticket accepts only an empty set and produces no published-head mutation. For every closure key, require exactly one decision and either a matching resource in the candidate snapshot or a matching tombstone plus `DispositionDeleted`. Reject duplicate closure/decision keys and resources outside the declared closure. This is structural closure validation only; Task 5 owns predecessor, last-good, quarantine and fail-closed policy.

- [ ] **Step 4: Implement durable stage, abort and one-transaction commit**

`Stage` writes a collision-checked random 128-bit token from `crypto/rand`, the ticket, complete candidates, canonical snapshot payloads, closures and decisions into `generation_publication_transactions`. It does not write the shared artifact bucket and does not update a published head, decision record or `RevisionSet`; therefore Abort cannot leak orphan artifacts. The staged transaction, published head and decision records use private explicit-tag wire DTOs inside size/digest envelopes. Maps become stable HTTP-then-stream sequences, closure and decisions are key-sorted, decode rebuilds snapshots through `generation.NewSnapshot`, and canonical re-encoding must equal the stored payload.

`publishedHeadWire` stores the complete artifact, canonical closure and the digest of the separate decision record. Decision records use `domain || big-endian revision` keys. `LoadPublished` cross-checks head key/domain/revision, artifact digest/ID and snapshot, decision key/revision/digest, closure and one-to-one decisions before returning a deep copy. Unknown domains are rejected, absent heads return `ErrNotFound`, and malformed or mismatched records return `ErrIntegrity`.

The bbolt helpers used by `Stage` and `Commit` have these exact signatures:

```go
func loadStagedPublicationTx(*bolt.Tx, generation.PublicationToken) (stagedPublication, error)
func validateStageTicketTx(*bolt.Tx, generation.ApplyTicket, bool) error
func putArtifactTx(*bolt.Tx, generation.Snapshot) error
func putPublishedHeadTx(*bolt.Tx, generation.Domain, generation.PublicationCandidate) error
func putDecisionsTx(*bolt.Tx, generation.Domain, uint64, []generation.ResourceDecision) error
func deleteStagedPublicationTx(*bolt.Tx, generation.PublicationToken) error
func acknowledgementFrom(stagedPublication) generation.Acknowledgement
func loadPublishedTx(*bolt.Tx, generation.Domain) (generation.PublishedGeneration, error)
func loadRevisionSetTx(*bolt.Tx) (generation.RevisionSet, error)
```

```go
func (s *Store) Commit(ctx context.Context, token generation.PublicationToken) (generation.Acknowledgement, error) {
	if err := contextErr(ctx); err != nil {
		return generation.Acknowledgement{}, err
	}
	var ack generation.Acknowledgement
	err := s.db.Update(func(tx *bolt.Tx) error {
		staged, err := loadStagedPublicationTx(tx, token)
		if err != nil {
			return err
		}
		if err := validateStageTicketTx(tx, staged.Ticket, true); err != nil {
			return err
		}
		current, err := loadRevisionSetTx(tx)
		if err != nil {
			return err
		}
		for _, domain := range sortedPublicationDomains(staged.Set.Domains) {
			candidate := staged.Set.Domains[domain]
			if candidate.Artifact.Revision <= revisionForDomain(current, domain) {
				return generation.ErrStaleCursor
			}
			if err := putArtifactTx(tx, candidate.Snapshot); err != nil {
				return err
			}
			if err := putPublishedHeadTx(tx, domain, candidate); err != nil {
				return err
			}
			if err := putDecisionsTx(tx, domain, candidate.Artifact.Revision, candidate.Decisions); err != nil {
				return err
			}
		}
		if err := deleteStagedPublicationTx(tx, token); err != nil {
			return err
		}
		revisions, err := loadRevisionSetTx(tx)
		if err != nil {
			return err
		}
		ack = acknowledgementFrom(staged)
		ack.Revisions = revisions
		return nil
	})
	if err != nil {
		return generation.Acknowledgement{}, err
	}
	return ack, nil
}
```

`Commit` changes durable heads only. The coordinator has already reversibly activated the detached runtime owners before calling it, but the engine retains predecessor owners and leases until `FinalizeActivation` follows a successful commit. Task 9 makes `GenerationEngine.Activate` atomically install the prepared `PublishedView` together with the HTTP/stream runtime owner; this avoids an intermediate library commit that changes the active Store read path.

Before any write, `Commit` revalidates the staged ticket against the current desired/authority records and requires every candidate revision to be strictly greater than its domain's committed head. Multiple pending tokens may coexist, but after a newer or same-revision token commits, older competitors fail without changing any artifact, head, decision or token. If either domain of a two-domain token is stale or any write fails, the whole bbolt transaction rolls back and the token remains available for retry or Abort. Success writes all artifacts, heads and decisions, deletes the token and derives the acknowledgement from the committed records in that same transaction.

`Abort` validates the bounded UTF-8 reason code but schema v1 does not persist it: success deletes only the pending token and changes no committed state; missing, already committed or already aborted tokens return `ErrNotFound`. `Revisions` reads desired plus both strict published heads in one read transaction, ignores pending transactions and fails closed on malformed heads. Task 6 reuses these strict primitives to add per-domain recovery degradation.

- [ ] **Step 5: Run publication and bbolt atomicity tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/store -run "^(TestJournalCommit|TestJournalStage|TestJournalAbort|TestJournalPublished|TestJournalPublication|TestJournalLoadPublished|TestJournalRevisions)" -count=1'`

Expected: PASS, including a forced bbolt write error that leaves both published heads unchanged.

- [ ] **Step 6: Commit domain publication transactions**

```bash
git add pkg/store/journal_publish.go pkg/store/journal_publish_test.go
git commit -m "feat(store): commit dependency-closed domain generations"
```

---

### Task 5: Enforce Per-Resource Last-Good, Quarantine and Delete Invariants

**Files:**

- Modify: `pkg/store/journal_publish.go`
- Test: `pkg/store/journal_publish_test.go`
- Modify: `pkg/store/store.go`

**Interfaces:**

- Consumes: current desired tombstones, previous published artifact and each `ResourceDecision`.
- Produces: journal-enforced consistency between each submitted decision, its predecessor, its desired tombstone and its candidate snapshot; security classification and the choice between quarantine and fail-closed belong to the trusted `PublicationEngine` and are tested in Task 9.

- [ ] **Step 1: Write the failing policy matrix**

```go
func TestJournalPublicationDispositionPolicy(t *testing.T) {
	tests := []struct {
		name            string
		predecessor     bool
		deleted         bool
		disposition     generation.ResourceDisposition
		wantErr         error
	}{
		{name: "predecessor may use last-good", predecessor: true, disposition: generation.DispositionLastGood},
		{name: "first invalid may fail closed", disposition: generation.DispositionFailClosed},
		{name: "first invalid cannot claim last-good", disposition: generation.DispositionLastGood, wantErr: generation.ErrNoLastGood},
		{name: "delete cannot resurrect predecessor", predecessor: true, deleted: true, disposition: generation.DispositionLastGood, wantErr: generation.ErrNoLastGood},
		{name: "delete is authoritative", predecessor: true, deleted: true, disposition: generation.DispositionDeleted},
		{name: "nonsecurity invalid may quarantine", disposition: generation.DispositionQuarantined},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := exerciseDisposition(t, test.predecessor, test.deleted, test.disposition)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func exerciseDisposition(
	t *testing.T,
	predecessor bool,
	deleted bool,
	disposition generation.ResourceDisposition,
) error {
	t.Helper()
	key := generation.ResourceKey{Kind: "ssls", ID: "ssl-1"}
	journal := &Store{}
	desiredResources := []generation.Resource{{Key: key, Value: []byte(`{"id":"ssl-1"}`)}}
	var tombstones []generation.Tombstone
	if deleted {
		desiredResources = nil
		tombstones = []generation.Tombstone{{Key: key, Revision: 2}}
	}
	desired, err := generation.NewSnapshot(2, desiredResources, tombstones)
	if err != nil {
		t.Fatal(err)
	}
	previousResources := []generation.Resource(nil)
	if predecessor {
		previousResources = []generation.Resource{{Key: key, Value: []byte(`{"id":"ssl-1","status":1}`)}}
	}
	previousSnapshot, err := generation.NewSnapshot(1, previousResources, nil)
	if err != nil {
		t.Fatal(err)
	}
	previous := generation.PublishedGeneration{Snapshot: previousSnapshot}
	candidateResources := []generation.Resource(nil)
	if disposition == generation.DispositionPublished || disposition == generation.DispositionLastGood {
		value := []byte(`{"id":"ssl-1"}`)
		if disposition == generation.DispositionLastGood && predecessor {
			value = []byte(`{"id":"ssl-1","status":1}`)
		}
		candidateResources = []generation.Resource{{Key: key, Value: value}}
	}
	candidateSnapshot, err := generation.NewSnapshot(2, candidateResources, tombstones)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{Snapshot: candidateSnapshot}
	return journal.validateDecision(desired, previous, candidate, generation.ResourceDecision{
		Key: key, Disposition: disposition, Code: "test-decision",
	})
}
```

- [ ] **Step 2: Run the policy matrix and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/store -run "^TestJournalPublicationDispositionPolicy$" -count=1'`

Expected: FAIL because Stage does not yet enforce predecessor and tombstone rules.

- [ ] **Step 3: Implement exact disposition validation**

```go
func (s *Store) validateDecision(
	desired generation.Snapshot,
	previous generation.PublishedGeneration,
	candidate generation.PublicationCandidate,
	decision generation.ResourceDecision,
) error {
	_, predecessor := previous.Snapshot.Lookup(decision.Key)
	_, candidateHasValue := candidate.Snapshot.Lookup(decision.Key)
	deleted := desired.Deleted(decision.Key)
	switch decision.Disposition {
	case generation.DispositionPublished:
		if deleted || !candidateHasValue {
			return fmt.Errorf("published decision has no candidate value for %s/%s", decision.Key.Kind, decision.Key.ID)
		}
	case generation.DispositionLastGood:
		if deleted || !predecessor || !candidateHasValue {
			return generation.ErrNoLastGood
		}
		oldValue, _ := previous.Snapshot.Lookup(decision.Key)
		newValue, _ := candidate.Snapshot.Lookup(decision.Key)
		if !bytes.Equal(oldValue, newValue) {
			return generation.ErrNoLastGood
		}
	case generation.DispositionQuarantined:
		if candidateHasValue {
			return fmt.Errorf("quarantined resource remains in candidate")
		}
	case generation.DispositionFailClosed:
		if candidateHasValue {
			return fmt.Errorf("fail-closed resource remains in candidate")
		}
	case generation.DispositionDeleted:
		if !deleted || candidateHasValue {
			return fmt.Errorf("deleted decision does not match desired tombstone")
		}
	default:
		return fmt.Errorf("unknown resource disposition %q", decision.Disposition)
	}
	return nil
}
```

Every decision must have a stable non-empty `Code`; the code is suitable for metrics and audit and must not contain raw configuration or a secret value. A candidate with a duplicate or missing decision is rejected before it is staged.

- [ ] **Step 4: Run focused policy and deletion tests**

Run: `bash -lc 'source .envrc && go test ./pkg/store -run "^(TestJournalPublicationDispositionPolicy|TestJournalExplicitDelete|TestJournalSecurityLastGood)" -count=1'`

Expected: PASS.

- [ ] **Step 5: Commit policy enforcement**

```bash
git add pkg/store/store.go pkg/store/journal_publish.go pkg/store/journal_publish_test.go
git commit -m "feat(store): enforce durable last-good decisions"
```

---

### Task 6: Recover Published Last-Good State and Verify Integrity Per Domain

**Files:**

- Create: `pkg/store/journal_recovery.go`
- Test: `pkg/store/journal_recovery_test.go`
- Create: `pkg/store/published_view.go`
- Test: `pkg/store/published_view_test.go`

**Interfaces:**

- Consumes: committed desired/published heads, artifact envelopes, and Task 4's strict `loadPublishedTx`/`Store.LoadPublished` plus complete committed revision reader.
- Produces: `Store.Recover`, per-domain recovery failure isolation, verified HTTP/stream `PublishedGeneration` values and an inert immutable `store.PublishedView` type. Existing production getters remain on their current path until Task 9 switches every caller in one commit.

- [ ] **Step 1: Write failing restart and domain-isolation tests**

```go
func TestJournalRecoverServesCommittedPublishedStateWithoutProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	first := openJournalAt(t, path)
	publishHTTPRoute(t, first, "r1", "/last-good")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := openJournalAt(t, path)
	state, err := second.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	httpGeneration, ok := state.Published[generation.DomainHTTP]
	if !ok || httpGeneration.Artifact.Revision != state.Revisions.HTTP {
		t.Fatalf("recovered HTTP generation = %+v/%t", httpGeneration, ok)
	}
	if len(state.Failures) != 0 {
		t.Fatalf("recovery failures = %+v", state.Failures)
	}
}

func TestJournalRecoverKeepsValidHTTPWhenStreamArtifactIsCorrupt(t *testing.T) {
	journal := journalWithTwoPublishedDomains(t)
	corruptPublishedArtifact(t, journal.db, generation.DomainStream)
	state, err := journal.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Published[generation.DomainHTTP]; !ok {
		t.Fatal("valid HTTP publication was discarded")
	}
	if _, ok := state.Published[generation.DomainStream]; ok {
		t.Fatal("corrupt stream publication was returned")
	}
	assertRecoveryFailure(t, state.Failures, generation.DomainStream, "artifact-integrity")
}

func openJournalAt(t *testing.T, path string) *Store {
	t.Helper()
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func publishHTTPRoute(t *testing.T, journal *Store, id, uri string) {
	t.Helper()
	ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "91"},
		Mutations: []generation.Mutation{{
			Type: generation.MutationPut,
			Key: generation.ResourceKey{Kind: "routes", ID: id},
			Value: []byte(fmt.Sprintf(`{"id":%q,"uri":%q}`, id, uri)),
		}},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := publicationCandidate(t, generation.DomainHTTP, ticket, []generation.ResourceKey{{Kind: "routes", ID: id}})
	token, err := journal.Stage(context.Background(), ticket, generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains: map[generation.Domain]generation.PublicationCandidate{generation.DomainHTTP: candidate},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
}

func journalWithTwoPublishedDomains(t *testing.T) *Store {
	t.Helper()
	journal := openTestJournal(t)
	ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "92"},
		Mutations: []generation.Mutation{
			{Type: generation.MutationPut, Key: generation.ResourceKey{Kind: "routes", ID: "http"}, Value: []byte(`{"id":"http"}`)},
			{Type: generation.MutationPut, Key: generation.ResourceKey{Kind: "stream_routes", ID: "tcp"}, Value: []byte(`{"id":"tcp"}`)},
		},
		RequiredDomains: []generation.Domain{generation.DomainHTTP, generation.DomainStream},
	})
	if err != nil {
		t.Fatal(err)
	}
	set := generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: publicationCandidate(t, generation.DomainHTTP, ticket, []generation.ResourceKey{{Kind: "routes", ID: "http"}}),
			generation.DomainStream: publicationCandidate(t, generation.DomainStream, ticket, []generation.ResourceKey{{Kind: "stream_routes", ID: "tcp"}}),
		},
	}
	token, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	return journal
}

func corruptPublishedArtifact(t *testing.T, db *bolt.DB, domain generation.Domain) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		id, err := publishedArtifactIDTx(tx, domain)
		if err != nil {
			return err
		}
		bucket := tx.Bucket(artifactBucket)
		encoded := bytes.Clone(bucket.Get([]byte(id)))
		if len(encoded) == 0 {
			return errors.New("artifact is missing")
		}
		encoded[len(encoded)-1] ^= 0xff
		return bucket.Put([]byte(id), encoded)
	}); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveryFailure(t *testing.T, failures []generation.RecoveryFailure, domain generation.Domain, code string) {
	t.Helper()
	for _, failure := range failures {
		if failure.Domain == domain && failure.Code == code {
			return
		}
	}
	t.Fatalf("failures = %+v, want %s/%s", failures, domain, code)
}
```

- [ ] **Step 2: Run recovery tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/store -run "^TestJournalRecover" -count=1'`

Expected: FAIL because recovery and published views do not exist.

- [ ] **Step 3: Implement deterministic recovery**

`Recover` verifies meta first. Metadata corruption and unknown schema return an error and no state. It loads desired independently, then verifies HTTP and stream heads separately. A corrupt or missing domain artifact adds one stable `RecoveryFailure` and omits only that domain. Pending staged transactions are not active and are removed by the writable owner after recovery; committed heads are never inferred from a pending transaction.

```go
func (s *Store) Recover(ctx context.Context) (generation.RecoveryState, error) {
	state := generation.RecoveryState{Published: make(map[generation.Domain]generation.PublishedGeneration)}
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := verifyJournalMetaTx(tx); err != nil {
			return err
		}
		state.Revisions = loadRevisionSetTx(tx)
		desired, err := loadDesiredSnapshotTx(tx, state.Revisions.Desired)
		if err != nil && !errors.Is(err, generation.ErrNotFound) {
			return err
		}
		state.Desired = desired
		for _, domain := range []generation.Domain{generation.DomainHTTP, generation.DomainStream} {
			published, err := loadPublishedTx(tx, domain)
			if errors.Is(err, generation.ErrNotFound) {
				continue
			}
			if err != nil {
				state.Failures = append(state.Failures, generation.RecoveryFailure{Domain: domain, Code: "artifact-integrity"})
				continue
			}
			state.Published[domain] = published
		}
		return nil
	})
	return state, err
}
```

- [ ] **Step 4: Build published read views without bbolt access**

```go
type PublishedView struct {
	generation generation.PublishedGeneration
	resources  map[generation.ResourceKey][]byte
}

func NewPublishedView(published generation.PublishedGeneration) (*PublishedView, error) {
	snapshotResources := published.Snapshot.Resources()
	resources := make(map[generation.ResourceKey][]byte, len(snapshotResources))
	for _, resource := range snapshotResources {
		resources[resource.Key] = bytes.Clone(resource.Value)
	}
	return &PublishedView{generation: published, resources: resources}, nil
}

func (v *PublishedView) Raw(kind, id string) ([]byte, bool) {
	value, ok := v.resources[generation.ResourceKey{Kind: kind, ID: id}]
	return bytes.Clone(value), ok
}
```

Add explicit decoding methods on `PublishedView` needed by Task 9 (`ConfigSnapshot`, `StreamRoutes`, `Consumer`, `ConsumerGroup`, `SSL`, `Proto`, and plugin metadata). They decode only the immutable snapshot and never consult bbolt or the package-level Store. Do not route an existing production getter through this type yet; Task 9 switches all provider/runtime ownership and deletes the old persistence path atomically.

- [ ] **Step 5: Run restart, view-isolation and getter tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/store -run "^(TestJournalRecover|TestPublishedView|TestStoreGettersPublished)" -count=1'`

Expected: PASS.

- [ ] **Step 6: Commit durable recovery and read views**

```bash
git add pkg/store/journal_recovery.go pkg/store/journal_recovery_test.go pkg/store/published_view.go pkg/store/published_view_test.go
git commit -m "feat(store): recover published generations across restart"
```

---

### Task 7: Implement the End-to-End Acknowledgement Coordinator

**Files:**

- Create: `pkg/generation/coordinator.go`
- Test: `pkg/generation/coordinator_test.go`

**Interfaces:**

- Consumes: the exact `generation.Journal` and `generation.PublicationEngine` interfaces.
- Produces: serialized `Coordinator.Apply`; later providers advance their own cursors only from its returned `Acknowledgement`.

- [ ] **Step 1: Write failing coordinator sequencing tests**

```go
func TestCoordinatorAcknowledgesOnlyAfterCommitAndFinalize(t *testing.T) {
	journal := newFakeJournal()
	engine := &fakeEngine{calls: &journal.calls}
	coordinator := NewCoordinator(journal, engine)
	ack, err := coordinator.Apply(context.Background(), desiredHTTPBatch("etcd", "61"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"apply-desired", "load-desired", "load-published:http", "prepare",
		"stage", "activate", "commit", "finalize",
	}
	if !slices.Equal(journalAndEngineCalls(journal, engine), want) {
		t.Fatalf("calls = %v, want %v", journalAndEngineCalls(journal, engine), want)
	}
	if ack.Cursor.Revision != "61" {
		t.Fatalf("ack cursor = %+v", ack.Cursor)
	}
}

func TestCoordinatorActivationFailureAbortsAndDoesNotAcknowledge(t *testing.T) {
	wantErr := errors.New("listener probe failed")
	journal := newFakeJournal()
	engine := &fakeEngine{calls: &journal.calls, activateErr: wantErr}
	coordinator := NewCoordinator(journal, engine)
	_, err := coordinator.Apply(context.Background(), desiredHTTPBatch("etcd", "62"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Apply() error = %v, want %v", err, wantErr)
	}
	if journal.commitCalls != 0 || journal.abortCalls != 1 {
		t.Fatalf("commit/abort = %d/%d, want 0/1", journal.commitCalls, journal.abortCalls)
	}
	want := []string{
		"apply-desired", "load-desired", "load-published:http", "prepare",
		"stage", "activate", "rollback", "abort",
	}
	if !slices.Equal(journalAndEngineCalls(journal, engine), want) {
		t.Fatalf("calls = %v, want %v", journalAndEngineCalls(journal, engine), want)
	}
	if engine.rollbackCalls != 1 || engine.finalizeCalls != 0 {
		t.Fatalf("rollback/finalize = %d/%d, want 1/0", engine.rollbackCalls, engine.finalizeCalls)
	}
}

func TestCoordinatorCommitFailureRollsBackActivationAndAborts(t *testing.T) {
	wantErr := errors.New("bbolt commit failed")
	journal := newFakeJournal()
	journal.commitErr = wantErr
	engine := &fakeEngine{calls: &journal.calls}
	coordinator := NewCoordinator(journal, engine)
	_, err := coordinator.Apply(context.Background(), desiredHTTPBatch("etcd", "63"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Apply() error = %v, want %v", err, wantErr)
	}
	want := []string{
		"apply-desired", "load-desired", "load-published:http", "prepare",
		"stage", "activate", "commit", "rollback", "abort",
	}
	if !slices.Equal(journalAndEngineCalls(journal, engine), want) {
		t.Fatalf("calls = %v, want %v", journalAndEngineCalls(journal, engine), want)
	}
	if journal.abortCalls != 1 || engine.rollbackCalls != 1 || engine.finalizeCalls != 0 {
		t.Fatalf("abort/rollback/finalize = %d/%d/%d, want 1/1/0",
			journal.abortCalls, engine.rollbackCalls, engine.finalizeCalls)
	}
}

func TestCoordinatorStageFailureDiscardsPreparedWithUncanceledContext(t *testing.T) {
	stageErr := errors.New("bbolt stage failed")
	discardErr := errors.New("candidate close failed")
	j := newFakeJournal()
	j.stageErr = stageErr
	engine := &fakeEngine{calls: &j.calls, discardErr: discardErr}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewCoordinator(j, engine).Apply(ctx, desiredHTTPBatch("etcd", "64"))
	if !errors.Is(err, stageErr) || !errors.Is(err, discardErr) {
		t.Fatalf("Apply() error = %v, want joined stage and discard errors", err)
	}
	if !engine.discardContextLive || engine.discardCalls != 1 {
		t.Fatalf("discard context live/calls = %t/%d, want true/1",
			engine.discardContextLive, engine.discardCalls)
	}
	want := []string{
		"apply-desired", "load-desired", "load-published:http", "prepare", "stage", "discard",
	}
	if !slices.Equal(journalAndEngineCalls(j, engine), want) {
		t.Fatalf("calls = %v, want %v", journalAndEngineCalls(j, engine), want)
	}
}

type fakeJournal struct {
	calls       []string
	desired     Snapshot
	ticket      ApplyTicket
	staged      PublicationSet
	stageErr    error
	commitErr   error
	commitCalls int
	abortCalls  int
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{}
}

func (j *fakeJournal) ApplyDesired(_ context.Context, batch DesiredBatch) (ApplyTicket, error) {
	j.calls = append(j.calls, "apply-desired")
	resources := make([]Resource, 0, len(batch.Mutations))
	for _, mutation := range batch.Mutations {
		if mutation.Type == MutationPut {
			resources = append(resources, Resource{Key: mutation.Key, Value: bytes.Clone(mutation.Value)})
		}
	}
	snapshot, err := NewSnapshot(1, resources, nil)
	if err != nil {
		return ApplyTicket{}, err
	}
	j.desired = snapshot
	j.ticket = ApplyTicket{
		DesiredRevision: 1, DesiredDigest: snapshot.Digest(), Cursor: batch.Cursor,
		RequiredDomains: append([]Domain(nil), batch.RequiredDomains...),
	}
	return j.ticket, nil
}

func (j *fakeJournal) LoadDesired(context.Context, uint64) (Snapshot, error) {
	j.calls = append(j.calls, "load-desired")
	return j.desired.Clone(), nil
}

func (j *fakeJournal) LoadPublished(_ context.Context, domain Domain) (PublishedGeneration, error) {
	j.calls = append(j.calls, "load-published:"+string(domain))
	return PublishedGeneration{}, ErrNotFound
}

func (j *fakeJournal) Stage(_ context.Context, _ ApplyTicket, set PublicationSet) (PublicationToken, error) {
	j.calls = append(j.calls, "stage")
	j.staged = set
	if j.stageErr != nil {
		return "", j.stageErr
	}
	return PublicationToken("test-token"), nil
}

func (j *fakeJournal) Commit(context.Context, PublicationToken) (Acknowledgement, error) {
	j.calls = append(j.calls, "commit")
	j.commitCalls++
	if j.commitErr != nil {
		return Acknowledgement{}, j.commitErr
	}
	return Acknowledgement{
		Cursor: j.ticket.Cursor,
		Revisions: RevisionSet{Desired: j.ticket.DesiredRevision, HTTP: j.ticket.DesiredRevision},
	}, nil
}

func (j *fakeJournal) Abort(context.Context, PublicationToken, string) error {
	j.calls = append(j.calls, "abort")
	j.abortCalls++
	return nil
}

func (j *fakeJournal) Revisions(context.Context) (RevisionSet, error) {
	return RevisionSet{Desired: j.ticket.DesiredRevision}, nil
}

func (j *fakeJournal) Recover(context.Context) (RecoveryState, error) {
	return RecoveryState{}, nil
}

func (j *fakeJournal) Close() error { return nil }

type fakeEngine struct {
	calls         *[]string
	discardErr    error
	activateErr   error
	rollbackErr   error
	discardCalls  int
	discardContextLive bool
	rollbackCalls int
	finalizeCalls int
}

func (e *fakeEngine) Prepare(
	_ context.Context,
	ticket ApplyTicket,
	desired Snapshot,
	_ map[Domain]PublishedGeneration,
) (PublicationSet, error) {
	*e.calls = append(*e.calls, "prepare")
	resources := desired.Resources()
	closure := make([]ResourceKey, 0, len(resources))
	decisions := make([]ResourceDecision, 0, len(resources))
	for _, resource := range resources {
		closure = append(closure, resource.Key)
		decisions = append(decisions, ResourceDecision{
			Key: resource.Key, Disposition: DispositionPublished, Code: "test-published",
		})
	}
	candidate := PublicationCandidate{
		Artifact: GenerationArtifact{
			Domain: DomainHTTP, Revision: ticket.DesiredRevision,
			Digest: desired.Digest(), Snapshot: desired.SnapshotID(),
		},
		Snapshot: desired.Clone(), Closure: closure, Decisions: decisions,
	}
	return PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains: map[Domain]PublicationCandidate{DomainHTTP: candidate},
	}, nil
}

func (e *fakeEngine) DiscardPrepared(ctx context.Context, _ PublicationSet) error {
	*e.calls = append(*e.calls, "discard")
	e.discardCalls++
	e.discardContextLive = ctx.Err() == nil
	return e.discardErr
}

func (e *fakeEngine) Activate(context.Context, PublicationToken, PublicationSet) error {
	*e.calls = append(*e.calls, "activate")
	return e.activateErr
}

func (e *fakeEngine) RollbackActivation(context.Context, PublicationToken, PublicationSet) error {
	*e.calls = append(*e.calls, "rollback")
	e.rollbackCalls++
	return e.rollbackErr
}

func (e *fakeEngine) FinalizeActivation(context.Context, PublicationToken, PublicationSet) {
	*e.calls = append(*e.calls, "finalize")
	e.finalizeCalls++
}

func desiredHTTPBatch(provider, revision string) DesiredBatch {
	return DesiredBatch{
		Cursor: ProviderCursor{Provider: provider, Revision: revision},
		Mutations: []Mutation{{
			Type: MutationPut, Key: ResourceKey{Kind: "routes", ID: "r1"}, Value: []byte(`{"id":"r1"}`),
		}},
		RequiredDomains: []Domain{DomainHTTP},
	}
}

func journalAndEngineCalls(journal *fakeJournal, _ *fakeEngine) []string {
	return append([]string(nil), journal.calls...)
}
```

Also test that prepare errors do not stage or discard, a successful discard removes the candidate exactly once, concurrent calls serialize, and a retry of the same provider cursor reuses the desired ticket but retries an uncommitted publication. Add a rollback-error case which proves `errors.Join` preserves both the activation/commit error and rollback error, while `Abort` still runs. `FinalizeActivation` is asserted only after a successful journal commit; it has no recoverable-error path.

- [ ] **Step 2: Run coordinator tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/generation -run "^TestCoordinator" -count=1'`

Expected: FAIL because `Coordinator` and `PublicationEngine` do not exist.

- [ ] **Step 3: Implement the coordinator with one lifecycle transaction at a time**

```go
func (c *Coordinator) Apply(ctx context.Context, batch DesiredBatch) (Acknowledgement, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ticket, err := c.journal.ApplyDesired(ctx, batch)
	if err != nil {
		return Acknowledgement{}, err
	}
	desired, err := c.journal.LoadDesired(ctx, ticket.DesiredRevision)
	if err != nil {
		return Acknowledgement{}, err
	}
	previous := make(map[Domain]PublishedGeneration)
	for _, domain := range ticket.RequiredDomains {
		published, loadErr := c.journal.LoadPublished(ctx, domain)
		if loadErr == nil {
			previous[domain] = published
		} else if !errors.Is(loadErr, ErrNotFound) {
			return Acknowledgement{}, loadErr
		}
	}
	set, err := c.engine.Prepare(ctx, ticket, desired, previous)
	if err != nil {
		return Acknowledgement{}, err
	}
	token, err := c.journal.Stage(ctx, ticket, set)
	if err != nil {
		discardErr := c.engine.DiscardPrepared(context.WithoutCancel(ctx), set)
		return Acknowledgement{}, errors.Join(err, discardErr)
	}
	if err := c.engine.Activate(ctx, token, set); err != nil {
		cleanupCtx := context.WithoutCancel(ctx)
		rollbackErr := c.engine.RollbackActivation(cleanupCtx, token, set)
		abortErr := c.journal.Abort(cleanupCtx, token, stableAbortCode("activation", err))
		return Acknowledgement{}, errors.Join(err, rollbackErr, abortErr)
	}
	ack, err := c.journal.Commit(ctx, token)
	if err != nil {
		cleanupCtx := context.WithoutCancel(ctx)
		rollbackErr := c.engine.RollbackActivation(cleanupCtx, token, set)
		abortErr := c.journal.Abort(cleanupCtx, token, stableAbortCode("commit", err))
		return Acknowledgement{}, errors.Join(err, rollbackErr, abortErr)
	}
	c.engine.FinalizeActivation(context.WithoutCancel(ctx), token, set)
	return ack, nil
}

func stableAbortCode(phase string, err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return phase + "-context-canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return phase + "-deadline-exceeded"
	default:
		return phase + "-failed"
	}
}
```

`DiscardPrepared` is called only when `Prepare` succeeded but `Stage` did not return a token. It receives an uncanceled cleanup context, is idempotent by publication-set identity, and releases every new candidate lease; `errors.Join` preserves both persistence and cleanup failures. `RollbackActivation` is called after every activation error because it is idempotent and is the only safe way to clean up an engine that switched one domain before another failed. A commit error means the bbolt transaction did not publish any head: the coordinator restores the old active owners, closes the new owners, aborts the stage and returns no acknowledgement. `FinalizeActivation` runs only after the durable commit and cannot report a recoverable business error; an implementation invariant failure there must terminate/fail the runtime rather than return an acknowledgement from split truth. `stableAbortCode` maps errors to bounded codes and never passes `err.Error()` into the journal. Schema v1 validates this Abort code but does not persist abort audit records. A prepare error represents an unresolved transient/system failure and is not an acknowledgement. Deterministic invalid-resource handling must be represented by a valid candidate and explicit decisions, allowing unrelated resources to publish.

- [ ] **Step 4: Run coordinator tests with race detection**

Run: `bash -lc 'source .envrc && go test -race ./pkg/generation -run "^TestCoordinator" -count=1'`

Expected: PASS.

- [ ] **Step 5: Commit the acknowledgement state machine**

```bash
git add pkg/generation/coordinator.go pkg/generation/coordinator_test.go
git commit -m "feat(generation): coordinate durable publication acknowledgement"
```

---

### Task 8: Prepare Provider Batch Translation Without a Second Runtime Path

**Files:**

- Modify: `pkg/etcd/watcher.go:491-707`
- Test: `pkg/etcd/watcher_test.go`
- Modify: `pkg/config/standalone.go:45-590`
- Test: `pkg/config/standalone_test.go`

**Interfaces:**

- Consumes: etcd snapshot/watch responses and normalized standalone file resources.
- Produces: pure `desiredBatchFromEtcdSnapshot`, `desiredBatchFromEtcdWatch`, and `desiredBatchFromStandalone` functions returning `generation.DesiredBatch`; production constructors remain unchanged until Task 9 atomically switches ownership.

- [ ] **Step 1: Write failing pure-translation tests**

```go
func TestDesiredBatchFromEtcdWatchCarriesRevisionDeleteAndDomains(t *testing.T) {
	batch, err := desiredBatchFromEtcdWatch("/apisix/", clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 71},
		Events: []*clientv3.Event{{
			Type: mvccpb.DELETE,
			Kv: &mvccpb.KeyValue{Key: []byte("/apisix/stream_routes/mqtt"), ModRevision: 71},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Cursor != (generation.ProviderCursor{Provider: "etcd", Revision: "71"}) ||
		batch.Mutations[0].Type != generation.MutationDelete ||
		!slices.Equal(batch.RequiredDomains, []generation.Domain{generation.DomainStream}) {
		t.Fatalf("batch = %+v", batch)
	}
}

func TestDesiredBatchFromStandaloneUsesContentDigestCursor(t *testing.T) {
	batch := desiredBatchFromStandalone(standaloneSnapshot{
		"routes": {"r1": []byte(`{"id":"r1","uri":"/"}`)},
	}, []byte("canonical-file"))
	if !batch.ReplaceManaged || batch.Cursor.Provider != "standalone" ||
		!strings.HasPrefix(batch.Cursor.Revision, "sha256:") {
		t.Fatalf("batch = %+v", batch)
	}
}
```

- [ ] **Step 2: Run translation tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/etcd ./pkg/config -run "^TestDesiredBatchFrom" -count=1'`

Expected: FAIL because the translation functions do not exist.

- [ ] **Step 3: Implement pure response/file translation**

```go
func desiredBatchFromEtcdWatch(prefix string, response clientv3.WatchResponse) (generation.DesiredBatch, error) {
	batch := generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: strconv.FormatInt(response.Header.Revision, 10)},
	}
	for _, event := range response.Events {
		mutation, domains, ok, err := desiredMutationFromEtcdEvent(prefix, event)
		if err != nil {
			return generation.DesiredBatch{}, err
		}
		if !ok {
			continue
		}
		batch.Mutations = append(batch.Mutations, mutation)
		batch.RequiredDomains = append(batch.RequiredDomains, domains...)
	}
	batch.RequiredDomains = normalizeRequiredDomains(batch.RequiredDomains)
	return batch, nil
}
```

Etcd snapshot translation sets `ReplaceManaged=true`; watch translation preserves event order and explicit deletes. Standalone translation sets `ReplaceManaged=true`, sorts all mutations by resource kind/ID, hashes the canonical normalized file content for the cursor and never uses an in-memory counter.

Required domains use this exact conservative table until the immutable compiler owns dependency impact. A batch unions and sorts all domains returned by the table.

```go
func desiredDomains(kind string) []generation.Domain {
	switch kind {
	case "stream_routes":
		return []generation.Domain{generation.DomainStream}
	case "services", "upstreams", "secrets":
		return []generation.Domain{generation.DomainHTTP, generation.DomainStream}
	case "routes", "global_rules", "plugin_configs", "plugin_metadata", "plugins",
		"ssls", "consumers", "consumer_groups", "protos":
		return []generation.Domain{generation.DomainHTTP}
	default:
		return nil
	}
}
```

- [ ] **Step 4: Run provider translation and namespace tests**

Run: `bash -lc 'source .envrc && go test ./pkg/etcd ./pkg/config -run "^(TestDesiredBatchFrom|TestManagedKey|TestStandalone.*Snapshot)" -count=1'`

Expected: PASS. Production still uses the old path at this checkpoint; no second production path is selectable.

- [ ] **Step 5: Commit the inert translation seam**

```bash
git add pkg/etcd/watcher.go pkg/etcd/watcher_test.go pkg/config/standalone.go pkg/config/standalone_test.go
git commit -m "refactor(provider): normalize desired generation batches"
```

---

### Task 9: Atomically Cut Providers and Runtime to the Journal and Delete the Legacy Store Path

**Files:**

- Create: `pkg/server/generation_engine.go`
- Test: `pkg/server/generation_engine_test.go`
- Modify: `pkg/store/store.go`
- Modify: `pkg/store/getter.go`
- Modify: `pkg/store/plugin_metadata_cache.go`
- Modify: `pkg/etcd/watcher.go`
- Modify: `pkg/etcd/watcher_test.go`
- Modify: `pkg/config/standalone.go`
- Modify: `pkg/config/standalone_test.go`
- Modify: `pkg/server/server.go:150-215, 640-725, 940-1110, 1180-1395`
- Modify: `pkg/server/reload.go`
- Modify: `pkg/server/reload_test.go`
- Modify: `pkg/server/stream_test.go`
- Modify: `pkg/server/tls.go:140-170`
- Modify: `pkg/observability/metrics/config_apply.go`
- Modify: `pkg/observability/metrics/config_apply_test.go`
- Delete: `pkg/store/event.go`
- Delete: `pkg/store/event_ack_test.go`
- Delete: `pkg/store/durable_apply_test.go`
- Delete: `pkg/store/standalone_snapshot.go`

**Interfaces:**

- Consumes: `store.OpenJournal`, `generation.NewCoordinator`, `config.JournalPath(*config.EffectiveConfig) string` from the static-config plan, pure provider translation functions and the existing HTTP/stream prepare/activate behavior.
- Produces: the only production path: provider → `Coordinator.Apply` → journal desired/stage → reversible `server.GenerationEngine` activation → journal commit → activation finalize → ack; recovered published views feed HTTP, TLS, consumers and stream.

- [ ] **Step 1: Write failing server-engine tests before changing production wiring**

```go
func TestGenerationEnginePrepareUsesDependencyClosedSnapshot(t *testing.T) {
	engine := newGenerationEngineForTest(t)
	desired := snapshotWithRouteAndUpstream(t, 81, "route-1", "upstream-1")
	set, err := engine.Prepare(context.Background(), generation.ApplyTicket{
		DesiredRevision: 81,
		DesiredDigest: desired.Digest(),
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := set.Domains[generation.DomainHTTP]
	assertClosure(t, candidate.Closure,
		generation.ResourceKey{Kind: "routes", ID: "route-1"},
		generation.ResourceKey{Kind: "upstreams", ID: "upstream-1"})
}

func TestGenerationEngineInvalidSecurityResourceWithoutPredecessorFailsClosed(t *testing.T) {
	engine := newGenerationEngineForTest(t)
	desired := snapshotWithInvalidEnabledSSL(t, 82, "ssl-1")
	set, err := engine.Prepare(context.Background(), ticketFor(desired, generation.DomainHTTP), desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, set.Domains[generation.DomainHTTP].Decisions,
		generation.ResourceKey{Kind: "ssls", ID: "ssl-1"}, generation.DispositionFailClosed)
}

func newGenerationEngineForTest(t *testing.T) *GenerationEngine {
	t.Helper()
	server := &Server{
		addr: "127.0.0.1:9080",
		routes: newRouteHandler(http.NotFoundHandler(), nil),
		clusters: pxy.NewClusterRegistry(newClusterObserver()),
	}
	t.Cleanup(server.routes.Close)
	t.Cleanup(server.clusters.Close)
	return NewGenerationEngine(server)
}

func snapshotWithRouteAndUpstream(t *testing.T, revision uint64, routeID, upstreamID string) generation.Snapshot {
	t.Helper()
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{
		{
			Key: generation.ResourceKey{Kind: "routes", ID: routeID},
			Value: []byte(fmt.Sprintf(`{"id":%q,"uri":"/","upstream_id":%q}`, routeID, upstreamID)),
		},
		{
			Key: generation.ResourceKey{Kind: "upstreams", ID: upstreamID},
			Value: []byte(fmt.Sprintf(`{"id":%q,"nodes":{"127.0.0.1:80":1}}`, upstreamID)),
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func snapshotWithInvalidEnabledSSL(t *testing.T, revision uint64, id string) generation.Snapshot {
	t.Helper()
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: generation.ResourceKey{Kind: "ssls", ID: id},
		Value: []byte(fmt.Sprintf(`{"id":%q,"status":1,"cert":"bad","key":"bad"}`, id)),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func ticketFor(snapshot generation.Snapshot, domains ...generation.Domain) generation.ApplyTicket {
	return generation.ApplyTicket{
		DesiredRevision: snapshot.Revision(),
		DesiredDigest: snapshot.Digest(),
		Cursor: generation.ProviderCursor{Provider: "test", Revision: strconv.FormatUint(snapshot.Revision(), 10)},
		RequiredDomains: append([]generation.Domain(nil), domains...),
	}
}

func assertClosure(t *testing.T, got []generation.ResourceKey, want ...generation.ResourceKey) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("closure = %+v, want %+v", got, want)
	}
}

func assertDecision(
	t *testing.T,
	decisions []generation.ResourceDecision,
	key generation.ResourceKey,
	disposition generation.ResourceDisposition,
) {
	t.Helper()
	for _, decision := range decisions {
		if decision.Key == key && decision.Disposition == disposition {
			return
		}
	}
	t.Fatalf("decisions = %+v, want %s/%s %s", decisions, key.Kind, key.ID, disposition)
}
```

Add these lifecycle tests in the same file:

```go
func TestGenerationEnginePartialActivationFailureCanRestoreOldOwners(t *testing.T) {
	httpOwner, streamOwner := "http-old", "stream-old"
	httpLease := &fakeDomainActivationLease{owner: &httpOwner, old: "http-old", next: "http-new"}
	streamErr := errors.New("stream listener activation failed")
	streamLease := &fakeDomainActivationLease{
		owner: &streamOwner, old: "stream-old", next: "stream-new", activateErr: streamErr,
	}
	set := generation.PublicationSet{DesiredRevision: 83}
	engine := &GenerationEngine{
		prepared: map[uint64]*preparedActivation{83: {set: set, leases: []domainActivationLease{httpLease, streamLease}}},
		activations: make(map[generation.PublicationToken]*activationState),
	}
	token := generation.PublicationToken("partial-activation")
	if err := engine.Activate(context.Background(), token, set); !errors.Is(err, streamErr) {
		t.Fatalf("Activate() error = %v, want %v", err, streamErr)
	}
	if httpOwner != "http-new" || streamOwner != "stream-old" {
		t.Fatalf("owners before rollback = %q/%q", httpOwner, streamOwner)
	}
	if err := engine.RollbackActivation(context.Background(), token, set); err != nil {
		t.Fatal(err)
	}
	if httpOwner != "http-old" || streamOwner != "stream-old" {
		t.Fatalf("owners after rollback = %q/%q", httpOwner, streamOwner)
	}
	if httpLease.newReleaseCalls != 1 || streamLease.newReleaseCalls != 1 {
		t.Fatalf("new release calls = %d/%d, want 1/1",
			httpLease.newReleaseCalls, streamLease.newReleaseCalls)
	}
}

func TestGenerationEngineFinalizeRetiresOldOwnersOnlyAfterCommit(t *testing.T) {
	owner := "http-old"
	lease := &fakeDomainActivationLease{owner: &owner, old: "http-old", next: "http-new"}
	set := generation.PublicationSet{DesiredRevision: 84}
	engine := &GenerationEngine{
		prepared: map[uint64]*preparedActivation{84: {set: set, leases: []domainActivationLease{lease}}},
		activations: make(map[generation.PublicationToken]*activationState),
	}
	token := generation.PublicationToken("finalize-after-commit")
	if err := engine.Activate(context.Background(), token, set); err != nil {
		t.Fatal(err)
	}
	if owner != "http-new" || lease.oldReleaseCalls != 0 || lease.newReleaseCalls != 0 {
		t.Fatalf("pre-commit owner/releases = %q/%d/%d", owner, lease.oldReleaseCalls, lease.newReleaseCalls)
	}
	engine.FinalizeActivation(context.Background(), token, set)
	if owner != "http-new" || lease.oldReleaseCalls != 0 || lease.newReleaseCalls != 0 || len(engine.retiring) != 1 {
		t.Fatalf("final owner/releases/queue = %q/%d/%d/%d, want http-new/0/0/1",
			owner, lease.oldReleaseCalls, lease.newReleaseCalls, len(engine.retiring))
	}
	if err := engine.retireNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lease.oldReleaseCalls != 1 {
		t.Fatalf("old release calls after asynchronous retirement = %d, want 1", lease.oldReleaseCalls)
	}
}

func TestGenerationEngineDiscardPreparedClosesOnlyCandidateLeases(t *testing.T) {
	owner := "http-old"
	lease := &fakeDomainActivationLease{owner: &owner, old: "http-old", next: "http-new"}
	set := generation.PublicationSet{DesiredRevision: 85}
	engine := &GenerationEngine{
		prepared: map[uint64]*preparedActivation{85: {set: set, leases: []domainActivationLease{lease}}},
		activations: make(map[generation.PublicationToken]*activationState),
	}
	if err := engine.DiscardPrepared(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	if err := engine.DiscardPrepared(context.Background(), set); err != nil {
		t.Fatalf("repeated DiscardPrepared() = %v", err)
	}
	if owner != "http-old" || lease.newReleaseCalls != 1 || lease.oldReleaseCalls != 0 {
		t.Fatalf("owner/new/old releases = %q/%d/%d, want http-old/1/0",
			owner, lease.newReleaseCalls, lease.oldReleaseCalls)
	}
}

type fakeDomainActivationLease struct {
	owner            *string
	old              string
	next             string
	activateErr      error
	activated        bool
	rolledBack       bool
	discarded        bool
	retired          bool
	newReleaseCalls  int
	oldReleaseCalls  int
}

func (l *fakeDomainActivationLease) Discard(context.Context) error {
	if l.discarded {
		return nil
	}
	l.newReleaseCalls++
	l.discarded = true
	return nil
}

func (l *fakeDomainActivationLease) Activate(context.Context) error {
	if l.activateErr != nil {
		return l.activateErr
	}
	*l.owner = l.next
	l.activated = true
	return nil
}

func (l *fakeDomainActivationLease) Rollback(context.Context) error {
	if l.rolledBack {
		return nil
	}
	if l.activated {
		*l.owner = l.old
	}
	l.newReleaseCalls++
	l.rolledBack = true
	return nil
}

func (l *fakeDomainActivationLease) Retire(context.Context) error {
	if l.retired {
		return nil
	}
	l.oldReleaseCalls++
	l.retired = true
	return nil
}
```

The commit-failure integration case uses `generation.NewCoordinator` with this real engine and a `generation.Journal` fixture whose `Commit` returns `errCommit`. It asserts `errors.Is(err, errCommit)`, the old HTTP/stream owners are restored, every new lease is released once, every old lease remains live, the staged token is aborted once, and `FinalizeActivation` is never called. Also add a test proving a valid SSL predecessor is copied with `DispositionLastGood`, and a test proving an explicit SSL delete cannot be retained.

- [ ] **Step 2: Run engine tests and confirm RED**

Run: `bash -lc 'source .envrc && go test ./pkg/server -run "^TestGenerationEngine" -count=1'`

Expected: FAIL because `GenerationEngine` does not exist.

- [ ] **Step 3: Implement the permanent publication-engine boundary**

```go
type domainActivationLease interface {
	Discard(context.Context) error
	Activate(context.Context) error
	Rollback(context.Context) error
	Retire(context.Context) error
}

type preparedActivation struct {
	set    generation.PublicationSet
	leases []domainActivationLease // deterministic HTTP-then-stream order
}

type activationState struct {
	prepared *preparedActivation
}

type GenerationEngine struct {
	server      *Server
	mu          sync.Mutex
	prepared    map[uint64]*preparedActivation
	activations map[generation.PublicationToken]*activationState
	retiring    []*preparedActivation
}

func NewGenerationEngine(server *Server) *GenerationEngine {
	return &GenerationEngine{
		server: server,
		prepared: make(map[uint64]*preparedActivation),
		activations: make(map[generation.PublicationToken]*activationState),
		retiring: make([]*preparedActivation, 0),
	}
}

func (e *GenerationEngine) Prepare(
	ctx context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	previous map[generation.Domain]generation.PublishedGeneration,
) (generation.PublicationSet, error) {
	set := generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains: make(map[generation.Domain]generation.PublicationCandidate),
	}
	for _, domain := range ticket.RequiredDomains {
		candidate, err := e.prepareDomain(ctx, domain, ticket, desired, previous[domain])
		if err != nil {
			return generation.PublicationSet{}, err
		}
		set.Domains[domain] = candidate
	}
	prepared, err := e.prepareActivationLeases(ctx, set)
	if err != nil {
		return generation.PublicationSet{}, err
	}
	e.mu.Lock()
	e.prepared[ticket.DesiredRevision] = prepared
	e.mu.Unlock()
	return set, nil
}

func (e *GenerationEngine) DiscardPrepared(
	ctx context.Context,
	set generation.PublicationSet,
) error {
	e.mu.Lock()
	prepared, ok := e.prepared[set.DesiredRevision]
	if !ok {
		e.mu.Unlock()
		return nil
	}
	if !samePublicationSetIdentity(prepared.set, set) {
		e.mu.Unlock()
		return ErrPreparedActivationNotFound
	}
	delete(e.prepared, set.DesiredRevision)
	e.mu.Unlock()

	var discardErr error
	for index := len(prepared.leases) - 1; index >= 0; index-- {
		discardErr = errors.Join(discardErr, prepared.leases[index].Discard(ctx))
	}
	return discardErr
}
```

`prepareDomain` builds a detached `store.PublishedView`, validates every resource, resolves the complete dependency closure and creates all resource decisions. It must not replace the live route handler, listener, stream runtime, cache or global pointer. `prepareActivationLeases` creates HTTP then stream leases and returns the exact `preparedActivation` stored for the desired revision; `samePublicationSetIdentity` compares desired revision plus each domain artifact's domain/revision/digest/snapshot before discard or activation, so a caller cannot close or bind different prepared bytes. `domainActivationLease.Discard` closes only the detached new owner and its dependency leases; it never mutates or releases the predecessor.

Implement the reversible boundary with these exact state transitions:

```go
func (e *GenerationEngine) Activate(
	ctx context.Context,
	token generation.PublicationToken,
	set generation.PublicationSet,
) error {
	e.mu.Lock()
	prepared, ok := e.prepared[set.DesiredRevision]
	if !ok || !samePublicationSetIdentity(prepared.set, set) {
		e.mu.Unlock()
		return ErrPreparedActivationNotFound
	}
	if _, duplicate := e.activations[token]; duplicate {
		e.mu.Unlock()
		return ErrActivationTokenInUse
	}
	delete(e.prepared, set.DesiredRevision)
	e.activations[token] = &activationState{prepared: prepared}
	e.mu.Unlock()

	for _, lease := range prepared.leases {
		if err := lease.Activate(ctx); err != nil {
			return err // coordinator always calls RollbackActivation
		}
	}
	return nil
}

func (e *GenerationEngine) RollbackActivation(
	ctx context.Context,
	token generation.PublicationToken,
	set generation.PublicationSet,
) error {
	state, ok := e.activation(token, set)
	if !ok {
		return nil // already rolled back is success
	}
	var rollbackErr error
	for index := len(state.prepared.leases) - 1; index >= 0; index-- {
		rollbackErr = errors.Join(rollbackErr, state.prepared.leases[index].Rollback(ctx))
	}
	if rollbackErr == nil {
		e.deleteActivation(token)
	}
	return rollbackErr
}

func (e *GenerationEngine) FinalizeActivation(
	_ context.Context,
	token generation.PublicationToken,
	set generation.PublicationSet,
) {
	e.mu.Lock()
	state, ok := e.activations[token]
	if !ok || !samePublicationSetIdentity(state.prepared.set, set) {
		e.mu.Unlock()
		panic("generation invariant: committed activation token is missing or mismatched")
	}
	e.retiring = append(e.retiring, state.prepared)
	delete(e.activations, token)
	e.mu.Unlock()
}

func (e *GenerationEngine) retireNext(ctx context.Context) error {
	e.mu.Lock()
	if len(e.retiring) == 0 {
		e.mu.Unlock()
		return nil
	}
	retiring := e.retiring[0]
	e.retiring = e.retiring[1:]
	e.mu.Unlock()

	var retireErr error
	for _, lease := range retiring.leases {
		retireErr = errors.Join(retireErr, lease.Retire(ctx))
	}
	return retireErr
}
```

`activation` and `deleteActivation` lock `GenerationEngine.mu`; they never call a lease while holding that mutex. Each domain lease captures both the old live owner and the detached new owner. `Activate` swaps to the new owner but retains old handler/runtime/resource leases. `Rollback` is idempotent and restores the old owner before closing the new owner and releasing every new dependency lease, including leases for a later domain that never activated. `Finalize` only transfers permanent ownership to the new generation, appends the predecessor leases to the in-memory retirement queue, deletes the activation record, and returns. The server main loop calls `retireNext` asynchronously to drain old handlers/runtimes and release predecessor dependency leases; retirement errors change runtime status and are retried or escalated, but cannot retroactively fail finalize or the already committed provider acknowledgement. A missing/mismatched token or an ownership transition that cannot be finalized is a core invariant violation and must fail the runtime, never return a provider-visible business error after durable commit.

Plan 04's `compiler.PreparedGeneration` is the permanent producer of these same domain leases: its prepared owners and dependency leases remain owned by the preparation/activation record from `Prepare` through `Activate`; `DiscardPrepared` closes the un-staged prepared generation; `RollbackActivation` restores the predecessor and releases the new generation; `FinalizeActivation` transfers active ownership and enqueues the predecessor for retirement. Plan 04 may replace `preparedActivation` internals, but it must not change the `generation.PublicationEngine` signatures or release predecessor leases before journal commit.

Until plan 04 replaces this classifier with the immutable compiler's closure-aware implementation, use this explicit conservative first-milestone classification. An invalid first resource in these kinds receives `DispositionFailClosed`; an invalid replacement may use exact predecessor bytes with `DispositionLastGood`. Other invalid first resources receive `DispositionQuarantined` and remain absent from the candidate.

```go
func securitySensitiveResource(key generation.ResourceKey) bool {
	switch key.Kind {
	case "routes", "services", "global_rules", "plugin_configs", "plugin_metadata", "plugins",
		"ssls", "secrets", "consumers", "consumer_groups", "stream_routes":
		return true
	default:
		return false
	}
}
```

- [ ] **Step 4: Change both provider constructors to require the coordinator**

```go
type ConfigClient struct {
	coordinator *generation.Coordinator
	// existing etcd transport and watch fields remain
}

func (c *ConfigClient) applyWatchResponse(ctx context.Context, response clientv3.WatchResponse) error {
	batch, err := desiredBatchFromEtcdWatch(c.prefix, response)
	if err != nil {
		return err
	}
	ack, err := c.coordinator.Apply(ctx, batch)
	if err != nil {
		return err
	}
	c.lastRevision = parseAcknowledgedEtcdRevision(ack.Cursor)
	c.commitAcknowledgedDecisions(ack.Decisions)
	return nil
}
```

Etcd `knownKeys`, quarantine and `lastRevision` advance only from the acknowledgement. A transient prepare/activate/journal error leaves them unchanged so watch recovery retries the same authoritative response.

`StandaloneFileWatcher` stores no authoritative `current` snapshot. It reads and normalizes the complete file, calls `Coordinator.Apply`, and derives `StandaloneReloadResult` from the returned decisions for observation only. A failed apply leaves the journal's desired revision durable when `ApplyDesired` succeeded but returns an error and does not advance an acknowledged file digest.

- [ ] **Step 5: Open and recover the journal before starting a provider**

```go
journalPath := config.JournalPath(effective)
if journalPath == "" {
	return errors.New("resolve generation journal path: effective config is nil")
}
journal, err := store.OpenJournal(journalPath, store.JournalOptions{
	LegacyResourceBuckets: store.ManagedResourceKinds(),
})
if err != nil {
	return fmt.Errorf("open generation journal: %w", err)
}
recovery, err := journal.Recover(ctx)
if err != nil {
	_ = journal.Close()
	return fmt.Errorf("recover generation journal: %w", err)
}
server.installRecovery(recovery)
server.journal = journal
server.coordinator = generation.NewCoordinator(journal, NewGenerationEngine(server))
```

When a verified HTTP or stream publication exists, install it before connecting to the provider and allow traffic to use it. Record provider stage unhealthy until the authoritative provider completes a fresh acknowledged apply; therefore offline last-good can serve while readiness remains false/degraded. A missing or corrupt required domain stays unready and is never rebuilt from desired state during offline startup.

- [ ] **Step 6: Delete the legacy event, bucket and in-memory last-good implementation**

Delete the four files listed above and remove every symbol named in the File and Responsibility Map. Remove `events chan *store.Event` from Server, etcd and standalone constructors. Remove direct resource-bucket creation from `InitBuckets`, old `processMutations`, hook registration, reload event scheduling and stream `atomic.Pointer` last-good.

Run these scans immediately after deletion:

```bash
rg -n 'NewEvent|NewAcknowledged|AddEventUpdateHook|AddAcknowledgedEventUpdateHook|HTTPConfigGeneration|PrepareStreamRoutes|CommitStreamRouteLastGood|streamRouteLastGood|configGeneration' --glob '*.go' cmd pkg
rg -n 'Bucket\(\[\]byte\("(routes|services|upstreams|global_rules|plugin_configs|plugin_metadata|consumers|consumer_groups|plugins|protos|ssls|stream_routes|secrets)"\)\)' --glob '*.go' pkg
```

Expected: both commands print no production persistence call site. Direct bucket access remains only in journal-format tests using journal bucket constants.

- [ ] **Step 7: Run provider, server and store cutover tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/generation ./pkg/store ./pkg/etcd ./pkg/config ./pkg/server ./pkg/observability/metrics -run "^(TestJournal|TestPublished|TestOffline|TestExplicitDelete|TestAcknowledg|TestDesiredBatchFrom|TestGenerationEngine|TestStandalone|TestApplyWatchResponse|TestApplySnapshot|TestConfigApply)" -count=1'`

Expected: PASS. Specifically, an etcd hook-style publication failure test no longer exists; its replacement proves desired revision is durable, provider revision is unacknowledged, published revision remains unchanged, and retry succeeds.

- [ ] **Step 8: Run build smoke and Windows source-build check**

Run: `bash -lc 'source .envrc && make build'`

Expected: PASS.

Run: `bash -lc 'source .envrc && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...'`

Expected: PASS or an already-recorded unrelated platform failure with exact package/file/line. No new journal code may import Unix-only APIs.

- [ ] **Step 9: Commit the atomic runtime cutover**

```bash
git add pkg/generation pkg/store pkg/etcd pkg/config pkg/server pkg/observability/metrics
git commit -m "refactor(runtime): cut providers over to generation journal"
```

---

### Task 10: Document the Durable Contract and Run the Milestone Gate

**Files:**

- Create: `docs/architecture/durable-generation-journal.md`
- Modify: `docs/design.md`
- Modify: `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program.md`

**Interfaces:**

- Consumes: accepted code signatures and bbolt schema from Tasks 1-9.
- Produces: the durable-format governance record used by compiler, supervisor, diagnostics and release evidence plans.

- [ ] **Step 1: Write the durable format document with exact compatibility rules**

The document must contain this bucket table and state machine using the implemented constant names and schema value:

```markdown
| Bucket | Key | Value | Commit owner |
| --- | --- | --- | --- |
| `generation_meta` | `schema_version` | uint64 big-endian `1` | migration only |
| `generation_desired_head` | `revision`, `artifact` | current desired revision and artifact ID | `ApplyDesired` |
| `generation_desired_revisions` | uint64 big-endian revision | desired artifact ID | migration/`ApplyDesired` |
| `generation_artifacts` | `sha256:<hex>` | integrity envelope and canonical snapshot | `ApplyDesired`/`Commit` |
| `generation_published_heads` | `http` or `stream` | artifact, published revision, closure and decision-record digest | `Commit` |
| `generation_publication_transactions` | random token | ticket and complete candidate set | `Stage`/`Abort`/`Commit` |
| `generation_provider_cursors` | `active_provider` or SHA-256 cursor identity | active authority, exact cursor, ordered batch digest and desired ticket | `ApplyDesired` |
| `generation_publication_decisions` | domain/revision | bounded per-resource decisions | `Commit` |

desired durable -> prepared -> staged durable -> reversibly activated -> published durable
                                    |                         | commit failure
                                    | activation failure      v
                                    +----------------------> rollback -> aborted, not acknowledged

published durable -> finalized (old owners retired) -> acknowledged
```

State explicitly that schema versions are forward-incompatible and unknown newer versions fail before mutation; migrations are one-way and transactional; legacy resource buckets import as desired-only then are deleted; `Snapshot` IDs are content hashes; explicit deletes cannot use last-good; published closures are atomic; HTTP and stream revisions advance independently; offline serving never marks the provider healthy. `DiscardPrepared` is the mandatory pre-stage cleanup boundary. `FinalizeActivation` is outside the recoverable business-error state machine: after the published-head transaction commits it may only complete the in-memory ownership transfer and enqueue the predecessor for asynchronous retirement. A missing token or impossible ownership state is fatal because returning a normal error would leave durable and active truth split.

- [ ] **Step 2: Update the total plan milestone paths and commit scope**

Change Task 3 of `2026-08-23-apisix-go-convergence-program.md` so its produced files and commit command include `pkg/config`, `pkg/server`, and `pkg/observability/metrics`, because the no-adapter cutover necessarily changes both providers and the runtime owner. Do not change shared interface names.

- [ ] **Step 3: Run plan-specific focused tests**

Run: `bash -lc 'source .envrc && go test -race ./pkg/generation ./pkg/store ./pkg/etcd ./pkg/config ./pkg/server ./pkg/observability/metrics -run "^(TestJournal|TestPublished|TestOffline|TestExplicitDelete|TestAcknowledg|TestDesiredBatchFrom|TestGenerationEngine|TestStandalone|TestApplyWatchResponse|TestApplySnapshot|TestConfigApply)" -count=1'`

Expected: PASS with no skipped package or test hidden by retries.

- [ ] **Step 4: Run scoped lint, build and diff gates**

```bash
bash -lc 'source .envrc && golangci-lint run ./pkg/generation/... ./pkg/store/... ./pkg/etcd/... ./pkg/config/... ./pkg/server/... ./pkg/observability/metrics/...'
bash -lc 'source .envrc && make build'
git diff --check
```

Expected: all commands PASS. If a pre-existing failure remains, record its exact package, file, line and message; do not describe that gate as passing.

- [ ] **Step 5: Verify the legacy persistence path is absent**

```bash
test ! -e pkg/store/event.go
test ! -e pkg/store/standalone_snapshot.go
! rg -n 'NewEvent|NewAcknowledged|AddEventUpdateHook|AddAcknowledgedEventUpdateHook|HTTPConfigGeneration|PrepareStreamRoutes|CommitStreamRouteLastGood|streamRouteLastGood|configGeneration' --glob '*.go' cmd pkg
! rg -n 'generation_artifacts|generation_published_heads|generation_desired_head' --glob '*.go' pkg/etcd pkg/config pkg/server
```

Expected: PASS. Only `pkg/store` knows journal bucket names; providers and runtime depend on `generation.Journal`.

- [ ] **Step 6: Commit documentation and master-plan alignment**

```bash
git add docs/architecture/durable-generation-journal.md docs/design.md docs/superpowers/plans/2026-08-23-apisix-go-convergence-program.md
git commit -m "docs(runtime): specify durable generation journal"
```

## Plan Self-Review

- Spec coverage: schema version, migrations, integrity, desired state, independent domain revisions, ack-after-publication-and-finalize, partial activation rollback, commit-failure rollback, restart last-good, explicit delete, per-resource security last-good/fail-closed, atomic dependency closure, offline degraded startup and unknown-newer-schema failure each have a named task and red/green test.
- Migration coverage: legacy resource buckets are imported exactly once as desired-only state and deleted transactionally; Task 9 deletes Store events, hooks and in-memory last-good rather than preserving a compatibility path.
- Interface consistency: every task uses `generation.Domain`, `generation.RevisionSet`, `generation.GenerationArtifact`, `generation.Journal`, `generation.PublicationEngine` and `generation.Coordinator` with the exact signatures declared under Shared Interfaces, including pre-stage `DiscardPrepared`, reversible `Activate`/`RollbackActivation`, and post-commit infallible `FinalizeActivation`.
- Dependency consistency: this plan consumes plan 02's `config.JournalPath(*config.EffectiveConfig)` for the database path. It produces the journal and reversible transaction boundary consumed by plan 04's trusted compiler/`PreparedGeneration` lease owner and the later supervisor plan; plan 04 remains the final owner of security-sensitive classification.
- Verification scope: every Go command sources `.envrc`; tests are restricted to affected packages and focused names; race, lint and build gates occur only after the production cutover.
- Completeness scan: the plan contains no deferred implementation markers, generic error-handling requests, unspecified tests or undefined neighboring interfaces.
