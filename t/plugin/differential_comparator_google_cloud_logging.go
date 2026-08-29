package pluginintegration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
)

func compareDifferentialGoogleCloudLoggingFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialGoogleCloudLoggingCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned Google Cloud Logging case",
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
		if err := normalizeDifferentialGoogleCloudLoggingObservation(
			spec, side.name, side.observation,
		); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialGoogleCloudLoggingObservation(
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
	if err := validateDifferentialGoogleCloudLoggingGatewayHeaders(
		side, step.Headers, len(spec.Fixture.Response.Body),
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
		len(observation.UpstreamCalls) != spec.Fixture.ExpectedCalls {
		return fmt.Errorf(
			"comparison policy %q requires exactly %d identified %s fixture calls",
			spec.ComparisonPolicy, spec.Fixture.ExpectedCalls, side,
		)
	}
	if !differentialLoggerUpstreamIsCapturedCall(observation.Upstream, observation.UpstreamCalls) {
		return fmt.Errorf(
			"comparison policy %q %s summary upstream is not a captured call",
			spec.ComparisonPolicy,
			side,
		)
	}

	origin, token, entries, err := differentialGoogleCloudLoggingCalls(spec, side, observation.UpstreamCalls)
	if err != nil {
		return err
	}
	if origin.Host != "differential.example.test" || len(origin.Headers) != 0 || origin.Body != "" {
		return fmt.Errorf("comparison policy %q %s origin request is not the pinned GET", spec.ComparisonPolicy, side)
	}
	for name, call := range map[string]*DifferentialUpstreamObservation{
		"token":   &token,
		"entries": &entries,
	} {
		if err := validateDifferentialLoggerFixtureHost(
			side, call.Host, observation.UpstreamAddress, false,
		); err != nil {
			return fmt.Errorf("comparison policy %q %s %s Host: %w", spec.ComparisonPolicy, side, name, err)
		}
	}
	if err := validateDifferentialLoggerHeaders(token.Headers, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	}); err != nil {
		return fmt.Errorf("comparison policy %q %s token headers: %w", spec.ComparisonPolicy, side, err)
	}
	canonicalAssertion, err := validateDifferentialGoogleCloudLoggingTokenForm(
		token.Body, observation.UpstreamAddress,
	)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s token form: %w", spec.ComparisonPolicy, side, err)
	}
	if err := validateDifferentialLoggerHeaders(entries.Headers, map[string]string{
		"Authorization": "Bearer differential-access-token",
		"Content-Type":  "application/json",
	}); err != nil {
		return fmt.Errorf("comparison policy %q %s entries headers: %w", spec.ComparisonPolicy, side, err)
	}
	canonicalEntries, err := validateDifferentialGoogleCloudLoggingEntries(entries.Body, spec.RouteID)
	if err != nil {
		return fmt.Errorf("comparison policy %q %s entries payload: %w", spec.ComparisonPolicy, side, err)
	}

	origin.Path = differentialGoogleCloudLoggingGatewayPath
	token.Path = differentialGoogleCloudLoggingTokenPath
	token.Host = "fixture:" + spec.Fixture.Name
	token.Body = url.Values{
		"assertion":  {canonicalAssertion},
		"grant_type": {differentialGoogleCloudLoggingJWTBearerGrantType},
	}.Encode()
	entries.Path = differentialGoogleCloudLoggingEntriesPath
	entries.Host = "fixture:" + spec.Fixture.Name
	entries.Body = canonicalEntries
	observation.UpstreamAddress = "fixture:" + spec.Fixture.Name
	observation.UpstreamCalls = []DifferentialUpstreamObservation{origin, token, entries}
	observation.Upstream = entries
	return nil
}

