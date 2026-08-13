package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewTransportDoesNotAutoDecompressUpstreamResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "plain upstream bytes")
	}))
	defer upstream.Close()

	client := &http.Client{Transport: NewTransport((&TransportOptionBuilder{}).Build())}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET upstream: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read upstream body: %v", err)
	}
	if string(body) != "plain upstream bytes" {
		t.Fatalf("body = %q, want raw upstream bytes", body)
	}
	if got := response.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip preserved", got)
	}
}

func TestNewTransportAppliesConnectionCaps(t *testing.T) {
	transport := NewTransport((&TransportOptionBuilder{}).
		WithMaxIdleConnections(64).
		WithMaxIdleConnectionsPerHost(16).
		WithMaxConnectionsPerHost(32).
		Build())
	if transport.MaxIdleConns != 64 || transport.MaxIdleConnsPerHost != 16 || transport.MaxConnsPerHost != 32 {
		t.Fatalf(
			"transport caps = %d/%d/%d",
			transport.MaxIdleConns,
			transport.MaxIdleConnsPerHost,
			transport.MaxConnsPerHost,
		)
	}
}

func TestNewTransportHonorsTLSVerification(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	insecure := &http.Client{Transport: NewTransport((&TransportOptionBuilder{}).WithInsecureSkipVerify(true).Build())}
	response, err := insecure.Get(upstream.URL)
	if err != nil {
		t.Fatalf("insecure transport GET: %v", err)
	}
	_ = response.Body.Close()

	verified := &http.Client{Transport: NewTransport((&TransportOptionBuilder{}).WithInsecureSkipVerify(false).Build())}
	if _, err := verified.Get(upstream.URL); err == nil {
		t.Fatal("verified transport accepted an untrusted certificate")
	}
}

func TestNewTransportSendsConfiguredTLSClientCertificate(t *testing.T) {
	serverCertificate, clientCertificate, clientCAs := testMutualTLSCertificates(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
			t.Fatalf("peer certificates = %#v, want one client certificate", r.TLS)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	upstream.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	upstream.StartTLS()
	defer upstream.Close()

	option := (&TransportOptionBuilder{}).
		WithInsecureSkipVerify(true).
		WithTLSClientCertificate(clientCertificate).
		Build()
	transport := NewTransport(option)
	client := &http.Client{Transport: transport}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("mTLS GET: %v", err)
	}
	_ = response.Body.Close()

	withoutCertificate := &http.Client{Transport: NewTransport((&TransportOptionBuilder{}).
		WithInsecureSkipVerify(true).
		Build())}
	if _, err := withoutCertificate.Get(upstream.URL); err == nil {
		t.Fatal("upstream accepted a client without a certificate")
	}
}

func TestTransportClientCertificateIsImmutableAfterBuild(t *testing.T) {
	serverCertificate, clientCertificate, clientCAs := testMutualTLSCertificates(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
			t.Fatalf("peer certificates = %#v, want one client certificate", r.TLS)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	upstream.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	upstream.StartTLS()
	defer upstream.Close()

	option := (&TransportOptionBuilder{}).
		WithInsecureSkipVerify(true).
		WithTLSClientCertificate(clientCertificate).
		Build()
	transport := NewTransport(option)
	clientCertificate.Certificate[0][0] ^= 0xff
	option.tlsClientCertificate.Certificate[0][0] ^= 0xff

	client := &http.Client{Transport: transport}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("mTLS GET after caller mutation: %v", err)
	}
	_ = response.Body.Close()
}

func testMutualTLSCertificates(t *testing.T) (tls.Certificate, tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "proxy test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(caCertificate)

	serverCertificate := testSignedCertificate(
		t,
		caCertificate,
		caKey,
		2,
		x509.ExtKeyUsageServerAuth,
		[]string{"localhost"},
	)
	clientCertificate := testSignedCertificate(t, caCertificate, caKey, 3, x509.ExtKeyUsageClientAuth, nil)
	return serverCertificate, clientCertificate, clientCAs
}

func testSignedCertificate(
	t *testing.T,
	caCertificate *x509.Certificate,
	caKey *rsa.PrivateKey,
	serial int64,
	extKeyUsage x509.ExtKeyUsage,
	dnsNames []string,
) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate certificate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "proxy test certificate"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{extKeyUsage},
		DNSNames:     dnsNames,
	}
	if len(dnsNames) > 0 {
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, caCertificate, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatalf("parse certificate key pair: %v", err)
	}
	return certificate
}
