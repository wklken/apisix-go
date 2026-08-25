package etcd

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/testutil"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestDesiredBatchFromEtcdSnapshotReplacesManagedNamespace(t *testing.T) {
	value := []byte(`{"id":"r1"}`)
	batch, err := desiredBatchFromEtcdSnapshot("apisix", &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 0xabc, Revision: 71},
		Kvs: []*mvccpb.KeyValue{
			{Key: []byte("/apisix/routes/r1"), Value: value, ModRevision: 69},
			{Key: []byte("/apisix/data_plane/server_info/node-1"), Value: []byte(`{"id":"node-1"}`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Cursor != (generation.ProviderCursor{
		Provider: etcdProviderID(0xabc, "/apisix/"), Revision: "71",
	}) {
		t.Fatalf("cursor = %+v", batch.Cursor)
	}
	if !batch.ReplaceManaged {
		t.Fatal("ReplaceManaged = false, want authoritative replacement")
	}
	if !slices.Equal(batch.RequiredDomains, []generation.Domain{
		generation.DomainHTTP, generation.DomainStream,
	}) {
		t.Fatalf("required domains = %v", batch.RequiredDomains)
	}
	if len(batch.Mutations) != 1 || batch.Mutations[0].Type != generation.MutationPut ||
		batch.Mutations[0].Key != (generation.ResourceKey{Kind: "routes", ID: "r1"}) ||
		string(batch.Mutations[0].Value) != string(value) {
		t.Fatalf("mutations = %+v", batch.Mutations)
	}
	value[0] = 'x'
	if string(batch.Mutations[0].Value) != `{"id":"r1"}` {
		t.Fatalf("mutation value aliases etcd response: %q", batch.Mutations[0].Value)
	}
}

func TestDesiredBatchFromEtcdSnapshotPreservesNilAndEmptyPutValues(t *testing.T) {
	batch, err := desiredBatchFromEtcdSnapshot("/apisix", &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 1},
		Kvs: []*mvccpb.KeyValue{
			{Key: []byte("/apisix/routes/nil"), Value: nil},
			{Key: []byte("/apisix/routes/empty"), Value: []byte{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Mutations[0].Value != nil {
		t.Fatalf("nil value = %#v, want nil", batch.Mutations[0].Value)
	}
	if batch.Mutations[1].Value == nil || len(batch.Mutations[1].Value) != 0 {
		t.Fatalf("empty value = %#v, want non-nil empty bytes", batch.Mutations[1].Value)
	}
}

func TestDesiredBatchFromEtcdWatchCarriesRevisionDeleteAndDomains(t *testing.T) {
	batch, err := desiredBatchFromEtcdWatch("/apisix/", clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: 0xabc, Revision: 71},
		Events: []*clientv3.Event{{
			Type: mvccpb.DELETE,
			Kv:   &mvccpb.KeyValue{Key: []byte("/apisix/stream_routes/mqtt"), ModRevision: 71},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Cursor != (generation.ProviderCursor{
		Provider: etcdProviderID(0xabc, "/apisix/"), Revision: "71",
	}) || batch.ReplaceManaged || len(batch.Mutations) != 1 ||
		batch.Mutations[0].Type != generation.MutationDelete || batch.Mutations[0].Value != nil ||
		batch.Mutations[0].Key != (generation.ResourceKey{Kind: "stream_routes", ID: "mqtt"}) ||
		!slices.Equal(batch.RequiredDomains, []generation.Domain{generation.DomainStream}) {
		t.Fatalf("batch = %+v", batch)
	}
}

func TestDesiredBatchFromEtcdWatchMarksPluginsForHTTPAndStream(t *testing.T) {
	batch, err := desiredBatchFromEtcdWatch("/apisix", clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: 0xabc, Revision: 72},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key: []byte("/apisix/plugins"), Value: []byte(`[]`), ModRevision: 72,
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Mutations) != 1 ||
		batch.Mutations[0].Key != (generation.ResourceKey{Kind: "plugins", ID: "plugins"}) ||
		!slices.Equal(batch.RequiredDomains, []generation.Domain{
			generation.DomainHTTP, generation.DomainStream,
		}) {
		t.Fatalf("plugins batch = %+v", batch)
	}
}

func TestDesiredBatchFromEtcdWatchUsesLastEventRevisionNotDelayedHeader(t *testing.T) {
	watchPut := func(modRevision int64, id string) clientv3.WatchResponse {
		return clientv3.WatchResponse{
			Header: etcdserverpb.ResponseHeader{ClusterId: 0xabc, Revision: 100},
			Events: []*clientv3.Event{{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key: []byte("/apisix/routes/" + id), Value: []byte(`{"id":"` + id + `"}`),
					ModRevision: modRevision,
				},
			}},
		}
	}
	first, err := desiredBatchFromEtcdWatch("/apisix", watchPut(91, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := desiredBatchFromEtcdWatch("/apisix/", watchPut(95, "r2"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Cursor.Revision != "91" || second.Cursor.Revision != "95" {
		t.Fatalf("cursors = %q/%q, want 91/95", first.Cursor.Revision, second.Cursor.Revision)
	}
	if first.Cursor.Provider != second.Cursor.Provider {
		t.Fatalf("canonical prefix changed provider: %q != %q", first.Cursor.Provider, second.Cursor.Provider)
	}
}

func TestDesiredBatchFromEtcdBindsClusterPrefixAndTranslationVersion(t *testing.T) {
	base := etcdProviderID(0xabc, "/apisix-a/")
	if !strings.HasPrefix(base, "etcd/v1/0000000000000abc/") {
		t.Fatalf("provider = %q, want versioned zero-padded cluster identity", base)
	}
	if base != etcdProviderID(0xabc, "//apisix-a//") {
		t.Fatal("canonical-equivalent prefixes produced different providers")
	}
	if base == etcdProviderID(0xdef, "/apisix-a/") || base == etcdProviderID(0xabc, "/apisix-b/") {
		t.Fatal("etcd provider authority identity collision")
	}
}

func TestDesiredBatchFromEtcdWatchNormalizesConservativeDomains(t *testing.T) {
	batch, err := desiredBatchFromEtcdWatch("/apisix", clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 12},
		Events: []*clientv3.Event{
			{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte("/apisix/stream_routes/s1"), ModRevision: 10}},
			{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte("/apisix/routes/r1"), ModRevision: 11}},
			{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte("/apisix/upstreams/u1"), ModRevision: 12}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(batch.RequiredDomains, []generation.Domain{
		generation.DomainHTTP, generation.DomainStream,
	}) {
		t.Fatalf("required domains = %v", batch.RequiredDomains)
	}
}

func TestDesiredBatchFromEtcdWatchAdvancesPastUnmanagedEvents(t *testing.T) {
	batch, err := desiredBatchFromEtcdWatch("/apisix", clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 30},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key: []byte("/apisix/data_plane/server_info/node-1"), ModRevision: 29,
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Cursor.Revision != "29" || len(batch.Mutations) != 0 || len(batch.RequiredDomains) != 0 {
		t.Fatalf("batch = %+v, want cursor-only consumed boundary", batch)
	}
}

func TestDesiredBatchFromEtcdWatchAcceptsOnlyProgressForEmptyCursor(t *testing.T) {
	progress, err := desiredBatchFromEtcdWatch("/apisix", clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 15},
	})
	if err != nil {
		t.Fatalf("progress translation: %v", err)
	}
	if progress.Cursor.Revision != "15" || len(progress.Mutations) != 0 ||
		len(progress.RequiredDomains) != 0 || progress.ReplaceManaged {
		t.Fatalf("progress batch = %+v", progress)
	}

	for name, response := range map[string]clientv3.WatchResponse{
		"created":       {Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 15}, Created: true},
		"zero revision": {Header: etcdserverpb.ResponseHeader{ClusterId: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := desiredBatchFromEtcdWatch("/apisix", response); err == nil {
				t.Fatal("error = nil, want empty non-progress rejection")
			}
		})
	}
}

func TestDesiredBatchFromEtcdWatchRejectsInvalidEventBoundaries(t *testing.T) {
	validEvent := func(revision int64) *clientv3.Event {
		return &clientv3.Event{
			Type: mvccpb.PUT,
			Kv:   &mvccpb.KeyValue{Key: []byte("/apisix/routes/r1"), ModRevision: revision},
		}
	}
	tests := map[string]clientv3.WatchResponse{
		"missing cluster": {
			Header: etcdserverpb.ResponseHeader{Revision: 1}, Events: []*clientv3.Event{validEvent(1)},
		},
		"nil event": {
			Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 1}, Events: []*clientv3.Event{nil},
		},
		"nil key value": {
			Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 1}, Events: []*clientv3.Event{{}},
		},
		"zero event revision": {
			Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 1}, Events: []*clientv3.Event{validEvent(0)},
		},
		"decreasing event revision": {
			Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 5},
			Events: []*clientv3.Event{validEvent(5), validEvent(4)},
		},
		"header behind event": {
			Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 4}, Events: []*clientv3.Event{validEvent(5)},
		},
		"unknown managed event type": {
			Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 5},
			Events: []*clientv3.Event{{Type: mvccpb.Event_EventType(99), Kv: validEvent(5).Kv}},
		},
		"created event batch": {
			Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 5},
			Events: []*clientv3.Event{validEvent(5)}, Created: true,
		},
		"canceled event batch": {
			Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 5},
			Events: []*clientv3.Event{validEvent(5)}, Canceled: true,
		},
		"compacted event batch": {
			Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 5},
			Events: []*clientv3.Event{validEvent(5)}, CompactRevision: 4,
		},
	}
	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := desiredBatchFromEtcdWatch("/apisix", response); err == nil {
				t.Fatal("error = nil, want invalid boundary rejection")
			}
		})
	}
}

