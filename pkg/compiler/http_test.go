package compiler

import (
	"crypto/sha256"
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
)

func TestHTTPSnapshotTLSConfigReturnsClone(t *testing.T) {
	t.Parallel()

	snapshot := &HTTPSnapshot{
		artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: 7,
			Digest: sha256.Sum256([]byte("http-7")),
		},
		handler:   http.NotFoundHandler(),
		tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	first := snapshot.TLSConfig()
	first.MinVersion = tls.VersionTLS13
	if got := snapshot.TLSConfig().MinVersion; got != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want %x", got, tls.VersionTLS12)
	}
}

func TestHTTPSnapshotAccessorsRejectZeroOwner(t *testing.T) {
	t.Parallel()

	var snapshot *HTTPSnapshot
	if snapshot.Revision() != 0 || snapshot.Handler() != nil || snapshot.TLSConfig() != nil {
		t.Fatal("nil HTTPSnapshot exposed candidate data")
	}
}
