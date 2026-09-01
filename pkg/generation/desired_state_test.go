package generation

import (
	"errors"
	"testing"
)

func TestDesiredStateRejectsInvalidBatches(t *testing.T) {
	tests := []struct {
		name  string
		batch DesiredBatch
	}{
		{name: "empty provider", batch: DesiredBatch{Cursor: ProviderCursor{Revision: "1"}}},
		{name: "empty revision", batch: DesiredBatch{Cursor: ProviderCursor{Provider: "etcd"}}},
		{
			name: "invalid mutation",
			batch: DesiredBatch{
				Cursor:          ProviderCursor{Provider: "etcd", Revision: "1"},
				Mutations:       []Mutation{{Type: "patch", Key: ResourceKey{Kind: "routes", ID: "r1"}}},
				RequiredDomains: []Domain{DomainHTTP},
			},
		},
		{
			name: "replacement missing stream domain",
			batch: DesiredBatch{
				Cursor: ProviderCursor{Provider: "etcd", Revision: "1"}, ReplaceManaged: true,
				RequiredDomains: []Domain{DomainHTTP},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newDesiredState()
			if _, err := state.candidate(test.batch); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("candidate error = %v", err)
			}
		})
	}
}

func TestDesiredStateCommitRejectsMismatchedPublicationSet(t *testing.T) {
	state := newDesiredState()
	candidate, err := state.candidate(coordinatorBatch("1", "r1"))
	if err != nil {
		t.Fatal(err)
	}
	set := PublicationSet{DesiredRevision: candidate.ticket.DesiredRevision, Domains: map[Domain]PublicationCandidate{}}
	if _, err := state.commit(candidate, set); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("commit error = %v", err)
	}
	if state.snapshot.Revision() != 0 {
		t.Fatalf("state revision after rejected commit = %d", state.snapshot.Revision())
	}
}

func TestDesiredStateCarriesAPISIXSourceIdentity(t *testing.T) {
	state := newDesiredState()
	origin := ResourceOrigin{
		Provider: "standalone", ResourceKey: "/routes/r1", ModifiedIndex: "1700000000",
	}
	candidate, err := state.candidate(DesiredBatch{
		Cursor: ProviderCursor{Provider: "standalone", Revision: "digest-1"},
		Mutations: []Mutation{{
			Type: MutationPut, Key: ResourceKey{Kind: "routes", ID: "r1"},
			Origin: origin, Value: []byte(`{"id":"r1"}`),
		}},
		CollectionVersions: map[string]string{"routes": "1700000000"},
		RequiredDomains:    []Domain{DomainHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	resources := candidate.snapshot.Resources()
	if len(resources) != 1 || resources[0].Origin != origin {
		t.Fatalf("candidate resource origin = %#v, want %#v", resources, origin)
	}
	if got, ok := candidate.snapshot.CollectionVersion("routes"); !ok || got != "1700000000" {
		t.Fatalf("candidate routes collection version = %q, %t", got, ok)
	}

	replayWithoutSource := DesiredBatch{
		Cursor: ProviderCursor{Provider: "standalone", Revision: "digest-1"},
		Mutations: []Mutation{
			{Type: MutationPut, Key: ResourceKey{Kind: "routes", ID: "r1"}, Value: []byte(`{"id":"r1"}`)},
		},
		RequiredDomains: []Domain{DomainHTTP},
	}
	state.cursor = candidate.ticket.Cursor
	state.batchDigest = candidate.batchDigest
	state.acknowledgement.Revisions.Desired = 1
	if _, err := state.candidate(replayWithoutSource); !errors.Is(err, ErrCursorConflict) {
		t.Fatalf("source identity change with same cursor error = %v, want cursor conflict", err)
	}
}
