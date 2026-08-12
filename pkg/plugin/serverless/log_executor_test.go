package serverless_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/serverless"
)

func TestServerlessLogPhaseThroughExecutorCapturesFinalResponseBody(t *testing.T) {
	serverlessPlugin := serverless.NewPreFunction()
	if err := serverlessPlugin.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := serverlessPlugin.Config().(*serverless.Config)
	*config = serverless.Config{
		Phase: "log",
		Functions: []string{`return function()
			if ngx.status ~= 418 or ngx.arg[1] ~= "final-body" then
				error("executor response snapshot missing")
			end
		end`},
	}
	if err := serverlessPlugin.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	executor, err := plugin.NewLogExecutor([]plugin.LogBinding{{
		Plugin: serverlessPlugin,
		Scope:  plugin.ScopeRoute,
		Policy: serverlessPlugin.LogCapturePolicy(),
	}})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "http://example.com/log", strings.NewReader("request-body")),
		time.Now(),
	)
	wrapped, capture := base.CaptureResponseOutcomeController(httptest.NewRecorder())
	request = base.WithResponseCapture(request, capture)
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := executor.SealAndRegister(request); err != nil {
		t.Fatalf("SealAndRegister() error = %v", err)
	}
	wrapped.Header().Set("X-Final", "yes")
	wrapped.WriteHeader(http.StatusTeapot)
	_, _ = wrapped.Write([]byte("final-body"))
	lifecycle.Complete(capture.Outcome(), time.Now())
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
}
