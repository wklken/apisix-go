package route

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/dubbo_proxy"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestPreparedDubboTerminalUsesUpstreamReadTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if !discardRouteHessianFrame(conn) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}()
	address := listener.Addr().(*net.TCPAddr)
	upstream := resource.Upstream{
		Scheme:  "dubbo",
		Timeout: resource.Timeout{Connect: 0.02, Send: 0.02, Read: 0.02},
		Nodes:   []resource.Node{{Host: "127.0.0.1", Port: address.Port, Weight: 1}},
	}
	targets, _, err := planUpstreamNodes(upstream)
	if err != nil {
		t.Fatal(err)
	}
	_, terminals, err := buildPreparedReverseHandler(
		resource.Route{ID: "dubbo-timeout"},
		upstream,
		targets,
		PreparedUpstreamRuntime{
			LoadBalancer: pxy.NewWeightedRRLoadBalance(targets),
			RoundTripper: http.DefaultTransport,
		},
		&testEffectiveConfig().Config,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := dubbo_proxy.WithConfig(
		httptest.NewRequest("POST", "/dubbo", nil),
		dubbo_proxy.Config{ServiceName: "svc", ServiceVersion: "1.0.0", Method: "hello"},
	)
	response := httptest.NewRecorder()
	if _, _, _, err := terminals.dubbo.RunExclusiveProtocol(response, request, nil); err != nil {
		t.Fatal(err)
	}
	<-done
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%q; want configured read timeout 504", response.Code, response.Body.String())
	}
}
