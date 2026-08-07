package ai_auth

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSignAWSRequestAddsDeterministicSigV4Headers(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`)
	req, err := http.NewRequest(
		http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-v1/converse?trace=on",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	now := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	err = SignAWSRequest(req, body, AWSConfig{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret",
		SessionToken:    "session-token",
	}, "us-east-1", "bedrock", now)
	if err != nil {
		t.Fatalf("SignAWSRequest() error = %v", err)
	}

	const wantAuth = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260711/us-east-1/bedrock/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, " +
		"Signature=4df878a3e20d23fb974ef5817f2b30372a3524df30d9920398122fecb59f04b9"
	if got := req.Header.Get("Authorization"); got != wantAuth {
		t.Fatalf("Authorization = %q, want %q", got, wantAuth)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20260711T010203Z" {
		t.Fatalf("X-Amz-Date = %q", got)
	}
	if got := req.Header.Get(
		"X-Amz-Content-Sha256",
	); got != "331b626fbec967399a3c05643d22e56af43ee9839874a2db641bd025f84436d8" {
		t.Fatalf("X-Amz-Content-Sha256 = %q", got)
	}
	if got := req.Header.Get("X-Amz-Security-Token"); got != "session-token" {
		t.Fatalf("X-Amz-Security-Token = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want preserved", got)
	}
	if got := req.URL.RawQuery; got != "trace=on" {
		t.Fatalf("RawQuery after signing = %q, want trace=on", got)
	}
}

func TestSignAWSRequestValidatesCredentialsAndRegion(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/model/x/converse", nil)
	if err := SignAWSRequest(req, nil, AWSConfig{}, "us-east-1", "bedrock", time.Now()); err == nil {
		t.Fatal("missing credentials error = nil")
	}
	if err := SignAWSRequest(req, nil, AWSConfig{
		AccessKeyID: "key", SecretAccessKey: "secret",
	}, "", "bedrock", time.Now()); err == nil {
		t.Fatal("missing region error = nil")
	}
}

func TestSignAWSRequestWithOptionsReproducesComprehendSigning(t *testing.T) {
	body := []byte(`{"text":"hello"}`)
	req, err := http.NewRequest(
		http.MethodPost,
		"https://comprehend.us-east-1.amazonaws.com/",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "Comprehend_20171127.DetectToxicContent")

	now := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	err = SignAWSRequestWithOptions(req, body, AWSConfig{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret",
	}, SignAWSRequestOptions{
		Region:           "us-east-1",
		Service:          "comprehend",
		SetSecurityToken: true,
		CanonicalHeaders: []string{"content-type", "host", "x-amz-date", "x-amz-target"},
		HeaderValue:      strings.TrimSpace,
		CanonicalURI:     CanonicalURIPlain,
		CanonicalQuery:   CanonicalQueryRaw,
	}, now)
	if err != nil {
		t.Fatalf("SignAWSRequestWithOptions() error = %v", err)
	}

	if got := req.Header.Get("X-Amz-Date"); got != "20260711T010203Z" {
		t.Fatalf("X-Amz-Date = %q", got)
	}
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != "" {
		t.Fatalf("X-Amz-Content-Sha256 = %q, want empty", got)
	}
	const wantAuth = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260711/us-east-1/comprehend/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date;x-amz-target, " +
		"Signature=ee30a4ab3563f9dab4b23aa0529868c33b6aa775c878b27752f5411015a64e01"
	if got := req.Header.Get("Authorization"); got != wantAuth {
		t.Fatalf("Authorization = %q, want %q", got, wantAuth)
	}
}

func TestSignAWSRequestWithOptionsReproducesLambdaSigning(t *testing.T) {
	body := []byte(`{"text":"hello"}`)
	req, err := http.NewRequest(
		http.MethodGet,
		"https://lambda.us-east-1.amazonaws.com/2015-03-31/functions/my-func/invocations?x=1&b=2",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "lambda.us-east-1.amazonaws.com"
	req.Header.Set("X-Api-Key", "api-key")

	now := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	err = SignAWSRequestWithOptions(req, body, AWSConfig{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret",
	}, SignAWSRequestOptions{
		Region:                   "us-east-1",
		Service:                  "execute-api",
		DeriveHeadersFromRequest: true,
		CanonicalURI:             CanonicalURICleaned,
		CanonicalQuery:           CanonicalQuerySortedParts,
		RewriteQuery:             true,
	}, now)
	if err != nil {
		t.Fatalf("SignAWSRequestWithOptions() error = %v", err)
	}

	if got := req.Header.Get("X-Amz-Date"); got != "20260711T010203Z" {
		t.Fatalf("X-Amz-Date = %q", got)
	}
	const wantAuth = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260711/us-east-1/execute-api/aws4_request, " +
		"SignedHeaders=host;x-amz-date;x-api-key, " +
		"Signature=d8f0f30db6aab0dc20f917b10c1430d437bfb9163802663722a77d2c4217a88e"
	if got := req.Header.Get("Authorization"); got != wantAuth {
		t.Fatalf("Authorization = %q, want %q", got, wantAuth)
	}
	if got := req.URL.RawQuery; got != "b=2&x=1" {
		t.Fatalf("RawQuery after signing = %q, want b=2&x=1", got)
	}
}

func TestCanonicalRequestComponentsMatchAPISIXNormalization(t *testing.T) {
	if got := CanonicalURICleaned(&url.URL{Path: "api//v1/../users/"}); got != "/api/users" {
		t.Fatalf("CanonicalURICleaned() = %q, want /api/users", got)
	}
	if got := CanonicalQuerySortedParts(
		&url.URL{RawQuery: "z=last&name=APISIX%20Go&a=first"},
	); got != "a=first&name=APISIX%20Go&z=last" {
		t.Fatalf("CanonicalQuerySortedParts() = %q, want encoded and sorted query", got)
	}
	complexQuery := "with%20space=a%2Fb%20c&multi=m2&multi=m1&flag&a=*&a-=x"
	wantComplex := "a=%2A&a-=x&flag=&multi=m1&multi=m2&with%20space=a%2Fb%20c"
	if got := CanonicalQuerySortedParts(&url.URL{RawQuery: complexQuery}); got != wantComplex {
		t.Fatalf("CanonicalQuerySortedParts(complex) = %q, want %q", got, wantComplex)
	}

	req, err := http.NewRequest(http.MethodPost, "https://lambda.example/prod", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", " application/json;  charset=utf-8 ")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("X-Amz-Date", "20200102T030405Z")
	req.Header.Set("X-Custom", "  first   second ")

	now := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	err = SignAWSRequestWithOptions(req, nil, AWSConfig{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret",
	}, SignAWSRequestOptions{
		Region:                   "us-east-1",
		Service:                  "execute-api",
		DeriveHeadersFromRequest: true,
		CanonicalURI:             CanonicalURICleaned,
		CanonicalQuery:           CanonicalQuerySortedParts,
		RewriteQuery:             true,
	}, now)
	if err != nil {
		t.Fatalf("SignAWSRequestWithOptions() error = %v", err)
	}

	const wantAuth = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260711/us-east-1/execute-api/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date;x-custom, " +
		"Signature=9987dd8aba430aa60259a131a13d37a02b0645e88d1fdc68845c647db8e18c39"
	if got := req.Header.Get("Authorization"); got != wantAuth {
		t.Fatalf("Authorization = %q, want %q", got, wantAuth)
	}
}
