package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	bolt "go.etcd.io/bbolt"
)

var policyTestKey = generation.ResourceKey{Kind: "routes", ID: "policy"}

func TestJournalPolicyLastGoodRequiresSameDomainPredecessor(t *testing.T) {
	t.Run("first generation", func(t *testing.T) {
		journal := openTestJournal(t)
		ticket := applyPolicyDesired(t, journal, "1", []byte("desired"), generation.DomainHTTP)
		candidate := policyCandidate(
			t,
			ticket,
			generation.DomainHTTP,
			[]byte("desired"),
			generation.DispositionLastGood,
		)

		_, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
		if !errors.Is(err, generation.ErrNoLastGood) {
			t.Fatalf("Stage() error = %v, want ErrNoLastGood", err)
		}
		assertNoPolicyToken(t, journal)
	})

	t.Run("same domain predecessor round trip", func(t *testing.T) {
		journal := openTestJournal(t)
		first := applyPolicyDesired(t, journal, "1", []byte("old"), generation.DomainHTTP)
		publishPolicyCandidate(t, journal, first, policyCandidate(
			t, first, generation.DomainHTTP, []byte("old"), generation.DispositionPublished,
		))
		second := applyPolicyDesired(t, journal, "2", []byte("new"), generation.DomainHTTP)
		candidate := policyCandidate(
			t,
			second,
			generation.DomainHTTP,
			[]byte("old"),
			generation.DispositionLastGood,
		)
		token, err := journal.Stage(context.Background(), second, policySet(second, candidate))
		if err != nil {
			t.Fatal(err)
		}
		ack, err := journal.Commit(context.Background(), token)
		if err != nil {
			t.Fatal(err)
		}
		if got := ack.Decisions[generation.DomainHTTP]; len(got) != 1 ||
			got[0] != candidate.Decisions[0] {
			t.Fatalf("ack decisions = %+v, want %+v", got, candidate.Decisions)
		}
		published, err := journal.LoadPublished(context.Background(), generation.DomainHTTP)
		if err != nil {
			t.Fatal(err)
		}
		value, found := published.Snapshot.Lookup(policyTestKey)
		if !found || !bytes.Equal(value, []byte("old")) ||
			published.Decisions[0] != candidate.Decisions[0] {
			t.Fatalf("published = %+v value=%q, want last-good round trip", published, value)
		}
	})

	t.Run("bytes mismatch", func(t *testing.T) {
		journal := openTestJournal(t)
		first := applyPolicyDesired(t, journal, "1", []byte("old"), generation.DomainHTTP)
		publishPolicyCandidate(t, journal, first, policyCandidate(
			t, first, generation.DomainHTTP, []byte("old"), generation.DispositionPublished,
		))
		second := applyPolicyDesired(t, journal, "2", []byte("new"), generation.DomainHTTP)
		candidate := policyCandidate(
			t,
			second,
			generation.DomainHTTP,
			[]byte("wrong"),
			generation.DispositionLastGood,
		)
		_, err := journal.Stage(context.Background(), second, policySet(second, candidate))
		if !errors.Is(err, generation.ErrNoLastGood) {
			t.Fatalf("Stage() error = %v, want ErrNoLastGood", err)
		}
		assertNoPolicyToken(t, journal)
	})
}

