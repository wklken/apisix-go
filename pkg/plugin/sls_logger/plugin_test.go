package sls_logger

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestPostInitSetsSLSDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
	})

	if p.config.Timeout != 5000 {
		t.Fatalf("timeout = %d, want 5000", p.config.Timeout)
	}
	if p.addr != "127.0.0.1:10009" {
		t.Fatalf("addr = %q, want 127.0.0.1:10009", p.addr)
	}
}

func TestBuildMessageUsesRFC5424Shape(t *testing.T) {
	p := newTestPlugin(t, Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
	})

	message := p.buildMessage(map[string]any{
		"path":   "/orders",
		"status": 201,
	})

	if !strings.HasPrefix(message, "<46>1 ") {
		t.Fatalf("message = %q, want RFC5424 SYSLOG/INFO prefix <46>1", message)
	}
	wantStructured := `[logservice project="project-a" logstore="store-a" access-key-id="id" access-key-secret="secret"]`
	if !strings.Contains(message, wantStructured) {
		t.Fatalf("message = %q, want SLS structured data %s", message, wantStructured)
	}
	if !strings.Contains(message, `"path":"/orders"`) {
		t.Fatalf("message = %q, want JSON log entry", message)
	}
	if !strings.HasSuffix(message, "\n") {
		t.Fatalf("message = %q, want newline suffix", message)
	}
}

func TestSendWritesTLSMessage(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
		Timeout:         1000,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		if !strings.Contains(message, `[logservice project="project-a" logstore="store-a"`) {
			t.Fatalf("message = %q, want SLS structured data", message)
		}
		if !strings.Contains(message, `"path":"/orders"`) {
			t.Fatalf("message = %q, want JSON log payload", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS log message")
	}
}

func startTLSServer(t *testing.T) (string, <-chan string) {
	t.Helper()

	cert := testCertificate(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("listen tls: %v", err)
	}
	t.Cleanup(func() {
		ln.Close()
	})

	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err == nil {
			received <- string(buf[:n])
		}
	}()

	return ln.Addr().String(), received
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	return cert
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var n int
	for _, r := range value {
		if r < '0' || r > '9' {
			t.Fatalf("invalid integer %q", value)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
