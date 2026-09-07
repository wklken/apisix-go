package route

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
)

func TestRequestIDMetadataFilterDecisionSurvivesHeaderMutation(t *testing.T) {
	target := plugin.New("request-id", base.Dependencies{Config: &appconfig.EffectiveConfig{}})
	if err := target.Init(); err != nil {
		t.Fatal(err)
	}
	if err := target.PostInit(); err != nil {
		t.Fatal(err)
	}
	filter, err := pluginexpr.Compile([]any{[]any{"http_x_request_id", "==", ""}})
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := newTestMetadataPlugin("request-id", target, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	requestPhase, ok := wrapped.(base.RequestPhasePlugin)
	if !ok {
		t.Fatalf("wrapped request-id lost request phase: %T", wrapped)
	}
	headerPhase, ok := wrapped.(base.StreamingHeaderFilterPlugin)
	if !ok {
		t.Fatalf("wrapped request-id lost header phase: %T", wrapped)
	}

	request, _ := apisixctx.EnsureRequestLifecycle(httptest.NewRequest(http.MethodGet, "/", nil), time.Now())
	result := requestPhase.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Request.Header.Get("X-Request-Id") == "" {
		t.Fatal("request-id filter did not match request phase")
	}
	state := &base.StreamingResponseState{Header: make(http.Header)}
	if err := headerPhase.RunStreamingHeaderFilter(result.Request, state); err != nil {
		t.Fatal(err)
	}
	if state.Header.Get("X-Request-Id") == "" {
		t.Fatal(
			"header phase re-evaluated the now-mutated request header instead of reusing request-phase metadata filter match",
		)
	}
}
