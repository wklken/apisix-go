package elasticsearch_logger

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestParityExplicitZeroRetryDelay(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal(
		[]byte(`{"endpoint_addr": "http://127.0.0.1:9200", "field": {"index": "test"}, "retry_delay": 0}`),
		&cfg,
	); err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(t, cfg)
	t.Cleanup(p.Stop)
	if p.config.RetryDelay != 0 {
		t.Fatalf("retry_delay=%d, want explicit zero", p.config.RetryDelay)
	}
}
