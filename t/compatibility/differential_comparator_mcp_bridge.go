package pluginintegration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
)

const differentialMCPBridgePostedPayload = `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`

var differentialMCPBridgeSessionIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`,
)

func init() {
	differentialComparatorRegistry[differentialMCPBridgeSSESessionPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"mcp-bridge": {}},
		compare:        compareDifferentialMCPBridgeSSESession,
	}
}

func compareDifferentialMCPBridgeSSESession(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if err := validateDifferentialMCPBridgeDriverSpec(spec); err != nil {
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
		if err := normalizeDifferentialMCPBridgeObservation(spec, side.name, side.observation); err != nil {
			return false, "", err
		}
	}
	return compareNormalizedObservations(left, right, policy)
}

func normalizeDifferentialMCPBridgeObservation(
	spec DifferentialCase,
	side string,
	observation *DifferentialObservation,
) error {
	if observation.Status != http.StatusOK || observation.Host != spec.Request.Host ||
		observation.SNI != spec.Request.SNI || observation.SecurityDecision != spec.SecurityDecision {
		return fmt.Errorf("%s MCP SSE response identity is invalid", side)
	}
	if observation.RetryCount != 0 || len(observation.RouteObserver) != 0 ||
		observation.Upstream.Received || observation.UpstreamFixture != "" ||
		observation.UpstreamAddress != "" || len(observation.UpstreamCalls) != 0 ||
		observation.File != nil {
		return fmt.Errorf("%s MCP session unexpectedly reached or captured an upstream", side)
	}
	if contentTypes := differentialHeaderValues(
		observation.Headers,
		"Content-Type",
	); len(contentTypes) != 1 ||
		!strings.HasPrefix(strings.ToLower(contentTypes[0]), "text/event-stream") {
		return fmt.Errorf("%s MCP SSE Content-Type = %q, want text/event-stream", side, contentTypes)
	}
	if cacheControl := differentialHeaderValues(
		observation.Headers,
		"Cache-Control",
	); len(cacheControl) != 1 ||
		!headerValueContainsToken(cacheControl[0], "no-cache") {
		return fmt.Errorf("%s MCP SSE Cache-Control = %q, want no-cache", side, cacheControl)
	}
	if len(observation.Steps) != 1 {
		return fmt.Errorf("%s MCP message response count = %d, want 1", side, len(observation.Steps))
	}
	post := &observation.Steps[0]
	if post.Status != http.StatusAccepted || post.Body != "" || post.Host != spec.Request.Host ||
		post.SNI != spec.Request.SNI || post.SecurityDecision != spec.SecurityDecision {
		return fmt.Errorf("%s MCP dynamic session POST response is invalid", side)
	}

	transcript, err := decodeDifferentialMCPBridgeTranscript(observation.Body)
	if err != nil {
		return fmt.Errorf("%s MCP transcript: %w", side, err)
	}
	if transcript.Endpoint.Event != "endpoint" {
		return fmt.Errorf("%s MCP endpoint event = %q, want endpoint", side, transcript.Endpoint.Event)
	}
	endpoint, err := url.Parse(transcript.Endpoint.Data)
	if err != nil {
		return fmt.Errorf("%s MCP endpoint URL: %w", side, err)
	}
	if endpoint.IsAbs() || endpoint.Host != "" || endpoint.Path != "/mcp/message" ||
		endpoint.Fragment != "" || len(endpoint.Query()) != 1 {
		return fmt.Errorf(
			"%s MCP endpoint = %q, want the relative /mcp/message session endpoint",
			side,
			transcript.Endpoint.Data,
		)
	}
	sessionIDs := endpoint.Query()["sessionId"]
	if len(sessionIDs) != 1 || !differentialMCPBridgeSessionIDPattern.MatchString(sessionIDs[0]) {
		return fmt.Errorf("%s MCP endpoint sessionId is not a UUID v4", side)
	}
	if transcript.Ping.Event != "message" {
		return fmt.Errorf("%s MCP ping event = %q, want message", side, transcript.Ping.Event)
	}
	if err := validateDifferentialMCPBridgePing(transcript.Ping.Data); err != nil {
		return fmt.Errorf("%s MCP ping: %w", side, err)
	}
	if transcript.Message.Event != "message" {
		return fmt.Errorf("%s MCP process event = %q, want message", side, transcript.Message.Event)
	}
	if err := compareDifferentialMCPBridgeJSON(
		transcript.Message.Data,
		differentialMCPBridgePostedPayload,
	); err != nil {
		return fmt.Errorf("%s MCP echoed process message: %w", side, err)
	}

	canonical, err := json.Marshal(differentialMCPBridgeTranscript{
		Endpoint: differentialMCPBridgeSSEEvent{
			Event: "endpoint", Data: "/mcp/message?sessionId=<session>",
		},
		Ping: differentialMCPBridgeSSEEvent{
			Event: "message",
			Data:  `{"jsonrpc":"2.0","method":"ping","id":"ping:1"}`,
		},
		Message: differentialMCPBridgeSSEEvent{
			Event: "message", Data: differentialMCPBridgePostedPayload,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal canonical MCP transcript: %w", err)
	}
	observation.Body = string(canonical)
	observation.Headers = map[string][]string{
		"cache-control": {"no-cache"},
		"content-type":  {"text/event-stream"},
	}
	post.Headers = nil
	return nil
}

func validateDifferentialMCPBridgePing(raw string) error {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("contains trailing JSON")
	}
	if len(fields) != 3 {
		return fmt.Errorf("field count = %d, want 3", len(fields))
	}
	want := map[string]string{"jsonrpc": "2.0", "method": "ping", "id": "ping:1"}
	for name, wantValue := range want {
		var value string
		if err := json.Unmarshal(fields[name], &value); err != nil || value != wantValue {
			return fmt.Errorf("%s = %q, want %q", name, value, wantValue)
		}
	}
	return nil
}

func compareDifferentialMCPBridgeJSON(got string, want string) error {
	decode := func(raw string) (any, error) {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return nil, fmt.Errorf("contains trailing JSON")
		}
		return value, nil
	}
	gotValue, err := decode(got)
	if err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	wantValue, err := decode(want)
	if err != nil {
		return fmt.Errorf("decode expected JSON: %w", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		return fmt.Errorf("JSON payload does not match the posted request")
	}
	return nil
}

func headerValueContainsToken(value string, want string) bool {
	for token := range strings.SplitSeq(value, ",") {
		if strings.EqualFold(strings.TrimSpace(token), want) {
			return true
		}
	}
	return false
}
