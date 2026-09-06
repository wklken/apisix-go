package tlsconfig

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestParityExplicitTLSVersionsHandshake(t *testing.T) {
	for _, tc := range []struct {
		name, protocols, ciphers string
		version                  uint16
		cipher                   uint16
	}{
		{"TLS11", "TLSv1.1", "ECDHE-ECDSA-AES128-SHA", tls.VersionTLS11, tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA},
		{"TLS13InheritedCiphers", "TLSv1.3", testTLS12Cipher, tls.VersionTLS13, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, key := testServerKeyPair(t, "parity.test")
			snap, err := Compile(
				Input{
					Config: testConfigWithTLS(tc.protocols, tc.ciphers),
					SSLs: map[string]resource.SSL{
						"1": {ID: "1", Sni: "parity.test", Cert: cert, Key: key, Status: 1},
					},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }),
			)
			server.TLS = snap.TLSConfig()
			server.StartTLS()
			defer server.Close()
			cfg := &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         "parity.test",
				MinVersion:         tc.version,
				MaxVersion:         tc.version,
			}
			if tc.cipher != 0 {
				cfg.CipherSuites = []uint16{tc.cipher}
			}
			transport := &http.Transport{TLSClientConfig: cfg}
			defer transport.CloseIdleConnections()
			response, err := (&http.Client{Transport: transport}).Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != 204 || response.TLS.Version != tc.version {
				t.Fatalf("response=%d TLS=%x", response.StatusCode, response.TLS.Version)
			}
		})
	}
}

func TestParityTLS13StillRejectsInvalidCipherNames(t *testing.T) {
	if _, err := CompileBase(BaseInput{Config: testConfigWithTLS("TLSv1.3", "not-a-cipher")}); err == nil {
		t.Fatal("invalid cipher accepted")
	}
}