func validateDifferentialGoogleCloudLoggingGatewayHeaders(
	side string,
	headers map[string][]string,
	bodyLength int,
) error {
	for name := range headers {
		switch strings.ToLower(http.CanonicalHeaderKey(name)) {
		case "content-length", "content-type", "date", "server":
		default:
			return fmt.Errorf("unapproved gateway response header %q", name)
		}
	}
	contentType, err := singleDifferentialHeader(headers, "Content-Type")
	if err != nil || contentType != "application/json" {
		return fmt.Errorf("Content-Type = %q, want application/json: %v", contentType, err)
	}
	contentLength, err := singleDifferentialHeader(headers, "Content-Length")
	wantContentLength := fmt.Sprintf("%d", bodyLength)
	if err != nil || contentLength != wantContentLength {
		return fmt.Errorf("Content-Length = %q, want %s: %v", contentLength, wantContentLength, err)
	}
	server, err := singleDifferentialHeader(headers, "Server")
	if err != nil {
		return err
	}
	switch side {
	case "candidate":
		if !strings.HasPrefix(server, "APISIX/") {
			return fmt.Errorf("candidate Server = %q, want APISIX identity", server)
		}
		date, err := singleDifferentialHeader(headers, "Date")
		if err != nil {
			return err
		}
		if _, err := http.ParseTime(date); err != nil {
			return fmt.Errorf("candidate Date = %q: %w", date, err)
		}
	case "oracle":
		if server != "APISIX/3.17.0" {
			return fmt.Errorf("oracle Server = %q, want APISIX/3.17.0", server)
		}
		if len(differentialHeaderValues(headers, "Date")) != 0 {
			return fmt.Errorf("oracle Date is unexpectedly present")
		}
	default:
		return fmt.Errorf("unknown gateway side %q", side)
	}
	for name := range headers {
		delete(headers, name)
	}
	headers["Content-Type"] = []string{"application/json"}
	return nil
}

func differentialGoogleCloudLoggingCalls(
	spec DifferentialCase,
	side string,
	calls []DifferentialUpstreamObservation,
) (
	DifferentialUpstreamObservation,
	DifferentialUpstreamObservation,
	DifferentialUpstreamObservation,
	error,
) {
	var origin DifferentialUpstreamObservation
	var token DifferentialUpstreamObservation
	var entries DifferentialUpstreamObservation
	for _, call := range calls {
		if !call.Received || call.Fixture != spec.Fixture.Name {
			return origin, token, entries, fmt.Errorf(
				"comparison policy %q %s fixture call identity is invalid", spec.ComparisonPolicy, side,
			)
		}
		switch {
		case call.Method == http.MethodGet &&
			differentialLoggerRequestTargetMatches(call.Path, differentialGoogleCloudLoggingGatewayPath):
			if origin.Received {
				return origin, token, entries, fmt.Errorf(
					"comparison policy %q %s has duplicate origin GET",
					spec.ComparisonPolicy,
					side,
				)
			}
			origin = call
		case call.Method == http.MethodPost &&
			differentialLoggerRequestTargetMatches(call.Path, differentialGoogleCloudLoggingTokenPath):
			if token.Received {
				return origin, token, entries, fmt.Errorf(
					"comparison policy %q %s has duplicate token POST",
					spec.ComparisonPolicy,
					side,
				)
			}
			token = call
		case call.Method == http.MethodPost &&
			differentialLoggerRequestTargetMatches(call.Path, differentialGoogleCloudLoggingEntriesPath):
			if entries.Received {
				return origin, token, entries, fmt.Errorf(
					"comparison policy %q %s has duplicate entries POST",
					spec.ComparisonPolicy,
					side,
				)
			}
			entries = call
		default:
			return origin, token, entries, fmt.Errorf(
				"comparison policy %q %s has an unapproved fixture call %s %s",
				spec.ComparisonPolicy, side, call.Method, call.Path,
			)
		}
	}
	if !origin.Received {
		return origin, token, entries, fmt.Errorf(
			"comparison policy %q %s fixture calls are missing GET %s",
			spec.ComparisonPolicy, side, differentialGoogleCloudLoggingGatewayPath,
		)
	}
	if !token.Received {
		return origin, token, entries, fmt.Errorf(
			"comparison policy %q %s fixture calls are missing POST %s",
			spec.ComparisonPolicy, side, differentialGoogleCloudLoggingTokenPath,
		)
	}
	if !entries.Received {
		return origin, token, entries, fmt.Errorf(
			"comparison policy %q %s fixture calls are missing POST %s",
			spec.ComparisonPolicy, side, differentialGoogleCloudLoggingEntriesPath,
		)
	}
	return origin, token, entries, nil
}

