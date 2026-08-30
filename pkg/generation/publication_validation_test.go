package generation

import "testing"

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
