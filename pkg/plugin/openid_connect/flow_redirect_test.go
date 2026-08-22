package openid_connect

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"golang.org/x/oauth2"
)

func TestBeginAuthorizationDoesNotMutateCachedRedirectURL(t *testing.T) {
	p := newTestPlugin(t, codeFlowConfig("https://idp.example"))
	p.provider = &providerClient{oauth2Config: oauth2.Config{
		Endpoint: oauth2.Endpoint{AuthURL: "https://idp.example/authorize"},
	}}

	recorder := httptest.NewRecorder()
	p.beginAuthorization(
		recorder,
		httptest.NewRequest(http.MethodGet, "https://gateway.example/orders", nil),
		"https://gateway.example/orders/.apisix/redirect",
		nil,
		"",
	)

	if got := p.provider.oauth2Config.RedirectURL; got != "" {
		t.Fatalf("cached oauth2 redirect URL = %q, want unchanged", got)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization location: %v", err)
	}
	if got, want := location.Query().Get("redirect_uri"),
		"https://gateway.example/orders/.apisix/redirect"; got != want {
		t.Fatalf("redirect_uri = %q, want %q", got, want)
	}
}

func TestExchangeCodeDoesNotMutateCachedRedirectURL(t *testing.T) {
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-token"}`))
	}))
	t.Cleanup(tokenEndpoint.Close)

	p := newTestPlugin(t, codeFlowConfig("https://idp.example"))
	p.provider = &providerClient{oauth2Config: oauth2.Config{
		ClientID:     "apisix",
		ClientSecret: "secret-a",
		Endpoint:     oauth2.Endpoint{TokenURL: tokenEndpoint.URL},
	}}

	if _, err := p.exchangeCode(
		httptest.NewRequest(http.MethodGet, "https://gateway.example/orders", nil),
		"code-a",
		"https://gateway.example/orders/.apisix/redirect",
		"",
	); err != nil {
		t.Fatalf("exchangeCode() error = %v", err)
	}
	if got := p.provider.oauth2Config.RedirectURL; got != "" {
		t.Fatalf("cached oauth2 redirect URL = %q, want unchanged", got)
	}
}
