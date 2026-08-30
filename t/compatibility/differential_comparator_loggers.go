package pluginintegration

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

type differentialLoggerFixtureContract struct {
	pinned                DifferentialCase
	loggerMethod          string
	loggerPath            string
	oracleHostWithoutPort bool
	extraCalls            []differentialLoggerExpectedCall
	validateEntry         func(string, *DifferentialUpstreamObservation) error
}

type differentialLoggerExpectedCall struct {
	method   string
	path     string
	validate func(string, *DifferentialUpstreamObservation) error
}

func compareDifferentialClickHouseLoggerFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialLoggerFixtureDelivery(spec, left, right, policy, differentialLoggerFixtureContract{
		pinned:                mustDifferentialCase(spec.Name),
		loggerMethod:          http.MethodPost,
		loggerPath:            "/clickhouse",
		oracleHostWithoutPort: true,
		validateEntry: func(side string, call *DifferentialUpstreamObservation) error {
			if err := validateDifferentialLoggerHeaders(call.Headers, map[string]string{
				"Content-Type":          "application/json",
				"X-ClickHouse-User":     "default",
				"X-ClickHouse-Key":      "differential-password",
				"X-ClickHouse-Database": "default",
			}); err != nil {
				return fmt.Errorf("%s ClickHouse headers: %w", side, err)
			}
			const prefix = "INSERT INTO logs FORMAT JSONEachRow "
			if !strings.HasPrefix(call.Body, prefix) ||
				validateDifferentialLoggerCustomObject(
					strings.TrimPrefix(call.Body, prefix), "case", "clickhouse-logger", spec.RouteID,
				) != nil {
				return fmt.Errorf("%s ClickHouse payload is not the pinned single JSONEachRow entry", side)
			}
			call.Body = prefix + `{"case":"clickhouse-logger","route_id":"` + spec.RouteID + `"}`
			return nil
		},
	})
}

func compareDifferentialElasticsearchLoggerFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialLoggerFixtureDelivery(spec, left, right, policy, differentialLoggerFixtureContract{
		pinned:       mustDifferentialCase(spec.Name),
		loggerMethod: http.MethodPost,
		loggerPath:   "/_bulk",
		extraCalls: []differentialLoggerExpectedCall{{
			method: http.MethodGet,
			path:   "/",
			validate: func(side string, call *DifferentialUpstreamObservation) error {
				if len(call.Headers) != 0 || call.Body != "" {
					return fmt.Errorf("%s Elasticsearch version probe must have no semantic headers or body", side)
				}
				return nil
			},
		}},
		validateEntry: func(side string, call *DifferentialUpstreamObservation) error {
			if err := validateDifferentialLoggerHeaders(call.Headers, map[string]string{
				"Content-Type": "application/x-ndjson",
			}); err != nil {
				return fmt.Errorf("%s Elasticsearch headers: %w", side, err)
			}
			if err := validateDifferentialElasticsearchNDJSON(call.Body, spec.RouteID); err != nil {
				return fmt.Errorf("%s Elasticsearch NDJSON: %w", side, err)
			}
			call.Body = "{\"index\":{\"_index\":\"services\"}}\n" +
				"{\"custom_case\":\"elasticsearch-logger\",\"route_id\":\"" + spec.RouteID + "\"}\n"
			return nil
		},
	})
}

func compareDifferentialHTTPLoggerFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialLoggerFixtureDelivery(spec, left, right, policy, differentialLoggerFixtureContract{
		pinned:                mustDifferentialCase(spec.Name),
		loggerMethod:          http.MethodPost,
		loggerPath:            "/http-log",
		oracleHostWithoutPort: true,
		validateEntry: func(side string, call *DifferentialUpstreamObservation) error {
			if err := validateDifferentialLoggerHeaders(call.Headers, map[string]string{
				"Authorization": "Basic differential",
				"Content-Type":  "application/json",
			}); err != nil {
				return fmt.Errorf("%s HTTP logger headers: %w", side, err)
			}
			if err := validateDifferentialLoggerCustomObject(
				call.Body, "case", "http-logger", spec.RouteID,
			); err != nil {
				return fmt.Errorf("%s HTTP logger payload: %w", side, err)
			}
			call.Body = `{"case":"http-logger","route_id":"` + spec.RouteID + `"}`
			return nil
		},
	})
}

func compareDifferentialLokiLoggerFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialLoggerFixtureDelivery(spec, left, right, policy, differentialLoggerFixtureContract{
		pinned:       mustDifferentialCase(spec.Name),
		loggerMethod: http.MethodPost,
		loggerPath:   "/loki/api/v1/push",
		validateEntry: func(side string, call *DifferentialUpstreamObservation) error {
			if err := validateDifferentialLoggerHeaders(call.Headers, map[string]string{
				"Authorization": "test1234",
				"Content-Type":  "application/json",
				"X-Scope-OrgID": "tenant-differential",
			}); err != nil {
				return fmt.Errorf("%s Loki headers: %w", side, err)
			}
			if err := validateDifferentialLokiPayload(call.Body, spec.RouteID); err != nil {
				return fmt.Errorf("%s Loki payload: %w", side, err)
			}
			call.Body = "loki-payload:validated-single-entry"
			return nil
		},
	})
}

func compareDifferentialSplunkHECLoggingFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialLoggerFixtureDelivery(spec, left, right, policy, differentialLoggerFixtureContract{
		pinned:       mustDifferentialCase(spec.Name),
		loggerMethod: http.MethodPost,
		loggerPath:   "/services/collector",
		validateEntry: func(side string, call *DifferentialUpstreamObservation) error {
			if err := validateDifferentialLoggerHeaders(call.Headers, map[string]string{
				"Authorization": "Splunk BD274822-96AA-4DA6-90EC-18940FB2414C",
				"Content-Type":  "application/json",
			}); err != nil {
				return fmt.Errorf("%s Splunk headers: %w", side, err)
			}
			if err := validateDifferentialSplunkPayload(call.Body, spec.RouteID); err != nil {
				return fmt.Errorf("%s Splunk payload: %w", side, err)
			}
			call.Body = "splunk-payload:validated-single-event"
			return nil
		},
	})
}

func compareDifferentialTencentCloudCLSFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	return compareDifferentialLoggerFixtureDelivery(spec, left, right, policy, differentialLoggerFixtureContract{
		pinned:       mustDifferentialCase(spec.Name),
		loggerMethod: http.MethodPost,
		loggerPath:   "/structuredlog?topic_id=143b5d70-139b-4aec-b54e-bb97756916de",
		validateEntry: func(side string, call *DifferentialUpstreamObservation) error {
			contentType, err := singleDifferentialHeader(call.Headers, "Content-Type")
			if err != nil || contentType != "application/x-protobuf" {
				return fmt.Errorf("%s CLS Content-Type: got %q: %v", side, contentType, err)
			}
			authorization, err := singleDifferentialHeader(call.Headers, "Authorization")
			if err != nil {
				return fmt.Errorf("%s CLS Authorization: %w", side, err)
			}
			if len(call.Headers) != 2 {
				return fmt.Errorf("%s CLS headers contain unapproved semantic fields", side)
			}
			logTime, err := validateDifferentialTencentCLSProtobuf([]byte(call.Body), spec.RouteID)
			if err != nil {
				return fmt.Errorf("%s CLS protobuf: %w", side, err)
			}
			if err := validateDifferentialTencentCLSAuthorization(authorization, logTime); err != nil {
				return fmt.Errorf("%s CLS Authorization: %w", side, err)
			}
			call.Headers = map[string][]string{
				"Authorization": {"q-signature=validated"},
				"Content-Type":  {"application/x-protobuf"},
			}
			call.Body = "cls-protobuf:validated-single-entry"
			return nil
		},
	})
}

