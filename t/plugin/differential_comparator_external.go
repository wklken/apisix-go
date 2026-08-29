package pluginintegration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

func compareDifferentialAzureFunctionsFixtureInvocation(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialAzureFunctionsCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned Azure Functions case",
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
		if err := normalizeDifferentialAzureFunctionsObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialAzureFunctionsObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if observation.Status != http.StatusOK || observation.Body != spec.Fixture.Response.Body ||
		observation.Host != spec.Request.Host || observation.SNI != "" ||
		observation.SecurityDecision != "allow" || len(observation.Steps) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires the fixed %s successful function response",
			spec.ComparisonPolicy,
			side,
		)
	}
	extra, err := singleDifferentialHeader(observation.Headers, "X-Extra-Header")
	if err != nil || extra != "MUST" {
		return fmt.Errorf(
			"comparison policy %q requires %s X-Extra-Header=MUST: %v",
			spec.ComparisonPolicy,
			side,
			err,
		)
	}
	if err := validateDifferentialExternalSingleFixture(spec, side, observation); err != nil {
		return err
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
		observation.Upstream.Path != "/httptrigger?" || observation.Upstream.Body != "" ||
		observation.Upstream.Host != fixtureHost {
		return fmt.Errorf(
			"comparison policy %q requires %s exact GET /httptrigger? fixture request",
			spec.ComparisonPolicy,
			side,
		)
	}
	if err := validateDifferentialExternalHeaders(
		observation.Upstream.Headers,
		map[string]string{"X-Functions-Key": "test_key"},
	); err != nil {
		return fmt.Errorf(
			"comparison policy %q %s function headers: %w",
			spec.ComparisonPolicy,
			side,
			err,
		)
	}
	observation.Upstream.Host = "fixture:" + observation.UpstreamFixture
	return nil
}

func compareDifferentialOPAFixtureDecision(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialOPACases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned OPA case",
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
		if err := normalizeDifferentialOPAObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialOPAObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if observation.Status != http.StatusForbidden || observation.Body != "Give you a string reason" ||
		observation.Host != spec.Request.Host || observation.SNI != "" ||
		observation.SecurityDecision != "deny" || len(observation.Steps) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires the fixed %s OPA denial",
			spec.ComparisonPolicy,
			side,
		)
	}
	if err := validateDifferentialExternalSingleFixture(spec, side, observation); err != nil {
		return err
	}
	if observation.Upstream.Method != http.MethodPost ||
		observation.Upstream.Path != "/v1/data/example" ||
		observation.Upstream.Host != observation.UpstreamAddress {
		return fmt.Errorf(
			"comparison policy %q requires %s exact POST /v1/data/example fixture request",
			spec.ComparisonPolicy,
			side,
		)
	}
	if err := validateDifferentialExternalHeaders(
		observation.Upstream.Headers,
		map[string]string{"Content-Type": "application/json"},
	); err != nil {
		return fmt.Errorf(
			"comparison policy %q %s OPA headers: %w",
			spec.ComparisonPolicy,
			side,
			err,
		)
	}
	normalizedBody, err := normalizeDifferentialOPARequest(observation.Upstream.Body)
	if err != nil {
		return fmt.Errorf(
			"comparison policy %q %s OPA request body: %w",
			spec.ComparisonPolicy,
			side,
			err,
		)
	}
	observation.Upstream.Body = normalizedBody
	observation.Upstream.Host = "fixture:" + observation.UpstreamFixture
	return nil
}

type differentialOPARequestEnvelope struct {
	Input *differentialOPARequestInput `json:"input"`
}

type differentialOPARequestInput struct {
	Version *int                            `json:"version,omitempty"`
	Type    string                          `json:"type"`
	Request differentialOPAHTTPRequest      `json:"request"`
	Vars    differentialOPARequestVariables `json:"var"`
}

type differentialOPAHTTPRequest struct {
	Scheme  string                     `json:"scheme"`
	Method  string                     `json:"method"`
	Host    string                     `json:"host"`
	Port    int                        `json:"port"`
	Path    string                     `json:"path"`
	Headers map[string]json.RawMessage `json:"headers"`
	Query   map[string]json.RawMessage `json:"query"`
}

