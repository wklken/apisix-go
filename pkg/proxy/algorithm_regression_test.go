package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/chash"
)

func testAlgorithmBalancer(t *testing.T, config ClusterConfig) *algorithmLoadBalance {
	t.Helper()
	lb, err := newClusterLoadBalancer(config)
	if err != nil {
		t.Fatal(err)
	}
	return lb.(*algorithmLoadBalance)
}

func TestAlgorithmHashSelectionAndMissingValues(t *testing.T) {
	targets := map[string]int{"http://a:80": 1, "http://b:80": 3}
	lb := testAlgorithmBalancer(
		t,
		ClusterConfig{Type: "chash", HashOn: "header", HashKey: "X-Tenant", Targets: targets},
	)
	ring, err := chash.New(
		[]chash.Node{{ID: "a:80", Target: "http://a:80", Weight: 1}, {ID: "b:80", Target: "http://b:80", Weight: 3}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"one", "two", "", "long-tenant-name"} {
		r := httptest.NewRequest("GET", "http://gateway/path", nil)
		r.Header.Set("X-Tenant", key)
		target := lb.NextForRequest(r)
		want, _ := ring.Lookup(key)
		if target != want {
			t.Fatalf("key %q target=%s want=%s", key, target, want)
		}
		if next := lb.NextForRequest(r); next == target || next == "" {
			t.Fatalf("retry target=%s first=%s", next, target)
		}
		if next := lb.NextForRequest(r); next != "" {
			t.Fatalf("exhausted retry=%s", next)
		}
		algorithmScope(r).finish()
	}
	r := httptest.NewRequest("GET", "http://gateway/path?tenant=", nil)
	r.RemoteAddr = "192.0.2.7:4500"
	r.Header.Set("X-Empty", "")
	r.AddCookie(&http.Cookie{Name: "empty", Value: ""})
	for _, tc := range []struct{ on, key, want string }{
		{"header", "X-Missing", "192.0.2.7"},
		{"header", "X-Empty", ""},
		{"cookie", "missing", "192.0.2.7"},
		{"cookie", "empty", ""},
		{"vars", "arg_tenant", ""},
		{"vars", "arg_missing", "192.0.2.7"},
		{"vars", "missing_variable", "192.0.2.7"},
		{"consumer", "", "192.0.2.7"},
		{"vars_combinations", "$missing", "192.0.2.7"},
		{"vars_combinations", "prefix-${arg_tenant??fallback}", "prefix-"},
		{"vars_combinations", "${arg_missing??fallback}", "fallback"},
	} {
		if got := upstreamHashValue(r, tc.on, tc.key); got != tc.want {
			t.Errorf("%s/%s=%q want=%q", tc.on, tc.key, got, tc.want)
		}
	}
	r = apisixctx.WithRequestVars(r)
	apisixctx.RegisterRequestVar(r, "$consumer_name", "alice")
	if got := upstreamHashValue(r, "consumer", ""); got != "alice" {
		t.Fatal(got)
	}
}

func TestAlgorithmHealthPriorityAndSoleZeroNode(t *testing.T) {
	for _, algorithm := range []string{"chash", "least_conn", "ewma"} {
		t.Run(algorithm, func(t *testing.T) {
			lb := testAlgorithmBalancer(
				t,
				ClusterConfig{
					Type:       algorithm,
					HashKey:    "remote_addr",
					Targets:    map[string]int{"http://a:80": 1, "http://b:80": 1},
					Priorities: map[string]int{"http://a:80": 10},
					Checks: map[string]any{
						"passive": map[string]any{
							"unhealthy": map[string]any{"http_failures": 1, "http_statuses": []any{500}},
						},
					},
				},
			)
			r := httptest.NewRequest("GET", "http://gateway", nil)
			if got := lb.NextForRequest(r); got != "http://a:80" {
				t.Fatal(got)
			}
			algorithmScope(r).finish()
			lb.ReportHTTP("http://a:80", 500)
			r = httptest.NewRequest("GET", "http://gateway", nil)
			if got := lb.NextForRequest(r); got != "http://b:80" {
				t.Fatal(got)
			}
			algorithmScope(r).finish()
			lb.ReportHTTP("http://b:80", 500)
			r = httptest.NewRequest("GET", "http://gateway", nil)
			if got := lb.NextForRequest(r); got != "http://a:80" {
				t.Fatal(got)
			}
			algorithmScope(r).finish()
			sole := testAlgorithmBalancer(
				t,
				ClusterConfig{Type: algorithm, HashKey: "remote_addr", Targets: map[string]int{"http://sole:80": 0}},
			)
			r = httptest.NewRequest("GET", "http://gateway", nil)
			for range 3 {
				if got := sole.NextForRequest(r); got != "http://sole:80" {
					t.Fatal(got)
				}
			}
			algorithmScope(r).finish()
		})
	}
}

type algorithmRoundTripFunc func(*http.Request) (*http.Response, error)

func (f algorithmRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAlgorithmLeastConnectionsBodyLifetimeAndRetries(t *testing.T) {
	lb := testAlgorithmBalancer(
		t,
		ClusterConfig{Type: "least_conn", Targets: map[string]int{"http://a:80": 2, "http://b:80": 1}},
	)
	transport := &algorithmTransport{lb: lb, base: algorithmRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("payload"))}, nil
	})}
	requests := make([]*http.Request, 3)
	responses := make([]*http.Response, 3)
	for index, want := range []string{"http://a:80", "http://a:80", "http://b:80"} {
		requests[index] = httptest.NewRequest("GET", "http://gateway", nil)
		if got := lb.NextForRequest(requests[index]); got != want {
			t.Fatalf("request %d got=%s want=%s", index, got, want)
		}
		var err error
		responses[index], err = transport.RoundTrip(requests[index])
		if err != nil {
			t.Fatal(err)
		}
	}
	// Headers alone must not release active connections.
	if lb.stats["http://a:80"].inFlight != 2 || lb.stats["http://b:80"].inFlight != 1 {
		t.Fatal("released at headers")
	}
	_, _ = io.Copy(io.Discard, responses[0].Body)
	_ = responses[0].Body.Close()
	algorithmScope(requests[0]).finish()
	if lb.stats["http://a:80"].inFlight != 1 {
		t.Fatal("EOF/Close/finalizer did not release exactly once")
	}
	for i := 1; i < 3; i++ {
		_ = responses[i].Body.Close()
		algorithmScope(requests[i]).finish()
	}
	r := httptest.NewRequest("GET", "http://gateway", nil)
	if got := lb.NextForRequest(r); got != "http://a:80" {
		t.Fatal(got)
	}
	transport.base = algorithmRoundTripFunc(
		func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") },
	)
	_, _ = transport.RoundTrip(r)
	if lb.stats["http://a:80"].inFlight != 0 {
		t.Fatal("failure leaked active connection")
	}
	if got := lb.NextForRequest(r); got != "http://b:80" {
		t.Fatal(got)
	}
	algorithmScope(r).finish()
}

