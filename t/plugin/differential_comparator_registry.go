package pluginintegration

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	brotli "github.com/andybalholm/brotli"
)

type differentialComparator func(
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
	NormalizationPolicy,
) (bool, string, error)

type differentialComparatorRegistration struct {
	allowedPlugins map[string]struct{}
	compare        differentialComparator
}

var differentialComparatorRegistry = map[string]differentialComparatorRegistration{
	"ai-aws-comprehend-sigv4": {
		allowedPlugins: map[string]struct{}{"ai-aws-content-moderation": {}},
		compare:        compareDifferentialAIAWSComprehendSigV4,
	},
	"ai-rate-limiting-window": {
		allowedPlugins: map[string]struct{}{"ai-rate-limiting": {}},
		compare:        compareDifferentialAIRateLimitingWindow,
	},
	"compressed-response-semantics": {
		allowedPlugins: map[string]struct{}{"brotli": {}, "gzip": {}},
		compare:        compareDifferentialCompressedResponseSemantics,
	},
	"chaitin-waf-elapsed-time": {
		allowedPlugins: map[string]struct{}{"chaitin-waf": {}},
		compare:        compareDifferentialChaitinWAFElapsedTime,
	},
	differentialDataMaskRequestLinePolicy: {
		allowedPlugins: map[string]struct{}{"data-mask": {}},
		compare:        compareDifferentialDataMaskRequestLine,
	},
	differentialErrorLogLoggerClickHouseDeliveryPolicy: {
		allowedPlugins: map[string]struct{}{"error-log-logger": {}},
		compare:        compareDifferentialErrorLogLoggerClickHouseDelivery,
	},
	differentialGoogleCloudLoggingFixtureDeliveryPolicy: {
		allowedPlugins: map[string]struct{}{"google-cloud-logging": {}},
		compare:        compareDifferentialGoogleCloudLoggingFixtureDelivery,
	},
	"limit-req-burst-response": {
		allowedPlugins: map[string]struct{}{"limit-req": {}},
		compare:        compareDifferentialLimitReqBurstResponse,
	},
	differentialLimitConnGlobalSharedCapacityPolicy: {
		allowedPlugins: map[string]struct{}{"limit-conn": {}},
		compare:        compareDifferentialLimitConnGlobalSharedCapacity,
	},
	differentialComparisonNodeStatusJSONCounters: {
		allowedPlugins: map[string]struct{}{"node-status": {}},
		compare:        compareDifferentialNodeStatusJSONCounters,
	},
	differentialComparisonPrometheusRouteStatusSeries: {
		allowedPlugins: map[string]struct{}{"prometheus": {}},
		compare:        compareDifferentialPrometheusRouteStatusSeries,
	},
	"fixture-owned-function-endpoint": {
		allowedPlugins: map[string]struct{}{"aws-lambda": {}},
		compare:        compareDifferentialFixtureOwnedFunctionEndpoint,
	},
	differentialAzureFunctionsFixtureInvocationPolicy: {
		allowedPlugins: map[string]struct{}{"azure-functions": {}},
		compare:        compareDifferentialAzureFunctionsFixtureInvocation,
	},
	differentialOPAFixtureDecisionPolicy: {
		allowedPlugins: map[string]struct{}{"opa": {}},
		compare:        compareDifferentialOPAFixtureDecision,
	},
	differentialOpenTelemetryOTLPHTTPServerSpanPolicy: {
		allowedPlugins: map[string]struct{}{"opentelemetry": {}},
		compare:        compareDifferentialOpenTelemetryOTLPHTTPServerSpanCore,
	},
	differentialDingTalkAuthFixtureOAuthPolicy: {
		allowedPlugins: map[string]struct{}{"dingtalk-auth": {}},
		compare:        compareDifferentialDingTalkAuthFixtureOAuth,
	},
	differentialFeishuAuthFixtureOAuthPolicy: {
		allowedPlugins: map[string]struct{}{"feishu-auth": {}},
		compare:        compareDifferentialFeishuAuthFixtureOAuth,
	},
	"clickhouse-logger-fixture-delivery": {
		allowedPlugins: map[string]struct{}{"clickhouse-logger": {}},
		compare:        compareDifferentialClickHouseLoggerFixtureDelivery,
	},
	"elasticsearch-logger-fixture-delivery": {
		allowedPlugins: map[string]struct{}{"elasticsearch-logger": {}},
		compare:        compareDifferentialElasticsearchLoggerFixtureDelivery,
	},
	"http-logger-fixture-delivery": {
		allowedPlugins: map[string]struct{}{"http-logger": {}},
		compare:        compareDifferentialHTTPLoggerFixtureDelivery,
	},
	"lago-fixture-delivery": {
		allowedPlugins: map[string]struct{}{"lago": {}},
		compare:        compareDifferentialLagoFixtureDelivery,
	},
	"loggly-http-fixture-delivery": {
		allowedPlugins: map[string]struct{}{"loggly": {}},
		compare:        compareDifferentialLogglyHTTPFixtureDelivery,
	},
	"loki-logger-fixture-delivery": {
		allowedPlugins: map[string]struct{}{"loki-logger": {}},
		compare:        compareDifferentialLokiLoggerFixtureDelivery,
	},
	differentialDatadogSixDatagramsPolicy: {
		allowedPlugins: map[string]struct{}{"datadog": {}},
		compare:        compareDifferentialDatadogSixDatagrams,
	},
	differentialTCPLoggerFixtureDeliveryPolicy: {
		allowedPlugins: map[string]struct{}{"tcp-logger": {}},
		compare:        compareDifferentialTCPLoggerFixtureDelivery,
	},
	differentialUDPLoggerFixtureDeliveryPolicy: {
		allowedPlugins: map[string]struct{}{"udp-logger": {}},
		compare:        compareDifferentialUDPLoggerFixtureDelivery,
	},
	differentialSyslogTCPDeliveryPolicy: {
		allowedPlugins: map[string]struct{}{"syslog": {}},
		compare:        compareDifferentialSyslogTCPDelivery,
	},
	differentialSLSLoggerTLSDeliveryPolicy: {
		allowedPlugins: map[string]struct{}{"sls-logger": {}},
		compare:        compareDifferentialSLSLoggerTLSDelivery,
	},
	differentialSkyWalkingLoggerFixtureDeliveryPolicy: {
		allowedPlugins: map[string]struct{}{"skywalking-logger": {}},
		compare:        compareDifferentialSkyWalkingLoggerFixtureDelivery,
	},
	"splunk-hec-logging-fixture-delivery": {
		allowedPlugins: map[string]struct{}{"splunk-hec-logging": {}},
		compare:        compareDifferentialSplunkHECLoggingFixtureDelivery,
	},
	"tencent-cloud-cls-fixture-delivery": {
		allowedPlugins: map[string]struct{}{"tencent-cloud-cls": {}},
		compare:        compareDifferentialTencentCloudCLSFixtureDelivery,
	},
	"zipkin-v2-server-span-core": {
		allowedPlugins: map[string]struct{}{"zipkin": {}},
		compare:        compareDifferentialZipkinV2ServerSpanCore,
	},
	differentialComparisonCASAuthCallbackNosniff: {
		allowedPlugins: map[string]struct{}{"cas-auth": {}},
		compare:        compareDifferentialCASAuthCallbackNosniff,
	},
	differentialComparisonPlatformOwnedErrorRepresentation: {
		allowedPlugins: map[string]struct{}{
			"authz-casdoor": {}, "client-control": {}, "openid-connect": {},
		},
		compare: compareDifferentialPlatformOwnedErrorRepresentation,
	},
	differentialComparisonErrorPageCharsetParameter: {
		allowedPlugins: map[string]struct{}{"error-page": {}},
		compare:        compareDifferentialErrorPageCharsetParameter,
	},
	differentialComparisonForwardAuthEmptyErrorContentType: {
		allowedPlugins: map[string]struct{}{"forward-auth": {}},
		compare:        compareDifferentialForwardAuthEmptyErrorContentType,
	},
	differentialComparisonGraphQLHeadErrorContentType: {
		allowedPlugins: map[string]struct{}{
			"graphql-limit-count": {}, "graphql-proxy-cache": {},
		},
		compare: compareDifferentialGraphQLHeadErrorContentType,
	},
	differentialComparisonLimitCountFixedWindowResponse: {
		allowedPlugins: map[string]struct{}{"limit-count": {}},
		compare:        compareDifferentialLimitCountFixedWindowResponse,
	},
	differentialComparisonPlatformOwnedRedirectRepresentation: {
		allowedPlugins: map[string]struct{}{"redirect": {}},
		compare:        compareDifferentialPlatformOwnedRedirectRepresentation,
	},
	differentialComparisonFixtureOwnedUpstreamEndpoint: {
		allowedPlugins: map[string]struct{}{"openwhisk": {}},
		compare:        compareDifferentialFixtureOwnedUpstreamEndpoint,
	},
	differentialCSRFIssuedCookieComparisonPolicy: {
		allowedPlugins: map[string]struct{}{"csrf": {}},
		compare:        compareDifferentialCSRFIssuedCookie,
	},
}

