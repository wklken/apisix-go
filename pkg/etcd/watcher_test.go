package etcd

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/store"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var _ generation.DesiredApplier = (*generation.Coordinator)(nil)

type recordingDesiredApplier struct {
	mu      sync.Mutex
	batches []generation.DesiredBatch
	apply   func(context.Context, generation.DesiredBatch) (generation.Acknowledgement, error)
}

func (a *recordingDesiredApplier) Apply(
	ctx context.Context,
	batch generation.DesiredBatch,
) (generation.Acknowledgement, error) {
	a.mu.Lock()
	a.batches = append(a.batches, batch)
	apply := a.apply
	a.mu.Unlock()
	if apply == nil {
		return acknowledgedEtcdTestBatch(batch, 1, generation.DispositionPublished), nil
	}
	return apply(ctx, batch)
}

func (a *recordingDesiredApplier) recordedBatches() []generation.DesiredBatch {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.batches)
}

type acknowledgedEtcdTestState struct {
	knownKeys    map[string]int64
	tombstones   map[string]int64
	quarantine   map[string]int64
	lastCursor   generation.ProviderCursor
	lastRevision int64
	revisions    generation.RevisionSet
	domains      []generation.Domain
	decisions    map[generation.Domain][]generation.ResourceDecision
}

func (c *ConfigClient) snapshotAcknowledgedState() acknowledgedEtcdTestState {
	return acknowledgedEtcdTestState{
		knownKeys: cloneKnownKeys(c.knownKeys), tombstones: cloneKnownKeys(c.tombstones),
		quarantine: cloneQuarantine(c.quarantine),
		lastCursor: c.lastCursor, lastRevision: c.lastRevision, revisions: c.revisions,
		domains: slices.Clone(c.domains), decisions: cloneEtcdDecisions(c.decisions),
	}
}

func newEtcdTestConfigClient(applier generation.DesiredApplier) *ConfigClient {
	lifetimeCtx, cancelLifetime := context.WithCancel(context.Background())
	return &ConfigClient{
		prefix: "/apisix/", applier: applier,
		requestTimeout: time.Second, healthCheckInterval: time.Hour,
		knownKeys: make(map[string]int64), tombstones: make(map[string]int64),
		quarantine:  make(map[string]int64),
		decisions:   make(map[generation.Domain][]generation.ResourceDecision),
		lifetimeCtx: lifetimeCtx, cancelLifetime: cancelLifetime,
		reporters: make(map[*ServerInfoReporter]struct{}),
	}
}

