package authz_casdoor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type casdoorDependencyTransport func(*http.Request) (*http.Response, error)

func (transport casdoorDependencyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestCasdoorOAuthDependencyFailureMatrix(t *testing.T) {
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":`))
	}))
	t.Cleanup(malformed.Close)

	tests := []struct {
		name      string
		endpoint  string
		transport http.RoundTripper
		timeout   time.Duration
	}{
		{
			name: "connect failure", endpoint: "http://casdoor.invalid",
			transport: casdoorDependencyTransport(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed")
			}),
		},
		{
			name: "timeout", endpoint: "http://casdoor.invalid", timeout: 5 * time.Millisecond,
			transport: casdoorDependencyTransport(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}),
		},
		{name: "malformed response", endpoint: malformed.URL},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				EndpointAddr: test.endpoint, ClientID: "client-a", ClientSecret: testClientSecret,
				CallbackURL: "http://gateway.example.com/callback",
			})
			if test.transport != nil {
				p.client.Transport = test.transport
			}
			if test.timeout > 0 {
				p.client.Timeout = test.timeout
			}

			request := httptest.NewRequestWithContext(
				context.Background(), http.MethodGet, "http://gateway.example.com/callback", nil,
			)
			p.lifecycleMu.RLock()
			_, _, err := p.fetchAccessTokenLocked(request, "code-a")
			p.lifecycleMu.RUnlock()
			if err == nil {
				t.Fatal("fetchAccessTokenLocked() error = nil for failed dependency")
			}
		})
	}
}
