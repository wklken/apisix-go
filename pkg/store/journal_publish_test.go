package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	bolt "go.etcd.io/bbolt"
)

func TestJournalCommitAdvancesOnlyRequiredPublishedDomains(t *testing.T) {
	journal := openTestJournal(t)
	httpTicket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	httpSet := publicationSet(t, httpTicket, generation.DomainHTTP)
	httpToken, err := journal.Stage(context.Background(), httpTicket, httpSet)
	if err != nil {
		t.Fatal(err)
	}
	if got := bucketKeyCount(t, journal.db, artifactBucket); got != 1 {
		t.Fatalf("artifact count after Stage = %d, want desired artifact only", got)
	}
	assertRevisions(t, journal, generation.RevisionSet{Desired: 1})
	ack, err := journal.Commit(context.Background(), httpToken)
	if err != nil {
		t.Fatal(err)
	}
	want := generation.RevisionSet{Desired: 1, HTTP: 1}
	if ack.Revisions != want {
		t.Fatalf("ack revisions = %+v, want %+v", ack.Revisions, want)
	}
	assertRevisions(t, journal, want)

	streamTicket := applyDesiredForPublication(t, journal, "2", generation.DomainStream)
	streamToken, err := journal.Stage(
		context.Background(),
		streamTicket,
		publicationSet(t, streamTicket, generation.DomainStream),
	)
	if err != nil {
		t.Fatal(err)
	}
	ack, err = journal.Commit(context.Background(), streamToken)
	if err != nil {
		t.Fatal(err)
	}
	want = generation.RevisionSet{Desired: 2, HTTP: 1, Stream: 2}
	if ack.Revisions != want {
		t.Fatalf("ack revisions = %+v, want %+v", ack.Revisions, want)
	}
	assertRevisions(t, journal, want)
}

