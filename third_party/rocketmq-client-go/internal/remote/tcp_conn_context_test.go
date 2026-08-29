package remote

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

func TestTLSHandshakeHonorsContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = initConn(ctx, listener.Addr().String(), &RemotingClientConfig{TcpOption: TcpOption{
		ConnectionTimeout: 5 * time.Second,
		UseTls:            true,
	}})
	if err == nil {
		t.Fatal("TLS handshake unexpectedly succeeded against a silent peer")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("TLS handshake ignored context: elapsed %s", elapsed)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatal("silent TLS peer did not accept the connection")
	}
}

func TestTLSVerificationUsesRootsAndAddressServerName(t *testing.T) {
	listener, cert := startTestTLSServer(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	roots := x509.NewCertPool()
	roots.AddCert(cert)

	conn, err := initConn(context.Background(), listener.Addr().String(), &RemotingClientConfig{
		TcpOption: TcpOption{
			ConnectionTimeout: 2 * time.Second,
			UseTls:            true,
			TLSVerify:         true,
			TLSRootCAs:        roots,
		},
	})
	if err != nil {
		t.Fatalf("verified TLS connection failed: %v", err)
	}
	if err := conn.destroy(); err != nil {
		t.Fatalf("close verified TLS connection: %v", err)
	}
}

func TestTLSVerificationRejectsUnknownAuthority(t *testing.T) {
	listener, _ := startTestTLSServer(t, nil, []net.IP{net.ParseIP("127.0.0.1")})

	_, err := initConn(context.Background(), listener.Addr().String(), &RemotingClientConfig{
		TcpOption: TcpOption{
			ConnectionTimeout: 2 * time.Second,
			UseTls:            true,
			TLSVerify:         true,
		},
	})
	if err == nil {
		t.Fatal("strict TLS connection accepted an untrusted certificate")
	}
}

func TestTLSVerificationRejectsWrongServerName(t *testing.T) {
	listener, cert := startTestTLSServer(t, []string{"broker.example"}, nil)
	roots := x509.NewCertPool()
	roots.AddCert(cert)

	_, err := initConn(context.Background(), listener.Addr().String(), &RemotingClientConfig{
		TcpOption: TcpOption{
			ConnectionTimeout: 2 * time.Second,
			UseTls:            true,
			TLSVerify:         true,
			TLSRootCAs:        roots,
		},
	})
	if err == nil {
		t.Fatal("strict TLS connection accepted a certificate for the wrong server name")
	}
}

func TestTLSVerificationDisabledSkipsVerification(t *testing.T) {
	listener, _ := startTestTLSServer(t, []string{"broker.example"}, nil)

	conn, err := initConn(context.Background(), listener.Addr().String(), &RemotingClientConfig{
		TcpOption: TcpOption{
			ConnectionTimeout: 2 * time.Second,
			UseTls:            true,
			TLSVerify:         false,
		},
	})
	if err != nil {
		t.Fatalf("compatibility TLS connection failed: %v", err)
	}
	if err := conn.destroy(); err != nil {
		t.Fatalf("close compatibility TLS connection: %v", err)
	}
}

func startTestTLSServer(t *testing.T, dnsNames []string, ipAddresses []net.IP) (net.Listener, *x509.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "RocketMQ test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "RocketMQ test server"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsNames,
		IPAddresses:  ipAddresses,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(
		rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x509.ParseCertificate(serverDER); err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(conn net.Conn) {
				if tlsConn, ok := conn.(*tls.Conn); ok {
					_ = tlsConn.Handshake()
				}
				_ = conn.Close()
			}(conn)
		}
	}()
	return listener, caCert
}
