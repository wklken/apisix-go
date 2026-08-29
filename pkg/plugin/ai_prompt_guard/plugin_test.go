package ai_prompt_guard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
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

func TestPostInitDefaultsFailModeToError(t *testing.T) {
	p := newTestPlugin(t, Config{})
	if p.config.FailMode != string(ai_common.SafetyFailError) {
		t.Fatalf("config.FailMode = %q, want %q", p.config.FailMode, ai_common.SafetyFailError)
	}
	if p.failMode != ai_common.SafetyFailError {
		t.Fatalf("failMode = %q, want %q", p.failMode, ai_common.SafetyFailError)
	}
}

func TestHandlerAppliesFailModeToUninspectableRequest(t *testing.T) {
	const body = `not-json`
	tests := []struct {
		name        string
		failMode    string
		path        string
		body        string
		wantStatus  int
		wantNext    int
		wantBody    string
		wantMessage string
	}{
		{
			name:        "invalid JSON default error",
			body:        body,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "Request body is not valid JSON",
		},
		{
			name:       "invalid JSON warn",
			failMode:   "warn",
			body:       body,
			wantStatus: http.StatusNoContent,
			wantNext:   1,
			wantBody:   body,
		},
		{
			name:       "invalid JSON skip",
			failMode:   "skip",
			body:       body,
			wantStatus: http.StatusNoContent,
			wantNext:   1,
			wantBody:   body,
		},
		{
			name:        "passthrough default error",
			path:        "/anything",
			body:        `{"prompt":"hello"}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "Request format not recognized by ai-prompt-guard",
		},
		{
			name:       "passthrough warn restores body",
			failMode:   "warn",
			path:       "/anything",
			body:       `{"prompt":"hello"}`,
			wantStatus: http.StatusNoContent,
			wantNext:   1,
			wantBody:   `{"prompt":"hello"}`,
		},
		{
			name:        "empty recognized messages default error",
			path:        "/v1/chat/completions",
			body:        `{"messages":[]}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "No inspectable AI prompt content",
		},
		{
			name:       "filtered history skip restores body",
			failMode:   "skip",
			path:       "/v1/chat/completions",
			body:       `{"messages":[{"role":"assistant","content":"assistant only"}]}`,
			wantStatus: http.StatusNoContent,
			wantNext:   1,
			wantBody:   `{"messages":[{"role":"assistant","content":"assistant only"}]}`,
		},
		{
			name:       "empty joined content warn",
			failMode:   "warn",
			path:       "/v1/chat/completions",
			body:       `{"messages":[{"role":"user","content":""}]}`,
			wantStatus: http.StatusNoContent,
			wantNext:   1,
			wantBody:   `{"messages":[{"role":"user","content":""}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{FailMode: tt.failMode})
			path := tt.path
			if path == "" {
				path = "/anything"
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(tt.body))
			rr := httptest.NewRecorder()
			nextCalls := 0
			var gotBody string
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalls++
				content, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read downstream body: %v", err)
				}
				gotBody = string(content)
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("response code = %d, want %d", rr.Code, tt.wantStatus)
			}
			if nextCalls != tt.wantNext {
				t.Fatalf("next calls = %d, want %d", nextCalls, tt.wantNext)
			}
			if tt.wantBody != "" && gotBody != tt.wantBody {
				t.Fatalf("downstream body = %q, want %q", gotBody, tt.wantBody)
			}
			if tt.wantMessage != "" && !strings.Contains(rr.Body.String(), tt.wantMessage) {
				t.Fatalf("response body = %q, want message %q", rr.Body.String(), tt.wantMessage)
			}
		})
	}
}

func TestHandlerRecordsPromptGuardAllowAndDenyOutcomes(t *testing.T) {
	vector := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_ai_prompt_guard_outcomes_total"},
		[]string{"plugin", "phase", "outcome", "reason"},
	)
	previous := metrics.AISafetyOutcomes
	metrics.AISafetyOutcomes = vector
	t.Cleanup(func() { metrics.AISafetyOutcomes = previous })

	tests := []struct {
		name   string
		config Config
		path   string
		body   string
		status int
		labels [4]string
	}{
		{
			name:   "clean",
			body:   chatBody("safe"),
			status: http.StatusNoContent,
			labels: [4]string{"ai-prompt-guard", "request", "allow", "clean"},
		},
		{
			name:   "allow pattern miss",
			config: Config{AllowPatterns: []string{`^allowed$`}},
			body:   chatBody("blocked"),
			status: http.StatusBadRequest,
			labels: [4]string{"ai-prompt-guard", "request", "deny", "allow_pattern_miss"},
		},
		{
			name:   "deny pattern match",
			config: Config{DenyPatterns: []string{`secret`}},
			body:   chatBody("secret"),
			status: http.StatusBadRequest,
			labels: [4]string{"ai-prompt-guard", "request", "deny", "deny_pattern_match"},
		},
		{
			name:   "invalid JSON error",
			body:   `not-json`,
			status: http.StatusBadRequest,
			labels: [4]string{"ai-prompt-guard", "request", "error", "invalid_payload"},
		},
		{
			name:   "passthrough warn degraded",
			config: Config{FailMode: "warn"},
			path:   "/anything",
			body:   `{"prompt":"hello"}`,
			status: http.StatusNoContent,
			labels: [4]string{"ai-prompt-guard", "request", "degraded", "unknown_protocol"},
		},
		{
			name:   "empty content skip degraded",
			config: Config{FailMode: "skip"},
			path:   "/v1/chat/completions",
			body:   `{"messages":[]}`,
			status: http.StatusNoContent,
			labels: [4]string{"ai-prompt-guard", "request", "degraded", "empty_content"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, tt.config)
			rr := httptest.NewRecorder()
			path := tt.path
			if path == "" {
				path = "/v1/chat/completions"
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(tt.body))
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)
			if rr.Code != tt.status {
				t.Fatalf("response code = %d, want %d", rr.Code, tt.status)
			}
			if got := promptGuardCounterValue(t, vector.WithLabelValues(tt.labels[:]...)); got != 1 {
				t.Fatalf("metric %v = %v, want 1", tt.labels, got)
			}
		})
	}
}

func promptGuardCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("write counter metric: %v", err)
	}
	return metric.GetCounter().GetValue()
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

func TestHandlerDefaultsToUserRoleAndAllowsSystemOnlyRequest(t *testing.T) {
	p := newTestPlugin(t, Config{AllowPatterns: []string{`goodword`}})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"system","content":"badword"}]}`),
	)
	rr := httptest.NewRecorder()
	nextCalls := 0

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
	if nextCalls != 1 {
		t.Fatalf("next calls = %d, want 1", nextCalls)
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
			if got := rr.Body.String(); got != "{\"message\":\"Request contains prohibited content\"}\n" {
				t.Fatalf("response body = %q, want APISIX 3.17 message body", got)
			}
			if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want APISIX 3.17 response type", got)
			}
		})
	}
}