func compareDifferentialCompressedResponseSemantics(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	const wantBody = "0123456789\n012345678"
	wantEncoding := map[string]string{"gzip": "gzip", "brotli": "br"}[spec.Plugin]
	wantName := spec.Plugin + "-default-compression"
	wantRouteID := "differential-" + spec.Plugin + "-default-compression"
	if wantEncoding == "" || spec.Name != wantName || spec.RouteID != wantRouteID ||
		spec.Request.Method != http.MethodPost || spec.Request.Path != "/echo" ||
		spec.Request.Host != "gateway.example.test" || spec.Request.Body != wantBody ||
		spec.Request.Headers["Accept-Encoding"] != wantEncoding ||
		spec.Request.Headers["Content-Type"] != "text/html" || len(spec.Steps) != 0 ||
		spec.Fixture.ExpectedCalls != 1 || spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "text/html" ||
		spec.Fixture.Response.Body != wantBody || spec.SecurityDecision != "not_applicable" {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the pinned %s compression case",
			spec.ComparisonPolicy,
			spec.Plugin,
		)
	}

	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		observation := side.observation
		if observation.Status != http.StatusOK || observation.SecurityDecision != "not_applicable" ||
			!observation.Upstream.Received || len(observation.UpstreamCalls) != 0 {
			return false, "", fmt.Errorf(
				"comparison policy %q requires %s 200 with the single-request upstream observation",
				spec.ComparisonPolicy,
				side.name,
			)
		}
		encoding, err := singleDifferentialHeader(observation.Headers, "Content-Encoding")
		if err != nil || encoding != wantEncoding {
			return false, "", fmt.Errorf(
				"comparison policy %q %s Content-Encoding = %q: %v",
				spec.ComparisonPolicy,
				side.name,
				encoding,
				err,
			)
		}
		contentType, err := singleDifferentialHeader(observation.Headers, "Content-Type")
		wantContentType := "text/html"
		if side.name == "oracle" {
			wantContentType = "text/html; charset=utf-8"
		}
		if err != nil || contentType != wantContentType {
			return false, "", fmt.Errorf(
				"comparison policy %q %s Content-Type = %q, want %q: %v",
				spec.ComparisonPolicy,
				side.name,
				contentType,
				wantContentType,
				err,
			)
		}
		decoded, err := decodeDifferentialCompressedBody(wantEncoding, observation.Body)
		if err != nil {
			return false, "", fmt.Errorf(
				"comparison policy %q decode %s %s body: %w",
				spec.ComparisonPolicy,
				side.name,
				wantEncoding,
				err,
			)
		}
		if decoded != wantBody {
			return false, "", fmt.Errorf(
				"comparison policy %q %s decoded body = %q, want fixture body",
				spec.ComparisonPolicy,
				side.name,
				decoded,
			)
		}
		observation.Body = decoded
		deleteDifferentialHeader(observation.Headers, "Content-Type")
		observation.Headers["Content-Type"] = []string{"text/html"}
		deleteDifferentialHeader(observation.Headers, "Content-Length")
	}
	return compareNormalizedObservations(left, right, policy)
}