func acknowledgedEtcdTestBatch(
	batch generation.DesiredBatch,
	desired uint64,
	disposition generation.ResourceDisposition,
) generation.Acknowledgement {
	decisions := make(map[generation.Domain][]generation.ResourceDecision, len(batch.RequiredDomains))
	for _, domain := range batch.RequiredDomains {
		decisions[domain] = nil
	}
	for _, mutation := range batch.Mutations {
		mutationDisposition := disposition
		if mutation.Type == generation.MutationDelete {
			mutationDisposition = generation.DispositionDeleted
		}
		for _, domain := range generation.DomainsForResourceKind(mutation.Key.Kind) {
			if slices.Contains(batch.RequiredDomains, domain) {
				decisions[domain] = append(decisions[domain], generation.ResourceDecision{
					Key: mutation.Key, Disposition: mutationDisposition, Code: "test-acknowledged",
				})
			}
		}
	}
	revisions := generation.RevisionSet{Desired: desired}
	if slices.Contains(batch.RequiredDomains, generation.DomainHTTP) {
		revisions.HTTP = desired
	}
	if slices.Contains(batch.RequiredDomains, generation.DomainStream) {
		revisions.Stream = desired
	}
	return generation.Acknowledgement{Cursor: batch.Cursor, Revisions: revisions, Decisions: decisions}
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

func TestDesiredBatchFromEtcdSnapshotReplacesManagedNamespace(t *testing.T) {
	value := []byte(`{"id":"r1"}`)
	batch, err := desiredBatchFromEtcdSnapshot("apisix", &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 0xabc, Revision: 71},
		Kvs: []*mvccpb.KeyValue{
			{Key: []byte("/apisix/routes/r1"), Value: value, ModRevision: 69},
			{Key: []byte("/apisix/data_plane/server_info/node-1")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Cursor != (generation.ProviderCursor{
		Provider: etcdProviderID(0xabc, "/apisix/"), Revision: "71",
	}) || !batch.ReplaceManaged || len(batch.Mutations) != 1 ||
		batch.Mutations[0].Key != (generation.ResourceKey{Kind: "routes", ID: "r1"}) ||
		!slices.Equal(batch.RequiredDomains, []generation.Domain{
			generation.DomainHTTP, generation.DomainStream,
		}) {
		t.Fatalf("snapshot batch = %+v", batch)
	}
	value[0] = 'x'
	if string(batch.Mutations[0].Value) != `{"id":"r1"}` {
		t.Fatal("snapshot mutation aliases etcd response value")
	}
}

func TestDesiredBatchFromEtcdWatchCarriesRevisionDeleteAndDomains(t *testing.T) {
	batch, err := desiredBatchFromEtcdWatch("/apisix", clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: 0xabc, Revision: 100},
		Events: []*clientv3.Event{{
			Type: mvccpb.DELETE,
			Kv:   &mvccpb.KeyValue{Key: []byte("/apisix/stream_routes/mqtt"), ModRevision: 71},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Cursor.Revision != "71" || batch.ReplaceManaged || len(batch.Mutations) != 1 ||
		batch.Mutations[0].Type != generation.MutationDelete ||
		batch.Mutations[0].Key != (generation.ResourceKey{Kind: "stream_routes", ID: "mqtt"}) ||
		!slices.Equal(batch.RequiredDomains, []generation.Domain{generation.DomainStream}) {
		t.Fatalf("watch batch = %+v", batch)
	}
}

func TestDesiredBatchFromEtcdWatchRejectsInvalidBoundaries(t *testing.T) {
	valid := &clientv3.Event{
		Type: mvccpb.PUT,
		Kv:   &mvccpb.KeyValue{Key: []byte("/apisix/routes/r1"), ModRevision: 1},
	}
	for name, response := range map[string]clientv3.WatchResponse{
		"missing cluster": {Header: etcdserverpb.ResponseHeader{Revision: 1}, Events: []*clientv3.Event{valid}},
		"nil event":       {Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 1}, Events: []*clientv3.Event{nil}},
		"created":         {Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 1}, Created: true},
		"canceled":        {Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 1}, Canceled: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := desiredBatchFromEtcdWatch("/apisix", response); err == nil {
				t.Fatal("error = nil, want invalid boundary rejection")
			}
		})
	}
}

func TestDesiredBatchFromEtcdProviderAuthorityIdentity(t *testing.T) {
	base := etcdProviderID(0xabc, "/apisix-a/")
	if !strings.HasPrefix(base, "etcd/v1/0000000000000abc/") ||
		base != etcdProviderID(0xabc, "//apisix-a//") ||
		base == etcdProviderID(0xdef, "/apisix-a/") ||
		base == etcdProviderID(0xabc, "/apisix-b/") {
		t.Fatalf("provider authority identity = %q", base)
	}
}

func TestDesiredBatchFromEtcdProviderAuthorityRequiresSnapshotTransfer(t *testing.T) {
	journal, err := store.OpenJournal(t.TempDir()+"/journal.db", store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	coordinator := generation.NewCoordinator(journal, &etcdCoordinatorTestEngine{})

	clusterA, err := desiredBatchFromEtcdWatch(
		"/apisix", etcdWatchPut(0xaaa, 71, 71, "/apisix/routes/r1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Apply(context.Background(), clusterA); err != nil {
		t.Fatal(err)
	}
	clusterB, err := desiredBatchFromEtcdWatch(
		"/apisix", etcdWatchPut(0xbbb, 4, 4, "/apisix/routes/r1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Apply(context.Background(), clusterB); !errors.Is(err, generation.ErrProviderConflict) {
		t.Fatalf("incremental authority change error = %v, want ErrProviderConflict", err)
	}
	transfer, err := desiredBatchFromEtcdSnapshot("/apisix", &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 0xbbb, Revision: 4},
		Kvs:    []*mvccpb.KeyValue{{Key: []byte("/apisix/routes/r1"), ModRevision: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := coordinator.Apply(context.Background(), transfer)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Cursor != transfer.Cursor || ack.Revisions != (generation.RevisionSet{Desired: 2, HTTP: 2, Stream: 2}) {
		t.Fatalf("authority transfer acknowledgement = %+v", ack)
	}
}

type etcdCoordinatorTestEngine struct{}

func (*etcdCoordinatorTestEngine) Prepare(
	_ context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	_ map[generation.Domain]generation.PublishedGeneration,
) (generation.PublicationSet, error) {
	set := generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(ticket.RequiredDomains)),
	}
	for _, domain := range ticket.RequiredDomains {
		var resources []generation.Resource
		var tombstones []generation.Tombstone
		var closure []generation.ResourceKey
		var decisions []generation.ResourceDecision
		for _, resource := range desired.Resources() {
			if !slices.Contains(generation.DomainsForResourceKind(resource.Key.Kind), domain) {
				continue
			}
			resources = append(resources, resource)
			closure = append(closure, resource.Key)
			decisions = append(decisions, generation.ResourceDecision{
				Key: resource.Key, Disposition: generation.DispositionPublished, Code: "test-published",
			})
		}
		for _, tombstone := range desired.Tombstones() {
			if !slices.Contains(generation.DomainsForResourceKind(tombstone.Key.Kind), domain) {
				continue
			}
			tombstones = append(tombstones, tombstone)
			closure = append(closure, tombstone.Key)
			decisions = append(decisions, generation.ResourceDecision{
				Key: tombstone.Key, Disposition: generation.DispositionDeleted, Code: "test-deleted",
			})
		}
		snapshot, err := generation.NewSnapshot(ticket.DesiredRevision, resources, tombstones)
		if err != nil {
			return generation.PublicationSet{}, err
		}
		set.Domains[domain] = generation.PublicationCandidate{
			Artifact: generation.GenerationArtifact{
				Domain: domain, Revision: ticket.DesiredRevision,
				Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
			},
			Snapshot: snapshot, Closure: closure, Decisions: decisions,
		}
	}
	return set, nil
}

func (*etcdCoordinatorTestEngine) DiscardPrepared(context.Context, generation.PublicationSet) error {
	return nil
}

func (*etcdCoordinatorTestEngine) Activate(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) error {
	return nil
}

func (*etcdCoordinatorTestEngine) RollbackActivation(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) error {
	return nil
}

func (*etcdCoordinatorTestEngine) FinalizeActivation(
	context.Context,
	generation.PublicationToken,
	generation.PublicationSet,
) {
}

func (*etcdCoordinatorTestEngine) ConfirmActive(context.Context, generation.PublicationSet) error {
	return nil
}

func TestConfigClientSnapshotAppliesCanonicalDesiredBatch(t *testing.T) {
	applier := &recordingDesiredApplier{}
	client := newEtcdTestConfigClient(applier)
	response := &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 0xabc, Revision: 71},
		Kvs: []*mvccpb.KeyValue{{
			Key: []byte("/apisix/routes/r1"), Value: []byte(`{"id":"r1"}`), ModRevision: 69,
		}},
	}
	if err := client.applySnapshot(context.Background(), response); err != nil {
		t.Fatal(err)
	}
	want, err := desiredBatchFromEtcdSnapshot("/apisix/", response)
	if err != nil {
		t.Fatal(err)
	}
	if got := applier.recordedBatches(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("applied batches = %+v, want canonical %+v", got, want)
	}
	if client.knownKeys["/apisix/routes/r1"] != 69 || client.lastCursor != want.Cursor ||
		client.lastRevision != 71 {
		t.Fatalf("committed snapshot state = %+v", client.snapshotAcknowledgedState())
	}
}

func TestConfigClientWatchAdvancesOnlyAfterAcknowledgement(t *testing.T) {
	wantErr := errors.New("compile failed")
	applier := &recordingDesiredApplier{apply: func(
		context.Context,
		generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		return generation.Acknowledgement{}, wantErr
	}}
	client := newEtcdTestConfigClient(applier)
	client.knownKeys["/apisix/routes/old"] = 7
	client.lastCursor = generation.ProviderCursor{Provider: etcdProviderID(1, "/apisix"), Revision: "7"}
	client.lastRevision = 7
	client.revisions = generation.RevisionSet{Desired: 2, HTTP: 2}
	if err := client.applyWatchResponse(
		context.Background(), etcdWatchPut(1, 14, 12, "/apisix/routes/new"),
	); !errors.Is(err, wantErr) {
		t.Fatalf("applyWatchResponse() error = %v, want %v", err, wantErr)
	}
	if client.lastRevision != 7 || !reflect.DeepEqual(client.knownKeys, map[string]int64{"/apisix/routes/old": 7}) {
		t.Fatalf("state advanced before acknowledgement: %+v", client.snapshotAcknowledgedState())
	}
}

func TestConfigClientFailedApplyRetainsCursorKnownKeysDecisionsAndQuarantine(t *testing.T) {
	oldReady, oldQuarantine := metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined
	oldRevision, oldModifyIndexes := metrics.EtcdRevision, metrics.EtcdModifyIndexes
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_etcd_failed_apply_ready"})
	metrics.ConfigApplyQuarantined = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "test_etcd_failed_apply_quarantine"},
	)
	metrics.EtcdRevision = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_etcd_failed_apply_revision"})
	metrics.EtcdModifyIndexes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_etcd_failed_apply_modify_index"}, []string{"key"},
	)
	t.Cleanup(func() {
		metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined = oldReady, oldQuarantine
		metrics.EtcdRevision, metrics.EtcdModifyIndexes = oldRevision, oldModifyIndexes
	})
	wantErr := errors.New("journal failed")
	applier := &recordingDesiredApplier{apply: func(
		context.Context,
		generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		return generation.Acknowledgement{}, wantErr
	}}
	client := newEtcdTestConfigClient(applier)
	client.knownKeys["/apisix/routes/old"] = 7
	client.quarantine["/apisix/routes/old"] = 7
	client.lastCursor = generation.ProviderCursor{Provider: etcdProviderID(1, "/apisix"), Revision: "7"}
	client.lastRevision = 7
	client.revisions = generation.RevisionSet{Desired: 2, HTTP: 2}
	client.domains = []generation.Domain{generation.DomainHTTP}
	client.decisions = map[generation.Domain][]generation.ResourceDecision{
		generation.DomainHTTP: {{
			Key:         generation.ResourceKey{Kind: "routes", ID: "old"},
			Disposition: generation.DispositionLastGood, Code: "old-last-good",
		}},
	}
	metrics.RecordConfigApplyQuarantine(1)
	metrics.RecordEtcdAppliedRevision(7)
	metrics.RecordEtcdModifyIndex("/apisix/routes/old", 7)
	wantState := client.snapshotAcknowledgedState()
	wantReady := metricGaugeValue(t, metrics.ConfigApplyReady)
	wantQuarantine := metricGaugeValue(t, metrics.ConfigApplyQuarantined)
	wantRevision := metricGaugeValue(t, metrics.EtcdRevision)
	wantModify := metricGaugeValue(t, metrics.EtcdModifyIndexes.WithLabelValues("routes"))

	if err := client.applyWatchResponse(
		context.Background(), etcdWatchPut(1, 14, 12, "/apisix/routes/new"),
	); !errors.Is(err, wantErr) {
		t.Fatalf("applyWatchResponse() error = %v, want %v", err, wantErr)
	}
	if got := client.snapshotAcknowledgedState(); !reflect.DeepEqual(got, wantState) {
		t.Fatalf("acknowledged state changed on failure:\n got: %#v\nwant: %#v", got, wantState)
	}
	if got := metricGaugeValue(t, metrics.ConfigApplyReady); got != wantReady {
		t.Fatalf("readiness changed on failed attempt: got %v want %v", got, wantReady)
	}
	if got := metricGaugeValue(t, metrics.ConfigApplyQuarantined); got != wantQuarantine {
		t.Fatalf("quarantine gauge changed on failed attempt: got %v want %v", got, wantQuarantine)
	}
	if got := metricGaugeValue(t, metrics.EtcdRevision); got != wantRevision {
		t.Fatalf("applied revision changed on failed attempt: got %v want %v", got, wantRevision)
	}
	if got := metricGaugeValue(t, metrics.EtcdModifyIndexes.WithLabelValues("routes")); got != wantModify {
		t.Fatalf("modify index changed on failed attempt: got %v want %v", got, wantModify)
	}
}

