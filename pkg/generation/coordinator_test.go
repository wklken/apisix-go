package generation

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
)

func TestCoordinatorAppliesDesiredStateInMemory(t *testing.T) {
	engine := &coordinatorPublisher{}
	coordinator := NewCoordinator(engine)

	first := DesiredBatch{
		Cursor:          ProviderCursor{Provider: "etcd", Revision: "1"},
		ReplaceManaged:  true,
		RequiredDomains: []Domain{DomainHTTP, DomainStream},
		Mutations: []Mutation{
			{Type: MutationPut, Key: ResourceKey{Kind: "routes", ID: "r1"}, Value: []byte(`{"id":"r1"}`)},
			{Type: MutationPut, Key: ResourceKey{Kind: "routes", ID: "r2"}, Value: []byte(`{"id":"r2"}`)},
		},
	}
	ack, err := coordinator.Apply(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Revisions != (RevisionSet{Desired: 1, HTTP: 1, Stream: 1}) {
		t.Fatalf("first revisions = %+v", ack.Revisions)
	}

	second := DesiredBatch{
		Cursor:          ProviderCursor{Provider: "etcd", Revision: "2"},
		RequiredDomains: []Domain{DomainHTTP},
		Mutations: []Mutation{
			{Type: MutationDelete, Key: ResourceKey{Kind: "routes", ID: "r1"}},
			{Type: MutationPut, Key: ResourceKey{Kind: "routes", ID: "r3"}, Value: []byte(`{"id":"r3"}`)},
		},
	}
	ack, err = coordinator.Apply(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Revisions != (RevisionSet{Desired: 2, HTTP: 2, Stream: 1}) {
		t.Fatalf("second revisions = %+v", ack.Revisions)
	}
	got := engine.lastDesired
	if _, found := got.Lookup(ResourceKey{Kind: "routes", ID: "r1"}); found {
		t.Fatal("deleted route r1 remains in desired state")
	}
	if !got.Deleted(ResourceKey{Kind: "routes", ID: "r1"}) {
		t.Fatal("deleted route r1 has no tombstone")
	}
	for _, id := range []string{"r2", "r3"} {
		if _, found := got.Lookup(ResourceKey{Kind: "routes", ID: id}); !found {
			t.Fatalf("route %s missing from desired state", id)
		}
	}
}

func TestCoordinatorPublishesTombstoneOnceThenCompactsDesiredState(t *testing.T) {
	engine := &coordinatorPublisher{}
	coordinator := NewCoordinator(engine)
	if _, err := coordinator.Apply(context.Background(), coordinatorBatch("1", "r1")); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Apply(context.Background(), DesiredBatch{
		Cursor:          ProviderCursor{Provider: "etcd", Revision: "2"},
		RequiredDomains: []Domain{DomainHTTP},
		Mutations: []Mutation{{
			Type: MutationDelete, Key: ResourceKey{Kind: "routes", ID: "r1"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if !engine.lastDesired.Deleted(ResourceKey{Kind: "routes", ID: "r1"}) {
		t.Fatal("deletion publication did not carry the tombstone")
	}
	if _, err := coordinator.Apply(context.Background(), coordinatorBatch("3", "r2")); err != nil {
		t.Fatal(err)
	}
	if engine.lastDesired.Deleted(ResourceKey{Kind: "routes", ID: "r1"}) {
		t.Fatal("acknowledged tombstone leaked into the next publication")
	}
}

func TestCoordinatorCommitsOnlyAfterSuccessfulPublish(t *testing.T) {
	wantErr := errors.New("publish failed")
	engine := &coordinatorPublisher{failAtCall: 2, publishErr: wantErr}
	coordinator := NewCoordinator(engine)

	if _, err := coordinator.Apply(context.Background(), coordinatorBatch("1", "r1")); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Apply(context.Background(), coordinatorBatch("2", "r2")); !errors.Is(err, wantErr) {
		t.Fatalf("failed publish error = %v", err)
	}
	engine.failAtCall = 0
	ack, err := coordinator.Apply(context.Background(), coordinatorBatch("2", "r2"))
	if err != nil {
		t.Fatal(err)
	}
	if ack.Revisions.Desired != 2 {
		t.Fatalf("desired revision after retry = %d, want 2", ack.Revisions.Desired)
	}
	if engine.lastTicket.DesiredRevision != 2 {
		t.Fatalf("published revision after retry = %d, want 2", engine.lastTicket.DesiredRevision)
	}
}

func TestCoordinatorReplaysOnlyTheSameCanonicalBatch(t *testing.T) {
	engine := &coordinatorPublisher{}
	coordinator := NewCoordinator(engine)
	batch := coordinatorBatch("1", "r1")

	first, err := coordinator.Apply(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := coordinator.Apply(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if engine.calls != 1 {
		t.Fatalf("publish calls = %d, want 1", engine.calls)
	}
	if !reflect.DeepEqual(replayed, first) {
		t.Fatalf("replayed acknowledgement = %+v, want %+v", replayed, first)
	}

	conflict := coordinatorBatch("1", "r2")
	if _, err := coordinator.Apply(context.Background(), conflict); !errors.Is(err, ErrCursorConflict) {
		t.Fatalf("cursor conflict error = %v", err)
	}
}

func TestCoordinatorRejectsIncrementalProviderSwitch(t *testing.T) {
	coordinator := NewCoordinator(&coordinatorPublisher{})
	if _, err := coordinator.Apply(context.Background(), coordinatorBatch("1", "r1")); err != nil {
		t.Fatal(err)
	}
	batch := coordinatorBatch("2", "r2")
	batch.Cursor.Provider = "standalone"
	if _, err := coordinator.Apply(context.Background(), batch); !errors.Is(err, ErrProviderConflict) {
		t.Fatalf("provider conflict error = %v", err)
	}
}

func TestCoordinatorSerializesApply(t *testing.T) {
	engine := &coordinatorPublisher{entered: make(chan struct{}), release: make(chan struct{})}
	coordinator := NewCoordinator(engine)
	done := make(chan error, 2)
	go func() {
		_, err := coordinator.Apply(context.Background(), coordinatorBatch("1", "r1"))
		done <- err
	}()
	<-engine.entered
	go func() {
		_, err := coordinator.Apply(context.Background(), coordinatorBatch("2", "r2"))
		done <- err
	}()
	close(engine.release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if engine.maxActive != 1 {
		t.Fatalf("maximum concurrent publishes = %d, want 1", engine.maxActive)
	}
}

type coordinatorPublisher struct {
	mu          sync.Mutex
	calls       int
	active      int
	maxActive   int
	failAtCall  int
	publishErr  error
	entered     chan struct{}
	release     chan struct{}
	lastTicket  ApplyTicket
	lastDesired Snapshot
}

func (p *coordinatorPublisher) Publish(
	_ context.Context,
	ticket ApplyTicket,
	desired Snapshot,
	_ map[Domain]PublishedGeneration,
) (PublicationSet, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.active++
	if p.active > p.maxActive {
		p.maxActive = p.active
	}
	p.lastTicket = ticket
	p.lastDesired = desired.Clone()
	p.mu.Unlock()
	if p.entered != nil && call == 1 {
		close(p.entered)
		<-p.release
	}
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	if call == p.failAtCall {
		return PublicationSet{}, p.publishErr
	}

	set := PublicationSet{DesiredRevision: ticket.DesiredRevision, Domains: map[Domain]PublicationCandidate{}}
	for _, domain := range ticket.RequiredDomains {
		set.Domains[domain] = coordinatorCandidate(ticket, domain, desired)
	}
	return set, nil
}

func coordinatorCandidate(ticket ApplyTicket, domain Domain, desired Snapshot) PublicationCandidate {
	resources := make([]Resource, 0)
	tombstones := make([]Tombstone, 0)
	closure := make([]ResourceKey, 0)
	decisions := make([]ResourceDecision, 0)
	for _, resource := range desired.Resources() {
		if slices.Contains(DomainsForResourceKind(resource.Key.Kind), domain) {
			resources = append(resources, resource)
			closure = append(closure, resource.Key)
			decisions = append(decisions, ResourceDecision{
				Key: resource.Key, Disposition: DispositionPublished, Code: "test-published",
			})
		}
	}
	for _, tombstone := range desired.Tombstones() {
		if slices.Contains(DomainsForResourceKind(tombstone.Key.Kind), domain) {
			tombstones = append(tombstones, tombstone)
			closure = append(closure, tombstone.Key)
			decisions = append(decisions, ResourceDecision{
				Key: tombstone.Key, Disposition: DispositionDeleted, Code: "test-deleted",
			})
		}
	}
	snapshot, err := NewSnapshot(ticket.DesiredRevision, resources, tombstones)
	if err != nil {
		panic(err)
	}
	return PublicationCandidate{
		Artifact: GenerationArtifact{
			Domain: domain, Revision: ticket.DesiredRevision,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot, Closure: closure, Decisions: decisions,
	}
}

func coordinatorBatch(revision, id string) DesiredBatch {
	return DesiredBatch{
		Cursor: ProviderCursor{Provider: "etcd", Revision: revision},
		Mutations: []Mutation{{
			Type: MutationPut, Key: ResourceKey{Kind: "routes", ID: id}, Value: []byte(`{"id":"` + id + `"}`),
		}},
		RequiredDomains: []Domain{DomainHTTP},
	}
}
