package ai_common

import (
	"fmt"
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
)

// SafetyFailMode controls how an AI safety check handles an operational
// failure or an uninspectable request/response.
type SafetyFailMode string

const (
	SafetyFailError SafetyFailMode = "error"
	SafetyFailWarn  SafetyFailMode = "warn"
	SafetyFailSkip  SafetyFailMode = "skip"
)

// ParseSafetyFailMode parses the configured safety failure mode. An empty
// value uses the fail-closed default.
func ParseSafetyFailMode(raw string) (SafetyFailMode, error) {
	switch mode := SafetyFailMode(raw); mode {
	case "":
		return SafetyFailError, nil
	case SafetyFailError, SafetyFailWarn, SafetyFailSkip:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid safety fail mode %q", raw)
	}
}

// SafetyFailureClass classifies why an AI safety check could not complete.
type SafetyFailureClass string

const (
	SafetyInvalidPayload          SafetyFailureClass = "invalid_payload"
	SafetyUnknownProtocol         SafetyFailureClass = "unknown_protocol"
	SafetyEmptyContent            SafetyFailureClass = "empty_content"
	SafetyBackendUnavailable      SafetyFailureClass = "backend_unavailable"
	SafetyBackendInvalidResponse  SafetyFailureClass = "backend_invalid_response"
	SafetyUpstreamInvalidResponse SafetyFailureClass = "upstream_invalid_response"
)

// SafetyPhase identifies whether the safety check ran on a request or a
// response.
type SafetyPhase string

const (
	SafetyPhaseRequest  SafetyPhase = "request"
	SafetyPhaseResponse SafetyPhase = "response"
)

// SafetyOutcome is the externally observable outcome of a safety check.
type SafetyOutcome string

const (
	SafetyOutcomeAllow    SafetyOutcome = "allow"
	SafetyOutcomeDeny     SafetyOutcome = "deny"
	SafetyOutcomeDegraded SafetyOutcome = "degraded"
	SafetyOutcomeError    SafetyOutcome = "error"
)

// SafetyReason is a bounded policy reason used by safety metrics and logs.
type SafetyReason string

const (
	SafetyReasonClean            SafetyReason = "clean"
	SafetyReasonAllowPatternMiss SafetyReason = "allow_pattern_miss"
	SafetyReasonDenyPatternMatch SafetyReason = "deny_pattern_match"
	SafetyReasonRiskThreshold    SafetyReason = "risk_threshold"
)

// SafetyAction determines whether the owning plugin should continue the
// request chain or reject the request.
type SafetyAction string

const (
	SafetyReject   SafetyAction = "reject"
	SafetyContinue SafetyAction = "continue"
)

// SafetyDecision is the pure result of applying a fail mode to a failure
// class. The owner remains responsible for writing the protocol-specific
// response and deciding whether to call the next handler.
type SafetyDecision struct {
	Action  SafetyAction
	Status  int
	Outcome SafetyOutcome
}

// DecideSafetyFailure maps a safety failure to the owning plugin's control
// flow. Explicit warn/skip modes always continue in a degraded state. Unknown
// classes fail closed as invalid request content.
func DecideSafetyFailure(mode SafetyFailMode, class SafetyFailureClass) SafetyDecision {
	if mode == SafetyFailWarn || mode == SafetyFailSkip {
		return SafetyDecision{Action: SafetyContinue, Outcome: SafetyOutcomeDegraded}
	}

	status := http.StatusBadRequest
	switch class {
	case SafetyBackendUnavailable, SafetyBackendInvalidResponse:
		status = http.StatusServiceUnavailable
	case SafetyUpstreamInvalidResponse:
		status = http.StatusBadGateway
	case SafetyInvalidPayload, SafetyUnknownProtocol, SafetyEmptyContent:
		// Request-content failures remain a client error.
	default:
		// Keep an unknown class fail closed rather than silently allowing it.
	}
	return SafetyDecision{Action: SafetyReject, Status: status, Outcome: SafetyOutcomeError}
}

// LogSafetyDegradation emits only bounded safety fields. The request is used
// solely to resolve a route ID; payloads, backend responses, and error text
// are intentionally never accepted or logged here.
func LogSafetyDegradation(
	r *http.Request,
	plugin string,
	mode SafetyFailMode,
	phase SafetyPhase,
	class SafetyFailureClass,
) {
	if mode != SafetyFailWarn && mode != SafetyFailSkip {
		return
	}
	if !validSafetyPhase(phase) || !validSafetyFailureClass(class) {
		return
	}

	routeID := safetyRouteID(r)
	message := fmt.Sprintf(
		"ai safety degradation plugin=%s mode=%s phase=%s reason=%s route_id=%s outcome=%s",
		plugin,
		mode,
		phase,
		class,
		routeID,
		SafetyOutcomeDegraded,
	)
	if mode == SafetyFailWarn {
		logger.Warn(message)
		return
	}
	logger.Info(message)
}

func safetyRouteID(r *http.Request) string {
	if r == nil {
		return ""
	}
	if value := apisixctx.GetRequestVar(r, "$route_id"); value != nil {
		if routeID, ok := value.(string); ok && routeID != "" {
			return routeID
		}
	}
	if value := apisixctx.GetApisixVar(r, "$route_id"); value != nil {
		if routeID, ok := value.(string); ok {
			return routeID
		}
	}
	return ""
}

func validSafetyPhase(phase SafetyPhase) bool {
	return phase == SafetyPhaseRequest || phase == SafetyPhaseResponse
}

func validSafetyFailureClass(class SafetyFailureClass) bool {
	switch class {
	case SafetyInvalidPayload, SafetyUnknownProtocol, SafetyEmptyContent,
		SafetyBackendUnavailable, SafetyBackendInvalidResponse, SafetyUpstreamInvalidResponse:
		return true
	default:
		return false
	}
}
