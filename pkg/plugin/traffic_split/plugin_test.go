package traffic_split

import (
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

func performRequest(t *testing.T, p *Plugin) *Override {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
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