func TestJournalPolicyDispositionShapes(t *testing.T) {
	t.Run("published matches desired bytes", func(t *testing.T) {
		journal := openTestJournal(t)
		ticket := applyPolicyDesired(t, journal, "1", []byte("desired"), generation.DomainHTTP)
		candidate := policyCandidate(
			t,
			ticket,
			generation.DomainHTTP,
			[]byte("different"),
			generation.DispositionPublished,
		)
		_, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
		if !errors.Is(err, generation.ErrInvalidClosure) {
			t.Fatalf("Stage() error = %v, want ErrInvalidClosure", err)
		}
		assertNoPolicyToken(t, journal)
	})

	for _, disposition := range []generation.ResourceDisposition{
		generation.DispositionQuarantined,
		generation.DispositionFailClosed,
	} {
		t.Run(string(disposition)+" decision only", func(t *testing.T) {
			journal := openTestJournal(t)
			ticket := applyPolicyDesired(t, journal, "1", []byte("desired"), generation.DomainHTTP)
			candidate := policyDecisionOnlyCandidate(
				t,
				ticket,
				generation.DomainHTTP,
				disposition,
				"policy-blocked",
			)
			token, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
			if err != nil {
				t.Fatal(err)
			}
			ack, err := journal.Commit(context.Background(), token)
			if err != nil {
				t.Fatal(err)
			}
			published, err := journal.LoadPublished(context.Background(), generation.DomainHTTP)
			if err != nil {
				t.Fatal(err)
			}
			if _, found := published.Snapshot.Lookup(policyTestKey); found ||
				published.Snapshot.Deleted(policyTestKey) {
				t.Fatal("decision-only candidate persisted a value or tombstone")
			}
			if got := ack.Decisions[generation.DomainHTTP]; len(got) != 1 ||
				got[0] != published.Decisions[0] {
				t.Fatalf("ack decisions = %+v, published decisions = %+v", got, published.Decisions)
			}
		})

		t.Run(string(disposition)+" rejects candidate value", func(t *testing.T) {
			journal := openTestJournal(t)
			ticket := applyPolicyDesired(t, journal, "1", []byte("desired"), generation.DomainHTTP)
			candidate := policyCandidate(
				t,
				ticket,
				generation.DomainHTTP,
				[]byte("desired"),
				disposition,
			)
			_, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
			if !errors.Is(err, generation.ErrInvalidClosure) {
				t.Fatalf("Stage() error = %v, want ErrInvalidClosure", err)
			}
			assertNoPolicyToken(t, journal)
		})
	}

	t.Run("deleted requires exact desired tombstone", func(t *testing.T) {
		journal := openTestJournal(t)
		_ = applyPolicyDesired(t, journal, "1", []byte("desired"), generation.DomainHTTP)
		ticket := deletePolicyDesired(t, journal, "2", generation.DomainHTTP)
		valid := policyDeletedCandidate(t, ticket, ticket.DesiredRevision)
		token, err := journal.Stage(context.Background(), ticket, policySet(ticket, valid))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := journal.Commit(context.Background(), token); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("deleted rejects wrong tombstone revision", func(t *testing.T) {
		journal := openTestJournal(t)
		_ = applyPolicyDesired(t, journal, "1", []byte("desired"), generation.DomainHTTP)
		ticket := deletePolicyDesired(t, journal, "2", generation.DomainHTTP)
		candidate := policyDeletedCandidate(t, ticket, ticket.DesiredRevision-1)
		_, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
		if !errors.Is(err, generation.ErrInvalidClosure) {
			t.Fatalf("Stage() error = %v, want ErrInvalidClosure", err)
		}
		assertNoPolicyToken(t, journal)
	})

	t.Run("explicit delete cannot use last-good", func(t *testing.T) {
		journal := openTestJournal(t)
		first := applyPolicyDesired(t, journal, "1", []byte("old"), generation.DomainHTTP)
		publishPolicyCandidate(t, journal, first, policyCandidate(
			t, first, generation.DomainHTTP, []byte("old"), generation.DispositionPublished,
		))
		ticket := deletePolicyDesired(t, journal, "2", generation.DomainHTTP)
		candidate := policyCandidate(
			t,
			ticket,
			generation.DomainHTTP,
			[]byte("old"),
			generation.DispositionLastGood,
		)
		_, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
		if !errors.Is(err, generation.ErrNoLastGood) {
			t.Fatalf("Stage() error = %v, want ErrNoLastGood", err)
		}
		assertNoPolicyToken(t, journal)
	})
}

func TestJournalPolicyDistinguishesNilAndEmptyValues(t *testing.T) {
	values := []struct {
		name      string
		persisted []byte
		candidate []byte
	}{
		{name: "nil versus empty", persisted: nil, candidate: make([]byte, 0)},
		{name: "empty versus nil", persisted: make([]byte, 0), candidate: nil},
	}
	for _, test := range values {
		t.Run("published "+test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			ticket := applyPolicyDesired(t, journal, "1", test.persisted, generation.DomainHTTP)
			candidate := policyCandidate(
				t, ticket, generation.DomainHTTP, test.candidate, generation.DispositionPublished,
			)
			_, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
			if !errors.Is(err, generation.ErrInvalidClosure) {
				t.Fatalf("Stage() error = %v, want ErrInvalidClosure", err)
			}
			assertNoPolicyToken(t, journal)
		})

		t.Run("last-good "+test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			first := applyPolicyDesired(t, journal, "1", test.persisted, generation.DomainHTTP)
			publishPolicyCandidate(t, journal, first, policyCandidate(
				t, first, generation.DomainHTTP, test.persisted, generation.DispositionPublished,
			))
			second := applyPolicyDesired(t, journal, "2", []byte("new"), generation.DomainHTTP)
			candidate := policyCandidate(
				t, second, generation.DomainHTTP, test.candidate, generation.DispositionLastGood,
			)
			_, err := journal.Stage(context.Background(), second, policySet(second, candidate))
			if !errors.Is(err, generation.ErrNoLastGood) {
				t.Fatalf("Stage() error = %v, want ErrNoLastGood", err)
			}
			assertNoPolicyToken(t, journal)
		})
	}
}

