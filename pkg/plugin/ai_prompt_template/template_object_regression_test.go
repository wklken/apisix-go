package ai_prompt_template

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestTemplatePreservesExtraFieldsAndEscapesSubstitutions(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal(
		[]byte(
			`{"templates":[{"name":"extra","template":{"model":"m","temperature":0.5,"messages":[{"role":"user","content":"Say {{prompt}}","name":"{{speaker}}"}],"options":{"user":"{{speaker}}","stream":false},"{{key}}":"value"}}]}`,
		),
		&cfg,
	); err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(t, cfg)
	request := httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"template_name":"extra","prompt":"say \"hi\" & <tag> / '","speaker":"Ada","key":"custom"}`),
	)
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got["temperature"] != 0.5 || got["custom"] != "value" {
			t.Fatalf("extra fields missing: %s", body)
		}
		message := got["messages"].([]any)[0].(map[string]any)
		if message["content"] != "Say say &quot;hi&quot; &amp; &lt;tag&gt; &#47; &#39;" || message["name"] != "Ada" {
			t.Fatalf("message=%#v", message)
		}
		options := got["options"].(map[string]any)
		if options["user"] != "Ada" || options["stream"] != false {
			t.Fatalf("options=%#v", options)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestNumericTemplateNameIsUnconfigured(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"templates":[{"name":"1","template":{"model":"m"}}]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(t, cfg)
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("numeric name must not match string name") })).
		ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"template_name":1}`)))
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 400 || body["message"] != "template: 1 not configured." {
		t.Fatalf("status=%d body=%#v", response.Code, body)
	}
}
