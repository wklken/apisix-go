package loggly

import (
	"regexp"
	"strings"
	"testing"
)

func TestTimestampUsesMillisecondZuluPrecision(t *testing.T) {
	message := (&Plugin{config: Config{Severity: "INFO"}}).buildMessage(map[string]any{"path": "/x"}, "token")
	timestamp := strings.Fields(message)[1]
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`).MatchString(timestamp) {
		t.Fatalf("timestamp = %q", timestamp)
	}
}
