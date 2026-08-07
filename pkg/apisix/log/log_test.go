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
