package ai_auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// BenchmarkProviderDispatch measures the per-request provider signing path
// for the default and plain-URI signer shapes.
func BenchmarkProviderDispatch(b *testing.B) {
	body := []byte(`{"model":"claude","messages":[{"role":"user","content":"hello"}]}`)
	now := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)

	for _, scenario := range []struct {
		name string
		opts SignAWSRequestOptions
	}{
		{
			name: "sign-default",
			opts: SignAWSRequestOptions{
				Region:             "us-east-1",
				Service:            "bedrock",
				IncludePayloadHash: true,
				SetSecurityToken:   true,
			},
		},
		{
			name: "sign-plain-uri",
			opts: SignAWSRequestOptions{
				Region:             "us-east-1",
				Service:            "bedrock",
				IncludePayloadHash: true,
				SetSecurityToken:   true,
				CanonicalURI:       CanonicalURIPlain,
			},
		},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			req := httptest.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/claude/converse", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := SignAWSRequestWithOptions(req, body, AWSConfig{
					AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret",
				}, scenario.opts, now); err != nil {
					b.Fatalf("sign error = %v", err)
				}
			}
		})
	}
}