func TestHandlerRejectsUnrecognizedJSONWhenFailModeIsError(t *testing.T) {
	p := newTestPlugin(t, Config{DenyPatterns: []string{`secret`}, FailMode: "error"})
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"foo":"bar"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for unrecognized JSON in error mode")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if got := strings.TrimSpace(
		rr.Body.String(),
	); got != `{"message":"Request format not recognized by ai-prompt-guard"}` {
		t.Fatalf("response body = %q, want fail_mode error message", got)
	}
}

func TestHandlerDeniesStructuredResponsesInputText(t *testing.T) {
	p := newTestPlugin(t, Config{MatchAllRoles: true, DenyPatterns: []string{`secret`}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"secret"}]}]
	}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for denied structured Responses input")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
}

func TestHandlerAppliesFailModeToInvalidJSONBody(t *testing.T) {
	tests := []struct {
		name       string
		failMode   string
		wantStatus int
		wantNext   bool
	}{
		{name: "default error", wantStatus: http.StatusBadRequest},
		{name: "warn", failMode: "warn", wantStatus: http.StatusNoContent, wantNext: true},
		{name: "error", failMode: "error", wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{DenyPatterns: []string{`secret`}, FailMode: tt.failMode})
			req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`not-json`))
			rr := httptest.NewRecorder()
			nextCalled := false

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("response code = %d, want %d", rr.Code, tt.wantStatus)
			}
			if nextCalled != tt.wantNext {
				t.Fatalf("next called = %v, want %v", nextCalled, tt.wantNext)
			}
		})
	}
}

func TestHandlerLeavesUnsupportedJSONUncheckedInWarnMode(t *testing.T) {
	p := newTestPlugin(t, Config{DenyPatterns: []string{`secret`}, FailMode: "warn"})
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"foo":"bar"}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
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
