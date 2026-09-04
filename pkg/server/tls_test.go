package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/testutil"
)

const frontendTLS12Cipher = "ECDHE-RSA-AES128-GCM-SHA256"

func TestFrontendTLSConfigUsesGenerationSelector(t *testing.T) {
	fixture := newTLSHTTPLeaseFixture(t, 400, "generation.example", nil)
	cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable: true, Listen: []config.Listen{{Port: 9443}},
		SslProtocols: "TLSv1.2", SslCiphers: frontendTLS12Cipher,
	}}}
	outer, err := buildGenerationFrontendTLSConfig(cfg, fixture.Acquire)
	if err != nil {
		t.Fatal(err)
	}
	if outer.GetCertificate != nil || outer.GetConfigForClient == nil {
		t.Fatalf("generation outer selectors present GetCertificate/GetConfigForClient = %t/%t",
			outer.GetCertificate != nil, outer.GetConfigForClient != nil)
	}
	selected, err := outer.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "generation.example"})
	if err != nil {
		t.Fatal(err)
	}
	assertTLSConfigCertificateName(t, selected, "generation.example")
	if fixture.acquires.Load() != 1 || fixture.releases.Load() != 1 {
		t.Fatalf("generation outer lease counts = %d/%d, want 1/1",
			fixture.acquires.Load(), fixture.releases.Load())
	}
}

func TestFrontendTLSSelectorUsesOneHTTPGenerationLease(t *testing.T) {
	old := newTLSHTTPLeaseFixture(t, 401, "old.example", nil)
	next := newTLSHTTPLeaseFixture(t, 402, "new.example", nil)
	source := newSwitchableHTTPLeaseSource(old)
	selector := generationFrontendTLSConfigSelector(source.Acquire)

	oldConfig, err := selector(&tls.ClientHelloInfo{ServerName: "old.example"})
	if err != nil {
		t.Fatalf("select old TLS config: %v", err)
	}
	assertTLSConfigCertificateName(t, oldConfig, "old.example")
	source.Store(next)
	nextConfig, err := selector(&tls.ClientHelloInfo{ServerName: "new.example"})
	if err != nil {
		t.Fatalf("select next TLS config: %v", err)
	}
	assertTLSConfigCertificateName(t, nextConfig, "new.example")
	if old.acquires.Load() != 1 || old.releases.Load() != 1 ||
		next.acquires.Load() != 1 || next.releases.Load() != 1 {
		t.Fatalf("TLS lease counts old=%d/%d next=%d/%d, want 1/1 each",
			old.acquires.Load(), old.releases.Load(), next.acquires.Load(), next.releases.Load())
	}
}

func TestFrontendTLSSelectorRollbackRestoresExactPredecessor(t *testing.T) {
	old := newTLSHTTPLeaseFixture(t, 403, "rollback-old.example", nil)
	next := newTLSHTTPLeaseFixture(t, 404, "rollback-next.example", nil)
	source := newSwitchableHTTPLeaseSource(old)
	selector := generationFrontendTLSConfigSelector(source.Acquire)
	source.Store(next)
	selected, err := selector(&tls.ClientHelloInfo{ServerName: "rollback-next.example"})
	if err != nil {
		t.Fatal(err)
	}
	assertTLSConfigCertificateName(t, selected, "rollback-next.example")
	source.Store(old)
	selected, err = selector(&tls.ClientHelloInfo{ServerName: "rollback-old.example"})
	if err != nil {
		t.Fatal(err)
	}
	assertTLSConfigCertificateName(t, selected, "rollback-old.example")
}

func TestFrontendTLSSelectorFailsClosedWithoutHTTPDomain(t *testing.T) {
	selector := generationFrontendTLSConfigSelector(func() (httpGenerationLease, bool) {
		return httpGenerationLease{}, false
	})
	if _, err := selector(
		&tls.ClientHelloInfo{ServerName: "missing.example"},
	); !errors.Is(
		err,
		errHTTPGenerationUnavailable,
	) {
		t.Fatalf("selector error = %v, want %v", err, errHTTPGenerationUnavailable)
	}
}

