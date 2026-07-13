package proxy

import (
	"net/http"
	"net/http/httputil"
	"testing"
	"time"
)

func TestNewProxyHandlerWithFlushInterval(t *testing.T) {
	handler := NewProxyHandlerWithFlushInterval(
		http.DefaultTransport,
		func(req *http.Request) {},
		nil,
		nil,
		-1*time.Second,
	)

	rp, ok := handler.(*httputil.ReverseProxy)
	if !ok {
		t.Fatalf("handler type = %T, want *httputil.ReverseProxy", handler)
	}
	if rp.FlushInterval != -1*time.Second {
		t.Fatalf("FlushInterval = %s, want -1s", rp.FlushInterval)
	}
}
