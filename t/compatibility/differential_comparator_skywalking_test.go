package pluginintegration

import (
	"encoding/base64"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialSkyWalkingSW8FullSamplingAcceptsOnlyValidatedRandomIDs(t *testing.T) {
	spec := differentialCasesForPlugin("skywalking")[0]
	candidate := differentialSkyWalkingSW8Observation(spec, "127.0.0.1:31121", "candidate-trace", "candidate-segment")
	oracle := differentialSkyWalkingSW8Observation(spec, "127.0.0.1:1980", "oracle-trace", "oracle-segment")
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialSkyWalkingSW8FullSampling(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare valid SW8 observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("SkyWalking SW8 comparison mutated caller observations")
	}
}

func TestCompareDifferentialSkyWalkingSW8FullSamplingAcceptsEquivalentBase64URLPadding(t *testing.T) {
	spec := differentialCasesForPlugin("skywalking")[0]
	candidate := differentialSkyWalkingSW8Observation(spec, "127.0.0.1:31121", "candidate-trace", "candidate-segment")
	oracle := differentialSkyWalkingSW8Observation(spec, "127.0.0.1:1980", "oracle-trace", "oracle-segment")
	oracleCall := &oracle.UpstreamCalls[0]
	parts := strings.Split(oracleCall.Headers["Sw8"][0], "-")
	for _, index := range []int{1, 2, 4, 5, 6, 7} {
		decoded, err := base64.RawURLEncoding.DecodeString(parts[index])
		if err != nil {
			t.Fatalf("decode SW8 field %d: %v", index+1, err)
		}
		parts[index] = base64.URLEncoding.EncodeToString(decoded)
	}
	oracleCall.Headers["Sw8"] = []string{strings.Join(parts, "-")}
	oracle.Upstream = *oracleCall

	passed, diff, err := compareDifferentialSkyWalkingSW8FullSampling(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare padded and unpadded SW8 = %t, %q, %v", passed, diff, err)
	}
}

func TestCompareDifferentialSkyWalkingSW8FullSamplingRejectsInvalidPropagation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "unpinned case",
			mutate: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				spec.Fixture.RequestWindowQuietMillis++
			},
			want: "pinned",
		},
		{
			name: "missing SW8 field",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := &candidate.UpstreamCalls[0]
				call.Headers["Sw8"] = []string{strings.Join(strings.Split(call.Headers["Sw8"][0], "-")[:7], "-")}
				candidate.Upstream = *call
			},
			want: "exactly 8 fields",
		},
		{
			name: "bad base64url encoding",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := &oracle.UpstreamCalls[0]
				parts := strings.Split(call.Headers["Sw8"][0], "-")
				parts[4] = "***"
				call.Headers["Sw8"] = []string{strings.Join(parts, "-")}
				oracle.Upstream = *call
			},
			want: "base64url",
		},
		{
			name: "sampling bit disabled",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := &candidate.UpstreamCalls[0]
				parts := strings.Split(call.Headers["Sw8"][0], "-")
				parts[0] = "0"
				call.Headers["Sw8"] = []string{strings.Join(parts, "-")}
				candidate.Upstream = *call
			},
			want: "sample flag",
		},
		{
			name: "wrong propagated span",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				call := &candidate.UpstreamCalls[0]
				parts := strings.Split(call.Headers["Sw8"][0], "-")
				parts[3] = "0"
				call.Headers["Sw8"] = []string{strings.Join(parts, "-")}
				candidate.Upstream = *call
			},
			want: "span id",
		},
		{
			name: "wrong peer service",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				call := &oracle.UpstreamCalls[0]
				parts := strings.Split(call.Headers["Sw8"][0], "-")
				parts[7] = base64.RawURLEncoding.EncodeToString([]byte("wrong peer"))
				call.Headers["Sw8"] = []string{strings.Join(parts, "-")}
				oracle.Upstream = *call
			},
			want: "peer service",
		},
		{
			name: "extra upstream call",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls = append(candidate.UpstreamCalls, candidate.UpstreamCalls[0])
			},
			want: "exactly 1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialCasesForPlugin("skywalking")[0]
			candidate := differentialSkyWalkingSW8Observation(
				spec,
				"127.0.0.1:31121",
				"candidate-trace",
				"candidate-segment",
			)
			oracle := differentialSkyWalkingSW8Observation(spec, "127.0.0.1:1980", "oracle-trace", "oracle-segment")
			test.mutate(&spec, &candidate, &oracle)

			passed, _, err := compareDifferentialSkyWalkingSW8FullSampling(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare invalid SW8 propagation = %t, %v, want %q", passed, err, test.want)
			}
		})
	}
}

func TestCompareDifferentialSkyWalkingSW8FullSamplingRejectsWrongOperation(t *testing.T) {
	spec := differentialCasesForPlugin("skywalking")[0]
	candidate := differentialSkyWalkingSW8Observation(spec, "127.0.0.1:31121", "candidate-trace", "candidate-segment")
	oracle := differentialSkyWalkingSW8Observation(spec, "127.0.0.1:1980", "oracle-trace", "oracle-segment")
	call := &candidate.UpstreamCalls[0]
	parts := strings.Split(call.Headers["Sw8"][0], "-")
	parts[6] = base64.RawURLEncoding.EncodeToString([]byte("/different-operation"))
	call.Headers["Sw8"] = []string{strings.Join(parts, "-")}
	candidate.Upstream = *call

	passed, _, err := compareDifferentialSkyWalkingSW8FullSampling(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err == nil || passed || !strings.Contains(err.Error(), "operation") {
		t.Fatalf("compare wrong SW8 operation = %t, %v", passed, err)
	}
}

func differentialSkyWalkingSW8Observation(
	spec DifferentialCase,
	address string,
	traceID string,
	segmentID string,
) DifferentialObservation {
	sw8 := strings.Join([]string{
		"1",
		base64.RawURLEncoding.EncodeToString([]byte(traceID)),
		base64.RawURLEncoding.EncodeToString([]byte(segmentID)),
		"1",
		base64.RawURLEncoding.EncodeToString([]byte("APISIX")),
		base64.RawURLEncoding.EncodeToString([]byte("APISIX Instance Name")),
		base64.RawURLEncoding.EncodeToString([]byte("/opentracing")),
		base64.RawURLEncoding.EncodeToString([]byte("upstream service")),
	}, "-")
	call := DifferentialUpstreamObservation{
		Received: true,
		Fixture:  spec.Fixture.Name,
		Method:   http.MethodGet,
		Path:     "/opentracing",
		Host:     "differential.example.test",
		Headers:  map[string][]string{"Sw8": {sw8}},
	}
	return DifferentialObservation{
		Steps: []DifferentialStepObservation{{
			Status:           http.StatusOK,
			Body:             "opentracing",
			Host:             "gateway.example.test",
			SecurityDecision: "not_applicable",
		}},
		UpstreamFixture: spec.Fixture.Name,
		UpstreamAddress: address,
		Upstream:        call,
		UpstreamCalls:   []DifferentialUpstreamObservation{call},
	}
}
