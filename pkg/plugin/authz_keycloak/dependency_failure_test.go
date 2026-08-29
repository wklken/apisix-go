package authz_keycloak

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKeycloakDiscoveryDependencyFailureMatrix(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "rejection", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}},
		{name: "malformed response", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"token_endpoint":`))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			t.Cleanup(server.Close)
			p := newTestPlugin(t, Config{
				Discovery: server.URL, ClientID: "apisix", CacheTTLSeconds: 60,
			})
			if _, err := p.discover(); err == nil {
				t.Fatal("discover() error = nil for failed dependency")
			}
		})
	}
}

func TestKeycloakDiscoveryTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{
		Discovery: server.URL, ClientID: "apisix", CacheTTLSeconds: 60, Timeout: 5,
	})
	if _, err := p.discover(); err == nil {
		t.Fatal("discover() error = nil after configured timeout")
	}
}
