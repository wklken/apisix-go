package pluginintegration

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
)

func compareDifferentialLimitConnGlobalSharedCapacity(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialLimitConnCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned limit-conn global-rule case",
			spec.ComparisonPolicy,
		)
	}
	left = copyDifferentialObservation(left)
	right = copyDifferentialObservation(right)
	if err := validateAndCanonicalizeDifferentialLimitConnObservation(spec, "candidate", &left, false); err != nil {
		return false, "", err
	}
	if err := validateAndCanonicalizeDifferentialLimitConnObservation(spec, "oracle", &right, true); err != nil {
		return false, "", err
	}
	return compareNormalizedObservations(left, right, policy)
}

func validateAndCanonicalizeDifferentialLimitConnObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
	oracle bool,
) error {
	const (
		batchSize    = 10
		allowedCount = 3
	)
	if len(observation.Steps) != batchSize+1 {
		return fmt.Errorf(
			"comparison policy %q requires %s ten batch responses and one release probe",
			spec.ComparisonPolicy,
			side,
		)
	}
	allowed := 0
	rejected := 0
	for index := range batchSize {
		step := &observation.Steps[index]
		if step.Host != "gateway.example.test" || step.SecurityDecision != "mixed" {
			return fmt.Errorf(
				"comparison policy %q %s batch step %d host/decision = %q/%q",
				spec.ComparisonPolicy,
				side,
				index,
				step.Host,
				step.SecurityDecision,
			)
		}
		switch step.Status {
		case http.StatusOK:
			allowed++
			if err := validateDifferentialLimitConnAllowedStep(spec, side, index, *step, oracle); err != nil {
				return err
			}
			if oracle {
				deleteDifferentialHeader(step.Headers, "Content-Type")
				step.Headers["Content-Type"] = []string{"text/plain"}
			}
		case http.StatusServiceUnavailable:
			rejected++
			var err error
			if oracle {
				err = validateDifferentialLimitCountOracle503(spec, index, *step)
			} else {
				err = validateDifferentialLimitCountCandidate503(spec, index, *step)
			}
			if err != nil {
				return err
			}
			step.Body = ""
			deleteDifferentialHeader(step.Headers, "Content-Type")
			deleteDifferentialHeader(step.Headers, "Content-Length")
		default:
			return fmt.Errorf(
				"comparison policy %q %s batch step %d status = %d, want 200 or 503",
				spec.ComparisonPolicy,
				side,
				index,
				step.Status,
			)
		}
	}
	if allowed != allowedCount || rejected != batchSize-allowedCount {
		return fmt.Errorf(
			"comparison policy %q requires %s batch to contain 3 allowed and 7 rejected responses, got %d/%d",
			spec.ComparisonPolicy,
			side,
			allowed,
			rejected,
		)
	}
	probe := &observation.Steps[batchSize]
	if probe.Status != http.StatusOK || probe.Host != "gateway.example.test" ||
		probe.SecurityDecision != "allow" {
		return fmt.Errorf(
			"comparison policy %q %s release probe = %d/%q/%q, want 200/allow",
			spec.ComparisonPolicy,
			side,
			probe.Status,
			probe.Host,
			probe.SecurityDecision,
		)
	}
	if err := validateDifferentialLimitConnAllowedStep(spec, side, batchSize, *probe, oracle); err != nil {
		return err
	}
	if oracle {
		deleteDifferentialHeader(probe.Headers, "Content-Type")
		probe.Headers["Content-Type"] = []string{"text/plain"}
	}

	sort.Slice(observation.Steps[:batchSize], func(i, j int) bool {
		return observation.Steps[i].Status < observation.Steps[j].Status
	})
	return validateAndCanonicalizeDifferentialLimitConnUpstream(spec, side, observation)
}

func validateDifferentialLimitConnAllowedStep(
	spec DifferentialCase,
	side string,
	index int,
	step DifferentialStepObservation,
	oracle bool,
) error {
	if step.Body != "hello world" {
		return fmt.Errorf(
			"comparison policy %q %s allowed step %d body = %q",
			spec.ComparisonPolicy,
			side,
			index,
			step.Body,
		)
	}
	contentType, err := singleDifferentialHeader(step.Headers, "Content-Type")
	wantContentType := "text/plain"
	if oracle {
		wantContentType = "text/plain; charset=utf-8"
	}
	if err != nil || contentType != wantContentType {
		return fmt.Errorf(
			"comparison policy %q %s allowed step %d Content-Type = %q: %v",
			spec.ComparisonPolicy,
			side,
			index,
			contentType,
			err,
		)
	}
	return nil
}

func validateAndCanonicalizeDifferentialLimitConnUpstream(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if observation.Status != 0 || observation.Body != "" || observation.Host != "" ||
		observation.SecurityDecision != "" || observation.RetryCount != 0 ||
		observation.UpstreamFixture != "limit-conn-origin" || observation.UpstreamAddress == "" {
		return fmt.Errorf(
			"comparison policy %q %s sequence envelope is malformed",
			spec.ComparisonPolicy,
			side,
		)
	}
	if len(observation.UpstreamCalls) != 4 {
		return fmt.Errorf(
			"comparison policy %q requires %s exactly four upstream calls, got %d",
			spec.ComparisonPolicy,
			side,
			len(observation.UpstreamCalls),
		)
	}
	if !reflect.DeepEqual(observation.Upstream, observation.UpstreamCalls[3]) {
		return fmt.Errorf(
			"comparison policy %q %s legacy upstream projection is not the release probe",
			spec.ComparisonPolicy,
			side,
		)
	}
	for index := range observation.UpstreamCalls {
		call := &observation.UpstreamCalls[index]
		if !call.Received || call.Fixture != "limit-conn-origin" || call.Method != http.MethodGet ||
			call.Host != "differential.example.test" || len(call.Headers) != 0 || call.Body != "" {
			return fmt.Errorf(
				"comparison policy %q %s upstream call %d is malformed: %#v",
				spec.ComparisonPolicy,
				side,
				index,
				*call,
			)
		}
		if index < 3 {
			if call.Path != "/limit-a" && call.Path != "/limit-b" {
				return fmt.Errorf(
					"comparison policy %q %s batch upstream path %d = %q",
					spec.ComparisonPolicy,
					side,
					index,
					call.Path,
				)
			}
			call.Path = "/limit-shared-batch"
		} else if call.Path != "/limit-b" {
			return fmt.Errorf(
				"comparison policy %q %s release probe upstream path = %q",
				spec.ComparisonPolicy,
				side,
				call.Path,
			)
		}
	}
	observation.Upstream = observation.UpstreamCalls[3]
	return nil
}
