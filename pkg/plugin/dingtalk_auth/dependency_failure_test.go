package dingtalk_auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type dingtalkDependencyTransport func(*http.Request) (*http.Response, error)

func (transport dingtalkDependencyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestDingTalkTokenDependencyFailureMatrix(t *testing.T) {
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"accessToken":`))
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
			name: "connect failure", endpoint: "http://dingtalk.invalid",
			transport: dingtalkDependencyTransport(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed")
			}),
		},
		{
			name: "timeout", endpoint: "http://dingtalk.invalid", timeout: 5 * time.Millisecond,
			transport: dingtalkDependencyTransport(func(request *http.Request) (*http.Response, error) {
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
				AppKey: "app-key", AppSecret: "app-secret", AccessTokenURL: test.endpoint,
				UserInfoURL: "http://dingtalk.invalid/userinfo", RedirectURI: "http://login.dingtalk.invalid",
				Secret: "12345678",
			})
			if test.transport != nil {
				p.client.Transport = test.transport
			}
			if test.timeout > 0 {
				p.client.Timeout = test.timeout
			}
			request := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/callback", nil)
			if _, err := p.fetchAccessToken(request); err == nil {
				t.Fatal("fetchAccessToken() error = nil for failed dependency")
			}
		})
	}
}
