package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	csrfplugin "github.com/wklken/apisix-go/pkg/plugin/csrf"
	proxycache "github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func TestCSRFAndCacheCompose(t *testing.T) {
	p := &csrfplugin.Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.Config().(*csrfplugin.Config).Key = "test-key"
	secrets, scope, closeAttempt := testutil.ScopedSecretHarness(
		t,
		"csrf",
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	t.Cleanup(closeAttempt)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	cache := &proxycache.Plugin{}
	if err := cache.Init(); err != nil {
		t.Fatal(err)
	}
	cfg := cache.Config().(*proxycache.Config)
	cfg.CacheStrategy = "memory"
	cfg.CacheZone = "review-cache"
	cache.SetConfiguredZones([]appconfig.Zone{{Name: "review-cache", MemorySize: "1M"}})
	if err := cache.PostInit(); err != nil {
		t.Fatal(err)
	}
	defer cache.Stop()
	bindings := []Binding{
		resolvedPlan16Binding(t, "csrf", p, "csrf-route"),
		resolvedPlan16Binding(t, "proxy-cache", cache, "cache-route"),
	}
	if _, err := BuildResponsePlan([]Binding{bindings[0]}); err != nil {
		t.Fatalf("csrf alone: %v", err)
	}
	if _, err := BuildResponsePlan([]Binding{bindings[1]}); err != nil {
		t.Fatalf("cache alone: %v", err)
	}
	plan, err := BuildResponsePlan(
		ResponsePlanInput{
			StaticBindings: bindings,
			BufferedConfig: base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	upstreamCalls := 0
	handler := plan.Install(
		NewRequestPipeline(bindings, nil),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamCalls++
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("cached"))
		}),
	)
	var lastCookie string
	for _, tt := range []struct {
		method, cache string
		status        int
	}{
		{http.MethodGet, "MISS", http.StatusOK},
		{http.MethodGet, "HIT", http.StatusOK},
		{http.MethodPost, "", http.StatusUnauthorized},
		{http.MethodGet, "HIT", http.StatusOK},
	} {
		request, _ := apisixctx.EnsureRequestLifecycle(
			httptest.NewRequest(tt.method, "http://gateway.test/cache", nil),
			time.Now(),
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tt.status || response.Header().Get("Apisix-Cache-Status") != tt.cache {
			t.Fatalf(
				"%s: status=%d cache=%q body=%q",
				tt.method,
				response.Code,
				response.Header().Get("Apisix-Cache-Status"),
				response.Body.String(),
			)
		}
		if tt.status == http.StatusOK && response.Body.String() != "cached" {
			t.Fatalf("body = %q", response.Body.String())
		}
		cookies := response.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("%s: cookies=%v, want exactly one CSRF cookie", tt.method, cookies)
		}
		if cookies[0].Name != "apisix-csrf-token" || cookies[0].Value == "" || cookies[0].Value == lastCookie {
			t.Fatalf("CSRF cookie was missing or reused from the cache")
		}
		lastCookie = cookies[0].Value
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls=%d, want 1", upstreamCalls)
	}
}