func TestDesiredBatchFromEtcdSnapshotRejectsInvalidEnvelope(t *testing.T) {
	for name, response := range map[string]*clientv3.GetResponse{
		"nil response":    nil,
		"missing header":  {},
		"missing cluster": {Header: &etcdserverpb.ResponseHeader{Revision: 1}},
		"zero revision":   {Header: &etcdserverpb.ResponseHeader{ClusterId: 1}},
		"nil key value": {
			Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 1}, Kvs: []*mvccpb.KeyValue{nil},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := desiredBatchFromEtcdSnapshot("/apisix", response); err == nil {
				t.Fatal("error = nil, want invalid snapshot rejection")
			}
		})
	}
}

func TestDesiredBatchFromEtcdWatchCommitsDelayedHeadersInEventOrder(t *testing.T) {
	journal, err := store.OpenJournal(t.TempDir()+"/journal.db", store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	engine := &etcdCoordinatorTestEngine{}
	coordinator := generation.NewCoordinator(journal, engine)

	first, err := desiredBatchFromEtcdWatch("/apisix", etcdWatchPut(0xabc, 100, 91, "/apisix/routes/r1"))
	if err != nil {
		t.Fatal(err)
	}
	firstAck, err := coordinator.Apply(context.Background(), first)
	if err != nil {
		t.Fatalf("apply first delayed response: %v", err)
	}
	second, err := desiredBatchFromEtcdWatch("/apisix", etcdWatchPut(0xabc, 100, 95, "/apisix/routes/r2"))
	if err != nil {
		t.Fatal(err)
	}
	secondAck, err := coordinator.Apply(context.Background(), second)
	if err != nil {
		t.Fatalf("apply second delayed response: %v", err)
	}

	if firstAck.Revisions != (generation.RevisionSet{Desired: 1, HTTP: 1}) ||
		secondAck.Revisions != (generation.RevisionSet{Desired: 2, HTTP: 2}) {
		t.Fatalf("ack revisions = %+v then %+v", firstAck.Revisions, secondAck.Revisions)
	}
	if engine.prepareCalls != 2 || engine.activateCalls != 2 || engine.finalizeCalls != 2 ||
		engine.confirmCalls != 0 {
		t.Fatalf("engine calls prepare/activate/finalize/confirm = %d/%d/%d/%d",
			engine.prepareCalls, engine.activateCalls, engine.finalizeCalls, engine.confirmCalls)
	}
}

func TestDesiredBatchFromEtcdProviderAuthorityRequiresSnapshotTransfer(t *testing.T) {
	journal, err := store.OpenJournal(t.TempDir()+"/journal.db", store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	engine := &etcdCoordinatorTestEngine{}
	coordinator := generation.NewCoordinator(journal, engine)

	clusterA, err := desiredBatchFromEtcdWatch(
		"/apisix-a", etcdWatchPut(0xaaa, 71, 71, "/apisix-a/routes/r1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Apply(context.Background(), clusterA); err != nil {
		t.Fatal(err)
	}

	clusterB, err := desiredBatchFromEtcdWatch(
		"/apisix-a", etcdWatchPut(0xbbb, 71, 71, "/apisix-a/routes/r1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Apply(context.Background(), clusterB); !errors.Is(err, generation.ErrProviderConflict) {
		t.Fatalf("cluster B incremental apply error = %v, want ErrProviderConflict", err)
	}

	prefixB, err := desiredBatchFromEtcdWatch(
		"/apisix-b", etcdWatchPut(0xaaa, 71, 71, "/apisix-b/routes/r1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Apply(context.Background(), prefixB); !errors.Is(err, generation.ErrProviderConflict) {
		t.Fatalf("prefix B incremental apply error = %v, want ErrProviderConflict", err)
	}
	for name, cursor := range map[string]generation.ProviderCursor{
		"cluster B": clusterB.Cursor,
		"prefix B":  prefixB.Cursor,
	} {
		if _, err := journal.LoadAcknowledgement(
			context.Background(),
			cursor,
		); !errors.Is(
			err,
			generation.ErrNotFound,
		) {
			t.Fatalf("%s acknowledgement error = %v, want ErrNotFound", name, err)
		}
	}
	if revisions, err := journal.Revisions(context.Background()); err != nil ||
		revisions != (generation.RevisionSet{Desired: 1, HTTP: 1}) {
		t.Fatalf("revisions after rejected incrementals = %+v, %v", revisions, err)
	}

	transfer, err := desiredBatchFromEtcdSnapshot("/apisix-a", &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 0xbbb, Revision: 71},
		Kvs: []*mvccpb.KeyValue{{
			Key: []byte("/apisix-a/routes/r1"), Value: []byte(`{"id":"r1"}`), ModRevision: 71,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := coordinator.Apply(context.Background(), transfer)
	if err != nil {
		t.Fatalf("full snapshot authority transfer: %v", err)
	}
	if ack.Cursor.Provider != etcdProviderID(0xbbb, "/apisix-a") ||
		ack.Revisions != (generation.RevisionSet{Desired: 2, HTTP: 2, Stream: 2}) {
		t.Fatalf("transfer acknowledgement = %+v", ack)
	}
	if engine.prepareCalls != 2 {
		t.Fatalf("prepare calls = %d, want initial apply plus full transfer only", engine.prepareCalls)
	}
}

func TestDesiredBatchFromEtcdRestartSnapshotReplaysCommittedWatchCursor(t *testing.T) {
	path := t.TempDir() + "/journal.db"
	journal, err := store.OpenJournal(path, store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	watchBatch, err := desiredBatchFromEtcdWatch(
		"/apisix", etcdWatchPut(0xabc, 100, 91, "/apisix/routes/r1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	initialEngine := &etcdCoordinatorTestEngine{}
	committed, err := generation.NewCoordinator(journal, initialEngine).Apply(context.Background(), watchBatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenJournal(path, store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	fullSnapshot, err := desiredBatchFromEtcdSnapshot("/apisix/", &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 0xabc, Revision: 91},
		Kvs: []*mvccpb.KeyValue{{
			Key: []byte("/apisix/routes/r1"), Value: []byte(`{"id":"r1"}`), ModRevision: 91,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fullSnapshot.ReplaceManaged || fullSnapshot.Cursor != watchBatch.Cursor {
		t.Fatalf("restart representations do not share cursor: watch=%+v snapshot=%+v", watchBatch, fullSnapshot)
	}
	replayEngine := &etcdCoordinatorTestEngine{}
	replayed, err := generation.NewCoordinator(reopened, replayEngine).Apply(context.Background(), fullSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Cursor != committed.Cursor || replayed.Revisions != committed.Revisions {
		t.Fatalf("replayed acknowledgement = %+v, want %+v", replayed, committed)
	}
	if replayEngine.prepareCalls != 0 || replayEngine.activateCalls != 0 || replayEngine.finalizeCalls != 0 ||
		replayEngine.confirmCalls != 1 {
		t.Fatalf("replay engine calls prepare/activate/finalize/confirm = %d/%d/%d/%d",
			replayEngine.prepareCalls, replayEngine.activateCalls,
			replayEngine.finalizeCalls, replayEngine.confirmCalls)
	}
	if replayEngine.confirmed.DesiredRevision != 1 ||
		len(replayEngine.confirmed.Domains) != 1 ||
		replayEngine.confirmed.Domains[generation.DomainHTTP].Artifact.Revision != 1 {
		t.Fatalf("confirmed publication set = %+v", replayEngine.confirmed)
	}
}

type etcdCoordinatorTestEngine struct {
	prepareCalls  int
	activateCalls int
	finalizeCalls int
	confirmCalls  int
	confirmed     generation.PublicationSet
}

func (e *etcdCoordinatorTestEngine) Prepare(
	_ context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	_ map[generation.Domain]generation.PublishedGeneration,
) (generation.PublicationSet, error) {
	e.prepareCalls++
	closure := make([]generation.ResourceKey, 0, len(desired.Resources())+len(desired.Tombstones()))
	decisions := make([]generation.ResourceDecision, 0, cap(closure))
	for _, resource := range desired.Resources() {
		closure = append(closure, resource.Key)
		decisions = append(decisions, generation.ResourceDecision{
			Key: resource.Key, Disposition: generation.DispositionPublished, Code: "test-published",
		})
	}
	for _, tombstone := range desired.Tombstones() {
		closure = append(closure, tombstone.Key)
		decisions = append(decisions, generation.ResourceDecision{
			Key: tombstone.Key, Disposition: generation.DispositionDeleted, Code: "test-deleted",
		})
	}
	set := generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(ticket.RequiredDomains)),
	}
	for _, domain := range ticket.RequiredDomains {
		set.Domains[domain] = generation.PublicationCandidate{
			Artifact: generation.GenerationArtifact{
				Domain: domain, Revision: ticket.DesiredRevision,
				Digest: desired.Digest(), Snapshot: desired.SnapshotID(),
			},
			Snapshot:  desired.Clone(),
			Closure:   slices.Clone(closure),
			Decisions: slices.Clone(decisions),
		}
	}
	return set, nil
}

func (*etcdCoordinatorTestEngine) DiscardPrepared(context.Context, generation.PublicationSet) error {
	return nil
}

func (e *etcdCoordinatorTestEngine) Activate(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) error {
	e.activateCalls++
	return nil
}

func (*etcdCoordinatorTestEngine) RollbackActivation(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) error {
	return nil
}

func (e *etcdCoordinatorTestEngine) FinalizeActivation(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) {
	e.finalizeCalls++
}

func (e *etcdCoordinatorTestEngine) ConfirmActive(
	_ context.Context,
	set generation.PublicationSet,
) error {
	e.confirmCalls++
	e.confirmed = set
	return nil
}

func etcdWatchPut(clusterID uint64, headerRevision, modRevision int64, key string) clientv3.WatchResponse {
	id := key[strings.LastIndexByte(key, '/')+1:]
	return clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: clusterID, Revision: headerRevision},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key: []byte(key), Value: []byte(`{"id":"` + id + `"}`), ModRevision: modRevision,
			},
		}},
	}
}

func TestNewConfigClientWithOptionsAppliesRuntimeSettings(t *testing.T) {
	client, err := NewConfigClientWithOptions(
		[]string{"http://127.0.0.1:2379"},
		"",
		"",
		"/apisix",
		nil,
		ClientOptions{
			DialTimeout:    2 * time.Second,
			RequestTimeout: 3 * time.Second,
			StartupRetry:   2,
			WatchTimeout:   4 * time.Second,
			ResyncDelay:    5 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("NewConfigClientWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if client.requestTimeout != 3*time.Second {
		t.Fatalf("requestTimeout = %s, want 3s", client.requestTimeout)
	}
	if client.startupRetry != 2 {
		t.Fatalf("startupRetry = %d, want 2", client.startupRetry)
	}
	if client.watchTimeout != 4*time.Second {
		t.Fatalf("watchTimeout = %s, want 4s", client.watchTimeout)
	}
	if client.resyncDelay != 5*time.Second {
		t.Fatalf("resyncDelay = %s, want 5s", client.resyncDelay)
	}
	if client.prefix != "/apisix/" {
		t.Fatalf("prefix = %q, want canonical /apisix/", client.prefix)
	}
}

func TestNewTLSConfigHonorsVerificationAndSNI(t *testing.T) {
	verify := false
	config, err := NewTLSConfig("", "", "etcd.example.com", &verify)
	if err != nil {
		t.Fatalf("NewTLSConfig() error = %v", err)
	}
	if config.ServerName != "etcd.example.com" {
		t.Fatalf("ServerName = %q, want etcd.example.com", config.ServerName)
	}
	if !config.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify = false, want true")
	}
}

func TestWatchStartsAtRevisionAfterLastKnown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &ConfigClient{
		prefix:    "/apisix",
		events:    make(chan *store.Event, 1),
		knownKeys: make(map[string]struct{}),
	}
	var openedRevision int64
	client.openWatch = func(_ context.Context, revision int64) clientv3.WatchChan {
		openedRevision = revision
		cancel()
		stream := make(chan clientv3.WatchResponse)
		close(stream)
		return stream
	}

	client.Watch(ctx)
	if openedRevision != 1 {
		t.Fatalf("watch opened at revision %d, want 1", openedRevision)
	}
}

func TestWatchReconcilesSnapshotAfterUnexpectedClosure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix: "/apisix",
		events: events,
		knownKeys: map[string]struct{}{
			"/apisix/routes/old": {},
		},
		lastRevision: 7,
	}

	var opens atomic.Int32
	var resumedRevision int64
	client.openWatch = func(_ context.Context, revision int64) clientv3.WatchChan {
		stream := make(chan clientv3.WatchResponse)
		if opens.Add(1) == 1 {
			close(stream)
			return stream
		}
		resumedRevision = revision
		cancel()
		close(stream)
		return stream
	}
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		return &clientv3.GetResponse{
			Header: &etcdserverpb.ResponseHeader{Revision: 10},
			Kvs: []*mvccpb.KeyValue{{
				Key:         []byte("/apisix/routes/new"),
				Value:       []byte(`{"id":"new"}`),
				ModRevision: 10,
			}},
		}, nil
	}

	client.Watch(ctx)
	if resumedRevision != 11 {
		t.Fatalf("resumed revision = %d, want 11", resumedRevision)
	}
	if value, err := storage.GetFromBucket("routes", []byte("old")); err != nil || value != nil {
		t.Fatalf("old route = %q, %v; want deleted", value, err)
	}
	if value, err := storage.GetFromBucket("routes", []byte("new")); err != nil || string(value) != `{"id":"new"}` {
		t.Fatalf("new route = %q, %v; want replacement", value, err)
	}
}

func TestWatchStopsWhileSnapshotRecoveryIsFailing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &ConfigClient{
		prefix:    "/apisix",
		events:    make(chan *store.Event, 1),
		knownKeys: make(map[string]struct{}),
	}
	client.openWatch = func(context.Context, int64) clientv3.WatchChan {
		stream := make(chan clientv3.WatchResponse)
		close(stream)
		return stream
	}
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		cancel()
		return nil, errors.New("etcd unavailable")
	}

	done := make(chan struct{})
	go func() {
		client.Watch(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch did not stop after context cancellation")
	}
}

func TestRecoverSnapshotUsesFreshApplyTimeout(t *testing.T) {
	storage, events := newWatcherStore(t)
	var delayed atomic.Bool
	storage.AddAcknowledgedEventUpdateHook(func(*store.Event) error {
		if delayed.CompareAndSwap(false, true) {
			time.Sleep(300 * time.Millisecond)
		}
		return nil
	})
	client := &ConfigClient{
		prefix:         canonicalEtcdPrefix("/apisix"),
		events:         events,
		knownKeys:      make(map[string]struct{}),
		requestTimeout: 500 * time.Millisecond,
		loadSnapshot: func(context.Context) (*clientv3.GetResponse, error) {
			time.Sleep(300 * time.Millisecond)
			return &clientv3.GetResponse{
				Header: &etcdserverpb.ResponseHeader{Revision: 2},
				Kvs: []*mvccpb.KeyValue{{
					Key:         []byte("/apisix/routes/route"),
					Value:       []byte(`{"id":"route"}`),
					ModRevision: 2,
				}},
			}, nil
		},
	}
	if err := client.recoverSnapshot(context.Background()); err != nil {
		t.Fatalf("recoverSnapshot() error = %v, want independent load and apply budgets", err)
	}
}

func TestWatchReconcilesSnapshotAfterCompaction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:       "/apisix",
		events:       events,
		knownKeys:    map[string]struct{}{"/apisix/routes/stale": {}},
		lastRevision: 7,
	}

	var opens atomic.Int32
	var resumedRevision int64
	client.openWatch = func(_ context.Context, revision int64) clientv3.WatchChan {
		stream := make(chan clientv3.WatchResponse, 1)
		if opens.Add(1) == 1 {
			stream <- clientv3.WatchResponse{CompactRevision: 8}
			close(stream)
			return stream
		}
		resumedRevision = revision
		cancel()
		close(stream)
		return stream
	}
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		return &clientv3.GetResponse{
			Header: &etcdserverpb.ResponseHeader{Revision: 10},
			Kvs: []*mvccpb.KeyValue{{
				Key:         []byte("/apisix/routes/replacement"),
				Value:       []byte(`{"id":"replacement"}`),
				ModRevision: 10,
			}},
		}, nil
	}

	client.Watch(ctx)
	if resumedRevision != 11 {
		t.Fatalf("resumed revision = %d, want 11", resumedRevision)
	}
	if value, err := storage.GetFromBucket("routes", []byte("stale")); err != nil || value != nil {
		t.Fatalf("stale route = %q, %v; want deleted", value, err)
	}
	replacement, err := storage.GetFromBucket("routes", []byte("replacement"))
	if err != nil || string(replacement) != `{"id":"replacement"}` {
		t.Fatalf("replacement route = %q, %v; want replacement", replacement, err)
	}
}

func TestApplyWatchResponseMutatesKnownKeysAndRevision(t *testing.T) {
	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:       "/apisix",
		events:       events,
		knownKeys:    map[string]struct{}{},
		lastRevision: 10,
	}
	response := clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 14},
		Events: []*clientv3.Event{
			{
				Type: mvccpb.PUT,
				Kv:   &mvccpb.KeyValue{Key: []byte("/apisix/routes/a"), Value: []byte(`{"id":"a"}`), ModRevision: 12},
			},
			{Type: mvccpb.DELETE, Kv: &mvccpb.KeyValue{Key: []byte("/apisix/routes/b"), ModRevision: 13}},
		},
	}

	if err := client.applyWatchResponse(context.Background(), response); err != nil {
		t.Fatalf("applyWatchResponse() error = %v", err)
	}
	if value, err := storage.GetFromBucket("routes", []byte("a")); err != nil || string(value) != `{"id":"a"}` {
		t.Fatalf("route a = %q, %v; want put", value, err)
	}
	if value, err := storage.GetFromBucket("routes", []byte("b")); err != nil || value != nil {
		t.Fatalf("route b = %q, %v; want delete", value, err)
	}
	if _, ok := client.knownKeys["/apisix/routes/a"]; !ok {
		t.Fatal("knownKeys does not retain the put key")
	}
	if _, ok := client.knownKeys["/apisix/routes/b"]; ok {
		t.Fatal("knownKeys still contains the deleted key")
	}
	if client.lastRevision != 14 {
		t.Fatalf("lastRevision = %d, want header revision 14", client.lastRevision)
	}
}

