package datadog

import (
	"net"
	"net/http"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// A missing match must not tag the raw request URI (cardinality).
func TestDatadogIncludePathOmitsRawURI(t *testing.T) {
	addr, received := startUDPServer(t, 5)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	p := newTestPlugin(t, Config{IncludePath: true, BatchMaxSize: 1})
	p.metadata = Metadata{
		Host: host, Port: mustAtoi(t, port), Namespace: "apisix",
		ConstantTags: []string{"source:apisix"},
	}
	if err := p.RunLogPhase(base.LogSnapshot{
		Started:  time.Now(),
		Finished: time.Now(),
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodGet,
			URI:    "/orders/12345?x=1",
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusOK},
	}); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	messages := collectMetricLines(t, received, 5, 5)
	if containsLinePart(messages, "path:/orders/12345") || containsLinePart(messages, "path:/orders/12345?x=1") {
		t.Fatalf("messages = %v, official include_path must not fall back to raw URI", messages)
	}
}
