package ai_rate_limiting

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestAPISIX317AIRateLimitSharesLimitCountStateAcrossInstances(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	state := limitbase.NewStateWithClock(func() time.Time { return now })
	newPlugin := func() *Plugin {
		plugin := &Plugin{config: Config{Limit: 3, TimeWindow: 60}, now: func() time.Time { return now }}
		plugin.SetRateLimitState(state)
		if err := plugin.Init(); err != nil {
			t.Fatal(err)
		}
		if err := plugin.PostInit(); err != nil {
			t.Fatal(err)
		}
		return plugin
	}
	first := newPlugin()
	second := newPlugin()
	quota := quota{key: "shared", limit: 3, window: time.Minute}

	first.reconcile(quota, 3, true)
	if second.reserve(quota, 1) {
		t.Fatal("replacement plugin admitted a request after the shared quota was exhausted")
	}
}

func TestAPISIX317AIRouteQuotaIsSharedByAuthenticatedConsumers(t *testing.T) {
	plugin := newTestPlugin(t, Config{Limit: 3, TimeWindow: 60}, time.Now)
	if err := plugin.SetAPISIXPluginContext(base.APISIXPluginContext{
		SourceResourceKey: "/apisix/routes/ai", SourceKind: "route", SourceID: "ai",
		SourceConfig: map[string]any{"limit": 3, "time_window": 60},
	}); err != nil {
		t.Fatal(err)
	}
	requestFor := func(username string) *http.Request {
		request := apisixctx.WithApisixVars(
			httptest.NewRequest(http.MethodPost, "http://gateway.test/ai", nil),
			nil,
		)
		apisixctx.AttachConsumer(request, resource.Consumer{Username: username})
		return request
	}

	jack, ok, err := plugin.quotaForRequest(requestFor("jack"))
	if err != nil || !ok {
		t.Fatalf("jack quota = %#v, %t, %v", jack, ok, err)
	}
	jill, ok, err := plugin.quotaForRequest(requestFor("jill"))
	if err != nil || !ok {
		t.Fatalf("jill quota = %#v, %t, %v", jill, ok, err)
	}
	if jack.key != jill.key || !strings.HasPrefix(jack.key, "/apisix/routes/ai:") ||
		!strings.HasSuffix(jack.key, ":ai-rate-limiting#global:ai-rate-limiting#global") {
		t.Fatalf("consumer route quota keys = %q/%q", jack.key, jill.key)
	}
}

func TestAPISIX317AIAccessCheckDoesNotConsumeQuota(t *testing.T) {
	plugin := newTestPlugin(t, Config{Limit: 1, TimeWindow: 60}, time.Now)
	quota := quota{key: "dry-run", limit: 1, window: time.Minute}

	firstAllowed := plugin.reserve(quota, 1)
	secondAllowed := plugin.reserve(quota, 1)
	if !firstAllowed || !secondAllowed {
		t.Fatal("access dry-run consumed token quota before response accounting")
	}
	plugin.reconcile(quota, 1, true)
	if plugin.reserve(quota, 1) {
		t.Fatal("access dry-run admitted an exhausted quota")
	}
}
