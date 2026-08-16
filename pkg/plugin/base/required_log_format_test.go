package base

import (
	"strings"
	"testing"
)

func TestRequireStringLogFormatRoutePrecedesMetadataAndClones(t *testing.T) {
	route := map[string]string{"route": "$request_id"}
	metadata := map[string]string{"metadata": "$route_id"}

	got, err := RequireStringLogFormat("example-logger", route, metadata)
	if err != nil {
		t.Fatalf("RequireStringLogFormat() error = %v", err)
	}
	if got["route"] != route["route"] || len(got) != len(route) {
		t.Fatalf("effective format = %#v, want route format %#v", got, route)
	}
	got["route"] = "mutated"
	if route["route"] == "mutated" {
		t.Fatal("route format was not cloned")
	}
}

func TestRequireStringLogFormatFallsBackToMetadataAndClones(t *testing.T) {
	metadata := map[string]string{"route": "$route_id"}

	got, err := RequireStringLogFormat("example-logger", nil, metadata)
	if err != nil {
		t.Fatalf("RequireStringLogFormat() error = %v", err)
	}
	got["route"] = "mutated"
	if metadata["route"] == "mutated" {
		t.Fatal("metadata format was not cloned")
	}
}

func TestRequireStringLogFormatRejectsEmptyFormats(t *testing.T) {
	_, err := RequireStringLogFormat("example-logger", nil, nil)
	if err == nil {
		t.Fatal("RequireStringLogFormat() error = nil, want empty-format rejection")
	}
	if !strings.Contains(err.Error(), "example-logger") || !strings.Contains(err.Error(), "log_format") {
		t.Fatalf("RequireStringLogFormat() error = %q, want plugin name and log_format", err)
	}
}