func TestFrontendTLSSelectorReleasesUnavailableSnapshotLease(t *testing.T) {
	fixture := newCountedHTTPLeaseFixture(t, 407)
	selector := generationFrontendTLSConfigSelector(fixture.Acquire)
	if _, err := selector(
		&tls.ClientHelloInfo{ServerName: "disabled.example"},
	); !errors.Is(
		err,
		errHTTPGenerationUnavailable,
	) {
		t.Fatalf("selector error = %v, want %v", err, errHTTPGenerationUnavailable)
	}
	if got := fixture.releases.Load(); got != 1 {
		t.Fatalf("release count for snapshot without TLS = %d, want 1", got)
	}
}

func TestFrontendTLSSelectorNilReleaseFailsClosedWithoutPanic(t *testing.T) {
	fixture := newTLSHTTPLeaseFixture(t, 408, "nil-release.example", nil)
	lease, ok := fixture.Acquire()
	if !ok {
		t.Fatal("fixture lease unavailable")
	}
	release := lease.Release
	lease.Release = nil
	t.Cleanup(release)
	selector := generationFrontendTLSConfigSelector(func() (httpGenerationLease, bool) { return lease, true })
	if _, err := selector(
		&tls.ClientHelloInfo{ServerName: "nil-release.example"},
	); !errors.Is(
		err,
		errHTTPGenerationUnavailable,
	) {
		t.Fatalf("selector error = %v, want %v", err, errHTTPGenerationUnavailable)
	}
}

func TestFrontendTLSSelectorUsesSnapshotClientCAAndDepth(t *testing.T) {
	clientCA := newFrontendTestCA(t)
	fixture := newTLSHTTPLeaseFixture(t, 405, "mtls.example", &resource.SSLClient{
		CA: string(clientCA.certPEM), Depth: 1,
	})
	selector := generationFrontendTLSConfigSelector(fixture.Acquire)
	selected, err := selector(&tls.ClientHelloInfo{ServerName: "mtls.example"})
	if err != nil {
		t.Fatal(err)
	}
	if selected.ClientAuth != tls.RequireAndVerifyClientCert || selected.ClientCAs == nil {
		t.Fatalf("selected client auth/CAs = %v/%v", selected.ClientAuth, selected.ClientCAs)
	}
	if selected.VerifyConnection == nil {
		t.Fatal("selected generation TLS config omitted client certificate depth enforcement")
	}
}

func TestFrontendTLSSelectorReleasesLeaseOnSelectionError(t *testing.T) {
	fixture := newTLSHTTPLeaseFixture(t, 406, "known.example", nil)
	selector := generationFrontendTLSConfigSelector(fixture.Acquire)
	if _, err := selector(&tls.ClientHelloInfo{ServerName: "unknown.example"}); err == nil {
		t.Fatal("unknown SNI selection error = nil")
	}
	if got := fixture.releases.Load(); got != 1 {
		t.Fatalf("release count after selection error = %d, want 1", got)
	}
}

