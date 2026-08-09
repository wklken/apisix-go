package log

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestGetFieldResolvesOwnersAndPassesThroughLiterals(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://example.test/orders", nil)
	request = ctx.WithApisixVars(request, map[string]string{"$route_id": "route-1"})
	request = ctx.WithRequestVars(request)
	ctx.RegisterRequestVar(request, "$status", 201)

	tests := []struct {
		key  string
		want any
	}{
		{key: "literal", want: "literal"},
		{key: "$request_method", want: http.MethodPost},
		{key: "$route_id", want: "route-1"},
		{key: "$status", want: 201},
		{key: "$not_registered", want: ""},
	}
	for _, test := range tests {
		if got := GetField(request, test.key); got != test.want {
			t.Fatalf("GetField(%q) = %#v, want %#v", test.key, got, test.want)
		}
	}
}

func TestGetFieldResolvesDynamicRegisteredVariables(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/orders", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	request = ctx.WithRequestVars(request)
	ctx.RegisterApisixVar(request, "$balancer_ip", "192.0.2.10")
	ctx.RegisterApisixVar(request, "$balancer_port", "8080")
	ctx.RegisterRequestVar(request, "$upstream_addr", "192.0.2.11:8081")
	ctx.RegisterRequestVar(request, "$response_source", "upstream")
	ctx.RegisterRequestVar(request, "$upstream_latency", int64(7))

	tests := []struct {
		key  string
		want any
	}{
		{key: "$balancer_ip", want: "192.0.2.10"},
		{key: "$balancer_port", want: "8080"},
		{key: "$upstream_addr", want: "192.0.2.11:8081"},
		{key: "$response_source", want: "upstream"},
		{key: "$upstream_latency", want: int64(7)},
		{key: "$matched_host", want: ""},
	}
	for _, test := range tests {
		if got := GetField(request, test.key); got != test.want {
			t.Fatalf("GetField(%q) = %#v, want %#v", test.key, got, test.want)
		}
	}
}

func TestGetFieldsExpandsVariableValues(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://example.test/orders", nil)

	fields := GetFields(request, map[string]string{"method": "$request_method", "tag": "edge"})
	want := map[string]any{"method": "POST", "tag": "edge"}
	for key, value := range want {
		if got := fields[key]; got != value {
			t.Fatalf("GetFields()[%q] = %#v, want %#v", key, got, value)
		}
	}
}
