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
	candidate := set.Domains[generation.DomainHTTP]
	streamCandidate := set.Domains[generation.DomainStream]
	slices.Reverse(candidate.Closure)
	slices.Reverse(candidate.Decisions)
	set.Domains = map[generation.Domain]generation.PublicationCandidate{
		generation.DomainStream: streamCandidate,
		generation.DomainHTTP:   candidate,
	}
	second := CandidateAttemptID(ticket, set)
	if first == (AttemptID{}) || first != second {
		t.Fatalf("permuted candidate IDs = %x/%x, want equal non-zero IDs", first, second)
	}
}

func TestRecoveryAttemptIDIsPermutationStable(t *testing.T) {
	revisions := generation.RevisionSet{Desired: 17, HTTP: 13, Stream: 11}
	httpPublished := generation.PublishedGeneration(attemptPublicationSet(t, false).Domains[generation.DomainHTTP])
	streamCandidate := attemptPublicationSet(t, true).Domains[generation.DomainStream]
	streamPublished := generation.PublishedGeneration(streamCandidate)

	first := RecoveryAttemptID(revisions, map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP:   httpPublished,
		generation.DomainStream: streamPublished,
	})
	slices.Reverse(httpPublished.Closure)
	slices.Reverse(httpPublished.Decisions)
	second := RecoveryAttemptID(revisions, map[generation.Domain]generation.PublishedGeneration{
		generation.DomainStream: streamPublished,
		generation.DomainHTTP:   httpPublished,
	})
	if first == (AttemptID{}) || first != second {
		t.Fatalf("permuted recovery IDs = %x/%x, want equal non-zero IDs", first, second)
	}
}

func TestAttemptIDChangesForEveryCandidateMaterialIdentity(t *testing.T) {
	baseTicket := attemptTicket()
	baseSet := attemptPublicationSet(t, false)
	want := CandidateAttemptID(baseTicket, baseSet)
	if want == (AttemptID{}) {
		t.Fatal("base candidate ID is zero")
	}

	cases := map[string]func(generation.ApplyTicket, generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet){
		"ticket desired revision": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			ticket.DesiredRevision++
			return ticket, set
		},
		"ticket desired digest": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			ticket.DesiredDigest[0]++
			return ticket, set
		},
		"ticket cursor provider": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			ticket.Cursor.Provider += "-changed"
			return ticket, set
		},
		"ticket cursor revision": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			ticket.Cursor.Revision += "-changed"
			return ticket, set
		},
		"ticket required domains": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			ticket.RequiredDomains = append(ticket.RequiredDomains, generation.DomainStream)
			return ticket, set
		},
		"set desired revision": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			set.DesiredRevision++
			return ticket, set
		},
		"candidate artifact domain": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Artifact.Domain = generation.DomainStream
			set.Domains[generation.DomainHTTP] = candidate
			return ticket, set
		},
		"candidate artifact revision": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Artifact.Revision++
			set.Domains[generation.DomainHTTP] = candidate
			return ticket, set
		},
		"candidate artifact digest": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Artifact.Digest[0]++
			set.Domains[generation.DomainHTTP] = candidate
			return ticket, set
		},
		"candidate artifact snapshot": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Artifact.Snapshot += "-changed"
			set.Domains[generation.DomainHTTP] = candidate
			return ticket, set
		},
		"candidate snapshot": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Snapshot = attemptSnapshot(t, []byte("changed"))
			set.Domains[generation.DomainHTTP] = candidate
			return ticket, set
		},
		"candidate closure": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Closure = append(candidate.Closure, generation.ResourceKey{Kind: "routes", ID: "r2"})
			set.Domains[generation.DomainHTTP] = candidate
			return ticket, set
		},
		"candidate decision key": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Decisions[0].Key.ID += "-changed"
			set.Domains[generation.DomainHTTP] = candidate
			return ticket, set
		},
		"candidate decision disposition": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Decisions[0].Disposition = generation.DispositionLastGood
			set.Domains[generation.DomainHTTP] = candidate
			return ticket, set
		},
		"candidate decision code": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			candidate.Decisions[0].Code += "-changed"
			set.Domains[generation.DomainHTTP] = candidate
			return ticket, set
		},
		"publication domain": func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
			candidate := set.Domains[generation.DomainHTTP]
			set.Domains = map[generation.Domain]generation.PublicationCandidate{
				generation.DomainStream: candidate,
			}
			return ticket, set
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ticket, set := cloneAttemptInputs(baseTicket, baseSet)
			got := CandidateAttemptID(mutate(ticket, set))
			if got == (AttemptID{}) || got == want {
				t.Fatalf("mutated candidate ID = %x, baseline = %x", got, want)
			}
		})
	}
}

