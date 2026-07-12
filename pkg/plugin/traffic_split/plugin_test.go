package traffic_split

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerSetsInlineUpstreamOverride(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				WeightedUpstreams: []WeightedUpstream{
					{
						Weight: 1,
						Upstream: &Upstream{
							Type:   "roundrobin",
							Scheme: "https",
							Nodes: []Node{
								{Host: "shadow.example.com", Port: 9443, Weight: 1},
							},
						},
					},
				},
			},
		},
	})

	override := performRequest(t, p)
	if override == nil {
		t.Fatal("traffic split override is nil")
	}
	if override.Scheme != "https" || override.Host != "shadow.example.com:9443" {
		t.Fatalf("override = %#v, want https://shadow.example.com:9443", override)
	}
}

func TestHandlerFormatsIPv6InlineUpstreamNode(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{
				Nodes: []Node{{Host: "2001:db8::1", Port: 8080, Weight: 1}},
			},
		}},
	}}})

	override := performRequest(t, p)
	if override == nil {
		t.Fatal("traffic split override is nil")
	}
	if override.Host != "[2001:db8::1]:8080" {
		t.Fatalf("override host = %q, want bracketed IPv6 address", override.Host)
	}
}

func TestHandlerCarriesInlineHostRewriteSettings(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{{
			WeightedUpstreams: []WeightedUpstream{{
				Upstream: &Upstream{
					PassHost:     "rewrite",
					UpstreamHost: "api.example.com",
					Nodes:        []Node{{Host: "127.0.0.1", Port: 8080, Weight: 1}},
				},
			}},
		}},
	})

	override := performRequest(t, p)
	if override == nil {
		t.Fatal("traffic split override is nil")
	}
	if override.PassHost != "rewrite" || override.UpstreamHost != "api.example.com" {
		t.Fatalf("override host settings = %#v, want rewrite/api.example.com", override)
	}
}

func TestHandlerAppliesSelectedUpstreamTimeoutToRequestContext(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{
				Timeout: resource.Timeout{Connect: 3, Send: 2, Read: 1},
				Nodes:   []Node{{Host: "127.0.0.1", Port: 8080, Weight: 1}},
			},
		}},
	}}})

	var deadline time.Time
	performRequestWithHandler(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		deadline, ok = r.Context().Deadline()
		if !ok {
			t.Fatal("request context has no deadline")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > 1100*time.Millisecond {
		t.Fatalf("selected upstream deadline remaining = %s, want about one second", remaining)
	}
}

func TestHandlerUsesWeightedRoundRobin(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				WeightedUpstreams: []WeightedUpstream{
					{
						Weight: 1,
						Upstream: &Upstream{
							Type:   "roundrobin",
							Scheme: "http",
							Nodes: []Node{
								{Host: "one.example.com", Port: 80, Weight: 1},
							},
						},
					},
					{
						Weight: 1,
						Upstream: &Upstream{
							Type:   "roundrobin",
							Scheme: "http",
							Nodes: []Node{
								{Host: "two.example.com", Port: 80, Weight: 1},
							},
						},
					},
				},
			},
		},
	})

	first := performRequest(t, p)
	second := performRequest(t, p)

	if first == nil || second == nil {
		t.Fatalf("overrides = %#v, %#v; want two overrides", first, second)
	}
	if first.Host == second.Host {
		t.Fatalf("weighted round-robin returned same host twice: %s", first.Host)
	}
}

func TestHandlerUsesStableHashForChashHeader(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{
				Type:   "chash",
				HashOn: "header",
				Key:    "X-User",
				Nodes: []Node{
					{Host: "one.example.com", Port: 80, Weight: 1},
					{Host: "two.example.com", Port: 80, Weight: 1},
				},
			},
		}},
	}}})

	seen := make(map[string]struct{})
	for range 4 {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
		req.Header.Set("X-User", "alice")
		if override := performRequestWithRequest(t, p, req); override == nil {
			t.Fatal("traffic split override is nil")
		} else {
			seen[override.Host] = struct{}{}
		}
	}
	if len(seen) != 1 {
		t.Fatalf("hash-selected hosts = %#v, want one stable host", seen)
	}
}