func TestApplyWatchResponseQuarantinesInvalidResourceAndAdvances(t *testing.T) {
	oldFailures, oldReady, oldQuarantine := metrics.ConfigApplyFailures, metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_quarantine_failures_total"})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_quarantine_ready"})
	metrics.ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_quarantine_count"})
	t.Cleanup(func() {
		metrics.ConfigApplyFailures, metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined = oldFailures, oldReady, oldQuarantine
	})

	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:    "/apisix",
		events:    events,
		knownKeys: map[string]struct{}{},
	}
	err := client.applyWatchResponse(context.Background(), clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 14},
		Events: []*clientv3.Event{
			{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte("/apisix/ssls/bad"),
					Value:       []byte(`{"id":"bad","cert":"bad","key":"bad","status":1}`),
					ModRevision: 12,
				},
			},
			{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte("/apisix/routes/good"),
					Value:       []byte(`{"id":"good"}`),
					ModRevision: 13,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("applyWatchResponse() error = %v, want invalid resource quarantined", err)
	}
	if client.lastRevision != 14 {
		t.Fatalf("lastRevision = %d, want header revision 14", client.lastRevision)
	}
	if _, ok := client.knownKeys["/apisix/ssls/bad"]; !ok {
		t.Fatal("knownKeys does not retain the current invalid etcd key")
	}
	if _, ok := client.knownKeys["/apisix/routes/good"]; !ok {
		t.Fatal("knownKeys does not retain the unrelated valid key")
	}
	if got := client.quarantine["/apisix/ssls/bad"]; got != 12 {
		t.Fatalf("quarantine revision = %d, want 12", got)
	}
	if got := watcherConfigApplyGaugeValue(t, metrics.ConfigApplyQuarantined); got != 1 {
		t.Fatalf("quarantine gauge = %v, want 1", got)
	}
	if value, err := storage.GetFromBucket("routes", []byte("good")); err != nil || string(value) != `{"id":"good"}` {
		t.Fatalf("durable unrelated route = %q, %v; want stored", value, err)
	}
	if value, err := storage.GetFromBucket("ssls", []byte("bad")); err != nil || value != nil {
		t.Fatalf("durable invalid SSL = %q, %v; want absent", value, err)
	}

	if err := client.applyWatchResponse(context.Background(), clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 15},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte("/apisix/ssls/bad"),
				Value:       []byte(`{"id":"bad","status":0}`),
				ModRevision: 15,
			},
		}},
	}); err != nil {
		t.Fatalf("applyWatchResponse(valid replacement) error = %v", err)
	}
	if len(client.quarantine) != 0 {
		t.Fatalf("quarantine after valid replacement = %v, want empty", client.quarantine)
	}

	if err := client.applyWatchResponse(context.Background(), clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 16},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte("/apisix/ssls/bad"),
				Value:       []byte(`{"id":"bad","cert":"bad","key":"bad","status":1}`),
				ModRevision: 16,
			},
		}},
	}); err != nil {
		t.Fatalf("applyWatchResponse(second invalid) error = %v", err)
	}
	if err := client.applyWatchResponse(context.Background(), clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 17},
		Events: []*clientv3.Event{{
			Type: mvccpb.DELETE,
			Kv:   &mvccpb.KeyValue{Key: []byte("/apisix/ssls/bad"), ModRevision: 17},
		}},
	}); err != nil {
		t.Fatalf("applyWatchResponse(delete) error = %v", err)
	}
	if len(client.quarantine) != 0 {
		t.Fatalf("quarantine after delete = %v, want empty", client.quarantine)
	}
}

