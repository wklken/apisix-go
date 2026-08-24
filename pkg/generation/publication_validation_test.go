package generation

import (
	"errors"
	"testing"
)

func TestValidatePublicationCandidateAcceptsCompleteClosure(t *testing.T) {
	snapshot := publicationValidationSnapshot(t, 7)
	candidate := publicationValidationCandidate(snapshot, DomainHTTP)
	if err := ValidatePublicationCandidate(DomainHTTP, 7, candidate); err != nil {
		t.Fatalf("ValidatePublicationCandidate() error = %v", err)
	}
}

func TestValidatePublicationCandidateRejectsForgedIdentity(t *testing.T) {
	snapshot := publicationValidationSnapshot(t, 7)
	cases := map[string]func(*PublicationCandidate){
		"unknown requested domain": func(_ *PublicationCandidate) {},
		"zero requested revision":  func(_ *PublicationCandidate) {},
		"domain": func(candidate *PublicationCandidate) {
			candidate.Artifact.Domain = DomainStream
		},
		"revision": func(candidate *PublicationCandidate) {
			candidate.Artifact.Revision++
		},
		"digest": func(candidate *PublicationCandidate) {
			candidate.Artifact.Digest[0]++
		},
		"snapshot id": func(candidate *PublicationCandidate) {
			candidate.Artifact.Snapshot = "sha256:forged"
		},
		"snapshot revision": func(candidate *PublicationCandidate) {
			candidate.Snapshot = publicationValidationSnapshot(t, 8)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := publicationValidationCandidate(snapshot, DomainHTTP)
			mutate(&candidate)
			domain, revision := DomainHTTP, uint64(7)
			if name == "unknown requested domain" {
				domain = "unknown"
			}
			if name == "zero requested revision" {
				revision = 0
			}
			if err := ValidatePublicationCandidate(domain, revision, candidate); err != ErrIntegrity {
				t.Fatalf("ValidatePublicationCandidate() error = %v, want %v", err, ErrIntegrity)
			}
		})
	}
}

func TestValidatePublishedGenerationDelegatesCandidateValidation(t *testing.T) {
	snapshot := publicationValidationSnapshot(t, 7)
	candidate := publicationValidationCandidate(snapshot, DomainHTTP)
	published := PublishedGeneration(candidate)
	if err := ValidatePublishedGeneration(DomainHTTP, 7, published); err != nil {
		t.Fatalf("ValidatePublishedGeneration() error = %v", err)
	}
	cases := map[string]struct {
		domain   Domain
		revision uint64
		mutate   func(*PublishedGeneration)
	}{
		"unknown domain": {domain: "unknown", revision: 7},
		"zero revision":  {domain: DomainHTTP, revision: 0},
		"artifact domain": {
			domain: DomainHTTP, revision: 7,
			mutate: func(value *PublishedGeneration) { value.Artifact.Domain = DomainStream },
		},
		"artifact revision": {
			domain: DomainHTTP, revision: 7,
			mutate: func(value *PublishedGeneration) { value.Artifact.Revision++ },
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			value := published
			if test.mutate != nil {
				test.mutate(&value)
			}
			if err := ValidatePublishedGeneration(test.domain, test.revision, value); err != ErrIntegrity {
				t.Fatalf("ValidatePublishedGeneration() error = %v, want %v", err, ErrIntegrity)
			}
		})
	}
}

func TestValidatePublicationCandidateRejectsClosureAndDecisionGaps(t *testing.T) {
	snapshot := publicationValidationSnapshot(t, 7)
	cases := map[string]func(*PublicationCandidate){
		"duplicate closure": func(candidate *PublicationCandidate) {
			candidate.Closure = append(candidate.Closure, candidate.Closure[0])
		},
		"missing resource closure": func(candidate *PublicationCandidate) {
			candidate.Closure = candidate.Closure[1:]
		},
		"duplicate decision": func(candidate *PublicationCandidate) {
			candidate.Decisions = append(candidate.Decisions, candidate.Decisions[0])
		},
		"missing decision": func(candidate *PublicationCandidate) {
			candidate.Decisions = candidate.Decisions[1:]
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := publicationValidationCandidate(snapshot, DomainHTTP)
			mutate(&candidate)
			if err := ValidatePublicationCandidate(DomainHTTP, 7, candidate); err != ErrInvalidClosure {
				t.Fatalf("ValidatePublicationCandidate() error = %v, want %v", err, ErrInvalidClosure)
			}
		})
	}
}

