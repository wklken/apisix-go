package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/tlsconfig"
)

var errHTTPGenerationUnavailable = errors.New("HTTP generation unavailable")

func buildFrontendTLSConfig(cfg *config.Config) (*tls.Config, error) {
	var trustedClientCAPEM []byte
	var fallbackSNI string
	if cfg != nil {
		fallbackSNI = cfg.Apisix.Ssl.FallbackSNI
	}
	if trustedCertificate := frontendTLSTrustedCertificate(cfg); trustedCertificate != "" {
		certificatePEM, err := os.ReadFile(trustedCertificate)
		if err != nil {
			return nil, fmt.Errorf("read trusted client CA %q: %w", trustedCertificate, err)
		}
		trustedClientCAPEM = certificatePEM
	}
	base, err := tlsconfig.CompileBase(tlsconfig.BaseInput{
		Config: cfg, TrustedClientCAPEM: trustedClientCAPEM,
	})
	if err != nil {
		return nil, err
	}
	tlsConfig := base.TLSConfig()
	tlsConfig.GetCertificate = frontendTLSCertificateSelector(fallbackSNI)
	tlsConfig.GetConfigForClient = frontendTLSConfigSelector(tlsConfig, fallbackSNI)
	return tlsConfig, nil
}

func buildGenerationFrontendTLSConfig(cfg *config.Config, source httpLeaseSource) (*tls.Config, error) {
	var trustedClientCAPEM []byte
	if trustedCertificate := frontendTLSTrustedCertificate(cfg); trustedCertificate != "" {
		certificatePEM, err := os.ReadFile(trustedCertificate)
		if err != nil {
			return nil, fmt.Errorf("read trusted client CA %q: %w", trustedCertificate, err)
		}
		trustedClientCAPEM = certificatePEM
	}
	base, err := tlsconfig.CompileBase(tlsconfig.BaseInput{
		Config: cfg, TrustedClientCAPEM: trustedClientCAPEM,
	})
	if err != nil {
		return nil, err
	}
	tlsConfig := base.TLSConfig()
	tlsConfig.GetCertificate = nil
	tlsConfig.GetConfigForClient = generationFrontendTLSConfigSelector(source)
	return tlsConfig, nil
}

func generationFrontendTLSConfigSelector(source httpLeaseSource) func(*tls.ClientHelloInfo) (*tls.Config, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if source == nil {
			return nil, errHTTPGenerationUnavailable
		}
		lease, ok := source()
		if !ok || lease.Snapshot == nil || lease.Release == nil {
			if ok && lease.Release != nil {
				lease.Release()
			}
			return nil, errHTTPGenerationUnavailable
		}
		defer lease.Release()
		selected := lease.Snapshot.TLSConfig()
		if selected == nil {
			return nil, errHTTPGenerationUnavailable
		}
		if selected.GetConfigForClient != nil {
			candidate, err := selected.GetConfigForClient(hello)
			if err != nil {
				return nil, err
			}
			if candidate != nil {
				selected = candidate
			}
		}
		return selected.Clone(), nil
	}
}

func frontendTLSTrustedCertificate(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Apisix.Ssl.SslTrustedCertificate)
}

func frontendTLSCertificateSelector(fallbackSNI string) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		serverName := frontendTLSServerName(hello, fallbackSNI)
		return store.GetSSLCertificateForSNI(serverName)
	}
}

func frontendTLSConfigSelector(base *tls.Config, fallbackSNI string) func(*tls.ClientHelloInfo) (*tls.Config, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		serverName := frontendTLSServerName(hello, fallbackSNI)
		selected, err := store.GetSSLCertificateConfigForSNI(serverName)
		if err != nil {
			// Tests and embedders may supply static certificates directly. Keep
			// that net/http behavior when the dynamic SSL index has no match.
			if len(base.Certificates) > 0 {
				return nil, nil
			}
			return nil, err
		}
		selectedConfig := base.Clone()
		selectedConfig.GetConfigForClient = nil
		selectedConfig.GetCertificate = nil
		selectedConfig.Certificates = []tls.Certificate{*selected.Certificate}
		if selected.ClientCAs != nil {
			selectedConfig.ClientCAs = selected.ClientCAs
			selectedConfig.ClientAuth = tls.RequireAndVerifyClientCert
			enforceClientCertificateDepth(selectedConfig, selected.ClientDepth)
		}
		return selectedConfig, nil
	}
}

func enforceClientCertificateDepth(tlsConfig *tls.Config, maximumDepth int) {
	previous := tlsConfig.VerifyConnection
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if previous != nil {
			if err := previous(state); err != nil {
				return err
			}
		}
		for _, chain := range state.VerifiedChains {
			if len(chain)-1 <= maximumDepth {
				return nil
			}
		}
		return fmt.Errorf("client certificate chain exceeds verification depth %d", maximumDepth)
	}
}

func frontendTLSServerName(hello *tls.ClientHelloInfo, fallbackSNI string) string {
	if hello != nil {
		if serverName := strings.TrimSpace(hello.ServerName); serverName != "" {
			return serverName
		}
	}
	return strings.TrimSpace(fallbackSNI)
}
