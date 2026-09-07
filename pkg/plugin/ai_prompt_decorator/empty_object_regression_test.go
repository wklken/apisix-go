package ai_prompt_decorator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

func TestEmptyObjectAcceptsPromptDecoration(t *testing.T) {
	p := newTestPlugin(t, Config{Prepend: []ai_protocols.Message{{Role: "system", Content: "only-prepend"}}})
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		messages := body["messages"].([]any)
		if len(messages) != 1 || messages[0].(map[string]any)["content"] != "only-prepend" {
			t.Fatalf("body=%#v", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
