package saml_auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/testutil"
)

// BenchmarkStaticConfigPath measures the per-request SAML authentication
// redirect path with valid static configuration. The SP key pair is staged
// during materialization and the IDP metadata at PostInit, never per request.
func BenchmarkStaticConfigPath(b *testing.B) {
	certPEM, keyPEM := benchmarkCertificate(b)
	cfg := Config{
		SPIssuer:                  "https://sp.example.com",
		IDPURI:                    "https://idp.example.com/sso",
		IDPCert:                   certPEM,
		LoginCallbackURI:          "http://example.com/login/callback",
		LogoutURI:                 "/logout",
		LogoutCallbackURI:         "http://example.com/logout/callback",
		LogoutRedirectURI:         "/logged-out",
		SPCert:                    certPEM,
		SPPrivateKey:              keyPEM,
		AuthProtocolBindingMethod: "HTTP-Redirect",
		Secret:                    strings.Repeat("s", 16),
	}
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	secrets, scope, closeAttempt := testutil.ScopedSecretHarness(
		b,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	b.Cleanup(closeAttempt)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		b.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	b.Cleanup(p.Stop)
	if p.spKeyPair == nil || p.spIDPMetadata == nil {
		b.Fatal("static SAML state was not prepared before requests")
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		p.Handler(next).ServeHTTP(rr, req)
		if rr.Code != http.StatusFound {
			b.Fatalf("status = %d, want 302 redirect to IDP", rr.Code)
		}
	}
}

func benchmarkCertificate(b testing.TB) (string, string) {
	b.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		b.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sp.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		b.Fatalf("create certificate: %v", err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return certPEM, keyPEM
}