func TestAlgorithmSelectionReleasedWhenBeforeProxyFails(t *testing.T) {
	lb := testAlgorithmBalancer(t, ClusterConfig{Type: "least_conn", Targets: map[string]int{"http://a:80": 1}})
	handler := NewProxyHandler(algorithmRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("hook failed before cluster transport")
	}), func(r *http.Request) { r.URL, _ = url.Parse(lb.NextForRequest(r)) }, nil, func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(502) })
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "http://gateway", nil))
	if response.Code != 502 || lb.stats["http://a:80"].inFlight != 0 {
		t.Fatalf("status=%d inFlight=%d", response.Code, lb.stats["http://a:80"].inFlight)
	}
}

func TestAlgorithmEWMALearnsRealResponseLatency(t *testing.T) {
	fast := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "fast") }),
	)
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(w, "slow")
	}))
	defer slow.Close()
	lb := testAlgorithmBalancer(t, ClusterConfig{Type: "ewma", Targets: map[string]int{fast.URL: 1, slow.URL: 100}})
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	transport := &algorithmTransport{lb: lb, base: base}
	// Drive both endpoints through the normal selector/transport to seed scores.
	for _, target := range []string{slow.URL, fast.URL} {
		r := httptest.NewRequest("GET", "http://gateway", nil)
		r.RequestURI = ""
		other := fast.URL
		if target == fast.URL {
			other = slow.URL
		}
		state := priorityStateForRequest(r)
		state.tried[other] = struct{}{}
		if selected := lb.NextForRequest(r); selected != target {
			t.Fatal(selected)
		}
		r.URL, _ = url.Parse(target)
		response, err := transport.RoundTrip(r)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		algorithmScope(r).finish()
	}
	if lb.stats[slow.URL].ewma < 0.025 {
		t.Fatalf("body latency missing: %g", lb.stats[slow.URL].ewma)
	}
	for range 20 {
		r := httptest.NewRequest("GET", "http://gateway", nil)
		if target := lb.NextForRequest(r); target != fast.URL {
			t.Fatalf("selected slow weighted endpoint %s", target)
		}
		algorithmScope(r).finish()
	}
}

