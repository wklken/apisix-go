package variable

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestRequestVarsRegistersRateLimitingInfo(t *testing.T) {
	if _, ok := RequestVars["$rate_limiting_info"]; !ok {
		t.Fatal("$rate_limiting_info is not registered as a request variable")
	}
}

func TestGetRequestVarResolvesRegisteredValueAndAbsence(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://example.test/chat", nil)
	request = ctx.WithRequestVars(request)
	ctx.RegisterRequestVar(request, "$llm_model", "gpt-test")

	if got := GetRequestVar(request, "$llm_model"); got != "gpt-test" {
		t.Fatalf("$llm_model = %#v, want gpt-test", got)
	}
	if got := GetRequestVar(request, "$request_type"); got != nil {
		t.Fatalf("$request_type = %#v, want nil when absent", got)
	}
}
