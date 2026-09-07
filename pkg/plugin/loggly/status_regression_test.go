package loggly

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTP302FailsDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{
		CustomerToken: "tok",
		Protocol:      "http",
		Host:          server.URL,
		Timeout:       1000,
	})
	err := p.sendHTTPBulk(context.Background(), []byte(`{"a":1}`), "tok")
	if err == nil {
		t.Fatalf("302 send error=%v, Go currently treats <400 as success; APISIX requires status 200", err)
	}
}