func compareDifferentialLoggerFixtureDelivery(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
	contract differentialLoggerFixtureContract,
) (bool, string, error) {
	if !reflect.DeepEqual(spec, contract.pinned) {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the exact pinned %s case",
			spec.ComparisonPolicy,
			contract.pinned.Plugin,
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
		if err := normalizeDifferentialLoggerFixtureObservation(
			spec, side.name, side.observation, contract,
		); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialNetworkLoggerGatewayHeaders(
	side string,
	headers map[string][]string,
	bodyLength int,
	candidateContentType string,
	oracleContentType string,
) error {
	for name := range headers {
		switch strings.ToLower(http.CanonicalHeaderKey(name)) {
		case "content-length", "content-type", "date", "server":
		default:
			return fmt.Errorf("unapproved gateway response header %q", name)
		}
	}
	wantContentType := candidateContentType
	if side == "oracle" {
		wantContentType = oracleContentType
	}
	contentType, err := singleDifferentialHeader(headers, "Content-Type")
	if err != nil || contentType != wantContentType {
		return fmt.Errorf("content type = %q, want %q: %v", contentType, wantContentType, err)
	}
	wantContentLength := strconv.Itoa(bodyLength)
	contentLength, err := singleDifferentialHeader(headers, "Content-Length")
	if err != nil || contentLength != wantContentLength {
		return fmt.Errorf("content length = %q, want %s: %v", contentLength, wantContentLength, err)
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
	headers["Content-Length"] = []string{wantContentLength}
	headers["Content-Type"] = []string{"fixture-response"}
	return nil
}

func normalizeDifferentialLoggerFixtureObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
	contract differentialLoggerFixtureContract,
) error {
	if len(observation.Steps) != 1 {
		return fmt.Errorf("comparison policy %q requires one %s gateway step", spec.ComparisonPolicy, side)
	}
	step := observation.Steps[0]
	wantStep := spec.Steps[0]
	if step.Status != spec.Fixture.Response.Status || step.Body != spec.Fixture.Response.Body ||
		step.Host != wantStep.Request.Host || step.SNI != wantStep.Request.SNI ||
		step.SecurityDecision != wantStep.SecurityDecision {
		return fmt.Errorf("comparison policy %q requires the exact %s gateway step", spec.ComparisonPolicy, side)
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
			spec.ComparisonPolicy,
			spec.Fixture.ExpectedCalls,
			side,
		)
	}
	if !differentialLoggerUpstreamIsCapturedCall(observation.Upstream, observation.UpstreamCalls) {
		return fmt.Errorf(
			"comparison policy %q %s summary upstream is not a captured call",
			spec.ComparisonPolicy,
			side,
		)
	}

	expected := make([]differentialLoggerExpectedCall, 0, len(contract.extraCalls)+2)
	expected = append(expected, differentialLoggerExpectedCall{
		method: spec.Steps[0].Request.Method,
		path:   spec.Steps[0].Request.Path,
		validate: func(side string, call *DifferentialUpstreamObservation) error {
			if call.Host != "differential.example.test" || len(call.Headers) != 0 || call.Body != "" {
				return fmt.Errorf("%s origin request does not preserve the pinned rewritten Host", side)
			}
			return nil
		},
	})
	expected = append(expected, contract.extraCalls...)
	expected = append(expected, differentialLoggerExpectedCall{
		method: contract.loggerMethod, path: contract.loggerPath, validate: contract.validateEntry,
	})
	canonicalCalls := make([]DifferentialUpstreamObservation, 0, len(expected))
	used := make([]bool, len(observation.UpstreamCalls))
	for _, want := range expected {
		index := -1
		for current := range observation.UpstreamCalls {
			call := observation.UpstreamCalls[current]
			if !used[current] && call.Method == want.method &&
				differentialLoggerRequestTargetMatches(call.Path, want.path) {
				index = current
				break
			}
		}
		if index < 0 {
			return fmt.Errorf(
				"comparison policy %q %s fixture calls are missing %s %s",
				spec.ComparisonPolicy,
				side,
				want.method,
				want.path,
			)
		}
		used[index] = true
		call := observation.UpstreamCalls[index]
		if !call.Received || call.Fixture != spec.Fixture.Name {
			return fmt.Errorf("comparison policy %q %s fixture call identity is invalid", spec.ComparisonPolicy, side)
		}
		if want.path != spec.Steps[0].Request.Path {
			if err := validateDifferentialLoggerFixtureHost(
				side,
				call.Host,
				observation.UpstreamAddress,
				contract.oracleHostWithoutPort,
			); err != nil {
				return fmt.Errorf("comparison policy %q %s logger fixture Host: %w", spec.ComparisonPolicy, side, err)
			}
		}
		if err := want.validate(side, &call); err != nil {
			return fmt.Errorf("comparison policy %q: %w", spec.ComparisonPolicy, err)
		}
		call.Path = want.path
		call.Host = "fixture:" + spec.Fixture.Name
		canonicalCalls = append(canonicalCalls, call)
	}
	observation.UpstreamCalls = canonicalCalls
	observation.Upstream = canonicalCalls[len(canonicalCalls)-1]
	return nil
}

func differentialLoggerRequestTargetMatches(got string, want string) bool {
	gotURL, gotErr := url.ParseRequestURI(got)
	wantURL, wantErr := url.ParseRequestURI(want)
	if gotErr != nil || wantErr != nil {
		return got == want
	}
	return gotURL.EscapedPath() == wantURL.EscapedPath() && gotURL.RawQuery == wantURL.RawQuery
}

func differentialLoggerUpstreamIsCapturedCall(
	upstream DifferentialUpstreamObservation,
	calls []DifferentialUpstreamObservation,
) bool {
	for _, call := range calls {
		if reflect.DeepEqual(upstream, call) {
			return true
		}
	}
	return false
}

func validateDifferentialLoggerFixtureHost(
	side string,
	host string,
	address string,
	oracleWithoutPort bool,
) error {
	addressHost, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("split fixture address %q: %w", address, err)
	}
	want := address
	if side == "oracle" && oracleWithoutPort {
		want = addressHost
	}
	if host != want {
		return fmt.Errorf("got %q, want %q", host, want)
	}
	return nil
}

