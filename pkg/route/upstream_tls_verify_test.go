package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestHTTPSIgnoresKafkaTLSVerify(t *testing.T) {
	upstream := httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hello world")) }),
	)
	defer upstream.Close()
	var u resource.Upstream
	if err := json.Unmarshal(
		[]byte(`{"scheme":"https","nodes":{"127.0.0.1:1983":1},"tls":{"verify":true}}`),
		&u,
	); err != nil {
		t.Fatal(err)
	}
	option, err := buildTransportOptionWithSSLResolver(resource.Route{}, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := proxy.NewTransport(option)
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Get(upstream.URL)
	if err != nil {
		t.Fatalf("HTTPS tls.verify=true should preserve APISIX 3.17 behavior: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello world" {
		t.Fatal(string(body))
	}
}
