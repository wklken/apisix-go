package skywalking_logger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParitySkyWalkingEndpointPathMatchesAPISIX317(t *testing.T) {
	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := newTestPlugin(t, Config{EndpointAddr: server.URL + "/collector", Timeout: 1})
	defer p.Stop()
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/orders"}}, 1); err != nil {
		t.Fatal(err)
	}
	if got := <-paths; got != "/v3/logs" {
		t.Fatalf("request path = %q, want APISIX 3.17 fixed /v3/logs", got)
	}
}
