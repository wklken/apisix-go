package openwhisk

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParityContinuousResponseCrossesTotalTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"body":"`))
		w.(http.Flusher).Flush()
		for range 6 {
			time.Sleep(50 * time.Millisecond)
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write([]byte(`"}`))
	}))
	defer server.Close()
	p := newTestPlugin(
		t,
		Config{APIHost: server.URL, ServiceToken: "test:test", Namespace: "guest", Action: "action", Timeout: 200},
	)
	defer p.Stop()
	rr := performRequest(p, "{}")
	t.Logf("observed status=%d body=%q", rr.Code, rr.Body.String())
	if rr.Code != 200 {
		t.Errorf("APISIX per-I/O 200ms timeout should allow 50ms progress, got %d", rr.Code)
	}
}

func TestProgressTimeoutStillCancelsStalledResponse(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"body":"`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	p := newTestPlugin(
		t,
		Config{APIHost: server.URL, ServiceToken: "test:test", Namespace: "guest", Action: "action", Timeout: 75},
	)
	defer p.Stop()
	response := performRequest(p, "{}")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("stalled upstream was not canceled")
	}
}
