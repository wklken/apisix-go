package loki_logger

import (
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestEmptyRespBodyLabelResolvesToEmpty(t *testing.T) {
	p := &Plugin{config: Config{LogLabels: map[string]string{"body": "$resp_body"}}}
	snapshot := base.LogSnapshot{Response: apisixlog.ResponseLogSnapshot{Body: nil}}
	if got := lokiSnapshotLabels(p, snapshot)["body"]; got != "" {
		t.Fatalf("$resp_body label = %q, APISIX ctx.var.resp_body always resolves and is empty when uncaptured", got)
	}
}
