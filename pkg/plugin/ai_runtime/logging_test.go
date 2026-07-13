package ai_runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

func TestRegisterLoggingBuildsSummaryAndPayloads(t *testing.T) {
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	apisixctx.RegisterRequestVar(req, "$request_llm_model", "request-model")
	apisixctx.RegisterRequestVar(req, "$llm_model", "response-model")
	apisixctx.RegisterRequestVar(req, "$llm_time_to_first_token", int64(12))
	apisixctx.RegisterRequestVar(req, "$llm_prompt_tokens", int64(3))
	apisixctx.RegisterRequestVar(req, "$llm_completion_tokens", int64(4))
	apisixctx.RegisterRequestVar(req, "$apisix_upstream_response_time", int64(20))
	apisixctx.RegisterRequestVar(req, "$llm_response_text", "answer")

	RegisterLogging(req, true, true, ai_protocols.OpenAIChat, []byte(`{
	  "model":"request-model","stream":false,"messages":[{"role":"user","content":"question"}]
	}`))

	summary := apisixctx.GetRequestVar(req, "$llm_summary").(map[string]any)
	if summary["request_model"] != "request-model" || summary["model"] != "response-model" ||
		summary["duration"] != int64(12) || summary["upstream_response_time"] != int64(20) {
		t.Fatalf("$llm_summary = %#v", summary)
	}
	request := apisixctx.GetRequestVar(req, "$llm_request").(map[string]any)
	if request["stream"] != false || len(request["messages"].([]any)) != 1 {
		t.Fatalf("$llm_request = %#v", request)
	}
	response := apisixctx.GetRequestVar(req, "$llm_response").(map[string]any)
	if response["content"] != "answer" {
		t.Fatalf("$llm_response = %#v", response)
	}
	if got := apisixlog.GetField(req, "$llm_summary"); got == "" {
		t.Fatal("logger variable lookup did not expose $llm_summary")
	}
}
