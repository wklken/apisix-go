package compiler

import (
	"crypto/tls"
	"net/http"

	"github.com/wklken/apisix-go/pkg/generation"
)

// HTTPSnapshot is the authority-free HTTP/TLS observation surface of one
// prepared generation. Activation and cleanup remain generation-owned.
type HTTPSnapshot struct {
	artifact  generation.GenerationArtifact
	handler   http.Handler
	tlsConfig *tls.Config
}

func (snapshot *HTTPSnapshot) Revision() uint64 {
	if snapshot == nil {
		return 0
	}
	return snapshot.artifact.Revision
}

func (snapshot *HTTPSnapshot) Handler() http.Handler {
	if snapshot == nil {
		return nil
	}
	return snapshot.handler
}

func (snapshot *HTTPSnapshot) TLSConfig() *tls.Config {
	if snapshot == nil || snapshot.tlsConfig == nil {
		return nil
	}
	return snapshot.tlsConfig.Clone()
}
