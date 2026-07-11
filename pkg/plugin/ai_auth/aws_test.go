package ai_auth

import (
	"bytes"
	"net/http"
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
	now := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	err = SignAWSRequest(req, body, AWSConfig{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret",
		SessionToken:    "session-token",
	}, "us-east-1", "bedrock", now)
	if err != nil {
		t.Fatalf("SignAWSRequest() error = %v", err)
	}

	if got := req.Header.Get("X-Amz-Date"); got != "20260711T010203Z" {
		t.Fatalf("X-Amz-Date = %q", got)
	}
	if got := req.Header.Get("X-Amz-Security-Token"); got != "session-token" {
		t.Fatalf("X-Amz-Security-Token = %q", got)
	}
	authorization := req.Header.Get("Authorization")
	if !strings.Contains(authorization, "Credential=AKIDEXAMPLE/20260711/us-east-1/bedrock/aws4_request") ||
		!strings.Contains(authorization, "SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token") ||
		!strings.Contains(authorization, "Signature=") {
		t.Fatalf("Authorization = %q", authorization)
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
