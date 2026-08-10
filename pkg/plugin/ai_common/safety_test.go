package ai_common

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
)

func TestParseSafetyFailMode(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want SafetyFailMode
		err  bool
	}{
		{name: "default", raw: "", want: SafetyFailError},
		{name: "error", raw: "error", want: SafetyFailError},
		{name: "warn", raw: "warn", want: SafetyFailWarn},
		{name: "skip", raw: "skip", want: SafetyFailSkip},
		{name: "invalid", raw: "log", err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseSafetyFailMode(test.raw)
			if test.err {
				if err == nil {
					t.Fatalf("ParseSafetyFailMode(%q) error = nil", test.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSafetyFailMode(%q) error = %v", test.raw, err)
			}
			if got != test.want {
				t.Fatalf("ParseSafetyFailMode(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

func TestDecideSafetyFailure(t *testing.T) {
	tests := []struct {
		name    string
		mode    SafetyFailMode
		class   SafetyFailureClass
		action  SafetyAction
		status  int
		outcome SafetyOutcome
	}{
		{
			name:    "warn backend",
			mode:    SafetyFailWarn,
			class:   SafetyBackendUnavailable,
			action:  SafetyContinue,
			outcome: SafetyOutcomeDegraded,
		},
		{
			name:    "skip invalid payload",
			mode:    SafetyFailSkip,
			class:   SafetyInvalidPayload,
			action:  SafetyContinue,
			outcome: SafetyOutcomeDegraded,
		},
		{
			name:    "error invalid payload",
			mode:    SafetyFailError,
			class:   SafetyInvalidPayload,
			action:  SafetyReject,
			status:  http.StatusBadRequest,
			outcome: SafetyOutcomeError,
		},
		{
			name:    "error unknown protocol",
			mode:    SafetyFailError,
			class:   SafetyUnknownProtocol,
			action:  SafetyReject,
			status:  http.StatusBadRequest,
			outcome: SafetyOutcomeError,
		},
		{
			name:    "error empty content",
			mode:    SafetyFailError,
			class:   SafetyEmptyContent,
			action:  SafetyReject,
			status:  http.StatusBadRequest,
			outcome: SafetyOutcomeError,
		},
		{
			name:    "error backend unavailable",
			mode:    SafetyFailError,
			class:   SafetyBackendUnavailable,
			action:  SafetyReject,
			status:  http.StatusServiceUnavailable,
			outcome: SafetyOutcomeError,
		},
		{
			name:    "error backend invalid response",
			mode:    SafetyFailError,
			class:   SafetyBackendInvalidResponse,
			action:  SafetyReject,
			status:  http.StatusServiceUnavailable,
			outcome: SafetyOutcomeError,
		},
		{
			name:    "error upstream invalid response",
			mode:    SafetyFailError,
			class:   SafetyUpstreamInvalidResponse,
			action:  SafetyReject,
			status:  http.StatusBadGateway,
			outcome: SafetyOutcomeError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := DecideSafetyFailure(test.mode, test.class)
			if got.Action != test.action || got.Status != test.status || got.Outcome != test.outcome {
				t.Fatalf(
					"DecideSafetyFailure(%q, %q) = %#v, want action=%q status=%d outcome=%q",
					test.mode,
					test.class,
					got,
					test.action,
					test.status,
					test.outcome,
				)
			}
		})
	}
}

func TestLogSafetyDegradation(t *testing.T) {
	previousLevel := logger.DebugEnabled()
	if err := logger.ConfigureLevel("info"); err != nil {
		t.Fatalf("ConfigureLevel(info): %v", err)
	}
	t.Cleanup(func() {
		if previousLevel {
			_ = logger.ConfigureLevel("debug")
		} else {
			_ = logger.ConfigureLevel("info")
		}
	})

	routeRequest := apisixctx.WithApisixVars(
		httptest.NewRequest(http.MethodPost, "/v1/chat", nil),
		map[string]string{"$route_id": "route-safety-1"},
	)

	tests := []struct {
		name  string
		mode  SafetyFailMode
		level string
	}{
		{name: "warn", mode: SafetyFailWarn, level: "WARN"},
		{name: "skip", mode: SafetyFailSkip, level: "INFO"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var entries []logger.Entry
			stop := logger.ReplaceObserver("ai-safety-contract-test-"+test.name, func(entry logger.Entry) {
				entries = append(entries, entry)
			})
			t.Cleanup(stop)

			LogSafetyDegradation(
				routeRequest,
				"ai-prompt-guard",
				test.mode,
				SafetyPhaseRequest,
				SafetyBackendUnavailable,
			)

			if len(entries) != 1 || entries[0].Level != test.level {
				t.Fatalf("entries = %#v, want one %s entry", entries, test.level)
			}
			wantFields := []string{
				"plugin=ai-prompt-guard",
				"mode=" + string(test.mode),
				"phase=request",
				"reason=backend_unavailable",
				"route_id=route-safety-1",
				"outcome=degraded",
			}
			for _, field := range wantFields {
				if !strings.Contains(entries[0].Message, field) {
					t.Errorf("message %q does not contain %q", entries[0].Message, field)
				}
			}
		})
	}
}
