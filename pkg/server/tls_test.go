package server

import (
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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
)

const frontendTLS12Cipher = "ECDHE-RSA-AES128-GCM-SHA256"

func mustFrontendTLSConfig(t testing.TB) *tls.Config {
	t.Helper()
	tlsConfig, err := buildFrontendTLSConfig()
	if err != nil {
		t.Fatalf("buildFrontendTLSConfig() error = %v", err)
	}
	return tlsConfig
}

func TestFrontendTLSProtocolConfigStrict(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })

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
			config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
				Enable:       true,
				SslProtocols: test.protocols,
				SslCiphers:   frontendTLS12Cipher,
			}}}
			if test.protocols == "TLSv1.3" {
				config.GlobalConfig.Apisix.Ssl.SslCiphers = ""
			}

			got, err := buildFrontendTLSConfig()
			if test.wantErr != "" {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.wantErr) {
					t.Fatalf("buildFrontendTLSConfig() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildFrontendTLSConfig() error = %v", err)
			}
			if got.MinVersion != test.wantMin || got.MaxVersion != test.wantMax {
				t.Fatalf("TLS versions = %d/%d, want %d/%d", got.MinVersion, got.MaxVersion, test.wantMin, test.wantMax)
			}
		})
	}
}

func TestFrontendTLSCipherConfigStrict(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })

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
			config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
				Enable:       true,
				SslProtocols: "TLSv1.2",
				SslCiphers:   test.ciphers,
			}}}
			_, err := buildFrontendTLSConfig()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("buildFrontendTLSConfig() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.wantErr)) {
				t.Fatalf("buildFrontendTLSConfig() error = %v, want %q", err, test.wantErr)
			}
		})
	}

	config.GlobalConfig.Apisix.Ssl.SslProtocols = "TLSv1.3"
	config.GlobalConfig.Apisix.Ssl.SslCiphers = frontendTLS12Cipher
	if _, err := buildFrontendTLSConfig(); err == nil || !strings.Contains(strings.ToLower(err.Error()), "tls 1.2") {
		t.Fatalf("TLS 1.3-only cipher policy error = %v, want TLS 1.2 explanation", err)
	}
}

func TestFrontendTLSSessionTicketsAndClientCA(t *testing.T) {
	ca := newFrontendTestCA(t)
	caPath := filepath.Join(t.TempDir(), "client-ca.pem")
	if err := os.WriteFile(caPath, ca.certPEM, 0o600); err != nil {
		t.Fatalf("write client CA: %v", err)
	}
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:                true,
		SslProtocols:          "TLSv1.2",
		SslCiphers:            frontendTLS12Cipher,
		SslSessionTickets:     true,
		SslTrustedCertificate: caPath,
	}}}

	tlsConfig, err := buildFrontendTLSConfig()
	if err != nil {
		t.Fatalf("buildFrontendTLSConfig() error = %v", err)
	}
	if tlsConfig.SessionTicketsDisabled {
		t.Fatal("SessionTicketsDisabled = true, want false when tickets are enabled")
	}
	if tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", tlsConfig.ClientAuth)
	}
	if tlsConfig.ClientCAs == nil {
		t.Fatal("ClientCAs = nil, want configured client CA pool")
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
			previous := config.GlobalConfig
			t.Cleanup(func() { config.GlobalConfig = previous })
			config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
				Enable:            true,
				SslProtocols:      "TLSv1.2",
				SslCiphers:        frontendTLS12Cipher,
				SslSessionTickets: test.tickets,
			}}}
			serverConfig, err := buildFrontendTLSConfig()
			if err != nil {
				t.Fatalf("buildFrontendTLSConfig() error = %v", err)
			}
			serverConfig.GetCertificate = nil
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