func decodeDifferentialCompressedBody(encoding, body string) (string, error) {
	source := bytes.NewReader([]byte(body))
	var reader io.Reader
	var closeReader io.Closer
	switch encoding {
	case "gzip":
		gzipReader, err := gzip.NewReader(source)
		if err != nil {
			return "", err
		}
		reader = gzipReader
		closeReader = gzipReader
	case "br":
		reader = brotli.NewReader(source)
	default:
		return "", fmt.Errorf("unsupported content encoding %q", encoding)
	}
	decoded, err := io.ReadAll(reader)
	if closeReader != nil {
		if closeErr := closeReader.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func compareDifferentialChaitinWAFElapsedTime(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	const wantBody = "{\"code\": 403, \"success\":false, \"message\": \"blocked by Chaitin SafeLine Web Application Firewall\", \"event_id\": \"b3c6ce574dc24f09a01f634a39dca83b\"}\n"
	if spec.Name != "chaitin-waf-block-mode-reject" ||
		spec.RouteID != "differential-chaitin-waf-reject" ||
		spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
		spec.Request.Host != "gateway.example.test" || len(spec.Steps) != 0 ||
		spec.Fixture.Name != "waf" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.WireProtocol != differentialFixtureWireT1KV2 ||
		spec.Fixture.Response.Status != http.StatusForbidden || spec.SecurityDecision != "deny" {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the pinned Chaitin WAF block case",
			spec.ComparisonPolicy,
		)
	}
	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		observation := side.observation
		if observation.Status != http.StatusForbidden || observation.Body != wantBody ||
			observation.SecurityDecision != "deny" || !observation.Upstream.Received ||
			observation.UpstreamFixture != "waf" ||
			observation.Upstream.Host != spec.Request.Host {
			return false, "", fmt.Errorf(
				"comparison policy %q requires exact %s WAF rejection semantics and embedded Host %q",
				spec.ComparisonPolicy,
				side.name,
				spec.Request.Host,
			)
		}
		for name, want := range map[string]string{
			"X-APISIX-CHAITIN-WAF":        "yes",
			"X-APISIX-CHAITIN-WAF-STATUS": "403",
			"X-APISIX-CHAITIN-WAF-ACTION": "reject",
		} {
			got, err := singleDifferentialHeader(observation.Headers, name)
			if err != nil || got != want {
				return false, "", fmt.Errorf(
					"comparison policy %q %s %s = %q: %v",
					spec.ComparisonPolicy,
					side.name,
					name,
					got,
					err,
				)
			}
		}
		elapsed, err := singleDifferentialHeader(observation.Headers, "X-APISIX-CHAITIN-WAF-TIME")
		if err != nil {
			return false, "", fmt.Errorf(
				"comparison policy %q %s elapsed time: %w",
				spec.ComparisonPolicy,
				side.name,
				err,
			)
		}
		milliseconds, err := strconv.ParseUint(elapsed, 10, 64)
		if err != nil {
			return false, "", fmt.Errorf(
				"comparison policy %q %s elapsed time %q is not milliseconds: %w",
				spec.ComparisonPolicy,
				side.name,
				elapsed,
				err,
			)
		}
		_ = milliseconds
		deleteDifferentialHeader(observation.Headers, "X-APISIX-CHAITIN-WAF-TIME")
	}
	return compareNormalizedObservations(left, right, policy)
}

func compareDifferentialAIRateLimitingWindow(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	const (
		window       = 60
		limit        = 30
		providerPath = "/v1/chat/completions"
		resetHeader  = "X-AI-RateLimit-Reset-ai-proxy-openai"
	)
	if spec.Name != "ai-rate-limiting-custom-rejection" ||
		spec.RouteID != "differential-ai-rate-custom-reject" || len(spec.Steps) != 4 ||
		spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 3 ||
		spec.Fixture.Response.Status != http.StatusOK {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the pinned AI rate-limiting case",
			spec.ComparisonPolicy,
		)
	}
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 2 {
		return false, "", fmt.Errorf("comparison policy %q requires two routes", spec.ComparisonPolicy)
	}
	route, ok := routes[1].(map[string]any)
	if !ok || route["id"] != spec.RouteID || route["uri"] != "/ai" {
		return false, "", fmt.Errorf("comparison policy %q cannot identify its route", spec.ComparisonPolicy)
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		return false, "", fmt.Errorf("comparison policy %q requires route plugins", spec.ComparisonPolicy)
	}
	rateConfig, ok := plugins["ai-rate-limiting"].(map[string]any)
	if !ok || rateConfig["limit"] != limit || rateConfig["time_window"] != window ||
		rateConfig["rejected_code"] != http.StatusForbidden ||
		rateConfig["rejected_msg"] != "rate limit exceeded" {
		return false, "", fmt.Errorf(
			"comparison policy %q requires limit=30 time_window=60 and the pinned rejection",
			spec.ComparisonPolicy,
		)
	}
	for index, step := range spec.Steps {
		wantDecision := "allow"
		if index == 3 {
			wantDecision = "deny"
		}
		if step.Request.Method != http.MethodPost || step.Request.Path != "/ai" ||
			step.Request.Host != "gateway.example.test" || step.SecurityDecision != wantDecision {
			return false, "", fmt.Errorf(
				"comparison policy %q step %d does not match the pinned request",
				spec.ComparisonPolicy,
				index,
			)
		}
	}
	if len(left.Steps) != 4 || len(right.Steps) != 4 ||
		len(left.UpstreamCalls) != 3 || len(right.UpstreamCalls) != 3 {
		return false, "", fmt.Errorf(
			"comparison policy %q requires four steps and three provider calls per side",
			spec.ComparisonPolicy,
		)
	}

	if _, err := validateDifferentialAIRateLimitingSteps(spec, "candidate", left.Steps, limit, window); err != nil {
		return false, "", err
	}
	if _, err := validateDifferentialAIRateLimitingSteps(spec, "oracle", right.Steps, limit, window); err != nil {
		return false, "", err
	}
	for index := range left.Steps {
		deleteDifferentialHeader(left.Steps[index].Headers, resetHeader)
		deleteDifferentialHeader(right.Steps[index].Headers, resetHeader)
	}

	if err := validateDifferentialAIProviderPaths(spec, "candidate", &left, providerPath); err != nil {
		return false, "", err
	}
	if err := validateDifferentialAIProviderPaths(spec, "oracle", &right, providerPath+"?"); err != nil {
		return false, "", err
	}
	right.Upstream.Path = providerPath
	for index := range right.UpstreamCalls {
		right.UpstreamCalls[index].Path = providerPath
	}
	return compareNormalizedObservations(left, right, policy)
}

