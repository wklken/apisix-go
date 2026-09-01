package base

import (
	"fmt"
	"net/http"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

// LogSnapshot is the plugin-facing alias for the detached canonical snapshot.
type LogSnapshot = apisixlog.LogSnapshot

type LogCapturePolicy struct {
	RequestBodyBytes  int
	ResponseBodyBytes int
}

type LogCapturePolicyPlugin interface {
	LogCapturePolicy() LogCapturePolicy
}

type LogPhasePlugin interface {
	RunLogPhase(LogSnapshot) error
}

type SnapshotFinalizerPlugin interface {
	RunSnapshotFinalizer(LogSnapshot) error
}

// LogSnapshotSanitizerPlugin mutates only the detached canonical logging
// snapshot. The log executor runs sanitizers before cloning the snapshot for
// any logger or snapshot finalizer callback.
type LogSnapshotSanitizerPlugin interface {
	SanitizeLogSnapshot(*LogSnapshot) error
}

// LogSnapshotSanitizerSelectorPlugin optionally restricts a sanitizer to a
// detached snapshot. The log executor evaluates every selector against the
// same pre-sanitized snapshot before running any sanitizer callback.
type LogSnapshotSanitizerSelectorPlugin interface {
	ShouldSanitizeLogSnapshot(LogSnapshot) bool
}

// GetFieldsFromSnapshot keeps field expansion in the detached snapshot layer
// while giving plugin packages a base-level entry point.
func GetFieldsFromSnapshot(snapshot LogSnapshot, logFormat map[string]string) map[string]any {
	return apisixlog.GetFieldsFromSnapshot(snapshot, logFormat)
}

func LogSnapshotValue(snapshot LogSnapshot, name string) any {
	return apisixlog.ValueFromSnapshot(snapshot, name)
}

// ValidateLogCapturePolicy enforces the existing hard body ceilings. Zero is
// intentional and means that the corresponding body is not captured.
func ValidateLogCapturePolicy(policy LogCapturePolicy) error {
	if policy.RequestBodyBytes < 0 || policy.RequestBodyBytes > MAX_REQ_BODY {
		return fmt.Errorf(
			"request body capture must be between 0 and %d bytes: %d",
			MAX_REQ_BODY,
			policy.RequestBodyBytes,
		)
	}
	if policy.ResponseBodyBytes < 0 || policy.ResponseBodyBytes > MAX_RESP_BODY {
		return fmt.Errorf(
			"response body capture must be between 0 and %d bytes: %d",
			MAX_RESP_BODY,
			policy.ResponseBodyBytes,
		)
	}
	return nil
}

// BuildLogSnapshotFromOwnedInputs transfers detached response capture and a
// previously captured request body into a canonical snapshot. The request is
// inspected only for metadata; its live body is never read.
func BuildLogSnapshotFromOwnedInputs(
	r *http.Request,
	response ResponseCaptureSnapshot,
	requestBody []byte,
	requestBodyTruncated bool,
	outcome apisixctx.ResponseOutcome,
	source apisixctx.ResponseSource,
	started,
	finished time.Time,
) LogSnapshot {
	return apisixlog.BuildSnapshotFromOwnedInputs(
		r,
		apisixlog.ResponseSnapshot{
			Header:        response.Header,
			Trailer:       response.Trailer,
			Body:          response.Body,
			BodyTruncated: response.BodyTruncated,
		},
		requestBody,
		requestBodyTruncated,
		outcome,
		source,
		started,
		finished,
	)
}

// CloneLogSnapshotForPolicy gives one callback a private bounded view. Every
// invocation returns a fresh clone, including when both body limits are zero.
func CloneLogSnapshotForPolicy(snapshot LogSnapshot, policy LogCapturePolicy) LogSnapshot {
	requestBody := snapshot.Request.Body
	responseBody := snapshot.Response.Body
	snapshot.Request.Body = nil
	snapshot.Response.Body = nil
	clone := apisixlog.CloneSnapshot(snapshot)
	clone.Request.Body, clone.Request.BodyTruncated = boundedBody(
		requestBody,
		snapshot.Request.BodyTruncated,
		policy.RequestBodyBytes,
	)
	clone.Response.Body, clone.Response.BodyTruncated = boundedBody(
		responseBody,
		snapshot.Response.BodyTruncated,
		policy.ResponseBodyBytes,
	)
	return clone
}

func boundedBody(body []byte, alreadyTruncated bool, limit int) ([]byte, bool) {
	if limit <= 0 {
		return nil, false
	}
	if len(body) > limit {
		return append([]byte(nil), body[:limit]...), true
	}
	return append([]byte(nil), body...), alreadyTruncated
}
