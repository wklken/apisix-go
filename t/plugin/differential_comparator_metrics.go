package pluginintegration

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"

	"github.com/gofrs/uuid"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

func compareDifferentialNodeStatusJSONCounters(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialNodeStatusCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned node-status case",
			spec.ComparisonPolicy,
		)
	}
	left = copyDifferentialObservation(left)
	right = copyDifferentialObservation(right)
	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
		oracle      bool
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right, oracle: true},
	} {
		observation := side.observation
		if observation.Status != http.StatusOK || observation.Host != spec.Request.Host ||
			observation.SecurityDecision != "not_applicable" || len(observation.Steps) != 0 {
			return false, "", fmt.Errorf(
				"comparison policy %q requires %s exact 200 node-status response",
				spec.ComparisonPolicy,
				side.name,
			)
		}
		if !reflect.DeepEqual(observation.Upstream, DifferentialUpstreamObservation{}) ||
			observation.UpstreamFixture != "" || observation.UpstreamAddress != "" ||
			len(observation.UpstreamCalls) != 0 || observation.RetryCount != 0 {
			return false, "", fmt.Errorf(
				"comparison policy %q requires zero %s upstream activity",
				spec.ComparisonPolicy,
				side.name,
			)
		}
		contentType, err := singleDifferentialHeader(observation.Headers, "Content-Type")
		if err != nil {
			return false, "", fmt.Errorf(
				"comparison policy %q %s Content-Type: %w",
				spec.ComparisonPolicy,
				side.name,
				err,
			)
		}
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "text/plain" {
			return false, "", fmt.Errorf(
				"comparison policy %q requires %s text/plain Content-Type, got %q: %v",
				spec.ComparisonPolicy,
				side.name,
				contentType,
				err,
			)
		}
		if err := validateDifferentialNodeStatusJSON(
			spec.ComparisonPolicy,
			side.name,
			observation.Body,
			side.oracle,
		); err != nil {
			return false, "", err
		}
		observation.Body = ""
		deleteDifferentialHeader(observation.Headers, "Content-Type")
		observation.Headers["Content-Type"] = []string{"text/plain"}
		deleteDifferentialHeader(observation.Headers, "Content-Length")
	}
	return compareNormalizedObservations(left, right, policy)
}

func validateDifferentialNodeStatusJSON(
	comparisonPolicy string,
	side string,
	body string,
	allowNGINXCounters bool,
) error {
	outer, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{"id": {}, "status": {}},
		[]string{"id", "status"},
	)
	if err != nil {
		return fmt.Errorf(
			"comparison policy %q decode %s node-status JSON: %w",
			comparisonPolicy,
			side,
			err,
		)
	}
	var idValue string
	if err := json.Unmarshal(outer["id"], &idValue); err != nil {
		return fmt.Errorf(
			"comparison policy %q %s node-status field %q must be a string: %w",
			comparisonPolicy,
			side,
			"id",
			err,
		)
	}
	allowed := map[string]struct{}{
		"active": {}, "accepted": {}, "handled": {}, "total": {},
	}
	if allowNGINXCounters {
		allowed["reading"] = struct{}{}
		allowed["writing"] = struct{}{}
		allowed["waiting"] = struct{}{}
	}
	fields, err := decodeDifferentialJSONObject(
		string(outer["status"]),
		allowed,
		[]string{"active", "accepted", "handled", "total"},
	)
	if err != nil {
		return fmt.Errorf(
			"comparison policy %q decode %s node-status status JSON: %w",
			comparisonPolicy,
			side,
			err,
		)
	}
	values := make(map[string]string, len(fields))
	for name, raw := range fields {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf(
				"comparison policy %q %s node-status field %q must be a string: %w",
				comparisonPolicy,
				side,
				name,
				err,
			)
		}
		values[name] = value
	}
	id, err := uuid.FromString(idValue)
	if err != nil || id == uuid.Nil {
		return fmt.Errorf(
			"comparison policy %q %s node-status id is not a non-empty UUID",
			comparisonPolicy,
			side,
		)
	}
	for _, name := range []string{"active", "accepted", "handled", "total"} {
		if !isDifferentialCanonicalDecimal(values[name]) {
			return fmt.Errorf(
				"comparison policy %q %s node-status %s = %q, want a canonical decimal string",
				comparisonPolicy,
				side,
				name,
				values[name],
			)
		}
	}
	for _, name := range []string{"reading", "writing", "waiting"} {
		if value, exists := values[name]; exists && !isDifferentialCanonicalDecimal(value) {
			return fmt.Errorf(
				"comparison policy %q %s node-status %s = %q, want a canonical decimal string",
				comparisonPolicy,
				side,
				name,
				value,
			)
		}
	}
	return nil
}

func decodeDifferentialJSONObject(
	body string,
	allowed map[string]struct{},
	required []string,
) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("top-level value is not an object")
	}
	fields := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("object key is not a string")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate field %q", name)
		}
		if _, exists := allowed[name]; !exists {
			return nil, fmt.Errorf("unknown field %q", name)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		fields[name] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, fmt.Errorf("trailing JSON data: %w", err)
	}
	for _, name := range required {
		if _, exists := fields[name]; !exists {
			return nil, fmt.Errorf("missing field %q", name)
		}
	}
	return fields, nil
}

