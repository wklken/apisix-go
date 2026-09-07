package ai_request_rewrite

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatusOnlyLLMFailuresKeepDiagnosticLogs(t *testing.T) {
	for _, test := range []struct{ body, want string }{{"not-json", "failed to decode LLM response"}, {"{}", "failed to extract text from LLM response"}} {
		provider := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, test.body) }),
		)
		p := newTestPlugin(
			t,
			Config{
				Prompt:   "rewrite",
				Provider: "openai-compatible",
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer local"}},
				Override: Override{Endpoint: provider.URL},
			},
		)
		var messages []string
		p.logError = func(message string) { messages = append(messages, message) }
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("failed rewrite reached upstream") })).
			ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("original")))
		provider.Close()
		if response.Code != 500 || response.Body.Len() != 0 || len(messages) != 1 || messages[0] != test.want {
			t.Fatalf("status=%d body=%q logs=%v", response.Code, response.Body.String(), messages)
		}
	}
}

func TestStatusOnlyRequestBodyFailureKeepsDiagnostic(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{
			Prompt:   "rewrite",
			Provider: "openai-compatible",
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer local"}},
			Override: Override{Endpoint: "http://127.0.0.1:1"},
		},
	)
	var messages []string
	p.warn = func(message string) { messages = append(messages, message) }
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Body = failingRequestBody{}
	response := httptest.NewRecorder()
	p.RunRequestPhase(response, request)
	if response.Code != 400 || response.Body.Len() != 0 || len(messages) != 1 ||
		messages[0] != "failed to get request body" {
		t.Fatalf("status=%d body=%q logs=%v", response.Code, response.Body.String(), messages)
	}
}

type failingRequestBody struct{}

func (failingRequestBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingRequestBody) Close() error             { return nil }