func TestResolveHashValueSupportsVariableCombinations(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/pets?id=42", nil)
	req.RemoteAddr = "192.0.2.40:12345"

	for _, test := range []struct {
		name string
		key  string
		want string
	}{
		{name: "adjacent variables", key: "$request_uri$remote_addr", want: "/pets?id=42192.0.2.40"},
		{name: "default value", key: "${arg_missing ?? fallback}$remote_addr", want: "fallback192.0.2.40"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveHashValue(req, "vars_combinations", test.key); got != test.want {
				t.Fatalf("vars_combinations hash value = %q, want %q", got, test.want)
			}
		})
	}
}

func TestHandlerExcludesPassivelyUnhealthyInlineUpstream(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{
				Nodes: []Node{
					{Host: "one.example.com", Port: 80, Weight: 1},
					{Host: "two.example.com", Port: 80, Weight: 1},
				},
				Checks: map[string]interface{}{
					"passive": map[string]interface{}{
						"unhealthy": map[string]interface{}{
							"http_statuses": []interface{}{500},
							"http_failures": 1,
						},
					},
				},
			},
		}},
	}}})

	first := performRequest(t, p)
	if first == nil || first.HealthReporter == nil || first.HealthTarget == "" {
		t.Fatalf("first override = %#v, want passive health reporter and target", first)
	}
	first.HealthReporter.ReportHTTP(first.HealthTarget, http.StatusInternalServerError)

	second := performRequest(t, p)
	if second == nil || second.Host == first.Host {
		t.Fatalf("second override = %#v, want the other healthy node", second)
	}
}

func TestHandlerFallsBackToRouteUpstreamForEmptyWeightedEntry(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				WeightedUpstreams: []WeightedUpstream{
					{Weight: 1},
				},
			},
		},
	})

	override := performRequest(t, p)
	if override != nil {
		t.Fatalf("override = %#v, want nil route-upstream fallback", override)
	}
}