func TestConfigClientSameCursorReplayUsesCommittedAcknowledgement(t *testing.T) {
	applier := &recordingDesiredApplier{}
	client := newEtcdTestConfigClient(applier)
	response := &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 9},
		Kvs:    []*mvccpb.KeyValue{{Key: []byte("/apisix/routes/r1"), ModRevision: 8}},
	}
	if err := client.applySnapshot(context.Background(), response); err != nil {
		t.Fatal(err)
	}
	if err := client.applySnapshot(context.Background(), response); err != nil {
		t.Fatalf("same cursor replay: %v", err)
	}
	if got := len(applier.recordedBatches()); got != 2 {
		t.Fatalf("Apply calls = %d, want 2 including committed replay", got)
	}
}

func TestConfigClientCommittedSnapshotReplayUsesAcknowledgedDomains(t *testing.T) {
	for name, committedReplay := range map[string]bool{
		"committed replay":                 true,
		"unmarked partial acknowledgement": false,
	} {
		t.Run(name, func(t *testing.T) {
			applier := &recordingDesiredApplier{apply: func(
				_ context.Context,
				batch generation.DesiredBatch,
			) (generation.Acknowledgement, error) {
				return generation.Acknowledgement{
					Cursor:          batch.Cursor,
					Revisions:       generation.RevisionSet{Desired: 4, HTTP: 4, Stream: 2},
					CommittedReplay: committedReplay,
					Decisions: map[generation.Domain][]generation.ResourceDecision{
						generation.DomainHTTP: {{
							Key:         generation.ResourceKey{Kind: "routes", ID: "current"},
							Disposition: generation.DispositionPublished,
							Code:        "committed-http-publication",
						}},
					},
				}, nil
			}}
			client := newEtcdTestConfigClient(applier)
			err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
				Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 12},
				Kvs: []*mvccpb.KeyValue{{
					Key: []byte("/apisix/routes/current"), Value: []byte(`{"id":"current"}`), ModRevision: 10,
				}},
			})
			if !committedReplay {
				if err == nil {
					t.Fatal("applySnapshot() error = nil, want partial acknowledgement rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("applySnapshot() committed replay: %v", err)
			}
			if client.revisions != (generation.RevisionSet{Desired: 4, HTTP: 4, Stream: 2}) ||
				!slices.Equal(client.domains, []generation.Domain{generation.DomainHTTP}) ||
				client.lastRevision != 12 {
				t.Fatalf("committed replay state = %+v", client.snapshotAcknowledgedState())
			}
		})
	}
}

