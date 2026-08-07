package variable

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestGetNginxVarResolvesDeterministicVariables(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://api.example.test:8443/orders?q=blue", nil)
	request.Host = "api.example.test:8443"
	request.RemoteAddr = "192.0.2.20:1234"
	request.Proto = "HTTP/2.0"
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("User-Agent", "unit-agent")
	request.Header.Set("Referer", "https://referrer.test/")
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	request.Header.Set("Content-Length", "7")
	request.Header.Set("Content-Type", "application/json")

	tests := map[string]string{
		"$request_method":       "POST",
		"$request_line":         "POST /orders?q=blue HTTP/2.0",
		"$request_uri":          "/orders?q=blue",
		"$remote_addr":          "192.0.2.20",
		"$host":                 "api.example.test",
		"$http_host":            "api.example.test:8443",
		"$uri":                  "/orders",
		"$args":                 "q=blue",
		"$query_string":         "q=blue",
		"$http_user_agent":      "unit-agent",
		"$http_referer":         "https://referrer.test/",
		"$server_protocol":      "HTTP/2.0",
		"$http_x_forwarded_for": "198.51.100.8",
		"$scheme":               "https",
		"$content_length":       "7",
		"$content_type":         "application/json",
	}
	for key, want := range tests {
		if got := GetNginxVar(request, key); got != want {
			t.Fatalf("GetNginxVar(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestGetNginxVarTimeVariablesParseWithProductionLayouts(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)

	if got := GetNginxVar(request, "$time_iso8601"); got == "" {
		t.Fatal("$time_iso8601 = empty, want current time in RFC3339")
	} else if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Fatalf("$time_iso8601 = %q, not RFC3339: %v", got, err)
	}

	if got := GetNginxVar(request, "$time_local"); got == "" {
		t.Fatal("$time_local = empty, want current time in NGINX layout")
	} else if _, err := time.Parse("02/Jan/2006:15:04:05 -0700", got); err != nil {
		t.Fatalf("$time_local = %q, not NGINX layout: %v", got, err)
	}
}

func TestGetNginxVarUnknownVariableIsEmpty(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	if got := GetNginxVar(request, "$not_a_known_variable"); got != "" {
		t.Fatalf("GetNginxVar(unknown) = %q, want empty", got)
	}
}
