package route

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestRouteDecisionIndexSelectsMostSpecificWildcardWithManyUnrelatedSuffixes(t *testing.T) {
	decision := &routeDecisionIndex{}
	for index := range 1000 {
		decision.add(wildcardRoute{
			method: "*", hosts: []string{fmt.Sprintf("*.unrelated-%d.test", index)},
			registrationIndex: uint64(index),
		})
	}
	decision.add(wildcardRoute{
		method: "*", hosts: []string{"*.example.com"}, registrationIndex: 2000,
	})
	decision.add(wildcardRoute{
		method: "*", hosts: []string{"*.b.example.com"}, registrationIndex: 1002,
	})

	host := strings.Repeat("a.", 4096) + "b.example.com"
	candidate, matchedHost, ok := decision.lookup(host, "", 1, 1, http.MethodGet)
	if !matchedHost || !ok || candidate.route.registrationIndex != 1002 {
		t.Fatalf(
			"wildcard lookup = index:%d matched:%v ok:%v, want latest matching index 1002",
			candidate.route.registrationIndex,
			matchedHost,
			ok,
		)
	}
}

func TestRouteDecisionIndexFallsBackToBroaderWildcardAfterSpecificMethodMiss(t *testing.T) {
	decision := &routeDecisionIndex{}
	decision.add(wildcardRoute{
		method: http.MethodGet, hosts: []string{"*.example.com"}, registrationIndex: 2000,
	})
	decision.add(wildcardRoute{
		method: http.MethodPost, hosts: []string{"*.b.example.com"}, registrationIndex: 1002,
	})

	candidate, matchedHost, ok := decision.lookup("a.b.example.com", "", 1, 0, http.MethodGet)
	if !matchedHost || !ok || candidate.route.registrationIndex != 2000 {
		t.Fatalf(
			"wildcard fallback = index:%d matched:%v ok:%v, want broad GET index 2000",
			candidate.route.registrationIndex,
			matchedHost,
			ok,
		)
	}
}

func TestCompileHTTPSingularHost(t *testing.T) {
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes: testPreparedRoutes(resource.Route{
			ID: "singular-host-route", Uri: "/singular-host", Host: "api.example.com",
		}),
	})
	if err != nil {
		t.Fatalf("CompileHTTP() error = %v", err)
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
			snapshot.Handler().ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("Host %q status = %d, want %d", test.host, response.Code, test.want)
			}
		})
	}
}

