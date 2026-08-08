package store

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

	bolt "go.etcd.io/bbolt"
)

func testCertificatePEM(t testing.TB, commonName string) (string, string) {
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

// BenchmarkTLSCertificate models the pre-index handshake selection path at
// the store layer: list every SSL resource, scan for the requested SNI, and
// decode the matched keypair. These rows isolate the decode cost that the
// published index removes; the server-side rows measure the production path.
func BenchmarkTLSCertificate(b *testing.B) {
	for _, n := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("list-scan-decode/n=%d", n), func(b *testing.B) {
			benchmarkTLSCertificateListScanDecode(b, n)
		})
	}
}

func openBenchmarkDB(b *testing.B) *bolt.DB {
	b.Helper()
	db, err := bolt.Open(b.TempDir()+"/ssl-bench.db", 0o600, nil)
	if err != nil {
		b.Fatalf("open database: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return db
}

func benchmarkTLSCertificateListScanDecode(b *testing.B, n int) {
	b.ReportAllocs()
	cert, key := testCertificatePEM(b, "bench.example.test")
	storage := &Store{db: openBenchmarkDB(b)}
	if err := storage.InitBuckets(); err != nil {
		b.Fatalf("init buckets: %v", err)
	}
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("ssls"))
		for i := range n {
			value := []byte(`{"id":"ssl-` + fmt.Sprintf("%d", i) + `","snis":["api-` +
				fmt.Sprintf("%d", i) + `.example.test"],"cert":"` + cert + `","key":"` + key + `","status":1}`)
			if err := bucket.Put(fmt.Appendf(nil, "ssl-%d", i), value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatalf("seed ssls: %v", err)
	}
	target := fmt.Sprintf("api-%d.example.test", n/2)

	for b.Loop() {
		data, err := storage.GetBucketData("ssls")
		if err != nil {
			b.Fatal(err)
		}
		selected := false
		for _, value := range data {
			sslResource, err := ParseSSL(value)
			if err != nil {
				b.Fatal(err)
			}
			if sslResource.Status == 0 || !benchmarkSNIMatch(sslResource.Snis, target) {
				continue
			}
			certificate, err := tls.X509KeyPair([]byte(sslResource.Cert), []byte(sslResource.Key))
			if err != nil {
				b.Fatal(err)
			}
			_ = certificate
			selected = true
			break
		}
		if !selected {
			b.Fatal("no certificate selected")
		}
	}
}

func benchmarkSNIMatch(snis []string, serverName string) bool {
	for _, sni := range snis {
		sni = strings.TrimSpace(sni)
		if strings.EqualFold(sni, serverName) {
			return true
		}
		if strings.HasPrefix(sni, "*.") && strings.HasSuffix(strings.ToLower(serverName), strings.ToLower(sni[1:])) {
			return true
		}
	}
	return false
}
