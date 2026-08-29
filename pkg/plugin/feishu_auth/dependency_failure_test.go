package feishu_auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type feishuDependencyTransport func(*http.Request) (*http.Response, error)

func (transport feishuDependencyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestFeishuTokenDependencyFailureMatrix(t *testing.T) {
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":`))
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
			name: "connect failure", endpoint: "http://feishu.invalid",
			transport: feishuDependencyTransport(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed")
			}),
		},
		{
			name: "timeout", endpoint: "http://feishu.invalid", timeout: 5 * time.Millisecond,
			transport: feishuDependencyTransport(func(request *http.Request) (*http.Response, error) {
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
				AppID: "app-id", AppSecret: "app-secret", AccessTokenURL: test.endpoint,
				UserInfoURL: "http://feishu.invalid/userinfo", AuthRedirectURI: "http://gateway.example.com/callback",
				RedirectURI: "http://login.feishu.invalid", Secret: "12345678",
			})
			if test.transport != nil {
				p.client.Transport = test.transport
			}
			if test.timeout > 0 {
				p.client.Timeout = test.timeout
			}
			if _, err := p.fetchAccessToken(
				httptest.NewRequest(http.MethodGet, "http://gateway.example.com/callback", nil), "code-a",
			); err == nil {
				t.Fatal("fetchAccessToken() error = nil for failed dependency")
			}
		})
	}
}
