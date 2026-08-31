package ai_prompt_guard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
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

func TestSchemaMatchesAPISIXPublicFields(t *testing.T) {
	var document struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema), &document); err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	want := []string{
		"match_all_roles",
		"match_all_conversation_history",
		"allow_patterns",
		"deny_patterns",
	}
	if len(document.Properties) != len(want) {
		t.Fatalf("schema properties = %v, want exactly %v", document.Properties, want)
	}
	for _, name := range want {
		if _, ok := document.Properties[name]; !ok {
			t.Errorf("schema is missing property %q", name)
		}
	}
}

func TestHandlerRejectsEmptyRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "/anything", http.NoBody)
	rr := httptest.NewRecorder()
	nextCalls := 0

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalls++
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"Empty request body"}` {
		t.Fatalf("response body = %q, want empty body message", got)
	}
	if nextCalls != 0 {
		t.Fatalf("next calls = %d, want 0", nextCalls)
	}
}

func TestHandlerReturnsJSONDecodeError(t *testing.T) {
	const body = " \n\t"
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(body))
	rr := httptest.NewRecorder()

	var decoded map[string]any
	decodeErr := json.Unmarshal([]byte(body), &decoded)
	if decodeErr == nil {
		t.Fatal("test body unexpectedly decoded as JSON")
	}

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for invalid JSON")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	wantBody, err := json.Marshal(map[string]string{"message": decodeErr.Error()})
	if err != nil {
		t.Fatalf("marshal expected body: %v", err)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != string(wantBody) {
		t.Fatalf("response body = %q, want actual decoder error %q", got, wantBody)
	}
}

func TestHandlerReturnsEmpty200WhenNoMessages(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "unknown protocol",
			path: "/anything",
			body: `{"prompt":"hello"}`,
		},
		{
			name: "detect error",
			path: "/anything",
			body: `{}`,
		},
		{
			name: "empty messages",
			path: "/v1/chat/completions",
			body: `{"messages":[]}`,
		},
		{
			name: "system only",
			path: "/v1/chat/completions",
			body: `{"messages":[{"role":"system","content":"system policy"}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{})
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			rr := httptest.NewRecorder()
			nextCalls := 0

			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				nextCalls++
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("response code = %d, want 200", rr.Code)
			}
			if rr.Body.Len() != 0 {
				t.Fatalf("response body = %q, want empty body", rr.Body.String())
			}
			if nextCalls != 0 {
				t.Fatalf("next calls = %d, want 0", nextCalls)
			}
		})
	}
}

func TestHandlerChecksEmptyContentPatterns(t *testing.T) {
	tests := []struct {
		name       string
		config     Config
		wantStatus int
		wantBody   string
		wantNext   int
	}{
		{
			name:       "allow empty content",
			config:     Config{AllowPatterns: []string{`^$`}},
			wantStatus: http.StatusNoContent,
			wantNext:   1,
		},
		{
			name:       "empty content without patterns",
			wantStatus: http.StatusNoContent,
			wantNext:   1,
		},
		{
			name:       "deny empty content",
			config:     Config{DenyPatterns: []string{`^$`}},
			wantStatus: http.StatusBadRequest,
			wantBody:   "Request contains prohibited content",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, tt.config)
			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(`{"messages":[{"role":"user","content":""}]}`),
			)
			rr := httptest.NewRecorder()
			nextCalls := 0

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				nextCalls++
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("response code = %d, want %d", rr.Code, tt.wantStatus)
			}
			if nextCalls != tt.wantNext {
				t.Fatalf("next calls = %d, want %d", nextCalls, tt.wantNext)
			}
			if tt.wantBody != "" && !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Fatalf("response body = %q, want message %q", rr.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestHandlerPreservesFinalEmptyContentAfterMultimodalMessage(t *testing.T) {
	p := newTestPlugin(t, Config{DenyPatterns: []string{`^$`}})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(
			`{"messages":[{"role":"user","content":[{"type":"text","text":"safe"}]},{"role":"user","content":""}]}`,
		),
	)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for denied empty final message")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Request contains prohibited content") {
		t.Fatalf("response body = %q, want deny message", rr.Body.String())
	}
}

