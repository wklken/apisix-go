package compiler

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"net/http"
	"sync"
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
		handler:     http.NotFoundHandler(),
		tlsConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		quarantined: []generation.ResourceKey{{Kind: "routes", ID: "bad"}},
	}
	first := snapshot.TLSConfig()
	first.MinVersion = tls.VersionTLS13
	if got := snapshot.TLSConfig().MinVersion; got != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want %x", got, tls.VersionTLS12)
	}
	quarantined := snapshot.Quarantined()
	quarantined[0].ID = "mutated"
	if got := snapshot.Quarantined()[0].ID; got != "bad" {
		t.Fatalf("quarantined route = %q, want defensive clone", got)
	}
}

func TestHTTPSnapshotAccessorsRejectZeroOwner(t *testing.T) {
	t.Parallel()

	var snapshot *HTTPSnapshot
	if snapshot.Revision() != 0 || snapshot.Handler() != nil || snapshot.TLSConfig() != nil || snapshot.Quarantined() != nil {
		t.Fatal("nil HTTPSnapshot exposed candidate data")
	}
}

func TestPreparedGenerationAttachHTTPSerializesWithClose(t *testing.T) {
	for range 50 {
		prepared, _, _ := preparedGenerationFixture(t)
		snapshot := &HTTPSnapshot{
			artifact: generation.GenerationArtifact{Domain: generation.DomainHTTP, Revision: 1},
			handler:  http.NotFoundHandler(),
		}
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		var attachErr error
		go func() {
			defer wait.Done()
			<-start
			attachErr = prepared.attachHTTP(snapshot)
		}()
		go func() {
			defer wait.Done()
			<-start
			_ = prepared.Close(context.Background())
		}()
		close(start)
		wait.Wait()
		if prepared.HTTP() != nil {
			t.Fatal("closed generation published an HTTP snapshot")
		}
		if attachErr == nil && (snapshot.Revision() != 0 || snapshot.Handler() != nil) {
			t.Fatal("snapshot attached before Close was not revoked")
		}
	}
}

func TestCompileAndAttachHTTPSkipsStreamOnlyGeneration(t *testing.T) {
	prepared, _ := newEffectiveBindingMaterializerFixture(t, nil, map[generation.Domain]generation.PublicationCandidate{
		generation.DomainStream: {},
	})
	if err := prepared.compileAndAttachHTTP(context.Background()); err != nil {
		t.Fatal(err)
	}
	if prepared.HTTP() != nil {
		t.Fatal("stream-only generation exposed an HTTP snapshot")
	}
}