func validateDifferentialLoggerHeaders(headers map[string][]string, want map[string]string) error {
	wantByLowerName := make(map[string]string, len(want))
	canonicalName := make(map[string]string, len(want))
	for name, value := range want {
		lowerName := strings.ToLower(name)
		wantByLowerName[lowerName] = value
		canonicalName[lowerName] = name
	}
	for name, value := range want {
		got, err := singleDifferentialHeader(headers, name)
		if err != nil || got != value {
			return fmt.Errorf("%s = %q, want %q: %v", name, got, value, err)
		}
	}
	for name := range headers {
		if _, exists := wantByLowerName[strings.ToLower(name)]; !exists {
			return fmt.Errorf("unapproved semantic header %q", name)
		}
	}
	if len(headers) != len(want) {
		return fmt.Errorf("semantic header count = %d, want %d", len(headers), len(want))
	}
	for name := range headers {
		delete(headers, name)
	}
	for lowerName, value := range wantByLowerName {
		headers[http.CanonicalHeaderKey(canonicalName[lowerName])] = []string{value}
	}
	return nil
}

func validateDifferentialLoggerCustomObject(body, key, value, routeID string) error {
	fields, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{key: {}, "route_id": {}},
		[]string{key, "route_id"},
	)
	if err != nil {
		return err
	}
	var got string
	if err := json.Unmarshal(fields[key], &got); err != nil {
		return fmt.Errorf("field %q is not a string: %w", key, err)
	}
	if got != value {
		return fmt.Errorf("field %q = %q, want %q", key, got, value)
	}
	var gotRouteID string
	if err := json.Unmarshal(fields["route_id"], &gotRouteID); err != nil {
		return fmt.Errorf("field route_id is not a string: %w", err)
	}
	if gotRouteID != routeID {
		return fmt.Errorf("field route_id = %q, want %q", gotRouteID, routeID)
	}
	return nil
}

func validateDifferentialElasticsearchNDJSON(body string, routeID string) error {
	if !strings.HasSuffix(body, "\n") {
		return fmt.Errorf("missing terminal newline")
	}
	lines := strings.Split(body, "\n")
	if len(lines) != 3 || lines[2] != "" {
		return fmt.Errorf("contains %d lines, want exactly two records", len(lines)-1)
	}
	action, err := decodeDifferentialJSONObject(
		lines[0], map[string]struct{}{"index": {}}, []string{"index"},
	)
	if err != nil {
		return fmt.Errorf("decode action: %w", err)
	}
	index, err := decodeDifferentialJSONObject(
		string(action["index"]), map[string]struct{}{"_index": {}}, []string{"_index"},
	)
	if err != nil {
		return fmt.Errorf("decode index action: %w", err)
	}
	var indexName string
	if err := json.Unmarshal(index["_index"], &indexName); err != nil || indexName != "services" {
		return fmt.Errorf("index = %q, want services: %v", indexName, err)
	}
	if err := validateDifferentialLoggerCustomObject(
		lines[1], "custom_case", "elasticsearch-logger", routeID,
	); err != nil {
		return fmt.Errorf("decode document: %w", err)
	}
	return nil
}