func TestFrontendTLSHandshakeSelectsExactWildcardAndFallbackSNI(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/frontend-sni.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previousStore)
		_ = storage.Stop()
	})

	putCertificate := func(id string, snis []string, commonName string) {
		certificate := frontendHandshakeCertificate(
			t,
			commonName,
			nil,
			[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		)
		privateKey, ok := certificate.PrivateKey.(*rsa.PrivateKey)
		if !ok {
			t.Fatalf("certificate private key type = %T, want *rsa.PrivateKey", certificate.PrivateKey)
		}
		value, err := json.Marshal(resource.SSL{
			ID:   id,
			Snis: snis,
			Cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})),
			Key: string(
				pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}),
			),
			Status: 1,
		})
		if err != nil {
			t.Fatalf("marshal SSL resource: %v", err)
		}
		event := store.NewEvent()
		event.Type = store.EventTypePut
		event.Key = []byte("/apisix/ssls/" + id)
		event.Value = value
		events <- event
	}
	putCertificate("wildcard", []string{"*.example.test"}, "wildcard-certificate")
	putCertificate("exact", []string{"api.example.test"}, "exact-certificate")
	putCertificate("fallback", []string{"fallback.example.test"}, "fallback-certificate")
	if err := storage.Sync(); err != nil {
		t.Fatalf("SSL storage sync: %v", err)
	}

	previousConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previousConfig })
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:       true,
		SslProtocols: "TLSv1.2",
		SslCiphers:   frontendTLS12Cipher,
		FallbackSNI:  "fallback.example.test",
	}}}
	serverConfig, err := buildFrontendTLSConfig()
	if err != nil {
		t.Fatalf("buildFrontendTLSConfig() error = %v", err)
	}
	for index, test := range []struct {
		serverName string
		wantName   string
	}{
		{serverName: "api.example.test", wantName: "exact-certificate"},
		{serverName: "child.example.test", wantName: "wildcard-certificate"},
		{serverName: "", wantName: "fallback-certificate"},
	} {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			clientConfig := &tls.Config{
				MinVersion:         tls.VersionTLS12,
				MaxVersion:         tls.VersionTLS12,
				ServerName:         test.serverName,
				InsecureSkipVerify: true,
			}
			state, clientErr, serverErr := frontendTLSHandshake(serverConfig, clientConfig)
			if clientErr != nil || serverErr != nil {
				t.Fatalf("TLS handshake errors = client %v/server %v", clientErr, serverErr)
			}
			if len(state.PeerCertificates) == 0 || state.PeerCertificates[0].Subject.CommonName != test.wantName {
				t.Fatalf("peer certificate = %#v, want common name %q", state.PeerCertificates, test.wantName)
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
			previous := config.GlobalConfig
			t.Cleanup(func() { config.GlobalConfig = previous })
			config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
				Enable:       true,
				SslProtocols: test.serverConfig,
				SslCiphers:   frontendTLS12Cipher,
			}}}
			if test.serverConfig == "TLSv1.3" {
				config.GlobalConfig.Apisix.Ssl.SslCiphers = ""
			}
			serverConfig, err := buildFrontendTLSConfig()
			if err != nil {
				t.Fatalf("buildFrontendTLSConfig() error = %v", err)
			}
			serverConfig.GetCertificate = nil
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
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:       true,
		SslProtocols: "TLSv1.2",
		SslCiphers:   frontendTLS12Cipher,
	}}}
	serverConfig, err := buildFrontendTLSConfig()
	if err != nil {
		t.Fatalf("buildFrontendTLSConfig() error = %v", err)
	}
	serverConfig.GetCertificate = nil
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
	caPath := filepath.Join(t.TempDir(), "client-ca.pem")
	if err := os.WriteFile(caPath, ca.certPEM, 0o600); err != nil {
		t.Fatalf("write client CA: %v", err)
	}
	serverCertificate := frontendHandshakeCertificate(
		t,
		"server.example.test",
		&ca,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	)
	clientCertificate := frontendHandshakeCertificate(
		t,
		"client.example.test",
		&ca,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:                true,
		SslProtocols:          "TLSv1.2",
		SslCiphers:            frontendTLS12Cipher,
		SslTrustedCertificate: caPath,
	}}}
	serverConfig, err := buildFrontendTLSConfig()
	if err != nil {
		t.Fatalf("buildFrontendTLSConfig() error = %v", err)
	}
	serverConfig.GetCertificate = nil
	serverConfig.Certificates = []tls.Certificate{serverCertificate}

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

func TestFrontendTLSConfigRejectsMalformedTrustedCA(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	caPath := filepath.Join(t.TempDir(), "invalid-ca.pem")
	if err := os.WriteFile(caPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write invalid CA: %v", err)
	}
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:                true,
		SslProtocols:          "TLSv1.3",
		SslTrustedCertificate: caPath,
	}}}
	_, err := buildFrontendTLSConfig()
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "ca") {
		t.Fatalf("buildFrontendTLSConfig() error = %v, want CA parsing context", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("malformed CA error unexpectedly reported missing file")
	}
}

func TestStartHTTPListenersBuildsTLSBeforeBinding(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:       true,
		Listen:       []config.Listen{{Port: 9443}},
		SslProtocols: "TLSv1.1",
		SslCiphers:   frontendTLS12Cipher,
	}}}
	server := &Server{
		addr:   "127.0.0.1:0",
		addrs:  []string{"127.0.0.1:0"},
		server: &http.Server{},
	}
	err := server.startHTTPListeners(context.Background())
	if err == nil || !strings.Contains(err.Error(), "build frontend TLS config") {
		t.Fatalf("startHTTPListeners() error = %v, want TLS build context", err)
	}
	if len(server.listeners) != 0 {
		t.Fatalf("listeners retained after TLS policy rejection: %d", len(server.listeners))
	}
}
