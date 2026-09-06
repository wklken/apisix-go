package loki_logger

import (
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestMixedLabelUsesRequestVariableInsteadOfCustomLogField(t *testing.T) {
	body := captureRunLogPayload(t, Config{
		LogLabels: map[string]string{"code": "prefix-$status"},
		LogFormat: map[string]string{"status": "custom-log-field"},
	}, nil, base.LogSnapshot{Outcome: apisixctx.ResponseOutcome{Status: 201}})
	labels := requiredObject(t, extractLokiStream(t, body, 0), "stream")
	if got := labels["code"]; got != "prefix-201" {
		t.Fatalf("label=%v", got)
	}
}