func TestHandlerAppliesFirstMatchingRule(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []Match{
					{Vars: []any{[]any{"http_x_stage", "==", "beta"}}},
				},
				WeightedUpstreams: []WeightedUpstream{
					{
						Weight: 1,
						Upstream: &Upstream{
							Scheme: "http",
							Nodes:  []Node{{Host: "beta.example.com", Port: 80, Weight: 1}},
						},
					},
				},
			},
			{
				Match: []Match{
					{Vars: []any{[]any{"http_x_stage", "==", "stable"}}},
				},
				WeightedUpstreams: []WeightedUpstream{
					{
						Weight: 1,
						Upstream: &Upstream{
							Scheme: "http",
							Nodes:  []Node{{Host: "stable.example.com", Port: 80, Weight: 1}},
						},
					},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("X-Stage", "stable")
	override := performRequestWithRequest(t, p, req)

	if override == nil {
		t.Fatal("traffic split override is nil")
	}
	if override.Host != "stable.example.com:80" {
		t.Fatalf("override host = %q, want stable.example.com:80", override.Host)
	}
}

func TestHandlerSkipsWhenNoMatchVarsPass(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []Match{
					{Vars: []any{[]any{"arg_stage", "==", "beta"}}},
				},
				WeightedUpstreams: []WeightedUpstream{
					{
						Weight: 1,
						Upstream: &Upstream{
							Scheme: "http",
							Nodes:  []Node{{Host: "beta.example.com", Port: 80, Weight: 1}},
						},
					},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?stage=stable", nil)
	override := performRequestWithRequest(t, p, req)

	if override != nil {
		t.Fatalf("override = %#v, want route-upstream fallback", override)
	}
}

func TestMatchSupportsPrefixedVarsNumericAndRegexOperators(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []Match{
					{Vars: []any{
						[]any{"$request_method", "==", http.MethodGet},
						[]any{"arg_score", ">=", "10"},
						[]any{"http_x_region", "~", "^west-[0-9]+$"},
						[]any{"uri", "!~", "/internal"},
					}},
				},
				WeightedUpstreams: []WeightedUpstream{
					{
						Weight: 1,
						Upstream: &Upstream{
							Scheme: "http",
							Nodes:  []Node{{Host: "canary.example.com", Port: 80, Weight: 1}},
						},
					},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?score=12", nil)
	req.Header.Set("X-Region", "west-1")
	override := performRequestWithRequest(t, p, req)

	if override == nil {
		t.Fatal("traffic split override is nil")
	}
	if override.Host != "canary.example.com:80" {
		t.Fatalf("override host = %q, want canary.example.com:80", override.Host)
	}
}

func TestMatchSupportsNestedRestyExpressionAndApisixVariables(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match: []Match{{Vars: []any{
					"AND",
					[]any{"request_method", "in", []any{"GET", "HEAD"}},
					[]any{"remote_addr", "ipmatch", []any{"192.0.2.0/24"}},
					[]any{"http_x_env", "~*", "^prod$"},
					[]any{"graphql_root_fields", "has", "owner"},
					[]any{"arg_skip", "!", "==", "yes"},
				}}},
				WeightedUpstreams: []WeightedUpstream{{
					Weight: 1,
					Upstream: &Upstream{
						Scheme: "http",
						Nodes:  []Node{{Host: "canary.example.com", Port: 80, Weight: 1}},
					},
				}},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?skip=no", nil)
	req.RemoteAddr = "192.0.2.40:12345"
	req.Header.Set("X-Env", "PrOd")
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$graphql_root_fields", []string{"viewer", "owner"})
	override := performRequestWithRequest(t, p, req)
	if override == nil || override.Host != "canary.example.com:80" {
		t.Fatalf("override = %#v, want canary.example.com:80", override)
	}
}

func TestPostInitRejectsInvalidMatchExpression(t *testing.T) {
	p := &Plugin{config: Config{Rules: []Rule{{
		Match: []Match{{Vars: []any{[]any{"uri", "bogus", "/get"}}}},
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{Nodes: []Node{{Host: "canary.example.com", Port: 80, Weight: 1}}},
		}},
	}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid match rejected")
	}
}

func TestPostInitRejectsInvalidPassHostMode(t *testing.T) {
	p := &Plugin{config: Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{
				PassHost: "invalid",
				Nodes:    []Node{{Host: "example.com", Port: 80, Weight: 1}},
			},
		}},
	}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid pass_host rejected")
	}
}

func TestSchemaRejectsInvalidInlineUpstreamHostMode(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"rules": []any{map[string]any{
			"weighted_upstreams": []any{map[string]any{
				"upstream": map[string]any{
					"pass_host": "invalid",
					"nodes":     []any{map[string]any{"host": "example.com", "port": 80}},
				},
			}},
		}},
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("Validate() error = nil, want invalid pass_host rejected")
	}
}

func TestWeightedRouteFallbackCompetesWithInlineUpstream(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{
			{
				Weight: 1,
				Upstream: &Upstream{
					Nodes: []Node{{Host: "canary.example.com", Port: 80, Weight: 1}},
				},
			},
			{Weight: 1},
		},
	}}})

	seenRoute := 0
	seenCanary := 0
	for range 2 {
		if override := performRequest(t, p); override == nil {
			seenRoute++
		} else if override.Host == "canary.example.com:80" {
			seenCanary++
		}
	}
	if seenRoute != 1 || seenCanary != 1 {
		t.Fatalf("route selections = %d, canary selections = %d; want one each", seenRoute, seenCanary)
	}
}