func TestValidatePublicationCandidateRejectsCrossDomainResourcesAndTombstones(t *testing.T) {
	tests := map[string]struct {
		domain  Domain
		kind    string
		deleted bool
	}{
		"stream http resource":  {domain: DomainStream, kind: "routes"},
		"stream http tombstone": {domain: DomainStream, kind: "routes", deleted: true},
		"http stream resource":  {domain: DomainHTTP, kind: "stream_routes"},
		"http stream tombstone": {domain: DomainHTTP, kind: "stream_routes", deleted: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			key := ResourceKey{Kind: test.kind, ID: "cross-domain"}
			var resources []Resource
			var tombstones []Tombstone
			disposition := DispositionPublished
			if test.deleted {
				tombstones = []Tombstone{{Key: key, Revision: 7}}
				disposition = DispositionDeleted
			} else {
				resources = []Resource{{Key: key, Value: []byte(`{}`)}}
			}
			snapshot, err := NewSnapshot(7, resources, tombstones)
			if err != nil {
				t.Fatal(err)
			}
			candidate := PublicationCandidate{
				Artifact: GenerationArtifact{
					Domain: test.domain, Revision: 7, Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
				},
				Snapshot: snapshot,
				Closure:  []ResourceKey{key},
				Decisions: []ResourceDecision{{
					Key: key, Disposition: disposition, Code: "cross-domain",
				}},
			}
			if err := ValidatePublicationCandidate(test.domain, 7, candidate); err != ErrInvalidClosure {
				t.Fatalf("ValidatePublicationCandidate() error = %v, want %v", err, ErrInvalidClosure)
			}
		})
	}
}

func TestValidatePublicationSetRejectsInvalidTicketDomainsAndRevision(t *testing.T) {
	snapshot := publicationValidationSnapshot(t, 7)
	candidate := publicationValidationCandidate(snapshot, DomainHTTP)
	cases := map[string]func(*ApplyTicket, *PublicationSet){
		"zero desired revision": func(ticket *ApplyTicket, _ *PublicationSet) {
			ticket.DesiredRevision = 0
		},
		"set revision mismatch": func(_ *ApplyTicket, set *PublicationSet) {
			set.DesiredRevision++
		},
		"unknown ticket domain": func(ticket *ApplyTicket, set *PublicationSet) {
			ticket.RequiredDomains = []Domain{"unknown"}
			set.Domains = map[Domain]PublicationCandidate{"unknown": candidate}
		},
		"unknown set domain": func(_ *ApplyTicket, set *PublicationSet) {
			set.Domains = map[Domain]PublicationCandidate{"unknown": candidate}
		},
		"duplicate ticket domains": func(ticket *ApplyTicket, _ *PublicationSet) {
			ticket.RequiredDomains = []Domain{DomainHTTP, DomainHTTP}
		},
		"unsorted ticket domains": func(ticket *ApplyTicket, _ *PublicationSet) {
			ticket.RequiredDomains = []Domain{DomainStream, DomainHTTP}
		},
		"missing candidate": func(_ *ApplyTicket, set *PublicationSet) {
			set.Domains = map[Domain]PublicationCandidate{}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ticket := ApplyTicket{DesiredRevision: 7, RequiredDomains: []Domain{DomainHTTP}}
			set := PublicationSet{DesiredRevision: 7, Domains: map[Domain]PublicationCandidate{
				DomainHTTP: candidate,
			}}
			mutate(&ticket, &set)
			if err := ValidatePublicationSet(ticket, set); err != ErrIntegrity {
				t.Fatalf("ValidatePublicationSet() error = %v, want %v", err, ErrIntegrity)
			}
		})
	}
}

func TestValidatePublicationSetAcceptsEachRequiredDomain(t *testing.T) {
	httpSnapshot := publicationValidationSnapshot(t, 7)
	streamSnapshot := publicationValidationStreamSnapshot(t, 7)
	ticket := ApplyTicket{
		DesiredRevision: 7,
		RequiredDomains: []Domain{DomainHTTP, DomainStream},
	}
	set := PublicationSet{
		DesiredRevision: 7,
		Domains: map[Domain]PublicationCandidate{
			DomainHTTP:   publicationValidationCandidate(httpSnapshot, DomainHTTP),
			DomainStream: publicationValidationCandidate(streamSnapshot, DomainStream),
		},
	}
	if err := ValidatePublicationSet(ticket, set); err != nil {
		t.Fatalf("ValidatePublicationSet() error = %v", err)
	}
}

