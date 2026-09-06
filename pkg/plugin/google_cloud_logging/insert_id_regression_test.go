package google_cloud_logging

import (
	"net/http"
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestInsertIDUsesRequestIDVariable(t *testing.T) {
	fields := googleSnapshotDefaultLogFields(base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Header: http.Header{"X-Request-ID": {"client-header"}},
			APISIXVars: map[string]any{
				"$request_id": "ngx-request-id",
			},
		},
	})
	entry := (&Plugin{config: Config{Resource: MonitoredResource{Type: "global"}, LogID: defaultLogID}}).
		buildEntryForProject(fields, "project")
	if entry.InsertID != "ngx-request-id" {
		t.Fatalf("insertId=%q, want APISIX $request_id", entry.InsertID)
	}
}
