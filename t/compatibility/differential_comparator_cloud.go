package pluginintegration

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
)

const (
	differentialAIAWSBlockingBody         = "request body exceeds toxicity threshold"
	differentialAWSCandidateSignedHeaders = "content-type;host;x-amz-date;x-amz-target"
	differentialAWSOracleSignedHeaders    = "accept;content-length;content-type;host;x-amz-date;x-amz-target"
)

var differentialAWSAuthorizationPattern = regexp.MustCompile(
	`^AWS4-HMAC-SHA256 Credential=access/([0-9]{8})/us-east-1/comprehend/aws4_request, ` +
		`SignedHeaders=([a-z0-9;-]+), Signature=([0-9a-f]{64})$`,
)

func compareDifferentialAIAWSComprehendSigV4(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if err := validateDifferentialAIAWSPinnedCase(spec); err != nil {
		return false, "", err
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
		if err := normalizeDifferentialAIAWSObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func validateDifferentialAIAWSPinnedCase(spec DifferentialCase) error {
	const fixtureBody = `{"ResultList":[{"Toxicity":0.72150000333786,"Labels":[{"Name":"PROFANITY","Score":0.25589999556541}]}]}`
	if spec.Name != "ai-aws-content-moderation-toxic-raw-body" ||
		spec.Plugin != "ai-aws-content-moderation" ||
		spec.RouteID != "differential-ai-aws-toxic-raw-body" ||
		spec.ComparisonPolicy != "ai-aws-comprehend-sigv4" ||
		spec.Request.Method != http.MethodPost || spec.Request.Path != "/echo" ||
		spec.Request.Host != "gateway.example.test" || spec.Request.SNI != "" ||
		len(spec.Request.Headers) != 0 || spec.Request.Body != "toxic" || len(spec.Steps) != 0 ||
		spec.Fixture.Name != "primary" || spec.Fixture.WireProtocol != "" ||
		spec.Fixture.ExpectedCalls != 1 || spec.Fixture.Response.Status != http.StatusOK ||
		len(spec.Fixture.Response.Headers) != 1 ||
		spec.Fixture.Response.Headers["Content-Type"] != "application/json" ||
		spec.Fixture.Response.Body != fixtureBody ||
		spec.SecurityDecision != "deny" {
		return fmt.Errorf(
			"comparison policy %q requires the pinned APISIX 3.17 raw-body toxicity case",
			spec.ComparisonPolicy,
		)
	}
	return nil
}

func normalizeDifferentialAIAWSObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if observation.Status != http.StatusBadRequest {
		return fmt.Errorf(
			"comparison policy %q requires %s 400 response, got %d",
			spec.ComparisonPolicy,
			side,
			observation.Status,
		)
	}
	if observation.Body != differentialAIAWSBlockingBody {
		return fmt.Errorf(
			"comparison policy %q requires the fixed %s blocking response",
			spec.ComparisonPolicy,
			side,
		)
	}
	if observation.SecurityDecision != "deny" {
		return fmt.Errorf(
			"comparison policy %q requires %s deny security decision",
			spec.ComparisonPolicy,
			side,
		)
	}
	if observation.Host != spec.Request.Host || observation.SNI != "" || len(observation.Steps) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires the pinned %s gateway observation",
			spec.ComparisonPolicy,
			side,
		)
	}
	contentType, err := singleDifferentialHeader(observation.Headers, "Content-Type")
	if err != nil || !strings.HasPrefix(strings.ToLower(contentType), "text/plain") {
		return fmt.Errorf(
			"comparison policy %q requires the fixed %s plain-text blocking response",
			spec.ComparisonPolicy,
			side,
		)
	}
	if !observation.Upstream.Received || observation.UpstreamAddress == "" ||
		observation.UpstreamFixture != spec.Fixture.Name ||
		observation.Upstream.Fixture != observation.UpstreamFixture {
		return fmt.Errorf(
			"comparison policy %q requires one identified %s fixture request",
			spec.ComparisonPolicy,
			side,
		)
	}
	if observation.RetryCount != 0 || len(observation.UpstreamCalls) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires exactly one %s fixture request",
			spec.ComparisonPolicy,
			side,
		)
	}
	wantPath := "/"
	if side == "oracle" {
		wantPath = "/?"
	}
	if observation.Upstream.Method != http.MethodPost || observation.Upstream.Path != wantPath {
		return fmt.Errorf(
			"comparison policy %q requires %s fixture request POST %s",
			spec.ComparisonPolicy,
			side,
			wantPath,
		)
	}
	if observation.Upstream.Host != observation.UpstreamAddress {
		return fmt.Errorf(
			"comparison policy %q requires %s upstream Host %q to equal fixture address %q",
			spec.ComparisonPolicy,
			side,
			observation.Upstream.Host,
			observation.UpstreamAddress,
		)
	}
	if err := validateDifferentialAIAWSBody(observation.Upstream.Body); err != nil {
		return fmt.Errorf(
			"comparison policy %q %s Comprehend request body: %w",
			spec.ComparisonPolicy,
			side,
			err,
		)
	}
	authorization, err := singleDifferentialHeader(observation.Upstream.Headers, "Authorization")
	if err != nil {
		return fmt.Errorf(
			"comparison policy %q %s Authorization: %w",
			spec.ComparisonPolicy,
			side,
			err,
		)
	}
	matches := differentialAWSAuthorizationPattern.FindStringSubmatch(authorization)
	wantSignedHeaders := differentialAWSCandidateSignedHeaders
	if side == "oracle" {
		wantSignedHeaders = differentialAWSOracleSignedHeaders
	}
	if len(matches) != 4 || matches[2] != wantSignedHeaders {
		return fmt.Errorf(
			"comparison policy %q %s Authorization does not match the pinned Comprehend SigV4 contract",
			spec.ComparisonPolicy,
			side,
		)
	}

	deleteDifferentialHeader(observation.Upstream.Headers, "Authorization")
	observation.Upstream.Headers["Authorization"] = []string{
		"AWS4-HMAC-SHA256 Credential=access/20000101/us-east-1/comprehend/aws4_request, " +
			"SignedHeaders=validated, Signature=" + strings.Repeat("0", 64),
	}
	observation.Upstream.Host = "fixture:" + observation.UpstreamFixture
	observation.Upstream.Path = "/"
	deleteDifferentialHeader(observation.Headers, "Content-Type")
	deleteDifferentialHeader(observation.Headers, "Content-Length")
	observation.Headers["Content-Type"] = []string{"text/plain"}
	return nil
}

