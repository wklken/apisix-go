package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/plugin/request_id"
)

func TestMetadataRequestIDPreservesRequestAndHeaderCallbacks(t *testing.T) {
	target := &request_id.Plugin{}
	if err := target.Init(); err != nil {
		t.Fatal(err)
	}
	if err := target.PostInit(); err != nil {
		t.Fatal(err)
	}
	filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := newTestMetadataPlugin("request-id", target, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	requestPhase, ok := wrapper.(base.RequestPhasePlugin)
	if !ok {
		t.Fatalf("missing request callback: %T", wrapper)
	}
	headerPhase, ok := wrapper.(base.StreamingHeaderFilterPlugin)
	if !ok {
		t.Fatalf("missing header callback: %T", wrapper)
	}
	for _, enabled := range []string{"yes", "no"} {
		request := httptest.NewRequest(http.MethodGet, "/?enabled="+enabled, nil)
		request.Header.Set("X-Request-Id", "client-id")
		result := requestPhase.RunRequestPhase(httptest.NewRecorder(), request)
		if result.Request == nil {
			t.Fatal("missing continued request")
		}
		for _, existing := range []string{"", "upstream-id"} {
			state := base.StreamingResponseState{Header: make(http.Header)}
			if existing != "" {
				state.Header.Set("X-Request-Id", existing)
			}
			if err := headerPhase.RunStreamingHeaderFilter(result.Request, &state); err != nil {
				t.Fatal(err)
			}
			want := existing
			if want == "" && enabled == "yes" {
				want = "client-id"
			}
			if got := state.Header.Get("X-Request-Id"); got != want {
				t.Fatalf("enabled=%s header=%q want=%q", enabled, got, want)
			}
		}
	}
}
