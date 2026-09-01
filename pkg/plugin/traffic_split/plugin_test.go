package traffic_split

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetUpstreamResolver(testUpstreamResolver)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestSchemaMatchesAPISIX317Fields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	var document struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(p.GetSchema()), &document); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if len(document.Properties) != 1 {
		t.Fatalf("schema properties = %v, want only APISIX 3.17 fields", document.Properties)
	}
	if _, ok := document.Properties["rules"]; !ok {
		t.Fatalf("schema is missing APISIX 3.17 field %q", "rules")
	}
}

func TestOverrideFromNodeRemapsGRPCSchemesAndDefaultPorts(t *testing.T) {
	grpc := overrideFromNode(&Upstream{Scheme: "grpc"}, Node{Host: "127.0.0.1", Port: 50051})
	if grpc.Scheme != "http" {
		t.Fatalf("grpc scheme = %q, want http", grpc.Scheme)
	}
	if grpc.Host != "127.0.0.1:50051" {
		t.Fatalf("grpc host = %q, want 127.0.0.1:50051", grpc.Host)
	}

	grpcs := overrideFromNode(&Upstream{Scheme: "grpcs"}, Node{Host: "127.0.0.1"})
	if grpcs.Scheme != "https" {
		t.Fatalf("grpcs scheme = %q, want https", grpcs.Scheme)
	}
	if grpcs.Host != "127.0.0.1:443" {
		t.Fatalf("grpcs host = %q, want 127.0.0.1:443", grpcs.Host)
	}
}

