package forward_auth

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParityGenerationCleanupClosesIdleAuthConnection(t *testing.T) {
	idle, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	auth := httptest.NewUnstartedServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	)
	auth.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateIdle:
			idle <- struct{}{}
		case http.StateClosed:
			closed <- struct{}{}
		}
	}
	auth.Start()
	defer auth.Close()
	p := newTestPlugin(t, Config{URI: auth.URL})
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).
		ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	if response.Code != 204 {
		t.Fatalf("authentication status %d", response.Code)
	}
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("auth connection was not retained idle")
	}
	stop, ok := any(p).(interface{ Stop() })
	if !ok {
		t.Fatal("idle connection has no generation cleanup owner")
	}
	stop.Stop()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("retired plugin retained idle connection")
	}
	stop.Stop()
}
