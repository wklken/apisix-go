package ai_prompt_decorator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestHandlerDecoratesOpenAIChatMessages(t *testing.T) {
	p := newTestPlugin(t, Config{
		Prepend: []Message{{Role: "system", Content: "answer briefly"}},
		Append:  []Message{{Role: "user", Content: "end with analogy"}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model": "gpt-4",
	  "messages": [{"role":"user","content":"What is mTLS?"}]
	}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readTestBody(t, r)
		var got struct {
			Model    string    `json:"model"`
			Messages []Message `json:"messages"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode rewritten body: %v", err)
		}
		if got.Model != "gpt-4" {
			t.Fatalf("model = %q, want gpt-4", got.Model)
		}
		want := []Message{
			{Role: "system", Content: "answer briefly"},
			{Role: "user", Content: "What is mTLS?"},
			{Role: "user", Content: "end with analogy"},
		}
		if !messagesEqual(got.Messages, want) {
			t.Fatalf("messages = %#v, want %#v", got.Messages, want)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerDecoratesOpenAIResponsesRequest(t *testing.T) {
	p := newTestPlugin(t, Config{
		Prepend: []Message{{Role: "system", Content: "policy"}},
		Append:  []Message{{Role: "user", Content: "footer"}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
	  "model": "gpt-4.1",
	  "instructions": "existing",
	  "input": "hello"
	}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readTestBody(t, r)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode rewritten body: %v", err)
		}
		if got["instructions"] != "policy\nexisting" {
			t.Fatalf("instructions = %q, want policy plus existing", got["instructions"])
		}
		if got["input"] != "hello\nfooter" {
			t.Fatalf("input = %q, want appended footer", got["input"])
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
}

func TestHandlerDecoratesAnthropicRequest(t *testing.T) {
	p := newTestPlugin(t, Config{
		Prepend: []Message{{Role: "system", Content: "policy"}},
		Append:  []Message{{Role: "user", Content: "footer"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
      "system":"existing",
      "messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
    }`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.Unmarshal(readTestBody(t, r), &got); err != nil {
			t.Fatalf("decode rewritten body: %v", err)
		}
		messages := got["messages"].([]any)
		if got["system"] != "existing" || messages[0].(map[string]any)["role"] != "system" ||
			messages[2].(map[string]any)["content"] != "footer" {
			t.Fatalf("anthropic body = %#v", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerDecoratesBedrockConverseRequest(t *testing.T) {
	p := newTestPlugin(t, Config{
		Prepend: []Message{{Role: "system", Content: "policy"}},
		Append:  []Message{{Role: "user", Content: "footer"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/model/x/converse", strings.NewReader(`{
      "system":[{"text":"existing"}],
      "messages":[{"role":"user","content":[{"text":"hello"}]}]
    }`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.Unmarshal(readTestBody(t, r), &got); err != nil {
			t.Fatalf("decode rewritten body: %v", err)
		}
		system := got["system"].([]any)
		messages := got["messages"].([]any)
		if system[0].(map[string]any)["text"] != "policy" ||
			messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "footer" {
			t.Fatalf("bedrock body = %#v", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerLeavesEmbeddingsAndPassthroughRequestsUnchanged(t *testing.T) {
	p := newTestPlugin(t, Config{Prepend: []Message{{Role: "system", Content: "policy"}}})
	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "embeddings", path: "/v1/embeddings", body: `{"input":"hello"}`},
		{name: "passthrough", path: "/anything", body: `{"prompt":"hello"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got map[string]any
				if err := json.Unmarshal(readTestBody(t, r), &got); err != nil {
					t.Fatalf("decode rewritten body: %v", err)
				}
				var want map[string]any
				if err := json.Unmarshal([]byte(tt.body), &want); err != nil {
					t.Fatalf("decode expected body: %v", err)
				}
				if !mapsEqual(got, want) {
					t.Fatalf("rewritten body = %#v, want %#v", got, want)
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("response code = %d, want 204", rr.Code)
			}
		})
	}
}

func mapsEqual(got, want map[string]any) bool {
	return reflect.DeepEqual(got, want)
}

func TestHandlerRejectsInvalidJSONBody(t *testing.T) {
	p := newTestPlugin(t, Config{Prepend: []Message{{Role: "system", Content: "policy"}}})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`not-json`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
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
