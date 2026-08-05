package logger

import (
	"testing"

	"go.uber.org/zap/zapcore"
)

func TestParseAPISIXLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  zapcore.Level
	}{
		{input: "", want: zapcore.InfoLevel},
		{input: "debug", want: zapcore.DebugLevel},
		{input: "INFO", want: zapcore.InfoLevel},
		{input: "notice", want: zapcore.InfoLevel},
		{input: "warn", want: zapcore.WarnLevel},
		{input: "error", want: zapcore.ErrorLevel},
		{input: "crit", want: zapcore.ErrorLevel},
		{input: "alert", want: zapcore.ErrorLevel},
		{input: "emerg", want: zapcore.ErrorLevel},
	}
	for _, test := range tests {
		got, err := parseAPISIXLogLevel(test.input)
		if err != nil || got != test.want {
			t.Errorf("parseAPISIXLogLevel(%q) = %v, %v; want %v", test.input, got, err, test.want)
		}
	}
	if _, err := parseAPISIXLogLevel("verbose"); err == nil {
		t.Fatal("parseAPISIXLogLevel(verbose) error = nil")
	}
}

func TestConfigureLevelControlsDebugGate(t *testing.T) {
	t.Cleanup(func() { _ = ConfigureLevel("info") })
	if err := ConfigureLevel("debug"); err != nil || !DebugEnabled() {
		t.Fatalf("debug configuration error = %v, enabled = %v", err, DebugEnabled())
	}
	if err := ConfigureLevel("warn"); err != nil || DebugEnabled() {
		t.Fatalf("warn configuration error = %v, debug enabled = %v", err, DebugEnabled())
	}
}
