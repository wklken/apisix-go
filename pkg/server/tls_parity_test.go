package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func TestFrontendTLSHandshakeUsesPerResourceClientCA(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/resource-mtls.db", events, testutil.DataEncryptionService(false, nil))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previousStore)
		_ = storage.Stop()
	})

	clientCA := newFrontendTestCA(t)
	serverCertificate := frontendHandshakeCertificate(
		t,
		"resource-mtls.example.test",
		nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	)
	privateKey, ok := serverCertificate.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("server certificate private key type = %T, want *rsa.PrivateKey", serverCertificate.PrivateKey)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: serverCertificate.Certificate[0],
	})
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	sslValue, err := json.Marshal(resource.SSL{
		ID:   "resource-mtls",
		Sni:  "resource-mtls.example.test",
		Cert: string(certificatePEM),
		Key:  string(privateKeyPEM),
		Client: &resource.SSLClient{
			CA: string(clientCA.certPEM),
		},
	})
	if err != nil {
		t.Fatalf("marshal SSL resource: %v", err)
	}
	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/ssls/resource-mtls")
	event.Value = sslValue
	events <- event
	if err := storage.Sync(); err != nil {
		t.Fatalf("SSL storage sync: %v", err)
	}

	cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:       true,
		SslProtocols: "TLSv1.2",
		SslCiphers:   frontendTLS12Cipher,
	}}}
	serverConfig, err := buildFrontendTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildFrontendTLSConfig() error = %v", err)
	}

	missingClient := &tls.Config{
		MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
		ServerName: "resource-mtls.example.test", InsecureSkipVerify: true,
	}
	_, clientErr, serverErr := frontendTLSHandshake(serverConfig, missingClient)
	if clientErr == nil && serverErr == nil {
		t.Fatal("TLS handshake without a resource client certificate succeeded")
	}

	trustedClientCertificate := frontendHandshakeCertificate(
		t,
		"resource-client.example.test",
		&clientCA,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	trustedClient := &tls.Config{
		MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
		ServerName: "resource-mtls.example.test", InsecureSkipVerify: true,
		Certificates: []tls.Certificate{trustedClientCertificate},
	}
	_, clientErr, serverErr = frontendTLSHandshake(serverConfig, trustedClient)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("TLS handshake with trusted resource client errors = client %v/server %v", clientErr, serverErr)
	}

	intermediateCA := newFrontendIntermediateCA(t, clientCA)
	deepClientCertificate := frontendHandshakeCertificate(
		t,
		"deep-resource-client.example.test",
		&intermediateCA,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	)
	deepClient := &tls.Config{
		MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
		ServerName: "resource-mtls.example.test", InsecureSkipVerify: true,
		Certificates: []tls.Certificate{deepClientCertificate},
	}
	_, clientErr, serverErr = frontendTLSHandshake(serverConfig, deepClient)
	if clientErr == nil && serverErr == nil {
		t.Fatal("TLS handshake exceeding resource client depth succeeded")
	}
}

func newFrontendIntermediateCA(t *testing.T, parent frontendTestCA) frontendTestCA {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate intermediate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          newSerialNumber(t),
		Subject:               pkix.Name{CommonName: "frontend-test-intermediate-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(
		rand.Reader,
		template,
		parent.cert,
		&privateKey.PublicKey,
		parent.key,
	)
	if err != nil {
		t.Fatalf("create intermediate CA certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse intermediate CA certificate: %v", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certificatePEM = append(certificatePEM, parent.certPEM...)
	return frontendTestCA{cert: certificate, key: privateKey, certPEM: certificatePEM}
}
