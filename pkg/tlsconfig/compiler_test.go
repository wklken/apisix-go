package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

const testTLS12Cipher = "ECDHE-ECDSA-AES128-GCM-SHA256"

func TestFrontendEnabledRequiresEnabledValidListener(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{name: "nil"},
		{name: "disabled", cfg: &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
			Listen: []config.Listen{{Port: 9443}},
		}}}},
		{name: "missing listener", cfg: &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{Enable: true}}}},
		{name: "invalid listener", cfg: &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
			Enable: true, Listen: []config.Listen{{Port: 0}, {Port: 65536}},
		}}}},
		{name: "valid listener", cfg: &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
			Enable: true, Listen: []config.Listen{{Port: 9443}},
		}}}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := FrontendEnabled(test.cfg); got != test.want {
				t.Fatalf("FrontendEnabled() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCompileBaseLeavesCertificateSelectionToTheCaller(t *testing.T) {
	cfg := testFrontendConfig()
	cfg.Apisix.Ssl.FallbackSNI = "fallback.example.test"
	snapshot, err := CompileBase(BaseInput{Config: cfg})
	if err != nil {
		t.Fatalf("CompileBase() error = %v", err)
	}
	tlsConfig := snapshot.TLSConfig()
	if tlsConfig.GetCertificate != nil || tlsConfig.GetConfigForClient != nil {
		t.Fatal("CompileBase() installed a certificate selector")
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 || len(tlsConfig.CipherSuites) != 1 {
		t.Fatalf("CompileBase() TLS settings = %#v", tlsConfig)
	}
}

func TestCompileOwnsTLSSettingsAndReturnsDefensiveClones(t *testing.T) {
	cfg := testFrontendConfig()
	cfg.Apisix.EnableHttp2 = true
	cfg.Apisix.Ssl.SslProtocols = "TLSv1.2 TLSv1.3"
	cfg.Apisix.Ssl.SslSessionTickets = true

	snapshot, err := Compile(Input{Config: cfg})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	first := snapshot.TLSConfig()
	if first.MinVersion != tls.VersionTLS12 || first.MaxVersion != tls.VersionTLS13 {
		t.Fatalf("TLS versions = %x-%x, want TLS 1.2-1.3", first.MinVersion, first.MaxVersion)
	}
	if len(first.CipherSuites) != 1 || first.CipherSuites[0] != tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 {
		t.Fatalf("CipherSuites = %#v", first.CipherSuites)
	}
	if got := strings.Join(first.NextProtos, ","); got != "h2,http/1.1" {
		t.Fatalf("NextProtos = %q", got)
	}
	if first.SessionTicketsDisabled {
		t.Fatal("SessionTicketsDisabled = true, want false")
	}

	cfg.Apisix.Ssl.SslProtocols = "TLSv1.3"
	first.MinVersion = tls.VersionTLS13
	first.CipherSuites[0] = 0
	first.NextProtos[0] = "mutated"
	second := snapshot.TLSConfig()
	if second.MinVersion != tls.VersionTLS12 || second.CipherSuites[0] == 0 || second.NextProtos[0] != "h2" {
		t.Fatalf("TLSConfig() leaked caller mutation: %#v", second)
	}
}

func TestCompileRejectsDuplicateNormalizedSNI(t *testing.T) {
	certificate, key := testServerKeyPair(t, "duplicate")
	for _, test := range []struct {
		name      string
		firstSNI  string
		secondSNI string
	}{
		{name: "exact", firstSNI: "duplicate.example.test", secondSNI: " DUPLICATE.EXAMPLE.TEST. "},
		{name: "wildcard", firstSNI: "*.example.test", secondSNI: " *.EXAMPLE.TEST. "},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Compile(Input{
				Config: testFrontendConfig(),
				SSLs: map[string]resource.SSL{
					"first": {
						ID: "first", Sni: test.firstSNI, Cert: certificate, Key: key, Status: 1,
					},
					"second": {
						ID: "second", Sni: test.secondSNI, Cert: certificate, Key: key, Status: 1,
					},
				},
			})
			if err == nil || !strings.Contains(err.Error(), "duplicate SNI") ||
				!strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
				t.Fatalf("Compile() error = %v, want deterministic duplicate ownership error", err)
			}
		})
	}
}

