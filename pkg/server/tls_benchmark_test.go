package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/store"
)

func frontendTestCertificatePEM(t testing.TB, commonName string) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{commonName},
	}
	certificateDER, err := x509.CreateCertificate(
		rand.Reader,
		&template,
		&template,
		&key.PublicKey,
		key,
	)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return string(certificatePEM), string(keyPEM)
}

// BenchmarkTLSCertificate measures handshake certificate selection through
// the production GetCertificate path. SSL resources are seeded through the
// store event channel; the corpus uses only stable APIs so the same rows
// measure the per-handshake decode path before and the published-index
// lookup after the change.
func BenchmarkTLSCertificate(b *testing.B) {
	events := make(chan *store.Event)
	storage, err := store.GetStore(b.TempDir()+"/tls-bench.db", events, data_encryption.Service{})
	if err != nil {
		b.Fatalf("get store: %v", err)
	}
	storage.Start()
	b.Cleanup(func() { _ = storage.Stop() })

	cert, key := frontendTestCertificatePEM(b, "api.bench.test")
	put := func(id string, snis ...string) {
		event := store.NewEvent()
		event.Type = store.EventTypePut
		event.Key = []byte("/apisix/ssls/" + id)
		event.Value = []byte(`{"id":"` + id + `","snis":["` + strings.Join(snis, `","`) +
			`"],"cert":"` + cert + `","key":"` + key + `","status":1}`)
		events <- event
	}

	for _, n := range []int{1, 100, 1000} {
		for i := range n {
			put(fmt.Sprintf("bench-%d-%d", n, i), fmt.Sprintf("api-%d-%d.bench.test", n, i))
		}
		if err := storage.Sync(); err != nil {
			b.Fatalf("Sync() error = %v", err)
		}
		b.Run(fmt.Sprintf("exact/n=%d", n), func(b *testing.B) {
			benchmarkTLSCertificateLookup(b, fmt.Sprintf("api-%d-%d.bench.test", n, n/2))
		})
	}

	put("bench-wildcard", "*.wild.bench.test")
	if err := storage.Sync(); err != nil {
		b.Fatalf("Sync() error = %v", err)
	}
	b.Run("wildcard/n=1000", func(b *testing.B) {
		benchmarkTLSCertificateLookup(b, "a.wild.bench.test")
	})
}

func benchmarkTLSCertificateLookup(b *testing.B, serverName string) {
	b.ReportAllocs()
	getCertificate := mustFrontendTLSConfig(b, nil).GetCertificate
	hello := &tls.ClientHelloInfo{ServerName: serverName}
	for b.Loop() {
		certificate, err := getCertificate(hello)
		if err != nil {
			b.Fatal(err)
		}
		if len(certificate.Certificate) == 0 {
			b.Fatal("empty certificate selected")
		}
	}
}