func newTLSHTTPLeaseFixture(
	t *testing.T,
	revision uint64,
	serverName string,
	client *resource.SSLClient,
) *countedHTTPLeaseFixture {
	t.Helper()
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	effective := &config.EffectiveConfig{
		Config: config.Config{
			Apisix: config.Apisix{Ssl: config.Ssl{
				Enable: true, Listen: []config.Listen{{Port: 9443}},
				SslProtocols: "TLSv1.2", SslCiphers: frontendTLS12Cipher,
			}},
		},
	}
	materializer := &ownerTestMaterializer{
		delegate: testutil.NewSecretMaterializer(ownerTestResolver{}, catalog),
	}
	factory, err := compiler.NewWorkerCompilerFactory(
		effective,
		materializer,
		compiler.WorkerRuntimeObservers{Cluster: proxy.NopClusterObserver{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Errorf("WorkerCompilerFactory.Close() error = %v", err)
		}
	})
	certificate := frontendHandshakeCertificate(
		t, serverName, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	)
	certificatePEM := make([]byte, 0)
	for _, der := range certificate.Certificate {
		certificatePEM = append(certificatePEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	privateKey, ok := certificate.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("private key type = %T, want *rsa.PrivateKey", certificate.PrivateKey)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	sslValue, err := json.Marshal(resource.SSL{
		ID: serverName, Sni: serverName, Cert: string(certificatePEM), Key: string(keyPEM), Client: client, Status: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	desired, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: generation.ResourceKey{Kind: "ssls", ID: serverName}, Value: bytes.Clone(sslValue),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: desired.Revision(),
		DesiredDigest:   desired.Digest(),
		Cursor:          generation.ProviderCursor{Provider: "generation-tls-test", Revision: "1"},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	prepared, err := factory.PrepareGeneration(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := newGenerationOwner(prepared)
	owner.activateDomains(ownerDomainHTTP)
	return &countedHTTPLeaseFixture{owner: owner}
}

func assertTLSConfigCertificateName(t *testing.T, selected *tls.Config, want string) {
	t.Helper()
	if selected == nil || len(selected.Certificates) != 1 || len(selected.Certificates[0].Certificate) == 0 {
		t.Fatalf("selected TLS config certificate = %#v", selected)
	}
	certificate, err := x509.ParseCertificate(selected.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if certificate.Subject.CommonName != want {
		t.Fatalf("selected certificate common name = %q, want %q", certificate.Subject.CommonName, want)
	}
}

func mustGenerationFrontendTLSConfig(t testing.TB, cfg *config.Config) *tls.Config {
	t.Helper()
	tlsConfig, err := buildGenerationFrontendTLSConfig(cfg, nil)
	if err != nil {
		t.Fatalf("buildGenerationFrontendTLSConfig() error = %v", err)
	}
	return tlsConfig
}

func TestFrontendTLSProtocolConfigStrict(t *testing.T) {
	tests := []struct {
		name      string
		protocols string
		wantErr   string
		wantMin   uint16
		wantMax   uint16
	}{
		{name: "tls12", protocols: "TLSv1.2", wantMin: tls.VersionTLS12, wantMax: tls.VersionTLS12},
		{name: "tls13", protocols: "TLSv1.3", wantMin: tls.VersionTLS13, wantMax: tls.VersionTLS13},
		{name: "both", protocols: "TLSv1.2 TLSv1.3", wantMin: tls.VersionTLS12, wantMax: tls.VersionTLS13},
		{name: "empty", protocols: "", wantErr: "protocol"},
		{name: "duplicate", protocols: "TLSv1.2 TLSv1.2", wantErr: "duplicate"},
		{name: "unknown", protocols: "TLSv1.1", wantErr: "unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
				Enable:       true,
				SslProtocols: test.protocols,
				SslCiphers:   frontendTLS12Cipher,
			}}}
			if test.protocols == "TLSv1.3" {
				cfg.Apisix.Ssl.SslCiphers = ""
			}

			got, err := buildGenerationFrontendTLSConfig(cfg, nil)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.wantErr) {
					t.Fatalf("generation frontend TLS config error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("generation frontend TLS config error = %v", err)
			}
			if got.MinVersion != test.wantMin || got.MaxVersion != test.wantMax {
				t.Fatalf("TLS versions = %d/%d, want %d/%d", got.MinVersion, got.MaxVersion, test.wantMin, test.wantMax)
			}
		})
	}
}

func TestFrontendTLSCipherConfigStrict(t *testing.T) {
	tests := []struct {
		name    string
		ciphers string
		wantErr string
	}{
		{name: "supported", ciphers: frontendTLS12Cipher},
		{name: "empty", ciphers: "", wantErr: "cipher"},
		{name: "empty segment", ciphers: frontendTLS12Cipher + "::ECDHE-RSA-AES256-GCM-SHA384", wantErr: "empty"},
		{name: "unsupported dhe", ciphers: "DHE-RSA-AES128-GCM-SHA256", wantErr: "unsupported"},
		{name: "tls13 suite", ciphers: "TLS_AES_128_GCM_SHA256", wantErr: "TLS 1.3"},
		{name: "unknown", ciphers: "NOT-A-CIPHER", wantErr: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
				Enable:       true,
				SslProtocols: "TLSv1.2",
				SslCiphers:   test.ciphers,
			}}}
			_, err := buildGenerationFrontendTLSConfig(cfg, nil)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("generation frontend TLS config error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.wantErr)) {
				t.Fatalf("generation frontend TLS config error = %v, want %q", err, test.wantErr)
			}
		})
	}

	cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable: true, SslProtocols: "TLSv1.3", SslCiphers: frontendTLS12Cipher,
	}}}
	if _, err := buildGenerationFrontendTLSConfig(cfg, nil); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "tls 1.2") {
		t.Fatalf("TLS 1.3-only cipher policy error = %v, want TLS 1.2 explanation", err)
	}
}