func validateDifferentialLokiPayload(body string, routeID string) error {
	root, err := decodeDifferentialJSONObject(
		body, map[string]struct{}{"streams": {}}, []string{"streams"},
	)
	if err != nil {
		return err
	}
	streams, err := decodeDifferentialJSONArray(root["streams"])
	if err != nil || len(streams) != 1 {
		return fmt.Errorf("streams must contain exactly one object: %v", err)
	}
	stream, err := decodeDifferentialJSONObject(
		string(streams[0]),
		map[string]struct{}{"stream": {}, "values": {}},
		[]string{"stream", "values"},
	)
	if err != nil {
		return err
	}
	labels, err := decodeDifferentialJSONObject(
		string(stream["stream"]), map[string]struct{}{"job": {}}, []string{"job"},
	)
	if err != nil {
		return err
	}
	var job string
	if err := json.Unmarshal(labels["job"], &job); err != nil || job != "apisix-differential" {
		return fmt.Errorf("job label = %q, want apisix-differential: %v", job, err)
	}
	values, err := decodeDifferentialJSONArray(stream["values"])
	if err != nil || len(values) != 1 {
		return fmt.Errorf("values must contain exactly one entry: %v", err)
	}
	entry, err := decodeDifferentialJSONArray(values[0])
	if err != nil || len(entry) != 2 {
		return fmt.Errorf("loki entry must contain timestamp and line: %v", err)
	}
	var timestamp, line string
	if err := json.Unmarshal(entry[0], &timestamp); err != nil ||
		!isDifferentialCanonicalDecimal(timestamp) || timestamp == "0" {
		return fmt.Errorf("timestamp %q is not a positive canonical decimal: %v", timestamp, err)
	}
	if _, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
		return fmt.Errorf("timestamp %q is out of int64 range", timestamp)
	}
	if err := json.Unmarshal(entry[1], &line); err != nil {
		return fmt.Errorf("log line is not a string: %w", err)
	}
	return validateDifferentialLoggerCustomObject(line, "case", "loki-logger", routeID)
}

func decodeDifferentialJSONArray(raw []byte) ([]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var values []json.RawMessage
	if err := decoder.Decode(&values); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, err
	}
	return values, nil
}

func validateDifferentialSplunkPayload(body string, routeID string) error {
	if strings.ContainsAny(body, "\r\n") {
		return fmt.Errorf("contains a newline delimiter not emitted by APISIX 3.17")
	}
	root, err := decodeDifferentialJSONObject(
		body,
		map[string]struct{}{
			"time": {}, "host": {}, "source": {}, "sourcetype": {}, "event": {},
		},
		[]string{"time", "host", "source", "sourcetype", "event"},
	)
	if err != nil {
		return err
	}
	var timestamp json.Number
	if err := json.Unmarshal(root["time"], &timestamp); err != nil {
		return fmt.Errorf("time is not a number: %w", err)
	}
	parsedTime, err := strconv.ParseFloat(timestamp.String(), 64)
	if err != nil || parsedTime <= 0 {
		return fmt.Errorf("time = %q, want positive Unix timestamp: %v", timestamp, err)
	}
	for name, want := range map[string]string{
		"source": "apache-apisix-splunk-hec-logging", "sourcetype": "_json",
	} {
		var got string
		if err := json.Unmarshal(root[name], &got); err != nil || got != want {
			return fmt.Errorf("%s = %q, want %q: %v", name, got, want, err)
		}
	}
	var host string
	if err := json.Unmarshal(root["host"], &host); err != nil || strings.TrimSpace(host) == "" {
		return fmt.Errorf("host must be a non-empty string: %v", err)
	}
	return validateDifferentialLoggerCustomObject(
		string(root["event"]), "message", "differential-splunk-event", routeID,
	)
}