func TestAttemptIDChangesForEveryRecoveryMaterialIdentity(t *testing.T) {
	baseRevisions := generation.RevisionSet{Desired: 17, HTTP: 13, Stream: 11}
	basePublished := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: generation.PublishedGeneration(
			attemptPublicationSet(t, false).Domains[generation.DomainHTTP],
		),
	}
	want := RecoveryAttemptID(baseRevisions, basePublished)
	if want == (AttemptID{}) {
		t.Fatal("base recovery ID is zero")
	}

	cases := map[string]func(generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration){
		"desired revision": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			revisions.Desired++
			return revisions, published
		},
		"http revision": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			revisions.HTTP++
			return revisions, published
		},
		"stream revision": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			revisions.Stream++
			return revisions, published
		},
		"published domain": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			published[generation.DomainStream] = published[generation.DomainHTTP]
			return revisions, published
		},
		"published artifact domain": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			value := published[generation.DomainHTTP]
			value.Artifact.Domain = generation.DomainStream
			published[generation.DomainHTTP] = value
			return revisions, published
		},
		"published artifact revision": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			value := published[generation.DomainHTTP]
			value.Artifact.Revision++
			published[generation.DomainHTTP] = value
			return revisions, published
		},
		"published artifact digest": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			value := published[generation.DomainHTTP]
			value.Artifact.Digest[0]++
			published[generation.DomainHTTP] = value
			return revisions, published
		},
		"published artifact snapshot": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			value := published[generation.DomainHTTP]
			value.Artifact.Snapshot += "-changed"
			published[generation.DomainHTTP] = value
			return revisions, published
		},
		"published snapshot": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			value := published[generation.DomainHTTP]
			value.Snapshot = attemptSnapshot(t, []byte("changed"))
			published[generation.DomainHTTP] = value
			return revisions, published
		},
		"published closure": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			value := published[generation.DomainHTTP]
			value.Closure = append(value.Closure, generation.ResourceKey{Kind: "routes", ID: "r2"})
			published[generation.DomainHTTP] = value
			return revisions, published
		},
		"published decision": func(revisions generation.RevisionSet, published map[generation.Domain]generation.PublishedGeneration) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
			value := published[generation.DomainHTTP]
			value.Decisions[0].Code += "-changed"
			published[generation.DomainHTTP] = value
			return revisions, published
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			revisions, published := cloneRecoveryInputs(baseRevisions, basePublished)
			got := RecoveryAttemptID(mutate(revisions, published))
			if got == (AttemptID{}) || got == want {
				t.Fatalf("mutated recovery ID = %x, baseline = %x", got, want)
			}
		})
	}
}

func TestAttemptIDSeparatesCandidateAndRecoveryAtSameDesiredRevision(t *testing.T) {
	ticket := attemptTicket()
	set := attemptPublicationSet(t, false)
	candidate := CandidateAttemptID(ticket, set)
	published := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: generation.PublishedGeneration(set.Domains[generation.DomainHTTP]),
	}
	recovery := RecoveryAttemptID(generation.RevisionSet{Desired: ticket.DesiredRevision}, published)
	if candidate == (AttemptID{}) || recovery == (AttemptID{}) || candidate == recovery {
		t.Fatalf("candidate/recovery IDs = %x/%x, want distinct non-zero IDs", candidate, recovery)
	}
}

