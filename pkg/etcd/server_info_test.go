package etcd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type fakeServerInfoLeaseClient struct {
	nextLeaseID clientv3.LeaseID
	grantCount  int
	grantTTL    int64
	putCount    int
	keepCount   int
	lastKey     string
	lastValue   string
	grantErr    error
	putErr      error
	keepErr     error
}

func (f *fakeServerInfoLeaseClient) Put(
	_ context.Context,
	key string,
	value string,
	_ ...clientv3.OpOption,
) (*clientv3.PutResponse, error) {
	f.putCount++
	f.lastKey = key
	f.lastValue = value
	return &clientv3.PutResponse{}, f.putErr
}

func (f *fakeServerInfoLeaseClient) Grant(
	_ context.Context,
	ttl int64,
) (*clientv3.LeaseGrantResponse, error) {
	f.grantCount++
	f.grantTTL = ttl
	if f.grantErr != nil {
		return nil, f.grantErr
	}
	return &clientv3.LeaseGrantResponse{ID: f.nextLeaseID}, nil
}

func (f *fakeServerInfoLeaseClient) KeepAliveOnce(
	_ context.Context,
	_ clientv3.LeaseID,
) (*clientv3.LeaseKeepAliveResponse, error) {
	f.keepCount++
	if f.keepErr != nil {
		return nil, f.keepErr
	}
	return &clientv3.LeaseKeepAliveResponse{}, nil
}

func TestServerInfoKeyUsesEtcdPrefix(t *testing.T) {
	if got := serverInfoKey("/apisix/", "node-a"); got != "/apisix/data_plane/server_info/node-a" {
		t.Fatalf("serverInfoKey() = %q, want prefixed server-info key", got)
	}
}

func TestServerInfoReporterCreatesLeasePutsValueAndRenews(t *testing.T) {
	client := &fakeServerInfoLeaseClient{nextLeaseID: 42}
	reporter := newServerInfoReporter(client, "/apisix/data_plane/server_info/node-a", 60*time.Second)

	if err := reporter.Report(context.Background(), []byte(`{"id":"node-a"}`)); err != nil {
		t.Fatalf("first Report() error = %v", err)
	}
	if err := reporter.Report(context.Background(), []byte(`{"id":"node-a","version":"apisix-go"}`)); err != nil {
		t.Fatalf("second Report() error = %v", err)
	}

	if client.grantCount != 1 {
		t.Fatalf("Grant calls = %d, want one lease reused across reports", client.grantCount)
	}
	if client.grantTTL != 60 {
		t.Fatalf("Grant TTL = %d, want configured 60 seconds", client.grantTTL)
	}
	if client.putCount != 2 {
		t.Fatalf("Put calls = %d, want two reports", client.putCount)
	}
	if client.keepCount != 2 {
		t.Fatalf("KeepAliveOnce calls = %d, want one renewal per report", client.keepCount)
	}
	if client.lastKey != "/apisix/data_plane/server_info/node-a" {
		t.Fatalf("last key = %q, want server-info key", client.lastKey)
	}
	if client.lastValue != `{"id":"node-a","version":"apisix-go"}` {
		t.Fatalf("last value = %q, want latest server-info payload", client.lastValue)
	}
}

func TestServerInfoReporterRecreatesLeaseAfterKeepAliveFailure(t *testing.T) {
	client := &fakeServerInfoLeaseClient{nextLeaseID: 42}
	reporter := newServerInfoReporter(client, "/apisix/data_plane/server_info/node-a", 60*time.Second)

	client.keepErr = context.Canceled
	if err := reporter.Report(context.Background(), []byte(`{"id":"node-a"}`)); err == nil {
		t.Fatal("Report() error = nil, want keepalive failure")
	}
	client.keepErr = nil
	client.nextLeaseID = 43
	if err := reporter.Report(context.Background(), []byte(`{"id":"node-a"}`)); err != nil {
		t.Fatalf("Report() after keepalive recovery error = %v", err)
	}

	if client.grantCount != 2 {
		t.Fatalf("Grant calls = %d, want lease recreation after keepalive failure", client.grantCount)
	}
}

func TestServerInfoReporterStartCancellationStopsRefresh(t *testing.T) {
	client := &fakeServerInfoLeaseClient{nextLeaseID: 42}
	reporter := newServerInfoReporter(client, "/apisix/data_plane/server_info/node-a", 2*time.Second)

	var providerCalls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	if err := reporter.Start(ctx, func() ([]byte, error) {
		providerCalls.Add(1)
		return []byte(`{"id":"node-a"}`), nil
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if client.putCount != 1 || providerCalls.Load() != 1 {
		t.Fatalf("put/provider after start = %d/%d, want immediate first report", client.putCount, providerCalls.Load())
	}

	cancel()
	time.Sleep(1100 * time.Millisecond)
	if client.putCount != 1 || providerCalls.Load() != 1 {
		t.Fatalf("put/provider after cancellation = %d/%d, want no refresh", client.putCount, providerCalls.Load())
	}
}

func TestServerInfoReporterStartRejectsNilProviderAndProviderErrors(t *testing.T) {
	client := &fakeServerInfoLeaseClient{nextLeaseID: 42}

	reporter := newServerInfoReporter(client, "/apisix/data_plane/server_info/node-a", 2*time.Second)
	if err := reporter.Start(context.Background(), nil); err == nil {
		t.Fatal("Start(nil provider) error = nil")
	}

	if err := reporter.Start(context.Background(), func() ([]byte, error) {
		return nil, errors.New("build failed")
	}); err == nil {
		t.Fatal("Start(failing provider) error = nil")
	}
	if client.putCount != 0 {
		t.Fatalf("Put calls = %d, want none before a successful report", client.putCount)
	}
}