func TestJournalCommitPublishesBothDomainsAtomically(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(
		t,
		journal,
		"both",
		generation.DomainHTTP,
		generation.DomainStream,
	)
	set := publicationSet(t, ticket, generation.DomainHTTP, generation.DomainStream)
	token, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := journal.Commit(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	want := generation.RevisionSet{Desired: 1, HTTP: 1, Stream: 1}
	if ack.Revisions != want || ack.Cursor != ticket.Cursor || len(ack.Decisions) != 2 {
		t.Fatalf("ack = %+v, want revisions %+v and both decision sets", ack, want)
	}
	for _, domain := range ticket.RequiredDomains {
		published, loadErr := journal.LoadPublished(context.Background(), domain)
		if loadErr != nil {
			t.Fatalf("LoadPublished(%q): %v", domain, loadErr)
		}
		if published.Artifact != set.Domains[domain].Artifact {
			t.Fatalf(
				"published artifact = %+v, want %+v",
				published.Artifact,
				set.Domains[domain].Artifact,
			)
		}
	}
}

func TestJournalCommitEmptyRequiredDomainsOnlyAcknowledgesDesired(t *testing.T) {
	journal := openTestJournal(t)
	ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "empty"},
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := journal.Stage(context.Background(), ticket, generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains:         map[generation.Domain]generation.PublicationCandidate{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := journal.Commit(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	want := generation.RevisionSet{Desired: 1}
	if ack.Revisions != want || ack.Cursor != ticket.Cursor || len(ack.Decisions) != 0 {
		t.Fatalf("ack = %+v, want desired-only %+v", ack, want)
	}
	assertRevisions(t, journal, want)
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 0 {
		t.Fatalf("staged transaction count = %d, want 0", got)
	}
	if got := bucketKeyCount(t, journal.db, publishedHeadBucket); got != 0 {
		t.Fatalf("published head count = %d, want 0", got)
	}
}

func TestJournalAcknowledgementPersistsLoadsAndRejectsFurtherPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	set := publicationSet(t, ticket, generation.DomainHTTP)
	first, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	loser, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.LoadAcknowledgement(context.Background(), ticket.Cursor); !errors.Is(
		err,
		generation.ErrNotFound,
	) {
		t.Fatalf("LoadAcknowledgement(before commit) error = %v, want ErrNotFound", err)
	}
	committed, err := journal.Commit(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.LoadAcknowledgement(context.Background(), ticket.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	assertAcknowledgementsEqual(t, loaded, committed)
	loaded.Decisions[generation.DomainHTTP][0].Code = "mutated"
	again, err := journal.LoadAcknowledgement(context.Background(), ticket.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	assertAcknowledgementsEqual(t, again, committed)
	canceled := newStepCancelContext(3)
	if _, err := journal.LoadAcknowledgement(canceled, ticket.Cursor); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadAcknowledgement(final cancellation) error = %v, want context canceled", err)
	}
	assertStepCancellation(t, canceled, 3)
	if _, err := journal.Stage(context.Background(), ticket, set); !errors.Is(
		err,
		generation.ErrStaleCursor,
	) {
		t.Fatalf("Stage(after commit) error = %v, want ErrStaleCursor", err)
	}
	if _, err := journal.Commit(context.Background(), loser); !errors.Is(
		err,
		generation.ErrStaleCursor,
	) {
		t.Fatalf("Commit(loser) error = %v, want ErrStaleCursor", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted, err := reopened.LoadAcknowledgement(context.Background(), ticket.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	assertAcknowledgementsEqual(t, restarted, committed)
}

func TestJournalBackfillsMarkerlessCommittedAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ticket := applyDesiredForPublication(t, journal, "legacy-committed", generation.DomainHTTP)
	token, err := journal.Stage(
		context.Background(), ticket, publicationSet(t, ticket, generation.DomainHTTP),
	)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := journal.Commit(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	clearCommittedAcknowledgement(t, journal, ticket.Cursor)
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(journalMetaBucket).Put(schemaVersionKey, encodeUint64(1))
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.LoadAcknowledgement(context.Background(), ticket.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	assertAcknowledgementsEqual(t, loaded, committed)
	assertCursorHasCommittedAcknowledgement(t, reopened, ticket.Cursor)
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	loaded, err = restarted.LoadAcknowledgement(context.Background(), ticket.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	assertAcknowledgementsEqual(t, loaded, committed)
}

func TestJournalKeepsMarkerlessUncommittedCursorRetryable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	batch := desiredBatch("etcd", "legacy-uncommitted", generation.DomainHTTP)
	ticket, err := journal.ApplyDesired(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.LoadAcknowledgement(context.Background(), ticket.Cursor); !errors.Is(
		err,
		generation.ErrNotFound,
	) {
		t.Fatalf("LoadAcknowledgement(markerless uncommitted) error = %v, want ErrNotFound", err)
	}
	replayed, err := reopened.ApplyDesired(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.DesiredRevision != ticket.DesiredRevision || replayed.DesiredDigest != ticket.DesiredDigest {
		t.Fatalf("replayed ticket = %+v, want %+v", replayed, ticket)
	}
}

func TestCoordinatorCompletesMarkerlessZeroDomainCursorWithSyntheticRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	batch := desiredBatch("etcd/v1/test", "legacy-zero", "")
	ticket, err := journal.ApplyDesired(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	token, err := journal.Stage(context.Background(), ticket, publicationSet(t, ticket))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	clearCommittedAcknowledgement(t, journal, ticket.Cursor)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	engine := &retryPublicationEngine{t: t}
	ack, err := generation.NewCoordinator(reopened, engine).Apply(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Revisions != (generation.RevisionSet{Desired: ticket.DesiredRevision}) ||
		len(ack.Decisions) != 0 || engine.prepareCalls != 1 || engine.activateCalls != 1 ||
		engine.finalizeCalls != 1 || len(engine.ticket.RequiredDomains) != 0 {
		t.Fatalf("ack/prepare/activate/finalize/ticket = %+v/%d/%d/%d/%+v",
			ack, engine.prepareCalls, engine.activateCalls, engine.finalizeCalls, engine.ticket)
	}
	assertCursorHasCommittedAcknowledgement(t, reopened, ticket.Cursor)
}

func TestCoordinatorReopensCommittedCursorBeforeDifferentlyShapedReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delta := generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "restart"},
		Mutations: []generation.Mutation{
			{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: "routes", ID: "r1"},
				Value: []byte(`{"id":"r1"}`),
			},
			{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: "upstreams", ID: "u1"},
				Value: []byte(`{"id":"u1"}`),
			},
		},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	ticket, err := journal.ApplyDesired(context.Background(), delta)
	if err != nil {
		t.Fatal(err)
	}
	token, err := journal.Stage(
		context.Background(), ticket, publicationSet(t, ticket, generation.DomainHTTP),
	)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := journal.Commit(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	engine := &committedReplayEngine{}
	fullSnapshot := generation.DesiredBatch{
		Cursor:         delta.Cursor,
		ReplaceManaged: true,
		Mutations:      slices.Clone(delta.Mutations),
		RequiredDomains: []generation.Domain{
			generation.DomainHTTP,
			generation.DomainStream,
		},
	}
	loaded, err := generation.NewCoordinator(reopened, engine).Apply(
		context.Background(), fullSnapshot,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertAcknowledgementsEqual(t, loaded, committed)
	if engine.confirmCalls != 1 || engine.prepareCalls != 0 || engine.activateCalls != 0 ||
		engine.finalizeCalls != 0 {
		t.Fatalf(
			"engine confirm/prepare/activate/finalize calls = %d/%d/%d/%d, want 1/0/0/0",
			engine.confirmCalls,
			engine.prepareCalls,
			engine.activateCalls,
			engine.finalizeCalls,
		)
	}
	confirmed, found := engine.confirmed.Domains[generation.DomainHTTP]
	if engine.confirmed.DesiredRevision != ticket.DesiredRevision || !found ||
		confirmed.Artifact.Revision != ticket.DesiredRevision ||
		len(engine.confirmed.Domains) != 1 {
		t.Fatalf("confirmed publication set = %+v", engine.confirmed)
	}
	published, err := reopened.LoadPublished(context.Background(), generation.DomainHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Artifact != published.Artifact ||
		confirmed.Snapshot.SnapshotID() != published.Snapshot.SnapshotID() ||
		!slices.Equal(confirmed.Closure, published.Closure) ||
		!slices.Equal(confirmed.Decisions, published.Decisions) {
		t.Fatalf("confirmed HTTP candidate = %+v, want committed %+v", confirmed, published)
	}
	revisions, err := reopened.Revisions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if revisions != (generation.RevisionSet{Desired: 1, HTTP: 1}) {
		t.Fatalf("revisions = %+v, want desired/http revision 1", revisions)
	}
}

func TestCoordinatorRetriesMarkerlessUncommittedCursorFromEquivalentFullSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delta := generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd/v1/test", Revision: "91"},
		Mutations: []generation.Mutation{
			{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: "routes", ID: "r1"},
				Value: []byte(`{"id":"r1"}`),
			},
			{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: "upstreams", ID: "u1"},
				Value: []byte(`{"id":"u1"}`),
			},
		},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	ticket, err := journal.ApplyDesired(context.Background(), delta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Stage(
		context.Background(), ticket, publicationSet(t, ticket, generation.DomainHTTP),
	); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	engine := &retryPublicationEngine{t: t}
	fullSnapshot := generation.DesiredBatch{
		Cursor: delta.Cursor, ReplaceManaged: true,
		Mutations:       slices.Clone(delta.Mutations),
		RequiredDomains: []generation.Domain{generation.DomainHTTP, generation.DomainStream},
	}
	ack, err := generation.NewCoordinator(reopened, engine).Apply(context.Background(), fullSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Cursor != delta.Cursor || ack.Revisions != (generation.RevisionSet{Desired: 1, HTTP: 1}) ||
		engine.prepareCalls != 1 || engine.activateCalls != 1 || engine.finalizeCalls != 1 {
		t.Fatalf("ack/prepare/activate/finalize = %+v/%d/%d/%d",
			ack, engine.prepareCalls, engine.activateCalls, engine.finalizeCalls)
	}
	if !slices.Equal(engine.ticket.RequiredDomains, []generation.Domain{generation.DomainHTTP}) {
		t.Fatalf("retried ticket domains = %v, want original http domain", engine.ticket.RequiredDomains)
	}
	assertRevisions(t, reopened, generation.RevisionSet{Desired: 1, HTTP: 1})
	assertCursorHasCommittedAcknowledgement(t, reopened, delta.Cursor)
}

type committedReplayEngine struct {
	prepareCalls  int
	activateCalls int
	finalizeCalls int
	confirmCalls  int
	confirmed     generation.PublicationSet
}

type retryPublicationEngine struct {
	t             *testing.T
	ticket        generation.ApplyTicket
	prepareCalls  int
	activateCalls int
	finalizeCalls int
}

func (e *retryPublicationEngine) Prepare(
	_ context.Context,
	ticket generation.ApplyTicket,
	_ generation.Snapshot,
	_ map[generation.Domain]generation.PublishedGeneration,
) (generation.PublicationSet, error) {
	e.prepareCalls++
	e.ticket = ticket
	return publicationSet(e.t, ticket, ticket.RequiredDomains...), nil
}

func (*retryPublicationEngine) DiscardPrepared(
	context.Context,
	generation.PublicationSet,
) error {
	return nil
}

func (e *retryPublicationEngine) Activate(
	_ context.Context,
	_ generation.PublicationToken,
	_ generation.PublicationSet,
) error {
	e.activateCalls++
	return nil
}

func (*retryPublicationEngine) RollbackActivation(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) error {
	return nil
}

func (e *retryPublicationEngine) FinalizeActivation(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) {
	e.finalizeCalls++
}

func (*retryPublicationEngine) ConfirmActive(
	context.Context,
	generation.PublicationSet,
) error {
	return errors.New("unexpected confirm active")
}

func (e *committedReplayEngine) Prepare(
	context.Context,
	generation.ApplyTicket,
	generation.Snapshot,
	map[generation.Domain]generation.PublishedGeneration,
) (generation.PublicationSet, error) {
	e.prepareCalls++
	return generation.PublicationSet{}, errors.New("unexpected prepare")
}

func (*committedReplayEngine) DiscardPrepared(
	context.Context,
	generation.PublicationSet,
) error {
	return errors.New("unexpected discard")
}

func (e *committedReplayEngine) Activate(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) error {
	e.activateCalls++
	return errors.New("unexpected activate")
}

func (*committedReplayEngine) RollbackActivation(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) error {
	return errors.New("unexpected rollback")
}

func (e *committedReplayEngine) FinalizeActivation(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) {
	e.finalizeCalls++
}

func (e *committedReplayEngine) ConfirmActive(
	_ context.Context,
	set generation.PublicationSet,
) error {
	e.confirmCalls++
	e.confirmed = set
	return nil
}

func TestJournalAcknowledgementZeroDomainAndFailedCommit(t *testing.T) {
	t.Run("zero domain", func(t *testing.T) {
		journal := openTestJournal(t)
		ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
			Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "empty"},
		})
		if err != nil {
			t.Fatal(err)
		}
		set := generation.PublicationSet{
			DesiredRevision: ticket.DesiredRevision,
			Domains:         map[generation.Domain]generation.PublicationCandidate{},
		}
		first, err := journal.Stage(context.Background(), ticket, set)
		if err != nil {
			t.Fatal(err)
		}
		loser, err := journal.Stage(context.Background(), ticket, set)
		if err != nil {
			t.Fatal(err)
		}
		committed, err := journal.Commit(context.Background(), first)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := journal.LoadAcknowledgement(context.Background(), ticket.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		assertAcknowledgementsEqual(t, loaded, committed)
		if _, err := journal.Stage(context.Background(), ticket, set); !errors.Is(
			err,
			generation.ErrStaleCursor,
		) {
			t.Fatalf("Stage(zero domain after commit) error = %v, want ErrStaleCursor", err)
		}
		if _, err := journal.Commit(context.Background(), loser); !errors.Is(
			err,
			generation.ErrStaleCursor,
		) {
			t.Fatalf("Commit(zero domain loser) error = %v, want ErrStaleCursor", err)
		}
	})

	t.Run("failed commit", func(t *testing.T) {
		journal := openTestJournal(t)
		ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
		token, err := journal.Stage(
			context.Background(), ticket, publicationSet(t, ticket, generation.DomainHTTP),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.db.Update(func(tx *bolt.Tx) error {
			return tx.DeleteBucket(publicationDecisionBucket)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := journal.Commit(context.Background(), token); !errors.Is(
			err,
			generation.ErrIntegrity,
		) {
			t.Fatalf("Commit() error = %v, want ErrIntegrity", err)
		}
		if err := journal.db.Update(func(tx *bolt.Tx) error {
			_, err := tx.CreateBucket(publicationDecisionBucket)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := journal.LoadAcknowledgement(context.Background(), ticket.Cursor); !errors.Is(
			err,
			generation.ErrNotFound,
		) {
			t.Fatalf("LoadAcknowledgement(after failed commit) error = %v, want ErrNotFound", err)
		}
	})
}

func TestJournalLoadAcknowledgementRejectsTampering(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*testing.T, *Store, generation.ApplyTicket, generation.Acknowledgement)
	}{
		{
			name: "marker cursor",
			tamper: func(t *testing.T, journal *Store, ticket generation.ApplyTicket, _ generation.Acknowledgement) {
				mutateCommittedAcknowledgement(
					t,
					journal,
					ticket,
					func(ack *generation.Acknowledgement) {
						ack.Cursor.Revision = "tampered"
					},
				)
			},
		},
		{
			name: "marker revisions",
			tamper: func(t *testing.T, journal *Store, ticket generation.ApplyTicket, _ generation.Acknowledgement) {
				mutateCommittedAcknowledgement(
					t,
					journal,
					ticket,
					func(ack *generation.Acknowledgement) {
						ack.Revisions.HTTP = 0
					},
				)
			},
		},
		{
			name: "marker domain set",
			tamper: func(t *testing.T, journal *Store, ticket generation.ApplyTicket, _ generation.Acknowledgement) {
				mutateCommittedAcknowledgement(
					t,
					journal,
					ticket,
					func(ack *generation.Acknowledgement) {
						delete(ack.Decisions, generation.DomainHTTP)
					},
				)
			},
		},
		{
			name: "marker decisions",
			tamper: func(t *testing.T, journal *Store, ticket generation.ApplyTicket, _ generation.Acknowledgement) {
				mutateCommittedAcknowledgement(
					t,
					journal,
					ticket,
					func(ack *generation.Acknowledgement) {
						ack.Decisions[generation.DomainHTTP][0].Code = "tampered"
					},
				)
			},
		},
		{
			name: "active keyed identity",
			tamper: func(t *testing.T, journal *Store, ticket generation.ApplyTicket, _ generation.Acknowledgement) {
				if err := journal.db.Update(func(tx *bolt.Tx) error {
					bucket := tx.Bucket(providerCursorBucket)
					key := cursorRecordKey(ticket.Cursor)
					record, err := decodeCursorRecord(bucket.Get(key), &ticket.Cursor)
					if err != nil {
						return err
					}
					record.Committed.Decisions[generation.DomainHTTP][0].Code = "tampered"
					encoded, err := encodeCursorRecord(record)
					if err != nil {
						return err
					}
					return bucket.Put(key, encoded)
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "published head",
			tamper: func(t *testing.T, journal *Store, _ generation.ApplyTicket, _ generation.Acknowledgement) {
				corruptBucketValue(t, journal, publishedHeadBucket, []byte(generation.DomainHTTP))
			},
		},
		{
			name: "published decisions",
			tamper: func(t *testing.T, journal *Store, _ generation.ApplyTicket, ack generation.Acknowledgement) {
				corruptBucketValue(
					t,
					journal,
					publicationDecisionBucket,
					publicationDecisionKey(generation.DomainHTTP, ack.Revisions.HTTP),
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
			token, err := journal.Stage(
				context.Background(), ticket, publicationSet(t, ticket, generation.DomainHTTP),
			)
			if err != nil {
				t.Fatal(err)
			}
			ack, err := journal.Commit(context.Background(), token)
			if err != nil {
				t.Fatal(err)
			}
			test.tamper(t, journal, ticket, ack)
			if _, err := journal.LoadAcknowledgement(context.Background(), ticket.Cursor); !errors.Is(
				err,
				generation.ErrIntegrity,
			) {
				t.Fatalf("LoadAcknowledgement() error = %v, want ErrIntegrity", err)
			}
		})
	}

	t.Run("unknown and stale cursor", func(t *testing.T) {
		journal := openTestJournal(t)
		ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
		token, err := journal.Stage(
			context.Background(), ticket, publicationSet(t, ticket, generation.DomainHTTP),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := journal.Commit(context.Background(), token); err != nil {
			t.Fatal(err)
		}
		unknown := generation.ProviderCursor{Provider: ticket.Cursor.Provider, Revision: "unknown"}
		if _, err := journal.LoadAcknowledgement(context.Background(), unknown); !errors.Is(
			err,
			generation.ErrNotFound,
		) {
			t.Fatalf("LoadAcknowledgement(unknown cursor) error = %v, want ErrNotFound", err)
		}
		_ = applyDesiredForPublication(t, journal, "2", generation.DomainHTTP)
		if _, err := journal.LoadAcknowledgement(context.Background(), ticket.Cursor); !errors.Is(
			err,
			generation.ErrStaleCursor,
		) {
			t.Fatalf("LoadAcknowledgement(stale cursor) error = %v, want ErrStaleCursor", err)
		}
	})

	t.Run("corrupt current active record while requested cursor is stale", func(t *testing.T) {
		journal := openTestJournal(t)
		oldTicket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
		oldToken, err := journal.Stage(
			context.Background(),
			oldTicket,
			publicationSet(t, oldTicket, generation.DomainHTTP),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := journal.Commit(context.Background(), oldToken); err != nil {
			t.Fatal(err)
		}
		currentTicket := applyDesiredForPublication(t, journal, "2", generation.DomainHTTP)
		if err := journal.db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(providerCursorBucket)
			key := cursorRecordKey(currentTicket.Cursor)
			record, err := decodeCursorRecord(bucket.Get(key), &currentTicket.Cursor)
			if err != nil {
				return err
			}
			record.Ticket.DesiredDigest[0]++
			encoded, err := encodeCursorRecord(record)
			if err != nil {
				return err
			}
			if err := bucket.Put(key, encoded); err != nil {
				return err
			}
			return bucket.Put(activeProviderRecordKey, encoded)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := journal.LoadAcknowledgement(
			context.Background(), oldTicket.Cursor,
		); !errors.Is(err, generation.ErrIntegrity) {
			t.Fatalf("LoadAcknowledgement(stale cursor with corrupt active) error = %v, want ErrIntegrity", err)
		}
	})

	t.Run("strict non-required revision", func(t *testing.T) {
		journal := openTestJournal(t)
		httpTicket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
		httpToken, err := journal.Stage(
			context.Background(), httpTicket, publicationSet(t, httpTicket, generation.DomainHTTP),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := journal.Commit(context.Background(), httpToken); err != nil {
			t.Fatal(err)
		}
		streamTicket := applyDesiredForPublication(t, journal, "2", generation.DomainStream)
		streamToken, err := journal.Stage(
			context.Background(),
			streamTicket,
			publicationSet(t, streamTicket, generation.DomainStream),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := journal.Commit(context.Background(), streamToken); err != nil {
			t.Fatal(err)
		}
		mutateCommittedAcknowledgement(
			t,
			journal,
			streamTicket,
			func(ack *generation.Acknowledgement) {
				ack.Revisions.HTTP = 0
			},
		)
		if _, err := journal.LoadAcknowledgement(context.Background(), streamTicket.Cursor); !errors.Is(
			err,
			generation.ErrIntegrity,
		) {
			t.Fatalf("LoadAcknowledgement(revision mismatch) error = %v, want ErrIntegrity", err)
		}
	})
}

func mutateCommittedAcknowledgement(
	t *testing.T,
	journal *Store,
	ticket generation.ApplyTicket,
	mutate func(*generation.Acknowledgement),
) {
	t.Helper()
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(providerCursorBucket)
		key := cursorRecordKey(ticket.Cursor)
		record, err := decodeCursorRecord(bucket.Get(key), &ticket.Cursor)
		if err != nil {
			return err
		}
		mutate(record.Committed)
		encoded, err := encodeCursorRecord(record)
		if err != nil {
			return err
		}
		if err := bucket.Put(key, encoded); err != nil {
			return err
		}
		return bucket.Put(activeProviderRecordKey, encoded)
	}); err != nil {
		t.Fatal(err)
	}
}

func assertAcknowledgementsEqual(
	t *testing.T,
	got generation.Acknowledgement,
	want generation.Acknowledgement,
) {
	t.Helper()
	if got.Cursor != want.Cursor || got.Revisions != want.Revisions ||
		!slices.Equal(
			got.Decisions[generation.DomainHTTP],
			want.Decisions[generation.DomainHTTP],
		) ||
		!slices.Equal(
			got.Decisions[generation.DomainStream],
			want.Decisions[generation.DomainStream],
		) ||
		len(got.Decisions) != len(want.Decisions) {
		t.Fatalf("acknowledgement = %+v, want %+v", got, want)
	}
}

func TestJournalStageRejectsTicketDomainCandidateAndClosureViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(generation.ApplyTicket, generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet)
		want   error
	}{
		{
			name: "forged ticket digest",
			want: generation.ErrIntegrity,
			mutate: func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
				ticket.DesiredDigest[0]++
				return ticket, set
			},
		},
		{
			name: "forged ticket cursor",
			want: generation.ErrIntegrity,
			mutate: func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
				ticket.Cursor.Revision = "forged"
				return ticket, set
			},
		},
		{
			name: "stale desired revision",
			want: generation.ErrIntegrity,
			mutate: func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
				ticket.DesiredRevision++
				set.DesiredRevision++
				return ticket, set
			},
		},
		{
			name: "set desired mismatch",
			want: generation.ErrIntegrity,
			mutate: func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
				set.DesiredRevision++
				return ticket, set
			},
		},
		{
			name: "missing required domain",
			want: generation.ErrIntegrity,
			mutate: func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
				delete(set.Domains, generation.DomainHTTP)
				return ticket, set
			},
		},
		{
			name: "extra domain",
			want: generation.ErrIntegrity,
			mutate: func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
				set.Domains[generation.DomainStream] = publicationCandidateFor(
					t,
					generation.DomainStream,
					ticket,
				)
				return ticket, set
			},
		},
		{
			name: "unknown domain",
			want: generation.ErrIntegrity,
			mutate: func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
				set.Domains["udp"] = set.Domains[generation.DomainHTTP]
				delete(set.Domains, generation.DomainHTTP)
				ticket.RequiredDomains = []generation.Domain{"udp"}
				return ticket, set
			},
		},
		{
			name: "artifact domain",
			want: generation.ErrIntegrity,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Artifact.Domain = generation.DomainStream
			}),
		},
		{
			name: "artifact revision",
			want: generation.ErrIntegrity,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Artifact.Revision++
			}),
		},
		{
			name: "artifact digest",
			want: generation.ErrIntegrity,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Artifact.Digest[0]++
			}),
		},
		{
			name: "artifact snapshot id",
			want: generation.ErrIntegrity,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Artifact.Snapshot += "x"
			}),
		},
		{
			name: "snapshot revision",
			want: generation.ErrIntegrity,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Snapshot = mustSnapshot(
					t,
					candidate.Snapshot.Revision()+1,
					candidate.Snapshot.Resources(),
					nil,
				)
			}),
		},
		{
			name: "missing closure key",
			want: generation.ErrInvalidClosure,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Closure = candidate.Closure[:1]
			}),
		},
		{
			name: "extra closure key",
			want: generation.ErrInvalidClosure,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Closure = append(
					candidate.Closure,
					generation.ResourceKey{Kind: "routes", ID: "missing"},
				)
				candidate.Decisions = append(
					candidate.Decisions,
					generation.ResourceDecision{
						Key:         candidate.Closure[len(candidate.Closure)-1],
						Disposition: generation.DispositionPublished,
						Code:        "extra",
					},
				)
			}),
		},
		{
			name: "duplicate closure",
			want: generation.ErrInvalidClosure,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Closure = append(candidate.Closure, candidate.Closure[0])
			}),
		},
		{
			name: "missing decision",
			want: generation.ErrInvalidClosure,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Decisions = candidate.Decisions[:1]
			}),
		},
		{
			name: "duplicate decision",
			want: generation.ErrInvalidClosure,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Decisions = append(candidate.Decisions, candidate.Decisions[0])
			}),
		},
		{
			name: "empty decision code",
			want: generation.ErrInvalidClosure,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Decisions[0].Code = ""
			}),
		},
		{
			name: "unknown decision disposition",
			want: generation.ErrInvalidClosure,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Decisions[0].Disposition = "unknown"
			}),
		},
		{
			name: "deleted resource",
			want: generation.ErrInvalidClosure,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Decisions[0].Disposition = generation.DispositionDeleted
			}),
		},
		{
			name: "non-deleted tombstone",
			want: generation.ErrInvalidClosure,
			mutate: mutateHTTPCandidate(func(candidate *generation.PublicationCandidate) {
				key := generation.ResourceKey{Kind: "routes", ID: "gone"}
				candidate.Snapshot = mustSnapshot(
					t,
					candidate.Snapshot.Revision(),
					candidate.Snapshot.Resources(),
					[]generation.Tombstone{{Key: key, Revision: candidate.Snapshot.Revision()}},
				)
				candidate.Artifact.Digest = candidate.Snapshot.Digest()
				candidate.Artifact.Snapshot = candidate.Snapshot.SnapshotID()
				candidate.Closure = append(candidate.Closure, key)
				candidate.Decisions = append(
					candidate.Decisions,
					generation.ResourceDecision{
						Key:         key,
						Disposition: generation.DispositionPublished,
						Code:        "wrong",
					},
				)
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
			set := publicationSet(t, ticket, generation.DomainHTTP)
			ticket, set = test.mutate(ticket, set)
			_, err := journal.Stage(context.Background(), ticket, set)
			if !errors.Is(err, test.want) {
				t.Fatalf("Stage() error = %v, want %v", err, test.want)
			}
			if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 0 {
				t.Fatalf("staged transaction count = %d, want 0", got)
			}
		})
	}
}

