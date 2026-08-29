package pluginintegration

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCompareDifferentialLimitConnGlobalSharedCapacityAcceptsUnorderedBatch(t *testing.T) {
	spec := differentialCasesForPlugin("limit-conn")[0]
	candidate, oracle := differentialLimitConnComparatorObservations()
	candidateBefore := copyDifferentialObservation(candidate)
	oracleBefore := copyDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialLimitConnGlobalSharedCapacity(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil || !passed || diff != "" {
		t.Fatalf("compare pinned limit-conn observations = %t, %q, %v", passed, diff, err)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("limit-conn comparison mutated caller observations")
	}
}

func TestCompareDifferentialLimitConnGlobalSharedCapacityRejectsLooseContracts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation)
		want   string
	}{
		{
			name: "unpinned global config",
			mutate: func(spec *DifferentialCase, _, _ *DifferentialObservation) {
				rules := spec.Config["global_rules"].([]any)
				rule := rules[0].(map[string]any)
				plugins := rule["plugins"].(map[string]any)
				plugins["limit-conn"].(map[string]any)["conn"] = 3
			},
			want: "pinned",
		},
		{
			name: "fourth admission",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				for index := range 10 {
					if candidate.Steps[index].Status == http.StatusServiceUnavailable {
						candidate.Steps[index] = differentialLimitConnAllowedStep("mixed")
						return
					}
				}
			},
			want: "3 allowed and 7 rejected",
		},
		{
			name: "failed release probe",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.Steps[10] = differentialLimitConnOracleRejectedStep("allow")
			},
			want: "release probe",
		},
		{
			name: "extra upstream admission",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				candidate.UpstreamCalls = append(candidate.UpstreamCalls,
					candidate.UpstreamCalls[0])
			},
			want: "exactly four upstream calls",
		},
		{
			name: "unexpected batch route",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				oracle.UpstreamCalls[0].Path = "/other"
			},
			want: "batch upstream path",
		},
		{
			name: "candidate cannot use Oracle charset representation",
			mutate: func(_ *DifferentialCase, candidate, _ *DifferentialObservation) {
				for index := range 10 {
					if candidate.Steps[index].Status == http.StatusOK {
						candidate.Steps[index].Headers["Content-Type"] = []string{"text/plain; charset=utf-8"}
						return
					}
				}
			},
			want: "Content-Type",
		},
		{
			name: "Oracle wrong allowed content type",
			mutate: func(_ *DifferentialCase, _, oracle *DifferentialObservation) {
				for index := range 10 {
					if oracle.Steps[index].Status == http.StatusOK {
						oracle.Steps[index].Headers["Content-Type"] = []string{"application/json"}
						return
					}
				}
			},
			want: "Content-Type",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := differentialCasesForPlugin("limit-conn")[0]
			candidate, oracle := differentialLimitConnComparatorObservations()
			test.mutate(&spec, &candidate, &oracle)

			passed, _, err := compareDifferentialLimitConnGlobalSharedCapacity(
				spec, candidate, oracle, testNormalizationPolicy(),
			)
			if err == nil || passed || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("compare loose %s = %t, %v, want error containing %q", test.name, passed, err, test.want)
			}
		})
	}
}

func differentialLimitConnComparatorObservations() (DifferentialObservation, DifferentialObservation) {
	candidateStatuses := []int{503, 200, 503, 503, 200, 503, 503, 200, 503, 503}
	oracleStatuses := []int{200, 503, 503, 200, 503, 503, 200, 503, 503, 503}
	candidate := differentialLimitConnTestObservation(candidateStatuses, false)
	oracle := differentialLimitConnTestObservation(oracleStatuses, true)
	return candidate, oracle
}

func differentialLimitConnTestObservation(statuses []int, oracle bool) DifferentialObservation {
	steps := make([]DifferentialStepObservation, 0, len(statuses)+1)
	for _, status := range statuses {
		if status == http.StatusOK {
			steps = append(steps, differentialLimitConnAllowedStepForSide("mixed", oracle))
			continue
		}
		if oracle {
			steps = append(steps, differentialLimitConnOracleRejectedStep("mixed"))
		} else {
			steps = append(steps, differentialLimitConnCandidateRejectedStep("mixed"))
		}
	}
	steps = append(steps, differentialLimitConnAllowedStepForSide("allow", oracle))

	address := "127.0.0.1:31980"
	paths := []string{"/limit-a", "/limit-b", "/limit-a", "/limit-b"}
	if oracle {
		address = "host.containers.internal:1980"
		paths = []string{"/limit-b", "/limit-b", "/limit-a", "/limit-b"}
	}
	calls := make([]DifferentialUpstreamObservation, 0, len(paths))
	for _, path := range paths {
		calls = append(calls, DifferentialUpstreamObservation{
			Received: true,
			Fixture:  "limit-conn-origin",
			Method:   http.MethodGet,
			Path:     path,
			Host:     "differential.example.test",
		})
	}
	return DifferentialObservation{
		Steps:           steps,
		UpstreamFixture: "limit-conn-origin",
		UpstreamAddress: address,
		Upstream:        calls[len(calls)-1],
		UpstreamCalls:   calls,
	}
}

func differentialLimitConnAllowedStep(decision string) DifferentialStepObservation {
	return differentialLimitConnAllowedStepForSide(decision, false)
}

func differentialLimitConnAllowedStepForSide(decision string, oracle bool) DifferentialStepObservation {
	contentType := "text/plain"
	if oracle {
		contentType = "text/plain; charset=utf-8"
	}
	return DifferentialStepObservation{
		Status: http.StatusOK,
		Headers: map[string][]string{
			"Content-Type":   {contentType},
			"Content-Length": {fmt.Sprint(len("hello world"))},
		},
		Body:             "hello world",
		Host:             "gateway.example.test",
		SecurityDecision: decision,
	}
}

func differentialLimitConnCandidateRejectedStep(decision string) DifferentialStepObservation {
	return DifferentialStepObservation{
		Status:           http.StatusServiceUnavailable,
		Headers:          map[string][]string{"Content-Length": {"0"}},
		Host:             "gateway.example.test",
		SecurityDecision: decision,
	}
}

func differentialLimitConnOracleRejectedStep(decision string) DifferentialStepObservation {
	return DifferentialStepObservation{
		Status: http.StatusServiceUnavailable,
		Headers: map[string][]string{
			"Content-Type":   {"text/html; charset=utf-8"},
			"Content-Length": {fmt.Sprint(len(differentialLimitCountOracle503Body))},
		},
		Body:             differentialLimitCountOracle503Body,
		Host:             "gateway.example.test",
		SecurityDecision: decision,
	}
}