func TestJournalPolicyCommitRevalidatesPersistedStage(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyPolicyDesired(t, journal, "1", []byte("desired"), generation.DomainHTTP)
	candidate := policyCandidate(
		t, ticket, generation.DomainHTTP, []byte("wrong"), generation.DispositionPublished,
	)
	token := generation.PublicationToken(strings.Repeat("cd", 16))
	encoded, err := encodeStagedPublication(stagedPublication{
		Token: token, Ticket: ticket, Set: policySet(ticket, candidate),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(publicationTxnBucket).Put([]byte(token), encoded)
	}); err != nil {
		t.Fatal(err)
	}
	beforeArtifacts := bucketKeyCount(t, journal.db, artifactBucket)

	if _, err := journal.Commit(context.Background(), token); !errors.Is(err, generation.ErrInvalidClosure) {
		t.Fatalf("Commit() error = %v, want ErrInvalidClosure", err)
	}
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 1 {
		t.Fatalf("staged transaction count = %d, want rejected token retained", got)
	}
	if got := bucketKeyCount(t, journal.db, artifactBucket); got != beforeArtifacts {
		t.Fatalf("artifact count = %d, want %d", got, beforeArtifacts)
	}
	if got := bucketKeyCount(t, journal.db, publishedHeadBucket); got != 0 {
		t.Fatalf("published head count = %d, want 0", got)
	}
	if got := bucketKeyCount(t, journal.db, publicationDecisionBucket); got != 0 {
		t.Fatalf("decision count = %d, want 0", got)
	}
}