func validateDifferentialAIRateLimitingSteps(
	spec DifferentialCase,
	side string,
	steps []DifferentialStepObservation,
	limit int,
	window int,
) ([]int, error) {
	resets := make([]int, len(steps))
	for index, step := range steps {
		wantStatus := http.StatusOK
		wantDecision := "allow"
		if index == 3 {
			wantStatus = http.StatusForbidden
			wantDecision = "deny"
		}
		if step.Status != wantStatus || step.SecurityDecision != wantDecision ||
			step.Host != "gateway.example.test" {
			return nil, fmt.Errorf(
				"comparison policy %q %s step %d status/decision/host = %d/%q/%q",
				spec.ComparisonPolicy,
				side,
				index,
				step.Status,
				step.SecurityDecision,
				step.Host,
			)
		}
		for name, want := range map[string]string{
			"X-AI-RateLimit-Limit-ai-proxy-openai":     strconv.Itoa(limit),
			"X-AI-RateLimit-Remaining-ai-proxy-openai": strconv.Itoa(max(limit-index*10, 0)),
		} {
			got, err := singleDifferentialHeader(step.Headers, name)
			if err != nil || got != want {
				return nil, fmt.Errorf(
					"comparison policy %q %s step %d %s = %q, want %q: %v",
					spec.ComparisonPolicy,
					side,
					index,
					name,
					got,
					want,
					err,
				)
			}
		}
		resetValue, err := singleDifferentialHeader(step.Headers, "X-AI-RateLimit-Reset-ai-proxy-openai")
		if err != nil {
			return nil, fmt.Errorf(
				"comparison policy %q %s step %d reset header: %w",
				spec.ComparisonPolicy,
				side,
				index,
				err,
			)
		}
		reset, err := strconv.Atoi(resetValue)
		if err != nil || reset <= 0 || reset > window || resetValue != strconv.Itoa(reset) {
			return nil, fmt.Errorf(
				"comparison policy %q %s step %d reset = %q, want an integer in [1,%d]",
				spec.ComparisonPolicy,
				side,
				index,
				resetValue,
				window,
			)
		}
		if index == 0 && reset != window {
			return nil, fmt.Errorf(
				"comparison policy %q %s first reset = %d, want full %d-second window",
				spec.ComparisonPolicy,
				side,
				reset,
				window,
			)
		}
		if index > 0 && reset > resets[index-1] {
			return nil, fmt.Errorf(
				"comparison policy %q %s reset increases from %d to %d at step %d",
				spec.ComparisonPolicy,
				side,
				resets[index-1],
				reset,
				index,
			)
		}
		resets[index] = reset
	}
	return resets, nil
}

func validateDifferentialAIProviderPaths(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
	want string,
) error {
	if !observation.Upstream.Received || observation.UpstreamFixture != "primary" ||
		observation.UpstreamAddress == "" || observation.Upstream.Fixture != "primary" ||
		observation.Upstream.Host != observation.UpstreamAddress || observation.Upstream.Path != want {
		return fmt.Errorf(
			"comparison policy %q %s final provider endpoint = %q%s, want fixture address and %q",
			spec.ComparisonPolicy,
			side,
			observation.Upstream.Host,
			observation.Upstream.Path,
			want,
		)
	}
	observation.Upstream.Host = "fixture:primary"
	for index, call := range observation.UpstreamCalls {
		if !call.Received || call.Fixture != "primary" ||
			call.Host != observation.UpstreamAddress || call.Path != want {
			return fmt.Errorf(
				"comparison policy %q %s provider call %d endpoint = %q%s, want fixture address and %q",
				spec.ComparisonPolicy,
				side,
				index,
				call.Host,
				call.Path,
				want,
			)
		}
		observation.UpstreamCalls[index].Host = "fixture:primary"
	}
	return nil
}