func TestJournalStageAcceptsStructurallyClosedDeletedTombstone(t *testing.T) {
	journal := openTestJournal(t)
	_ = applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	deleted := generation.ResourceKey{Kind: "routes", ID: "deleted"}
	ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "2"},
		Mutations: []generation.Mutation{{
			Type: generation.MutationDelete,
			Key:  deleted,
		}},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := publicationCandidateFor(t, generation.DomainHTTP, ticket)
	candidate.Snapshot = mustSnapshot(
		t,
		ticket.DesiredRevision,
		candidate.Snapshot.Resources(),
		[]generation.Tombstone{{Key: deleted, Revision: ticket.DesiredRevision}},
	)
	candidate.Artifact.Digest = candidate.Snapshot.Digest()
	candidate.Artifact.Snapshot = candidate.Snapshot.SnapshotID()
	candidate.Closure = append(candidate.Closure, deleted)
	candidate.Decisions = append(
		candidate.Decisions,
		generation.ResourceDecision{
			Key:         deleted,
			Disposition: generation.DispositionDeleted,
			Code:        "deleted",
		},
	)
	set := generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	token, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	published, err := journal.LoadPublished(context.Background(), generation.DomainHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if !published.Snapshot.Deleted(deleted) {
		t.Fatal("published tombstone is missing")
	}
}

