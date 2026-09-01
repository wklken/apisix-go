package base

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestValidateLogCapturePolicyPreservesZeroAndHardBounds(t *testing.T) {
	if err := ValidateLogCapturePolicy(LogCapturePolicy{}); err != nil {
		t.Fatalf("zero policy error = %v", err)
	}
	if err := ValidateLogCapturePolicy(LogCapturePolicy{
		RequestBodyBytes: MAX_REQ_BODY, ResponseBodyBytes: MAX_RESP_BODY,
	}); err != nil {
		t.Fatalf("hard-bound policy error = %v", err)
	}
	for _, policy := range []LogCapturePolicy{
		{RequestBodyBytes: -1},
		{ResponseBodyBytes: -1},
		{RequestBodyBytes: MAX_REQ_BODY + 1},
		{ResponseBodyBytes: MAX_RESP_BODY + 1},
	} {
		if err := ValidateLogCapturePolicy(policy); err == nil {
			t.Fatalf("ValidateLogCapturePolicy(%#v) = nil", policy)
		}
	}
}

func TestCloneLogSnapshotForPolicyIsFreshAndBounded(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://gateway.test/", strings.NewReader("request-body"))
	snapshot := BuildLogSnapshotFromOwnedInputs(
		request,
		ResponseCaptureSnapshot{
			Header: http.Header{"X-Test": {"one"}}, Body: []byte("response-body"), BodyTruncated: true,
		},
		[]byte("request-body"),
		false,
		ctx.ResponseOutcome{Status: http.StatusOK},
		ctx.ResponseSourceUpstream,
		time.Time{},
		time.Time{},
	)
	clone := CloneLogSnapshotForPolicy(snapshot, LogCapturePolicy{RequestBodyBytes: 4, ResponseBodyBytes: 5})
	if string(clone.Request.Body) != "requ" || !clone.Request.BodyTruncated {
		t.Fatalf("request body = %q/%t", clone.Request.Body, clone.Request.BodyTruncated)
	}
	if string(clone.Response.Body) != "respo" || !clone.Response.BodyTruncated {
		t.Fatalf("response body = %q/%t", clone.Response.Body, clone.Response.BodyTruncated)
	}
	clone.Response.Header.Set("X-Test", "mutated")
	clone.Response.Body[0] = 'X'
	if snapshot.Response.Header.Get("X-Test") != "one" || string(snapshot.Response.Body) != "response-body" {
		t.Fatal("policy clone aliases source snapshot")
	}
	zero := CloneLogSnapshotForPolicy(snapshot, LogCapturePolicy{})
	if len(zero.Request.Body) != 0 || len(zero.Response.Body) != 0 {
		t.Fatalf("zero policy retained body: request=%q response=%q", zero.Request.Body, zero.Response.Body)
	}
}

func TestCloneLogSnapshotForPolicyKeepsEveryMutableViewPrivate(t *testing.T) {
	snapshot := LogSnapshot{
		Request:  LogSnapshot{}.Request,
		Response: LogSnapshot{}.Response,
	}
	snapshot.Request.Header = http.Header{"X-Request": {"one"}}
	snapshot.Request.Query = map[string][]string{"q": {"one"}}
	snapshot.Request.Body = []byte("request-body")
	snapshot.Request.APISIXVars = map[string]any{
		"$nested": map[string]any{"key": "one"},
	}
	snapshot.Request.RequestVars = map[string]any{
		"$list": []any{"one"},
	}
	snapshot.Response.Header = http.Header{"X-Response": {"one"}}
	snapshot.Response.Trailer = http.Header{"X-Trailer": {"one"}}
	snapshot.Response.Body = []byte("response-body")

	clone := CloneLogSnapshotForPolicy(snapshot, LogCapturePolicy{
		RequestBodyBytes: 4, ResponseBodyBytes: 5,
	})
	clone.Request.Header.Set("X-Request", "two")
	clone.Request.Query.Set("q", "two")
	clone.Request.Body[0] = 'R'
	clone.Request.APISIXVars["$nested"].(map[string]any)["key"] = "two"
	clone.Request.RequestVars["$list"].([]any)[0] = "two"
	clone.Response.Header.Set("X-Response", "two")
	clone.Response.Trailer.Set("X-Trailer", "two")
	clone.Response.Body[0] = 'R'

	if snapshot.Request.Header.Get("X-Request") != "one" || snapshot.Request.Query.Get("q") != "one" ||
		string(snapshot.Request.Body) != "request-body" ||
		snapshot.Request.APISIXVars["$nested"].(map[string]any)["key"] != "one" ||
		snapshot.Request.RequestVars["$list"].([]any)[0] != "one" ||
		snapshot.Response.Header.Get("X-Response") != "one" ||
		snapshot.Response.Trailer.Get("X-Trailer") != "one" ||
		string(snapshot.Response.Body) != "response-body" {
		t.Fatalf("policy clone aliases source snapshot: %#v", snapshot)
	}
	if got := string(clone.Request.Body); got != "Requ" || !clone.Request.BodyTruncated {
		t.Fatalf("request body after isolated mutation = %q/truncated=%t", got, clone.Request.BodyTruncated)
	}
	if got := string(clone.Response.Body); got != "Respo" || !clone.Response.BodyTruncated {
		t.Fatalf("response body after isolated mutation = %q/truncated=%t", got, clone.Response.BodyTruncated)
	}
}

func TestBuildLogSnapshotFromOwnedInputsKeepsResponseCaptureDetached(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	request, _ = ctx.EnsureRequestLifecycle(request, time.Now())
	recorder := httptest.NewRecorder()
	writer, capture := CaptureResponseOutcomeController(recorder)
	if err := capture.EnableBodyCapture(32); err != nil {
		t.Fatalf("EnableBodyCapture() error = %v", err)
	}
	writer.Header().Set("X-Trace", "one")
	if _, err := writer.Write([]byte("response")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	detached := capture.Snapshot()
	snapshot := BuildLogSnapshotFromOwnedInputs(
		request,
		detached,
		[]byte("request"),
		false,
		ctx.ResponseOutcome{Status: http.StatusOK},
		ctx.ResponseSourceUpstream,
		time.Time{},
		time.Time{},
	)
	if err := capture.EnableBodyCapture(0); err != nil {
		t.Fatalf("reset body capture error = %v", err)
	}
	recorder.Header().Set("X-Trace", "mutated")
	if snapshot.Response.Header.Get("X-Trace") != "one" || string(snapshot.Response.Body) != "response" {
		t.Fatalf("owned response snapshot changed after capture mutation: %#v", snapshot.Response)
	}
}
