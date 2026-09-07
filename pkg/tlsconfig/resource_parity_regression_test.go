package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestSSLExtraCertificatesServeECDSAClients(t *testing.T) {
	rsaCert, rsaKey := parityServerKeyPair(t, false)
	ecCert, ecKey := parityServerKeyPair(t, true)
	raw, err := json.Marshal(
		map[string]any{
			"id":    "dual",
			"sni":   "dual.example.test",
			"cert":  rsaCert,
			"key":   rsaKey,
			"certs": []string{ecCert},
			"keys":  []string{ecKey},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var ssl resource.SSL
	if err := json.Unmarshal(raw, &ssl); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Compile(
		Input{
			Config: testConfigWithTLS("TLSv1.2", "ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256"),
			SSLs:   map[string]resource.SSL{"dual": ssl},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	)
	server.TLS = snapshot.TLSConfig()
	server.StartTLS()
	defer server.Close()
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "dual.example.test",
			MinVersion:         tls.VersionTLS12,
			MaxVersion:         tls.VersionTLS12,
			CipherSuites:       []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if _, ok := response.TLS.PeerCertificates[0].PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatal("ECDSA client received a different key type")
	}
}

func TestSSLClientDepthCountsOnlyIntermediates(t *testing.T) {
	_, ca := testCertificateAuthority(t, "client-ca")
	cert, key := testServerKeyPair(t, "depth")
	for _, depth := range []int{0, 1, 2} {
		ssl := resource.SSL{
			Sni:    "depth.example.test",
			Cert:   cert,
			Key:    key,
			Status: 1,
			Client: &resource.SSLClient{CA: ca, Depth: depth},
		}
		snapshot, err := Compile(Input{Config: testFrontendConfig(), SSLs: map[string]resource.SSL{"depth": ssl}})
		if err != nil {
			t.Fatalf("depth=%d: %v", depth, err)
		}
		selected, err := snapshot.TLSConfig().GetConfigForClient(&tls.ClientHelloInfo{ServerName: ssl.Sni})
		if err != nil {
			t.Fatal(err)
		}
		chain := make([]*x509.Certificate, depth+2)
		for i := range chain {
			chain[i] = &x509.Certificate{}
		}
		if err := selected.VerifyConnection(
			tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{chain}},
		); err != nil {
			t.Fatalf("depth=%d valid chain: %v", depth, err)
		}
		chain = append(chain, &x509.Certificate{})
		if err := selected.VerifyConnection(
			tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{chain}},
		); err == nil {
			t.Fatalf("depth=%d accepted too many intermediates", depth)
		}
	}
}

func TestSSLDeepWildcardDoesNotFallThroughToCatchAll(t *testing.T) {
	cert, key := testServerKeyPair(t, "wildcard")
	snapshot, err := Compile(Input{Config: testFrontendConfig(), SSLs: map[string]resource.SSL{
		"wildcard": {Sni: "*.example.test", Cert: cert, Key: key, Status: 1},
		"fallback": {Sni: "*", Cert: cert, Key: key, Status: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.example.test", "other.test"} {
		if _, err := snapshot.TLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: name}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := snapshot.TLSConfig().
		GetCertificate(&tls.ClientHelloInfo{ServerName: "two.labels.example.test"}); err == nil {
		t.Fatal("invalid wildcard match fell through to catch-all")
	}
}

func parityServerKeyPair(t *testing.T, ellipticKey bool) (string, string) {
	t.Helper()
	var privateKey any
	var publicKey any
	if ellipticKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		privateKey, publicKey = key, &key.PublicKey
	} else {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		privateKey, publicKey = key, &key.PublicKey
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "dual.example.test"},
		DNSNames:     []string{"dual.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		), string(
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey}),
		)
}
