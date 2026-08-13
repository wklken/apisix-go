package server

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/store"
)

var frontendTLSCipherSuites = map[string]uint16{
	"ECDHE-ECDSA-AES128-GCM-SHA256": tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-RSA-AES128-GCM-SHA256":   tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-ECDSA-AES256-GCM-SHA384": tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-RSA-AES256-GCM-SHA384":   tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-ECDSA-CHACHA20-POLY1305": tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-RSA-CHACHA20-POLY1305":   tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

var frontendTLS13CipherNames = map[string]struct{}{
	"TLS_AES_128_GCM_SHA256":       {},
	"TLS_AES_256_GCM_SHA384":       {},
	"TLS_CHACHA20_POLY1305_SHA256": {},
	"TLS_AES_128_CCM_SHA256":       {},
	"TLS_AES_128_CCM_8_SHA256":     {},
}

func buildFrontendTLSConfig() (*tls.Config, error) {
	var ssl config.Ssl
	strict := false
	if config.GlobalConfig != nil {
		ssl = config.GlobalConfig.Apisix.Ssl
		strict = ssl.Enable
	}
	minVersion, maxVersion, err := parseFrontendTLSProtocols(ssl.SslProtocols, strict)
	if err != nil {
		return nil, fmt.Errorf("frontend TLS protocols: %w", err)
	}
	cipherSuites, err := parseFrontendTLSCipherSuites(ssl.SslCiphers, minVersion, strict)
	if err != nil {
		return nil, fmt.Errorf("frontend TLS ciphers: %w", err)
	}

	config := &tls.Config{
		MinVersion:             minVersion,
		MaxVersion:             maxVersion,
		CipherSuites:           cipherSuites,
		SessionTicketsDisabled: !ssl.SslSessionTickets,
		NextProtos:             frontendTLSNextProtos(),
		GetCertificate:         frontendTLSCertificateSelector(),
	}
	if trustedCertificate := strings.TrimSpace(ssl.SslTrustedCertificate); trustedCertificate != "" {
		certificatePEM, err := os.ReadFile(trustedCertificate)
		if err != nil {
			return nil, fmt.Errorf("read trusted client CA %q: %w", trustedCertificate, err)
		}
		clientCAs := x509.NewCertPool()
		if !clientCAs.AppendCertsFromPEM(certificatePEM) {
			return nil, fmt.Errorf("parse trusted client CA %q: no certificates found", trustedCertificate)
		}
		config.ClientCAs = clientCAs
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return config, nil
}

func parseFrontendTLSProtocols(raw string, required bool) (uint16, uint16, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if required {
			return 0, 0, fmt.Errorf("protocol list must not be empty when TLS is enabled")
		}
		// No enabled frontend listener retains the previous net/http default:
		// require TLS 1.2 while leaving MaxVersion unset for TLS 1.3 support.
		return tls.VersionTLS12, 0, nil
	}

	seen := make(map[string]struct{}, 2)
	var minVersion, maxVersion uint16
	for token := range strings.FieldsSeq(raw) {
		if token == "" {
			return 0, 0, fmt.Errorf("protocol token must not be empty")
		}
		if _, ok := seen[token]; ok {
			return 0, 0, fmt.Errorf("duplicate protocol %q", token)
		}
		seen[token] = struct{}{}
		var version uint16
		switch token {
		case "TLSv1.2":
			version = tls.VersionTLS12
		case "TLSv1.3":
			version = tls.VersionTLS13
		default:
			return 0, 0, fmt.Errorf("unsupported protocol %q", token)
		}
		if minVersion == 0 || version < minVersion {
			minVersion = version
		}
		if version > maxVersion {
			maxVersion = version
		}
	}
	if minVersion == 0 {
		return 0, 0, fmt.Errorf("protocol list must not be empty")
	}
	return minVersion, maxVersion, nil
}

func parseFrontendTLSCipherSuites(raw string, minVersion uint16, required bool) ([]uint16, error) {
	if strings.TrimSpace(raw) == "" {
		if required && minVersion == tls.VersionTLS12 {
			return nil, fmt.Errorf("TLS 1.2 requires a non-empty cipher list")
		}
		return nil, nil
	}
	if minVersion == tls.VersionTLS13 {
		return nil, fmt.Errorf("TLS 1.3-only configuration must not set TLS 1.2 cipher suites")
	}

	parts := strings.Split(raw, ":")
	cipherSuites := make([]uint16, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("cipher list contains an empty segment")
		}
		if _, ok := frontendTLS13CipherNames[name]; ok {
			return nil, fmt.Errorf("TLS 1.3 cipher suite %q cannot be configured", name)
		}
		suite, ok := frontendTLSCipherSuites[name]
		if !ok {
			return nil, fmt.Errorf("unknown or unsupported cipher suite %q", name)
		}
		cipherSuites = append(cipherSuites, suite)
	}
	return cipherSuites, nil
}

func frontendTLSNextProtos() []string {
	protocols := []string{"http/1.1"}
	if frontendHTTP2Enabled() {
		protocols = append([]string{"h2"}, protocols...)
	}
	return protocols
}

func frontendTLSCertificateSelector() func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		serverName := strings.TrimSpace(hello.ServerName)
		if serverName == "" && config.GlobalConfig != nil {
			serverName = strings.TrimSpace(config.GlobalConfig.Apisix.Ssl.FallbackSNI)
		}
		return store.GetSSLCertificateForSNI(serverName)
	}
}