func TestFrontendTLSSessionTicketsIgnoreOutboundTrustedCA(t *testing.T) {
	ca := newFrontendTestCA(t)
	caPath := filepath.Join(t.TempDir(), "client-ca.pem")
	if err := os.WriteFile(caPath, ca.certPEM, 0o600); err != nil {
		t.Fatalf("write client CA: %v", err)
	}
	cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:                true,
		SslProtocols:          "TLSv1.2",
		SslCiphers:            frontendTLS12Cipher,
		SslSessionTickets:     true,
		SslTrustedCertificate: caPath,
	}}}

	tlsConfig, err := buildGenerationFrontendTLSConfig(cfg, nil)
	if err != nil {
		t.Fatalf("generation frontend TLS config error = %v", err)
	}
	if tlsConfig.SessionTicketsDisabled {
		t.Fatal("SessionTicketsDisabled = true, want false when tickets are enabled")
	}
	if tlsConfig.ClientAuth != tls.NoClientCert {
		t.Fatalf("ClientAuth = %v, want NoClientCert", tlsConfig.ClientAuth)
	}
	if tlsConfig.ClientCAs != nil {
		t.Fatal("ClientCAs is configured from ssl_trusted_certificate, want outbound-only trust")
	}
}

func TestFrontendTLSSessionTicketsControlResumption(t *testing.T) {
	serverCertificate := frontendHandshakeCertificate(
		t,
		"server.example.test",
		nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	)
	for _, test := range []struct {
		name        string
		tickets     bool
		wantResumed bool
	}{
		{name: "enabled", tickets: true, wantResumed: true},
		{name: "disabled", tickets: false, wantResumed: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
				Enable:            true,
				SslProtocols:      "TLSv1.2",
				SslCiphers:        frontendTLS12Cipher,
				SslSessionTickets: test.tickets,
			}}}
			serverConfig, err := buildGenerationFrontendTLSConfig(cfg, nil)
			if err != nil {
				t.Fatalf("generation frontend TLS config error = %v", err)
			}
			serverConfig.GetConfigForClient = nil
			serverConfig.Certificates = []tls.Certificate{serverCertificate}
			clientConfig := &tls.Config{
				MinVersion:         tls.VersionTLS12,
				MaxVersion:         tls.VersionTLS12,
				ServerName:         "server.example.test",
				InsecureSkipVerify: true,
				ClientSessionCache: tls.NewLRUClientSessionCache(1),
			}

			first, clientErr, serverErr := frontendTLSHandshake(serverConfig, clientConfig)
			if clientErr != nil || serverErr != nil {
				t.Fatalf("first TLS handshake errors = client %v/server %v", clientErr, serverErr)
			}
			if first.DidResume {
				t.Fatal("first TLS handshake unexpectedly resumed a session")
			}
			second, clientErr, serverErr := frontendTLSHandshake(serverConfig, clientConfig)
			if clientErr != nil || serverErr != nil {
				t.Fatalf("second TLS handshake errors = client %v/server %v", clientErr, serverErr)
			}
			if second.DidResume != test.wantResumed {
				t.Fatalf("second DidResume = %t, want %t", second.DidResume, test.wantResumed)
			}
		})
	}
}