func TestCompileHTTPRejectsHostAndHosts(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		id   string
	}{
		{
			name: "non-empty hosts",
			id:   "conflicting-host-route",
			raw:  `{"id":"conflicting-host-route","uri":"/conflicting-host","host":"api.example.com","hosts":["other.example.com"]}`,
		},
		{
			name: "empty hosts",
			id:   "conflicting-empty-hosts-route",
			raw:  `{"id":"conflicting-empty-hosts-route","uri":"/conflicting-hosts-empty","host":"api.example.com","hosts":[]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := CompileHTTP(context.Background(), CompileInput{
				Revision: 1,
				Routes:   testPreparedRoutes(testRouteFromJSON(t, test.raw)),
			})
			if err == nil || snapshot != nil {
				t.Fatalf("CompileHTTP() = (%T, %v), want nil snapshot and host conflict", snapshot, err)
			}
			if !strings.Contains(err.Error(), test.id) || !strings.Contains(err.Error(), "host and hosts") {
				t.Fatalf("CompileHTTP() error = %q, want route ID and host conflict", err)
			}
		})
	}
}

func TestCompileHTTPRejectsBlankHost(t *testing.T) {
	for _, test := range []struct {
		name  string
		id    string
		value string
	}{
		{name: "null", id: "null-host-route", value: "null"},
		{name: "empty", id: "empty-host-route", value: `""`},
		{name: "whitespace", id: "whitespace-host-route", value: `"  "`},
	} {
		t.Run(test.name, func(t *testing.T) {
			routeResource := testRouteFromJSON(t,
				`{"id":"`+test.id+`","uri":"/blank-host","host":`+test.value+`}`,
			)
			snapshot, err := CompileHTTP(context.Background(), CompileInput{
				Revision: 1,
				Routes:   testPreparedRoutes(routeResource),
			})
			if err == nil || snapshot != nil {
				t.Fatalf("CompileHTTP() = (%T, %v), want nil snapshot and blank host rejection", snapshot, err)
			}
			if !strings.Contains(err.Error(), test.id) || !strings.Contains(err.Error(), "host must not be empty") {
				t.Fatalf("CompileHTTP() error = %q, want route ID and empty host rejection", err)
			}
		})
	}
}

func TestCompileHTTPRejectsEmptyOrInvalidHosts(t *testing.T) {
	for _, test := range []struct {
		name  string
		id    string
		hosts string
		want  string
	}{
		{name: "empty hosts array", id: "empty-hosts-route", hosts: `[]`, want: "hosts must not be empty"},
		{name: "invalid wildcard", id: "invalid-wildcard-host-route", hosts: `["*foo.example.com"]`, want: "invalid"},
		{name: "mixed valid and invalid hosts", id: "mixed-hosts-route", hosts: `["api.example.com","*foo.example.com"]`, want: "invalid"},
		{name: "nested wildcard labels", id: "nested-wildcard-host-route", hosts: `["*.*.example.com"]`, want: "invalid"},
		{name: "wildcard suffix glob", id: "suffix-glob-host-route", hosts: `["*.suffix*"]`, want: "invalid"},
		{name: "wildcard suffix question", id: "suffix-question-host-route", hosts: `["*.foo?"]`, want: "invalid"},
		{name: "empty wildcard suffix", id: "empty-wildcard-host-route", hosts: `["*."]`, want: "invalid"},
		{name: "mixed valid and nested wildcard", id: "mixed-nested-wildcard-route", hosts: `["api.example.com","*.*.example.com"]`, want: "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			routeResource := testRouteFromJSON(t,
				`{"id":"`+test.id+`","uri":"/host-test","hosts":`+test.hosts+`}`,
			)
			snapshot, err := CompileHTTP(context.Background(), CompileInput{
				Revision: 1,
				Routes:   testPreparedRoutes(routeResource),
			})
			if err == nil || snapshot != nil {
				t.Fatalf("CompileHTTP() = (%T, %v), want nil snapshot and host rejection", snapshot, err)
			}
			if !strings.Contains(err.Error(), test.id) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CompileHTTP() error = %q, want route ID and %q", err, test.want)
			}
		})
	}
}

func TestCompileHTTPAcceptsOneLabelWildcardHost(t *testing.T) {
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes: testPreparedRoutes(resource.Route{
			ID: "one-label-wildcard-host", Uri: "/wildcard-host", Hosts: []string{"*.example.com"},
		}),
	})
	if err != nil {
		t.Fatalf("CompileHTTP() error = %v", err)
	}
	for _, host := range []string{"api.example.com", "a.b.example.com", ".example.com"} {
		request := httptest.NewRequest(http.MethodGet, "/wildcard-host", nil)
		request.Host = host
		response := httptest.NewRecorder()
		snapshot.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("Host %q status = %d, want %d", request.Host, response.Code, http.StatusNoContent)
		}
	}
}

func TestRouteHostRankMatchesOneLabelWildcardAndBareIPv6(t *testing.T) {
	if got := routeHostRank([]string{"*.example.com"}, "foo.example.com"); got != 1 {
		t.Fatalf("one-label wildcard rank = %d, want 1", got)
	}
	if got := routeHostRank([]string{"*.example.com"}, "a.b.example.com"); got != 1 {
		t.Fatalf("multi-label wildcard rank = %d, want 1", got)
	}
	if got := routeHostRank([]string{"*.example.com"}, ".example.com"); got != 1 {
		t.Fatalf("empty-prefix wildcard rank = %d, want 1", got)
	}
	if got := routeHostRank([]string{"::1"}, "[::1]"); got != 2 {
		t.Fatalf("bracketed IPv6 rank = %d, want exact 2", got)
	}
	if got := routeHostRank([]string{"api.example.com"}, "api.example.com:9080"); got != 2 {
		t.Fatalf("host with port rank = %d, want exact 2", got)
	}
}
