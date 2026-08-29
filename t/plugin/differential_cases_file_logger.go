package pluginintegration

import (
	"encoding/json"
	"fmt"
	"net/http"
)

const differentialFileLoggerJSONLPolicy = "file-logger-jsonl-write"

func init() {
	differentialComparatorRegistry[differentialFileLoggerJSONLPolicy] = differentialComparatorRegistration{
		allowedPlugins: map[string]struct{}{"file-logger": {}},
		compare:        compareDifferentialFileLoggerJSONL,
	}
}

func differentialFileLoggerCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:             "file-logger-route-format-jsonl-write",
		Plugin:           "file-logger",
		RouteID:          "differential-file-logger-route-format",
		ComparisonPolicy: differentialFileLoggerJSONLPolicy,
		Config: map[string]any{"routes": []any{map[string]any{
			"id": "differential-file-logger-route-format", "uri": "/hello",
			"plugins": map[string]any{"file-logger": map[string]any{
				"path": differentialSideFilePlaceholder,
				"log_format": map[string]any{
					"host": "$host", "client_ip": "$remote_addr",
				},
			}},
			"upstream": differentialUpstream(),
		}}},
		Request: DifferentialRequest{Method: http.MethodGet, Path: "/hello", Host: "127.0.0.1"},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "hello world\n"},
		},
		File: &DifferentialFileCapture{
			Name: "access.log", MaxBytes: 16 << 10, WaitTimeoutMillis: 2000, ExpectedLines: 1,
		},
		SecurityDecision: "not_applicable",
	}}
}

func compareDifferentialFileLoggerJSONL(
	spec DifferentialCase,
	left DifferentialObservation,
	right DifferentialObservation,
	policy NormalizationPolicy,
) (bool, string, error) {
	if spec.Name != "file-logger-route-format-jsonl-write" || spec.Plugin != "file-logger" ||
		spec.RouteID != "differential-file-logger-route-format" || spec.File == nil ||
		spec.File.Name != "access.log" || spec.File.ExpectedLines != 1 {
		return false, "", fmt.Errorf(
			"comparison policy %q requires the pinned file-logger route-format write case",
			spec.ComparisonPolicy,
		)
	}
	leftFile, rightFile := left.File, right.File
	left.File, right.File = nil, nil
	passed, detail, err := compareNormalizedObservations(left, right, policy)
	if err != nil || !passed {
		return passed, detail, err
	}
	leftEntry, err := validateDifferentialFileLoggerObservation("candidate", leftFile, true)
	if err != nil {
		return false, "", err
	}
	rightEntry, err := validateDifferentialFileLoggerObservation("oracle", rightFile, false)
	if err != nil {
		return false, "", err
	}
	leftJSON, _ := json.Marshal(leftEntry)
	rightJSON, _ := json.Marshal(rightEntry)
	wantEntry := map[string]any{
		"host": "127.0.0.1", "client_ip": "127.0.0.1",
		"route_id": "differential-file-logger-route-format",
	}
	wantJSON, _ := json.Marshal(wantEntry)
	if string(leftJSON) != string(wantJSON) || string(rightJSON) != string(wantJSON) {
		return false, fmt.Sprintf("candidate_file=%s oracle_file=%s", leftJSON, rightJSON), nil
	}
	return true, "", nil
}

func validateDifferentialFileLoggerObservation(
	side string,
	observation *DifferentialFileObservation,
	wantGoEnvelope bool,
) (map[string]any, error) {
	if observation == nil || observation.Name != "access.log" || !observation.Exists {
		return nil, fmt.Errorf("file-logger %s observation is missing access.log", side)
	}
	if observation.Truncated || observation.Size != int64(len(observation.Content)) {
		return nil, fmt.Errorf("file-logger %s observation is incomplete", side)
	}
	entry, err := decodeDifferentialJSONLine(observation.Content)
	if err != nil {
		return nil, fmt.Errorf("file-logger %s: %w", side, err)
	}
	if wantGoEnvelope {
		if entry["level"] != "info" || entry["msg"] != "" {
			return nil, fmt.Errorf("file-logger candidate envelope is invalid")
		}
		if _, ok := entry["ts"].(json.Number); !ok {
			return nil, fmt.Errorf("file-logger candidate timestamp is invalid")
		}
		delete(entry, "level")
		delete(entry, "msg")
		delete(entry, "ts")
	} else if entry["level"] != nil || entry["msg"] != nil || entry["ts"] != nil {
		return nil, fmt.Errorf("file-logger oracle unexpectedly contains Go logger envelope")
	}
	wantFields := []string{"host", "client_ip", "route_id"}
	if len(entry) != len(wantFields) {
		return nil, fmt.Errorf("file-logger %s fields = %#v, want exactly host, client_ip, route_id", side, entry)
	}
	for _, name := range wantFields {
		if _, ok := entry[name].(string); !ok {
			return nil, fmt.Errorf("file-logger %s %s = %#v, want string", side, name, entry[name])
		}
	}
	return entry, nil
}
