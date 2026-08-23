package route

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildStrictSingularHost(t *testing.T) {
	ensureRouteStore(t)
	effective := httpPluginAllowlist()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	const routeID = "singular-host-route"
	putRouteResource(t, routeID, fmt.Appendf(nil,
		`{"id":%q,"uri":"/singular-host","host":"api.example.com","upstream":{"type":"roundrobin","nodes":{%q:1}}}`,
		routeID,
		routePriorityNode(t, upstream.URL),
	))

	builder := NewBuilder(nil, effective, testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil {
		t.Fatalf("BuildStrict() error = %v", err)
	}

	for _, test := range []struct {
		name string
		host string
		want int
	}{
		{name: "configured host", host: "api.example.com", want: http.StatusNoContent},
		{name: "wrong host", host: "other.example.com", want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/singular-host", nil)
			request.Host = test.host
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("Host %q status = %d, want %d", test.host, response.Code, test.want)
			}
		})
	}
}

func TestBuildStrictRejectsHostAndHosts(t *testing.T) {
	for _, test := range []struct {
		name    string
		routeID string
		payload string
	}{
		{
			name:    "non-empty hosts",
			routeID: "conflicting-host-route",
			payload: `{"id":"conflicting-host-route","uri":"/conflicting-host","host":"api.example.com","hosts":["other.example.com"]}`,
		},
		{
			name:    "empty hosts",
			routeID: "conflicting-empty-hosts-route",
			payload: `{"id":"conflicting-empty-hosts-route","uri":"/conflicting-hosts-empty","host":"api.example.com","hosts":[]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ensureRouteStore(t)
			effective := httpPluginAllowlist()
			putRouteResource(t, test.routeID, []byte(test.payload))

			builder := NewBuilder(nil, effective, testDataEncryptionResolver())
			t.Cleanup(builder.Stop)
			handler, err := builder.BuildStrict()
			if err == nil || handler != nil {
				t.Fatalf("BuildStrict() = (%T, %v), want nil handler and host conflict error", handler, err)
			}
			if !strings.Contains(err.Error(), test.routeID) || !strings.Contains(err.Error(), "host and hosts") {
				t.Fatalf("BuildStrict() error = %q, want route ID and host conflict", err)
			}
		})
	}
}

func TestBuildStrictRejectsBlankHost(t *testing.T) {
	for _, test := range []struct {
		name    string
		routeID string
		value   string
	}{
		{name: "null", routeID: "null-host-route", value: "null"},
		{name: "empty", routeID: "empty-host-route", value: `""`},
		{name: "whitespace", routeID: "whitespace-host-route", value: `"  "`},
	} {
		t.Run(test.name, func(t *testing.T) {
			ensureRouteStore(t)
			effective := httpPluginAllowlist()
			putRouteResource(t, test.routeID, fmt.Appendf(nil,
				`{"id":%q,"uri":"/blank-host","host":%s}`,
				test.routeID,
				test.value,
			))

			builder := NewBuilder(nil, effective, testDataEncryptionResolver())
			t.Cleanup(builder.Stop)
			handler, err := builder.BuildStrict()
			if err == nil || handler != nil {
				t.Fatalf("BuildStrict() = (%T, %v), want nil handler and blank host error", handler, err)
			}
			if !strings.Contains(err.Error(), test.routeID) ||
				!strings.Contains(err.Error(), "host must not be empty") {
				t.Fatalf("BuildStrict() error = %q, want route ID and empty host rejection", err)
			}
		})
	}
}

func TestBuildStrictRejectsEmptyOrInvalidHosts(t *testing.T) {
	for _, test := range []struct {
		name    string
		routeID string
		payload string
		want    string
	}{
		{
			name:    "empty hosts array",
			routeID: "empty-hosts-route",
			payload: `{"id":"empty-hosts-route","uri":"/empty-hosts","hosts":[],"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:1":1}}}`,
			want:    "hosts must not be empty",
		},
		{
			name:    "invalid wildcard",
			routeID: "invalid-wildcard-host-route",
			payload: `{"id":"invalid-wildcard-host-route","uri":"/invalid-wildcard","hosts":["*foo.example.com"],"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:1":1}}}`,
			want:    "invalid",
		},
		{
			name:    "mixed valid and invalid hosts",
			routeID: "mixed-hosts-route",
			payload: `{"id":"mixed-hosts-route","uri":"/mixed-hosts","hosts":["api.example.com","*foo.example.com"],"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:1":1}}}`,
			want:    "invalid",
		},
		{
			name:    "nested wildcard labels",
			routeID: "nested-wildcard-host-route",
			payload: `{"id":"nested-wildcard-host-route","uri":"/nested-wildcard","hosts":["*.*.example.com"],"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:1":1}}}`,
			want:    "invalid",
		},
		{
			name:    "wildcard suffix glob",
			routeID: "suffix-glob-host-route",
			payload: `{"id":"suffix-glob-host-route","uri":"/suffix-glob","hosts":["*.suffix*"],"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:1":1}}}`,
			want:    "invalid",
		},
		{
			name:    "wildcard suffix question",
			routeID: "suffix-question-host-route",
			payload: `{"id":"suffix-question-host-route","uri":"/suffix-question","hosts":["*.foo?"],"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:1":1}}}`,
			want:    "invalid",
		},
		{
			name:    "empty wildcard suffix",
			routeID: "empty-wildcard-host-route",
			payload: `{"id":"empty-wildcard-host-route","uri":"/empty-wildcard","hosts":["*."],"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:1":1}}}`,
			want:    "invalid",
		},
		{
			name:    "mixed valid and nested wildcard",
			routeID: "mixed-nested-wildcard-route",
			payload: `{"id":"mixed-nested-wildcard-route","uri":"/mixed-nested","hosts":["api.example.com","*.*.example.com"],"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:1":1}}}`,
			want:    "invalid",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ensureRouteStore(t)
			effective := httpPluginAllowlist()
			putRouteResource(t, test.routeID, []byte(test.payload))

			builder := NewBuilder(nil, effective, testDataEncryptionResolver())
			t.Cleanup(builder.Stop)
			handler, err := builder.BuildStrict()
			if err == nil || handler != nil {
				t.Fatalf("BuildStrict() = (%T, %v), want nil handler and host rejection", handler, err)
			}
			if !strings.Contains(err.Error(), test.routeID) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("BuildStrict() error = %q, want route ID and %q", err, test.want)
			}
		})
	}
}

func TestBuildStrictAcceptsOneLabelWildcardHost(t *testing.T) {
	ensureRouteStore(t)
	effective := httpPluginAllowlist()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	const routeID = "one-label-wildcard-host"
	putRouteResource(t, routeID, fmt.Appendf(nil,
		`{"id":%q,"uri":"/wildcard-host","hosts":["*.example.com"],"upstream":{"type":"roundrobin","nodes":{%q:1}}}`,
		routeID,
		routePriorityNode(t, upstream.URL),
	))

	builder := NewBuilder(nil, effective, testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil {
		t.Fatalf("BuildStrict() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/wildcard-host", nil)
	request.Host = "api.example.com"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("Host %q status = %d, want %d", request.Host, response.Code, http.StatusNoContent)
	}
}

func TestRouteHostRankMatchesOneLabelWildcardAndBareIPv6(t *testing.T) {
	if got := routeHostRank([]string{"*.example.com"}, "foo.example.com"); got != 1 {
		t.Fatalf("one-label wildcard rank = %d, want 1", got)
	}
	if got := routeHostRank([]string{"*.example.com"}, "a.b.example.com"); got != -1 {
		t.Fatalf("multi-label wildcard rank = %d, want -1", got)
	}
	if got := routeHostRank([]string{"::1"}, "[::1]"); got != 2 {
		t.Fatalf("bracketed IPv6 rank = %d, want exact 2", got)
	}
	if got := routeHostRank([]string{"api.example.com"}, "api.example.com:9080"); got != 2 {
		t.Fatalf("host with port rank = %d, want exact 2", got)
	}
}
