package key_auth

import (
	"net/http"
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestSanitizeLogSnapshotRemovesDefaultCredentialsFromCanonicalRepresentations(t *testing.T) {
	p := newTestPlugin(t, Config{})
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
		URI:    "/orders?apikey=secret&keep=yes",
		URL:    "https://gateway.example/orders?apikey=secret&keep=yes",
		Header: http.Header{"Apikey": {"secret"}, "X-Visible": {"yes"}},
		Query:  map[string][]string{"apikey": {"secret"}, "keep": {"yes"}},
		APISIXVars: map[string]any{
			"$args":         "apikey=secret&keep=yes",
			"$query_string": "apikey=secret&keep=yes",
			"$request_uri":  "/orders?apikey=secret&keep=yes",
			"$arg_apikey":   "secret",
			"$http_apikey":  "secret",
			"$upstream_uri": "/upstream?apikey=secret&keep=yes",
		},
		RequestVars: map[string]any{
			"$args":         "apikey=secret&keep=yes",
			"$query_string": "apikey=secret&keep=yes",
			"$request_uri":  "/orders?apikey=secret&keep=yes",
			"$arg_apikey":   "secret",
			"$http_apikey":  "secret",
		},
	}}

	if err := p.SanitizeLogSnapshot(&snapshot); err != nil {
		t.Fatalf("SanitizeLogSnapshot() error = %v", err)
	}
	if got := snapshot.Request.Header.Get("apikey"); got != "" {
		t.Fatalf("snapshot apikey header = %q, want removed", got)
	}
	if _, ok := snapshot.Request.Query["apikey"]; ok {
		t.Fatalf("snapshot query still contains apikey: %#v", snapshot.Request.Query)
	}
	if snapshot.Request.URI != "/orders?keep=yes" || snapshot.Request.URL != "https://gateway.example/orders?keep=yes" {
		t.Fatalf("sanitized request targets = %q / %q", snapshot.Request.URI, snapshot.Request.URL)
	}
	for _, vars := range []map[string]any{snapshot.Request.APISIXVars, snapshot.Request.RequestVars} {
		for _, key := range []string{"$arg_apikey", "$http_apikey"} {
			if _, ok := vars[key]; ok {
				t.Fatalf("sanitized request vars still contain %s: %#v", key, vars)
			}
		}
		for _, key := range []string{"$args", "$query_string", "$request_uri"} {
			if got := vars[key]; got != "keep=yes" && got != "/orders?keep=yes" {
				t.Fatalf("sanitized request var %s = %#v, want redacted value", key, got)
			}
		}
	}
}

func TestSanitizeLogSnapshotRemovesCustomCredentialsWithoutRequestPhase(t *testing.T) {
	p := newTestPlugin(t, Config{Header: "X-Custom-Key", Query: "access_token"})
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
		URI:    "/orders?access_token=secret&keep=yes",
		URL:    "https://gateway.example/orders?access_token=secret&keep=yes",
		Header: http.Header{"X-Custom-Key": {"secret"}},
		Query:  map[string][]string{"access_token": {"secret"}, "keep": {"yes"}},
		APISIXVars: map[string]any{
			"$arg_access_token":  "secret",
			"$http_x_custom_key": "secret",
		},
	}}

	if err := p.SanitizeLogSnapshot(&snapshot); err != nil {
		t.Fatalf("SanitizeLogSnapshot() error = %v", err)
	}
	if snapshot.Request.Header.Get("X-Custom-Key") != "" {
		t.Fatalf("custom credential header = %q, want removed", snapshot.Request.Header.Get("X-Custom-Key"))
	}
	if _, ok := snapshot.Request.Query["access_token"]; ok {
		t.Fatalf("custom credential query = %#v, want removed", snapshot.Request.Query)
	}
	if snapshot.Request.URI != "/orders?keep=yes" || snapshot.Request.URL != "https://gateway.example/orders?keep=yes" {
		t.Fatalf("sanitized custom targets = %q / %q", snapshot.Request.URI, snapshot.Request.URL)
	}
	if _, ok := snapshot.Request.APISIXVars["$arg_access_token"]; ok {
		t.Fatalf("custom query variable leaked: %#v", snapshot.Request.APISIXVars)
	}
	if _, ok := snapshot.Request.APISIXVars["$http_x_custom_key"]; ok {
		t.Fatalf("custom header variable leaked: %#v", snapshot.Request.APISIXVars)
	}
}
