package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestSSLAcceptsDeferredClientVerification(t *testing.T) {
	cert, key := testServerKeyPair(t, "skip")
	_, ca := testCertificateAuthority(t, "client")
	for _, patterns := range [][]string{{"^/health$"}, {}} {
		snapshot, err := Compile(Input{Config: testFrontendConfig(), SSLs: map[string]resource.SSL{"skip": {
			Sni: "skip.example.test", Cert: cert, Key: key, Status: 1,
			Client: &resource.SSLClient{CA: ca, Depth: 1, SkipMTLSURIRegex: patterns},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		selected, err := snapshot.TLSConfig().GetConfigForClient(&tls.ClientHelloInfo{ServerName: "skip.example.test"})
		if err != nil {
			t.Fatal(err)
		}
		if selected.ClientAuth != tls.RequestClientCert {
			t.Fatal("skip field did not defer client verification")
		}
		if err := selected.VerifyConnection(tls.ConnectionState{}); err == nil {
			t.Fatal("deferred handshake without an HTTP context succeeded")
		}

	}
}

func TestSSLDeferredClientVerificationOverHTTP(t *testing.T) {
	cert, key := testServerKeyPair(t, "skip")
	trusted, ca := deferredClientCertificate(t, "trusted")
	untrusted, _ := deferredClientCertificate(t, "untrusted")
	for _, http2 := range []bool{false, true} {
		for _, test := range []struct {
			name     string
			client   *tls.Certificate
			verified bool
		}{
			{"missing", nil, false}, {"untrusted", &untrusted, false}, {"trusted", &trusted, true},
		} {
			t.Run(fmt.Sprintf("http2=%t/%s", http2, test.name), func(t *testing.T) {
				ssl := resource.SSL{
					Sni:    "skip.example.test",
					Cert:   cert,
					Key:    key,
					Status: 1,
					Client: &resource.SSLClient{CA: ca, Depth: 0, SkipMTLSURIRegex: []string{"^/health$"}},
				}
				snapshot, err := Compile(
					Input{Config: testFrontendConfig(), SSLs: map[string]resource.SSL{"skip": ssl}},
				)
				if err != nil {
					t.Fatal(err)
				}
				var active atomic.Pointer[tls.Config]
				active.Store(snapshot.TLSConfig())
				var handshakes atomic.Int32
				handler := VerifyClientRequests(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
					),
				)
				server := httptest.NewUnstartedServer(handler)
				server.Config.ConnContext = ConnectionContext
				server.EnableHTTP2 = http2
				server.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					handshakes.Add(1)
					selected, err := active.Load().GetConfigForClient(hello)
					if err == nil && http2 {
						selected.NextProtos = []string{"h2", "http/1.1"}
					}
					return selected, err
				}}
				server.StartTLS()
				defer server.Close()
				transport := &http.Transport{ForceAttemptHTTP2: http2, TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
					ServerName:         ssl.Sni,
					GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
						if test.client == nil {
							return &tls.Certificate{}, nil
						}
						return test.client, nil
					},
				}}
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
				for round := range 2 {
					for _, path := range []string{"/health", "/health?check=1", "/%68ealth", "/foo/../health", "//health", "/private", "/health/other"} {
						request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
						if err != nil {
							t.Fatal(err)
						}
						request.Host = ssl.Sni
						response, err := client.Do(request)
						if err != nil {
							t.Fatal(err)
						}
						_, _ = io.Copy(io.Discard, response.Body)
						_ = response.Body.Close()
						want := http.StatusNoContent
						if !test.verified && (path == "/private" || path == "/health/other") {
							want = http.StatusBadRequest
						}
						if response.StatusCode != want {
							t.Fatalf("round=%d path=%s status=%d want=%d", round, path, response.StatusCode, want)
						}
						if http2 && response.ProtoMajor != 2 {
							t.Fatalf("protocol=%s", response.Proto)
						}
					}
					// A newly published SSL removes mTLS. The existing connection must still
					// enforce the certificate and URI policy selected during its handshake.
					ssl.Client = nil
					replacement, err := Compile(
						Input{Config: testFrontendConfig(), SSLs: map[string]resource.SSL{"skip": ssl}},
					)
					if err != nil {
						t.Fatal(err)
					}
					active.Store(replacement.TLSConfig())
				}
				if got := handshakes.Load(); got != 1 {
					t.Fatalf("handshakes=%d; keep-alive was not exercised", got)
				}
			})
		}
	}
}

