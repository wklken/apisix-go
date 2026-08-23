package compiler

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
)

func TestCompilerRejectsTicketSnapshotAndDependencyContractViolations(t *testing.T) {
	compiler := newTestCompiler(t)
	desired := mustGenerationSnapshot(t, 13, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","upstream_id":"missing"}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)

	badRevision := ticket
	badRevision.DesiredRevision++
	if _, err := compiler.PreparePublication(
		context.Background(),
		badRevision,
		desired,
		nil,
	); !errors.Is(
		err,
		generation.ErrIntegrity,
	) {
		t.Fatalf("revision mismatch error = %v, want ErrIntegrity", err)
	}
	badDigest := ticket
	badDigest.DesiredDigest[0]++
	if _, err := compiler.PreparePublication(
		context.Background(),
		badDigest,
		desired,
		nil,
	); !errors.Is(
		err,
		generation.ErrIntegrity,
	) {
		t.Fatalf("digest mismatch error = %v, want ErrIntegrity", err)
	}
	set, err := compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(
		t,
		set.Domains[generation.DomainHTTP],
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		generation.DispositionQuarantined,
		"dependency-unavailable",
	)
	if compiler.dependencies.Resources.Len() != 0 {
		t.Fatal("pure compiler acquired a runtime resource")
	}
	residuals, err := compiler.dependencies.Tasks.Stop(context.Background())
	if err != nil || len(residuals) != 0 {
		t.Fatalf("pure compiler task registry = %v/%v, want no started tasks", residuals, err)
	}
}

func TestCompilerStopsWhenContextIsCanceledAfterCompilationStarts(t *testing.T) {
	desired := mustGenerationSnapshot(t, 23, []generation.Resource{
		resourceValue("routes", "a", `{"id":"a"}`),
		resourceValue("routes", "b", `{"id":"b"}`),
		resourceValue("routes", "c", `{"id":"c"}`),
	}, nil)
	base, cancel := context.WithCancel(context.Background())
	ctx := &cancelOnCheckContext{Context: base, cancel: cancel, cancelAt: 3}
	_, err := newTestCompiler(t).PreparePublication(
		ctx,
		ticketForSnapshot(desired, generation.DomainHTTP),
		desired,
		nil,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("started compilation cancellation error = %v, want context.Canceled", err)
	}
	if got := ctx.checks.Load(); got < ctx.cancelAt {
		t.Fatalf("context checks = %d, want cancellation after compilation started", got)
	}
}

type cancelOnCheckContext struct {
	context.Context
	cancel   context.CancelFunc
	cancelAt int32
	checks   atomic.Int32
}

func (ctx *cancelOnCheckContext) Err() error {
	if ctx.checks.Add(1) == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestCompilerBuildsCanonicalCandidatesIncludingEmptyRequiredDomain(t *testing.T) {
	compiler := newTestCompiler(t)
	empty := mustGenerationSnapshot(t, 14, nil, nil)
	noDomains, err := compiler.PreparePublication(context.Background(), ticketForSnapshot(empty), empty, nil)
	if err != nil {
		t.Fatal(err)
	}
	if noDomains.DesiredRevision != 14 || len(noDomains.Domains) != 0 {
		t.Fatalf("zero-required-domain publication = %#v", noDomains)
	}
	set, err := compiler.PreparePublication(
		context.Background(),
		ticketForSnapshot(empty, generation.DomainHTTP),
		empty,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	candidate, ok := set.Domains[generation.DomainHTTP]
	if !ok || len(candidate.Closure) != 0 || len(candidate.Decisions) != 0 || len(candidate.Snapshot.Resources()) != 0 {
		t.Fatalf("empty candidate = %#v", candidate)
	}

	rawRoute := []byte(` {"id":"a","upstream_id":"z","exact":9007199254740993} `)
	desired := mustGenerationSnapshot(t, 15, []generation.Resource{
		resourceValue("upstreams", "z", `{"id":"z"}`),
		resourceValue("routes", "b", `{"id":"b","upstream_id":"z"}`),
		{Key: generation.ResourceKey{Kind: "routes", ID: "a"}, Value: rawRoute},
	}, nil)
	set, err = compiler.PreparePublication(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []generation.ResourceKey{{Kind: "routes", ID: "a"}, {Kind: "routes", ID: "b"}, {Kind: "upstreams", ID: "z"}}
	if got := set.Domains[generation.DomainHTTP].Closure; !slices.Equal(got, want) {
		t.Fatalf("closure = %v, want %v", got, want)
	}
	gotRaw, found := set.Domains[generation.DomainHTTP].Snapshot.Lookup(generation.ResourceKey{Kind: "routes", ID: "a"})
	if !found || !bytes.Equal(gotRaw, rawRoute) {
		t.Fatalf("published bytes = %q/%v, want exact %q", gotRaw, found, rawRoute)
	}
}

func TestCompilerNewRequiresManifestAndCompleteRuntimeDependencies(t *testing.T) {
	if _, err := New(nil, runtime.RuntimeDependencies{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil manifest error = %v, want ErrInvalidInput", err)
	}
	if _, err := New(mustManifest(t), runtime.RuntimeDependencies{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("incomplete dependencies error = %v, want ErrInvalidInput", err)
	}
	if _, err := New(&capability.Manifest{}, testRuntimeDependencies(t)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unvalidated manifest error = %v, want ErrInvalidInput", err)
	}
}

func TestCompilerCoversEveryManagedKindInCanonicalDomainCandidates(t *testing.T) {
	desired := mustGenerationSnapshot(t, 16, []generation.Resource{
		resourceValue("routes", "r", `{"id":"r"}`),
		resourceValue("services", "svc", `{"id":"svc"}`),
		resourceValue("upstreams", "u", `{"id":"u"}`),
		resourceValue("global_rules", "g", `{"id":"g","plugins":{}}`),
		resourceValue("plugin_configs", "pc", `{"plugins":{}}`),
		resourceValue("plugin_metadata", "request-id", `{}`),
		resourceValue("consumers", "alice", `{"username":"alice","plugins":{}}`),
		resourceValue("consumer_groups", "staff", `{"plugins":{}}`),
		resourceValue("plugins", "plugins", `[{"name":"request-id","stream":false}]`),
		resourceValue("protos", "p", `{"id":"p"}`),
		resourceValue("ssls", "cert", `{"id":"cert"}`),
		resourceValue("stream_routes", "stream", `{"id":"stream"}`),
		resourceValue("secrets", "vault/team", `{}`),
	}, nil)
	set, err := newTestCompiler(t).PreparePublication(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP, generation.DomainStream),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateKeys(t, set.Domains[generation.DomainHTTP], []generation.ResourceKey{
		{Kind: "consumer_groups", ID: "staff"},
		{Kind: "consumers", ID: "alice"},
		{Kind: "global_rules", ID: "g"},
		{Kind: "plugin_configs", ID: "pc"},
		{Kind: "plugin_metadata", ID: "request-id"},
		{Kind: "plugins", ID: "plugins"},
		{Kind: "protos", ID: "p"},
		{Kind: "routes", ID: "r"},
		{Kind: "secrets", ID: "vault/team"},
		{Kind: "services", ID: "svc"},
		{Kind: "ssls", ID: "cert"},
		{Kind: "upstreams", ID: "u"},
	})
	assertCandidateKeys(t, set.Domains[generation.DomainStream], []generation.ResourceKey{
		{Kind: "secrets", ID: "vault/team"},
		{Kind: "services", ID: "svc"},
		{Kind: "stream_routes", ID: "stream"},
		{Kind: "upstreams", ID: "u"},
	})
}

func TestCompilerCoversEveryDomainRelevantTombstoneExactlyOnce(t *testing.T) {
	desired := mustGenerationSnapshot(t, 19, nil, []generation.Tombstone{
		{Key: generation.ResourceKey{Kind: "routes", ID: "http"}, Revision: 19},
		{Key: generation.ResourceKey{Kind: "secrets", ID: "vault/shared"}, Revision: 19},
		{Key: generation.ResourceKey{Kind: "services", ID: "shared"}, Revision: 19},
		{Key: generation.ResourceKey{Kind: "ssls", ID: "http-cert"}, Revision: 19},
		{Key: generation.ResourceKey{Kind: "stream_routes", ID: "stream"}, Revision: 19},
	})
	set, err := newTestCompiler(t).PreparePublication(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP, generation.DomainStream),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateKeys(t, set.Domains[generation.DomainHTTP], []generation.ResourceKey{
		{Kind: "routes", ID: "http"},
		{Kind: "secrets", ID: "vault/shared"},
		{Kind: "services", ID: "shared"},
		{Kind: "ssls", ID: "http-cert"},
	})
	assertCandidateKeys(t, set.Domains[generation.DomainStream], []generation.ResourceKey{
		{Kind: "secrets", ID: "vault/shared"},
		{Kind: "services", ID: "shared"},
		{Kind: "stream_routes", ID: "stream"},
	})
	for _, candidate := range set.Domains {
		for _, decision := range candidate.Decisions {
			if decision.Disposition != generation.DispositionDeleted || decision.Code != "explicit-delete" {
				t.Fatalf("tombstone decision = %#v", decision)
			}
		}
	}
}

func TestCompilerRejectsNonCanonicalRequiredDomains(t *testing.T) {
	desired := mustGenerationSnapshot(t, 17, nil, nil)
	ticket := ticketForSnapshot(desired, generation.DomainStream, generation.DomainHTTP)
	if _, err := newTestCompiler(
		t,
	).PreparePublication(context.Background(), ticket, desired, nil); !errors.Is(
		err,
		generation.ErrIntegrity,
	) {
		t.Fatalf("non-canonical domains error = %v, want ErrIntegrity", err)
	}
}

func TestCompilerRejectsCrossDomainOrCorruptPredecessor(t *testing.T) {
	desired := mustGenerationSnapshot(t, 18, []generation.Resource{
		resourceValue("routes", "r", `{"id":"r"}`),
	}, nil)
	previousSnapshot := mustGenerationSnapshot(t, 17, []generation.Resource{
		resourceValue("routes", "r", `{"id":"r"}`),
	}, nil)
	previous := publishedForDomain(generation.DomainStream, previousSnapshot)
	_, err := newTestCompiler(t).PreparePublication(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired,
		map[generation.Domain]generation.PublishedGeneration{generation.DomainHTTP: previous},
	)
	if !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("cross-domain predecessor error = %v, want ErrIntegrity", err)
	}
}

func TestCompilerPublicationIsAcceptedByRealJournalStage(t *testing.T) {
	journal, err := store.OpenJournal(filepath.Join(t.TempDir(), "journal.db"), store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "compiler-test", Revision: "1"},
		Mutations: []generation.Mutation{
			{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: "routes", ID: "r1"},
				Value: []byte(`{"id":"r1","upstream_id":"u1"}`),
			},
			{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: "upstreams", ID: "u1"},
				Value: []byte(`{"id":"u1"}`),
			},
		},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	desired, err := journal.LoadDesired(context.Background(), ticket.DesiredRevision)
	if err != nil {
		t.Fatal(err)
	}
	set, err := newTestCompiler(t).PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Stage(context.Background(), ticket, set); err != nil {
		t.Fatalf("real Journal.Stage rejected compiler publication: %v", err)
	}
}

type panicMaterializer struct{}

func (panicMaterializer) Materialize(context.Context, secret.Scope, string) (secret.Value, error) {
	panic("pure compiler materialized a secret")
}

func newTestCompiler(t *testing.T) *Compiler {
	t.Helper()
	compiler, err := New(mustManifest(t), testRuntimeDependencies(t))
	if err != nil {
		t.Fatal(err)
	}
	return compiler
}

func testRuntimeDependencies(t *testing.T) runtime.RuntimeDependencies {
	t.Helper()
	resources := runtime.NewResourceRegistry()
	if err := resources.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	tasks := runtime.NewTaskRegistry(
		context.Background(),
		func(runtime.TaskFailure) { panic("pure compiler started a task") },
	)
	if residuals, err := tasks.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("stop task sentinel = %v/%v", residuals, err)
	}
	return runtime.RuntimeDependencies{
		Config:    &config.EffectiveConfig{},
		Secrets:   panicMaterializer{},
		Resources: resources,
		Tasks:     tasks,
	}
}

func ticketForSnapshot(snapshot generation.Snapshot, domains ...generation.Domain) generation.ApplyTicket {
	return generation.ApplyTicket{
		DesiredRevision: snapshot.Revision(),
		DesiredDigest:   snapshot.Digest(),
		Cursor:          generation.ProviderCursor{Provider: "compiler-test", Revision: "1"},
		RequiredDomains: slices.Clone(domains),
	}
}

func assertCandidateKeys(t *testing.T, candidate generation.PublicationCandidate, want []generation.ResourceKey) {
	t.Helper()
	if !slices.Equal(candidate.Closure, want) {
		t.Fatalf("candidate closure = %v, want %v", candidate.Closure, want)
	}
	if len(candidate.Decisions) != len(want) {
		t.Fatalf("candidate decisions = %d, want %d", len(candidate.Decisions), len(want))
	}
	for index, key := range want {
		if candidate.Decisions[index].Key != key {
			t.Fatalf("candidate decision[%d] key = %v, want %v", index, candidate.Decisions[index].Key, key)
		}
	}
}
