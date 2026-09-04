package route

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestResolveUpstreamTimeoutsUsesPerFieldRouteOverrides(t *testing.T) {
	got := resolveUpstreamTimeouts(
		resource.Timeout{Connect: 1, Send: 0, Read: 3},
		resource.Timeout{Connect: 10, Send: 20, Read: 30},
	)
	want := upstreamTimeouts{
		connect:        time.Second,
		send:           20 * time.Second,
		read:           3 * time.Second,
		responseHeader: 3 * time.Second,
	}
	if got != want {
		t.Fatalf("resolveUpstreamTimeouts() = %#v, want %#v", got, want)
	}
}

func TestResolveUpstreamTimeoutsAppliesAPISIXDefaultsWhenOmitted(t *testing.T) {
	got := resolveUpstreamTimeouts(resource.Timeout{}, resource.Timeout{})
	want := upstreamTimeouts{
		connect:        60 * time.Second,
		send:           60 * time.Second,
		read:           60 * time.Second,
		responseHeader: 60 * time.Second,
	}
	if got != want {
		t.Fatalf("omitted timeouts = %#v, want APISIX 60s defaults %#v", got, want)
	}
}

func TestResolveUpstreamTimeoutsKeepsExplicitPositiveValues(t *testing.T) {
	got := resolveUpstreamTimeouts(
		resource.Timeout{},
		resource.Timeout{Connect: 2, Send: 3, Read: 4},
	)
	want := upstreamTimeouts{
		connect:        2 * time.Second,
		send:           3 * time.Second,
		read:           4 * time.Second,
		responseHeader: 4 * time.Second,
	}
	if got != want {
		t.Fatalf("explicit timeouts = %#v, want %#v", got, want)
	}
}

func TestResolveUpstreamTimeoutsDefaultsOnlyUnresolvedFields(t *testing.T) {
	got := resolveUpstreamTimeouts(
		resource.Timeout{Connect: 1},
		resource.Timeout{},
	)
	want := upstreamTimeouts{
		connect:        time.Second,
		send:           60 * time.Second,
		read:           60 * time.Second,
		responseHeader: 60 * time.Second,
	}
	if got != want {
		t.Fatalf("partial timeouts = %#v, want %#v", got, want)
	}
}

func TestBuildClusterConfigSelectsCleartextHTTP2OnlyForGRPC(t *testing.T) {
	for _, test := range []struct {
		scheme string
		want   bool
	}{
		{scheme: "grpc", want: true},
		{scheme: "grpcs", want: false},
		{scheme: "http", want: false},
		{scheme: "https", want: false},
	} {
		t.Run(test.scheme, func(t *testing.T) {
			config, err := buildClusterConfigWithSSLResolver(
				resource.Route{},
				resource.Upstream{Scheme: test.scheme},
				map[string]int{"http://127.0.0.1:8080": 1},
				nil,
				&testEffectiveConfig().Config,
			)
			if err != nil {
				t.Fatalf("buildClusterConfigWithSSLResolver() error = %v", err)
			}
			if config.HTTP2Cleartext != test.want {
				t.Fatalf("HTTP2Cleartext = %t, want %t", config.HTTP2Cleartext, test.want)
			}
		})
	}
}