func compareDifferentialLimitReqBurstResponse(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if spec.Name != "limit-req-low-rate-small-burst-rejects-followups" ||
		spec.RouteID != "differential-limit-req-low-rate" || len(spec.Steps) != 4 ||
		spec.Fixture.ExpectedCalls != 1 {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the pinned four-step limit-req case",
			spec.ComparisonPolicy,
		)
	}
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		return false, "", fmt.Errorf("comparison policy %q requires one route", spec.ComparisonPolicy)
	}
	route, ok := routes[0].(map[string]any)
	if !ok {
		return false, "", fmt.Errorf("comparison policy %q route is malformed", spec.ComparisonPolicy)
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		return false, "", fmt.Errorf("comparison policy %q plugins are malformed", spec.ComparisonPolicy)
	}
	config, ok := plugins["limit-req"].(map[string]any)
	if !ok || config["rate"] != 0.1 || config["burst"] != 0.1 ||
		config["rejected_code"] != 503 || config["key"] != "remote_addr" {
		return false, "", fmt.Errorf(
			"comparison policy %q requires rate=0.1 burst=0.1 rejected_code=503",
			spec.ComparisonPolicy,
		)
	}
	for index, step := range spec.Steps {
		wantDecision := "deny"
		if index == 0 {
			wantDecision = "allow"
		}
		if step.Request.Method != http.MethodGet || step.Request.Path != "/hello" ||
			step.Request.Host != "gateway.example.test" || step.SecurityDecision != wantDecision {
			return false, "", fmt.Errorf(
				"comparison policy %q step %d does not match the pinned request",
				spec.ComparisonPolicy,
				index,
			)
		}
	}
	if len(left.Steps) != 4 || len(right.Steps) != 4 || len(left.UpstreamCalls) != 1 ||
		len(right.UpstreamCalls) != 1 {
		return false, "", fmt.Errorf(
			"comparison policy %q requires four responses and one upstream call per side",
			spec.ComparisonPolicy,
		)
	}
	for index := 1; index < 4; index++ {
		candidateStep := &left.Steps[index]
		oracleStep := &right.Steps[index]
		if candidateStep.Status != http.StatusServiceUnavailable || candidateStep.Body != "" ||
			len(differentialHeaderValues(candidateStep.Headers, "Content-Type")) != 0 {
			return false, "", fmt.Errorf(
				"comparison policy %q requires an empty candidate 503 at step %d",
				spec.ComparisonPolicy,
				index,
			)
		}
		if oracleStep.Status != http.StatusServiceUnavailable ||
			oracleStep.Body != differentialLimitCountOracle503Body {
			return false, "", fmt.Errorf(
				"comparison policy %q requires the pinned oracle 503 page at step %d",
				spec.ComparisonPolicy,
				index,
			)
		}
		contentType, err := singleDifferentialHeader(oracleStep.Headers, "Content-Type")
		if err != nil || contentType != "text/html; charset=utf-8" {
			return false, "", fmt.Errorf(
				"comparison policy %q oracle step %d Content-Type = %q: %v",
				spec.ComparisonPolicy,
				index,
				contentType,
				err,
			)
		}
		for _, step := range []*DifferentialStepObservation{candidateStep, oracleStep} {
			step.Body = ""
			deleteDifferentialHeader(step.Headers, "Content-Type")
			deleteDifferentialHeader(step.Headers, "Content-Length")
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func compareDifferentialCASAuthCallbackNosniff(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	const (
		wantName    = "cas-auth-callback-without-initiation-cookie"
		wantRouteID = "differential-cas-auth-callback-no-cookie"
		wantPath    = "/cas_callback?ticket=ST-test"
		wantBody    = "{\"message\":\"invalid callback state\"}\n"
	)
	if spec.Name != wantName || spec.RouteID != wantRouteID ||
		spec.Request.Method != http.MethodGet || spec.Request.Path != wantPath ||
		spec.Request.Host != "127.0.0.3" || spec.Request.Body != "" || spec.Request.SNI != "" ||
		spec.Fixture.ExpectedCalls != 0 || spec.SecurityDecision != "deny" ||
		len(spec.Steps) != 0 {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the pinned GET callback case with zero fixture calls",
			spec.ComparisonPolicy,
		)
	}
	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		observation := side.observation
		if observation.Status != http.StatusUnauthorized || observation.Body != wantBody ||
			observation.SecurityDecision != "deny" {
			return false, "", fmt.Errorf(
				"comparison policy %q requires %s 401 deny with the exact callback-state body",
				spec.ComparisonPolicy,
				side.name,
			)
		}
		contentType, err := singleDifferentialHeader(observation.Headers, "Content-Type")
		if err != nil || contentType != "text/plain; charset=utf-8" {
			return false, "", fmt.Errorf(
				"comparison policy %q %s Content-Type = %q: %v",
				spec.ComparisonPolicy,
				side.name,
				contentType,
				err,
			)
		}
		if values := differentialHeaderValues(observation.Headers, "Set-Cookie"); len(values) != 0 {
			return false, "", fmt.Errorf(
				"comparison policy %q requires no %s Set-Cookie",
				spec.ComparisonPolicy,
				side.name,
			)
		}
		if err := validateDifferentialCASAuthNoUpstream(spec, side.name, *observation); err != nil {
			return false, "", err
		}
	}
	candidateNosniff, err := singleDifferentialHeader(left.Headers, "X-Content-Type-Options")
	if err != nil || candidateNosniff != "nosniff" {
		return false, "", fmt.Errorf(
			"comparison policy %q candidate X-Content-Type-Options = %q: %v",
			spec.ComparisonPolicy,
			candidateNosniff,
			err,
		)
	}
	if values := differentialHeaderValues(right.Headers, "X-Content-Type-Options"); len(values) != 0 {
		return false, "", fmt.Errorf(
			"comparison policy %q requires no oracle X-Content-Type-Options",
			spec.ComparisonPolicy,
		)
	}
	deleteDifferentialHeader(left.Headers, "X-Content-Type-Options")
	return compareNormalizedObservations(left, right, policy)
}

func validateDifferentialCASAuthNoUpstream(
	spec DifferentialCase,
	side string,
	observation DifferentialObservation,
) error {
	upstream := observation.Upstream
	if upstream.Received || upstream.Fixture != "" || upstream.Method != "" || upstream.Path != "" ||
		upstream.Host != "" || len(upstream.Headers) != 0 || upstream.Body != "" ||
		observation.UpstreamFixture != "" || observation.UpstreamAddress != "" ||
		len(observation.UpstreamCalls) != 0 || observation.RetryCount != 0 {
		return fmt.Errorf(
			"comparison policy %q requires no %s upstream activity",
			spec.ComparisonPolicy,
			side,
		)
	}
	return nil
}

const differentialLimitCountOracle503Body = "<html>\r\n" +
	"<head><title>503 Service Temporarily Unavailable</title></head>\r\n" +
	"<body>\r\n" +
	"<center><h1>503 Service Temporarily Unavailable</h1></center>\r\n" +
	"<hr><center>openresty</center>\r\n" +
	"<p><em>Powered by <a href=\"https://apisix.apache.org/\">APISIX</a>.</em></p></body>\r\n" +
	"</html>\r\n"

