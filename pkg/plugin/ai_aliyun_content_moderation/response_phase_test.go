package ai_aliyun_content_moderation

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestRunBufferedBodyFilterPreservesOneSafeResponse(t *testing.T) {
	moderation := aliyunServer(t, `{"Data":{"RiskLevel":"low"}}`, http.StatusOK)
	defer moderation.Close()
	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt","messages":[{"role":"user","content":"hello"}]}`,
	))
	r.Header.Set("Content-Type", "application/json")
	r = apisixctx.WithRequestVars(r)
	result := p.RunRequestPhase(httptest.NewRecorder(), r)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	const body = `{"choices":[{"message":{"content":"safe answer"}}]}`
	state := &base.ResponseState{
		Status: http.StatusCreated,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(body),
	}
	if err := p.RunBufferedBodyFilter(result.Request, state); err != nil {
		t.Fatalf("RunBufferedBodyFilter() error = %v", err)
	}
	if state.Status != http.StatusCreated || string(state.Body) != body {
		t.Fatalf("filtered response = (%d, %q), want one unchanged safe response", state.Status, state.Body)
	}
}