func TestJournalStageCanonicalizesOrderAndDefensivelyCopies(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	set := publicationSet(t, ticket, generation.DomainHTTP)
	candidate := set.Domains[generation.DomainHTTP]
	reverse(candidate.Closure)
	reverse(candidate.Decisions)
	set.Domains[generation.DomainHTTP] = candidate
	token, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Decisions[0].Code = "mutated"
	candidate.Closure[0].ID = "mutated"
	set.Domains[generation.DomainHTTP] = candidate
	ticket.RequiredDomains[0] = generation.DomainStream
	ack, err := journal.Commit(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	decisions := ack.Decisions[generation.DomainHTTP]
	if len(decisions) != 2 || decisions[0].Key.ID != "r1" || decisions[0].Code == "mutated" {
		t.Fatalf("canonical decisions = %+v", decisions)
	}
	decisions[0].Code = "output-mutated"
	published, err := journal.LoadPublished(context.Background(), generation.DomainHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if published.Decisions[0].Code == "output-mutated" {
		t.Fatal("ack decisions alias durable state")
	}
	published.Snapshot.Resources()[0].Value[0] = 'x'
	published.Closure[0].ID = "x"
	published.Decisions[0].Code = "x"
	again, err := journal.LoadPublished(context.Background(), generation.DomainHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if again.Closure[0].ID == "x" || again.Decisions[0].Code == "x" {
		t.Fatal("LoadPublished result aliases durable state")
	}
}

func TestJournalAbortAndMissingTokensDoNotChangeHeads(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	token, err := journal.Stage(
		context.Background(),
		ticket,
		publicationSet(t, ticket, generation.DomainHTTP),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Abort(context.Background(), token, "activation"); err != nil {
		t.Fatal(err)
	}
	assertRevisions(t, journal, generation.RevisionSet{Desired: 1})
	if err := journal.Abort(context.Background(), token, "again"); !errors.Is(
		err,
		generation.ErrNotFound,
	) {
		t.Fatalf("second Abort() error = %v, want ErrNotFound", err)
	}
	if _, err := journal.Commit(context.Background(), token); !errors.Is(
		err,
		generation.ErrNotFound,
	) {
		t.Fatalf("Commit(aborted) error = %v, want ErrNotFound", err)
	}
	for _, reason := range []string{"", string([]byte{0xff}), strings.Repeat("x", 129)} {
		fresh, stageErr := journal.Stage(
			context.Background(),
			ticket,
			publicationSet(t, ticket, generation.DomainHTTP),
		)
		if stageErr != nil {
			t.Fatal(stageErr)
		}
		if err := journal.Abort(context.Background(), fresh, reason); err == nil {
			t.Fatalf("Abort() reason %q error = nil", reason)
		}
		if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 1 {
			t.Fatalf("invalid abort reason changed stage count to %d", got)
		}
		if err := journal.Abort(context.Background(), fresh, "valid-cleanup"); err != nil {
			t.Fatalf("cleanup Abort(): %v", err)
		}
	}
	for _, malformed := range []generation.PublicationToken{"", "short", generation.PublicationToken(strings.Repeat("z", 32))} {
		if _, err := journal.Commit(context.Background(), malformed); !errors.Is(
			err,
			generation.ErrNotFound,
		) {
			t.Fatalf("Commit(%q) error = %v, want ErrNotFound", malformed, err)
		}
	}
}

func TestJournalStageAndPublishedStateSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	token, err := journal.Stage(
		context.Background(),
		ticket,
		publicationSet(t, ticket, generation.DomainHTTP),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.LoadPublished(
		context.Background(),
		generation.DomainHTTP,
	); !errors.Is(
		err,
		generation.ErrNotFound,
	) {
		t.Fatalf("uncommitted LoadPublished() error = %v, want ErrNotFound", err)
	}
	if _, err := reopened.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err = OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.LoadPublished(context.Background(), generation.DomainHTTP); err != nil {
		t.Fatal(err)
	}
	assertRevisions(t, reopened, generation.RevisionSet{Desired: 1, HTTP: 1})
}

func TestJournalStageSurvivesRestartAndAbortRemovesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	token, err := journal.Stage(
		context.Background(),
		ticket,
		publicationSet(t, ticket, generation.DomainHTTP),
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Abort(context.Background(), token, "restart"); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Commit(context.Background(), token); !errors.Is(
		err,
		generation.ErrNotFound,
	) {
		t.Fatalf("Commit(aborted) error = %v, want ErrNotFound", err)
	}
}

func TestJournalCommitRejectsStaleStageAndCannotRegressOrOverwriteHead(t *testing.T) {
	journal := openTestJournal(t)
	oldTicket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	oldSet := publicationSet(t, oldTicket, generation.DomainHTTP)
	oldToken, err := journal.Stage(context.Background(), oldTicket, oldSet)
	if err != nil {
		t.Fatal(err)
	}
	secondOldToken, err := journal.Stage(context.Background(), oldTicket, oldSet)
	if err != nil {
		t.Fatal(err)
	}
	newTicket := applyDesiredForPublication(t, journal, "2", generation.DomainHTTP)
	newSet := publicationSet(t, newTicket, generation.DomainHTTP)
	newToken, err := journal.Stage(context.Background(), newTicket, newSet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), newToken); err != nil {
		t.Fatal(err)
	}
	for _, token := range []generation.PublicationToken{oldToken, secondOldToken} {
		if _, err := journal.Commit(context.Background(), token); !errors.Is(
			err,
			generation.ErrStaleCursor,
		) {
			t.Fatalf("Commit(stale) error = %v, want ErrStaleCursor", err)
		}
	}
	assertRevisions(t, journal, generation.RevisionSet{Desired: 2, HTTP: 2})
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 2 {
		t.Fatalf("staged transaction count = %d, want 2 retained stale stages", got)
	}
}

func TestJournalCommitOneDomainStaleRollsBackOtherDomain(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(
		t,
		journal,
		"1",
		generation.DomainHTTP,
		generation.DomainStream,
	)
	set := publicationSet(t, ticket, generation.DomainHTTP, generation.DomainStream)
	token, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	stream := set.Domains[generation.DomainStream]
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		if err := putArtifactTx(tx, stream.Snapshot); err != nil {
			return err
		}
		if err := putPublishedHeadTx(tx, generation.DomainStream, stream); err != nil {
			return err
		}
		return putDecisionsTx(tx, generation.DomainStream, stream.Artifact.Revision, stream.Decisions)
	}); err != nil {
		t.Fatal(err)
	}
	beforeArtifacts := bucketKeyCount(t, journal.db, artifactBucket)
	beforeHeads := bucketKeyCount(t, journal.db, publishedHeadBucket)
	beforeDecisions := bucketKeyCount(t, journal.db, publicationDecisionBucket)

	if _, err := journal.Commit(context.Background(), token); !errors.Is(
		err,
		generation.ErrStaleCursor,
	) {
		t.Fatalf("Commit() error = %v, want ErrStaleCursor", err)
	}
	assertRevisions(t, journal, generation.RevisionSet{Desired: 1, Stream: 1})
	if _, err := journal.LoadPublished(
		context.Background(),
		generation.DomainHTTP,
	); !errors.Is(err, generation.ErrNotFound) {
		t.Fatalf("LoadPublished(http) error = %v, want ErrNotFound", err)
	}
	if _, err := journal.LoadPublished(context.Background(), generation.DomainStream); err != nil {
		t.Fatalf("LoadPublished(stream) error = %v", err)
	}
	if got := bucketKeyCount(t, journal.db, artifactBucket); got != beforeArtifacts {
		t.Fatalf("artifact count = %d, want %d", got, beforeArtifacts)
	}
	if got := bucketKeyCount(t, journal.db, publishedHeadBucket); got != beforeHeads {
		t.Fatalf("published head count = %d, want %d", got, beforeHeads)
	}
	if got := bucketKeyCount(t, journal.db, publicationDecisionBucket); got != beforeDecisions {
		t.Fatalf("decision count = %d, want %d", got, beforeDecisions)
	}
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 1 {
		t.Fatalf("staged transaction count = %d, want stale token retained", got)
	}
}

func TestJournalCommitSecondTokenCannotOverwriteSameRevision(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	set := publicationSet(t, ticket, generation.DomainHTTP)
	first, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	second, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), second); !errors.Is(
		err,
		generation.ErrStaleCursor,
	) {
		t.Fatalf("second Commit() error = %v, want ErrStaleCursor", err)
	}
	assertRevisions(t, journal, generation.RevisionSet{Desired: 1, HTTP: 1})
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 1 {
		t.Fatalf("staged transaction count = %d, want rejected token retained", got)
	}
}