func TestParsedExplicitZeroWeightDoesNotSelectUpstream(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"rules": []any{map[string]any{
			"weighted_upstreams": []any{
				map[string]any{
					"weight": 0,
					"upstream": map[string]any{
						"nodes": []any{map[string]any{"host": "disabled.example.com", "port": 80, "weight": 1}},
					},
				},
				map[string]any{"weight": 1},
			},
		}},
	}, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	for range 4 {
		if override := performRequest(t, p); override != nil {
			t.Fatalf("override = %#v, want route fallback with zero-weight upstream disabled", override)
		}
	}
}

func TestParsedExplicitZeroNodeWeightDoesNotSelectNode(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"rules": []any{map[string]any{
			"weighted_upstreams": []any{map[string]any{
				"upstream": map[string]any{
					"nodes": []any{
						map[string]any{"host": "disabled.example.com", "port": 80, "weight": 0},
						map[string]any{"host": "enabled.example.com", "port": 80, "weight": 1},
					},
				},
			}},
		}},
	}, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	for range 4 {
		override := performRequest(t, p)
		if override == nil || override.Host != "enabled.example.com:80" {
			t.Fatalf("override = %#v, want enabled.example.com:80", override)
		}
	}
}

func TestConfigAcceptsNumericUpstreamID(t *testing.T) {
	withTestUpstreamResolver(t, func(id string) (*Upstream, error) {
		if id != "123" {
			return nil, fmt.Errorf("unexpected upstream id %q", id)
		}
		return &Upstream{Nodes: []Node{{Host: "numeric.example.com", Port: 80, Weight: 1}}}, nil
	})

	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"rules": []any{map[string]any{
			"weighted_upstreams": []any{map[string]any{"upstream_id": 123, "weight": 1}},
		}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("Validate() numeric upstream_id error = %v", err)
	}
	if err := util.Parse(config, p.Config()); err != nil {
		t.Fatalf("Parse() numeric upstream_id error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if override := performRequest(t, p); override == nil || override.Host != "numeric.example.com:80" {
		t.Fatalf("override = %#v, want numeric.example.com:80", override)
	}
}

func TestHandlerSetsUpstreamIDOverride(t *testing.T) {
	withTestUpstreamResolver(t, func(id string) (*Upstream, error) {
		if id != "shadow" {
			return nil, fmt.Errorf("unexpected upstream id %q", id)
		}
		return &Upstream{
			Scheme: "https",
			Nodes: []Node{
				{Host: "shadow.example.com", Port: 9443, Weight: 1},
			},
		}, nil
	})

	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				WeightedUpstreams: []WeightedUpstream{
					{UpstreamID: "shadow", Weight: 1},
				},
			},
		},
	})

	override := performRequest(t, p)
	if override == nil {
		t.Fatal("traffic split override is nil")
	}
	if override.Scheme != "https" || override.Host != "shadow.example.com:9443" {
		t.Fatalf("override = %#v, want https://shadow.example.com:9443", override)
	}
}

func TestReferencedUpstreamCarriesHostRewriteSettings(t *testing.T) {
	withTestUpstreamResolver(t, func(id string) (*Upstream, error) {
		return upstreamFromResource(resource.Upstream{
			PassHost:     "rewrite",
			UpstreamHost: "api.example.com",
			Nodes:        []resource.Node{{Host: "127.0.0.1", Port: 8080, Weight: 1}},
		}), nil
	})

	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{UpstreamID: "upstream-1"}},
	}}})
	override := performRequest(t, p)
	if override == nil {
		t.Fatal("traffic split override is nil")
	}
	if override.PassHost != "rewrite" || override.UpstreamHost != "api.example.com" {
		t.Fatalf("override host settings = %#v, want rewrite/api.example.com", override)
	}
}

