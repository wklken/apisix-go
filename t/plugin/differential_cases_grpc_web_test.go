package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialGRPCWebCasesCoverPinnedAPISIX317OptionsRequest(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "grpc-web-options-preflight",
		Plugin:  "grpc-web",
		RouteID: "differential-grpc-web-options",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  "differential-grpc-web-options",
				"uri": "/grpc/web/*",
				"plugins": map[string]any{
					"grpc-web": map[string]any{},
				},
				"upstream": map[string]any{
					"scheme": "grpc",
					"type":   "roundrobin",
					"nodes":  map[string]any{differentialFixturePlaceholder: 1},
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodOptions,
			Path:   "/grpc/web/a6.RouteService/GetRoute",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected",
			},
		},
		SecurityDecision: "not_applicable",
	}}

	got := differentialGRPCWebCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialGRPCWebCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
}