func TestJournalPolicySchemaV1LegacyCodeRemainsReadable(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyPolicyDesired(t, journal, "1", []byte("desired"), generation.DomainHTTP)
	candidate := policyCandidate(
		t, ticket, generation.DomainHTTP, []byte("desired"), generation.DispositionPublished,
	)
	candidate.Decisions[0].Code = "Legacy Code"
	token := generation.PublicationToken(strings.Repeat("ef", 16))
	encoded, err := encodeStagedPublication(stagedPublication{
		Token: token, Ticket: ticket, Set: policySet(ticket, candidate),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(publicationTxnBucket).Put([]byte(token), encoded)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatalf("Commit(schema-v1 token) error = %v", err)
	}
	published, err := journal.LoadPublished(context.Background(), generation.DomainHTTP)
	if err != nil {
		t.Fatalf("LoadPublished(schema-v1 code) error = %v", err)
	}
	if got := published.Decisions[0].Code; got != "Legacy Code" {
		t.Fatalf("published decision code = %q, want legacy code", got)
	}
}

func TestJournalPolicyRejectsUnknownMixedAndCrossDomainStateAtomically(t *testing.T) {
	t.Run("unknown desired key", func(t *testing.T) {
		journal := openTestJournal(t)
		ticket := applyPolicyDesired(t, journal, "1", []byte("desired"), generation.DomainHTTP)
		candidate := policyCandidate(
			t,
			ticket,
			generation.DomainHTTP,
			[]byte("desired"),
			generation.DispositionPublished,
		)
		candidate.Closure[0].ID = "unknown"
		candidate.Decisions[0].Key = candidate.Closure[0]
		resources := candidate.Snapshot.Resources()
		resources[0].Key = candidate.Closure[0]
		candidate.Snapshot = mustSnapshot(t, ticket.DesiredRevision, resources, nil)
		candidate.Artifact = policyArtifact(generation.DomainHTTP, candidate.Snapshot)
		_, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
		if !errors.Is(err, generation.ErrInvalidClosure) {
			t.Fatalf("Stage() error = %v, want ErrInvalidClosure", err)
		}
		assertNoPolicyToken(t, journal)
	})

	t.Run("mixed closure", func(t *testing.T) {
		journal := openTestJournal(t)
		ticket := applyTwoKeyPolicyDesired(t, journal, "1", generation.DomainHTTP)
		candidate := twoKeyPolicyCandidate(t, ticket, generation.DomainHTTP)
		candidate.Decisions[1].Disposition = generation.DispositionQuarantined
		_, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
		if !errors.Is(err, generation.ErrInvalidClosure) {
			t.Fatalf("Stage() error = %v, want ErrInvalidClosure", err)
		}
		assertNoPolicyToken(t, journal)
	})

	t.Run("two domains one invalid", func(t *testing.T) {
		journal := openTestJournal(t)
		ticket := applyPolicyDesired(
			t,
			journal,
			"1",
			[]byte("desired"),
			generation.DomainHTTP,
			generation.DomainStream,
		)
		http := policyCandidate(
			t,
			ticket,
			generation.DomainHTTP,
			[]byte("desired"),
			generation.DispositionPublished,
		)
		stream := policyCandidate(
			t,
			ticket,
			generation.DomainStream,
			[]byte("wrong"),
			generation.DispositionPublished,
		)
		set := generation.PublicationSet{
			DesiredRevision: ticket.DesiredRevision,
			Domains: map[generation.Domain]generation.PublicationCandidate{
				generation.DomainHTTP: http, generation.DomainStream: stream,
			},
		}
		_, err := journal.Stage(context.Background(), ticket, set)
		if !errors.Is(err, generation.ErrInvalidClosure) {
			t.Fatalf("Stage() error = %v, want ErrInvalidClosure", err)
		}
		assertNoPolicyToken(t, journal)
	})

	t.Run("cross-domain predecessor cannot be borrowed", func(t *testing.T) {
		journal := openTestJournal(t)
		first := applyPolicyDesired(t, journal, "1", []byte("old"), generation.DomainHTTP)
		publishPolicyCandidate(t, journal, first, policyCandidate(
			t, first, generation.DomainHTTP, []byte("old"), generation.DispositionPublished,
		))
		second := applyPolicyDesired(t, journal, "2", []byte("new"), generation.DomainStream)
		candidate := policyCandidate(
			t,
			second,
			generation.DomainStream,
			[]byte("old"),
			generation.DispositionLastGood,
		)
		_, err := journal.Stage(context.Background(), second, policySet(second, candidate))
		if !errors.Is(err, generation.ErrNoLastGood) {
			t.Fatalf("Stage() error = %v, want ErrNoLastGood", err)
		}
		assertNoPolicyToken(t, journal)
	})

	t.Run("corrupt predecessor fails closed", func(t *testing.T) {
		journal := openTestJournal(t)
		first := applyPolicyDesired(t, journal, "1", []byte("old"), generation.DomainHTTP)
		publishPolicyCandidate(t, journal, first, policyCandidate(
			t, first, generation.DomainHTTP, []byte("old"), generation.DispositionPublished,
		))
		if err := journal.db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(publishedHeadBucket)
			encoded := bytes.Clone(bucket.Get([]byte(generation.DomainHTTP)))
			encoded[len(encoded)-1] ^= 1
			return bucket.Put([]byte(generation.DomainHTTP), encoded)
		}); err != nil {
			t.Fatal(err)
		}
		second := applyPolicyDesired(t, journal, "2", []byte("new"), generation.DomainHTTP)
		candidate := policyCandidate(
			t,
			second,
			generation.DomainHTTP,
			[]byte("new"),
			generation.DispositionPublished,
		)
		_, err := journal.Stage(context.Background(), second, policySet(second, candidate))
		if !errors.Is(err, generation.ErrIntegrity) {
			t.Fatalf("Stage() error = %v, want ErrIntegrity", err)
		}
		assertNoPolicyToken(t, journal)
	})
}

