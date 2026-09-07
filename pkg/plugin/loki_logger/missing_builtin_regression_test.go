package loki_logger

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestMissingConsumerLabelVariableStaysLiteral(t *testing.T) {
	p := &Plugin{config: Config{LogLabels: map[string]string{"consumer": "prefix-$consumer_name"}}}
	if got := lokiSnapshotLabels(p, base.LogSnapshot{})["consumer"]; got != "prefix-$consumer_name" {
		t.Fatalf(
			"missing consumer variable label = %q; APISIX ctx.var.consumer_name is nil and leaves the template unchanged",
			got,
		)
	}
}
