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
	snapshot := BuildLogSnapshot(request, ResponseCaptureSnapshot{
		Header: http.Header{"X-Test": {"one"}}, Body: []byte("response-body"), BodyTruncated: true,
	}, ctx.ResponseOutcome{Status: http.StatusOK}, ctx.ResponseSourceUpstream, time.Time{}, time.Time{})
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