func TestBuildClusterConfigDoesNotInventAdmissionCapacity(t *testing.T) {
	for _, test := range []struct {
		name         string
		staticConfig *appconfig.Config
	}{
		{name: "no static config"},
		{name: "ordinary static config", staticConfig: &appconfig.Config{
			Apisix: appconfig.Apisix{ProxyMode: "http"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := buildClusterConfigWithSSLResolver(
				resource.Route{},
				resource.Upstream{Scheme: "http"},
				map[string]int{"http://127.0.0.1:8080": 1},
				nil,
				test.staticConfig,
			)
			if err != nil {
				t.Fatalf("buildClusterConfigWithSSLResolver() error = %v", err)
			}
			transport := proxy.NewTransport(config.Transport)
			t.Cleanup(transport.CloseIdleConnections)
			if transport.MaxIdleConns != proxy.DefaultMaxIdleConns ||
				transport.MaxIdleConnsPerHost != proxy.DefaultMaxIdleConnsPerHost ||
				transport.MaxConnsPerHost != proxy.DefaultMaxConnsPerHost {
				t.Fatalf(
					"transport capacities = %d/%d/%d, want %d/%d/%d",
					transport.MaxIdleConns,
					transport.MaxIdleConnsPerHost,
					transport.MaxConnsPerHost,
					proxy.DefaultMaxIdleConns,
					proxy.DefaultMaxIdleConnsPerHost,
					proxy.DefaultMaxConnsPerHost,
				)
			}
			if config.MaxInFlight != 0 {
				t.Fatalf("MaxInFlight = %d, want no default admission limit", config.MaxInFlight)
			}
		})
	}
}

func TestBuildClusterConfigCarriesAPISIXRetryTimeout(t *testing.T) {
	config, err := buildClusterConfigWithSSLResolver(
		resource.Route{},
		resource.Upstream{Scheme: "http", RetryTimeout: 0.25},
		map[string]int{"http://127.0.0.1:8080": 1},
		nil,
		&testEffectiveConfig().Config,
	)
	if err != nil {
		t.Fatalf("buildClusterConfigWithSSLResolver() error = %v", err)
	}
	field := reflect.ValueOf(config).FieldByName("RetryTimeout")
	if !field.IsValid() {
		t.Fatal("ClusterConfig is missing RetryTimeout")
	}
	if got := time.Duration(field.Int()); got != 250*time.Millisecond {
		t.Fatalf("RetryTimeout = %s, want 250ms", got)
	}
}

func TestAttachHTTPRetriesStopsAfterAPISIXRetryTimeout(t *testing.T) {
	const target = "http://127.0.0.1:8080"
	request := httptest.NewRequest(http.MethodGet, target+"/resource", nil)
	request = attachHTTPRetriesCompiled(
		request,
		resource.Upstream{Retries: 5, RetryTimeout: 0.05},
		proxy.NewSingleLoadBalance(target),
		map[string]compiledUpstreamTarget{
			target: {scheme: "http", host: "127.0.0.1:8080", nodeHost: "127.0.0.1:8080"},
		},
	)
	attempts := 0
	transport := proxy.NewRetryTransport(retryTimeoutRoundTripper(func(*http.Request) (*http.Response, error) {
		attempts++
		time.Sleep(30 * time.Millisecond)
		return nil, errors.New("connection refused")
	}))
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("RoundTrip() error = nil, want transport error")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 before retry_timeout expires", attempts)
	}
}

func TestAttachHTTPRetriesUsesReferencedTrafficSplitRetryTimeout(t *testing.T) {
	const target = "http://127.0.0.1:8080"
	override := &traffic_split.Override{
		Scheme:       "http",
		Host:         "127.0.0.1:8080",
		PassHost:     "node",
		Retries:      5,
		RetryTimeout: 0.05,
	}
	override.NextRetry = func(*http.Request) *traffic_split.Override { return override }
	request := httptest.NewRequest(http.MethodGet, target+"/resource", nil)
	request = traffic_split.WithOverride(request, override)
	request = attachHTTPRetriesCompiled(request, resource.Upstream{}, nil, nil)

	attempts := 0
	transport := proxy.NewRetryTransport(retryTimeoutRoundTripper(func(*http.Request) (*http.Response, error) {
		attempts++
		time.Sleep(30 * time.Millisecond)
		return nil, errors.New("connection refused")
	}))
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("RoundTrip() error = nil, want transport error")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 before referenced traffic-split retry_timeout expires", attempts)
	}
}

type retryTimeoutRoundTripper func(*http.Request) (*http.Response, error)

func (roundTripper retryTimeoutRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTripper(request)
}

func TestPlanRouteUpstreamPreparesClusterForDynamicGRPCSTarget(t *testing.T) {
	plan, err := PlanRouteUpstream(
		resource.Route{ID: "dynamic-grpcs", Upstream: resource.Upstream{Scheme: "grpcs"}},
		resource.Service{}, nil, nil, &testEffectiveConfig().Config,
	)
	if err != nil {
		t.Fatalf("PlanRouteUpstream() error = %v", err)
	}
	if plan.ClusterConfig == nil {
		t.Fatal("PlanRouteUpstream() returned nil cluster config for dynamic grpcs target")
	}
}