func TestJournalCommitConcurrentSameRevisionHasOneWinner(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	set := publicationSet(t, ticket, generation.DomainHTTP)
	first, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	second, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	for _, token := range []generation.PublicationToken{first, second} {
		workers.Go(func() {
			<-start
			_, commitErr := journal.Commit(context.Background(), token)
			errs <- commitErr
		})
	}
	close(start)
	workers.Wait()
	close(errs)
	var success, stale int
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, generation.ErrStaleCursor):
			stale++
		default:
			t.Fatalf("Commit() unexpected error = %v", err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("commit outcomes: success=%d stale=%d", success, stale)
	}
	assertRevisions(t, journal, generation.RevisionSet{Desired: 1, HTTP: 1})
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 1 {
		t.Fatalf("staged transaction count = %d, want losing token retained", got)
	}
}

func TestJournalCommitWriteFailureIsAtomicAndKeepsStage(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(
		t,
		journal,
		"1",
		generation.DomainHTTP,
		generation.DomainStream,
	)
	token, err := journal.Stage(
		context.Background(),
		ticket,
		publicationSet(t, ticket, generation.DomainHTTP, generation.DomainStream),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(
		func(tx *bolt.Tx) error { return tx.DeleteBucket(publicationDecisionBucket) },
	); err != nil {
		t.Fatal(err)
	}
	beforeArtifacts := bucketKeyCount(t, journal.db, artifactBucket)
	beforeHeads := bucketKeyCount(t, journal.db, publishedHeadBucket)
	if _, err := journal.Commit(context.Background(), token); !errors.Is(
		err,
		generation.ErrIntegrity,
	) {
		t.Fatalf("Commit() error = %v, want ErrIntegrity", err)
	}
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		_, createErr := tx.CreateBucket(publicationDecisionBucket)
		return createErr
	}); err != nil {
		t.Fatal(err)
	}
	assertRevisions(t, journal, generation.RevisionSet{Desired: 1})
	if got := bucketKeyCount(t, journal.db, artifactBucket); got != beforeArtifacts {
		t.Fatalf("artifact count = %d, want %d", got, beforeArtifacts)
	}
	if got := bucketKeyCount(t, journal.db, publishedHeadBucket); got != beforeHeads {
		t.Fatalf("published head count = %d, want %d", got, beforeHeads)
	}
	if got := bucketKeyCount(t, journal.db, publicationDecisionBucket); got != 0 {
		t.Fatalf("decision count = %d, want 0", got)
	}
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 1 {
		t.Fatalf("staged transaction count = %d, want 1", got)
	}
}

