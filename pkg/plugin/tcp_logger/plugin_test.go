package tcp_logger

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSendWritesTCPMessage(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{Host: host, Port: mustAtoi(t, port), Timeout: 1000})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		if !strings.Contains(message, `"path":"/orders"`) {
			t.Fatalf("message = %q, want JSON log entry", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tcp log message")
	}
}

func TestSendWritesTLSMessageWithServerName(t *testing.T) {
	addr, received, serverNames := startTLSServer(t)
	host, port := splitAddr(t, addr)
	serverName := "logs.example.test"

	p := newTestPlugin(t, Config{
		Host:       host,
		Port:       mustAtoi(t, port),
		TLS:        true,
		TLSOptions: &serverName,
		Timeout:    1000,
	})
	p.Send(map[string]any{"path": "/secure"})

	select {
	case message := <-received:
		if !strings.Contains(message, `"path":"/secure"`) {
			t.Fatalf("message = %q, want JSON log entry", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tls log message")
	}

	select {
	case got := <-serverNames:
		if got != serverName {
			t.Fatalf("SNI = %q, want %q", got, serverName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tls server name")
	}
}

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

func startTCPServer(t *testing.T) (string, <-chan string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	received := make(chan string, 1)
	go acceptMessage(ln, received)
	return ln.Addr().String(), received
}

func startTLSServer(t *testing.T) (string, <-chan string, <-chan string) {
	t.Helper()

	cert := testCertificate(t)
	serverNames := make(chan string, 1)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			serverNames <- info.ServerName
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("listen tls: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	received := make(chan string, 1)
	go acceptMessage(ln, received)
	return ln.Addr().String(), received, serverNames
}

func acceptMessage(ln net.Listener, received chan<- string) {
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
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "logs.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"logs.example.test"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

func splitAddr(t *testing.T, addr string) (string, string) {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return host, port
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var out int
	if _, err := fmt.Sscanf(value, "%d", &out); err != nil {
		t.Fatalf("parse int %q: %v", value, err)
	}
	return out
}