func TestJournalPolicyDecisionCodeValidation(t *testing.T) {
	tests := []struct {
		name string
		code string
		ok   bool
	}{
		{name: "empty"},
		{name: "blank", code: " "},
		{name: "control", code: "bad\ncode"},
		{name: "uppercase", code: "Bad"},
		{name: "too long", code: strings.Repeat("a", 129)},
		{name: "json", code: `{"secret":"value"}`},
		{name: "leading punctuation", code: "-bad"},
		{name: "slug", code: "policy.test-1_ok", ok: true},
		{name: "existing published", code: "test-published", ok: true},
		{name: "existing deleted", code: "deleted", ok: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			ticket := applyPolicyDesired(t, journal, "1", []byte("desired"), generation.DomainHTTP)
			candidate := policyCandidate(
				t,
				ticket,
				generation.DomainHTTP,
				[]byte("desired"),
				generation.DispositionPublished,
			)
			candidate.Decisions[0].Code = test.code
			_, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
			if test.ok && err != nil {
				t.Fatalf("Stage() error = %v", err)
			}
			if !test.ok && !errors.Is(err, generation.ErrInvalidClosure) {
				t.Fatalf("Stage() error = %v, want ErrInvalidClosure", err)
			}
		})
	}
}

func applyPolicyDesired(
	t *testing.T,
	journal *Store,
	revision string,
	value []byte,
	domains ...generation.Domain,
) generation.ApplyTicket {
	t.Helper()
	ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: revision},
		Mutations: []generation.Mutation{
			{Type: generation.MutationPut, Key: policyTestKey, Value: value},
		},
		RequiredDomains: domains,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func applyTwoKeyPolicyDesired(
	t *testing.T,
	journal *Store,
	revision string,
	domains ...generation.Domain,
) generation.ApplyTicket {
	t.Helper()
	ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: revision},
		Mutations: []generation.Mutation{
			{Type: generation.MutationPut, Key: policyTestKey, Value: []byte("one")},
			{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: "routes", ID: "policy-2"},
				Value: []byte("two"),
			},
		},
		RequiredDomains: domains,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func deletePolicyDesired(
	t *testing.T,
	journal *Store,
	revision string,
	domains ...generation.Domain,
) generation.ApplyTicket {
	t.Helper()
	ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: revision},
		Mutations: []generation.Mutation{
			{Type: generation.MutationDelete, Key: policyTestKey},
		},
		RequiredDomains: domains,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func policySet(
	ticket generation.ApplyTicket,
	candidate generation.PublicationCandidate,
) generation.PublicationSet {
	return generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			candidate.Artifact.Domain: candidate,
		},
	}
}