func deferredClientCertificate(t *testing.T, name string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{
			Certificate: [][]byte{der},
			PrivateKey:  key,
		}, string(
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		)
}

func TestSSLClientVerificationBindsRequestHostToSNI(t *testing.T) {
	cert, key := testServerKeyPair(t, "hosts")
	trusted, ca := deferredClientCertificate(t, "trusted")
	for _, http2 := range []bool{false, true} {
		for _, tc := range []struct {
			name, sni, host, uri string
			client               bool
			want                 int
		}{
			{"public-to-protected", "public.example.test", "secure.example.test", "/private", false, 400},
			{"public-to-protected-skip", "public.example.test", "secure.example.test", "/health", false, 400},
			{"wildcard-cross-host", "a.protected.test", "b.protected.test", "/private", true, 400},
			{"same-protected-host", "a.protected.test", "a.protected.test", "/private", true, 204},
			{"public-overrides-wildcard", "public.example.test", "public.protected.test", "/private", false, 204},
			{"same-host-skip", "secure.example.test", "secure.example.test", "/health", false, 204},
			{"same-host-denied", "secure.example.test", "secure.example.test", "/private", false, 400},
			{"public-cross-host", "public.example.test", "other.example.test", "/private", false, 204},
			{"no-sni-fallback", "", "a.protected.test", "/private", true, 204},
			{"no-sni-other-protected", "", "b.protected.test", "/private", true, 400},
		} {
			t.Run(fmt.Sprintf("http2=%t/%s", http2, tc.name), func(t *testing.T) {
				cfg := testFrontendConfig()
				cfg.Apisix.Ssl.FallbackSNI = "a.protected.test"
				snapshot, err := Compile(Input{Config: cfg, SSLs: map[string]resource.SSL{
					"public": {
						Snis:   []string{"public.example.test", "public.protected.test"},
						Cert:   cert,
						Key:    key,
						Status: 1,
					},
					"secure": {
						Sni:    "secure.example.test",
						Cert:   cert,
						Key:    key,
						Status: 1,
						Client: &resource.SSLClient{CA: ca, SkipMTLSURIRegex: []string{"^/health$"}},
					},
					"wildcard": {
						Sni:    "*.protected.test",
						Cert:   cert,
						Key:    key,
						Status: 1,
						Client: &resource.SSLClient{CA: ca},
					},
				}})
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewUnstartedServer(
					VerifyClientRequests(
						http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }),
					),
				)
				server.Config.ConnContext = ConnectionContext
				server.EnableHTTP2 = http2
				server.TLS = snapshot.TLSConfig()
				if http2 {
					selectConfig := server.TLS.GetConfigForClient
					server.TLS.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
						config, err := selectConfig(hello)
						if err == nil {
							config.NextProtos = []string{"h2", "http/1.1"}
						}
						return config, err
					}
				}
				server.StartTLS()
				defer server.Close()
				transport := &http.Transport{
					ForceAttemptHTTP2: http2,
					TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, ServerName: tc.sni},
				}
				if tc.client {
					transport.TLSClientConfig.Certificates = []tls.Certificate{trusted}
				}
				defer transport.CloseIdleConnections()
				request, err := http.NewRequest(http.MethodGet, server.URL+tc.uri, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Host = tc.host
				response, err := (&http.Client{Transport: transport, Timeout: 3 * time.Second}).Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := response.Body.Close(); err != nil {
						t.Error(err)
					}
				}()
				if response.StatusCode != tc.want {
					t.Fatalf("status=%d want=%d", response.StatusCode, tc.want)
				}
				if http2 && response.ProtoMajor != 2 {
					t.Fatalf("protocol=%s", response.Proto)
				}
			})
		}
	}
}
