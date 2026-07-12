package server_info

import (
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
)

func TestReportTTLReadsAndBoundsPluginAttribute(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })

	config.GlobalConfig = &config.Config{
		PluginAttr: map[string]map[string]interface{}{
			"server-info": {"report_ttl": 45},
		},
	}
	if got := ReportTTL(); got != 45*time.Second {
		t.Fatalf("ReportTTL() = %s, want 45s", got)
	}

	config.GlobalConfig.PluginAttr["server-info"]["report_ttl"] = 1
	if got := ReportTTL(); got != 3*time.Second {
		t.Fatalf("ReportTTL() below minimum = %s, want 3s", got)
	}

	config.GlobalConfig.PluginAttr["server-info"]["report_ttl"] = 90000
	if got := ReportTTL(); got != 86400*time.Second {
		t.Fatalf("ReportTTL() above maximum = %s, want 86400s", got)
	}
}
