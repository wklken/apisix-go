package pluginintegration

import (
	"net/http"
	"testing"
)

func TestCompareDifferentialGRPCTranscodeUnaryValidatesWireAndJSONSemantics(t *testing.T) {
	spec := differentialGRPCTranscodeCases()[0]
	candidate := differentialGRPCTranscodeObservation(
		spec,
		"127.0.0.1:31001",
		`{"message":"Hello world"}`,
	)
	oracle := differentialGRPCTranscodeObservation(
		spec,
		"host.containers.internal:32001",
		"{\n  \"message\": \"Hello world\"\n}",
	)
	passed, detail, err := compareDifferentialGRPCTranscodeUnary(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compareDifferentialGRPCTranscodeUnary() error = %v", err)
	}
	if !passed {
		t.Fatalf("compareDifferentialGRPCTranscodeUnary() = false: %s", detail)
	}

	mutated := copyDifferentialObservation(candidate)
	mutated.Upstream.Body = string([]byte{0, 0, 0, 0, 7, 0x0a, 5, 'w', 'o', 'r', 'l', 'x'})
	if passed, _, err = compareDifferentialGRPCTranscodeUnary(
		spec, mutated, oracle, testNormalizationPolicy(),
	); err == nil || passed {
		t.Fatalf("mutated request frame comparison = %t/%v, want rejection", passed, err)
	}
}

func TestCompareDifferentialGRPCTranscodeUnaryRejectsWrongResponseMessage(t *testing.T) {
	spec := differentialGRPCTranscodeCases()[0]
	candidate := differentialGRPCTranscodeObservation(spec, "127.0.0.1:31001", `{"message":"wrong"}`)
	oracle := differentialGRPCTranscodeObservation(spec, "host.containers.internal:32001", `{"message":"Hello world"}`)
	if passed, _, err := compareDifferentialGRPCTranscodeUnary(
		spec, candidate, oracle, testNormalizationPolicy(),
	); err == nil || passed {
		t.Fatalf("wrong response JSON comparison = %t/%v, want rejection", passed, err)
	}
}

func TestCompareDifferentialGRPCTranscodeUnaryRejectsTrailingJSONGarbage(t *testing.T) {
	spec := differentialGRPCTranscodeCases()[0]
	candidate := differentialGRPCTranscodeObservation(
		spec,
		"127.0.0.1:31001",
		`{"message":"Hello world"} trailing`,
	)
	oracle := differentialGRPCTranscodeObservation(
		spec,
		"host.containers.internal:32001",
		`{"message":"Hello world"}`,
	)
	if passed, _, err := compareDifferentialGRPCTranscodeUnary(
		spec, candidate, oracle, testNormalizationPolicy(),
	); err == nil || passed {
		t.Fatalf("trailing JSON comparison = %t/%v, want rejection", passed, err)
	}
}

func differentialGRPCTranscodeObservation(
	spec DifferentialCase,
	upstreamAddress string,
	body string,
) DifferentialObservation {
	requestFrame := string([]byte{0, 0, 0, 0, 7, 0x0a, 5, 'w', 'o', 'r', 'l', 'd'})
	upstream := DifferentialUpstreamObservation{
		Received: true,
		Fixture:  spec.Fixture.Name,
		Method:   http.MethodPost,
		Path:     "/helloworld.Greeter/SayHello",
		Host:     upstreamAddress,
		Headers:  map[string][]string{"Content-Type": {"application/grpc"}},
		Body:     requestFrame,
	}
	return DifferentialObservation{
		Status:           http.StatusOK,
		Headers:          map[string][]string{"Content-Type": {"application/json"}},
		Body:             body,
		Host:             spec.Request.Host,
		SecurityDecision: spec.SecurityDecision,
		UpstreamFixture:  spec.Fixture.Name,
		UpstreamAddress:  upstreamAddress,
		Upstream:         upstream,
	}
}