func TestReferencedUpstreamKeepsLegacyDefaultNodeWeight(t *testing.T) {
	withTestUpstreamResolver(t, func(id string) (*Upstream, error) {
		return upstreamFromResource(resource.Upstream{
			Nodes: []resource.Node{{Host: "default-weight.example.com", Port: 80}},
		}), nil
	})

	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{UpstreamID: "default-weight"}},
	}}})
	override := performRequest(t, p)
	if override == nil || override.Host != "default-weight.example.com:80" {
		t.Fatalf("override = %#v, want default-weight.example.com:80", override)
	}
}

func TestReferencedUpstreamDoesNotSelectExplicitZeroWeightNode(t *testing.T) {
	var stored resource.Upstream
	if err := json.Unmarshal([]byte(`{
		"nodes": [
			{"host": "disabled.example.com", "port": 80, "weight": 0},
			{"host": "enabled.example.com", "port": 80, "weight": 1}
		]
	}`), &stored); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	withTestUpstreamResolver(t, func(id string) (*Upstream, error) {
		return upstreamFromResource(stored), nil
	})
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{UpstreamID: "referenced"}},
	}}})

	target := p.rules[0].targets["traffic-split-0-0"]
	if got := len(target.overrides); got != 1 {
		t.Fatalf("compiled node overrides = %d, want one enabled node", got)
	}
	for nodeID, override := range target.overrides {
		if override.Host != "enabled.example.com:80" {
			t.Fatalf("compiled node %s override = %#v, want enabled.example.com:80", nodeID, override)
		}
	}
}

func TestHandlerReturnsInternalServerErrorForMissingUpstreamID(t *testing.T) {
	withTestUpstreamResolver(t, func(id string) (*Upstream, error) {
		return nil, fmt.Errorf("missing upstream %s", id)
	})

	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				WeightedUpstreams: []WeightedUpstream{
					{UpstreamID: "missing", Weight: 1},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusInternalServerError)
	}
	if got := rr.Body.String(); got != "failed to fetch upstream info by upstream id: missing\n" {
		t.Fatalf("response body = %q, want missing upstream error", got)
	}
}

func TestHandlerRejectsInvalidUpstreamIDBeforeRuleMatching(t *testing.T) {
	withTestUpstreamResolver(t, func(id string) (*Upstream, error) {
		return nil, fmt.Errorf("missing upstream %s", id)
	})

	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Match:             []Match{{Vars: []any{[]any{"arg_stage", "==", "beta"}}}},
				WeightedUpstreams: []WeightedUpstream{{UpstreamID: "missing", Weight: 1}},
			},
			{
				WeightedUpstreams: []WeightedUpstream{{
					Weight: 1,
					Upstream: &Upstream{
						Nodes: []Node{{Host: "stable.example.com", Port: 80, Weight: 1}},
					},
				}},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?stage=stable", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when any upstream_id is invalid")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "failed to fetch upstream info") {
		t.Fatalf("response body = %q, want upstream lookup error", rr.Body.String())
	}
}

func withTestUpstreamResolver(t *testing.T, resolver upstreamResolver) {
	t.Helper()

	old := getUpstreamByID
	getUpstreamByID = resolver
	t.Cleanup(func() {
		getUpstreamByID = old
	})
}

func performRequest(t *testing.T, p *Plugin) *Override {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	return performRequestWithRequest(t, p, req)
}

func performRequestWithRequest(t *testing.T, p *Plugin, req *http.Request) *Override {
	t.Helper()

	rr := httptest.NewRecorder()
	var seen *Override

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		override := GetOverride(r)
		if override != nil {
			overrideCopy := *override
			seen = &overrideCopy
		}
		w.Header().Set("X-Next-Called", "yes")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}
	if rr.Header().Get("X-Next-Called") != "yes" {
		t.Fatal("next handler was not called")
	}
	return seen
}

func performRequestWithHandler(t *testing.T, p *Plugin, next http.Handler) {
	t.Helper()

	rr := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://example.com/get", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}
}
