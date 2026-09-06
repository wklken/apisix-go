package loki_logger

import (
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestMixedLabelTemplatesResolveVariables(t *testing.T) {
	p := &Plugin{config: Config{
		LogLabels: map[string]string{
			"host":   "$remote_addr",
			"mixed":  "gw-$remote_addr",
			"braced": "gw-${remote_addr}",
		},
	}}
	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			APISIXVars: map[string]any{"$remote_addr": "192.0.2.10"},
		},
	}
	snapshot.Request.RemoteAddr = "192.0.2.10:1234"
	labels := lokiSnapshotLabels(p, snapshot)
	if labels["host"] != "192.0.2.10" {
		t.Fatal("full $remote_addr label was not resolved")
	}
	if labels["mixed"] != "gw-192.0.2.10" {
		t.Fatalf("mixed=%q, want resolved mixed template", labels["mixed"])
	}
	if labels["braced"] != "gw-192.0.2.10" {
		t.Fatalf("braced=%q, want resolved braced template", labels["braced"])
	}
}