func TestUpstreamUnmarshalSortsMapNodesByAddress(t *testing.T) {
	const config = `{"nodes":{"z.example.com:8080":1,"a.example.com:8080":1,"m.example.com:8080":1}}`
	const want = "a.example.com,m.example.com,z.example.com"

	for range 20 {
		var upstream Upstream
		if err := json.Unmarshal([]byte(config), &upstream); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		hosts := make([]string, 0, len(upstream.Nodes))
		for _, node := range upstream.Nodes {
			hosts = append(hosts, node.Host)
		}
		if got := strings.Join(hosts, ","); got != want {
			t.Fatalf("upstream node order = %q, want %q", got, want)
		}
	}
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

func TestHandlerDoesNotCollapsePhaseTimeoutsIntoOverallDeadline(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{
				Timeout: resource.Timeout{Connect: 3, Send: 2, Read: 60},
				Nodes:   []Node{{Host: "127.0.0.1", Port: 8080, Weight: 1}},
			},
		}},
	}}})

	performRequestWithHandler(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			t.Fatal("selected phase timeouts must not become one whole-request deadline")
		}
		override := GetOverride(r)
		if override == nil || override.Timeout.Connect != 3 || override.Timeout.Send != 2 ||
			override.Timeout.Read != 60 {
			t.Fatalf("override timeout = %#v, want connect=3 send=2 read=60", override)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
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

func TestParsedInlineUpstreamDefaultsRetriesToOtherNodes(t *testing.T) {
	p := &Plugin{}
	p.SetUpstreamResolver(testUpstreamResolver)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"rules": []any{map[string]any{
			"weighted_upstreams": []any{map[string]any{
				"upstream": map[string]any{
					"nodes": []any{
						map[string]any{"host": "one.example.com", "port": 80, "weight": 1},
						map[string]any{"host": "two.example.com", "port": 80, "weight": 1},
						map[string]any{"host": "three.example.com", "port": 80, "weight": 1},
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

	override := performRequest(t, p)
	if override == nil {
		t.Fatal("override = nil")
	}
	if override.Retries != 2 {
		t.Fatalf("override retries = %d, want two other nodes", override.Retries)
	}
}

func TestParsedInlineUpstreamDoesNotProjectExplicitRetries(t *testing.T) {
	p := &Plugin{}
	p.SetUpstreamResolver(testUpstreamResolver)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"rules": []any{map[string]any{
			"weighted_upstreams": []any{map[string]any{
				"upstream": map[string]any{
					"retries": 0,
					"nodes": []any{
						map[string]any{"host": "one.example.com", "port": 80, "weight": 1},
						map[string]any{"host": "two.example.com", "port": 80, "weight": 1},
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

	override := performRequest(t, p)
	if override == nil {
		t.Fatal("override = nil")
	}
	if override.Retries != 1 {
		t.Fatalf("override retries = %d, want generated upstream default for one peer", override.Retries)
	}
}

func TestAPISIX317InlineUpstreamSchemaRejectsInvalidGeneratedUpstream(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name     string
		upstream map[string]any
	}{
		{name: "missing nodes or discovery", upstream: map[string]any{}},
		{
			name: "unsupported scheme",
			upstream: map[string]any{
				"scheme": "smtp",
				"nodes":  map[string]any{"127.0.0.1:8080": 1},
			},
		},
		{
			name: "unknown field",
			upstream: map[string]any{
				"nodes":           map[string]any{"127.0.0.1:8080": 1},
				"local_extension": true,
			},
		},
		{
			name: "empty health checks",
			upstream: map[string]any{
				"checks": map[string]any{},
				"nodes":  map[string]any{"127.0.0.1:8080": 1},
			},
		},
		{
			name: "invalid client certificate id",
			upstream: map[string]any{
				"nodes": map[string]any{"127.0.0.1:8080": 1},
				"tls":   map[string]any{"client_cert_id": []any{}},
			},
		},
		{
			name: "unpaired client certificate",
			upstream: map[string]any{
				"nodes": map[string]any{"127.0.0.1:8080": 1},
				"tls":   map[string]any{"client_cert": strings.Repeat("c", 128)},
			},
		},
		{
			name: "node host contains whitespace",
			upstream: map[string]any{
				"nodes": []any{map[string]any{"host": "bad host", "weight": 1}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := map[string]any{
				"rules": []any{map[string]any{
					"weighted_upstreams": []any{map[string]any{"upstream": test.upstream}},
				}},
			}
			if err := util.Validate(config, p.GetSchema()); err == nil {
				t.Fatalf("schema accepted invalid inline upstream %#v", test.upstream)
			}
		})
	}
}

func TestAPISIX317InlineUpstreamSchemaAcceptsOfficialNestedFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"rules": []any{map[string]any{
			"weighted_upstreams": []any{map[string]any{
				"upstream": map[string]any{
					"checks": map[string]any{
						"active": map[string]any{"host": "health.example.com"},
						"passive": map[string]any{
							"unhealthy": map[string]any{"http_failures": 1},
						},
					},
					"nodes": []any{map[string]any{
						"host": "api.example.com", "port": 443, "weight": 1,
					}},
					"tls": map[string]any{
						"client_cert": strings.Repeat("c", 128),
						"client_key":  strings.Repeat("k", 64),
					},
				},
			}},
		}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected APISIX-valid nested upstream: %v", err)
	}
}

func TestAPISIX317InlineUpstreamProjectionDropsUncopiedFields(t *testing.T) {
	input := &Upstream{
		Name:         "canary",
		Type:         "chash",
		Scheme:       "https",
		TLS:          &resource.UpstreamTLS{Verify: true},
		PassHost:     "rewrite",
		UpstreamHost: "api.example.com",
		HashOn:       "header",
		Key:          "X-Tenant",
		Timeout:      resource.Timeout{Connect: 1, Send: 2, Read: 3},
		Retries:      0,
		retriesSet:   true,
		Checks:       map[string]any{"passive": map[string]any{}},
		Nodes:        []Node{{Host: "127.0.0.1", Port: 8443, Weight: 1, weightSet: true}},
	}

	projected := projectInlineUpstream(input)
	if projected == input {
		t.Fatal("inline projection reused the input object")
	}
	if projected.TLS != nil || projected.Checks != nil || projected.RetriesConfigured() {
		t.Fatalf("inline projection retained unprojected fields: %#v", projected)
	}
	if projected.Name != input.Name || projected.Type != input.Type || projected.Scheme != input.Scheme ||
		projected.PassHost != input.PassHost || projected.UpstreamHost != input.UpstreamHost ||
		projected.HashOn != input.HashOn || projected.Key != input.Key ||
		projected.Timeout != input.Timeout || len(projected.Nodes) != 1 {
		t.Fatalf("inline projection lost official runtime fields: %#v", projected)
	}
}

func TestRetryOverrideSelectsAnotherNodeFromSameTarget(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{
				Nodes: []Node{
					{Host: "one.example.com", Port: 80, Weight: 1},
					{Host: "two.example.com", Port: 80, Weight: 1},
					{Host: "three.example.com", Port: 80, Weight: 1},
				},
			},
		}},
	}}})

	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	first := performRequestWithRequest(t, p, request)
	if first == nil || first.NextRetry == nil {
		t.Fatalf("first override = %#v, want retry selector", first)
	}
	second := first.NextRetry(request)
	if second == nil || second.NextRetry == nil {
		t.Fatalf("second override = %#v, want chained retry selector", second)
	}
	if second.Host == first.Host {
		t.Fatalf("retry host = %q, want a node other than first host %q", second.Host, first.Host)
	}
	third := second.NextRetry(request)
	if third == nil {
		t.Fatal("third override = nil")
	}
	if third.Host == second.Host {
		t.Fatalf("second retry host = %q, want a node other than previous host", third.Host)
	}
}

func TestHandlerSelectsHighestPriorityInlineNode(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{Nodes: []Node{
				{Host: "low.example.com", Port: 80, Weight: 1, Priority: 0},
				{Host: "high.example.com", Port: 80, Weight: 1, Priority: 10},
			}},
		}},
	}}})

	for range 10 {
		override := performRequest(t, p)
		if override == nil || override.Host != "high.example.com:80" {
			t.Fatalf("priority override = %#v, want high.example.com:80", override)
		}
	}
}

