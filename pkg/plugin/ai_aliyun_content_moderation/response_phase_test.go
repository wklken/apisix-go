package ai_aliyun_content_moderation

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
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
	r = ai_runtime.WithSelectedInstanceName(r, "openai")
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

func TestRunBufferedBodyFilterAnnotatesRiskyFinalPacket(t *testing.T) {
	vector := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_aliyun_phase_final_packet_safety_total"},
		[]string{"plugin", "phase", "outcome", "reason"},
	)
	previous := metrics.AISafetyOutcomes
	metrics.AISafetyOutcomes = vector
	t.Cleanup(func() { metrics.AISafetyOutcomes = previous })

	moderation := aliyunServer(
		t,
		`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"replacement should not be used"}]}}`,
		http.StatusOK,
	)
	defer moderation.Close()
	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true, StreamCheckMode: "final_packet", RiskLevelBar: "high",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
	))
	r.Header.Set("Content-Type", "application/json")
	r = apisixctx.WithRequestVars(r)
	r = ai_runtime.WithSelectedInstanceName(r, "openai")
	result := p.RunRequestPhase(httptest.NewRecorder(), r)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	if mode := p.SelectResponseMode(result.Request); mode != base.RequestResponseModeBounded {
		t.Fatalf("SelectResponseMode() = %v, want bounded", mode)
	}
	const body = "data: {\"choices\":[{\"delta\":{\"content\":\"unsafe answer\"}}]}\n\n" +
		"data: [DONE]\n\n"
	state := &base.ResponseState{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   []byte(body),
	}
	if err := p.RunBufferedBodyFilter(result.Request, state); err != nil {
		t.Fatalf("RunBufferedBodyFilter() error = %v", err)
	}
	if !strings.Contains(string(state.Body), "unsafe answer") ||
		!strings.Contains(string(state.Body), `"risk_level":"high"`) ||
		strings.Contains(string(state.Body), "replacement should not be used") {
		t.Fatalf("bounded final-packet response = %q, want annotated original SSE", state.Body)
	}
	if got := counterValue(t, vector.WithLabelValues(name, "response", "allow", "risk_threshold")); got != 1 {
		t.Fatalf("final-packet allow/risk_threshold outcomes = %v, want 1", got)
	}
	if got := counterValue(t, vector.WithLabelValues(name, "response", "deny", "risk_threshold")); got != 0 {
		t.Fatalf("final-packet deny/risk_threshold outcomes = %v, want 0", got)
	}
}
