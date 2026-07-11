package traffic_split

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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
