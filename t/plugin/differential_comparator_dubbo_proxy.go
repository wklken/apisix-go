package pluginintegration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
)

func init() {
	differentialComparatorRegistry[differentialDubboProxyHessian2Policy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"dubbo-proxy": {}},
		compare:        compareDifferentialDubboProxyHessian2,
	}
}

func compareDifferentialDubboProxyHessian2(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialDubboProxyCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned dubbo-proxy case",
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
		if err := normalizeDifferentialDubboProxyObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialDubboProxyObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if len(observation.Steps) != 0 || observation.Status != http.StatusOK ||
		observation.Body != differentialDubboProxyResponseBody ||
		observation.Host != spec.Request.Host || observation.SNI != "" ||
		observation.SecurityDecision != spec.SecurityDecision || observation.RetryCount != 0 ||
		len(observation.RouteObserver) != 0 || observation.File != nil {
		return fmt.Errorf("%s dubbo-proxy gateway response envelope is not exact", side)
	}
	if values := differentialHeaderValues(
		observation.Headers,
		"Got-extra-arg-k",
	); len(values) != 1 ||
		values[0] != differentialDubboProxyHeaderValue {
		return fmt.Errorf("%s dubbo-proxy Got-extra-arg-k = %#v", side, values)
	}
	if observation.UpstreamFixture != spec.Fixture.Name || observation.UpstreamAddress == "" ||
		len(observation.UpstreamCalls) != 0 {
		return fmt.Errorf("%s dubbo-proxy upstream selection is not exact", side)
	}
	upstream := &observation.Upstream
	wantHeaders := map[string][]string{
		differentialDubboProxyParamsTypeHeader: {differentialDubboProxyParamsTypeDesc},
		differentialDubboProxyHTTPHostHeader:   {differentialDubboProxyHTTPHost},
		differentialDubboProxyHTTPBodyHeader:   {differentialDubboProxyRequestBody},
		"Extra-Arg-K":                          {differentialDubboProxyHeaderValue},
	}
	if !upstream.Received || upstream.Fixture != spec.Fixture.Name ||
		upstream.Method != differentialDubboProxyWireMethod ||
		upstream.Path != differentialDubboProxyServiceName+"/"+differentialDubboProxyMethodName ||
		upstream.Host != differentialDubboProxyServiceVersion ||
		!reflect.DeepEqual(upstream.Headers, wantHeaders) {
		return fmt.Errorf("%s dubbo-proxy upstream envelope is not exact", side)
	}
	var invocation differentialDubboProxyInvocation
	if err := json.Unmarshal([]byte(upstream.Body), &invocation); err != nil {
		return fmt.Errorf("%s dubbo-proxy invocation JSON: %w", side, err)
	}
	if err := validateDifferentialDubboProxyInvocation(invocation); err != nil {
		return fmt.Errorf("%s dubbo-proxy invocation: %w", side, err)
	}
	if err := normalizeDifferentialDubboProxyHTTPContext(invocation.HTTPContext); err != nil {
		return fmt.Errorf("%s dubbo-proxy HTTP context: %w", side, err)
	}
	invocation.RequestID = 0
	canonical, err := json.Marshal(invocation)
	if err != nil {
		return fmt.Errorf("marshal canonical dubbo-proxy invocation: %w", err)
	}
	upstream.Body = string(canonical)
	return nil
}

func normalizeDifferentialDubboProxyHTTPContext(context map[string][]string) error {
	required := map[string]string{
		"content-length":    "12",
		"extra-arg-k":       differentialDubboProxyHeaderValue,
		"host":              differentialDubboProxyHTTPHost,
		"user-agent":        "Go-http-client/1.1",
		"x-forwarded-host":  differentialDubboProxyHTTPHost,
		"x-forwarded-proto": "http",
	}
	for name, values := range context {
		if name == "x-forwarded-port" || name == "connection" {
			continue
		}
		want, ok := required[name]
		if !ok {
			return fmt.Errorf("unexpected field %q", name)
		}
		if len(values) != 1 || values[0] != want {
			return fmt.Errorf("field %q = %#v, want %q", name, values, want)
		}
	}
	for name := range required {
		if _, ok := context[name]; !ok {
			return fmt.Errorf("missing field %q", name)
		}
	}
	forwardedPorts := context["x-forwarded-port"]
	if len(forwardedPorts) != 1 {
		return fmt.Errorf("x-forwarded-port = %#v, want one port", forwardedPorts)
	}
	forwardedPort, err := strconv.ParseUint(forwardedPorts[0], 10, 16)
	if err != nil || forwardedPort == 0 {
		return fmt.Errorf("x-forwarded-port = %#v, want port 1..65535", forwardedPorts)
	}
	if connection, ok := context["connection"]; ok {
		if len(connection) != 1 || connection[0] != "close" {
			return fmt.Errorf("connection = %#v, want close when present", connection)
		}
		delete(context, "connection")
	}
	context["x-forwarded-port"] = []string{"gateway-listener"}
	return nil
}