func compareDifferentialLimitCountFixedWindowResponse(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	const (
		wantCount        = 2
		wantTimeWindow   = 60
		wantRejectedCode = http.StatusServiceUnavailable
		wantStepCount    = 4
	)
	count, timeWindow, rejectedCode, err := differentialLimitCountFixedWindowConfig(spec)
	if err != nil {
		return false, "", err
	}
	if count != wantCount || timeWindow != wantTimeWindow || rejectedCode != wantRejectedCode {
		return false, "", fmt.Errorf(
			"comparison policy %q requires count=%d time_window=%d rejected_code=%d, got %d/%d/%d",
			spec.ComparisonPolicy,
			wantCount,
			wantTimeWindow,
			wantRejectedCode,
			count,
			timeWindow,
			rejectedCode,
		)
	}
	if len(spec.Steps) != wantStepCount || spec.Fixture.ExpectedCalls != wantCount {
		return false, "", fmt.Errorf(
			"comparison policy %q requires four steps and two fixture calls",
			spec.ComparisonPolicy,
		)
	}
	for index, step := range spec.Steps {
		wantDecision := "allow"
		if index >= count {
			wantDecision = "deny"
		}
		if step.SecurityDecision != wantDecision {
			return false, "", fmt.Errorf(
				"comparison policy %q step %d decision = %q, want %q",
				spec.ComparisonPolicy,
				index,
				step.SecurityDecision,
				wantDecision,
			)
		}
	}
	if len(left.Steps) != wantStepCount || len(right.Steps) != wantStepCount {
		return false, "", fmt.Errorf(
			"comparison policy %q requires four observations per side, got %d and %d",
			spec.ComparisonPolicy,
			len(left.Steps),
			len(right.Steps),
		)
	}

	_, err = validateDifferentialLimitCountSteps(
		spec,
		"candidate",
		left.Steps,
		count,
		timeWindow,
		rejectedCode,
	)
	if err != nil {
		return false, "", err
	}
	_, err = validateDifferentialLimitCountSteps(
		spec,
		"oracle",
		right.Steps,
		count,
		timeWindow,
		rejectedCode,
	)
	if err != nil {
		return false, "", err
	}
	for index := range wantStepCount {
		deleteDifferentialHeader(left.Steps[index].Headers, "X-RateLimit-Reset")
		deleteDifferentialHeader(right.Steps[index].Headers, "X-RateLimit-Reset")
	}
	for index := count; index < wantStepCount; index++ {
		if err := validateDifferentialLimitCountCandidate503(spec, index, left.Steps[index]); err != nil {
			return false, "", err
		}
		if err := validateDifferentialLimitCountOracle503(spec, index, right.Steps[index]); err != nil {
			return false, "", err
		}
		left.Steps[index].Body = ""
		right.Steps[index].Body = ""
		deleteDifferentialHeader(left.Steps[index].Headers, "Content-Type")
		deleteDifferentialHeader(left.Steps[index].Headers, "Content-Length")
		deleteDifferentialHeader(right.Steps[index].Headers, "Content-Type")
		deleteDifferentialHeader(right.Steps[index].Headers, "Content-Length")
	}
	return compareNormalizedObservations(left, right, policy)
}

func differentialLimitCountFixedWindowConfig(spec DifferentialCase) (int, int, int, error) {
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		return 0, 0, 0, fmt.Errorf(
			"comparison policy %q requires one route",
			spec.ComparisonPolicy,
		)
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["id"] != spec.RouteID {
		return 0, 0, 0, fmt.Errorf(
			"comparison policy %q cannot identify its route",
			spec.ComparisonPolicy,
		)
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		return 0, 0, 0, fmt.Errorf(
			"comparison policy %q requires route plugins",
			spec.ComparisonPolicy,
		)
	}
	config, ok := plugins["limit-count"].(map[string]any)
	if !ok {
		return 0, 0, 0, fmt.Errorf(
			"comparison policy %q requires limit-count config",
			spec.ComparisonPolicy,
		)
	}
	count, countOK := config["count"].(int)
	timeWindow, timeWindowOK := config["time_window"].(int)
	rejectedCode, rejectedCodeOK := config["rejected_code"].(int)
	if !countOK || !timeWindowOK || !rejectedCodeOK {
		return 0, 0, 0, fmt.Errorf(
			"comparison policy %q requires integer count, time_window, and rejected_code",
			spec.ComparisonPolicy,
		)
	}
	return count, timeWindow, rejectedCode, nil
}

func validateDifferentialLimitCountSteps(
	spec DifferentialCase,
	side string,
	steps []DifferentialStepObservation,
	count int,
	timeWindow int,
	rejectedCode int,
) ([]int, error) {
	resets := make([]int, len(steps))
	for index, step := range steps {
		wantStatus := http.StatusOK
		wantDecision := "allow"
		wantRemaining := count - index - 1
		if index >= count {
			wantStatus = rejectedCode
			wantDecision = "deny"
			wantRemaining = 0
		}
		if step.Status != wantStatus || step.SecurityDecision != wantDecision {
			return nil, fmt.Errorf(
				"comparison policy %q %s step %d status/decision = %d/%q, want %d/%q",
				spec.ComparisonPolicy,
				side,
				index,
				step.Status,
				step.SecurityDecision,
				wantStatus,
				wantDecision,
			)
		}
		limit, err := singleDifferentialHeader(step.Headers, "X-RateLimit-Limit")
		if err != nil || limit != strconv.Itoa(count) {
			return nil, fmt.Errorf(
				"comparison policy %q %s step %d limit header = %q: %v",
				spec.ComparisonPolicy,
				side,
				index,
				limit,
				err,
			)
		}
		remaining, err := singleDifferentialHeader(step.Headers, "X-RateLimit-Remaining")
		if err != nil || remaining != strconv.Itoa(wantRemaining) {
			return nil, fmt.Errorf(
				"comparison policy %q %s step %d remaining header = %q, want %d: %v",
				spec.ComparisonPolicy,
				side,
				index,
				remaining,
				wantRemaining,
				err,
			)
		}
		resetValue, err := singleDifferentialHeader(step.Headers, "X-RateLimit-Reset")
		if err != nil {
			return nil, fmt.Errorf(
				"comparison policy %q %s step %d reset header: %w",
				spec.ComparisonPolicy,
				side,
				index,
				err,
			)
		}
		reset, err := strconv.Atoi(resetValue)
		if err != nil || reset <= 0 || reset > timeWindow || resetValue != strconv.Itoa(reset) {
			return nil, fmt.Errorf(
				"comparison policy %q %s step %d reset = %q, want an integer in [1,%d]",
				spec.ComparisonPolicy,
				side,
				index,
				resetValue,
				timeWindow,
			)
		}
		if index > 0 && reset > resets[index-1] {
			return nil, fmt.Errorf(
				"comparison policy %q %s reset increases from %d to %d at step %d",
				spec.ComparisonPolicy,
				side,
				resets[index-1],
				reset,
				index,
			)
		}
		resets[index] = reset
	}
	return resets, nil
}

