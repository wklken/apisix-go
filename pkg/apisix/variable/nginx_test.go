package variable

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetNginxVarResolvesHostAndRemoteAddressLikeNginx(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/hello", nil)
	request.Host = "logs.example.test"
	request.RemoteAddr = "192.0.2.10:8080"

	if _, ok := NginxVars["$host"]; !ok {
		t.Fatal("$host is not registered as an NGINX variable")
	}
	if got := GetNginxVar(request, "$host"); got != "logs.example.test" {
		t.Fatalf("$host = %q, want logs.example.test", got)
	}
	if got := GetNginxVar(request, "$remote_addr"); got != "192.0.2.10" {
		t.Fatalf("$remote_addr = %q, want 192.0.2.10", got)
	}
}