func TestJoinContentPreservesEmptyMessages(t *testing.T) {
	messages := []ai_protocols.Message{
		{Role: "user", Content: "a"},
		{Role: "user", Content: ""},
		{Role: "user", Content: "b"},
	}
	if got, want := joinContent(messages), "a  b"; got != want {
		t.Fatalf("joinContent() = %q, want %q", got, want)
	}
}

func TestHandlerChecksAllowBeforeDenyPatterns(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowPatterns: []string{`\$?\d+(\.\d+)?`},
		DenyPatterns:  []string{`\d{3}-\d{3}-\d{4}`},
	})

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "allowed",
			body:       chatBody("John paid $12.5 for coffee."),
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "missing allow",
			body:       chatBody("John paid a bit for coffee."),
			wantStatus: http.StatusBadRequest,
			wantBody:   "Request doesn't match allow patterns",
		},
		{
			name:       "denied after allow",
			body:       chatBody("John 647-200-9393 paid $12.5 for coffee."),
			wantStatus: http.StatusBadRequest,
			wantBody:   "Request contains prohibited content",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(tt.body))
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("response code = %d, want %d", rr.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Fatalf("response body = %q, want message %q", rr.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestHandlerDefaultsToLastUserMessageOnly(t *testing.T) {
	p := newTestPlugin(t, Config{DenyPatterns: []string{`secret`}})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
  "messages": [
    {"role":"system","content":"secret system policy"},
    {"role":"user","content":"secret older message"},
    {"role":"user","content":"safe final question"}
  ]
}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
}

func TestHandlerDefaultsToUserRoleAndTerminatesSystemOnlyRequest(t *testing.T) {
	p := newTestPlugin(t, Config{AllowPatterns: []string{`goodword`}})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"system","content":"badword"}]}`),
	)
	rr := httptest.NewRecorder()
	nextCalls := 0

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("response body = %q, want empty body", rr.Body.String())
	}
	if nextCalls != 0 {
		t.Fatalf("next calls = %d, want 0", nextCalls)
	}
}

func TestHandlerCanCheckAllRolesAndHistory(t *testing.T) {
	p := newTestPlugin(t, Config{
		MatchAllRoles:               true,
		MatchAllConversationHistory: true,
		DenyPatterns:                []string{`secret`},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
  "messages": [
    {"role":"system","content":"secret system policy"},
    {"role":"user","content":"safe final question"}
  ]
}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for denied prompt")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Request contains prohibited content") {
		t.Fatalf("response body = %q, want deny message", rr.Body.String())
	}
}

func TestHandlerChecksResponsesInputWithoutLastMessageFiltering(t *testing.T) {
	p := newTestPlugin(t, Config{DenyPatterns: []string{`secret`}})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
  "instructions": "secret system policy",
  "input": ["safe first input", "secret user input"]
}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for denied Responses input")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
}

func TestHandlerDeniesExtractableProtocolsWithAPISIXMessage(t *testing.T) {
	p := newTestPlugin(t, Config{DenyPatterns: []string{`secret`}})
	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "chat",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"secret"}]}`,
		},
		{
			name: "anthropic",
			path: "/v1/messages",
			body: `{"model":"claude","messages":[{"role":"user","content":[{"type":"text","text":"secret"}]}]}`,
		},
		{
			name: "bedrock",
			path: "/model/x/converse",
			body: `{"messages":[{"role":"user","content":[{"text":"secret"}]}]}`,
		},
		{
			name: "embeddings",
			path: "/v1/embeddings",
			body: `{"input":"secret"}`,
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: `{"model":"gpt-4.1","instructions":"safe","input":["secret", "safe"]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next handler was called for denied prompt")
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("response code = %d, want 400", rr.Code)
			}
			if got := rr.Body.String(); got != "{\"message\":\"Request contains prohibited content\"}" {
				t.Fatalf("response body = %q, want APISIX 3.17 message body", got)
			}
			if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want APISIX 3.17 response type", got)
			}
		})
	}
}

func TestPostInitRejectsInvalidRegex(t *testing.T) {
	p := &Plugin{config: Config{AllowPatterns: []string{`[`}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid regex")
	}
}

func chatBody(content string) string {
	return `{"messages":[{"role":"system","content":"Rate purchases."},{"role":"user","content":"` + content + `"}]}`
}
