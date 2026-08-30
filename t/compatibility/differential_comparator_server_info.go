package pluginintegration

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var (
	differentialServerInfoHostnamePattern = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	differentialServerInfoIDPattern       = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	differentialServerInfoVersionPattern  = regexp.MustCompile(`^[0-9A-Za-z._+-]+$`)
	differentialServerInfoEtcdPattern     = regexp.MustCompile(`^(unknown|[0-9]+(?:\.[0-9]+)*)$`)
)

func init() {
	differentialComparatorRegistry[differentialServerInfoControlAPIPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"server-info": {}},
		compare:        compareDifferentialServerInfoControlAPI,
	}
}

func compareDifferentialServerInfoControlAPI(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if spec.Name != "server-info-control-api-shape" || spec.Request.Path != "/v1/server_info" ||
		spec.Fixture.ExpectedCalls != 0 || spec.SecurityDecision != "not_applicable" {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the pinned server-info control API case",
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
		if err := normalizeDifferentialServerInfoSide(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialServerInfoSide(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if observation.Status != 200 || observation.Host != spec.Request.Host ||
		observation.SecurityDecision != spec.SecurityDecision || observation.SNI != "" ||
		len(observation.Steps) != 0 {
		return fmt.Errorf("%s server-info response identity is invalid", side)
	}
	if observation.Upstream.Received || observation.UpstreamFixture != "" ||
		observation.UpstreamAddress != "" || len(observation.UpstreamCalls) != 0 ||
		observation.RetryCount != 0 {
		return fmt.Errorf("%s server-info control API unexpectedly reached upstream", side)
	}
	contentTypes := differentialHeaderValues(observation.Headers, "Content-Type")
	if len(contentTypes) != 1 || !strings.HasPrefix(strings.ToLower(contentTypes[0]), "application/json") {
		return fmt.Errorf("%s server-info Content-Type = %q, want application/json", side, contentTypes)
	}
	if err := validateDifferentialServerInfoBody(observation.Body); err != nil {
		return fmt.Errorf("%s server-info body: %w", side, err)
	}

	deleteDifferentialHeader(observation.Headers, "Content-Type")
	deleteDifferentialHeader(observation.Headers, "Content-Length")
	observation.Body = "server-info:validated-five-field-control-response"
	return nil
}

func validateDifferentialServerInfoBody(body string) error {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("body contains trailing JSON")
	}
	allowed := map[string]struct{}{
		"boot_time": {}, "etcd_version": {}, "hostname": {}, "id": {}, "version": {},
	}
	if len(fields) != len(allowed) {
		return fmt.Errorf("field count = %d, want %d", len(fields), len(allowed))
	}
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("unexpected field %q", name)
		}
	}

	var bootTime int64
	if err := json.Unmarshal(fields["boot_time"], &bootTime); err != nil || bootTime <= 0 {
		return fmt.Errorf("boot_time must be a positive integer")
	}
	for name, pattern := range map[string]*regexp.Regexp{
		"etcd_version": differentialServerInfoEtcdPattern,
		"hostname":     differentialServerInfoHostnamePattern,
		"id":           differentialServerInfoIDPattern,
		"version":      differentialServerInfoVersionPattern,
	} {
		var value string
		if err := json.Unmarshal(fields[name], &value); err != nil || !pattern.MatchString(value) {
			return fmt.Errorf("%s has invalid format", name)
		}
	}
	return nil
}
