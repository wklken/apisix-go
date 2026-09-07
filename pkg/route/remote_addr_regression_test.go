package route

import (
	"context"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestRouteRemoteAddressesMatchAndFallThrough(t *testing.T) {
	for _, test := range []struct {
		name    string
		remote  string
		remotes []string
	}{
		{"single IP", "192.0.2.4", nil},
		{"single CIDR", "192.0.2.0/24", nil},
		{"multiple", "", []string{"192.0.2.0/24", "2001:db8::/32"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := resource.Route{
				ID:          "restricted",
				Uri:         "/users/:name",
				Priority:    10,
				RemoteAddr:  test.remote,
				RemoteAddrs: test.remotes,
				Vars:        json.RawMessage(`[["arg_allow","==","yes"]]`),
			}
			snapshot, err := CompileHTTP(context.Background(), CompileInput{Revision: 1, Routes: []PreparedRoute{
				{Route: resource.Route{ID: "fallback", Uri: "/users/*"}, Handler: varsTestHandler("fallback")},
				{Route: route, Handler: varsTestHandler("restricted")},
			}})
			if err != nil {
				t.Fatal(err)
			}
			// Caller mutation must not change the detached IP condition.
			if len(test.remotes) > 0 {
				test.remotes[0] = "203.0.113.0/24"
			}
			for _, tc := range []struct{ address, query, override, want string }{
				{"192.0.2.4:1234", "?allow=yes", "", "restricted"},
				{"192.0.2.4:1234", "?allow=no", "", "fallback"},
				{"203.0.113.4:1234", "?allow=yes", "", "fallback"},
				{"203.0.113.4:1234", "?allow=yes", "192.0.2.4", "restricted"},
			} {
				request := httptest.NewRequest("GET", "/users/alice"+tc.query, nil)
				request.RemoteAddr = tc.address
				request.Header.Set("X-Forwarded-For", "192.0.2.4")
				if tc.override != "" {
					request = request.WithContext(
						context.WithValue(request.Context(), apisixctx.RemoteAddrKey, tc.override),
					)
				}
				response := httptest.NewRecorder()
				snapshot.Handler().ServeHTTP(response, request)
				if got := response.Header().Get("Selected"); got != tc.want {
					t.Fatalf(
						"address=%s override=%s query=%s selected=%q want=%q",
						tc.address,
						tc.override,
						tc.query,
						got,
						tc.want,
					)
				}
			}
		})
	}
}

func TestRouteRemoteAddressesWithoutVarsUseIPv6AndEmptyList(t *testing.T) {
	for _, test := range []struct {
		name      string
		addresses []string
		peer      string
		want      int
	}{
		{"IPv6 allowed", []string{"2001:db8::/32"}, "[2001:db8::1]:4321", 204},
		{"IPv6 denied", []string{"2001:db8::/32"}, "[2001:db9::1]:4321", 404},
		{"empty list", []string{}, "192.0.2.4:4321", 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := CompileHTTP(
				context.Background(),
				CompileInput{
					Revision: 1,
					Routes: []PreparedRoute{
						{
							Route:   resource.Route{ID: "remote", Uri: "/", RemoteAddrs: test.addresses},
							Handler: varsTestHandler("selected"),
						},
					},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("GET", "/", nil)
			request.RemoteAddr = test.peer
			response := httptest.NewRecorder()
			snapshot.Handler().ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d", response.Code, test.want)
			}
		})
	}
}
