package cas_auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type casDependencyTransport func(*http.Request) (*http.Response, error)

func (transport casDependencyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestCASValidationDependencyFailureMatrix(t *testing.T) {
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<cas:serviceResponse><cas:authenticationSuccess>`))
	}))
	t.Cleanup(malformed.Close)
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(rejected.Close)

	tests := []struct {
		name      string
		endpoint  string
		transport http.RoundTripper
		timeout   time.Duration
	}{
		{
			name: "connect failure", endpoint: "http://cas.invalid",
			transport: casDependencyTransport(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed")
			}),
		},
		{
			name: "timeout", endpoint: "http://cas.invalid", timeout: 5 * time.Millisecond,
			transport: casDependencyTransport(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}),
		},
		{name: "malformed response", endpoint: malformed.URL},
		{name: "provider rejection", endpoint: rejected.URL},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				IDPURI: test.endpoint, CASCallbackURI: "http://gateway.example.com/callback",
				Cookie: CookieConfig{Secret: "01234567890123456789012345678901"},
			})
			if test.transport != nil {
				p.client.Transport = test.transport
			}
			if test.timeout > 0 {
				p.client.Timeout = test.timeout
			}
			user, err := p.validateTicket(
				httptest.NewRequest(http.MethodGet, "http://gateway.example.com/callback", nil),
				"ST-1",
			)
			if err == nil && user != "" {
				t.Fatalf("validateTicket() = %q, nil for failed dependency", user)
			}
		})
	}
}
