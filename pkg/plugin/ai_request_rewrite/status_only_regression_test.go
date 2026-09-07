package ai_request_rewrite

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAzureMissingEndpointIsAdmittedAndFailsAtRequestTime(t *testing.T) {
	p := &Plugin{
		config: Config{
			Provider: "azure-openai",
			Prompt:   "rewrite",
			Auth:     Auth{Header: map[string]string{"api-key": "local"}},
		},
	}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit rejected route: %v", err)
	}
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next called") })).
		ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello")))
	if response.Code != 500 || response.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestMissingBodyReturnsStatusWithoutPluginMessage(t *testing.T) {
	p := newTestPlugin(t, Config{Provider: "openai", Prompt: "rewrite"})
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next called") })).
		ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", nil))
	if response.Code != 400 || response.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}