func validateDifferentialTencentCLSProtobuf(body []byte, routeID string) (int64, error) {
	group, err := consumeDifferentialCLSSingleBytesField(body, 1, "LogGroupList")
	if err != nil {
		return 0, err
	}
	var logEntry []byte
	var source string
	seenLog := false
	seenSource := false
	for len(group) > 0 {
		number, fieldType, consumed := protowire.ConsumeTag(group)
		if consumed < 0 {
			return 0, protowire.ParseError(consumed)
		}
		group = group[consumed:]
		if fieldType != protowire.BytesType || (number != 1 && number != 4) {
			return 0, fmt.Errorf("log group contains unexpected field %d/%v", number, fieldType)
		}
		value, consumed := protowire.ConsumeBytes(group)
		if consumed < 0 {
			return 0, protowire.ParseError(consumed)
		}
		group = group[consumed:]
		switch number {
		case 1:
			if seenLog {
				return 0, fmt.Errorf("log group contains more than one log")
			}
			seenLog = true
			logEntry = value
		case 4:
			if seenSource {
				return 0, fmt.Errorf("log group contains duplicate source")
			}
			seenSource = true
			source = string(value)
		}
	}
	if !seenLog || !seenSource || net.ParseIP(source) == nil {
		return 0, fmt.Errorf("log group requires one log and an IP source")
	}
	return validateDifferentialTencentCLSLog(logEntry, routeID)
}

func consumeDifferentialCLSSingleBytesField(
	body []byte,
	wantNumber protowire.Number,
	name string,
) ([]byte, error) {
	number, fieldType, consumed := protowire.ConsumeTag(body)
	if consumed < 0 {
		return nil, protowire.ParseError(consumed)
	}
	if number != wantNumber || fieldType != protowire.BytesType {
		return nil, fmt.Errorf("%s field = %d/%v, want %d/bytes", name, number, fieldType, wantNumber)
	}
	body = body[consumed:]
	value, consumed := protowire.ConsumeBytes(body)
	if consumed < 0 {
		return nil, protowire.ParseError(consumed)
	}
	if consumed != len(body) {
		return nil, fmt.Errorf("%s contains more than one field", name)
	}
	return value, nil
}

func validateDifferentialTencentCLSLog(body []byte, routeID string) (int64, error) {
	var timestamp int64
	contents := make(map[string]string, 2)
	seenTimestamp := false
	for len(body) > 0 {
		number, fieldType, consumed := protowire.ConsumeTag(body)
		if consumed < 0 {
			return 0, protowire.ParseError(consumed)
		}
		body = body[consumed:]
		switch number {
		case 1:
			if seenTimestamp || fieldType != protowire.VarintType {
				return 0, fmt.Errorf("log contains duplicate or non-varint time")
			}
			value, consumed := protowire.ConsumeVarint(body)
			if consumed < 0 || value == 0 || value > uint64(^uint64(0)>>1) {
				return 0, fmt.Errorf("log time is invalid")
			}
			body = body[consumed:]
			timestamp = int64(value)
			seenTimestamp = true
		case 2:
			if fieldType != protowire.BytesType {
				return 0, fmt.Errorf("log content has wrong type")
			}
			value, consumed := protowire.ConsumeBytes(body)
			if consumed < 0 {
				return 0, protowire.ParseError(consumed)
			}
			body = body[consumed:]
			key, contentValue, err := decodeDifferentialTencentCLSContent(value)
			if err != nil {
				return 0, err
			}
			if _, exists := contents[key]; exists {
				return 0, fmt.Errorf("log contains duplicate content key %q", key)
			}
			contents[key] = contentValue
		default:
			return 0, fmt.Errorf("log contains unexpected field %d", number)
		}
	}
	if !seenTimestamp || len(contents) != 2 || contents["case"] != "tencent-cloud-cls" ||
		contents["route_id"] != routeID {
		return 0, fmt.Errorf("log contents do not match the pinned case and route_id")
	}
	return timestamp, nil
}

