package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
)

func TestCompareDifferentialLagoFixtureDelivery(t *testing.T) {
	spec := differentialCasesForPlugin("lago")[0]
	call := DifferentialUpstreamObservation{
		Received: true,
		Fixture:  spec.Fixture.Name,
		Method:   http.MethodPost,
		Path:     "/api/v1/events/batch",
		Host:     "127.0.0.1:31007",
		Headers: map[string][]string{
			"Authorization": {"Bearer differential-token"},
			"Content-Type":  {"application/json"},
		},
		Body: `{"events":[{"transaction_id":"differential-request","external_subscription_id":"differential-subscription","code":"differential-usage","timestamp":1700000000.25,"properties":{"route":"` + spec.RouteID + `","status":"200"}}]}`,
	}
	candidate, oracle := differentialLoggerComparatorPair(
		spec, "127.0.0.1:31007", "host.containers.internal:1980", call,
	)
	passed, diff, err := compareDifferentialLagoFixtureDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare Lago fixture delivery = %t, %q, %v", passed, diff, err)
	}
}

func TestCompareDifferentialLagoFixtureDeliveryRejectsLoosePayload(t *testing.T) {
	spec := differentialCasesForPlugin("lago")[0]
	call := DifferentialUpstreamObservation{
		Received: true,
		Fixture:  spec.Fixture.Name,
		Method:   http.MethodPost,
		Path:     "/api/v1/events/batch",
		Host:     "127.0.0.1:31007",
		Headers: map[string][]string{
			"Authorization": {"Bearer differential-token"},
			"Content-Type":  {"application/json"},
		},
		Body: `{"events":[{"transaction_id":"wrong","external_subscription_id":"differential-subscription","code":"differential-usage","timestamp":1700000000.25,"properties":{"route":"` + spec.RouteID + `","status":"200"}}]}`,
	}
	candidate, oracle := differentialLoggerComparatorPair(
		spec, "127.0.0.1:31007", "host.containers.internal:1980", call,
	)
	passed, _, err := compareDifferentialLagoFixtureDelivery(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err == nil || passed || !strings.Contains(err.Error(), "Lago payload") {
		t.Fatalf("compare loose Lago payload = %t, %v, want rejection", passed, err)
	}
}