func TestFrontendTLSHandshakeEnforcesConfiguredProtocols(t *testing.T) {
	serverCertificate := frontendHandshakeCertificate(
		t,
		"server.example.test",
		nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	)
	for _, test := range []struct {
		name         string
		serverConfig string
		clientConfig uint16
		wantSuccess  bool
	}{
		{name: "tls12 accepted", serverConfig: "TLSv1.2", clientConfig: tls.VersionTLS12, wantSuccess: true},
		{name: "tls13 rejected by tls12", serverConfig: "TLSv1.2", clientConfig: tls.VersionTLS13},
		{name: "tls13 accepted", serverConfig: "TLSv1.3", clientConfig: tls.VersionTLS13, wantSuccess: true},
		{name: "tls12 rejected by tls13", serverConfig: "TLSv1.3", clientConfig: tls.VersionTLS12},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
				Enable:       true,
				SslProtocols: test.serverConfig,
				SslCiphers:   frontendTLS12Cipher,
			}}}
			if test.serverConfig == "TLSv1.3" {
				cfg.Apisix.Ssl.SslCiphers = ""
			}
			serverConfig, err := buildGenerationFrontendTLSConfig(cfg, nil)
			if err != nil {
				t.Fatalf("generation frontend TLS config error = %v", err)
			}
			serverConfig.GetConfigForClient = nil
			serverConfig.Certificates = []tls.Certificate{serverCertificate}
			clientConfig := &tls.Config{
				MinVersion:         test.clientConfig,
				MaxVersion:         test.clientConfig,
				InsecureSkipVerify: true,
			}
			_, clientErr, serverErr := frontendTLSHandshake(serverConfig, clientConfig)
			if test.wantSuccess && (clientErr != nil || serverErr != nil) {
				t.Fatalf("TLS handshake errors = client %v/server %v, want success", clientErr, serverErr)
			}
			if !test.wantSuccess && clientErr == nil && serverErr == nil {
				t.Fatal("TLS handshake succeeded across configured protocol boundary")
			}
		})
	}
}

func TestFrontendTLSHandshakeSelectsConfiguredCipher(t *testing.T) {
	cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:       true,
		SslProtocols: "TLSv1.2",
		SslCiphers:   frontendTLS12Cipher,
	}}}
	serverConfig, err := buildGenerationFrontendTLSConfig(cfg, nil)
	if err != nil {
		t.Fatalf("generation frontend TLS config error = %v", err)
	}
	serverConfig.GetConfigForClient = nil
	serverConfig.Certificates = []tls.Certificate{
		frontendHandshakeCertificate(t, "server.example.test", nil, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}),
	}
	clientConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
		CipherSuites:       append([]uint16(nil), serverConfig.CipherSuites...),
		InsecureSkipVerify: true,
	}
	state, clientErr, serverErr := frontendTLSHandshake(serverConfig, clientConfig)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("TLS handshake errors = client %v/server %v", clientErr, serverErr)
	}
	if !slices.Contains(serverConfig.CipherSuites, state.CipherSuite) {
		t.Fatalf("negotiated cipher 0x%x not in configured suites %v", state.CipherSuite, serverConfig.CipherSuites)
	}
}

func TestFrontendTLSHandshakeRequiresTrustedClientCertificate(t *testing.T) {
	ca := newFrontendTestCA(t)
	clientCertificate := frontendHandshakeCertificate(
		t,
		"client.example.test",
		&ca,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	fixture := newTLSHTTPLeaseFixture(t, 409, "server.example.test", &resource.SSLClient{
		CA: string(ca.certPEM), Depth: 1,
	})
	serverConfig, err := generationFrontendTLSConfigSelector(fixture.Acquire)(
		&tls.ClientHelloInfo{ServerName: "server.example.test"},
	)
	if err != nil {
		t.Fatalf("select generation frontend TLS config: %v", err)
	}
	serverConfig.GetConfigForClient = nil

	missingClient := &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, InsecureSkipVerify: true}
	_, clientErr, serverErr := frontendTLSHandshake(serverConfig, missingClient)
	if clientErr == nil && serverErr == nil {
		t.Fatal("TLS handshake without a client certificate succeeded")
	}

	trustedClient := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{clientCertificate},
	}
	_, clientErr, serverErr = frontendTLSHandshake(serverConfig, trustedClient)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("TLS handshake with trusted client errors = client %v/server %v", clientErr, serverErr)
	}
}

