package loggly

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHTTPRedirectIsDeliveryFailure(t *testing.T) {
	var followed atomic.Bool
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if r.URL.Path == "/accepted" {
			followed.Store(true)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, server.URL+"/accepted", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{CustomerToken: "tok", Protocol: "http", Host: server.URL, Timeout: 1000})
	t.Cleanup(p.Stop)
	if err := p.sendHTTPBulk(context.Background(), []byte(`{"a":1}`), "tok"); err == nil {
		t.Fatalf("302 redirect was followed=%v and reported as successful delivery", followed.Load())
	}
}