func TestConfigClientAcknowledgementCommitsFullClosureDecisionsAndQuarantine(t *testing.T) {
	applier := &recordingDesiredApplier{apply: func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		disposition := generation.DispositionLastGood
		if batch.Cursor.Revision == "12" {
			disposition = generation.DispositionPublished
		}
		return acknowledgedEtcdTestBatch(
			batch,
			map[string]uint64{"10": 1, "12": 2}[batch.Cursor.Revision],
			disposition,
		), nil
	}}
	client := newEtcdTestConfigClient(applier)
	key := "/apisix/upstreams/u1"
	if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 10},
		Kvs:    []*mvccpb.KeyValue{{Key: []byte(key), ModRevision: 9}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := client.quarantine[key]; got != 9 {
		t.Fatalf("cross-domain last-good quarantine = %d, want 9", got)
	}
	if len(client.decisions[generation.DomainHTTP]) != 1 ||
		len(client.decisions[generation.DomainStream]) != 1 {
		t.Fatalf("committed decisions = %+v", client.decisions)
	}
	if err := client.applyWatchResponse(
		context.Background(), etcdWatchPut(1, 12, 12, key),
	); err != nil {
		t.Fatal(err)
	}
	if len(client.quarantine) != 0 {
		t.Fatalf("published acknowledgement did not clear quarantine: %v", client.quarantine)
	}
}

func TestConfigClientAcknowledgementRejectsIncompleteOrForeignDecisions(t *testing.T) {
	for name, mutate := range map[string]func(*generation.Acknowledgement){
		"missing closure decision": func(ack *generation.Acknowledgement) {
			ack.Decisions[generation.DomainHTTP] = nil
		},
		"foreign decision": func(ack *generation.Acknowledgement) {
			ack.Decisions[generation.DomainHTTP][0].Key.ID = "foreign"
		},
		"unexpected domain": func(ack *generation.Acknowledgement) {
			ack.Decisions[generation.Domain("other")] = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			applier := &recordingDesiredApplier{apply: func(
				_ context.Context,
				batch generation.DesiredBatch,
			) (generation.Acknowledgement, error) {
				ack := acknowledgedEtcdTestBatch(batch, 1, generation.DispositionPublished)
				mutate(&ack)
				return ack, nil
			}}
			client := newEtcdTestConfigClient(applier)
			if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
				Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 10},
				Kvs:    []*mvccpb.KeyValue{{Key: []byte("/apisix/routes/r1"), ModRevision: 9}},
			}); err == nil {
				t.Fatal("applySnapshot() error = nil, want invalid acknowledgement")
			}
			if client.lastRevision != 0 || len(client.knownKeys) != 0 {
				t.Fatalf("invalid acknowledgement advanced state: %+v", client.snapshotAcknowledgedState())
			}
		})
	}
}

