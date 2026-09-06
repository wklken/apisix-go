package google_cloud_logging

import (
	"regexp"
	"testing"
)

func TestTimestampUsesMillisecondZuluPrecision(t *testing.T) {
	entry := (&Plugin{config: Config{Resource: MonitoredResource{Type: "global"}, LogID: defaultLogID}}).buildEntryForProject(
		map[string]any{"path": "/x"},
		"project",
	)
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`).MatchString(entry.Timestamp) {
		t.Fatalf("timestamp = %q", entry.Timestamp)
	}
}