func isDifferentialCanonicalDecimal(value string) bool {
	if value == "0" {
		return true
	}
	if len(value) == 0 || value[0] < '1' || value[0] > '9' {
		return false
	}
	for index := 1; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func compareDifferentialPrometheusRouteStatusSeries(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialPrometheusCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned two-step Prometheus case",
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
		observation := side.observation
		if len(observation.Steps) != 2 {
			return false, "", fmt.Errorf(
				"comparison policy %q requires two %s observations",
				spec.ComparisonPolicy,
				side.name,
			)
		}
		first := observation.Steps[0]
		if first.Status != http.StatusOK || first.Body != spec.Fixture.Response.Body ||
			first.Host != spec.Steps[0].Request.Host || first.SNI != spec.Steps[0].Request.SNI ||
			first.SecurityDecision != spec.Steps[0].SecurityDecision {
			return false, "", fmt.Errorf(
				"comparison policy %q requires %s step 0 exact 200 profile-ok response",
				spec.ComparisonPolicy,
				side.name,
			)
		}
		if err := validateDifferentialPrometheusUpstream(
			spec,
			side.name,
			*observation,
		); err != nil {
			return false, "", err
		}
		scrape := &observation.Steps[1]
		if scrape.Status != http.StatusOK || scrape.Host != spec.Steps[1].Request.Host ||
			scrape.SNI != spec.Steps[1].Request.SNI ||
			scrape.SecurityDecision != spec.Steps[1].SecurityDecision {
			return false, "", fmt.Errorf(
				"comparison policy %q requires %s step 1 exact 200 scrape response",
				spec.ComparisonPolicy,
				side.name,
			)
		}
		contentType, err := singleDifferentialHeader(scrape.Headers, "Content-Type")
		if err != nil {
			return false, "", fmt.Errorf(
				"comparison policy %q %s scrape Content-Type: %w",
				spec.ComparisonPolicy,
				side.name,
				err,
			)
		}
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "text/plain" {
			return false, "", fmt.Errorf(
				"comparison policy %q requires %s text/plain scrape Content-Type, got %q: %v",
				spec.ComparisonPolicy,
				side.name,
				contentType,
				err,
			)
		}
		if err := validateDifferentialPrometheusTargetSeries(
			spec.ComparisonPolicy,
			side.name,
			scrape.Body,
			spec.RouteID,
		); err != nil {
			return false, "", err
		}
		scrape.Body = ""
		deleteDifferentialHeader(scrape.Headers, "Content-Type")
		scrape.Headers["Content-Type"] = []string{"text/plain"}
		deleteDifferentialHeader(scrape.Headers, "Content-Length")
	}
	return compareNormalizedObservations(left, right, policy)
}

func validateDifferentialPrometheusUpstream(
	spec DifferentialCase,
	side string,
	observation DifferentialObservation,
) error {
	if len(observation.UpstreamCalls) != 1 || observation.RetryCount != 0 ||
		!observation.Upstream.Received || observation.UpstreamFixture != spec.Fixture.Name ||
		observation.UpstreamAddress == "" ||
		!reflect.DeepEqual(observation.Upstream, observation.UpstreamCalls[0]) {
		return fmt.Errorf(
			"comparison policy %q requires exactly one identified %s upstream call",
			spec.ComparisonPolicy,
			side,
		)
	}
	call := observation.UpstreamCalls[0]
	if call.Fixture != spec.Fixture.Name || call.Method != spec.Steps[0].Request.Method ||
		call.Path != spec.Steps[0].Request.Path || call.Host != "differential.example.test" ||
		len(call.Headers) != 0 || call.Body != "" {
		return fmt.Errorf(
			"comparison policy %q %s upstream call does not match the pinned profile request",
			spec.ComparisonPolicy,
			side,
		)
	}
	return nil
}

func validateDifferentialPrometheusTargetSeries(
	comparisonPolicy string,
	side string,
	body string,
	routeID string,
) error {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		return fmt.Errorf(
			"comparison policy %q parse %s Prometheus scrape: %w",
			comparisonPolicy,
			side,
			err,
		)
	}
	family := families["apisix_http_status"]
	if family == nil || family.GetType() != dto.MetricType_COUNTER {
		return fmt.Errorf(
			"comparison policy %q requires %s apisix_http_status counter family",
			comparisonPolicy,
			side,
		)
	}
	var target []*dto.Metric
	for _, metric := range family.Metric {
		labels := make(map[string]string, len(metric.Label))
		for _, pair := range metric.Label {
			labels[pair.GetName()] = pair.GetValue()
		}
		if labels["code"] == "200" && labels["route"] == routeID {
			target = append(target, metric)
		}
	}
	if len(target) != 1 {
		return fmt.Errorf(
			"comparison policy %q %s target apisix_http_status series count = %d, want 1",
			comparisonPolicy,
			side,
			len(target),
		)
	}
	if target[0].Counter == nil || target[0].GetCounter().GetValue() != 1 {
		return fmt.Errorf(
			"comparison policy %q %s target apisix_http_status value = %v, want 1",
			comparisonPolicy,
			side,
			target[0].GetCounter().GetValue(),
		)
	}
	return nil
}
