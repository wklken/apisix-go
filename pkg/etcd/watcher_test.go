package etcd

import (
	"context"
	"errors"
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

func TestWatchReconcilesSnapshotAfterUnexpectedClosure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan *store.Event, 4)
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
	deleted := <-events
	created := <-events
	if deleted.Type != store.EventTypeDelete || string(deleted.Key) != "/apisix/routes/old" {
		t.Fatalf("delete event = %s", deleted)
	}
	if created.Type != store.EventTypePut || string(created.Key) != "/apisix/routes/new" {
		t.Fatalf("put event = %s", created)
	}
	store.PutBack(deleted)
	store.PutBack(created)
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