func TestChashRetryExhaustsActualHighPriorityPeersBeforeLowerGroup(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{
				Type: "chash", HashOn: "header", Key: "X-Key",
				Nodes: []Node{
					{Host: "a-high.example.com", Port: 80, Weight: 1, Priority: 10},
					{Host: "b-high.example.com", Port: 80, Weight: 1, Priority: 10},
					{Host: "low.example.com", Port: 80, Weight: 1, Priority: 0},
				},
			},
		}},
	}}})

	var key string
	compiled := p.rules[0]
	for _, target := range compiled.targets {
		for candidate := range 1000 {
			request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
			request.Header.Set("X-Key", fmt.Sprintf("key-%d", candidate))
			if target.selectHashedNode(request) == "http://b-high.example.com:80" {
				key = request.Header.Get("X-Key")
				break
			}
		}
	}
	if key == "" {
		t.Fatal("no deterministic chash key selected b-high.example.com")
	}

	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request.Header.Set("X-Key", key)
	first := performRequestWithRequest(t, p, request)
	if first == nil || first.Host != "b-high.example.com:80" {
		t.Fatalf("initial chash override = %#v, want b-high.example.com:80", first)
	}
	second := first.NextRetry(request)
	if second == nil || second.Host != "a-high.example.com:80" {
		t.Fatalf("first retry override = %#v, want remaining high-priority peer", second)
	}
	third := second.NextRetry(request)
	if third == nil || third.Host != "low.example.com:80" {
		t.Fatalf("second retry override = %#v, want lower-priority fallback", third)
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

func TestTrafficSplitChashUsesPinnedKetamaOwner(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{
				Type: "chash", HashOn: "header", Key: "X-Tenant",
				Nodes: []Node{
					{Host: "10.0.0.1", Port: 80, Weight: 1},
					{Host: "10.0.0.2", Port: 80, Weight: 2},
					{Host: "10.0.0.3", Port: 80, Weight: 1},
				},
			},
		}},
	}}})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request.Header.Set("X-Tenant", "alpha")

	override := performRequestWithRequest(t, p, request)
	if override == nil || override.Host != "10.0.0.3:80" {
		t.Fatalf("chash override = %#v, want pinned 10.0.0.3:80", override)
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

func TestHandlerDoesNotApplyInlinePassiveChecks(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{{
		WeightedUpstreams: []WeightedUpstream{{
			Upstream: &Upstream{
				Nodes: []Node{
					{Host: "one.example.com", Port: 80, Weight: 1},
					{Host: "two.example.com", Port: 80, Weight: 1},
				},
				Checks: map[string]any{
					"passive": map[string]any{
						"unhealthy": map[string]any{
							"http_statuses": []any{500},
							"http_failures": 1,
						},
					},
				},
			},
		}},
	}}})

	first := performRequest(t, p)
	if first == nil {
		t.Fatal("first override = nil")
	}
	if first.HealthReporter != nil {
		t.Fatalf("first override health reporter = %#v, want nil for inline checks", first.HealthReporter)
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

func TestHandlerMatchesFormPostArgumentWithoutConsumingBody(t *testing.T) {
	tests := []struct {
		name string
		form string
	}{
		{name: "small", form: "id=1&name=jack"},
		{name: "large", form: "id=1&name=" + strings.Repeat("x", 1<<20)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Rules: []Rule{{
				Match: []Match{{Vars: []any{[]any{"post_arg_id", "==", "1"}}}},
				WeightedUpstreams: []WeightedUpstream{{
					Upstream: &Upstream{
						Nodes: []Node{{Host: "form.example.com", Port: 80, Weight: 1}},
					},
				}},
			}}})

			req := httptest.NewRequest(http.MethodPost, "http://example.com/form", strings.NewReader(test.form))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
			var override *Override
			var body string
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				override = GetOverride(r)
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read downstream request body: %v", err)
				}
				body = string(data)
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(httptest.NewRecorder(), req)

			if override == nil || override.Host != "form.example.com:80" {
				t.Fatalf("override = %#v, want form.example.com:80", override)
			}
			if body != test.form {
				t.Fatalf("downstream request body = %d bytes, want %d bytes", len(body), len(test.form))
			}
		})
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
	p.SetUpstreamResolver(testUpstreamResolver)
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