func TestApplySnapshotQuarantineClearsOnReplacementAndDelete(t *testing.T) {
	oldFailures, oldReady, oldQuarantine := metrics.ConfigApplyFailures, metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined
	metrics.ConfigApplyFailures = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "test_snapshot_quarantine_failures_total"},
	)
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_snapshot_quarantine_ready"})
	metrics.ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_snapshot_quarantine_count"})
	t.Cleanup(func() {
		metrics.ConfigApplyFailures, metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined = oldFailures, oldReady, oldQuarantine
	})

	_, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:    "/apisix",
		events:    events,
		knownKeys: map[string]struct{}{},
	}
	invalid := &mvccpb.KeyValue{
		Key:         []byte("/apisix/consumers/alice"),
		Value:       []byte(`{"username":"alice","plugins":{"basic-auth":{"username":"alice"}}}`),
		ModRevision: 2,
	}
	if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{Revision: 2},
		Kvs:    []*mvccpb.KeyValue{invalid},
	}); err != nil {
		t.Fatalf("applySnapshot(invalid) error = %v, want quarantine", err)
	}
	if got := client.quarantine["/apisix/consumers/alice"]; got != 2 {
		t.Fatalf("snapshot quarantine revision = %d, want 2", got)
	}
	if got := watcherConfigApplyGaugeValue(t, metrics.ConfigApplyQuarantined); got != 1 {
		t.Fatalf("snapshot quarantine gauge = %v, want 1", got)
	}

	valid := &mvccpb.KeyValue{
		Key:         invalid.Key,
		Value:       []byte(`{"username":"alice","plugins":{}}`),
		ModRevision: 3,
	}
	if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{Revision: 3},
		Kvs:    []*mvccpb.KeyValue{valid},
	}); err != nil {
		t.Fatalf("applySnapshot(valid replacement) error = %v", err)
	}
	if len(client.quarantine) != 0 {
		t.Fatalf("quarantine after valid replacement = %v, want empty", client.quarantine)
	}
	if got := watcherConfigApplyGaugeValue(t, metrics.ConfigApplyQuarantined); got != 0 {
		t.Fatalf("quarantine gauge after replacement = %v, want 0", got)
	}

	if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{Revision: 4},
	}); err != nil {
		t.Fatalf("applySnapshot(delete) error = %v", err)
	}
	if _, ok := client.quarantine["/apisix/consumers/alice"]; ok {
		t.Fatal("delete did not clear resource quarantine")
	}
}

