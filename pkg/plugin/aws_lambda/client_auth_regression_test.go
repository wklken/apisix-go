package aws_lambda

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/function_upstream"
)

func TestClientAuthenticationHeadersTakePrecedence(t *testing.T) {
	for _, test := range []struct {
		name, api, auth   string
		wantAPI, wantAuth string
		signed            bool
	}{
		{name: "fill-api", wantAPI: "gateway-api"},
		{name: "keep-api-and-sign", api: "client-api", wantAPI: "client-api", signed: true},
		{name: "keep-both", api: "client-api", auth: "Bearer client", wantAPI: "client-api", wantAuth: "Bearer client"},
		{name: "fill-api-keep-auth", auth: "Bearer client", wantAPI: "gateway-api", wantAuth: "Bearer client"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(
				t,
				Config{
					FunctionURI: "http://lambda.invalid",
					Authorization: &Authorization{
						APIKey: "gateway-api",
						IAM:    &IAM{AccessKey: "AKID", SecretKey: "SECRET", AWSRegion: "us-east-1", Service: "lambda"},
					},
				},
			)
			req := httptest.NewRequest("POST", "http://lambda.invalid/?b=two+words&a=%2F", strings.NewReader("body"))
			if test.api != "" {
				req.Header.Set("X-Api-Key", test.api)
			}
			if test.auth != "" {
				req.Header.Set("Authorization", test.auth)
			}
			p.processRequest(req, function_upstream.Config{})
			if got := req.Header.Get("X-Api-Key"); got != test.wantAPI {
				t.Errorf("api=%q, want %q", got, test.wantAPI)
			}
			auth := req.Header.Get("Authorization")
			if test.signed {
				if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
					t.Errorf("missing IAM signature: %q", auth)
				}
			} else if auth != test.wantAuth {
				t.Errorf("auth=%q want %q", auth, test.wantAuth)
			}
		})
	}
}

func TestIAMSigningPreservesWireQuery(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{
			FunctionURI: "http://lambda.invalid",
			Authorization: &Authorization{
				IAM: &IAM{AccessKey: "AKID", SecretKey: "SECRET", AWSRegion: "us-east-1", Service: "lambda"},
			},
		},
	)
	req := httptest.NewRequest("POST", "http://lambda.invalid/?b=two+words&a=%2F", strings.NewReader("body"))
	p.processRequest(req, function_upstream.Config{})
	if !strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		t.Fatal("not signed")
	}
	if req.URL.RawQuery != "b=two+words&a=%2F" {
		t.Fatalf("wire query rewritten to %q", req.URL.RawQuery)
	}
}
