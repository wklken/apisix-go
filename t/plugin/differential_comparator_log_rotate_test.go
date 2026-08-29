package pluginintegration

import (
	"net/http"
	"testing"
)

func TestCompareDifferentialLogRotateValidatesDirectorySemantics(t *testing.T) {
	spec := differentialLogRotateCases()[0]
	candidate := differentialLogRotateObservationForTest(
		spec,
		"127.0.0.1:31001",
		`{"archive_name":"2026-08-29_12-34-56__access.log.tar.gz","archive_member":"2026-08-29_12-34-56__access.log","archive_content":"rotate-me-marker\n{\"path\":\"/rotate\"}\n","current_content":"{\"level\":\"info\",\"path\":\"/after-rotate\"}\n","sentinel_content":"keep-me\n"}`,
	)
	oracle := differentialLogRotateObservationForTest(
		spec,
		"host.containers.internal:32001",
		`{"archive_name":"2026-08-29_04-34-57__access.log.tar.gz","archive_member":"2026-08-29_04-34-57__access.log","archive_content":"rotate-me-marker\n127.0.0.1 access\n","current_content":"127.0.0.1 access\n{\"path\":\"/after-rotate\"}\n","sentinel_content":"keep-me\n"}`,
	)
	passed, detail, err := compareDifferentialLogRotate(
		spec, candidate, oracle, testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compareDifferentialLogRotate() error = %v", err)
	}
	if !passed {
		t.Fatalf("compareDifferentialLogRotate() = false: %s", detail)
	}

	mutated := copyDifferentialObservation(candidate)
	mutatedContent := `{"archive_name":"2026-08-29_12-34-56__access.log.tar.gz","archive_member":"2026-08-29_12-34-56__access.log","archive_content":"wrong","current_content":"{\"path\":\"/after-rotate\"}\n","sentinel_content":"keep-me\n"}`
	mutated.File = &DifferentialFileObservation{
		Name: differentialLogRotateObservationName, Exists: true,
		Size: int64(len(mutatedContent)), Content: mutatedContent,
	}
	if passed, _, err = compareDifferentialLogRotate(
		spec, mutated, oracle, testNormalizationPolicy(),
	); err == nil || passed {
		t.Fatalf("missing pre-marker comparison = %t/%v, want rejection", passed, err)
	}
}

func differentialLogRotateObservationForTest(
	spec DifferentialCase,
	upstreamAddress string,
	fileContent string,
) DifferentialObservation {
	steps := make([]DifferentialStepObservation, 0, len(spec.Steps))
	upstreamCalls := make([]DifferentialUpstreamObservation, 0, len(spec.Steps))
	for _, step := range spec.Steps {
		steps = append(steps, DifferentialStepObservation{
			Status: http.StatusOK, Headers: map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}},
			Body: "ok", Host: step.Request.Host, SecurityDecision: step.SecurityDecision,
		})
		upstreamCalls = append(upstreamCalls, DifferentialUpstreamObservation{
			Received: true, Fixture: spec.Fixture.Name, Method: http.MethodGet,
			Path: step.Request.Path, Host: upstreamAddress,
		})
	}
	return DifferentialObservation{
		Steps:           steps,
		UpstreamFixture: spec.Fixture.Name,
		UpstreamAddress: upstreamAddress,
		Upstream:        upstreamCalls[len(upstreamCalls)-1],
		UpstreamCalls:   upstreamCalls,
		File: &DifferentialFileObservation{
			Name: differentialLogRotateObservationName, Exists: true,
			Size: int64(len(fileContent)), Content: fileContent,
		},
	}
}
