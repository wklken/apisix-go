package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func requestAndDrain(t *testing.T, client *http.Client, url string, wantStatus int) {
	t.Helper()
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("drain %s: %v", url, err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("GET %s status = %d, want %d", url, response.StatusCode, wantStatus)
	}
}

func TestClusterRegistryReusesTransportAcrossUnrelatedReload(t *testing.T) {
	var acceptedConnections atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			acceptedConnections.Add(1)
		}
	}
	upstream.Start()
	t.Cleanup(upstream.Close)

	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/cluster.db", events, testutil.DataEncryptionService(false, nil))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previousStore)
		_ = storage.Stop()
	})

	put := func(bucket string, id string, value []byte) {
		event := store.NewEvent()
		event.Type = store.EventTypePut
		event.Key = []byte("/apisix/" + bucket + "/" + id)
		event.Value = value
		events <- event
	}
	put(
		"routes",
		"cluster-generation-one",
		[]byte(`{"id":"cluster-generation-one","uri":"/cluster-generation-one","upstream_id":"shared-upstream"}`),
	)
	put(
		"upstreams",
		"shared-upstream",
		fmt.Appendf(nil, `{"id":"shared-upstream","type":"roundrobin","nodes":{"%s":1}}`, upstream.Listener.Addr()),
	)
	if err := storage.Sync(); err != nil {
		t.Fatalf("initial storage sync: %v", err)
	}

	server := &Server{
		staticConfig:   &config.EffectiveConfig{},
		addr:           "127.0.0.1:9080",
		storage:        storage,
		dataEncryption: testutil.DataEncryptionService(false, nil),
		routes:         newRouteHandler(http.NotFoundHandler(), nil),
		clusters:       pxy.NewClusterRegistry(pxy.NopClusterObserver{}),
	}
	t.Cleanup(server.clusters.Close)
	t.Cleanup(func() { server.routes.Close() })

	gateway := httptest.NewServer(server.routes)
	t.Cleanup(gateway.Close)
	client := gateway.Client()

	firstBuilder := route.NewBuilderWithClusterRegistry(
		storage, server.addr, server.clusters, server.staticConfig, server.dataEncryption.Resolver(),
	)
	firstHandler, err := firstBuilder.BuildStrict()
	if err != nil {
		t.Fatalf("first BuildStrict() error = %v", err)
	}
	server.routes.Replace(firstHandler, firstBuilder.Stop)
	requestAndDrain(t, client, gateway.URL+"/cluster-generation-one", http.StatusOK)
	if got := acceptedConnections.Load(); got != 1 {
		t.Fatalf("connections after initial request = %d, want 1", got)
	}

	// An unrelated reload must reuse the shared cluster and its connection.
	put("routes", "unrelated-route", []byte(`{"id":"unrelated-route","uri":"/unrelated"}`))
	if err := storage.Sync(); err != nil {
		t.Fatalf("unrelated route storage sync: %v", err)
	}
	secondBuilder := route.NewBuilderWithClusterRegistry(
		storage, server.addr, server.clusters, server.staticConfig, server.dataEncryption.Resolver(),
	)
	secondHandler, err := secondBuilder.BuildStrict()
	if err != nil {
		t.Fatalf("second BuildStrict() error = %v", err)
	}
	server.routes.Replace(secondHandler, secondBuilder.Stop)
	requestAndDrain(t, client, gateway.URL+"/cluster-generation-one", http.StatusOK)
	if got := acceptedConnections.Load(); got != 1 {
		t.Fatalf("connections after unrelated reload = %d, want 1", got)
	}

	// Changing the shared upstream's read timeout changes the cluster config,
	// so a fresh connection must be established for the next request.
	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/upstreams/shared-upstream")
	event.Value = fmt.Appendf(
		nil,
		`{"id":"shared-upstream","type":"roundrobin","nodes":{"%s":1},"timeout":{"read":2}}`,
		upstream.Listener.Addr(),
	)
	events <- event
	if err := storage.Sync(); err != nil {
		t.Fatalf("upstream storage sync: %v", err)
	}

	thirdBuilder := route.NewBuilderWithClusterRegistry(
		storage, server.addr, server.clusters, server.staticConfig, server.dataEncryption.Resolver(),
	)
	thirdHandler, err := thirdBuilder.BuildStrict()
	if err != nil {
		t.Fatalf("third BuildStrict() error = %v", err)
	}
	server.routes.Replace(thirdHandler, thirdBuilder.Stop)
	requestAndDrain(t, client, gateway.URL+"/cluster-generation-one", http.StatusOK)
	if got := acceptedConnections.Load(); got != 2 {
		t.Fatalf("connections after transport-changing reload = %d, want 2", got)
	}
}