func decodeDifferentialTencentCLSContent(body []byte) (string, string, error) {
	var key string
	var value string
	seenKey := false
	seenValue := false
	for len(body) > 0 {
		number, fieldType, consumed := protowire.ConsumeTag(body)
		if consumed < 0 {
			return "", "", protowire.ParseError(consumed)
		}
		body = body[consumed:]
		if fieldType != protowire.BytesType {
			return "", "", fmt.Errorf("content field %d has type %v, want bytes", number, fieldType)
		}
		fieldValue, consumed := protowire.ConsumeBytes(body)
		if consumed < 0 {
			return "", "", protowire.ParseError(consumed)
		}
		body = body[consumed:]
		switch number {
		case 1:
			if seenKey {
				return "", "", fmt.Errorf("content contains duplicate key")
			}
			seenKey = true
			key = string(fieldValue)
		case 2:
			if seenValue {
				return "", "", fmt.Errorf("content contains duplicate value")
			}
			seenValue = true
			value = string(fieldValue)
		default:
			return "", "", fmt.Errorf("content contains unexpected field %d", number)
		}
	}
	if !seenKey || !seenValue || key == "" {
		return "", "", fmt.Errorf("content is malformed")
	}
	return key, value, nil
}

func validateDifferentialTencentCLSAuthorization(authorization string, logTimeMillis int64) error {
	values, err := parseDifferentialTencentCLSAuthorization(authorization)
	if err != nil {
		return err
	}
	wantFields := []string{
		"q-sign-algorithm", "q-ak", "q-sign-time", "q-key-time",
		"q-header-list", "q-url-param-list", "q-signature",
	}
	if len(values) != len(wantFields) {
		return fmt.Errorf("contains %d fields, want %d", len(values), len(wantFields))
	}
	for _, name := range wantFields {
		if len(values[name]) != 1 {
			return fmt.Errorf("%s has %d values, want one", name, len(values[name]))
		}
	}
	get := func(name string) string { return values[name][0] }
	if get("q-sign-algorithm") != "sha1" || get("q-ak") != "secret_id" ||
		get("q-header-list") != "" || get("q-url-param-list") != "" ||
		get("q-sign-time") != get("q-key-time") {
		return fmt.Errorf("static signing fields do not match the pinned CLS contract")
	}
	timeParts := strings.Split(get("q-sign-time"), ";")
	if len(timeParts) != 2 {
		return fmt.Errorf("q-sign-time is malformed")
	}
	start, startErr := strconv.ParseInt(timeParts[0], 10, 64)
	end, endErr := strconv.ParseInt(timeParts[1], 10, 64)
	if startErr != nil || endErr != nil || start <= 0 || end-start != 60 ||
		logTimeMillis/1000 < start-1 || logTimeMillis/1000 > start+1 {
		return fmt.Errorf("q-sign-time is not a 60-second window around the log timestamp")
	}
	wantSignature := differentialTencentCLSSignature("secret_key", get("q-sign-time"))
	if signature := get("q-signature"); signature != wantSignature ||
		len(signature) != sha1.Size*2 || strings.ToLower(signature) != signature {
		return fmt.Errorf("q-signature does not authenticate the pinned request")
	}
	return nil
}

func parseDifferentialTencentCLSAuthorization(authorization string) (map[string][]string, error) {
	values := make(map[string][]string)
	for field := range strings.SplitSeq(authorization, "&") {
		name, value, ok := strings.Cut(field, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("malformed authorization field %q", field)
		}
		decodedName, err := url.QueryUnescape(name)
		if err != nil {
			return nil, fmt.Errorf("decode authorization field name %q: %w", name, err)
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			return nil, fmt.Errorf("decode authorization field %q: %w", decodedName, err)
		}
		values[decodedName] = append(values[decodedName], decodedValue)
	}
	return values, nil
}

func differentialTencentCLSSignature(secretKey, signTime string) string {
	httpRequestInfo := "post\n/structuredlog\n\n\n"
	requestDigest := sha1.Sum([]byte(httpRequestInfo))
	stringToSign := "sha1\n" + signTime + "\n" + hex.EncodeToString(requestDigest[:]) + "\n"
	signKeyHMAC := hmac.New(sha1.New, []byte(secretKey))
	_, _ = signKeyHMAC.Write([]byte(signTime))
	signKey := hex.EncodeToString(signKeyHMAC.Sum(nil))
	signatureHMAC := hmac.New(sha1.New, []byte(signKey))
	_, _ = signatureHMAC.Write([]byte(stringToSign))
	return hex.EncodeToString(signatureHMAC.Sum(nil))
}
