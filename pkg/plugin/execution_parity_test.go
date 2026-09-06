package plugin

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func parityBinding(t *testing.T, factory, config string, scope Scope) Binding {
	t.Helper()
	p := New(factory, base.Dependencies{Config: &appconfig.EffectiveConfig{}})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(config), p.Config()); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(base.ScopedSecretMaterializer); ok {
		secrets, scope, closeAttempt := testutil.ScopedSecretHarness(
			t,
			factory,
			nil,
			generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
		)
		t.Cleanup(closeAttempt)
		if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
			t.Fatal(err)
		}
	}
	if setter, ok := p.(interface{ SetConfiguredZones([]appconfig.Zone) }); ok {
		setter.SetConfiguredZones(nil)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	kind := ResourceRoute
	if scope == ScopeConsumer {
		kind = ResourceConsumer
	}
	b, err := BindPluginChecked(factory, p, scope, ResourceProvenance{Kind: kind, ID: "r5"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParityAuthenticationMustRespectRewritePriority(t *testing.T) {
	bindings := []Binding{
		parityBinding(t, "fault-injection", `{"abort":{"http_status":503,"body":"fault"}}`, ScopeRoute),
		parityBinding(t, "key-auth", `{}`, ScopeRoute),
	}
	handler := NewRequestPipeline(
		bindings,
		nil,
	).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	r, _ := apisixctx.EnsureRequestLifecycle(httptest.NewRequest("GET", "http://gateway.test/", nil), time.Now())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf(
			"got status=%d body=%q; official fault-injection priority 11000 must abort before key-auth 2500",
			w.Code,
			w.Body.String(),
		)
	}
}

func TestParityBodyFilterOrderIndependentOfRequestPhase(t *testing.T) {
	for _, requestConfig := range []bool{false, true} {
		t.Run(map[bool]string{false: "response-only", true: "request-and-response"}[requestConfig], func(t *testing.T) {
			cfg := `{"response":{"input_format":"plain","template":"TRANSFORMED"}}`
			if requestConfig {
				cfg = `{"request":{"input_format":"plain","template":"REQUEST"},"response":{"input_format":"plain","template":"TRANSFORMED"}}`
			}
			bindings := []Binding{
				parityBinding(t, "body-transformer", cfg, ScopeRoute),
				parityBinding(t, "response-rewrite", `{"body":"REWRITTEN"}`, ScopeRoute),
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
			handler := plan.Install(
				NewRequestPipeline(bindings, nil),
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
					w.Header().Set("Content-Type", "text/plain")
					_, _ = w.Write([]byte("UPSTREAM"))
				}),
			)
			r, _ := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest("POST", "http://gateway.test/", strings.NewReader("ORIGINAL")),
				time.Now(),
			)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 200 || w.Body.String() != "REWRITTEN" {
				t.Fatalf(
					"got status=%d body=%q; body-transformer 1080 must precede response-rewrite 899 in body_filter",
					w.Code,
					w.Body.String(),
				)
			}
		})
	}
}

type parityConsumerLookup struct{}

func (parityConsumerLookup) ConsumerByPluginKey(string, string) (resource.Consumer, bool) {
	return resource.Consumer{Username: "r5"}, true
}

func (parityConsumerLookup) ConsumerByID(string) (resource.Consumer, bool) {
	return resource.Consumer{Username: "r5"}, true
}

func (parityConsumerLookup) ConsumerGroupByID(string) (resource.ConsumerGroup, bool) {
	return resource.ConsumerGroup{}, false
}

