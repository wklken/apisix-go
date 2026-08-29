package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
)

func TestCompareDifferentialChaitinWAFRequiresEmbeddedHostOnBothSides(t *testing.T) {
	spec := differentialChaitinWAFCases()[0]
	observation := func(elapsed, address string) DifferentialObservation {
		return DifferentialObservation{
			Status: http.StatusForbidden,
			Headers: map[string][]string{
				"X-APISIX-CHAITIN-WAF":        {"yes"},
				"X-APISIX-CHAITIN-WAF-STATUS": {"403"},
				"X-APISIX-CHAITIN-WAF-ACTION": {"reject"},
				"X-APISIX-CHAITIN-WAF-TIME":   {elapsed},
			},
			Body:             "{\"code\": 403, \"success\":false, \"message\": \"blocked by Chaitin SafeLine Web Application Firewall\", \"event_id\": \"b3c6ce574dc24f09a01f634a39dca83b\"}\n",
			UpstreamFixture:  "waf",
			UpstreamAddress:  address,
			Host:             spec.Request.Host,
			SecurityDecision: "deny",
			Upstream: DifferentialUpstreamObservation{
				Received: true,
				Fixture:  "waf",
				Method:   http.MethodGet,
				Path:     "/hello",
				Host:     spec.Request.Host,
			},
		}
	}
	candidate := observation("0", "127.0.0.1:31001")
	oracle := observation("1", "127.0.0.1:1980")

	equal, detail, err := compareDifferentialChaitinWAFElapsedTime(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil || !equal {
		t.Fatalf("compare pinned T1K observations = %t, %q, %v", equal, detail, err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*DifferentialObservation, *DifferentialObservation)
	}{
		{
			name: "candidate Host",
			mutate: func(candidate, _ *DifferentialObservation) {
				candidate.Upstream.Host = candidate.UpstreamAddress
			},
		},
		{
			name: "oracle Host",
			mutate: func(_, oracle *DifferentialObservation) {
				oracle.Upstream.Host = oracle.UpstreamAddress
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			left := observation("0", "127.0.0.1:31001")
			right := observation("1", "127.0.0.1:1980")
			test.mutate(&left, &right)
			_, _, err := compareDifferentialChaitinWAFElapsedTime(
				spec,
				left,
				right,
				testNormalizationPolicy(),
			)
			if err == nil || !strings.Contains(err.Error(), "embedded Host") {
				t.Fatalf("compare malformed %s error = %v", test.name, err)
			}
		})
	}
}
