package ai_prompt_template

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParityExplicitEmptyTemplateNameMatchesAPISIX317(t *testing.T) {
	p := newTestPlugin(t, Config{Templates: []NamedTemplate{{
		Name: "echo", Template: Template{"messages": []Message{{Role: "user", Content: "Echo"}}},
	}}})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"template_name":""}`))
	response := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called")
	})).ServeHTTP(response, req)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if !strings.Contains(response.Body.String(), "template:  not configured.") {
		t.Fatalf("body = %q, want APISIX explicit-empty unknown-template response", response.Body.String())
	}
}