func TestConfigClientSnapshotTransfersProviderAuthorityAtLowerEtcdRevision(t *testing.T) {
	applier := &recordingDesiredApplier{apply: func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		ack := acknowledgedEtcdTestBatch(batch, 4, generation.DispositionPublished)
		ack.Decisions[generation.DomainHTTP] = append(
			ack.Decisions[generation.DomainHTTP],
			generation.ResourceDecision{
				Key:         generation.ResourceKey{Kind: "routes", ID: "old"},
				Disposition: generation.DispositionDeleted, Code: "test-deleted",
			},
		)
		return ack, nil
	}}
	client := newEtcdTestConfigClient(applier)
	client.lastCursor = generation.ProviderCursor{Provider: etcdProviderID(1, "/apisix"), Revision: "100"}
	client.lastRevision = 100
	client.revisions = generation.RevisionSet{Desired: 3, HTTP: 3, Stream: 3}
	client.knownKeys["/apisix/routes/old"] = 99
	client.quarantine["/apisix/routes/old"] = 99
	if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 2, Revision: 5},
		Kvs:    []*mvccpb.KeyValue{{Key: []byte("/apisix/routes/new"), ModRevision: 4}},
	}); err != nil {
		t.Fatal(err)
	}
	if client.lastRevision != 5 || client.lastCursor.Provider != etcdProviderID(2, "/apisix") ||
		!reflect.DeepEqual(client.knownKeys, map[string]int64{"/apisix/routes/new": 4}) ||
		len(client.quarantine) != 0 {
		t.Fatalf("authority transfer state = %+v", client.snapshotAcknowledgedState())
	}
}

func TestConfigClientSameCursorReplayAcceptsCommittedHistoricalTombstones(t *testing.T) {
	applier := &recordingDesiredApplier{apply: func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		ack := acknowledgedEtcdTestBatch(batch, 7, generation.DispositionPublished)
		ack.Decisions[generation.DomainHTTP] = append(
			ack.Decisions[generation.DomainHTTP],
			generation.ResourceDecision{
				Key:         generation.ResourceKey{Kind: "routes", ID: "deleted-before-restart"},
				Disposition: generation.DispositionDeleted, Code: "committed-tombstone",
			},
		)
		return ack, nil
	}}
	client := newEtcdTestConfigClient(applier)
	if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 20},
		Kvs:    []*mvccpb.KeyValue{{Key: []byte("/apisix/routes/current"), ModRevision: 19}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := client.tombstones["/apisix/routes/deleted-before-restart"]; got != 20 {
		t.Fatalf("recovered tombstone acknowledgement revision = %d, want provider cursor 20", got)
	}
}

func TestConfigClientIncrementalAcknowledgementRejectsUnknownDeletedDecision(t *testing.T) {
	oldReady, oldQuarantine := metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined
	oldRevision, oldModifyIndexes := metrics.EtcdRevision, metrics.EtcdModifyIndexes
	metrics.ConfigApplyReady = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "test_etcd_unknown_deleted_ready"},
	)
	metrics.ConfigApplyQuarantined = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "test_etcd_unknown_deleted_quarantine"},
	)
	metrics.EtcdRevision = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "test_etcd_unknown_deleted_revision"},
	)
	metrics.EtcdModifyIndexes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_etcd_unknown_deleted_modify_index"}, []string{"key"},
	)
	t.Cleanup(func() {
		metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined = oldReady, oldQuarantine
		metrics.EtcdRevision, metrics.EtcdModifyIndexes = oldRevision, oldModifyIndexes
	})

	applier := &recordingDesiredApplier{apply: func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		ack := acknowledgedEtcdTestBatch(batch, 1, generation.DispositionPublished)
		ack.Decisions[generation.DomainHTTP] = append(
			ack.Decisions[generation.DomainHTTP],
			generation.ResourceDecision{
				Key:         generation.ResourceKey{Kind: "routes", ID: "invented"},
				Disposition: generation.DispositionDeleted, Code: "invented-tombstone",
			},
		)
		return ack, nil
	}}
	client := newEtcdTestConfigClient(applier)
	wantState := client.snapshotAcknowledgedState()
	wantReady := metricGaugeValue(t, metrics.ConfigApplyReady)
	wantQuarantine := metricGaugeValue(t, metrics.ConfigApplyQuarantined)
	wantRevision := metricGaugeValue(t, metrics.EtcdRevision)
	wantModify := metricGaugeValue(t, metrics.EtcdModifyIndexes.WithLabelValues("routes"))

	if err := client.applyWatchResponse(
		context.Background(), etcdWatchPut(1, 9, 9, "/apisix/routes/current"),
	); err == nil {
		t.Fatal("applyWatchResponse() error = nil, want unknown incremental tombstone rejection")
	}
	if got := client.snapshotAcknowledgedState(); !reflect.DeepEqual(got, wantState) {
		t.Fatalf("unknown tombstone changed acknowledged state:\n got: %#v\nwant: %#v", got, wantState)
	}
	if got := metricGaugeValue(t, metrics.ConfigApplyReady); got != wantReady {
		t.Fatalf("readiness changed: got %v want %v", got, wantReady)
	}
	if got := metricGaugeValue(t, metrics.ConfigApplyQuarantined); got != wantQuarantine {
		t.Fatalf("quarantine changed: got %v want %v", got, wantQuarantine)
	}
	if got := metricGaugeValue(t, metrics.EtcdRevision); got != wantRevision {
		t.Fatalf("revision changed: got %v want %v", got, wantRevision)
	}
	if got := metricGaugeValue(t, metrics.EtcdModifyIndexes.WithLabelValues("routes")); got != wantModify {
		t.Fatalf("modify index changed: got %v want %v", got, wantModify)
	}
}

