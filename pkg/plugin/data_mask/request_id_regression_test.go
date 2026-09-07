package data_mask

import (
	"net/http"
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestDataMaskTracksAPISIXRequestID(t *testing.T) {
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
		ID:          "client-secret",
		Header:      http.Header{"X-Request-Id": {"client-secret"}},
		APISIXVars:  map[string]any{"$apisix_request_id": "client-secret"},
		RequestVars: map[string]any{"$request_id": "0123456789abcdef0123456789abcdef"},
	}}
	maskSnapshotHeader(&snapshot, MaskRule{Name: "X-Request-Id", Action: "replace", Value: "[masked]"})
	t.Logf(
		"header=%q id=%q apisix_request_id=%q request_id=%q",
		snapshot.Request.Header.Get("X-Request-Id"),
		snapshot.Request.ID,
		snapshot.Request.APISIXVars["$apisix_request_id"],
		snapshot.Request.RequestVars["$request_id"],
	)
	if got := snapshot.Request.APISIXVars["$apisix_request_id"]; got != "[masked]" {
		t.Fatalf("sanitized snapshot retains apisix_request_id=%q", got)
	}
	if got := snapshot.Request.RequestVars["$request_id"]; got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("native request_id changed to %q", got)
	}
}
