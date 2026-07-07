package traffic_split

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
