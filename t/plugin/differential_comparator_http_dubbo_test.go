package pluginintegration

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func TestCompareDifferentialHTTPDubboPOJOValidatesExactFastJSONExchange(t *testing.T) {
	spec := differentialHTTPDubboCases()[0]
	candidate := differentialHTTPDubboObservation(spec, "127.0.0.1:31001")
	oracle := differentialHTTPDubboObservation(spec, "host.containers.internal:32001")
	passed, detail, err := compareDifferentialHTTPDubboPOJO(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compareDifferentialHTTPDubboPOJO() error = %v", err)
	}
	if !passed {
		t.Fatalf("compareDifferentialHTTPDubboPOJO() = false: %s", detail)
	}

	mutated := copyDifferentialObservation(candidate)
	frame := []byte(mutated.Upstream.Body)
	frame[len(frame)-4] = 'X'
	mutated.Upstream.Body = string(frame)
	if passed, _, err = compareDifferentialHTTPDubboPOJO(
		spec, mutated, oracle, testNormalizationPolicy(),
	); err == nil || passed {
		t.Fatalf("mutated request frame comparison = %t/%v, want rejection", passed, err)
	}
}

func TestCompareDifferentialHTTPDubboPOJORejectsWrongGatewayBody(t *testing.T) {
	spec := differentialHTTPDubboCases()[0]
	candidate := differentialHTTPDubboObservation(spec, "127.0.0.1:31001")
	candidate.Body = `{"aString":"wrong"}`
	oracle := differentialHTTPDubboObservation(spec, "host.containers.internal:32001")
	if passed, _, err := compareDifferentialHTTPDubboPOJO(
		spec, candidate, oracle, testNormalizationPolicy(),
	); err == nil || passed {
		t.Fatalf("wrong gateway body comparison = %t/%v, want rejection", passed, err)
	}
}

func differentialHTTPDubboObservation(
	spec DifferentialCase,
	upstreamAddress string,
) DifferentialObservation {
	frame, err := base64.StdEncoding.DecodeString(differentialHTTPDubboRequestFrameBase64)
	if err != nil {
		panic(err)
	}
	return DifferentialObservation{
		Status:           http.StatusOK,
		Headers:          map[string][]string{},
		Body:             differentialHTTPDubboPOJOJSON,
		Host:             spec.Request.Host,
		SecurityDecision: spec.SecurityDecision,
		UpstreamFixture:  spec.Fixture.Name,
		UpstreamAddress:  upstreamAddress,
		Upstream: DifferentialUpstreamObservation{
			Received: true,
			Fixture:  spec.Fixture.Name,
			Method:   differentialHTTPDubboMethod,
			Path:     differentialHTTPDubboServiceName + "/" + differentialHTTPDubboMethodName,
			Host:     differentialHTTPDubboServiceVersion,
			Headers: map[string][]string{
				differentialHTTPDubboParamsTypeHeader: {differentialHTTPDubboParamsTypeDesc},
			},
			Body: string(frame),
		},
	}
}