func TestApplyWatchResponseDoesNotQuarantineTransientStoreFailure(t *testing.T) {
	storage, events := newWatcherStore(t)
	if err := storage.Stop(); err != nil {
		t.Fatalf("stop watcher store: %v", err)
	}
	client := &ConfigClient{
		prefix:       "/apisix",
		events:       events,
		knownKeys:    map[string]struct{}{},
		lastRevision: 10,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := client.applyWatchResponse(ctx, clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 11},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv:   &mvccpb.KeyValue{Key: []byte("/apisix/routes/good"), Value: []byte(`{"id":"good"}`), ModRevision: 11},
		}},
	})
	if err == nil {
		t.Fatal("applyWatchResponse() error = nil, want transient store failure")
	}
	var validationErr *store.ResourceValidationError
	if errors.As(err, &validationErr) {
		t.Fatalf("transient store error was quarantined as validation: %v", err)
	}
	if client.lastRevision != 10 {
		t.Fatalf("lastRevision = %d, want unchanged 10", client.lastRevision)
	}
}

func TestApplyWatchResponseStagesStateUntilEveryEventAcknowledges(t *testing.T) {
	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:       "/apisix",
		events:       events,
		knownKeys:    map[string]struct{}{"/apisix/routes/old": {}},
		lastRevision: 10,
	}
	err := client.applyWatchResponse(context.Background(), clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 14},
		Events: []*clientv3.Event{
			{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte("/apisix/routes/good"),
					Value:       []byte(`{"id":"good"}`),
					ModRevision: 12,
				},
			},
			{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte("/apisix/ssls/bad"),
					Value:       []byte(`{"id":"bad","cert":"bad","key":"bad","status":1}`),
					ModRevision: 13,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("applyWatchResponse() error = %v, want invalid resource quarantined", err)
	}
	if client.lastRevision != 14 {
		t.Fatalf("lastRevision = %d, want header revision 14", client.lastRevision)
	}
	if _, ok := client.knownKeys["/apisix/routes/good"]; !ok {
		t.Fatal("knownKeys did not commit the valid event")
	}
	if got := client.quarantine["/apisix/ssls/bad"]; got != 13 {
		t.Fatalf("quarantine revision = %d, want 13", got)
	}
	if value, err := storage.GetFromBucket("routes", []byte("good")); err != nil || string(value) != `{"id":"good"}` {
		t.Fatalf("durable first event = %q, %v; want pruned valid event", value, err)
	}
}

func TestProviderBatchFailureStaysUnreadyAfterQueuedRouteSuccess(t *testing.T) {
	oldFailures, oldReady, oldQuarantine := metrics.ConfigApplyFailures, metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_provider_batch_failure_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_provider_batch_failure_ready",
	})
	metrics.ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_provider_batch_failure_quarantine",
	})
	t.Cleanup(func() {
		metrics.ConfigApplyFailures, metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined = oldFailures, oldReady, oldQuarantine
	})

	_, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:       "/apisix",
		events:       events,
		knownKeys:    map[string]struct{}{},
		lastRevision: 10,
	}
	err := client.applyWatchResponse(context.Background(), clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 14},
		Events: []*clientv3.Event{
			{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte("/apisix/routes/good"),
					Value:       []byte(`{"id":"good"}`),
					ModRevision: 12,
				},
			},
			{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte("/apisix/ssls/bad"),
					Value:       []byte(`{"id":"bad","cert":"bad","key":"bad","status":1}`),
					ModRevision: 13,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("applyWatchResponse() error = %v, want provider progress with quarantine", err)
	}

	// The route event was already durably acknowledged and can publish while
	// the invalid resource keeps provider readiness degraded.
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	metrics.RecordConfigApplyQuarantine(1)
	if got := watcherConfigApplyReadyValue(t, metrics.ConfigApplyReady); got != 0 {
		t.Fatalf("ready after delayed route success = %v, want 0", got)
	}
}

func TestApplySnapshotStagesStateUntilEveryEventAcknowledges(t *testing.T) {
	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:       "/apisix",
		events:       events,
		knownKeys:    map[string]struct{}{"/apisix/routes/old": {}},
		lastRevision: 10,
	}
	err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{Revision: 14},
		Kvs: []*mvccpb.KeyValue{
			{
				Key:         []byte("/apisix/routes/good"),
				Value:       []byte(`{"id":"good"}`),
				ModRevision: 12,
			},
			{
				Key:         []byte("/apisix/ssls/bad"),
				Value:       []byte(`{"id":"bad","cert":"bad","key":"bad","status":1}`),
				ModRevision: 13,
			},
		},
	})
	if err != nil {
		t.Fatalf("applySnapshot() error = %v, want invalid resource quarantined", err)
	}
	if client.lastRevision != 14 {
		t.Fatalf("lastRevision = %d, want snapshot revision 14", client.lastRevision)
	}
	if len(client.knownKeys) != 2 {
		t.Fatalf("knownKeys = %v, want current snapshot keys", client.knownKeys)
	}
	if _, ok := client.quarantine["/apisix/ssls/bad"]; !ok {
		t.Fatal("snapshot did not retain invalid resource quarantine")
	}
	if value, err := storage.GetFromBucket("routes", []byte("good")); err != nil || string(value) != `{"id":"good"}` {
		t.Fatalf("durable first snapshot event = %q, %v; want pruned valid event", value, err)
	}
}

func TestWatchRecoversSnapshotAfterApplyFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:         "/apisix",
		events:         events,
		knownKeys:      map[string]struct{}{},
		lastRevision:   4,
		requestTimeout: time.Second,
	}
	var opens atomic.Int32
	var resumed int64
	client.openWatch = func(_ context.Context, revision int64) clientv3.WatchChan {
		stream := make(chan clientv3.WatchResponse, 1)
		if opens.Add(1) == 1 {
			stream <- clientv3.WatchResponse{
				Header: etcdserverpb.ResponseHeader{Revision: 6},
				Events: []*clientv3.Event{
					{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte("/apisix/routes/good"), Value: []byte(`{"id":"good"}`), ModRevision: 5}},
					{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte("/apisix/ssls/bad"), Value: []byte(`{"id":"bad","cert":"bad","key":"bad","status":1}`), ModRevision: 6}},
				},
			}
			close(stream)
			return stream
		}
		resumed = revision
		cancel()
		close(stream)
		return stream
	}
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		return &clientv3.GetResponse{
			Header: &etcdserverpb.ResponseHeader{Revision: 8},
			Kvs: []*mvccpb.KeyValue{{
				Key:         []byte("/apisix/routes/good"),
				Value:       []byte(`{"id":"recovered"}`),
				ModRevision: 8,
			}},
		}, nil
	}

	client.Watch(ctx)
	if resumed != 9 {
		t.Fatalf("recovered watch revision = %d, want 9", resumed)
	}
	recovered, err := storage.GetFromBucket("routes", []byte("good"))
	if err != nil || string(recovered) != `{"id":"recovered"}` {
		t.Fatalf("recovered route = %q, %v", recovered, err)
	}
}

func TestFetchAllRetriesThenAppliesSnapshot(t *testing.T) {
	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:         "/apisix",
		events:         events,
		knownKeys:      map[string]struct{}{"/apisix/routes/stale": {}},
		lastRevision:   10,
		requestTimeout: time.Second,
		startupRetry:   1,
	}
	var loads atomic.Int32
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		if loads.Add(1) == 1 {
			return nil, errors.New("unavailable")
		}
		return &clientv3.GetResponse{
			Header: &etcdserverpb.ResponseHeader{Revision: 20},
			Kvs: []*mvccpb.KeyValue{{
				Key:         []byte("/apisix/routes/fresh"),
				Value:       []byte(`{"id":"fresh"}`),
				ModRevision: 20,
			}},
		}, nil
	}

	if err := client.FetchAll(); err != nil {
		t.Fatalf("FetchAll() error = %v", err)
	}
	if loads.Load() != 2 {
		t.Fatalf("snapshot loads = %d, want retry on first failure", loads.Load())
	}
	if value, err := storage.GetFromBucket("routes", []byte("stale")); err != nil || value != nil {
		t.Fatalf("stale route = %q, %v; want delete", value, err)
	}
	if value, err := storage.GetFromBucket("routes", []byte("fresh")); err != nil || string(value) != `{"id":"fresh"}` {
		t.Fatalf("fresh route = %q, %v; want put", value, err)
	}
	if client.lastRevision != 20 {
		t.Fatalf("lastRevision = %d, want 20", client.lastRevision)
	}
}

func TestFetchAllWithoutRetryPropagatesSnapshotError(t *testing.T) {
	client := &ConfigClient{
		prefix:         "/apisix",
		events:         make(chan *store.Event, 1),
		knownKeys:      map[string]struct{}{},
		requestTimeout: time.Second,
		startupRetry:   0,
	}
	var loads atomic.Int32
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		loads.Add(1)
		return nil, errors.New("unavailable")
	}

	if err := client.FetchAll(); err == nil || err.Error() != "unavailable" {
		t.Fatalf("FetchAll() error = %v, want unavailable", err)
	}
	if loads.Load() != 1 {
		t.Fatalf("snapshot loads = %d, want exactly one without retry", loads.Load())
	}
}

