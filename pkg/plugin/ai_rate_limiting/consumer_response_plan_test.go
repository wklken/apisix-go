package ai_rate_limiting_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/generation"
	pluginpkg "github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/ai_rate_limiting"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func TestConsumerScopedPluginJoinsResponsePlanAfterAuthentication(t *testing.T) {
	rateLimiter := &ai_rate_limiting.Plugin{}
	config := rateLimiter.Config().(*ai_rate_limiting.Config)
	*config = ai_rate_limiting.Config{Limit: 30, TimeWindow: 60}
	if err := rateLimiter.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, cleanup := testutil.ScopedSecretHarness(
		t,
		"ai-rate-limiting",
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	defer cleanup()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, rateLimiter,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := rateLimiter.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	binding, err := pluginpkg.BindPluginChecked(
		"ai-rate-limiting",
		rateLimiter,
		pluginpkg.ScopeConsumer,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceConsumer, ID: "jack1"},
	)
	if err != nil {
		t.Fatalf("BindPluginChecked() error = %v", err)
	}
	plan, err := pluginpkg.BuildResponsePlan(pluginpkg.ResponsePlanInput{})
	if err != nil {
		t.Fatalf("BuildResponsePlan() error = %v", err)
	}
	pipeline := pluginpkg.NewRequestPipeline(nil, func(r *http.Request) (pluginpkg.ConsumerResolution, error) {
		return pluginpkg.ConsumerResolution{
			Request: r, Resolved: true, Bindings: []pluginpkg.Binding{binding},
		}, nil
	})
	terminalCalls := 0
	handler := plan.Install(pipeline, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		terminalCalls++
		apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":10}}`))
	}))

	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "/ai", nil),
		time.Now(),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || terminalCalls != 1 {
		t.Fatalf(
			"consumer response = status %d, terminal calls %d, want 200 and one terminal call",
			response.Code,
			terminalCalls,
		)
	}
}