func validateDifferentialLimitCountCandidate503(
	spec DifferentialCase,
	index int,
	step DifferentialStepObservation,
) error {
	if step.Body != "" {
		return fmt.Errorf(
			"comparison policy %q candidate step %d requires an empty 503 body",
			spec.ComparisonPolicy,
			index,
		)
	}
	if values := differentialHeaderValues(step.Headers, "Content-Type"); len(values) != 0 {
		return fmt.Errorf(
			"comparison policy %q candidate step %d requires no Content-Type",
			spec.ComparisonPolicy,
			index,
		)
	}
	contentLength, err := singleDifferentialHeader(step.Headers, "Content-Length")
	if err != nil || contentLength != "0" {
		return fmt.Errorf(
			"comparison policy %q candidate step %d Content-Length = %q, want 0: %v",
			spec.ComparisonPolicy,
			index,
			contentLength,
			err,
		)
	}
	return nil
}

func validateDifferentialLimitCountOracle503(
	spec DifferentialCase,
	index int,
	step DifferentialStepObservation,
) error {
	if step.Body != differentialLimitCountOracle503Body {
		return fmt.Errorf(
			"comparison policy %q oracle step %d is not the pinned APISIX/NGINX 503 body",
			spec.ComparisonPolicy,
			index,
		)
	}
	contentType, err := singleDifferentialHeader(step.Headers, "Content-Type")
	if err != nil || contentType != "text/html; charset=utf-8" {
		return fmt.Errorf(
			"comparison policy %q oracle step %d Content-Type = %q: %v",
			spec.ComparisonPolicy,
			index,
			contentType,
			err,
		)
	}
	contentLength, err := singleDifferentialHeader(step.Headers, "Content-Length")
	wantContentLength := strconv.Itoa(len(step.Body))
	if err != nil || contentLength != wantContentLength {
		return fmt.Errorf(
			"comparison policy %q oracle step %d Content-Length = %q, want %s: %v",
			spec.ComparisonPolicy,
			index,
			contentLength,
			wantContentLength,
			err,
		)
	}
	return nil
}

func compareDifferentialErrorPageCharsetParameter(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	leftValue, err := singleDifferentialHeader(left.Headers, "Content-Type")
	if err != nil {
		return false, "", fmt.Errorf("comparison policy %q candidate Content-Type: %w", spec.ComparisonPolicy, err)
	}
	leftMediaType, leftParams, err := mime.ParseMediaType(leftValue)
	if err != nil {
		return false, "", fmt.Errorf("comparison policy %q candidate Content-Type: %w", spec.ComparisonPolicy, err)
	}
	if len(leftParams) != 0 {
		return false, "", fmt.Errorf(
			"comparison policy %q requires a parameter-free candidate Content-Type",
			spec.ComparisonPolicy,
		)
	}

	rightValue, err := singleDifferentialHeader(right.Headers, "Content-Type")
	if err != nil {
		return false, "", fmt.Errorf("comparison policy %q oracle Content-Type: %w", spec.ComparisonPolicy, err)
	}
	rightMediaType, rightParams, err := mime.ParseMediaType(rightValue)
	if err != nil {
		return false, "", fmt.Errorf("comparison policy %q oracle Content-Type: %w", spec.ComparisonPolicy, err)
	}
	if leftMediaType != rightMediaType || len(rightParams) != 1 ||
		!strings.EqualFold(rightParams["charset"], "utf-8") {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the same media type and only oracle charset=utf-8",
			spec.ComparisonPolicy,
		)
	}

	deleteDifferentialHeader(left.Headers, "Content-Type")
	deleteDifferentialHeader(right.Headers, "Content-Type")
	return compareNormalizedObservations(left, right, policy)
}

func compareDifferentialForwardAuthEmptyErrorContentType(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if left.Status < http.StatusBadRequest || right.Status != left.Status || left.Body != "" || right.Body != "" {
		return false, "", fmt.Errorf(
			"comparison policy %q requires equal error statuses and empty bodies, got %d/%q and %d/%q",
			spec.ComparisonPolicy,
			left.Status,
			left.Body,
			right.Status,
			right.Body,
		)
	}
	if values := differentialHeaderValues(left.Headers, "Content-Type"); len(values) != 0 {
		return false, "", fmt.Errorf("comparison policy %q requires no candidate Content-Type", spec.ComparisonPolicy)
	}
	rightValue, err := singleDifferentialHeader(right.Headers, "Content-Type")
	if err != nil {
		return false, "", fmt.Errorf("comparison policy %q oracle Content-Type: %w", spec.ComparisonPolicy, err)
	}
	mediaType, params, err := mime.ParseMediaType(rightValue)
	if err != nil {
		return false, "", fmt.Errorf("comparison policy %q oracle Content-Type: %w", spec.ComparisonPolicy, err)
	}
	if mediaType != "text/plain" || len(params) != 1 || !strings.EqualFold(params["charset"], "utf-8") {
		return false, "", fmt.Errorf(
			"comparison policy %q requires only oracle text/plain; charset=utf-8",
			spec.ComparisonPolicy,
		)
	}
	deleteDifferentialHeader(right.Headers, "Content-Type")

	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		observation := side.observation
		if !observation.Upstream.Received || observation.UpstreamAddress == "" ||
			observation.UpstreamFixture == "" ||
			observation.Upstream.Fixture != observation.UpstreamFixture ||
			observation.Upstream.Host != observation.UpstreamAddress {
			return false, "", fmt.Errorf(
				"comparison policy %q requires %s upstream Host %q to equal fixture address %q",
				spec.ComparisonPolicy,
				side.name,
				observation.Upstream.Host,
				observation.UpstreamAddress,
			)
		}
		observation.Upstream.Host = "fixture:" + observation.UpstreamFixture
	}

	return compareNormalizedObservations(left, right, policy)
}

