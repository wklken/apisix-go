package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
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

func TestClusterConfigKeyIncludesCleartextHTTP2Mode(t *testing.T) {
	regular := testClusterConfig()
	h2c := testClusterConfig()
	h2c.HTTP2Cleartext = true

	regularKey, err := regular.Key()
	if err != nil {
		t.Fatal(err)
	}
	h2cKey, err := h2c.Key()
	if err != nil {
		t.Fatal(err)
	}
	if regularKey == h2cKey {
		t.Fatal("cleartext HTTP/2 mode did not change the cluster key")
	}
}

func TestClusterConfigKeyIncludesTLSClientCertificateFingerprint(t *testing.T) {
	base := testClusterConfig()
	base.Transport = (&TransportOptionBuilder{}).
		WithTLSClientCertificate(tls.Certificate{Certificate: [][]byte{[]byte("leaf-a")}}).
		Build()
	same := testClusterConfig()
	same.Transport = (&TransportOptionBuilder{}).
		WithTLSClientCertificate(tls.Certificate{Certificate: [][]byte{[]byte("leaf-a")}}).
		Build()
	rotated := testClusterConfig()
	rotated.Transport = (&TransportOptionBuilder{}).
		WithTLSClientCertificate(tls.Certificate{Certificate: [][]byte{[]byte("leaf-b")}}).
		Build()
	privateMaterialChanged := testClusterConfig()
	privateMaterialChanged.Transport = (&TransportOptionBuilder{}).
		WithTLSClientCertificate(tls.Certificate{
			Certificate: [][]byte{[]byte("leaf-a")},
			PrivateKey:  "rotated-private-material",
		}).
		Build()

	baseKey, err := base.Key()
	if err != nil {
		t.Fatal(err)
	}
	sameKey, err := same.Key()
	if err != nil {
		t.Fatal(err)
	}
	rotatedKey, err := rotated.Key()
	if err != nil {
		t.Fatal(err)
	}
	privateMaterialChangedKey, err := privateMaterialChanged.Key()
	if err != nil {
		t.Fatal(err)
	}
	if baseKey != sameKey {
		t.Fatal("identical client certificates produced different cluster keys")
	}
	if baseKey == rotatedKey {
		t.Fatal("rotated client certificate produced the same cluster key")
	}
	if baseKey != privateMaterialChangedKey {
		t.Fatal("private material changed the cluster key")
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

type controlledDuplexBody struct {
	closed chan struct{}
	once   sync.Once
}

func newControlledDuplexBody() *controlledDuplexBody {
	return &controlledDuplexBody{closed: make(chan struct{})}
}

func (b *controlledDuplexBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *controlledDuplexBody) Write(payload []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, io.ErrClosedPipe
	default:
		return len(payload), nil
	}
}

func (b *controlledDuplexBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestClusterUpgradeBodyPreservesDuplexAndAdmissionLifetime(t *testing.T) {
	firstBody := newControlledDuplexBody()
	secondBody := newControlledDuplexBody()
	var calls int
	base := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body := firstBody
		if calls > 1 {
			body = secondBody
		}
		return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: body, Request: request}, nil
	})
	config := testClusterConfig()
	config.SendTimeout = 0
	config.ReadTimeout = 0
	cluster, err := newClusterWithTransport(config, NopClusterObserver{}, base, func() {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	request := httptest.NewRequest(http.MethodGet, "http://orders/socket", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	first, err := cluster.RoundTripper().RoundTrip(request)
	if err != nil {
		t.Fatalf("first upgrade RoundTrip() error = %v", err)
	}
	if _, ok := first.Body.(io.ReadWriteCloser); !ok {
		t.Fatalf("upgrade body type = %T, want io.ReadWriteCloser", first.Body)
	}
	if _, err := first.Body.(io.Writer).Write([]byte("frame")); err != nil {
		t.Fatalf("upgrade body Write() error = %v", err)
	}
	if _, err := cluster.RoundTripper().RoundTrip(request); !errors.Is(err, ErrClusterOverloaded) {
		t.Fatalf("second upgrade RoundTrip() error = %v, want ErrClusterOverloaded", err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatalf("close first upgrade body: %v", err)
	}
	second, err := cluster.RoundTripper().RoundTrip(request)
	if err != nil {
		t.Fatalf("third upgrade RoundTrip() error = %v, want success after tunnel close", err)
	}
	_ = second.Body.Close()
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

func TestCleartextHTTP2ClusterPreservesAdmissionProgressTimeoutAndClose(t *testing.T) {
	hold := make(chan struct{})
	address := startClusterH2CServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/headers" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		switch r.URL.Path {
		case "/hold":
			<-hold
			_, _ = io.WriteString(w, "done")
		case "/stall":
			<-r.Context().Done()
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))

	config := testClusterConfig()
	config.Targets = map[string]int{"http://" + address: 1}
	config.HTTP2Cleartext = true
	config.ReadTimeout = 25 * time.Millisecond
	config.Transport = (&TransportOptionBuilder{}).
		WithDialTimeout(time.Second).
		WithResponseHeaderTimeout(25 * time.Millisecond).
		Build()
	cluster, err := newCluster(config, NopClusterObserver{})
	if err != nil {
		t.Fatalf("newCluster() error = %v", err)
	}
	t.Cleanup(cluster.Close)

	first, err := cluster.RoundTripper().RoundTrip(
		httptest.NewRequest(http.MethodGet, "http://"+address+"/hold", nil),
	)
	if err != nil {
		t.Fatalf("first h2c RoundTrip() error = %v", err)
	}
	_, err = cluster.RoundTripper().RoundTrip(
		httptest.NewRequest(http.MethodGet, "http://"+address+"/hold", nil),
	)
	if !errors.Is(err, ErrClusterOverloaded) {
		t.Fatalf("second h2c RoundTrip() error = %v, want ErrClusterOverloaded", err)
	}
	close(hold)
	_, _ = io.ReadAll(first.Body)
	_ = first.Body.Close()

	stalled, err := cluster.RoundTripper().RoundTrip(
		httptest.NewRequest(http.MethodGet, "http://"+address+"/stall", nil),
	)
	if err != nil {
		t.Fatalf("stalled h2c RoundTrip() error = %v", err)
	}
	_, err = stalled.Body.Read(make([]byte, 1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled h2c body read error = %v, want deadline exceeded", err)
	}
	_ = stalled.Body.Close()

	_, err = cluster.RoundTripper().RoundTrip(
		httptest.NewRequest(http.MethodGet, "http://"+address+"/headers", nil),
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled h2c headers error = %v, want deadline exceeded", err)
	}

	cluster.Close()
	if !cluster.Closed() {
		t.Fatal("h2c cluster did not close its transport owner")
	}
}

type recordingRetryObserver struct {
	NopClusterObserver
	mu      sync.Mutex
	results []string
}

func (observer *recordingRetryObserver) ObserveRetry(_ string, result string) {
	observer.mu.Lock()
	observer.results = append(observer.results, result)
	observer.mu.Unlock()
}

func (observer *recordingRetryObserver) retryResults() []string {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return append([]string(nil), observer.results...)
}

func TestCleartextHTTP2ClusterRetriesReplayableGRPCAndObservesOutcome(t *testing.T) {
	address := startClusterH2CServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	closedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closed target: %v", err)
	}
	closedAddress := closedListener.Addr().String()
	_ = closedListener.Close()

	observer := &recordingRetryObserver{}
	config := testClusterConfig()
	config.HTTP2Cleartext = true
	config.Retries = 1
	cluster, err := newCluster(config, observer)
	if err != nil {
		t.Fatalf("newCluster() error = %v", err)
	}
	t.Cleanup(cluster.Close)

	request, err := http.NewRequest(
		http.MethodPost,
		"http://"+closedAddress+"/echo",
		strings.NewReader("frame"),
	)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Content-Type", "application/grpc")
	request = WithRetries(request, 1, func(retry *http.Request) bool {
		retry.URL.Host = address
		return true
	})
	response, err := cluster.RoundTripper().RoundTrip(request)
	if err != nil {
		t.Fatalf("h2c retry RoundTrip() error = %v", err)
	}
	_ = response.Body.Close()
	if got := observer.retryResults(); len(got) != 1 || got[0] != "success" {
		t.Fatalf("retry observer results = %v, want [success]", got)
	}
}

func startClusterH2CServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen h2c: %v", err)
	}
	var connectionsMu sync.Mutex
	connections := make([]net.Conn, 0, 1)
	t.Cleanup(func() {
		_ = listener.Close()
		connectionsMu.Lock()
		defer connectionsMu.Unlock()
		for _, connection := range connections {
			_ = connection.Close()
		}
	})
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connectionsMu.Lock()
			connections = append(connections, connection)
			connectionsMu.Unlock()
			go (&http2.Server{}).ServeConn(connection, &http2.ServeConnOpts{Handler: handler})
		}
	}()
	return listener.Addr().String()
}