func frontendTLSHandshake(serverConfig, clientConfig *tls.Config) (tls.ConnectionState, error, error) {
	serverConn, clientConn := net.Pipe()
	deadline := time.Now().Add(2 * time.Second)
	_ = serverConn.SetDeadline(deadline)
	_ = clientConn.SetDeadline(deadline)
	server := tls.Server(serverConn, serverConfig)
	client := tls.Client(clientConn, clientConfig)
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Handshake() }()
	clientErr := client.Handshake()
	serverErr := <-serverDone
	_ = serverConn.Close()
	_ = clientConn.Close()
	return client.ConnectionState(), clientErr, serverErr
}

type frontendTestCA struct {
	cert    *x509.Certificate
	key     *rsa.PrivateKey
	certPEM []byte
}

func newFrontendTestCA(t *testing.T) frontendTestCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          newSerialNumber(t),
		Subject:               pkix.Name{CommonName: "frontend-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return frontendTestCA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

func frontendHandshakeCertificate(
	t *testing.T,
	commonName string,
	ca *frontendTestCA,
	usages []x509.ExtKeyUsage,
) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate certificate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: newSerialNumber(t),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usages,
		DNSNames:     []string{commonName},
	}
	parent := template
	var signer crypto.Signer = key
	if ca != nil {
		parent = ca.cert
		signer = ca.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if ca != nil {
		chain = append(chain, ca.certPEM...)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certificate, err := tls.X509KeyPair(chain, keyPEM)
	if err != nil {
		t.Fatalf("parse certificate key pair: %v", err)
	}
	return certificate
}

func newSerialNumber(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate certificate serial: %v", err)
	}
	return serial
}

func TestFrontendTLSConfigDoesNotReadOutboundTrustedCA(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "invalid-ca.pem")
	if err := os.WriteFile(caPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write invalid CA: %v", err)
	}
	cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:                true,
		SslProtocols:          "TLSv1.3",
		SslTrustedCertificate: caPath,
	}}}
	tlsConfig, err := buildGenerationFrontendTLSConfig(cfg, nil)
	if err != nil {
		t.Fatalf("generation frontend TLS config error = %v", err)
	}
	if tlsConfig.ClientAuth != tls.NoClientCert || tlsConfig.ClientCAs != nil {
		t.Fatalf(
			"client authentication = %v/%v, want outbound-only trust ignored",
			tlsConfig.ClientAuth,
			tlsConfig.ClientCAs,
		)
	}
}

func TestStartHTTPListenersBuildsTLSBeforeBinding(t *testing.T) {
	effective := &config.EffectiveConfig{Config: config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:       true,
		Listen:       []config.Listen{{Port: 9443}},
		SslProtocols: "TLSv1.1",
		SslCiphers:   frontendTLS12Cipher,
	}}}}
	engine := &newServerTestEngine{}
	server := &Server{
		staticConfig: effective,
		addrs:        []string{"127.0.0.1:0"},
		server:       &http.Server{},
		engine:       engine,
	}
	_, err := server.startHTTPListenerRuntime(context.Background())
	if err == nil || !strings.Contains(err.Error(), "build frontend TLS config") {
		t.Fatalf("startHTTPListenerRuntime() error = %v, want TLS build context", err)
	}
	if len(server.listeners) != 0 {
		t.Fatalf("listeners retained after TLS policy rejection: %d", len(server.listeners))
	}
}