func TestNewConfigClientDelegatesToWithOptions(t *testing.T) {
	client, err := NewConfigClient([]string{"http://127.0.0.1:2379"}, "", "", "/apisix", nil)
	if err != nil {
		t.Fatalf("NewConfigClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if client.prefix != "/apisix/" || client.startupRetry != 0 {
		t.Fatalf("prefix/startupRetry = %q/%d, want /apisix//0", client.prefix, client.startupRetry)
	}
	if client.openWatch == nil || client.loadSnapshot == nil {
		t.Fatal("NewConfigClient() did not install watch and snapshot hooks")
	}
}

func TestNewConfigClientWithOptionsAppliesDefaults(t *testing.T) {
	client, err := NewConfigClientWithOptions(
		[]string{"http://127.0.0.1:2379"},
		"",
		"",
		"/apisix",
		nil,
		ClientOptions{},
	)
	if err != nil {
		t.Fatalf("NewConfigClientWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if client.requestTimeout != 5*time.Second {
		t.Fatalf("requestTimeout = %s, want default 5s", client.requestTimeout)
	}
}

func TestNewConfigClientHealthCheckDefaultsAndInjection(t *testing.T) {
	defaultClient, err := NewConfigClientWithOptions(
		[]string{"http://127.0.0.1:2379"}, "", "", "/apisix", nil, ClientOptions{},
	)
	if err != nil {
		t.Fatalf("NewConfigClientWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = defaultClient.Close() })
	if defaultClient.healthCheckInterval != 10*time.Second {
		t.Fatalf("healthCheckInterval = %s, want default 10s", defaultClient.healthCheckInterval)
	}
	if defaultClient.healthCheck == nil {
		t.Fatal("healthCheck = nil, want production probe")
	}

	probe := func(context.Context) error { return nil }
	configuredClient, err := NewConfigClientWithOptions(
		[]string{"http://127.0.0.1:2379"}, "", "", "/apisix", nil,
		ClientOptions{HealthCheck: probe, HealthCheckInterval: 25 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("NewConfigClientWithOptions() with health check error = %v", err)
	}
	t.Cleanup(func() { _ = configuredClient.Close() })
	if configuredClient.healthCheckInterval != 25*time.Millisecond {
		t.Fatalf("healthCheckInterval = %s, want 25ms", configuredClient.healthCheckInterval)
	}
	if configuredClient.healthCheck == nil {
		t.Fatal("healthCheck = nil after injection")
	}
}

func TestProductionHealthCheckUsesSingleKeyGet(t *testing.T) {
	var gotKey string
	var gotOptions int
	check := newHealthCheck(
		func(_ context.Context, key string, options ...clientv3.OpOption) (*clientv3.GetResponse, error) {
			gotKey = key
			gotOptions = len(options)
			return &clientv3.GetResponse{}, nil
		},
		"/apisix",
	)

	if err := check(context.Background()); err != nil {
		t.Fatalf("health check error = %v", err)
	}
	if gotKey != "/apisix" {
		t.Fatalf("health check key = %q, want /apisix", gotKey)
	}
	if gotOptions != 0 {
		t.Fatalf("health check options = %d, want no range options", gotOptions)
	}
}

func TestFetchAllApplySnapshotBoundedWhenEventsUnconsumed(t *testing.T) {
	client := &ConfigClient{
		prefix:         "/apisix",
		events:         make(chan *store.Event),
		knownKeys:      map[string]struct{}{},
		requestTimeout: 50 * time.Millisecond,
		startupRetry:   0,
	}
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		return &clientv3.GetResponse{
			Header: &etcdserverpb.ResponseHeader{Revision: 1},
			Kvs: []*mvccpb.KeyValue{{
				Key:   []byte("/apisix/routes/route-1"),
				Value: []byte(`{"id":"route-1"}`),
			}},
		}, nil
	}

	start := time.Now()
	err := client.FetchAll()
	if err == nil {
		t.Fatal("FetchAll() error = nil with an unconsumed events channel")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("FetchAll() took %s, want the snapshot bounded by requestTimeout", elapsed)
	}
}

func TestWatchRetryDelayBounded(t *testing.T) {
	if got := watchRetryDelay(0); got != 100*time.Millisecond {
		t.Fatalf("watchRetryDelay(0) = %s, want 100ms", got)
	}
	if got := watchRetryDelay(6); got != 5*time.Second {
		t.Fatalf("watchRetryDelay(6) = %s, want capped 5s", got)
	}
}

func TestApplySnapshotRejectsMissingHeader(t *testing.T) {
	client := &ConfigClient{events: make(chan *store.Event, 1)}
	if err := client.applySnapshot(context.Background(), nil); err == nil {
		t.Fatal("applySnapshot(nil) error = nil, want missing header rejection")
	}
}

func TestSendEventHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &ConfigClient{events: make(chan *store.Event)}
	if err := client.sendEvent(ctx, store.EventTypePut, []byte("k"), []byte("v")); !errors.Is(err, context.Canceled) {
		t.Fatalf("sendEvent() error = %v, want context.Canceled", err)
	}
}

func TestCanonicalEtcdPrefixAndManagedKeyShapes(t *testing.T) {
	tests := []struct {
		prefix string
		key    string
		want   bool
	}{
		{prefix: "/apisix", key: "/apisix/routes/route-1", want: true},
		{prefix: "/apisix", key: "/apisix2/routes/route-1", want: false},
		{prefix: "/apisix", key: "/apisix/data_plane/server_info/node-1", want: false},
		{prefix: "/apisix", key: "/apisix/unknown/item", want: false},
		{prefix: "/apisix", key: "/apisix/routes", want: false},
		{prefix: "/apisix", key: "/apisix/routes/", want: false},
		{prefix: "/apisix", key: "/apisix/routes/route-1/extra", want: false},
		{prefix: "/apisix", key: "/apisix/secrets/vault/item", want: true},
		{prefix: "/apisix", key: "/apisix/secrets/item", want: false},
		{prefix: "/apisix", key: "/apisix/plugins", want: true},
	}
	for _, test := range tests {
		client := &ConfigClient{prefix: canonicalEtcdPrefix(test.prefix)}
		if _, _, got := client.managedKey([]byte(test.key)); got != test.want {
			t.Errorf("managedKey(%q, %q) = %v, want %v", test.prefix, test.key, got, test.want)
		}
	}
}

func TestManagedKeySuccessfulShapesUseCanonicalMembership(t *testing.T) {
	client := &ConfigClient{prefix: canonicalEtcdPrefix("/apisix")}
	for _, key := range []string{
		"/apisix/routes/route-1",
		"/apisix/plugins",
		"/apisix/secrets/vault/item",
	} {
		bucket, _, ok := client.managedKey([]byte(key))
		if !ok {
			t.Fatalf("managedKey(%q) = not managed, want managed", key)
		}
		if !generation.IsManagedResourceKind(bucket) {
			t.Errorf("managedKey(%q) returned noncanonical bucket %q", key, bucket)
		}
	}
}

func TestApplySnapshotFiltersSiblingServerInfoUnknownAndCollectionKeys(t *testing.T) {
	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:    canonicalEtcdPrefix("/apisix"),
		events:    events,
		knownKeys: map[string]struct{}{},
	}
	response := &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{Revision: 20},
		Kvs: []*mvccpb.KeyValue{
			{Key: []byte("/apisix/routes/good"), Value: []byte(`{"id":"good"}`), ModRevision: 20},
			{Key: []byte("/apisix2/routes/sibling"), Value: []byte(`{"id":"sibling"}`), ModRevision: 20},
			{Key: []byte("/apisix/data_plane/server_info/node-1"), Value: []byte(`{"id":"node-1"}`), ModRevision: 20},
			{Key: []byte("/apisix/unknown/item"), Value: []byte(`{"id":"item"}`), ModRevision: 20},
			{Key: []byte("/apisix/routes"), Value: []byte(`{"id":"collection"}`), ModRevision: 20},
			{Key: []byte("/apisix/routes/good/extra"), Value: []byte(`{"id":"extra"}`), ModRevision: 20},
		},
	}
	if err := client.applySnapshot(context.Background(), response); err != nil {
		t.Fatalf("applySnapshot() error = %v", err)
	}
	if len(client.knownKeys) != 1 {
		t.Fatalf("knownKeys = %v, want only managed resource", client.knownKeys)
	}
	if _, ok := client.knownKeys["/apisix/routes/good"]; !ok {
		t.Fatalf("knownKeys = %v, want /apisix/routes/good", client.knownKeys)
	}
	if value, err := storage.GetFromBucket("routes", []byte("good")); err != nil || string(value) != `{"id":"good"}` {
		t.Fatalf("managed route = %q, %v", value, err)
	}
	for _, id := range []string{"sibling", "collection"} {
		value, err := storage.GetFromBucket("routes", []byte(id))
		if err != nil || value != nil {
			t.Fatalf("ignored route %q = %q, %v; want absent", id, value, err)
		}
	}
}

func TestApplySnapshotAuthoritativelyDeletesPersistedRowsWithEmptyKnownKeys(t *testing.T) {
	storage, events := newWatcherStore(t)
	applyWatcherMutation(t, events, store.Mutation{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/routes/stale"),
		Value: []byte(`{"id":"stale"}`),
	})
	client := &ConfigClient{
		prefix:    canonicalEtcdPrefix("/apisix"),
		events:    events,
		knownKeys: map[string]struct{}{},
	}
	if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{Revision: 21},
		Kvs: []*mvccpb.KeyValue{{
			Key:         []byte("/apisix/routes/fresh"),
			Value:       []byte(`{"id":"fresh"}`),
			ModRevision: 21,
		}},
	}); err != nil {
		t.Fatalf("applySnapshot() error = %v", err)
	}
	if value, err := storage.GetFromBucket("routes", []byte("stale")); err != nil || value != nil {
		t.Fatalf("persisted stale route = %q, %v; want deleted", value, err)
	}
	if value, err := storage.GetFromBucket("routes", []byte("fresh")); err != nil || string(value) != `{"id":"fresh"}` {
		t.Fatalf("fresh route = %q, %v", value, err)
	}
}

func TestApplySnapshotQuarantinesAndPreservesInvalidReplacementAtomically(t *testing.T) {
	storage, events := newWatcherStore(t)
	applyWatcherMutation(t, events, store.Mutation{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/ssls/keep"),
		Value: []byte(`{"id":"keep","cert":"","key":"","status":0}`),
	})
	var hookCalls atomic.Int32
	storage.AddAcknowledgedEventUpdateHook(func(*store.Event) error {
		hookCalls.Add(1)
		return nil
	})
	client := &ConfigClient{
		prefix: canonicalEtcdPrefix("/apisix"),
		events: events,
		knownKeys: map[string]struct{}{
			"/apisix/ssls/keep": {},
		},
	}
	if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{Revision: 22},
		Kvs: []*mvccpb.KeyValue{
			{
				Key:         []byte("/apisix/ssls/keep"),
				Value:       []byte(`{"id":"keep","cert":"bad","key":"bad","status":1}`),
				ModRevision: 22,
			},
			{Key: []byte("/apisix/routes/new"), Value: []byte(`{"id":"new"}`), ModRevision: 22},
		},
	}); err != nil {
		t.Fatalf("applySnapshot() error = %v, want one validation-pruned retry", err)
	}
	if got := hookCalls.Load(); got == 0 {
		t.Fatal("acknowledged batch hook was not called")
	}
	if value, err := storage.GetFromBucket("ssls", []byte("keep")); err != nil ||
		string(value) != `{"id":"keep","cert":"","key":"","status":0}` {
		t.Fatalf("preserved SSL = %q, %v", value, err)
	}
	if value, err := storage.GetFromBucket("routes", []byte("new")); err != nil || string(value) != `{"id":"new"}` {
		t.Fatalf("unrelated route = %q, %v", value, err)
	}
	if got := client.quarantine["/apisix/ssls/keep"]; got != 22 {
		t.Fatalf("quarantine revision = %d, want 22", got)
	}
}