func TestUpstreamTLSInsecureSkipVerify(t *testing.T) {
	tests := []struct {
		name     string
		upstream resource.Upstream
		want     bool
	}{
		{name: "tls omitted", upstream: resource.Upstream{}, want: true},
		{name: "verify false", upstream: resource.Upstream{TLS: &resource.UpstreamTLS{}}, want: true},
		{name: "verify true", upstream: resource.Upstream{TLS: &resource.UpstreamTLS{Verify: true}}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := upstreamTLSInsecureSkipVerify(test.upstream); got != test.want {
				t.Fatalf("upstreamTLSInsecureSkipVerify() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestBuildClusterConfigKeyIncludesUpstreamNameForObserverIdentity(t *testing.T) {
	servers := map[string]int{"http://127.0.0.1:18091": 1}
	base := resource.Upstream{Scheme: "http", Nodes: []resource.Node{
		{Host: "127.0.0.1", Port: 18091, Weight: 1},
	}}
	first, err := buildClusterConfigWithSSLResolver(resource.Route{}, base, servers, nil, &testEffectiveConfig().Config)
	if err != nil {
		t.Fatalf("first buildClusterConfig() error = %v", err)
	}
	renamed := base
	renamed.Name = "orders-v2"
	second, err := buildClusterConfigWithSSLResolver(
		resource.Route{UpstreamID: "upstream-orders"}, renamed, servers, nil, &testEffectiveConfig().Config,
	)
	if err != nil {
		t.Fatalf("second buildClusterConfig() error = %v", err)
	}

	firstKey, err := first.Key()
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := second.Key()
	if err != nil {
		t.Fatal(err)
	}
	if firstKey == secondKey {
		t.Fatal("upstream name changed the observer label without changing the cluster key")
	}
	if second.Name != "orders-v2" {
		t.Fatalf("cluster label = %q, want upstream name", second.Name)
	}
}

func TestBuildClusterConfigKeyChangesWithTimeout(t *testing.T) {
	servers := map[string]int{"http://127.0.0.1:18091": 1}
	base := resource.Upstream{Scheme: "http", Nodes: []resource.Node{
		{Host: "127.0.0.1", Port: 18091, Weight: 1},
	}}
	first, err := buildClusterConfigWithSSLResolver(resource.Route{}, base, servers, nil, &testEffectiveConfig().Config)
	if err != nil {
		t.Fatalf("first buildClusterConfig() error = %v", err)
	}
	changed := base
	changed.Timeout = resource.Timeout{Read: 2}
	second, err := buildClusterConfigWithSSLResolver(
		resource.Route{},
		changed,
		servers,
		nil,
		&testEffectiveConfig().Config,
	)
	if err != nil {
		t.Fatalf("second buildClusterConfig() error = %v", err)
	}

	firstKey, err := first.Key()
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := second.Key()
	if err != nil {
		t.Fatal(err)
	}
	if firstKey == secondKey {
		t.Fatal("changing the upstream read timeout did not change the cluster key")
	}
}

func TestBuildClusterConfigUsesKeyPrefixLabelWhenUnnamed(t *testing.T) {
	servers := map[string]int{"http://127.0.0.1:18091": 1}
	config, err := buildClusterConfigWithSSLResolver(
		resource.Route{},
		resource.Upstream{Scheme: "http", Nodes: []resource.Node{
			{Host: "127.0.0.1", Port: 18091, Weight: 1},
		}},
		servers,
		nil,
		&testEffectiveConfig().Config,
	)
	if err != nil {
		t.Fatalf("buildClusterConfig() error = %v", err)
	}
	if len(config.Name) != 12 {
		t.Fatalf("cluster label = %q, want 12-hex key prefix", config.Name)
	}
}

func TestBuildClusterConfigOmitsActiveChecksWhenDisabled(t *testing.T) {
	staticConfig := &appconfig.Config{Apisix: appconfig.Apisix{DisableUpstreamHealthcheck: true}}

	config, err := buildClusterConfigWithSSLResolver(
		resource.Route{},
		resource.Upstream{Scheme: "http", Nodes: []resource.Node{
			{Host: "127.0.0.1", Port: 18091, Weight: 1},
		}, Checks: map[string]any{
			"active":  map[string]any{"http_path": "/health"},
			"passive": map[string]any{},
		}},
		map[string]int{"http://127.0.0.1:18091": 1},
		nil,
		staticConfig,
	)
	if err != nil {
		t.Fatalf("buildClusterConfig() error = %v", err)
	}
	if _, ok := config.Checks["active"]; ok {
		t.Fatal("active checks retained after disable_upstream_healthcheck")
	}
	if _, ok := config.Checks["passive"]; !ok {
		t.Fatal("passive checks removed by disable_upstream_healthcheck")
	}
}