type differentialOPARequestVariables struct {
	ServerAddr string      `json:"server_addr"`
	ServerPort string      `json:"server_port"`
	RemoteAddr string      `json:"remote_addr"`
	RemotePort string      `json:"remote_port"`
	Timestamp  json.Number `json:"timestamp"`
}

func normalizeDifferentialOPARequest(body string) (string, error) {
	var payload differentialOPARequestEnvelope
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return "", err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return "", fmt.Errorf("contains more than one JSON value")
		}
		return "", err
	}
	if payload.Input == nil {
		return "", fmt.Errorf("missing field %q", "input")
	}
	input := payload.Input
	if input.Version != nil && *input.Version != 1 {
		return "", fmt.Errorf("version = %d, want 1 when present", *input.Version)
	}
	request := &input.Request
	if input.Type != "http" || request.Scheme != "http" || request.Method != http.MethodGet ||
		request.Host != "gateway.example.test" || request.Port < 1 || request.Port > 65535 ||
		request.Path != "/test" {
		return "", fmt.Errorf("request does not match the pinned HTTP input")
	}
	if err := validateDifferentialOPARawMap(
		request.Query,
		map[string]string{"test": "abcd", "user": "carla"},
		false,
	); err != nil {
		return "", fmt.Errorf("query: %w", err)
	}
	forwardedPortRaw, exists := request.Headers["x-forwarded-port"]
	if !exists {
		return "", fmt.Errorf("headers: missing field %q", "x-forwarded-port")
	}
	var forwardedPort string
	if err := json.Unmarshal(forwardedPortRaw, &forwardedPort); err != nil {
		return "", fmt.Errorf("headers: field %q must be a string: %w", "x-forwarded-port", err)
	}
	if forwardedPort != strconv.Itoa(request.Port) {
		return "", fmt.Errorf(
			"headers: field %q = %q, want request port %d",
			"x-forwarded-port",
			forwardedPort,
			request.Port,
		)
	}
	delete(request.Headers, "x-forwarded-port")
	if err := validateDifferentialOPARawMap(
		request.Headers,
		map[string]string{
			"host":              "gateway.example.test",
			"test-header":       "only-for-test",
			"user-agent":        "Go-http-client/1.1",
			"x-forwarded-host":  "gateway.example.test",
			"x-forwarded-proto": "http",
		},
		true,
	); err != nil {
		return "", fmt.Errorf("headers: %w", err)
	}
	delete(request.Headers, "connection")
	for label, value := range map[string]string{
		"server_addr": input.Vars.ServerAddr,
		"remote_addr": input.Vars.RemoteAddr,
	} {
		address := net.ParseIP(value)
		if address == nil || !address.IsLoopback() {
			return "", fmt.Errorf("%s = %q, want a loopback address", label, value)
		}
	}
	for label, value := range map[string]string{
		"server_port": input.Vars.ServerPort,
		"remote_port": input.Vars.RemotePort,
	} {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != value {
			return "", fmt.Errorf("%s = %q, want a canonical TCP port", label, value)
		}
	}
	if input.Vars.ServerPort != strconv.Itoa(request.Port) {
		return "", fmt.Errorf(
			"server_port = %q, want request port %d",
			input.Vars.ServerPort,
			request.Port,
		)
	}
	timestamp, err := strconv.ParseInt(input.Vars.Timestamp.String(), 10, 64)
	if err != nil || timestamp <= 0 || strconv.FormatInt(timestamp, 10) != input.Vars.Timestamp.String() {
		return "", fmt.Errorf("timestamp = %q, want a positive integer", input.Vars.Timestamp)
	}
	input.Vars.ServerAddr = "loopback"
	input.Vars.RemoteAddr = "loopback"
	input.Vars.ServerPort = "validated"
	input.Vars.RemotePort = "validated"
	input.Vars.Timestamp = json.Number("1")
	request.Port = 1
	normalized, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