func TestJournalPublishedTamperingFailsClosed(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	token, err := journal.Stage(
		context.Background(),
		ticket,
		publicationSet(t, ticket, generation.DomainHTTP),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range [][]byte{publishedHeadBucket, artifactBucket, publicationDecisionBucket} {
		t.Run(string(bucket), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			copyJournal(t, journal.db, path)
			candidate, openErr := OpenJournal(path, JournalOptions{})
			if openErr != nil {
				t.Fatal(openErr)
			}
			t.Cleanup(func() { _ = candidate.Close() })
			if err := candidate.db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket(bucket)
				var key []byte
				switch string(bucket) {
				case string(publishedHeadBucket):
					key = []byte(generation.DomainHTTP)
				case string(publicationDecisionBucket):
					key = publicationDecisionKey(generation.DomainHTTP, ticket.DesiredRevision)
				case string(artifactBucket):
					head, decodeErr := decodePublishedHead(
						tx.Bucket(publishedHeadBucket).Get([]byte(generation.DomainHTTP)),
						generation.DomainHTTP,
					)
					if decodeErr != nil {
						return decodeErr
					}
					key = []byte(head.Artifact.Snapshot)
				}
				return b.Put(key, []byte(`{"tampered":true}`))
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := candidate.LoadPublished(
				context.Background(),
				generation.DomainHTTP,
			); !errors.Is(
				err,
				generation.ErrIntegrity,
			) {
				t.Fatalf("LoadPublished() error = %v, want ErrIntegrity", err)
			}
		})
	}
}

func TestJournalPublishedDecisionCrossDigestRejectsValidButSwappedRecord(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	token, err := journal.Stage(
		context.Background(),
		ticket,
		publicationSet(t, ticket, generation.DomainHTTP),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		payload, encodeErr := encodeDecisionsPayload(generation.DomainHTTP, 1, []generation.ResourceDecision{
			{
				Key:         generation.ResourceKey{Kind: "routes", ID: "r1"},
				Disposition: generation.DispositionPublished,
				Code:        "changed",
			},
			{
				Key:         generation.ResourceKey{Kind: "upstreams", ID: "u1"},
				Disposition: generation.DispositionPublished,
				Code:        "changed",
			},
		})
		if encodeErr != nil {
			return encodeErr
		}
		encoded, encodeErr := encodePublicationEnvelope(payload)
		if encodeErr != nil {
			return encodeErr
		}
		return tx.Bucket(publicationDecisionBucket).Put(publicationDecisionKey(generation.DomainHTTP, 1), encoded)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.LoadPublished(
		context.Background(),
		generation.DomainHTTP,
	); !errors.Is(
		err,
		generation.ErrIntegrity,
	) {
		t.Fatalf("LoadPublished() error = %v, want ErrIntegrity", err)
	}
}

func TestJournalLoadPublishedMapsPersistedClosureMismatchToIntegrity(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	token, err := journal.Stage(
		context.Background(),
		ticket,
		publicationSet(t, ticket, generation.DomainHTTP),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(publishedHeadBucket)
		payload, err := decodePublicationEnvelope(bucket.Get([]byte(generation.DomainHTTP)))
		if err != nil {
			return err
		}
		var head publishedHeadWire
		if err := json.Unmarshal(payload, &head); err != nil {
			return err
		}
		head.Closure = head.Closure[:1]
		payload, err = json.Marshal(head)
		if err != nil {
			return err
		}
		encoded, err := encodePublicationEnvelope(payload)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(generation.DomainHTTP), encoded)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.LoadPublished(
		context.Background(),
		generation.DomainHTTP,
	); !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("LoadPublished() error = %v, want ErrIntegrity", err)
	}
}

func TestJournalLoadPublishedAndRevisionsRejectUnknownOrMalformedHeads(t *testing.T) {
	for _, test := range []struct {
		name string
		key  generation.Domain
	}{
		{name: "unknown", key: "udp"},
		{name: "malformed known", key: generation.DomainHTTP},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			if _, err := journal.LoadPublished(context.Background(), "udp"); !errors.Is(
				err,
				generation.ErrNotFound,
			) {
				t.Fatalf("LoadPublished(udp) error = %v, want ErrNotFound", err)
			}
			if err := journal.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket(publishedHeadBucket).Put([]byte(test.key), []byte(`{"bad":true}`))
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Revisions(context.Background()); !errors.Is(
				err,
				generation.ErrIntegrity,
			) {
				t.Fatalf("Revisions() error = %v, want ErrIntegrity", err)
			}
			if test.key == generation.DomainHTTP {
				if _, err := journal.LoadPublished(
					context.Background(),
					generation.DomainHTTP,
				); !errors.Is(
					err,
					generation.ErrIntegrity,
				) {
					t.Fatalf("LoadPublished() error = %v, want ErrIntegrity", err)
				}
			}
		})
	}
}