func TestReferencedUpstreamCarriesTLSSettings(t *testing.T) {
	upstream := upstreamFromResource(resource.Upstream{
		Scheme: "https",
		TLS: &resource.UpstreamTLS{
			ClientCert: "CERT",
			ClientKey:  "KEY",
			Verify:     true,
		},
	})
	if upstream.TLS == nil || upstream.TLS.ClientCert != "CERT" || upstream.TLS.ClientKey != "KEY" ||
		!upstream.TLS.Verify {
		t.Fatalf("referenced upstream TLS = %#v, want verify/certificate settings preserved", upstream.TLS)
	}
}

func TestReferencedUpstreamCarriesNodePriority(t *testing.T) {
	upstream := upstreamFromResource(resource.Upstream{Nodes: []resource.Node{{
		Host: "priority.example.com", Port: 80, Weight: 1, Priority: 10,
	}}})
	if len(upstream.Nodes) != 1 || upstream.Nodes[0].Priority != 10 {
		t.Fatalf("referenced upstream nodes = %#v, want priority 10", upstream.Nodes)
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
	if got := rr.Body.String(); got != "failed to find upstream by id: missing\n" {
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
	if !strings.Contains(rr.Body.String(), "failed to find upstream by id") {
		t.Fatalf("response body = %q, want upstream lookup error", rr.Body.String())
	}
}

var testUpstreamResolver ResourceUpstreamResolver

func withTestUpstreamResolver(t *testing.T, resolver func(string) (*Upstream, error)) {
	t.Helper()

	old := testUpstreamResolver
	testUpstreamResolver = func(id string) (resource.Upstream, error) {
		upstream, err := resolver(id)
		if err != nil {
			return resource.Upstream{}, err
		}
		return resourceFromUpstream(upstream)
	}
	t.Cleanup(func() {
		testUpstreamResolver = old
	})
}

func resourceFromUpstream(upstream *Upstream) (resource.Upstream, error) {
	if upstream == nil {
		return resource.Upstream{}, nil
	}
	nodes := make([]map[string]any, 0, len(upstream.Nodes))
	for _, node := range upstream.Nodes {
		encoded := map[string]any{
			"host": node.Host, "port": node.Port, "priority": node.Priority,
		}
		if node.weightSet {
			encoded["weight"] = node.Weight
		}
		nodes = append(nodes, encoded)
	}
	raw, err := json.Marshal(map[string]any{
		"type": upstream.Type, "scheme": upstream.Scheme, "tls": upstream.TLS,
		"pass_host": upstream.PassHost, "upstream_host": upstream.UpstreamHost,
		"hash_on": upstream.HashOn, "key": upstream.Key, "timeout": upstream.Timeout,
		"retries": upstream.Retries, "checks": upstream.Checks, "nodes": nodes,
	})
	if err != nil {
		return resource.Upstream{}, err
	}
	var stored resource.Upstream
	if err := json.Unmarshal(raw, &stored); err != nil {
		return resource.Upstream{}, err
	}
	return stored, nil
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

type testRuntimeAcquirer struct{}

func (testRuntimeAcquirer) Acquire(
	*Upstream,
	map[string]int,
	map[string]int,
) (*Runtime, error) {
	return &Runtime{}, nil
}

func TestPostInitDropsPreparationAuthorities(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.SetRuntimeAcquirer(testRuntimeAcquirer{})
	p.SetUpstreamResolver(func(string) (resource.Upstream, error) {
		return resource.Upstream{}, nil
	})
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	if p.runtimeAcquirer != nil || p.upstreamResolver != nil || p.runtimeAcquirerSet {
		t.Fatal("PostInit retained preparation-only runtime authority")
	}
}
