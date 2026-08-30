package compiler

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
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

func TestFinalizePublicationRejectsForgedPostRefinementSet(t *testing.T) {
	desired := mustGenerationSnapshot(t, 44, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1"}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	set, err := newTestCompiler(t).PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}

	forged := generation.PublicationSet{
		DesiredRevision: set.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(set.Domains)),
	}
	maps.Copy(forged.Domains, set.Domains)
	candidate := forged.Domains[generation.DomainHTTP]
	candidate.Decisions = nil
	forged.Domains[generation.DomainHTTP] = candidate

	got, err := finalizePublication(ticket, forged)
	if !errors.Is(err, generation.ErrInvalidClosure) {
		t.Fatalf("finalize forged publication error = %v, want ErrInvalidClosure", err)
	}
	if got.DesiredRevision != 0 || len(got.Domains) != 0 {
		t.Fatalf("finalize forged publication returned partial set: %#v", got)
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
		{Kind: "plugins", ID: "plugins"},
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

func TestCompilerRejectsSameFutureAndStructurallyInvalidPredecessors(t *testing.T) {
	desired := mustGenerationSnapshot(t, 18, []generation.Resource{
		resourceValue("routes", "r", `{"id":"r"}`),
	}, nil)
	validPrevious := publishedForDomain(
		generation.DomainHTTP,
		mustGenerationSnapshot(t, 17, []generation.Resource{
			resourceValue("routes", "r", `{"id":"r"}`),
		}, nil),
	)

	tests := []struct {
		name        string
		previous    generation.PublishedGeneration
		wantClosure bool
	}{
		{
			name: "same revision",
			previous: publishedForDomain(
				generation.DomainHTTP,
				mustGenerationSnapshot(t, 18, []generation.Resource{
					resourceValue("routes", "r", `{"id":"r"}`),
				}, nil),
			),
		},
		{
			name: "future revision",
			previous: publishedForDomain(
				generation.DomainHTTP,
				mustGenerationSnapshot(t, 19, []generation.Resource{
					resourceValue("routes", "r", `{"id":"r"}`),
				}, nil),
			),
		},
		{
			name: "forged artifact digest",
			previous: func() generation.PublishedGeneration {
				previous := clonePublishedGeneration(validPrevious)
				previous.Artifact.Digest[0]++
				return previous
			}(),
		},
		{
			name: "forged artifact snapshot",
			previous: func() generation.PublishedGeneration {
				previous := clonePublishedGeneration(validPrevious)
				previous.Artifact.Snapshot = "sha256:forged"
				return previous
			}(),
		},
		{
			name: "missing closure",
			previous: func() generation.PublishedGeneration {
				previous := clonePublishedGeneration(validPrevious)
				previous.Closure = nil
				return previous
			}(),
			wantClosure: true,
		},
		{
			name: "duplicate closure",
			previous: func() generation.PublishedGeneration {
				previous := clonePublishedGeneration(validPrevious)
				previous.Closure = append(previous.Closure, previous.Closure[0])
				return previous
			}(),
			wantClosure: true,
		},
		{
			name: "missing decision",
			previous: func() generation.PublishedGeneration {
				previous := clonePublishedGeneration(validPrevious)
				previous.Decisions = nil
				return previous
			}(),
			wantClosure: true,
		},
		{
			name: "invalid disposition",
			previous: func() generation.PublishedGeneration {
				previous := clonePublishedGeneration(validPrevious)
				previous.Decisions[0].Disposition = generation.ResourceDisposition("invalid")
				return previous
			}(),
			wantClosure: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newTestCompiler(t).PreparePublication(
				context.Background(),
				ticketForSnapshot(desired, generation.DomainHTTP),
				desired,
				map[generation.Domain]generation.PublishedGeneration{
					generation.DomainHTTP: test.previous,
				},
			)
			if !errors.Is(err, generation.ErrIntegrity) {
				t.Fatalf("predecessor error = %v, want ErrIntegrity", err)
			}
			if test.wantClosure && !errors.Is(err, generation.ErrInvalidClosure) {
				t.Fatalf("predecessor error = %v, want ErrInvalidClosure", err)
			}
		})
	}
}

func newTestCompiler(t *testing.T) *Compiler {
	t.Helper()
	compiler, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return compiler
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