func TestJournalRevisionsRejectsSemanticallyMalformedPublishedHeads(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*publishedHeadWire)
	}{
		{
			name:   "revision newer than desired",
			mutate: func(head *publishedHeadWire) { head.Artifact.Revision++ },
		},
		{
			name:   "zero decision digest",
			mutate: func(head *publishedHeadWire) { head.DecisionsDigest = [32]byte{} },
		},
		{
			name:   "invalid snapshot id",
			mutate: func(head *publishedHeadWire) { head.Artifact.Snapshot = "sha256:bad" },
		},
		{
			name:   "duplicate closure",
			mutate: func(head *publishedHeadWire) { head.Closure = append(head.Closure, head.Closure[0]) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
			token, err := journal.Stage(
				context.Background(),
				ticket,
				publicationSet(t, ticket, generation.DomainHTTP),
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Commit(context.Background(), token); err != nil {
				t.Fatal(err)
			}
			if err := journal.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket(publishedHeadBucket)
				payload, decodeErr := decodePublicationEnvelope(bucket.Get([]byte(generation.DomainHTTP)))
				if decodeErr != nil {
					return decodeErr
				}
				var head publishedHeadWire
				if decodeErr := json.Unmarshal(payload, &head); decodeErr != nil {
					return decodeErr
				}
				test.mutate(&head)
				payload, encodeErr := json.Marshal(head)
				if encodeErr != nil {
					return encodeErr
				}
				encoded, encodeErr := encodePublicationEnvelope(payload)
				if encodeErr != nil {
					return encodeErr
				}
				return bucket.Put([]byte(generation.DomainHTTP), encoded)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Revisions(context.Background()); !errors.Is(
				err,
				generation.ErrIntegrity,
			) {
				t.Fatalf("Revisions() error = %v, want ErrIntegrity", err)
			}
		})
	}
}

func TestJournalPublicationContextAndClosedStoreErrors(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	set := publicationSet(t, ticket, generation.DomainHTTP)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := journal.Stage(ctx, ticket, set); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stage() error = %v, want context canceled", err)
	}
	token, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(ctx, token); !errors.Is(err, context.Canceled) {
		t.Fatalf("Commit() error = %v, want context canceled", err)
	}
	if err := journal.Abort(ctx, token, "cancel"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Abort() error = %v, want context canceled", err)
	}
	if _, err := journal.LoadPublished(ctx, generation.DomainHTTP); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("LoadPublished() error = %v, want context canceled", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Stage(context.Background(), ticket, set); err == nil {
		t.Fatal("Stage() after close error = nil")
	}
	if _, err := journal.Commit(context.Background(), token); err == nil {
		t.Fatal("Commit() after close error = nil")
	}
	if err := journal.Abort(context.Background(), token, "closed"); err == nil {
		t.Fatal("Abort() after close error = nil")
	}
	if _, err := journal.LoadPublished(context.Background(), generation.DomainHTTP); err == nil {
		t.Fatal("LoadPublished() after close error = nil")
	}
}

func TestJournalPublicationCancellationRollsBackCompletedWrites(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	set := publicationSet(t, ticket, generation.DomainHTTP)

	// Stage checks the context at entry, transaction entry, immediately before
	// Put, and immediately after Put. Cancel on the final check so the test
	// proves that bbolt rolls the completed Put back.
	stageCtx := newStepCancelContext(4)
	if _, err := journal.Stage(stageCtx, ticket, set); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stage() error = %v, want context canceled", err)
	}
	assertStepCancellation(t, stageCtx, 4)
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 0 {
		t.Fatalf("staged transaction count after canceled Stage = %d, want 0", got)
	}

	token, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	// Commit checks at entry, transaction entry, immediately before deleting
	// the token, and after the delete plus final revision load. Cancel at the
	// fourth check to require rollback of artifacts, head, decisions and token.
	commitCtx := newStepCancelContext(4)
	if _, err := journal.Commit(commitCtx, token); !errors.Is(err, context.Canceled) {
		t.Fatalf("Commit() error = %v, want context canceled", err)
	}
	assertStepCancellation(t, commitCtx, 4)
	assertRevisions(t, journal, generation.RevisionSet{Desired: 1})
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 1 {
		t.Fatalf("staged transaction count after canceled Commit = %d, want 1", got)
	}
	if got := bucketKeyCount(t, journal.db, publishedHeadBucket); got != 0 {
		t.Fatalf("published head count after canceled Commit = %d, want 0", got)
	}

	// Abort's third check occurs after Delete, so cancellation must retain the
	// token by rolling the transaction back.
	abortCtx := newStepCancelContext(3)
	if err := journal.Abort(abortCtx, token, "cancel-after-delete"); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("Abort() error = %v, want context canceled", err)
	}
	assertStepCancellation(t, abortCtx, 3)
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 1 {
		t.Fatalf("staged transaction count after canceled Abort = %d, want 1", got)
	}

	// Revisions' third check occurs after the full revision set has loaded.
	revisionsCtx := newStepCancelContext(3)
	if _, err := journal.Revisions(revisionsCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Revisions() error = %v, want context canceled", err)
	}
	assertStepCancellation(t, revisionsCtx, 3)
}

func TestJournalPublicationWireGoldenAndTokenCollision(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	set := publicationSet(t, ticket, generation.DomainHTTP)
	fixedToken := generation.PublicationToken(strings.Repeat("ab", 16))
	encoded, err := encodeStagedPublication(
		stagedPublication{Token: fixedToken, Ticket: ticket, Set: set},
	)
	if err != nil {
		t.Fatal(err)
	}
	var envelope publicationEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Digest != sha256.Sum256(envelope.Payload) ||
		!bytes.Contains(envelope.Payload, []byte(`"desired_revision":1`)) ||
		bytes.Contains(envelope.Payload, []byte(`"Artifact"`)) {
		t.Fatalf("unexpected staged wire payload: %s", envelope.Payload)
	}
	wantPayload := []byte(
		`{"token":"abababababababababababababababab","ticket":{"desired_revision":1,"desired_digest":[255,34,247,236,119,115,175,167,88,110,191,95,97,222,84,148,88,119,130,47,116,145,122,203,199,27,184,81,131,1,232,154],"cursor":{"provider":"etcd","revision":"1"},"required_domains":["http"]},"desired_revision":1,"domains":[{"domain":"http","candidate":{"artifact":{"domain":"http","revision":1,"digest":[255,34,247,236,119,115,175,167,88,110,191,95,97,222,84,148,88,119,130,47,116,145,122,203,199,27,184,81,131,1,232,154],"snapshot":"sha256:ff22f7ec7773afa7586ebf5f61de54945877822f74917acbc71bb8518301e89a"},"snapshot_payload":"eyJyZXZpc2lvbiI6MSwicmVzb3VyY2VzIjpbeyJrZXkiOnsia2luZCI6InJvdXRlcyIsImlkIjoicjEifSwidmFsdWUiOiJleUpwWkNJNkluSXhJbjA9In0seyJrZXkiOnsia2luZCI6InVwc3RyZWFtcyIsImlkIjoidTEifSwidmFsdWUiOiJleUpwWkNJNkluVXhJbjA9In1dLCJ0b21ic3RvbmVzIjpudWxsfQ==","closure":[{"kind":"routes","id":"r1"},{"kind":"upstreams","id":"u1"}],"decisions":[{"key":{"kind":"routes","id":"r1"},"disposition":"published","code":"test-published"},{"key":{"kind":"upstreams","id":"u1"},"disposition":"published","code":"test-published"}]}}]}`,
	)
	if envelope.Size != uint64(len(wantPayload)) {
		t.Fatalf("envelope size = %d, want %d", envelope.Size, len(wantPayload))
	}
	if !bytes.Equal(envelope.Payload, wantPayload) {
		t.Fatalf("staged payload = %s, want %s", envelope.Payload, wantPayload)
	}

	previous := publicationTokenReader
	publicationTokenReader = bytes.NewReader(
		bytes.Repeat([]byte{0x11}, (publicationTokenAttempts+1)*16),
	)
	t.Cleanup(func() { publicationTokenReader = previous })
	first, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if first != generation.PublicationToken(strings.Repeat("11", 16)) {
		t.Fatalf("token = %q", first)
	}
	if _, err := journal.Stage(
		context.Background(),
		ticket,
		set,
	); err == nil ||
		!strings.Contains(err.Error(), "collision limit exceeded") {
		t.Fatalf("collision exhaustion error = %v", err)
	}
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 1 {
		t.Fatalf("staged transaction count = %d, want original token only", got)
	}
}