func validateDifferentialOPARawMap(
	values map[string]json.RawMessage,
	required map[string]string,
	allowConnection bool,
) error {
	if values == nil {
		return fmt.Errorf("missing object")
	}
	for name, raw := range values {
		want, exists := required[name]
		if !exists {
			if allowConnection && name == "connection" {
				want = "close"
			} else {
				return fmt.Errorf("unknown field %q", name)
			}
		}
		var got string
		if err := json.Unmarshal(raw, &got); err != nil {
			return fmt.Errorf("field %q must be a string: %w", name, err)
		}
		if got != want {
			return fmt.Errorf("field %q = %q, want %q", name, got, want)
		}
	}
	for name := range required {
		if _, exists := values[name]; !exists {
			return fmt.Errorf("missing field %q", name)
		}
	}
	return nil
}

func compareDifferentialDingTalkAuthFixtureOAuth(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialDingTalkAuthCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned DingTalk case",
			spec.ComparisonPolicy,
		)
	}
	return compareDifferentialExternalOAuth(
		spec,
		left,
		right,
		policy,
		differentialExternalOAuthContract{
			cookieName: "dingtalk_session",
			calls: []differentialExternalOAuthCallContract{
				{
					method:  http.MethodPost,
					path:    "/v1.0/oauth2/accessToken",
					headers: map[string]string{"Content-Type": "application/json"},
					body: map[string]string{
						"appKey": "testappkey", "appSecret": "testappsecret",
					},
				},
				{
					method:  http.MethodPost,
					path:    "/topapi/v2/user/getuserinfo?access_token=access-token-a",
					headers: map[string]string{"Content-Type": "application/json"},
					body:    map[string]string{"code": "valid_code"},
				},
				{
					method:   http.MethodGet,
					path:     "/hello",
					headers:  map[string]string{},
					userinfo: map[string]string{"name": "Alice", "userid": "user-a"},
					gateway:  true,
				},
			},
		},
	)
}

func compareDifferentialFeishuAuthFixtureOAuth(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, differentialFeishuAuthCases()[0]) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned Feishu case",
			spec.ComparisonPolicy,
		)
	}
	return compareDifferentialExternalOAuth(
		spec,
		left,
		right,
		policy,
		differentialExternalOAuthContract{
			cookieName: "feishu_session",
			calls: []differentialExternalOAuthCallContract{
				{
					method:  http.MethodPost,
					path:    "/token",
					headers: map[string]string{"Content-Type": "application/json"},
					body: map[string]string{
						"grant_type": "authorization_code",
						"client_id":  "123", "client_secret": "456",
						"redirect_uri": "https://example.com/callback", "code": "passed",
					},
				},
				{
					method: http.MethodGet,
					path:   "/userinfo",
					headers: map[string]string{
						"Authorization": "Bearer access-token-a", "Content-Type": "application/json",
					},
				},
				{
					method:   http.MethodGet,
					path:     "/hello",
					headers:  map[string]string{},
					userinfo: map[string]string{"name": "Alice", "open_id": "ou-a"},
					gateway:  true,
				},
			},
		},
	)
}

type differentialExternalOAuthContract struct {
	cookieName string
	calls      []differentialExternalOAuthCallContract
}

type differentialExternalOAuthCallContract struct {
	method   string
	path     string
	headers  map[string]string
	body     map[string]string
	userinfo map[string]string
	gateway  bool
}

