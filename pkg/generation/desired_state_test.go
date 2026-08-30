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