func TestApplyWatchResponseNonValidationHookFailureDoesNotAdvanceState(t *testing.T) {
	storage, events := newWatcherStore(t)
	storage.AddAcknowledgedEventUpdateHook(func(*store.Event) error {
		return errors.New("stream publication failed")
	})
	client := &ConfigClient{
		prefix:       canonicalEtcdPrefix("/apisix"),
		events:       events,
		knownKeys:    map[string]struct{}{},
		lastRevision: 30,
	}
	err := client.applyWatchResponse(context.Background(), clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 31},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv:   &mvccpb.KeyValue{Key: []byte("/apisix/routes/new"), Value: []byte(`{"id":"new"}`), ModRevision: 31},
		}},
	})
	if err == nil || err.Error() != "stream publication failed" {
		t.Fatalf("applyWatchResponse() error = %v, want hook failure", err)
	}
	if client.lastRevision != 30 {
		t.Fatalf("lastRevision = %d, want unchanged 30", client.lastRevision)
	}
	if len(client.knownKeys) != 0 || len(client.quarantine) != 0 {
		t.Fatalf("watcher state = keys=%v quarantine=%v, want unchanged", client.knownKeys, client.quarantine)
	}
	if value, err := storage.GetFromBucket("routes", []byte("new")); err != nil || string(value) != `{"id":"new"}` {
		t.Fatalf("durable route = %q, %v; want committed before hook acknowledgement", value, err)
	}
}

func TestFetchAllContextCancellationStopsStartupRetry(t *testing.T) {
	client := &ConfigClient{
		prefix:         canonicalEtcdPrefix("/apisix"),
		events:         make(chan *store.Event),
		knownKeys:      map[string]struct{}{},
		requestTimeout: time.Second,
		startupRetry:   2,
	}
	var loads atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		loads.Add(1)
		cancel()
		return nil, errors.New("unavailable")
	}
	start := time.Now()
	err := client.FetchAllContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchAllContext() error = %v, want context.Canceled", err)
	}
	if loads.Load() != 1 {
		t.Fatalf("snapshot loads = %d, want no retry after cancellation", loads.Load())
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("FetchAllContext() took %s, want cancellation-aware retry wait", elapsed)
	}
}

func TestWatchTimeoutReopensWithoutSnapshotRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var opens atomic.Int32
	var revisions []int64
	recoveryStarted := make(chan struct{}, 1)
	client := &ConfigClient{
		prefix:       canonicalEtcdPrefix("/apisix"),
		knownKeys:    map[string]struct{}{},
		lastRevision: 40,
		watchTimeout: 10 * time.Millisecond,
		loadSnapshot: func(context.Context) (*clientv3.GetResponse, error) {
			recoveryStarted <- struct{}{}
			return nil, errors.New("unexpected snapshot recovery")
		},
	}
	client.openWatch = func(watchCtx context.Context, revision int64) clientv3.WatchChan {
		revisions = append(revisions, revision)
		stream := make(chan clientv3.WatchResponse)
		if opens.Add(1) == 2 {
			cancel()
			close(stream)
			return stream
		}
		go func() {
			<-watchCtx.Done()
			close(stream)
		}()
		return stream
	}
	client.Watch(ctx)
	if len(revisions) != 2 || revisions[0] != 41 || revisions[1] != 41 {
		t.Fatalf("watch revisions = %v, want [41 41]", revisions)
	}
	select {
	case <-recoveryStarted:
		t.Fatal("watch timeout unexpectedly started snapshot recovery")
	default:
	}
}

func TestWatchTimeoutIsIdleNotConnectionLifetime(t *testing.T) {
	_, events := newWatcherStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const timeout = 30 * time.Millisecond
	var opens atomic.Int32
	firstOpened := make(chan struct{})
	stopProgress := make(chan struct{})
	reopened := make(chan struct{})
	client := &ConfigClient{
		prefix:       canonicalEtcdPrefix("/apisix"),
		events:       events,
		watchTimeout: timeout,
		knownKeys:    map[string]struct{}{},
	}
	client.openWatch = func(watchCtx context.Context, revision int64) clientv3.WatchChan {
		stream := make(chan clientv3.WatchResponse)
		if opens.Add(1) == 1 {
			close(firstOpened)
			go func() {
				ticker := time.NewTicker(timeout / 5)
				defer ticker.Stop()
				var progressRevision int64
				for {
					select {
					case <-watchCtx.Done():
						close(stream)
						return
					case <-stopProgress:
						return
					case <-ticker.C:
						progressRevision++
						stream <- clientv3.WatchResponse{
							Header: etcdserverpb.ResponseHeader{Revision: progressRevision},
						}
					}
				}
			}()
			return stream
		}
		close(reopened)
		cancel()
		close(stream)
		return stream
	}

	done := make(chan struct{})
	go func() {
		client.Watch(ctx)
		close(done)
	}()
	<-firstOpened
	time.Sleep(3 * timeout)
	if got := opens.Load(); got != 1 {
		t.Fatalf("watch reopened during continuous progress: opens = %d, want 1", got)
	}
	close(stopProgress)
	select {
	case <-reopened:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watch did not reopen after progress stopped")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch did not stop after cancellation")
	}
}

func TestConfiguredRecoveryDelayIncludesAtMostFiftyPercentJitter(t *testing.T) {
	client := &ConfigClient{resyncDelay: 20 * time.Millisecond}
	for range 20 {
		delay := client.recoveryDelay(0)
		if delay < 20*time.Millisecond || delay > 30*time.Millisecond {
			t.Fatalf("recovery delay = %s, want [20ms, 30ms]", delay)
		}
	}
}

func applyWatcherMutation(t *testing.T, events chan *store.Event, mutation store.Mutation) {
	t.Helper()
	event := store.NewAcknowledgedBatch([]store.Mutation{mutation}, store.BatchOptions{})
	events <- event
	if err := event.Wait(context.Background()); err != nil {
		t.Fatalf("apply watcher seed mutation: %v", err)
	}
}

func newWatcherStore(t *testing.T) (*store.Store, chan *store.Event) {
	t.Helper()
	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/watcher.db", events, testutil.DataEncryptionService(false, nil))
	if err != nil {
		t.Fatalf("open watcher store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })
	return storage, events
}

func TestNewTLSConfigLoadsCertificateAndRejectsMissingFiles(t *testing.T) {
	certDir := t.TempDir()
	certPath := certDir + "/cert.pem"
	keyPath := certDir + "/key.pem"
	if err := os.WriteFile(certPath, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTLSConfig(certPath, keyPath, "", nil); err == nil {
		t.Fatal("NewTLSConfig(invalid cert files) error = nil")
	}
}

func watcherConfigApplyReadyValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("write config-apply ready gauge: %v", err)
	}
	return metric.GetGauge().GetValue()
}

func watcherConfigApplyGaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("write config-apply quarantine gauge: %v", err)
	}
	return metric.GetGauge().GetValue()
}