func TestCompileSelectsExactWildcardAndFallbackCertificatesFromOwnedResources(t *testing.T) {
	exactCert, exactKey := testServerKeyPair(t, "exact")
	wildcardCert, wildcardKey := testServerKeyPair(t, "wildcard")
	fallbackCert, fallbackKey := testServerKeyPair(t, "fallback")
	ssls := map[string]resource.SSL{
		"exact": {
			ID: "exact", Snis: []string{" API.Example.Test. "}, Cert: exactCert, Key: exactKey, Status: 1,
		},
		"wildcard": {
			ID: "wildcard", Sni: "*.example.test", Cert: wildcardCert, Key: wildcardKey, Status: 1,
		},
		"fallback": {
			ID: "fallback", Sni: "fallback.example.test", Cert: fallbackCert, Key: fallbackKey, Status: 1,
		},
		"disabled": {
			ID: "disabled", Sni: "disabled.other.test", Cert: fallbackCert, Key: fallbackKey, Status: 0,
		},
	}
	cfg := testFrontendConfig()
	cfg.Apisix.Ssl.FallbackSNI = " fallback.example.test "
	snapshot, err := Compile(Input{Config: cfg, SSLs: ssls})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	ssls["exact"] = resource.SSL{ID: "mutated", Status: 0}
	cases := []struct {
		name       string
		serverName string
		wantCN     string
		wantError  bool
	}{
		{name: "exact beats wildcard", serverName: "API.EXAMPLE.TEST", wantCN: "exact"},
		{name: "one label wildcard", serverName: "one.example.test", wantCN: "wildcard"},
		{name: "empty SNI uses fallback", wantCN: "fallback"},
		{name: "deep label does not match wildcard", serverName: "two.labels.example.test", wantError: true},
		{name: "disabled is absent", serverName: "disabled.other.test", wantError: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			certificate, err := snapshot.TLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: test.serverName})
			if test.wantError {
				if err == nil {
					t.Fatal("GetCertificate() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("GetCertificate() error = %v", err)
			}
			if got := testCertificateCommonName(t, certificate); got != test.wantCN {
				t.Fatalf("selected common name = %q, want %q", got, test.wantCN)
			}
		})
	}

	first, err := snapshot.TLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "api.example.test"})
	if err != nil {
		t.Fatalf("first exact selection: %v", err)
	}
	first.Certificate[0][0] ^= 0xff
	second, err := snapshot.TLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "api.example.test"})
	if err != nil {
		t.Fatalf("second exact selection: %v", err)
	}
	if got := testCertificateCommonName(t, second); got != "exact" {
		t.Fatalf("certificate selector leaked caller mutation: common name = %q", got)
	}
}

func TestCompileUsesCatchAllSNIResourceAsCertificateFallback(t *testing.T) {
	certificate, key := testServerKeyPair(t, "catch-all")
	snapshot, err := Compile(Input{
		Config: testFrontendConfig(),
		SSLs: map[string]resource.SSL{
			"catch-all": {
				ID: "catch-all", Sni: "*", Cert: certificate, Key: key, Status: 1,
			},
		},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	selected, err := snapshot.TLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "unknown.example.test"})
	if err != nil {
		t.Fatalf("GetCertificate() error = %v", err)
	}
	if got := testCertificateCommonName(t, selected); got != "catch-all" {
		t.Fatalf("selected common name = %q, want catch-all", got)
	}
}