func TestRecoveryAttemptIDBindsOuterDomainKey(t *testing.T) {
	revisions := generation.RevisionSet{Desired: 17, HTTP: 13, Stream: 11}
	value := generation.PublishedGeneration(attemptPublicationSet(t, false).Domains[generation.DomainHTTP])
	withHTTP := RecoveryAttemptID(revisions, map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: value,
	})
	withStream := RecoveryAttemptID(revisions, map[generation.Domain]generation.PublishedGeneration{
		generation.DomainStream: value,
	})
	if withHTTP == (AttemptID{}) || withStream == (AttemptID{}) || withHTTP == withStream {
		t.Fatalf("outer-domain IDs = %x/%x, want distinct non-zero IDs", withHTTP, withStream)
	}
}

func TestCandidateAttemptIDV1Golden(t *testing.T) {
	got := CandidateAttemptID(attemptTicket(), attemptPublicationSet(t, false))
	const want = "83b351c0929dfcc65d9a9e2d8a4949e87b9b6f7984fb09039ada8684a6cadc9d"
	if encoded := hex.EncodeToString(got[:]); encoded != want {
		t.Fatalf("candidate v1 ID = %s, want %s", encoded, want)
	}
}

func TestRecoveryAttemptIDV1Golden(t *testing.T) {
	revisions := generation.RevisionSet{Desired: 17, HTTP: 13, Stream: 11}
	httpPublication := attemptPublicationSet(t, false).Domains[generation.DomainHTTP]
	published := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: generation.PublishedGeneration(httpPublication),
	}
	got := RecoveryAttemptID(revisions, published)
	const want = "e74d831dcb597391b2a5debd1fde629ec04d9ad8011b9b7ef09ea0e0b8179ce9"
	if encoded := hex.EncodeToString(got[:]); encoded != want {
		t.Fatalf("recovery v1 ID = %s, want %s", encoded, want)
	}
}

func TestAttemptIDLengthPrefixesPreventStringConcatenationAmbiguity(t *testing.T) {
	left := attemptTicket()
	left.Cursor.Provider = "a"
	left.Cursor.Revision = "bc"
	right := left
	right.Cursor.Provider = "ab"
	right.Cursor.Revision = "c"
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
	if !slices.Equal(candidate.Closure, originalClosure) {
		t.Fatalf("closure mutated from %v to %v", originalClosure, candidate.Closure)
	}
	if !slices.Equal(candidate.Decisions, originalDecisions) {
		t.Fatalf("decisions mutated from %v to %v", originalDecisions, candidate.Decisions)
	}
}

func attemptTicket() generation.ApplyTicket {
	return generation.ApplyTicket{
		DesiredRevision: 17,
		DesiredDigest:   sha256.Sum256([]byte("desired")),
		Cursor: generation.ProviderCursor{
			Provider: "etcd",
			Revision: "17",
		},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
}

func attemptPublicationSet(t *testing.T, includeStream bool) generation.PublicationSet {
	t.Helper()
	httpSnapshot := attemptSnapshot(t, []byte("http"))
	http := attemptPublicationCandidate(httpSnapshot, generation.DomainHTTP)
	set := generation.PublicationSet{
		DesiredRevision: 17,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: http,
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
			Domain:   domain,
			Revision: snapshot.Revision(),
			Digest:   snapshot.Digest(),
			Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure: []generation.ResourceKey{
			{Kind: "routes", ID: "r1"},
			{Kind: "services", ID: "s1"},
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

func cloneRecoveryInputs(
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
	clone := make(map[generation.Domain]generation.PublishedGeneration, len(published))
	for domain, value := range published {
		value.Closure = slices.Clone(value.Closure)
		value.Decisions = slices.Clone(value.Decisions)
		clone[domain] = value
	}
	return revisions, clone
}