func compareDifferentialGraphQLHeadErrorContentType(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if spec.Request.Method != http.MethodHead {
		return false, "", fmt.Errorf(
			"comparison policy %q requires a HEAD request",
			spec.ComparisonPolicy,
		)
	}
	if left.Status < http.StatusBadRequest || right.Status != left.Status {
		return false, "", fmt.Errorf(
			"comparison policy %q requires equal error statuses, got %d and %d",
			spec.ComparisonPolicy,
			left.Status,
			right.Status,
		)
	}
	if left.Body != "" || right.Body != "" {
		return false, "", fmt.Errorf(
			"comparison policy %q requires empty bodies, got %q and %q",
			spec.ComparisonPolicy,
			left.Body,
			right.Body,
		)
	}
	if values := differentialHeaderValues(left.Headers, "Content-Type"); len(values) != 0 {
		return false, "", fmt.Errorf("comparison policy %q requires no candidate Content-Type", spec.ComparisonPolicy)
	}
	rightValue, err := singleDifferentialHeader(right.Headers, "Content-Type")
	if err != nil {
		return false, "", fmt.Errorf("comparison policy %q oracle Content-Type: %w", spec.ComparisonPolicy, err)
	}
	if rightValue != "text/html; charset=utf-8" {
		return false, "", fmt.Errorf(
			"comparison policy %q requires only oracle text/html; charset=utf-8",
			spec.ComparisonPolicy,
		)
	}
	deleteDifferentialHeader(right.Headers, "Content-Type")
	return compareNormalizedObservations(left, right, policy)
}

func differentialHeaderValues(headers map[string][]string, name string) []string {
	var values []string
	for current, currentValues := range headers {
		if strings.EqualFold(current, name) {
			values = append(values, currentValues...)
		}
	}
	return values
}

func singleDifferentialHeader(headers map[string][]string, name string) (string, error) {
	values := differentialHeaderValues(headers, name)
	if len(values) != 1 {
		return "", fmt.Errorf("has %d values, want exactly 1", len(values))
	}
	return values[0], nil
}

func deleteDifferentialHeader(headers map[string][]string, name string) {
	for current := range headers {
		if strings.EqualFold(current, name) {
			delete(headers, current)
		}
	}
}

func compareDifferentialFixtureOwnedUpstreamEndpoint(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		observation := side.observation
		if !observation.Upstream.Received || observation.UpstreamAddress == "" ||
			observation.UpstreamFixture == "" ||
			observation.Upstream.Fixture != observation.UpstreamFixture {
			return false, "", fmt.Errorf(
				"comparison policy %q requires one identified %s fixture request",
				spec.ComparisonPolicy,
				side.name,
			)
		}
		if observation.Upstream.Host != observation.UpstreamAddress {
			return false, "", fmt.Errorf(
				"comparison policy %q requires %s upstream Host %q to equal fixture address %q",
				spec.ComparisonPolicy,
				side.name,
				observation.Upstream.Host,
				observation.UpstreamAddress,
			)
		}
		observation.Upstream.Host = "fixture:" + observation.UpstreamFixture

		requestURI, err := url.ParseRequestURI(observation.Upstream.Path)
		if err != nil {
			return false, "", fmt.Errorf(
				"comparison policy %q cannot parse %s upstream path: %w",
				spec.ComparisonPolicy,
				side.name,
				err,
			)
		}
		query, err := url.ParseQuery(requestURI.RawQuery)
		if err != nil {
			return false, "", fmt.Errorf(
				"comparison policy %q cannot parse %s upstream query: %w",
				spec.ComparisonPolicy,
				side.name,
				err,
			)
		}
		if len(query) != 3 {
			return false, "", fmt.Errorf(
				"comparison policy %q requires exactly the OpenWhisk query keys for %s",
				spec.ComparisonPolicy,
				side.name,
			)
		}
		for _, name := range []string{"blocking", "result", "timeout"} {
			if values := query[name]; len(values) != 1 {
				return false, "", fmt.Errorf(
					"comparison policy %q requires one %s query value for %s",
					spec.ComparisonPolicy,
					name,
					side.name,
				)
			}
		}
		requestURI.RawQuery = query.Encode()
		observation.Upstream.Path = requestURI.RequestURI()
	}
	return compareNormalizedObservations(left, right, policy)
}

func compareDifferentialPlatformOwnedRedirectRepresentation(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if left.Status < http.StatusMultipleChoices || left.Status >= http.StatusBadRequest ||
		right.Status < http.StatusMultipleChoices || right.Status >= http.StatusBadRequest {
		return false, "", fmt.Errorf(
			"comparison policy %q requires redirect responses, got %d and %d",
			spec.ComparisonPolicy,
			left.Status,
			right.Status,
		)
	}
	right.Body = ""
	for _, observation := range []*DifferentialObservation{&left, &right} {
		for name := range observation.Headers {
			if strings.EqualFold(name, "Content-Type") ||
				strings.EqualFold(name, "Content-Length") {
				delete(observation.Headers, name)
			}
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func compareDifferentialCaseObservations(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	left = copyDifferentialObservation(left)
	right = copyDifferentialObservation(right)
	if spec.ComparisonPolicy == "" {
		return compareNormalizedObservations(left, right, policy)
	}
	registration, exists := differentialComparatorRegistry[spec.ComparisonPolicy]
	if !exists {
		return false, "", fmt.Errorf(
			"unknown differential comparison policy %q",
			spec.ComparisonPolicy,
		)
	}
	if _, allowed := registration.allowedPlugins[spec.Plugin]; !allowed {
		return false, "", fmt.Errorf(
			"differential comparison policy %q is not allowed for plugin %q",
			spec.ComparisonPolicy,
			spec.Plugin,
		)
	}
	return registration.compare(spec, left, right, policy)
}

func compareDifferentialPlatformOwnedErrorRepresentation(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if left.Status < http.StatusBadRequest || right.Status < http.StatusBadRequest {
		return false, "", fmt.Errorf(
			"comparison policy %q requires error responses, got %d and %d",
			spec.ComparisonPolicy,
			left.Status,
			right.Status,
		)
	}
	right.Body = ""
	for name := range right.Headers {
		if strings.EqualFold(name, "Content-Type") ||
			strings.EqualFold(name, "Content-Length") {
			delete(right.Headers, name)
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func compareDifferentialCSRFIssuedCookie(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	left, err := normalizeDifferentialCSRFCookie(spec, left)
	if err != nil {
		return false, "", fmt.Errorf("candidate CSRF cookie: %w", err)
	}
	right, err = normalizeDifferentialCSRFCookie(spec, right)
	if err != nil {
		return false, "", fmt.Errorf("oracle CSRF cookie: %w", err)
	}
	return compareNormalizedObservations(left, right, policy)
}
