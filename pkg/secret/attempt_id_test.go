package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
)

func TestCandidateAttemptIDIsPermutationStable(t *testing.T) {
	ticket := attemptTicket()
	ticket.RequiredDomains = []generation.Domain{generation.DomainHTTP, generation.DomainStream}
	set := attemptPublicationSet(t, true)
	first := CandidateAttemptID(ticket, set)

	ticket.RequiredDomains = []generation.Domain{generation.DomainStream, generation.DomainHTTP}
	http := set.Domains[generation.DomainHTTP]
	stream := set.Domains[generation.DomainStream]
	slices.Reverse(http.Closure)
	slices.Reverse(http.Decisions)
	set.Domains = map[generation.Domain]generation.PublicationCandidate{
		generation.DomainStream: stream,
		generation.DomainHTTP:   http,
	}
	second := CandidateAttemptID(ticket, set)
	if first == (AttemptID{}) || first != second {
		t.Fatalf("permuted candidate IDs = %x/%x, want equal non-zero IDs", first, second)
	}
}

func TestCandidateAttemptIDBindsPublicationIdentity(t *testing.T) {
	baseTicket := attemptTicket()
	baseSet := attemptPublicationSet(t, false)
	want := CandidateAttemptID(baseTicket, baseSet)
	if want == (AttemptID{}) {
		t.Fatal("base candidate ID is zero")
	}
	tests := map[string]func(*generation.ApplyTicket, *generation.PublicationSet){
		"desired revision": func(ticket *generation.ApplyTicket, _ *generation.PublicationSet) {
			ticket.DesiredRevision++
		},
		"desired digest": func(ticket *generation.ApplyTicket, _ *generation.PublicationSet) {
			ticket.DesiredDigest[0]++
		},
		"provider cursor": func(ticket *generation.ApplyTicket, _ *generation.PublicationSet) {
			ticket.Cursor.Revision += "-changed"
		},
		"required domains": func(ticket *generation.ApplyTicket, _ *generation.PublicationSet) {
			ticket.RequiredDomains = append(ticket.RequiredDomains, generation.DomainStream)
		},
		"artifact": func(_ *generation.ApplyTicket, set *generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Artifact.Digest[0]++
			set.Domains[generation.DomainHTTP] = candidate
		},
		"snapshot": func(_ *generation.ApplyTicket, set *generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Snapshot = attemptSnapshot(t, []byte("changed"))
			set.Domains[generation.DomainHTTP] = candidate
		},
		"closure": func(_ *generation.ApplyTicket, set *generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Closure = append(candidate.Closure, generation.ResourceKey{Kind: "routes", ID: "r2"})
			set.Domains[generation.DomainHTTP] = candidate
		},
		"decision": func(_ *generation.ApplyTicket, set *generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Decisions[0].Code += "-changed"
			set.Domains[generation.DomainHTTP] = candidate
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			ticket, set := cloneAttemptInputs(baseTicket, baseSet)
			mutate(&ticket, &set)
			if got := CandidateAttemptID(ticket, set); got == (AttemptID{}) || got == want {
				t.Fatalf("mutated candidate ID = %x, baseline = %x", got, want)
			}
		})
	}
}

func TestCandidateAttemptIDV1Golden(t *testing.T) {
	got := CandidateAttemptID(attemptTicket(), attemptPublicationSet(t, false))
	const want = "83b351c0929dfcc65d9a9e2d8a4949e87b9b6f7984fb09039ada8684a6cadc9d"
	if encoded := hex.EncodeToString(got[:]); encoded != want {
		t.Fatalf("candidate v1 ID = %s, want %s", encoded, want)
	}
}

func TestAttemptIDLengthPrefixesPreventStringConcatenationAmbiguity(t *testing.T) {
	left := attemptTicket()
	left.Cursor.Provider, left.Cursor.Revision = "a", "bc"
	right := left
	right.Cursor.Provider, right.Cursor.Revision = "ab", "c"
	set := attemptPublicationSet(t, false)
	if first, second := CandidateAttemptID(left, set), CandidateAttemptID(right, set); first == second {
		t.Fatalf("length-prefix collision = %x", first)
	}
}

func TestAttemptIDEncodingDoesNotMutateInputs(t *testing.T) {
	ticket := attemptTicket()
	set := attemptPublicationSet(t, false)
	originalDomains := slices.Clone(ticket.RequiredDomains)
	originalClosure := slices.Clone(set.Domains[generation.DomainHTTP].Closure)
	originalDecisions := slices.Clone(set.Domains[generation.DomainHTTP].Decisions)
	_ = CandidateAttemptID(ticket, set)

	if !slices.Equal(ticket.RequiredDomains, originalDomains) {
		t.Fatalf("required domains mutated from %v to %v", originalDomains, ticket.RequiredDomains)
	}
	candidate := set.Domains[generation.DomainHTTP]
	if !slices.Equal(candidate.Closure, originalClosure) || !slices.Equal(candidate.Decisions, originalDecisions) {
		t.Fatal("candidate identity slices were mutated")
	}
}

func attemptTicket() generation.ApplyTicket {
	return generation.ApplyTicket{
		DesiredRevision: 17,
		DesiredDigest:   sha256.Sum256([]byte("desired")),
		Cursor:          generation.ProviderCursor{Provider: "etcd", Revision: "17"},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
}

func attemptPublicationSet(t *testing.T, includeStream bool) generation.PublicationSet {
	t.Helper()
	httpSnapshot := attemptSnapshot(t, []byte("http"))
	set := generation.PublicationSet{
		DesiredRevision: 17,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: attemptPublicationCandidate(httpSnapshot, generation.DomainHTTP),
		},
	}
	if includeStream {
		streamSnapshot := attemptSnapshot(t, []byte("stream"))
		set.Domains[generation.DomainStream] = attemptPublicationCandidate(streamSnapshot, generation.DomainStream)
	}
	return set
}

func attemptSnapshot(t *testing.T, value []byte) generation.Snapshot {
	t.Helper()
	snapshot, err := generation.NewSnapshot(17, []generation.Resource{
		{Key: generation.ResourceKey{Kind: "routes", ID: "r1"}, Value: value},
		{Key: generation.ResourceKey{Kind: "services", ID: "s1"}, Value: []byte("service")},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func attemptPublicationCandidate(
	snapshot generation.Snapshot,
	domain generation.Domain,
) generation.PublicationCandidate {
	return generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: domain, Revision: snapshot.Revision(), Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure: []generation.ResourceKey{
			{Kind: "routes", ID: "r1"}, {Kind: "services", ID: "s1"},
		},
		Decisions: []generation.ResourceDecision{
			{
				Key:         generation.ResourceKey{Kind: "routes", ID: "r1"},
				Disposition: generation.DispositionPublished,
				Code:        "ok",
			},
			{
				Key:         generation.ResourceKey{Kind: "services", ID: "s1"},
				Disposition: generation.DispositionPublished,
				Code:        "ok",
			},
		},
	}
}

func cloneAttemptInputs(
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (generation.ApplyTicket, generation.PublicationSet) {
	ticket.RequiredDomains = slices.Clone(ticket.RequiredDomains)
	clone := generation.PublicationSet{
		DesiredRevision: set.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(set.Domains)),
	}
	for domain, candidate := range set.Domains {
		candidate.Closure = slices.Clone(candidate.Closure)
		candidate.Decisions = slices.Clone(candidate.Decisions)
		clone.Domains[domain] = candidate
	}
	return ticket, clone
}
