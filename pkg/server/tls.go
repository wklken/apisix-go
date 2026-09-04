package server

import (
	"crypto/tls"
	"errors"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/tlsconfig"
)

var errHTTPGenerationUnavailable = errors.New("HTTP generation unavailable")

func buildGenerationFrontendTLSConfig(cfg *config.Config, source httpLeaseSource) (*tls.Config, error) {
	base, err := tlsconfig.CompileBase(tlsconfig.BaseInput{Config: cfg})
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