func compareDifferentialExternalOAuth(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
	contract differentialExternalOAuthContract,
) (bool, string, error) {
	left = copyDifferentialObservation(left)
	right = copyDifferentialObservation(right)
	for _, side := range []struct {
		name        string
		observation *DifferentialObservation
	}{
		{name: "candidate", observation: &left},
		{name: "oracle", observation: &right},
	} {
		if err := normalizeDifferentialExternalOAuthObservation(
			spec,
			side.name,
			side.observation,
			contract,
		); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialExternalOAuthObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
	contract differentialExternalOAuthContract,
) error {
	if observation.Status != http.StatusOK || observation.Body != spec.Fixture.Response.Body ||
		observation.Host != spec.Request.Host || observation.SNI != "" ||
		observation.SecurityDecision != "allow" || len(observation.Steps) != 0 {
		return fmt.Errorf(
			"comparison policy %q requires the fixed %s OAuth success response",
			spec.ComparisonPolicy,
			side,
		)
	}
	if observation.UpstreamFixture != spec.Fixture.Name || observation.UpstreamAddress == "" ||
		observation.RetryCount != 0 || len(observation.UpstreamCalls) != len(contract.calls) ||
		len(contract.calls) != spec.Fixture.ExpectedCalls {
		return fmt.Errorf(
			"comparison policy %q requires exactly %d identified %s fixture calls",
			spec.ComparisonPolicy,
			len(contract.calls),
			side,
		)
	}
	if !reflect.DeepEqual(observation.Upstream, observation.UpstreamCalls[len(observation.UpstreamCalls)-1]) {
		return fmt.Errorf(
			"comparison policy %q requires %s Upstream to identify the final fixture call",
			spec.ComparisonPolicy,
			side,
		)
	}
	for index, callContract := range contract.calls {
		call := &observation.UpstreamCalls[index]
		if !call.Received || call.Fixture != spec.Fixture.Name || call.Method != callContract.method {
			return fmt.Errorf(
				"comparison policy %q %s fixture call %d has the wrong identity or method",
				spec.ComparisonPolicy,
				side,
				index,
			)
		}
		if err := normalizeDifferentialExternalEmptyQueryPath(call, callContract.path); err != nil {
			return fmt.Errorf(
				"comparison policy %q %s fixture call %d path: %w",
				spec.ComparisonPolicy,
				side,
				index,
				err,
			)
		}
		wantHost := observation.UpstreamAddress
		if callContract.gateway {
			wantHost = "differential.example.test"
		}
		if call.Host != wantHost {
			return fmt.Errorf(
				"comparison policy %q %s fixture call %d Host = %q, want %q",
				spec.ComparisonPolicy,
				side,
				index,
				call.Host,
				wantHost,
			)
		}
		if callContract.gateway {
			if call.Body != "" {
				return fmt.Errorf("comparison policy %q %s gateway call has a body", spec.ComparisonPolicy, side)
			}
			normalizedUserinfo, err := normalizeDifferentialExternalUserinfoHeader(
				call.Headers,
				callContract.userinfo,
			)
			if err != nil {
				return fmt.Errorf(
					"comparison policy %q %s upstream X-Userinfo: %w",
					spec.ComparisonPolicy,
					side,
					err,
				)
			}
			call.Headers = map[string][]string{"X-Userinfo": {normalizedUserinfo}}
			continue
		}
		if err := validateDifferentialExternalHeaders(call.Headers, callContract.headers); err != nil {
			return fmt.Errorf(
				"comparison policy %q %s fixture call %d headers: %w",
				spec.ComparisonPolicy,
				side,
				index,
				err,
			)
		}
		if callContract.body == nil {
			if call.Body != "" {
				return fmt.Errorf(
					"comparison policy %q %s fixture call %d requires an empty body",
					spec.ComparisonPolicy,
					side,
					index,
				)
			}
		} else {
			normalizedBody, err := normalizeDifferentialExternalStringObject(call.Body, callContract.body)
			if err != nil {
				return fmt.Errorf(
					"comparison policy %q %s fixture call %d body: %w",
					spec.ComparisonPolicy,
					side,
					index,
					err,
				)
			}
			call.Body = normalizedBody
		}
		call.Host = "fixture:" + spec.Fixture.Name
	}
	observation.Upstream = observation.UpstreamCalls[len(observation.UpstreamCalls)-1]
	if err := normalizeDifferentialExternalSessionCookie(observation, contract.cookieName); err != nil {
		return fmt.Errorf(
			"comparison policy %q %s session cookie: %w",
			spec.ComparisonPolicy,
			side,
			err,
		)
	}
	return nil
}

func normalizeDifferentialExternalEmptyQueryPath(
	call *DifferentialUpstreamObservation,
	want string,
) error {
	if strings.Contains(want, "?") {
		parsed, err := url.ParseRequestURI(call.Path)
		if err != nil {
			return err
		}
		wantURL, err := url.ParseRequestURI(want)
		if err != nil {
			return err
		}
		if parsed.Path != wantURL.Path || !reflect.DeepEqual(parsed.Query(), wantURL.Query()) {
			return fmt.Errorf("got %q, want %q", call.Path, want)
		}
		call.Path = want
		return nil
	}
	if call.Path != want && call.Path != want+"?" {
		return fmt.Errorf("got %q, want %q with no query", call.Path, want)
	}
	call.Path = want
	return nil
}

func normalizeDifferentialExternalStringObject(body string, want map[string]string) (string, error) {
	var got map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(body))
	if err := decoder.Decode(&got); err != nil {
		return "", err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return "", fmt.Errorf("contains more than one JSON value")
		}
		return "", err
	}
	if len(got) != len(want) {
		return "", fmt.Errorf("field count = %d, want %d", len(got), len(want))
	}
	for name, wantValue := range want {
		raw, exists := got[name]
		if !exists {
			return "", fmt.Errorf("missing field %q", name)
		}
		var gotValue string
		if err := json.Unmarshal(raw, &gotValue); err != nil {
			return "", fmt.Errorf("field %q must be a string: %w", name, err)
		}
		if gotValue != wantValue {
			return "", fmt.Errorf("field %q = %q, want %q", name, gotValue, wantValue)
		}
	}
	normalized, err := json.Marshal(want)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