func TestConfigClientAcknowledgementRejectsImpossibleRevisionTransitions(t *testing.T) {
	prior := generation.RevisionSet{Desired: 3, HTTP: 3, Stream: 2}
	tests := map[string]struct {
		response clientv3.WatchResponse
		revise   func(*generation.Acknowledgement)
	}{
		"different cursor reuses desired revision": {
			response: etcdWatchPut(1, 11, 11, "/apisix/routes/current"),
			revise: func(ack *generation.Acknowledgement) {
				ack.Revisions = prior
			},
		},
		"http only advances untouched stream": {
			response: etcdWatchPut(1, 11, 11, "/apisix/routes/current"),
			revise: func(ack *generation.Acknowledgement) {
				ack.Revisions = generation.RevisionSet{Desired: 4, HTTP: 4, Stream: 4}
			},
		},
		"progress advances untouched stream": {
			response: clientv3.WatchResponse{
				Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 11},
			},
			revise: func(ack *generation.Acknowledgement) {
				ack.Revisions = generation.RevisionSet{Desired: 4, HTTP: 3, Stream: 4}
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			oldReady, oldQuarantine := metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined
			oldRevision, oldModifyIndexes := metrics.EtcdRevision, metrics.EtcdModifyIndexes
			metrics.ConfigApplyReady = prometheus.NewGauge(
				prometheus.GaugeOpts{Name: "test_etcd_impossible_revision_ready"},
			)
			metrics.ConfigApplyQuarantined = prometheus.NewGauge(
				prometheus.GaugeOpts{Name: "test_etcd_impossible_revision_quarantine"},
			)
			metrics.EtcdRevision = prometheus.NewGauge(
				prometheus.GaugeOpts{Name: "test_etcd_impossible_revision"},
			)
			metrics.EtcdModifyIndexes = prometheus.NewGaugeVec(
				prometheus.GaugeOpts{Name: "test_etcd_impossible_revision_modify_index"}, []string{"key"},
			)
			t.Cleanup(func() {
				metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined = oldReady, oldQuarantine
				metrics.EtcdRevision, metrics.EtcdModifyIndexes = oldRevision, oldModifyIndexes
			})

			applier := &recordingDesiredApplier{apply: func(
				_ context.Context,
				batch generation.DesiredBatch,
			) (generation.Acknowledgement, error) {
				ack := acknowledgedEtcdTestBatch(batch, 4, generation.DispositionPublished)
				test.revise(&ack)
				return ack, nil
			}}
			client := newEtcdTestConfigClient(applier)
			client.lastCursor = generation.ProviderCursor{
				Provider: etcdProviderID(1, "/apisix"), Revision: "10",
			}
			client.lastRevision = 10
			client.revisions = prior
			client.domains = []generation.Domain{generation.DomainHTTP, generation.DomainStream}
			client.decisions = map[generation.Domain][]generation.ResourceDecision{
				generation.DomainHTTP: nil, generation.DomainStream: nil,
			}
			metrics.RecordConfigApplyAcknowledgement(true, true, 0)
			metrics.RecordEtcdAppliedRevision(10)
			wantState := client.snapshotAcknowledgedState()
			wantReady := metricGaugeValue(t, metrics.ConfigApplyReady)
			wantQuarantine := metricGaugeValue(t, metrics.ConfigApplyQuarantined)
			wantRevision := metricGaugeValue(t, metrics.EtcdRevision)
			wantModify := metricGaugeValue(t, metrics.EtcdModifyIndexes.WithLabelValues("routes"))

			if err := client.applyWatchResponse(context.Background(), test.response); err == nil {
				t.Fatal("applyWatchResponse() error = nil, want impossible revision transition rejection")
			}
			if got := client.snapshotAcknowledgedState(); !reflect.DeepEqual(got, wantState) {
				t.Fatalf("impossible revision changed state:\n got: %#v\nwant: %#v", got, wantState)
			}
			if got := metricGaugeValue(t, metrics.ConfigApplyReady); got != wantReady {
				t.Fatalf("readiness changed: got %v want %v", got, wantReady)
			}
			if got := metricGaugeValue(t, metrics.ConfigApplyQuarantined); got != wantQuarantine {
				t.Fatalf("quarantine changed: got %v want %v", got, wantQuarantine)
			}
			if got := metricGaugeValue(t, metrics.EtcdRevision); got != wantRevision {
				t.Fatalf("revision changed: got %v want %v", got, wantRevision)
			}
			if got := metricGaugeValue(t, metrics.EtcdModifyIndexes.WithLabelValues("routes")); got != wantModify {
				t.Fatalf("modify index changed: got %v want %v", got, wantModify)
			}
		})
	}
}