func TestParityRouteRewriteRunsBeforeConsumerOverride(t *testing.T) {
	route := parityBinding(t, "redirect", `{"uri":"/route","ret_code":302}`, ScopeRoute)
	consumer := parityBinding(t, "redirect", `{"uri":"/consumer","ret_code":301}`, ScopeConsumer)
	auth := parityBinding(t, "key-auth", `{"anonymous_consumer":"r5"}`, ScopeRoute)
	auth.Plugin.(interface{ SetDependencies(base.Dependencies) }).SetDependencies(
		base.Dependencies{Consumers: parityConsumerLookup{}},
	)
	handler := NewRequestPipeline([]Binding{auth, route}, func(r *http.Request) (ConsumerResolution, error) {
		if _, ok := apisixctx.AuthenticationStateFrom(r); !ok {
			t.Fatal("key-auth did not authenticate")
		}
		return ConsumerResolution{Bindings: []Binding{consumer}, Request: r, Resolved: true}, nil
	}).Then(nil)
	r, _ := apisixctx.EnsureRequestLifecycle(httptest.NewRequest("GET", "http://gateway.test/", nil), time.Now())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 302 || w.Header().Get("Location") != "/route" {
		t.Fatalf(
			"got status=%d location=%q; official original route redirect must run after auth and before merging consumer plugins",
			w.Code,
			w.Header().Get("Location"),
		)
	}
}

func TestParityLoggerPrefixORRetainsAllOperands(t *testing.T) {
	r := httptest.NewRequest("POST", "http://gateway.test/", nil)
	rule := []any{
		"OR",
		[]any{"request_method", "==", "GET"},
		[]any{"request_method", "==", "PATCH"},
		[]any{"request_method", "==", "POST"},
	}
	if err := base.PrepareExprRegexps(rule); err != nil {
		t.Fatal(err)
	}
	if !base.ExprMatched(r, rule, 200) {
		t.Fatal("APISIX prefix OR(GET,PATCH,POST) must match POST; shared logger evaluator returned false")
	}
}

func TestParityConsumerGzipIsAcceptedByResponseExecutor(t *testing.T) {
	consumer := parityBinding(t, "gzip", `{"types":["text/plain"]}`, ScopeConsumer)
	plan, err := BuildResponsePlan(ResponsePlanInput{})
	if err != nil {
		t.Fatal(err)
	}
	pipeline := NewRequestPipeline(nil, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Bindings: []Binding{consumer}, Request: r, Resolved: true}, nil
	})
	handler := plan.Install(pipeline, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("payload", 50)))
	}))
	r, _ := apisixctx.EnsureRequestLifecycle(httptest.NewRequest("GET", "http://gateway.test/", nil), time.Now())
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("got status=%d body=%q from valid consumer gzip binding", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", w.Header().Get("Content-Encoding"))
	}
	reader, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(reader)
	if err != nil || string(decoded) != strings.Repeat("payload", 50) {
		t.Fatalf("decoded response = %q, %v", decoded, err)
	}
}

func TestParityURIBlockerRewriteMustRunBeforeRedirect(t *testing.T) {
	bindings := []Binding{
		parityBinding(t, "uri-blocker", `{"block_rules":["/blocked"]}`, ScopeRoute),
		parityBinding(t, "redirect", `{"uri":"/allowed","ret_code":302}`, ScopeRoute),
	}
	handler := NewRequestPipeline(bindings, nil).Then(nil)
	r, _ := apisixctx.EnsureRequestLifecycle(httptest.NewRequest("GET", "http://gateway.test/blocked", nil), time.Now())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf(
			"got status=%d location=%q; official rewrite uri-blocker2900 precedes redirect900",
			w.Code,
			w.Header().Get("Location"),
		)
	}
}

func TestParityCacheHeadersHiddenInResponsePlan(t *testing.T) {
	binding := parityBinding(
		t,
		"proxy-cache",
		`{"cache_strategy":"memory","hide_cache_headers":true,"cache_control":true}`,
		ScopeRoute,
	)
	plan, err := BuildResponsePlan(
		ResponsePlanInput{
			StaticBindings: []Binding{binding},
			BufferedConfig: base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	handler := plan.Install(
		NewRequestPipeline([]Binding{binding}, nil),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("Expires", "Wed, 21 Oct 2030 07:28:00 GMT")
			_, _ = w.Write([]byte("payload"))
		}),
	)
	for i := range 2 {
		r, _ := apisixctx.EnsureRequestLifecycle(
			httptest.NewRequest("GET", "http://gateway.test/cache", nil),
			time.Now(),
		)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != "payload" || w.Header().Get("Cache-Control") != "" ||
			w.Header().Get("Expires") != "" {
			t.Fatalf("response %d = %d %q %v", i, w.Code, w.Body.String(), w.Header())
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want one", calls)
	}
}