func TestAlgorithmClusterIdentityAndHealthOwner(t *testing.T) {
	config := ClusterConfig{
		Targets:   map[string]int{"http://a:80": 1, "http://b:80": 1},
		Transport: (&TransportOptionBuilder{}).Build(),
	}
	keys := make(map[ClusterKey]bool)
	for _, spec := range []struct{ algorithm, on, key string }{{"roundrobin", "", ""}, {"least_conn", "", ""}, {"ewma", "", ""}, {"chash", "header", "X-One"}, {"chash", "header", "X-Two"}, {"chash", "cookie", "X-One"}} {
		config.Type, config.HashOn, config.HashKey = spec.algorithm, spec.on, spec.key
		key, err := config.Key()
		if err != nil {
			t.Fatal(err)
		}
		if keys[key] {
			t.Fatal("algorithm identities collided")
		}
		keys[key] = true
		cluster, err := NewCluster(config, nil)
		if err != nil {
			t.Fatal(err)
		}
		if spec.algorithm != "roundrobin" && clusterHealthBalancer(cluster.lb) == nil {
			t.Fatal("lost health owner")
		}
		if err := cluster.CloseContext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAlgorithmHashModeKeyValidation(t *testing.T) {
	for _, tc := range []struct{ on, key string }{
		{"vars", "not_support"}, {"vars", "http_arbitrary"}, {"header", "X.Bad"}, {"cookie", "bad cookie"},
	} {
		if err := ValidateAlgorithm("chash", tc.on, tc.key); err == nil {
			t.Errorf("accepted %s key %q", tc.on, tc.key)
		}
	}
	for _, tc := range []struct{ on, key string }{
		{"vars", "server_name"}, {"vars", "arg_tenant-id"}, {"header", "custom_header"}, {"vars_combinations", "${arg_tenant??default}"},
	} {
		if err := ValidateAlgorithm("chash", tc.on, tc.key); err != nil {
			t.Errorf("rejected %s key %q: %v", tc.on, tc.key, err)
		}
	}
}

func TestAlgorithmHashUnderscoreHeaderAndServerVariables(t *testing.T) {
	r := httptest.NewRequest("GET", "http://gateway/", nil)
	r.Header.Set("custom_header", "custom-one")
	if got := upstreamHashValue(r, "header", "custom_header"); got != "custom-one" {
		t.Errorf("underscore header=%q", got)
	}
	r = r.WithContext(
		context.WithValue(
			r.Context(),
			http.LocalAddrContextKey,
			&net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 9080},
		),
	)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"server_addr": "192.0.2.30", "server_name": "_", "hostname": strings.ToLower(hostname)} {
		if got := upstreamHashValue(r, "vars", key); got != want {
			t.Errorf("%s=%q want=%q", key, got, want)
		}
	}
}

func TestAlgorithmHashRetriesKeepOriginalClientKey(t *testing.T) {
	lb := testAlgorithmBalancer(
		t,
		ClusterConfig{
			Type:    "chash",
			HashKey: "host",
			Targets: map[string]int{"http://a:80": 1, "http://b:80": 1, "http://c:80": 1},
		},
	)
	r := httptest.NewRequest("GET", "http://client.example/", nil)
	candidates := lb.rings[0].Candidates("client.example")
	defer func() { algorithmScope(r).finish() }()
	for _, want := range candidates {
		target := lb.NextForRequest(r)
		if target != want {
			t.Fatalf("retry target=%s want=%s original ring=%v", target, want, candidates)
		}
		selected, _ := url.Parse(target)
		r.Host = selected.Host
	}
}

func TestAlgorithmProtocolCompletionRecordsFinalAttempt(t *testing.T) {
	lb := testAlgorithmBalancer(
		t,
		ClusterConfig{Type: "ewma", Targets: map[string]int{"http://a:80": 1, "http://b:80": 1}},
	)
	request, finish := WithProtocolBalancing(httptest.NewRequest("GET", "http://gateway", nil))
	first := lb.NextForRequest(request)
	second := lb.NextForRequest(request)
	completeAttempt := BeginProtocolAttempt(request.Context())
	completeAttempt(true)
	if first == second {
		t.Fatal("retry repeated target")
	}
	finish()
	finish()
	for target, stats := range lb.stats {
		if stats.inFlight != 0 {
			t.Fatalf("%s leaked selection", target)
		}
	}
	if !lb.stats[first].touched.IsZero() {
		t.Fatal("retried attempt recorded as final response")
	}
	if lb.stats[second].touched.IsZero() {
		t.Fatal("completed protocol request did not record latency")
	}
}

func TestAlgorithmProtocolSelectionWithoutAttemptDoesNotRecordLatency(t *testing.T) {
	lb := testAlgorithmBalancer(t, ClusterConfig{Type: "ewma", Targets: map[string]int{"http://a:80": 1}})
	request, finish := WithProtocolBalancing(httptest.NewRequest("GET", "http://gateway", nil))
	target := lb.NextForRequest(request)
	finish()
	if stats := lb.stats[target]; stats.inFlight != 0 || !stats.touched.IsZero() {
		t.Fatalf("uncontacted backend stats = %+v", stats)
	}
}
