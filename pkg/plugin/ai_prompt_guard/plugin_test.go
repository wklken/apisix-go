package ai_prompt_guard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("response code = %d, want %d", rr.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Fatalf("response body = %q, want %q", rr.Body.String(), tt.wantBody)
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

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
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

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"Request contains prohibited content"}` {
				t.Fatalf("response body = %q, want APISIX message body", got)
			}
		})
	}
}

func TestHandlerLeavesPassthroughRequestUnchecked(t *testing.T) {
	p := newTestPlugin(t, Config{DenyPatterns: []string{`secret`}})
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"prompt":"secret"}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerRejectsInvalidJSONBody(t *testing.T) {
	p := newTestPlugin(t, Config{DenyPatterns: []string{`secret`}})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`not-json`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for invalid JSON")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
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