func TestConfigClientAcknowledgementCursorMismatchFailsClosed(t *testing.T) {
	applier := &recordingDesiredApplier{apply: func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		ack := acknowledgedEtcdTestBatch(batch, 1, generation.DispositionPublished)
		ack.Cursor.Revision = "10"
		return ack, nil
	}}
	client := newEtcdTestConfigClient(applier)
	if err := client.applyWatchResponse(
		context.Background(), etcdWatchPut(1, 9, 9, "/apisix/routes/r1"),
	); err == nil {
		t.Fatal("applyWatchResponse() error = nil, want cursor mismatch")
	}
	if client.lastRevision != 0 || len(client.knownKeys) != 0 {
		t.Fatalf("state advanced after cursor mismatch: %+v", client.snapshotAcknowledgedState())
	}
}

func TestConfigClientProgressAcknowledgementCommitsCursorWithoutInventingDomains(t *testing.T) {
	applier := &recordingDesiredApplier{}
	client := newEtcdTestConfigClient(applier)
	client.knownKeys["/apisix/routes/r1"] = 8
	response := clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 9},
	}
	if err := client.applyWatchResponse(context.Background(), response); err != nil {
		t.Fatal(err)
	}
	if client.lastRevision != 9 || len(client.domains) != 0 || len(client.decisions) != 0 ||
		client.knownKeys["/apisix/routes/r1"] != 8 {
		t.Fatalf("progress acknowledgement state = %+v", client.snapshotAcknowledgedState())
	}
}

func TestConfigClientProgressAcknowledgementAdvancesDesiredAndKeepsDomainHeads(t *testing.T) {
	applier := &recordingDesiredApplier{apply: func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		return generation.Acknowledgement{
			Cursor:    batch.Cursor,
			Revisions: generation.RevisionSet{Desired: 4, HTTP: 3, Stream: 2},
			Decisions: map[generation.Domain][]generation.ResourceDecision{},
		}, nil
	}}
	client := newEtcdTestConfigClient(applier)
	client.lastCursor = generation.ProviderCursor{Provider: etcdProviderID(1, "/apisix"), Revision: "10"}
	client.lastRevision = 10
	client.revisions = generation.RevisionSet{Desired: 3, HTTP: 3, Stream: 2}
	if err := client.applyWatchResponse(context.Background(), clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 11},
	}); err != nil {
		t.Fatal(err)
	}
	if client.revisions != (generation.RevisionSet{Desired: 4, HTTP: 3, Stream: 2}) ||
		client.lastRevision != 11 || len(client.domains) != 0 {
		t.Fatalf("progress acknowledgement state = %+v", client.snapshotAcknowledgedState())
	}
}

func TestConfigClientCompilerOrJournalFailureRetriesSameProviderPosition(t *testing.T) {
	wantErr := errors.New("publication failed")
	applier := &recordingDesiredApplier{apply: func(
		context.Context,
		generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		return generation.Acknowledgement{}, wantErr
	}}
	client := newEtcdTestConfigClient(applier)
	client.knownKeys["/apisix/routes/old"] = 40
	client.lastRevision = 40
	if err := client.applyWatchResponse(
		context.Background(), etcdWatchPut(1, 90, 75, "/apisix/routes/new"),
	); !errors.Is(err, wantErr) {
		t.Fatalf("applyWatchResponse() error = %v, want %v", err, wantErr)
	}
	if got := client.nextWatchRevision(); got != 41 {
		t.Fatalf("next watch revision = %d, want last acknowledged + 1 (41)", got)
	}
}

func TestConfigClientShutdownCancellationDoesNotCommitProviderState(t *testing.T) {
	started := make(chan struct{})
	applier := &recordingDesiredApplier{apply: func(
		ctx context.Context,
		_ generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		close(started)
		<-ctx.Done()
		return generation.Acknowledgement{}, ctx.Err()
	}}
	client := newEtcdTestConfigClient(applier)
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		return &clientv3.GetResponse{
			Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 9},
			Kvs:    []*mvccpb.KeyValue{{Key: []byte("/apisix/routes/r1"), ModRevision: 9}},
		}, nil
	}
	done := make(chan error, 1)
	go func() { done <- client.FetchAllContext(context.Background()) }()
	<-started
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchAllContext() error = %v, want context.Canceled", err)
	}
	if client.lastRevision != 0 || len(client.knownKeys) != 0 {
		t.Fatalf("state committed during shutdown: %+v", client.snapshotAcknowledgedState())
	}
}

func TestNewConfigClientRejectsNilAndTypedNilApplier(t *testing.T) {
	var typedNil *recordingDesiredApplier
	for name, applier := range map[string]generation.DesiredApplier{"nil": nil, "typed nil": typedNil} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewConfigClientWithOptions(
				[]string{"not a valid endpoint"}, "", "", "/apisix", applier, ClientOptions{},
			); err == nil {
				t.Fatal("constructor error = nil, want missing applier before etcd open")
			}
		})
	}
}

