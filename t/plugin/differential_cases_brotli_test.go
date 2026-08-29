package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialBrotliCasesMatchPinnedAPISIX317DefaultCompression(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	const body = "0123456789\n012345678"
	want := []DifferentialCase{{
		Name:             "brotli-default-compression",
		Plugin:           "brotli",
		RouteID:          "differential-brotli-default-compression",
		ComparisonPolicy: "compressed-response-semantics",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  "differential-brotli-default-compression",
				"uri": "/echo",
				"plugins": map[string]any{
					"brotli": map[string]any{},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/echo",
			Host:   "gateway.example.test",
			Headers: map[string]string{
				"Accept-Encoding": "br",
				"Content-Type":    "text/html",
			},
			Body: body,
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Headers: map[string]string{
					"Content-Type": "text/html",
				},
				Body: body,
			},
		},
		SecurityDecision: "not_applicable",
	}}

	if got := differentialBrotliCases(); !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialBrotliCases() = %#v, want %#v", got, want)
	}
	if len(want[0].RouteID) > 64 {
		t.Fatalf("route ID length = %d, want <= 64", len(want[0].RouteID))
	}
}