func policyCandidate(
	t *testing.T,
	ticket generation.ApplyTicket,
	domain generation.Domain,
	value []byte,
	disposition generation.ResourceDisposition,
) generation.PublicationCandidate {
	t.Helper()
	snapshot := mustSnapshot(
		t,
		ticket.DesiredRevision,
		[]generation.Resource{{Key: policyTestKey, Value: value}},
		nil,
	)
	return generation.PublicationCandidate{
		Artifact: policyArtifact(domain, snapshot), Snapshot: snapshot,
		Closure: []generation.ResourceKey{policyTestKey},
		Decisions: []generation.ResourceDecision{
			{Key: policyTestKey, Disposition: disposition, Code: "policy-valid"},
		},
	}
}

func twoKeyPolicyCandidate(
	t *testing.T,
	ticket generation.ApplyTicket,
	domain generation.Domain,
) generation.PublicationCandidate {
	t.Helper()
	second := generation.ResourceKey{Kind: "routes", ID: "policy-2"}
	snapshot := mustSnapshot(t, ticket.DesiredRevision, []generation.Resource{
		{Key: policyTestKey, Value: []byte("one")}, {Key: second, Value: []byte("two")},
	}, nil)
	return generation.PublicationCandidate{
		Artifact: policyArtifact(domain, snapshot), Snapshot: snapshot,
		Closure: []generation.ResourceKey{policyTestKey, second},
		Decisions: []generation.ResourceDecision{
			{
				Key:         policyTestKey,
				Disposition: generation.DispositionPublished,
				Code:        "policy-valid",
			},
			{Key: second, Disposition: generation.DispositionPublished, Code: "policy-valid"},
		},
	}
}

func policyDecisionOnlyCandidate(
	t *testing.T,
	ticket generation.ApplyTicket,
	domain generation.Domain,
	disposition generation.ResourceDisposition,
	code string,
) generation.PublicationCandidate {
	t.Helper()
	snapshot := mustSnapshot(t, ticket.DesiredRevision, nil, nil)
	return generation.PublicationCandidate{
		Artifact: policyArtifact(domain, snapshot), Snapshot: snapshot,
		Closure: []generation.ResourceKey{policyTestKey},
		Decisions: []generation.ResourceDecision{
			{Key: policyTestKey, Disposition: disposition, Code: code},
		},
	}
}

func policyDeletedCandidate(
	t *testing.T,
	ticket generation.ApplyTicket,
	tombstoneRevision uint64,
) generation.PublicationCandidate {
	t.Helper()
	snapshot := mustSnapshot(t, ticket.DesiredRevision, nil, []generation.Tombstone{{
		Key: policyTestKey, Revision: tombstoneRevision,
	}})
	return generation.PublicationCandidate{
		Artifact: policyArtifact(generation.DomainHTTP, snapshot), Snapshot: snapshot,
		Closure: []generation.ResourceKey{policyTestKey},
		Decisions: []generation.ResourceDecision{{
			Key: policyTestKey, Disposition: generation.DispositionDeleted, Code: "deleted",
		}},
	}
}

func policyArtifact(
	domain generation.Domain,
	snapshot generation.Snapshot,
) generation.GenerationArtifact {
	return generation.GenerationArtifact{
		Domain: domain, Revision: snapshot.Revision(), Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
	}
}

func publishPolicyCandidate(
	t *testing.T,
	journal *Store,
	ticket generation.ApplyTicket,
	candidate generation.PublicationCandidate,
) {
	t.Helper()
	token, err := journal.Stage(context.Background(), ticket, policySet(ticket, candidate))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
}

func assertNoPolicyToken(t *testing.T, journal *Store) {
	t.Helper()
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 0 {
		t.Fatalf("staged transaction count = %d, want 0", got)
	}
}
