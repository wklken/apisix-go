package google_cloud_logging

import (
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestParityGoogleDefaultServerIPMatchesAPISIX317(t *testing.T) {
	fields := googleSnapshotDefaultLogFields(base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Host: "gateway.example",
			APISIXVars: map[string]any{
				"$balancer_ip":   "192.0.2.20",
				"$balancer_port": "8080",
			},
		},
	})
	entry := (&Plugin{config: Config{Resource: MonitoredResource{Type: "global"}}}).
		buildEntryForProject(fields, "project")
	if got := entry.HTTPRequest.ServerIP; got != "192.0.2.20:8080" {
		t.Fatalf("serverIp = %q, want APISIX 3.17 upstream %q", got, "192.0.2.20:8080")
	}
}
