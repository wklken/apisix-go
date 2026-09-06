package compiler

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func TestFrontendSSLMaterializesReferencesAndEncryptedKeys(t *testing.T) {
	cert, key := sslSecretTestKeyPair(t)
	const gdeKey = "qeddd145sfvddff3"
	encrypted := sslSecretTestCBC(t, key, gdeKey)
	for _, test := range []struct {
		name      string
		enabled   bool
		cert, key string
	}{
		{"environment", false, "$ENV://FINAL_SSL_CERT", "$env://FINAL_SSL_KEY"},
		{"GDE key", true, cert, encrypted},
		{"GDE with plugin encryption disabled", false, cert, encrypted},
		{"environment GDE key", false, "$ENV://FINAL_SSL_CERT", "$ENV://FINAL_SSL_ENCRYPTED_KEY"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("FINAL_SSL_CERT", cert)
			t.Setenv("FINAL_SSL_KEY", key)
			t.Setenv("FINAL_SSL_ENCRYPTED_KEY", encrypted)
			catalog, err := capability.NewSecretDeclarationCatalog()
			if err != nil {
				t.Fatal(err)
			}
			service := data_encryption.NewService(test.enabled, []string{gdeKey}, catalog)
			resolver, err := secret.NewGenerationSecretResolver(service)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := resolver.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			effective := workerTestEffective()
			effective.Config.Apisix.ID = "ssl-secrets-test"
			effective.Config.Apisix.Ssl = config.Ssl{
				Enable:       true,
				Listen:       []config.Listen{{Port: 9443}},
				SslProtocols: "TLSv1.2",
				SslCiphers:   "ECDHE-ECDSA-AES128-GCM-SHA256",
			}
			factory, err := NewWorkerCompilerFactory(
				effective,
				secret.NewMaterializer(service, resolver),
				workerTestRuntimeObservers(),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := factory.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			raw, err := json.Marshal(
				map[string]any{
					"id":    "ssl",
					"sni":   "secret.example.test",
					"cert":  test.cert,
					"key":   test.key,
					"certs": []string{test.cert},
					"keys":  []string{test.key},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			desired := mustGenerationSnapshot(
				t,
				90,
				[]generation.Resource{resourceValue("ssls", "ssl", string(raw))},
				nil,
			)
			prepared, err := factory.PrepareGeneration(
				context.Background(),
				ticketForSnapshot(desired, generation.DomainHTTP),
				desired,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := prepared.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			selected, err := prepared.HTTP().
				TLSConfig().
				GetConfigForClient(&tls.ClientHelloInfo{ServerName: "secret.example.test"})
			if err != nil {
				t.Fatal(err)
			}
			if len(selected.Certificates) != 2 {
				t.Fatalf("certificates=%d", len(selected.Certificates))
			}
			if len(selected.Certificates[0].Certificate) == 0 {
				t.Fatal("missing parsed certificate")
			}
		})
	}
}

func sslSecretTestCBC(t *testing.T, plain, key string) string {
	t.Helper()
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append([]byte(plain), bytes.Repeat([]byte{byte(padding)}, padding)...)
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(encrypted, padded)
	return base64.StdEncoding.EncodeToString(encrypted)
}

func sslSecretTestKeyPair(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "secret.example.test"},
		DNSNames:     []string{"secret.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		), string(
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		)
}

func TestFrontendSSLDoesNotResolveInactiveMaterial(t *testing.T) {
	for _, test := range []struct {
		name      string
		enabled   bool
		status    int
		kind      string
		wantError bool
	}{
		{"listener disabled", false, 1, "server", false},
		{"SSL disabled", true, 0, "server", false},
		{"client resource", true, 1, "client", false},
		{"invalid status", true, 2, "server", true},
		{"invalid type", true, 1, "bogus", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog, err := capability.NewSecretDeclarationCatalog()
			if err != nil {
				t.Fatal(err)
			}
			broker := &consumerPreparationBroker{}
			effective := workerTestEffective()
			effective.Config.Apisix.ID = "inactive-ssl-test"
			effective.Config.Apisix.Ssl = config.Ssl{
				Enable:       test.enabled,
				Listen:       []config.Listen{{Port: 9443}},
				SslProtocols: "TLSv1.2",
				SslCiphers:   "ECDHE-ECDSA-AES128-GCM-SHA256",
			}
			factory, err := NewWorkerCompilerFactory(
				effective,
				testutil.NewSecretMaterializer(broker, catalog),
				workerTestRuntimeObservers(),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := factory.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			raw, err := json.Marshal(
				map[string]any{
					"id":     "inactive",
					"sni":    "inactive.example.test",
					"status": test.status,
					"type":   test.kind,
					"cert":   "$ENV://UNAVAILABLE_CERT",
					"key":    "$ENV://UNAVAILABLE_KEY",
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			desired := mustGenerationSnapshot(
				t,
				92,
				[]generation.Resource{resourceValue("ssls", "inactive", string(raw))},
				nil,
			)
			prepared, err := factory.PrepareGeneration(
				context.Background(),
				ticketForSnapshot(desired, generation.DomainHTTP),
				desired,
				nil,
			)
			if len(broker.scopes) != 0 {
				t.Fatalf("inactive or invalid SSL resolved %d secrets", len(broker.scopes))
			}
			if test.wantError {
				if err == nil {
					_ = prepared.Close(context.Background())
					t.Fatal("invalid SSL was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := prepared.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			if len(broker.scopes) != 0 {
				t.Fatalf("inactive SSL resolved %d secrets", len(broker.scopes))
			}
		})
	}
}
