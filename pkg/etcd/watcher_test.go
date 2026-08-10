package etcd

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/store"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

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
	if err == nil {
		t.Fatal("applyWatchResponse() error = nil, want second event failure")
	}
	if client.lastRevision != 10 {
		t.Fatalf("lastRevision = %d, want unchanged 10", client.lastRevision)
	}
	if _, ok := client.knownKeys["/apisix/routes/good"]; ok {
		t.Fatal("knownKeys committed partial batch")
	}
	if value, err := storage.GetFromBucket("routes", []byte("good")); err != nil || string(value) != `{"id":"good"}` {
		t.Fatalf("durable first event = %q, %v; want acknowledged partial event", value, err)
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
	if err == nil {
		t.Fatal("applySnapshot() error = nil, want second event failure")
	}
	if client.lastRevision != 10 {
		t.Fatalf("lastRevision = %d, want unchanged 10", client.lastRevision)
	}
	if len(client.knownKeys) != 1 {
		t.Fatalf("knownKeys = %v, want only previous key", client.knownKeys)
	}
	if _, ok := client.knownKeys["/apisix/routes/old"]; !ok {
		t.Fatal("knownKeys lost previous key after failed snapshot")
	}
	if value, err := storage.GetFromBucket("routes", []byte("good")); err != nil || string(value) != `{"id":"good"}` {
		t.Fatalf("durable first snapshot event = %q, %v; want acknowledged partial event", value, err)
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

	if client.prefix != "/apisix" || client.startupRetry != 0 {
		t.Fatalf("prefix/startupRetry = %q/%d, want /apisix/0", client.prefix, client.startupRetry)
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

func newWatcherStore(t *testing.T) (*store.Store, chan *store.Event) {
	t.Helper()
	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/watcher.db", events)
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
