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

func TestConfigClientServerVersionUsesFirstReachableEndpoint(t *testing.T) {
	client := &ConfigClient{
		endpoints:      []string{"unreachable:2379", "etcd:2379"},
		requestTimeout: time.Second,
		status: func(_ context.Context, endpoint string) (*clientv3.StatusResponse, error) {
			if endpoint == "unreachable:2379" {
				return nil, errors.New("unreachable")
			}
			return &clientv3.StatusResponse{Version: "3.6.13"}, nil
		},
	}
	version, err := client.ServerVersion(context.Background())
	if err != nil {
		t.Fatalf("ServerVersion() error = %v", err)
	}
	if version != "3.6.13" {
		t.Fatalf("ServerVersion() = %q, want 3.6.13", version)
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
	if err := reporter.Stop(); err != nil {
		t.Fatal(err)
	}
	if client.putCount != 1 || providerCalls.Load() != 1 {
		t.Fatalf("put/provider after cancellation = %d/%d, want no refresh", client.putCount, providerCalls.Load())
	}
}

func TestConfigClientCloseCancelsAndJoinsServerInfoReporter(t *testing.T) {
	leaseClient := &fakeServerInfoLeaseClient{nextLeaseID: 42}
	reporter := newServerInfoReporter(leaseClient, "/apisix/data_plane/server_info/node-a", 60*time.Second)
	if err := reporter.Start(context.Background(), func() ([]byte, error) {
		return []byte(`{"id":"node-a"}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	configClient := newEtcdTestConfigClient(&recordingDesiredApplier{})
	configClient.reporters[reporter] = struct{}{}
	if err := configClient.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reporter.done:
	default:
		t.Fatal("ConfigClient.Close returned before joining server-info reporter")
	}
	if err := configClient.Close(); err != nil {
		t.Fatalf("repeated Close() error = %v", err)
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

func TestStartServerInfoReporterRejectsUninitializedClientAndEmptyNodeID(t *testing.T) {
	if _, err := (&ConfigClient{}).StartServerInfoReporter(
		context.Background(),
		"node-a",
		60*time.Second,
		nil,
	); err == nil {
		t.Fatal("StartServerInfoReporter() error = nil with uninitialized client")
	}

	client := &ConfigClient{client: &clientv3.Client{}}
	if _, err := client.StartServerInfoReporter(context.Background(), "  ", 60*time.Second, nil); err == nil {
		t.Fatal("StartServerInfoReporter() error = nil with empty node ID")
	}
}

func TestServerInfoReporterRejectsEmptyLeaseAndGrantError(t *testing.T) {
	client := &fakeServerInfoLeaseClient{nextLeaseID: 0}
	reporter := newServerInfoReporter(client, "/apisix/data_plane/server_info/node-a", 60*time.Second)
	if err := reporter.Report(context.Background(), []byte(`{"id":"node-a"}`)); err == nil {
		t.Fatal("Report() error = nil with an empty lease ID")
	}

	client = &fakeServerInfoLeaseClient{nextLeaseID: 42, grantErr: errors.New("grant denied")}
	reporter = newServerInfoReporter(client, "/apisix/data_plane/server_info/node-a", 60*time.Second)
	if err := reporter.Report(context.Background(), []byte(`{"id":"node-a"}`)); err == nil {
		t.Fatal("Report() error = nil when granting fails")
	}

	client = &fakeServerInfoLeaseClient{nextLeaseID: 42, putErr: errors.New("put denied")}
	reporter = newServerInfoReporter(client, "/apisix/data_plane/server_info/node-a", 60*time.Second)
	if err := reporter.Report(context.Background(), []byte(`{"id":"node-a"}`)); err == nil {
		t.Fatal("Report() error = nil when putting fails")
	}
	if client.grantCount != 1 {
		t.Fatalf("Grant calls = %d, want one lease attempt before put failure", client.grantCount)
	}
}