func validateDifferentialGoogleCloudLoggingTokenForm(body string, fixtureAddress string) (string, error) {
	form, err := url.ParseQuery(body)
	if err != nil {
		return "", fmt.Errorf("parse form: %w", err)
	}
	if len(form) != 2 {
		return "", fmt.Errorf("form fields = %d, want exactly assertion and grant_type", len(form))
	}
	grantType, err := singleDifferentialFormValue(form, "grant_type")
	if err != nil || grantType != differentialGoogleCloudLoggingJWTBearerGrantType {
		return "", fmt.Errorf("grant_type = %q, want JWT bearer: %v", grantType, err)
	}
	assertion, err := singleDifferentialFormValue(form, "assertion")
	if err != nil {
		return "", fmt.Errorf("assertion: %w", err)
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("assertion is not a three-part signed JWT")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("decode JWT header: %w", err)
	}
	header, err := decodeDifferentialJSONObject(
		string(headerJSON), map[string]struct{}{"alg": {}, "typ": {}}, []string{"alg", "typ"},
	)
	if err != nil {
		return "", fmt.Errorf("JWT header: %w", err)
	}
	if err := validateDifferentialGoogleCloudLoggingJSONString(header["alg"], "alg", "RS256"); err != nil {
		return "", err
	}
	if err := validateDifferentialGoogleCloudLoggingJSONString(header["typ"], "typ", "JWT"); err != nil {
		return "", err
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT claims: %w", err)
	}
	claims, err := decodeDifferentialJSONObject(
		string(claimsJSON),
		map[string]struct{}{"iss": {}, "aud": {}, "scope": {}, "iat": {}, "exp": {}},
		[]string{"iss", "aud", "scope", "iat", "exp"},
	)
	if err != nil {
		return "", fmt.Errorf("JWT claims: %w", err)
	}
	if err := validateDifferentialGoogleCloudLoggingJSONString(
		claims["iss"], "iss", differentialGoogleCloudLoggingClientEmail,
	); err != nil {
		return "", err
	}
	wantAudience := "http://" + fixtureAddress + differentialGoogleCloudLoggingTokenPath
	if err := validateDifferentialGoogleCloudLoggingJSONString(claims["aud"], "aud", wantAudience); err != nil {
		return "", err
	}
	if err := validateDifferentialGoogleCloudLoggingJSONString(
		claims["scope"], "scope", differentialGoogleCloudLoggingScope,
	); err != nil {
		return "", err
	}
	var issuedAt int64
	if err := json.Unmarshal(claims["iat"], &issuedAt); err != nil || issuedAt <= 0 {
		return "", fmt.Errorf("iat is not a positive integer: %v", err)
	}
	var expiresAt int64
	if err := json.Unmarshal(claims["exp"], &expiresAt); err != nil || expiresAt-issuedAt != 3600 {
		return "", fmt.Errorf("exp-iat = %d, want 3600: %v", expiresAt-issuedAt, err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 128 {
		return "", fmt.Errorf("JWT signature is not a 128-byte RSA signature: %v", err)
	}
	canonicalHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	canonicalClaims := base64.RawURLEncoding.EncodeToString([]byte(
		`{"iss":"` + differentialGoogleCloudLoggingClientEmail +
			`","aud":"http://fixture` + differentialGoogleCloudLoggingTokenPath +
			`","scope":"` + differentialGoogleCloudLoggingScope + `","iat":0,"exp":3600}`,
	))
	canonicalSignature := base64.RawURLEncoding.EncodeToString(make([]byte, 128))
	return canonicalHeader + "." + canonicalClaims + "." + canonicalSignature, nil
}

func singleDifferentialFormValue(form url.Values, name string) (string, error) {
	values, exists := form[name]
	if !exists || len(values) != 1 {
		return "", fmt.Errorf("field %q has %d values, want exactly one", name, len(values))
	}
	return values[0], nil
}

func validateDifferentialGoogleCloudLoggingJSONString(
	raw json.RawMessage,
	name string,
	want string,
) error {
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("%s is not a string: %w", name, err)
	}
	if got != want {
		return fmt.Errorf("%s = %q, want %q", name, got, want)
	}
	return nil
}

func validateDifferentialGoogleCloudLoggingEntries(body string, routeID string) (string, error) {
	root, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{"entries": {}, "partialSuccess": {}},
		[]string{"entries", "partialSuccess"},
	)
	if err != nil {
		return "", err
	}
	var partialSuccess bool
	if err := json.Unmarshal(root["partialSuccess"], &partialSuccess); err != nil || partialSuccess {
		return "", fmt.Errorf("partialSuccess must be false: %v", err)
	}
	entries, err := decodeDifferentialJSONArray(root["entries"])
	if err != nil || len(entries) != 1 {
		return "", fmt.Errorf("entries contain %d records, want exactly one: %v", len(entries), err)
	}
	entry, err := decodeDifferentialJSONObject(
		string(entries[0]),
		map[string]struct{}{
			"jsonPayload": {}, "labels": {}, "timestamp": {}, "resource": {},
			"insertId": {}, "logName": {},
		},
		[]string{"jsonPayload", "labels", "timestamp", "resource", "logName"},
	)
	if err != nil {
		return "", err
	}
	if err := validateDifferentialLoggerCustomObject(
		string(entry["jsonPayload"]), "case", "google-cloud-logging", routeID,
	); err != nil {
		return "", fmt.Errorf("jsonPayload: %w", err)
	}
	labels, err := decodeDifferentialJSONObject(
		string(entry["labels"]), map[string]struct{}{"source": {}}, []string{"source"},
	)
	if err != nil {
		return "", fmt.Errorf("labels: %w", err)
	}
	if err := validateDifferentialGoogleCloudLoggingJSONString(
		labels["source"], "labels.source", "apache-apisix-google-cloud-logging",
	); err != nil {
		return "", err
	}
	var timestamp string
	if err := json.Unmarshal(entry["timestamp"], &timestamp); err != nil {
		return "", fmt.Errorf("timestamp is not a string: %w", err)
	}
	parsedTimestamp, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil || !strings.HasSuffix(timestamp, "Z") || parsedTimestamp.Location() != time.UTC {
		return "", fmt.Errorf("timestamp %q is not UTC RFC3339Nano", timestamp)
	}
	resource, err := decodeDifferentialJSONObject(
		string(entry["resource"]),
		map[string]struct{}{"type": {}, "labels": {}},
		[]string{"type", "labels"},
	)
	if err != nil {
		return "", fmt.Errorf("resource: %w", err)
	}
	if err := validateDifferentialGoogleCloudLoggingJSONString(
		resource["type"],
		"resource.type",
		"global",
	); err != nil {
		return "", err
	}
	resourceLabels, err := decodeDifferentialJSONObject(
		string(resource["labels"]),
		map[string]struct{}{"project_id": {}},
		[]string{"project_id"},
	)
	if err != nil {
		return "", fmt.Errorf("resource.labels: %w", err)
	}
	if err := validateDifferentialGoogleCloudLoggingJSONString(
		resourceLabels["project_id"], "resource.labels.project_id", differentialGoogleCloudLoggingProjectID,
	); err != nil {
		return "", err
	}
	wantLogName := "projects/" + differentialGoogleCloudLoggingProjectID + "/logs/" +
		differentialGoogleCloudLoggingLogID
	if err := validateDifferentialGoogleCloudLoggingJSONString(entry["logName"], "logName", wantLogName); err != nil {
		return "", err
	}
	if insertID, exists := entry["insertId"]; exists {
		var value string
		if err := json.Unmarshal(insertID, &value); err != nil || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("insertId is not a nonempty string: %v", err)
		}
	}
	return `{"entries":[{"jsonPayload":{"case":"google-cloud-logging","route_id":"` + routeID +
		`"},"labels":{"source":"apache-apisix-google-cloud-logging"},"timestamp":"normalized",` +
		`"resource":{"type":"global","labels":{"project_id":"` + differentialGoogleCloudLoggingProjectID +
		`"}},"logName":"` + wantLogName + `"}],"partialSuccess":false}`, nil
}