func normalizeDifferentialExternalUserinfoHeader(
	headers map[string][]string,
	want map[string]string,
) (string, error) {
	value, err := singleDifferentialHeader(headers, "X-Userinfo")
	if err != nil {
		return "", err
	}
	if err := validateDifferentialExternalHeaders(headers, map[string]string{"X-Userinfo": value}); err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	normalized, err := normalizeDifferentialExternalStringObject(string(decoded), want)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(normalized)), nil
}

func normalizeDifferentialExternalSessionCookie(
	observation *DifferentialObservation,
	wantName string,
) error {
	values := differentialHeaderValues(observation.Headers, "Set-Cookie")
	if len(values) != 1 {
		return fmt.Errorf("Set-Cookie count = %d, want 1", len(values))
	}
	cookie, err := http.ParseSetCookie(values[0])
	if err != nil {
		return err
	}
	if cookie.Name != wantName || cookie.Value == "" {
		return fmt.Errorf("cookie name/value = %q/%q", cookie.Name, cookie.Value)
	}
	if len(cookie.Unparsed) != 0 {
		return fmt.Errorf("cookie has unparsed attributes %q", cookie.Unparsed)
	}
	cookie.Value = "validated"
	if !cookie.Expires.IsZero() {
		cookie.Expires = time.Unix(1, 0).UTC()
	}
	deleteDifferentialHeader(observation.Headers, "Set-Cookie")
	observation.Headers["Set-Cookie"] = []string{cookie.String()}
	return nil
}

func validateDifferentialExternalSingleFixture(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if !observation.Upstream.Received || observation.UpstreamFixture != spec.Fixture.Name ||
		observation.UpstreamAddress == "" || observation.Upstream.Fixture != spec.Fixture.Name {
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
	return nil
}

func validateDifferentialExternalHeaders(
	headers map[string][]string,
	want map[string]string,
) error {
	seen := make(map[string]struct{}, len(headers))
	for name, values := range headers {
		canonical := strings.ToLower(http.CanonicalHeaderKey(name))
		if _, exists := seen[canonical]; exists {
			return fmt.Errorf("duplicate header %q", name)
		}
		seen[canonical] = struct{}{}
		var wantValue string
		found := false
		for wantName, value := range want {
			if strings.EqualFold(name, wantName) {
				wantValue = value
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unexpected header %q", name)
		}
		if len(values) != 1 || values[0] != wantValue {
			return fmt.Errorf("header %q = %q, want %q", name, values, wantValue)
		}
	}
	if len(seen) != len(want) {
		return fmt.Errorf("header count = %d, want %d", len(seen), len(want))
	}
	return nil
}