func TestCompileAppliesPerResourceSSLProtocols(t *testing.T) {
	certificate, key := testServerKeyPair(t, "tls13-only")
	snapshot, err := Compile(Input{
		Config: testFrontendConfig(),
		SSLs: map[string]resource.SSL{
			"tls13-only": {
				ID: "tls13-only", Sni: "tls13.example.test", Cert: certificate, Key: key, Status: 1,
				SSLProtocols: []string{"TLSv1.3"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	selected, err := snapshot.TLSConfig().GetConfigForClient(
		&tls.ClientHelloInfo{ServerName: "tls13.example.test"},
	)
	if err != nil {
		t.Fatalf("GetConfigForClient() error = %v", err)
	}
	if selected.MinVersion != tls.VersionTLS13 || selected.MaxVersion != tls.VersionTLS13 {
		t.Fatalf(
			"selected TLS versions = %x-%x, want TLS 1.3 only",
			selected.MinVersion,
			selected.MaxVersion,
		)
	}
}

func TestCompileRejectsProtocolVersionsMissingFromResourceSSLProtocols(t *testing.T) {
	certificate, key := testServerKeyPair(t, "non-contiguous-protocols")
	snapshot, err := Compile(Input{
		Config: testFrontendConfig(),
		SSLs: map[string]resource.SSL{
			"non-contiguous": {
				ID: "non-contiguous", Sni: "non-contiguous.example.test",
				Cert: certificate, Key: key, Status: 1,
				SSLProtocols: []string{"TLSv1.1", "TLSv1.3"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	selected, err := snapshot.TLSConfig().GetConfigForClient(
		&tls.ClientHelloInfo{
			ServerName:        "non-contiguous.example.test",
			SupportedVersions: []uint16{tls.VersionTLS12, tls.VersionTLS11},
		},
	)
	if err != nil {
		t.Fatalf("GetConfigForClient() error = %v", err)
	}
	if selected.MinVersion != tls.VersionTLS11 || selected.MaxVersion != tls.VersionTLS11 {
		t.Fatalf(
			"selected TLS versions = %x-%x, want highest offered configured version TLS 1.1",
			selected.MinVersion,
			selected.MaxVersion,
		)
	}
	_, err = snapshot.TLSConfig().GetConfigForClient(&tls.ClientHelloInfo{
		ServerName:        "non-contiguous.example.test",
		SupportedVersions: []uint16{tls.VersionTLS12},
	})
	if err == nil {
		t.Fatal("TLS 1.2-only client was accepted although TLS 1.2 is absent from ssl_protocols")
	}
}

func TestCompileDoesNotInheritGlobalProtocolsForExplicitEmptyResourceProtocols(t *testing.T) {
	certificate, key := testServerKeyPair(t, "empty-protocols")
	snapshot, err := Compile(Input{
		Config: testFrontendConfig(),
		SSLs: map[string]resource.SSL{
			"empty": {
				ID: "empty", Sni: "empty.example.test", Cert: certificate, Key: key, Status: 1,
				SSLProtocols: []string{},
			},
		},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	_, err = snapshot.TLSConfig().GetConfigForClient(&tls.ClientHelloInfo{
		ServerName:        "empty.example.test",
		SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12},
	})
	if err == nil {
		t.Fatal("explicit empty resource ssl_protocols inherited global TLS protocols")
	}
}

func TestCompileAppliesPerResourceClientCAAndVerificationDepth(t *testing.T) {
	clientCA, clientCAPEM := testCertificateAuthority(t, "client-root")
	serverCert, serverKey := testServerKeyPair(t, "mtls-server")
	cfg := testFrontendConfig()
	snapshot, err := Compile(Input{
		Config: cfg,
		SSLs: map[string]resource.SSL{
			"mtls": {
				ID: "mtls", Sni: "mtls.example.test", Cert: serverCert, Key: serverKey, Status: 1,
				Client: &resource.SSLClient{CA: clientCAPEM, Depth: 1},
			},
		},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	selected, err := snapshot.TLSConfig().GetConfigForClient(&tls.ClientHelloInfo{ServerName: "mtls.example.test"})
	if err != nil {
		t.Fatalf("GetConfigForClient() error = %v", err)
	}
	if selected.ClientAuth != tls.RequireAndVerifyClientCert || selected.ClientCAs == nil {
		t.Fatalf("client authentication = %v/%v", selected.ClientAuth, selected.ClientCAs)
	}
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: "client"}}
	intermediate := &x509.Certificate{Subject: pkix.Name{CommonName: "intermediate"}}
	validState := tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf, clientCA}}}
	if err := selected.VerifyConnection(validState); err != nil {
		t.Fatalf("depth-one chain rejected: %v", err)
	}
	tooDeepState := tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{leaf, intermediate, clientCA}},
	}
	if err := selected.VerifyConnection(tooDeepState); err == nil {
		t.Fatal("chain exceeding configured client depth was accepted")
	}

	selected.ClientCAs.AddCert(&x509.Certificate{Raw: []byte("caller mutation")})
	selected.Certificates[0].Certificate[0][0] ^= 0xff
	again, err := snapshot.TLSConfig().GetConfigForClient(&tls.ClientHelloInfo{ServerName: "mtls.example.test"})
	if err != nil {
		t.Fatalf("second GetConfigForClient() error = %v", err)
	}
	if got := testCertificateCommonName(t, &again.Certificates[0]); got != "mtls-server" {
		t.Fatalf("config selector leaked caller mutation: common name = %q", got)
	}
}

func TestCompileAppliesOwnedTrustedClientCA(t *testing.T) {
	_, trustedCAPEM := testCertificateAuthority(t, "trusted-client")
	extraCA, _ := testCertificateAuthority(t, "caller-only")
	cfg := testFrontendConfig()
	cfg.Apisix.Ssl.SslTrustedCertificate = "trusted-client.pem"
	snapshot, err := Compile(Input{Config: cfg, TrustedClientCAPEM: []byte(trustedCAPEM)})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	first := snapshot.TLSConfig()
	if first.ClientAuth != tls.RequireAndVerifyClientCert || first.ClientCAs == nil {
		t.Fatalf("client authentication = %v/%v", first.ClientAuth, first.ClientCAs)
	}
	wantClientCAs := first.ClientCAs.Clone()
	first.ClientCAs.AddCert(extraCA)
	if got := snapshot.TLSConfig().ClientCAs; !got.Equal(wantClientCAs) {
		t.Fatal("TLSConfig() leaked client CA pool mutation")
	}
}

func TestCompileValidatesTLSAndClientCAMaterial(t *testing.T) {
	validCert, validKey := testServerKeyPair(t, "valid")
	_, validCAPEM := testCertificateAuthority(t, "trusted")
	tests := []struct {
		name  string
		input Input
		want  string
	}{
		{
			name: "missing protocols in strict mode",
			input: Input{Config: &config.Config{
				Apisix: config.Apisix{Ssl: config.Ssl{Enable: true}},
			}},
			want: "protocol list must not be empty",
		},
		{
			name: "TLS 1.3 cipher cannot be configured",
			input: Input{
				Config: testConfigWithTLS("TLSv1.2", "TLS_AES_128_GCM_SHA256"),
			},
			want: "TLS 1.3 cipher suite",
		},
		{
			name: "trusted CA material required",
			input: Input{Config: func() *config.Config {
				cfg := testFrontendConfig()
				cfg.Apisix.Ssl.SslTrustedCertificate = "configured.pem"
				return cfg
			}()},
			want: "trusted client CA material was not provided",
		},
		{
			name: "malformed trusted CA",
			input: Input{
				Config: testFrontendConfig(), TrustedClientCAPEM: []byte("bad CA"),
			},
			want: "trusted client CA contains no certificates",
		},
		{
			name: "malformed server key pair",
			input: Input{
				Config: testFrontendConfig(),
				SSLs: map[string]resource.SSL{
					"bad": {ID: "bad", Sni: "bad.example.test", Cert: "bad", Key: "bad", Status: 1},
				},
			},
			want: "load certificate",
		},
		{
			name: "malformed resource client CA",
			input: Input{
				Config: testFrontendConfig(),
				SSLs: map[string]resource.SSL{
					"bad": {
						ID: "bad", Sni: "bad.example.test", Cert: validCert, Key: validKey, Status: 1,
						Client: &resource.SSLClient{CA: "bad CA", Depth: 1},
					},
				},
			},
			want: "client.ca contains no certificates",
		},
		{
			name: "unsupported skip URI",
			input: Input{
				Config: testFrontendConfig(),
				SSLs: map[string]resource.SSL{
					"bad": {
						ID: "bad", Sni: "bad.example.test", Cert: validCert, Key: validKey, Status: 1,
						Client: &resource.SSLClient{
							CA: validCAPEM, Depth: 1, SkipMTLSURIRegex: []string{"/skip"},
						},
					},
				},
			},
			want: "skip_mtls_uri_regex",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Compile(test.input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Compile() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func testFrontendConfig() *config.Config {
	return testConfigWithTLS("TLSv1.2", testTLS12Cipher)
}

func testConfigWithTLS(protocols, ciphers string) *config.Config {
	return &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable: true, SslProtocols: protocols, SslCiphers: ciphers,
	}}}
}

func testServerKeyPair(t *testing.T, commonName string) (string, string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func testCertificateAuthority(t *testing.T, commonName string) (*x509.Certificate, string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return certificate, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func testCertificateCommonName(t *testing.T, certificate *tls.Certificate) string {
	t.Helper()
	if certificate == nil || len(certificate.Certificate) == 0 {
		t.Fatal("selected certificate is empty")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatalf("parse selected certificate: %v", err)
	}
	return leaf.Subject.CommonName
}