func validateDifferentialAIAWSBody(body string) error {
	var payload struct {
		LanguageCode string `json:"LanguageCode"`
		TextSegments []struct {
			Text string `json:"Text"`
		} `json:"TextSegments"`
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("contains more than one JSON value")
		}
		return err
	}
	if payload.LanguageCode != "en" || len(payload.TextSegments) != 1 ||
		payload.TextSegments[0].Text != "toxic" {
		return fmt.Errorf("does not contain LanguageCode=en and the single toxic text segment")
	}
	return nil
}

func compareDifferentialFixtureOwnedFunctionEndpoint(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if err := validateDifferentialAWSLambdaPinnedCase(spec); err != nil {
		return false, "", err
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
		if err := normalizeDifferentialAWSLambdaObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func validateDifferentialAWSLambdaPinnedCase(spec DifferentialCase) error {
	if spec.Name != "aws-lambda-local-function-response" || spec.Plugin != "aws-lambda" ||
		spec.RouteID != "differential-aws-lambda-local-function" ||
		spec.ComparisonPolicy != "fixture-owned-function-endpoint" ||
		spec.Request.Method != http.MethodGet || spec.Request.Path != "/aws" ||
		spec.Request.Host != "gateway.example.test" || spec.Request.SNI != "" ||
		len(spec.Request.Headers) != 0 || spec.Request.Body != "" || len(spec.Steps) != 0 ||
		spec.Fixture.Name != "primary" || spec.Fixture.WireProtocol != "" ||
		spec.Fixture.ExpectedCalls != 1 || spec.Fixture.Response.Status != http.StatusOK ||
		len(spec.Fixture.Response.Headers) != 0 || spec.Fixture.Response.Body != "aws lambda invoked" ||
		spec.SecurityDecision != "allow" {
		return fmt.Errorf(
			"comparison policy %q requires the pinned APISIX 3.17 local function case",
			spec.ComparisonPolicy,
		)
	}
	return nil
}

func normalizeDifferentialAWSLambdaObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if observation.Status != http.StatusOK || observation.Body != spec.Fixture.Response.Body {
		return fmt.Errorf(
			"comparison policy %q requires the fixed %s 200 function response",
			spec.ComparisonPolicy,
			side,
		)
	}
	if observation.SecurityDecision != "allow" {
		return fmt.Errorf(
			"comparison policy %q requires %s allow security decision",
			spec.ComparisonPolicy,
			side,
		)
	}
	if observation.Host != spec.Request.Host || observation.SNI != "" || len(observation.Steps) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires the pinned %s gateway observation",
			spec.ComparisonPolicy,
			side,
		)
	}
	if !observation.Upstream.Received || observation.UpstreamAddress == "" ||
		observation.UpstreamFixture != spec.Fixture.Name ||
		observation.Upstream.Fixture != observation.UpstreamFixture {
		return fmt.Errorf(
			"comparison policy %q requires one identified %s fixture request",
			spec.ComparisonPolicy,
			side,
		)
	}
	if observation.RetryCount != 0 || len(observation.UpstreamCalls) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires exactly one %s fixture request",
			spec.ComparisonPolicy,
			side,
		)
	}
	fixtureHost, _, err := net.SplitHostPort(observation.UpstreamAddress)
	if err != nil {
		return fmt.Errorf(
			"comparison policy %q split %s fixture address %q: %w",
			spec.ComparisonPolicy,
			side,
			observation.UpstreamAddress,
			err,
		)
	}
	if observation.Upstream.Method != http.MethodGet ||
		observation.Upstream.Path != "/httptrigger?" ||
		observation.Upstream.Body != "" || len(observation.Upstream.Headers) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires %s exact GET /httptrigger? fixture request",
			spec.ComparisonPolicy,
			side,
		)
	}
	if observation.Upstream.Host != fixtureHost {
		return fmt.Errorf(
			"comparison policy %q requires %s upstream Host %q to equal fixture host %q",
			spec.ComparisonPolicy,
			side,
			observation.Upstream.Host,
			fixtureHost,
		)
	}
	observation.Upstream.Host = "fixture:" + observation.UpstreamFixture
	return nil
}
