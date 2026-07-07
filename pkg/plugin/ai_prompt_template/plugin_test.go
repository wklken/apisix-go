package ai_prompt_template

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerRendersSelectedPromptTemplate(t *testing.T) {
	p := newTestPlugin(t, Config{
		Templates: []NamedTemplate{
			{
				Name: "QnA with complexity",
				Template: Template{
					Model: "gpt-4",
					Messages: []Message{
						{Role: "system", Content: "Answer in {{complexity}}."},
						{Role: "user", Content: "Explain {{prompt}}."},
					},
				},
			},
			{
				Name: "echo",
				Template: Template{
					Model: "gpt-4",
					Messages: []Message{
						{Role: "user", Content: "Echo {{prompt}}."},
					},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/openai-chat", strings.NewReader(`{
	  "template_name": "QnA with complexity",
	  "complexity": "brief",
	  "prompt": "quick sort"
	}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Template
		if err := json.Unmarshal(readTestBody(t, r), &got); err != nil {
			t.Fatalf("decode rendered template: %v", err)
		}
		if got.Model != "gpt-4" {
			t.Fatalf("model = %q, want gpt-4", got.Model)
		}
		wantMessages := []Message{
			{Role: "system", Content: "Answer in brief."},
			{Role: "user", Content: "Explain quick sort."},
		}
		if !messagesEqual(got.Messages, wantMessages) {
			t.Fatalf("messages = %#v, want %#v", got.Messages, wantMessages)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerRejectsMissingTemplateName(t *testing.T) {
	p := newTestPlugin(t, Config{
		Templates: []NamedTemplate{
			{Name: "echo", Template: Template{Messages: []Message{{Role: "user", Content: "Echo"}}}},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/openai-chat", strings.NewReader(`{"prompt":"hello"}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for missing template_name")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "template name is missing in request.") {
		t.Fatalf("response body = %q, want missing template message", rr.Body.String())
	}
}

func TestHandlerRejectsUnknownTemplateName(t *testing.T) {
	p := newTestPlugin(t, Config{
		Templates: []NamedTemplate{
			{Name: "echo", Template: Template{Messages: []Message{{Role: "user", Content: "Echo"}}}},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/openai-chat", strings.NewReader(`{"template_name":"missing"}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for unknown template")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "template: missing not configured.") {
		t.Fatalf("response body = %q, want unknown template message", rr.Body.String())
	}
}

func TestHandlerRejectsInvalidJSONBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Templates: []NamedTemplate{
			{Name: "echo", Template: Template{Messages: []Message{{Role: "user", Content: "Echo"}}}},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/openai-chat", strings.NewReader(`not-json`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for invalid JSON")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "could not parse JSON request body") {
		t.Fatalf("response body = %q, want parse error", rr.Body.String())
	}
}

func readTestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func messagesEqual(got []Message, want []Message) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
