package loki_logger

import (
	"net/http"
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestPresentEmptyLabelVariableStillResolves(t *testing.T) {
	p := &Plugin{config: Config{LogLabels: map[string]string{"empty": "prefix-$http_x_empty"}}}
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{Header: http.Header{"X-Empty": {""}}}}
	if got := lokiSnapshotLabels(p, snapshot)["empty"]; got != "prefix-" {
		t.Fatalf("present empty variable label = %q, APISIX counts the variable as resolved", got)
	}
}