func TestNewConfigClientWithOptionsAppliesRuntimeSettings(t *testing.T) {
	client, err := NewConfigClientWithOptions(
		[]string{"http://127.0.0.1:2379"}, "", "", "/apisix", &recordingDesiredApplier{},
		ClientOptions{
			DialTimeout: 2 * time.Second, RequestTimeout: 3 * time.Second, StartupRetry: 2,
			WatchTimeout: 4 * time.Second, ResyncDelay: 5 * time.Second,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if client.requestTimeout != 3*time.Second || client.startupRetry != 2 ||
		client.watchTimeout != 4*time.Second || client.resyncDelay != 5*time.Second ||
		client.prefix != "/apisix/" || client.openWatch == nil || client.loadSnapshot == nil {
		t.Fatalf("runtime settings not applied: %+v", client)
	}
}

func TestWatchRecoversSnapshotAfterApplyFailureAtAcknowledgedPosition(t *testing.T) {
	watchResponses := make(chan clientv3.WatchResponse, 1)
	watchResponses <- etcdWatchPut(1, 90, 75, "/apisix/routes/new")
	close(watchResponses)
	var applyCalls atomic.Int32
	applier := &recordingDesiredApplier{apply: func(
		_ context.Context,
		batch generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		if applyCalls.Add(1) == 1 {
			return generation.Acknowledgement{}, errors.New("compile failed")
		}
		return acknowledgedEtcdTestBatch(batch, 2, generation.DispositionPublished), nil
	}}
	client := newEtcdTestConfigClient(applier)
	client.lastRevision = 40
	opened := make(chan int64, 2)
	ctx, cancel := context.WithCancel(context.Background())
	client.openWatch = func(_ context.Context, revision int64) clientv3.WatchChan {
		opened <- revision
		if len(opened) == 1 {
			return watchResponses
		}
		cancel()
		stream := make(chan clientv3.WatchResponse)
		close(stream)
		return stream
	}
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		return &clientv3.GetResponse{
			Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 40},
		}, nil
	}
	client.Watch(ctx)
	first, second := <-opened, <-opened
	if first != 41 || second != 41 {
		t.Fatalf("watch revisions = %d then %d, want rejected header ignored", first, second)
	}
}

func TestFetchAllRetriesThenAppliesSnapshot(t *testing.T) {
	client := newEtcdTestConfigClient(&recordingDesiredApplier{})
	client.startupRetry = 1
	var calls atomic.Int32
	client.loadSnapshot = func(context.Context) (*clientv3.GetResponse, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("unavailable")
		}
		return &clientv3.GetResponse{
			Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 20},
		}, nil
	}
	if err := client.FetchAllContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || client.lastRevision != 20 {
		t.Fatalf("fetch calls/revision = %d/%d", calls.Load(), client.lastRevision)
	}
}

func TestProductionHealthCheckUsesSingleKeyGet(t *testing.T) {
	var key string
	var options int
	health := newHealthCheck(func(
		_ context.Context,
		gotKey string,
		gotOptions ...clientv3.OpOption,
	) (*clientv3.GetResponse, error) {
		key, options = gotKey, len(gotOptions)
		return &clientv3.GetResponse{}, nil
	}, "/apisix")
	if err := health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if key != "/apisix" || options != 0 {
		t.Fatalf("health get = %q with %d options, want single-key get", key, options)
	}
}

func TestWatchRetryDelayBounded(t *testing.T) {
	if got := watchRetryDelay(0); got != 100*time.Millisecond {
		t.Fatalf("first retry delay = %s", got)
	}
	if got := watchRetryDelay(100); got != 5*time.Second {
		t.Fatalf("bounded retry delay = %s", got)
	}
}

func TestCanonicalEtcdPrefixAndManagedKeyShapes(t *testing.T) {
	client := &ConfigClient{prefix: canonicalEtcdPrefix("//apisix//")}
	if client.prefix != "/apisix/" {
		t.Fatalf("canonical prefix = %q", client.prefix)
	}
	for key, want := range map[string]generation.ResourceKey{
		"/apisix/routes/r1":           {Kind: "routes", ID: "r1"},
		"/apisix/plugins":             {Kind: "plugins", ID: "plugins"},
		"/apisix/secrets/vault/token": {Kind: "secrets", ID: "vault/token"},
	} {
		kind, id, ok := client.managedKey([]byte(key))
		if !ok || (generation.ResourceKey{Kind: kind, ID: id}) != want {
			t.Fatalf("managedKey(%q) = %q/%q/%v", key, kind, id, ok)
		}
	}
	for _, key := range []string{"/apisix/data_plane/server_info/n1", "/apisix/routes", "/other/routes/r1"} {
		if _, _, ok := client.managedKey([]byte(key)); ok {
			t.Fatalf("managedKey(%q) = managed, want ignored", key)
		}
	}
}

func TestConfiguredRecoveryDelayIncludesAtMostFiftyPercentJitter(t *testing.T) {
	client := &ConfigClient{resyncDelay: 100 * time.Millisecond}
	for range 100 {
		delay := client.recoveryDelay(0)
		if delay < 100*time.Millisecond || delay >= 150*time.Millisecond {
			t.Fatalf("recovery delay = %s, want [100ms,150ms)", delay)
		}
	}
}

func TestNewTLSConfigHonorsVerificationAndSNI(t *testing.T) {
	verify := false
	config, err := NewTLSConfig("", "", "etcd.example.com", &verify)
	if err != nil {
		t.Fatal(err)
	}
	if config.ServerName != "etcd.example.com" || !config.InsecureSkipVerify {
		t.Fatalf("TLS config = %+v", config)
	}
}
