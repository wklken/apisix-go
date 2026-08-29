package pluginintegration

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"reflect"
	"strings"
)

const differentialLogRotatePolicy = "log-rotate-directory-state"

func init() {
	differentialComparatorRegistry[differentialLogRotatePolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"log-rotate": {}},
		compare:        compareDifferentialLogRotate,
	}
}

func compareDifferentialLogRotate(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialLogRotateCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned log-rotate case",
			spec.ComparisonPolicy,
		)
	}
	left = copyDifferentialObservation(left)
	right = copyDifferentialObservation(right)
	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		if err := normalizeDifferentialLogRotateObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialLogRotateObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if observation.Status != 0 || observation.Body != "" || len(observation.Headers) != 0 ||
		observation.Host != "" || observation.SNI != "" || observation.SecurityDecision != "" ||
		observation.RetryCount != 0 || len(observation.RouteObserver) != 0 ||
		len(observation.Steps) != len(spec.Steps) {
		return fmt.Errorf("%s log-rotate sequence envelope is not exact", side)
	}
	for index := range spec.Steps {
		step := &observation.Steps[index]
		want := spec.Steps[index]
		if step.Status != http.StatusOK || step.Body != "ok" || step.Host != want.Request.Host ||
			step.SNI != "" || step.SecurityDecision != want.SecurityDecision {
			return fmt.Errorf("%s log-rotate step %d response is not exact", side, index)
		}
		if err := normalizeDifferentialLogRotateContentType(step.Headers, "text/plain"); err != nil {
			return fmt.Errorf("%s log-rotate step %d headers: %w", side, index, err)
		}
	}
	if observation.UpstreamFixture != spec.Fixture.Name || observation.UpstreamAddress == "" ||
		len(observation.UpstreamCalls) != len(spec.Steps) {
		return fmt.Errorf("%s log-rotate upstream sequence is not exact", side)
	}
	for index := range observation.UpstreamCalls {
		call := &observation.UpstreamCalls[index]
		want := spec.Steps[index].Request
		if !call.Received || call.Fixture != spec.Fixture.Name || call.Method != want.Method ||
			call.Path != want.Path || call.Host == "" || len(call.Headers) != 0 || call.Body != "" {
			return fmt.Errorf("%s log-rotate upstream call %d is not exact", side, index)
		}
		call.Host = "fixture:" + spec.Fixture.Name
	}
	last := observation.UpstreamCalls[len(observation.UpstreamCalls)-1]
	if !observation.Upstream.Received || observation.Upstream.Fixture != last.Fixture ||
		observation.Upstream.Method != last.Method || observation.Upstream.Path != last.Path ||
		observation.Upstream.Host == "" || len(observation.Upstream.Headers) != 0 ||
		observation.Upstream.Body != "" {
		return fmt.Errorf("%s log-rotate selected upstream is not the final exact call", side)
	}
	observation.Upstream.Host = last.Host
	canonicalFile, err := normalizeDifferentialLogRotateFile(side, observation.File)
	if err != nil {
		return err
	}
	observation.File = canonicalFile
	return nil
}

func normalizeDifferentialLogRotateFile(
	side string,
	observation *DifferentialFileObservation,
) (*DifferentialFileObservation, error) {
	if observation == nil || observation.Name != differentialLogRotateObservationName ||
		!observation.Exists || observation.Truncated || observation.Size != int64(len(observation.Content)) {
		return nil, fmt.Errorf("%s log-rotate directory observation is incomplete", side)
	}
	state, err := decodeDifferentialLogRotateState(observation.Content)
	if err != nil {
		return nil, fmt.Errorf("%s log-rotate directory observation: %w", side, err)
	}
	if !differentialLogRotateArchivePattern.MatchString(state.ArchiveName) ||
		state.ArchiveMember != strings.TrimSuffix(state.ArchiveName, ".tar.gz") {
		return nil, fmt.Errorf("%s log-rotate archive identity is invalid", side)
	}
	if !strings.Contains(state.ArchiveContent, differentialLogRotatePreMarker) ||
		strings.Contains(state.CurrentContent, differentialLogRotatePreMarker) ||
		!strings.Contains(state.CurrentContent, differentialLogRotatePostMarker) ||
		state.SentinelContent != "keep-me\n" {
		return nil, fmt.Errorf("%s log-rotate rotate/compress/prune/reopen semantics are invalid", side)
	}
	canonical, err := json.Marshal(map[string]string{
		"archive": "<timestamp>__access.log.tar.gz", "member": "<timestamp>__access.log",
		"archive_contains": differentialLogRotatePreMarker,
		"current_contains": differentialLogRotatePostMarker,
		"sentinel":         "keep-me\n",
	})
	if err != nil {
		return nil, err
	}
	return &DifferentialFileObservation{
		Name: differentialLogRotateObservationName, Exists: true,
		Size: int64(len(canonical)), Content: string(canonical),
	}, nil
}

func normalizeDifferentialLogRotateContentType(headers map[string][]string, want string) error {
	var values []string
	for name, current := range headers {
		if strings.EqualFold(name, "Content-Type") {
			values = append(values, current...)
			delete(headers, name)
		}
	}
	if len(values) != 1 {
		return fmt.Errorf("Content-Type values = %#v, want one", values)
	}
	mediaType, _, err := mime.ParseMediaType(values[0])
	if err != nil {
		return err
	}
	if !strings.EqualFold(mediaType, want) {
		return fmt.Errorf("Content-Type = %q, want %s", values[0], want)
	}
	headers["Content-Type"] = []string{want}
	return nil
}
