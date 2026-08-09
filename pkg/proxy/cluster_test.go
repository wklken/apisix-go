package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClusterConfig() ClusterConfig {
	return ClusterConfig{
		Name:    "orders",
		Targets: map[string]int{"http://127.0.0.1:8080": 1},
		Transport: (&TransportOptionBuilder{}).
			WithDialTimeout(time.Second).
			Build(),
		MaxInFlight: 1,
	}
}

func TestClusterRegistryReusesIdenticalConfigUntilFinalRelease(t *testing.T) {
	registry := NewClusterRegistry(NopClusterObserver{})
	t.Cleanup(registry.Close)
	config := testClusterConfig()
	first, err := registry.Acquire(config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Acquire(config)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cluster() != second.Cluster() {
		t.Fatal("identical configs created different clusters")
	}
	first.Stop()
	if second.Cluster().Closed() {
		t.Fatal("first release closed a referenced cluster")
	}
	second.Stop()
	if !second.Cluster().Closed() {
		t.Fatal("final release did not close the cluster")
	}
}

func TestClusterConfigKeyChangesWhenTransportChanges(t *testing.T) {
	base := testClusterConfig()
	changed := testClusterConfig()
	changed.Transport = (&TransportOptionBuilder{}).
		WithDialTimeout(time.Second).
		WithResponseHeaderTimeout(2 * time.Second).
		Build()

	baseKey, err := base.Key()
	if err != nil {
		t.Fatal(err)
	}
	changedKey, err := changed.Key()
	if err != nil {
		t.Fatal(err)
	}
	if baseKey == changedKey {
		t.Fatal("changing response header timeout did not change the cluster key")
	}
}

// controlledBody lets a test hold a response body open until it chooses to
// close it, so admission-token lifetime is deterministic.
type controlledBody struct {
	closed chan struct{}
}

func newControlledBody() *controlledBody {
	return &controlledBody{closed: make(chan struct{})}
}

func (b *controlledBody) Read(p []byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *controlledBody) Close() error {
	close(b.closed)
	return nil
}

func TestClusterAdmissionRejectsOverloadUntilBodyCloses(t *testing.T) {
	firstBody := newControlledBody()
	isFirst := true
	base := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body := firstBody
		if !isFirst {
			body = newControlledBody()
		}
		isFirst = false
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       body,
			Request:    request,
		}, nil
	})

	config := testClusterConfig()
	cluster, err := newClusterWithTransport(config, NopClusterObserver{}, base, func() {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)

	first, err := cluster.RoundTripper().RoundTrip(httptest.NewRequest(http.MethodGet, "http://orders", nil))
	if err != nil {
		t.Fatalf("first RoundTrip() error = %v", err)
	}

	_, err = cluster.RoundTripper().RoundTrip(httptest.NewRequest(http.MethodGet, "http://orders", nil))
	if !errors.Is(err, ErrClusterOverloaded) {
		t.Fatalf("second RoundTrip() error = %v, want ErrClusterOverloaded", err)
	}

	if err := first.Body.Close(); err != nil {
		t.Fatalf("close first body: %v", err)
	}
	third, err := cluster.RoundTripper().RoundTrip(httptest.NewRequest(http.MethodGet, "http://orders", nil))
	if err != nil {
		t.Fatalf("third RoundTrip() error = %v, want success after the first body closed", err)
	}
	_ = third.Body.Close()
}

func TestClusterCloseIsIdempotent(t *testing.T) {
	closed := 0
	config := testClusterConfig()
	cluster, err := newClusterWithTransport(config, NopClusterObserver{}, roundTripperFunc(
		func(*http.Request) (*http.Response, error) {
			return nil, errors.New("not dialed")
		},
	), func() { closed++ })
	if err != nil {
		t.Fatal(err)
	}
	cluster.Close()
	cluster.Close()
	if closed != 1 {
		t.Fatalf("CloseIdleConnections callbacks = %d, want 1", closed)
	}
	if !cluster.Closed() {
		t.Fatal("cluster did not report closed")
	}
}