func TestValidateRecoverySetRequiresExactDomainCoverage(t *testing.T) {
	http := PublishedGeneration(publicationValidationCandidate(publicationValidationSnapshot(t, 5), DomainHTTP))
	stream := PublishedGeneration(
		publicationValidationCandidate(publicationValidationStreamSnapshot(t, 7), DomainStream),
	)
	validRevisions := RevisionSet{Desired: 9, HTTP: 5, Stream: 7}
	validPublished := map[Domain]PublishedGeneration{DomainHTTP: http, DomainStream: stream}
	if err := ValidateRecoverySet(validRevisions, validPublished); err != nil {
		t.Fatalf("ValidateRecoverySet() error = %v", err)
	}
	if err := ValidateRecoverySet(RevisionSet{Desired: 9}, nil); err != nil {
		t.Fatalf("ValidateRecoverySet(empty recovery) error = %v", err)
	}

	tests := map[string]struct {
		revisions RevisionSet
		published map[Domain]PublishedGeneration
	}{
		"zero desired": {
			revisions: RevisionSet{HTTP: 5},
			published: map[Domain]PublishedGeneration{DomainHTTP: http},
		},
		"missing stream": {
			revisions: validRevisions,
			published: map[Domain]PublishedGeneration{DomainHTTP: http},
		},
		"extra stream": {
			revisions: RevisionSet{Desired: 9, HTTP: 5},
			published: validPublished,
		},
		"unknown domain": {
			revisions: RevisionSet{Desired: 9, HTTP: 5},
			published: map[Domain]PublishedGeneration{DomainHTTP: http, "unknown": stream},
		},
		"future http": {
			revisions: RevisionSet{Desired: 4, HTTP: 5},
			published: map[Domain]PublishedGeneration{DomainHTTP: http},
		},
		"revision mismatch": {
			revisions: RevisionSet{Desired: 9, HTTP: 6},
			published: map[Domain]PublishedGeneration{DomainHTTP: http},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateRecoverySet(test.revisions, test.published); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("ValidateRecoverySet() error = %v, want ErrIntegrity", err)
			}
		})
	}
}

func publicationValidationSnapshot(t *testing.T, revision uint64) Snapshot {
	t.Helper()
	snapshot, err := NewSnapshot(revision, []Resource{
		{Key: ResourceKey{Kind: "routes", ID: "r1"}, Value: []byte(`{"uri":"/"}`)},
	}, []Tombstone{
		{Key: ResourceKey{Kind: "routes", ID: "gone"}, Revision: revision - 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func publicationValidationStreamSnapshot(t *testing.T, revision uint64) Snapshot {
	t.Helper()
	snapshot, err := NewSnapshot(revision, []Resource{
		{Key: ResourceKey{Kind: "stream_routes", ID: "r1"}, Value: []byte(`{"server_addr":"127.0.0.1"}`)},
	}, []Tombstone{
		{Key: ResourceKey{Kind: "stream_routes", ID: "gone"}, Revision: revision - 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func publicationValidationCandidate(snapshot Snapshot, domain Domain) PublicationCandidate {
	routeKind := "routes"
	if domain == DomainStream {
		routeKind = "stream_routes"
	}
	return PublicationCandidate{
		Artifact: GenerationArtifact{
			Domain: domain, Revision: snapshot.Revision(), Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure: []ResourceKey{
			{Kind: routeKind, ID: "r1"},
			{Kind: routeKind, ID: "gone"},
			{Kind: "services", ID: "quarantined"},
		},
		Decisions: []ResourceDecision{
			{Key: ResourceKey{Kind: routeKind, ID: "r1"}, Disposition: DispositionPublished, Code: "published"},
			{Key: ResourceKey{Kind: routeKind, ID: "gone"}, Disposition: DispositionDeleted, Code: "deleted"},
			{
				Key:         ResourceKey{Kind: "services", ID: "quarantined"},
				Disposition: DispositionQuarantined,
				Code:        "quarantined",
			},
		},
	}
}
