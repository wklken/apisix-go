package pluginintegration

import (
	"fmt"
	"math"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

type differentialDatadogMetricContract struct {
	name           string
	metricType     string
	fixedValue     string
	candidateValue string
	oracleValue    string
}

var (
	differentialDatadogMetricPattern = regexp.MustCompile(
		`^apisix\.([a-z.]+):([^|]+)\|([a-z]+)\|#(.+)$`,
	)
	differentialDatadogDecimalPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)(?:\.[0-9]+)?$`)
	differentialDatadogMetrics        = []differentialDatadogMetricContract{
		{name: "request.counter", metricType: "c", fixedValue: "1"},
		{name: "request.latency", metricType: "h"},
		{name: "upstream.latency", metricType: "h"},
		{name: "apisix.latency", metricType: "h"},
		{name: "ingress.size", metricType: "ms", candidateValue: "89", oracleValue: "108"},
		{name: "egress.size", metricType: "ms", candidateValue: "164", oracleValue: "133"},
	}
)

func compareDifferentialDatadogSixDatagrams(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialDatadogCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned Datadog case",
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
		if err := normalizeDifferentialDatadogObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialDatadogObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if len(observation.Steps) != 1 {
		return fmt.Errorf("comparison policy %q requires one %s gateway step", spec.ComparisonPolicy, side)
	}
	step := &observation.Steps[0]
	wantStep := spec.Steps[0]
	if step.Status != spec.Fixture.Response.Status || step.Body != spec.Fixture.Response.Body ||
		step.Host != wantStep.Request.Host || step.SNI != wantStep.Request.SNI ||
		step.SecurityDecision != wantStep.SecurityDecision {
		return fmt.Errorf("comparison policy %q requires the exact %s gateway step", spec.ComparisonPolicy, side)
	}
	if err := normalizeDifferentialNetworkLoggerGatewayHeaders(
		side,
		step.Headers,
		len(spec.Fixture.Response.Body),
		"text/plain",
		"text/plain; charset=utf-8",
	); err != nil {
		return fmt.Errorf("comparison policy %q %s gateway headers: %w", spec.ComparisonPolicy, side, err)
	}
	if observation.Status != 0 || len(observation.Headers) != 0 || observation.Body != "" ||
		observation.Host != "" || observation.SNI != "" || observation.SecurityDecision != "" ||
		observation.RetryCount != 0 || len(observation.RouteObserver) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires the sequence-only %s observation envelope",
			spec.ComparisonPolicy,
			side,
		)
	}
	if observation.UpstreamFixture != spec.Fixture.Name || observation.UpstreamAddress == "" ||
		!observation.Upstream.Received || observation.Upstream.Fixture != spec.Fixture.Name ||
		len(observation.UpstreamCalls) != len(differentialDatadogMetrics) {
		return fmt.Errorf(
			"comparison policy %q requires exactly 6 identified %s UDP datagrams",
			spec.ComparisonPolicy, side,
		)
	}
	if !differentialLoggerUpstreamIsCapturedCall(observation.Upstream, observation.UpstreamCalls) {
		return fmt.Errorf("comparison policy %q %s summary datagram is not captured", spec.ComparisonPolicy, side)
	}

	canonicalCalls := make([]DifferentialUpstreamObservation, 0, len(differentialDatadogMetrics))
	for index, contract := range differentialDatadogMetrics {
		call := observation.UpstreamCalls[index]
		if !call.Received || call.Fixture != spec.Fixture.Name {
			return fmt.Errorf(
				"comparison policy %q %s datagram %d identity is invalid",
				spec.ComparisonPolicy, side, index+1,
			)
		}
		if call.Method != "UDP" || call.Path != "" || call.Host != "" || len(call.Headers) != 0 {
			return fmt.Errorf(
				"comparison policy %q %s datagram %d must be one raw UDP payload",
				spec.ComparisonPolicy, side, index+1,
			)
		}
		canonical, err := normalizeDifferentialDatadogMetric(call.Body, contract, side)
		if err != nil {
			return fmt.Errorf(
				"comparison policy %q %s datagram %d %s: %w",
				spec.ComparisonPolicy, side, index+1, contract.name, err,
			)
		}
		call.Body = canonical
		canonicalCalls = append(canonicalCalls, call)
	}
	observation.UpstreamAddress = "fixture:" + spec.Fixture.Name
	observation.UpstreamCalls = canonicalCalls
	observation.Upstream = canonicalCalls[len(canonicalCalls)-1]
	return nil
}

func normalizeDifferentialDatadogMetric(
	payload string,
	contract differentialDatadogMetricContract,
	side string,
) (string, error) {
	if strings.ContainsAny(payload, "\r\n") {
		return "", fmt.Errorf("datagram must contain exactly one metric")
	}
	matches := differentialDatadogMetricPattern.FindStringSubmatch(payload)
	if len(matches) != 5 {
		return "", fmt.Errorf("datagram is not one DogStatsD metric")
	}
	metricName, value, metricType, rawTags := matches[1], matches[2], matches[3], matches[4]
	if metricName != contract.name || metricType != contract.metricType {
		return "", fmt.Errorf(
			"metric = %s:%s|%s, want %s:<value>|%s",
			metricName, value, metricType, contract.name, contract.metricType,
		)
	}
	canonicalValue := value
	wantValue := contract.fixedValue
	if side == "candidate" && contract.candidateValue != "" {
		wantValue = contract.candidateValue
	}
	if side == "oracle" && contract.oracleValue != "" {
		wantValue = contract.oracleValue
	}
	if wantValue != "" {
		if value != wantValue {
			return "", fmt.Errorf("value = %q, want %q", value, wantValue)
		}
		if contract.candidateValue != "" || contract.oracleValue != "" {
			canonicalValue = "0"
		}
	} else {
		if !differentialDatadogDecimalPattern.MatchString(value) {
			return "", fmt.Errorf("value %q is not a nonnegative decimal", value)
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) || parsed < 0 {
			return "", fmt.Errorf("value %q is not a finite nonnegative decimal", value)
		}
		canonicalValue = "0"
	}
	canonicalTags, err := normalizeDifferentialDatadogTags(rawTags)
	if err != nil {
		return "", err
	}
	return "apisix." + contract.name + ":" + canonicalValue + "|" +
		contract.metricType + "|#" + canonicalTags, nil
}

func normalizeDifferentialDatadogTags(raw string) (string, error) {
	tags := strings.Split(raw, ",")
	if len(tags) != 6 || tags[0] != "source:apisix" || tags[1] != "route_name:datadog" ||
		tags[3] != "response_status:200" || tags[4] != "response_status_class:2xx" ||
		tags[5] != "scheme:http" {
		return "", fmt.Errorf("tags do not match the pinned APISIX Datadog order and values")
	}
	key, balancerIP, ok := strings.Cut(tags[2], ":")
	if !ok || key != "balancer_ip" || net.ParseIP(balancerIP) == nil {
		return "", fmt.Errorf("balancer_ip tag %q is not an IP address", tags[2])
	}
	tags[2] = "balancer_ip:fixture"
	return strings.Join(tags, ","), nil
}