func TestJournalStagedTokenBindingRejectsSwappedPayload(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	set := publicationSet(t, ticket, generation.DomainHTTP)
	first, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	second, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(publicationTxnBucket)
		return bucket.Put([]byte(second), bytes.Clone(bucket.Get([]byte(first))))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), second); !errors.Is(
		err,
		generation.ErrIntegrity,
	) {
		t.Fatalf("Commit(swapped token payload) error = %v, want ErrIntegrity", err)
	}
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 2 {
		t.Fatalf("staged transaction count = %d, want both retained", got)
	}
}

func mutateHTTPCandidate(
	mutate func(*generation.PublicationCandidate),
) func(generation.ApplyTicket, generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
	return func(ticket generation.ApplyTicket, set generation.PublicationSet) (generation.ApplyTicket, generation.PublicationSet) {
		candidate := set.Domains[generation.DomainHTTP]
		mutate(&candidate)
		set.Domains[generation.DomainHTTP] = candidate
		return ticket, set
	}
}

func applyDesiredForPublication(
	t *testing.T,
	journal *Store,
	revision string,
	domains ...generation.Domain,
) generation.ApplyTicket {
	t.Helper()
	mutations := make([]generation.Mutation, 0, 3)
	for _, domain := range domains {
		switch domain {
		case generation.DomainHTTP:
			mutations = append(
				mutations,
				generation.Mutation{
					Type:  generation.MutationPut,
					Key:   generation.ResourceKey{Kind: "routes", ID: "r1"},
					Value: []byte(`{"id":"r1"}`),
				},
				generation.Mutation{
					Type:  generation.MutationPut,
					Key:   generation.ResourceKey{Kind: "upstreams", ID: "u1"},
					Value: []byte(`{"id":"u1"}`),
				},
			)
		case generation.DomainStream:
			mutations = append(mutations, generation.Mutation{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: "stream_routes", ID: "s1"},
				Value: []byte(`{"id":"s1"}`),
			})
		}
	}
	ticket, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor:          generation.ProviderCursor{Provider: "etcd", Revision: revision},
		Mutations:       mutations,
		RequiredDomains: domains,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func publicationSet(
	t *testing.T,
	ticket generation.ApplyTicket,
	domains ...generation.Domain,
) generation.PublicationSet {
	t.Helper()
	set := generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate),
	}
	for _, domain := range domains {
		set.Domains[domain] = publicationCandidateFor(t, domain, ticket)
	}
	return set
}

func publicationCandidateFor(
	t *testing.T,
	domain generation.Domain,
	ticket generation.ApplyTicket,
) generation.PublicationCandidate {
	t.Helper()
	var keys []generation.ResourceKey
	switch domain {
	case generation.DomainHTTP:
		keys = []generation.ResourceKey{{Kind: "routes", ID: "r1"}, {Kind: "upstreams", ID: "u1"}}
	case generation.DomainStream:
		keys = []generation.ResourceKey{{Kind: "stream_routes", ID: "s1"}}
	default:
		keys = []generation.ResourceKey{{Kind: "unknown", ID: "x"}}
	}
	resources := make([]generation.Resource, 0, len(keys))
	decisions := make([]generation.ResourceDecision, 0, len(keys))
	for _, key := range keys {
		resources = append(
			resources,
			generation.Resource{Key: key, Value: fmt.Appendf(nil, `{"id":%q}`, key.ID)},
		)
		decisions = append(
			decisions,
			generation.ResourceDecision{
				Key:         key,
				Disposition: generation.DispositionPublished,
				Code:        "test-published",
			},
		)
	}
	snapshot := mustSnapshot(t, ticket.DesiredRevision, resources, nil)
	return generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain:   domain,
			Revision: ticket.DesiredRevision,
			Digest:   snapshot.Digest(),
			Snapshot: snapshot.SnapshotID(),
		},
		Snapshot:  snapshot,
		Closure:   append([]generation.ResourceKey(nil), keys...),
		Decisions: decisions,
	}
}

func mustSnapshot(
	t *testing.T,
	revision uint64,
	resources []generation.Resource,
	tombstones []generation.Tombstone,
) generation.Snapshot {
	t.Helper()
	snapshot, err := generation.NewSnapshot(revision, resources, tombstones)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func reverse[T any](values []T) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

type stepCancelContext struct {
	context.Context
	cancelAt int32
	calls    atomic.Int32
	done     chan struct{}
	once     sync.Once
}

func newStepCancelContext(cancelAt int32) *stepCancelContext {
	return &stepCancelContext{
		Context:  context.Background(),
		cancelAt: cancelAt,
		done:     make(chan struct{}),
	}
}

func (c *stepCancelContext) Done() <-chan struct{} {
	if c.calls.Add(1) >= c.cancelAt {
		c.once.Do(func() { close(c.done) })
	}
	return c.done
}

func (c *stepCancelContext) Err() error {
	if c.calls.Load() >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func assertStepCancellation(t *testing.T, ctx *stepCancelContext, want int32) {
	t.Helper()
	if got := ctx.calls.Load(); got != want {
		t.Fatalf("context checks = %d, want cancellation at check %d", got, want)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("context cancellation checkpoint was not reached")
	}
}

func clearCommittedAcknowledgement(
	t *testing.T,
	journal *Store,
	cursor generation.ProviderCursor,
) {
	t.Helper()
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(providerCursorBucket)
		record, err := decodeCursorRecord(bucket.Get(cursorRecordKey(cursor)), &cursor)
		if err != nil {
			return err
		}
		record.Committed = nil
		encoded, err := encodeCursorRecord(record)
		if err != nil {
			return err
		}
		if err := bucket.Put(cursorRecordKey(cursor), encoded); err != nil {
			return err
		}
		return bucket.Put(activeProviderRecordKey, encoded)
	}); err != nil {
		t.Fatal(err)
	}
}

func assertCursorHasCommittedAcknowledgement(
	t *testing.T,
	journal *Store,
	cursor generation.ProviderCursor,
) {
	t.Helper()
	if err := journal.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(providerCursorBucket)
		record, err := decodeCursorRecord(bucket.Get(cursorRecordKey(cursor)), &cursor)
		if err != nil {
			return err
		}
		if record.Committed == nil {
			return errors.New("cursor acknowledgement marker is missing")
		}
		if !bytes.Equal(
			bucket.Get(cursorRecordKey(cursor)),
			bucket.Get(activeProviderRecordKey),
		) {
			return errors.New("active and keyed cursor records differ")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func copyJournal(t *testing.T, db *bolt.DB, path string) {
	t.Helper()
	if err := db.View(func(tx *bolt.Tx) error { return tx.CopyFile(path, 0o600) }); err != nil {
		t.Fatal(err)
	}
}

func TestJournalPublishedKeyEncodingGolden(t *testing.T) {
	key := publicationDecisionKey(generation.DomainHTTP, 42)
	if got := hex.EncodeToString(key); got != "68747470000000000000002a" {
		t.Fatalf("decision key = %s", got)
	}
	if fmt.Sprint(sortedPublicationDomains(map[generation.Domain]generation.PublicationCandidate{
		generation.DomainStream: {}, generation.DomainHTTP: {},
	})) != "[http stream]" {
		t.Fatal("publication domains are not canonical")
	}
}
