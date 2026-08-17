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
	setHTTPPluginAllowlist(t)

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

	builder := NewBuilder(nil)
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
			setHTTPPluginAllowlist(t)
			putRouteResource(t, test.routeID, []byte(test.payload))

			builder := NewBuilder(nil)
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
			setHTTPPluginAllowlist(t)
			putRouteResource(t, test.routeID, fmt.Appendf(nil,
				`{"id":%q,"uri":"/blank-host","host":%s}`,
				test.routeID,
				test.value,
			))

			builder := NewBuilder(nil)
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
