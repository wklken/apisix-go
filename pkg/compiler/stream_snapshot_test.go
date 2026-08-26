package compiler

import (
	"context"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

func TestStreamSnapshotAccessorsRejectZeroOrRevokedOwner(t *testing.T) {
	t.Parallel()

	var missing *StreamSnapshot
	if missing.Revision() != 0 || missing.Router() != nil {
		t.Fatal("nil StreamSnapshot exposed candidate data")
	}

	router := &streamruntime.Router{}
	snapshot := &StreamSnapshot{
		artifact: generation.GenerationArtifact{
			Domain: generation.DomainStream, Revision: 7,
		},
		router: router,
	}
	if snapshot.Revision() != 7 || snapshot.Router() != router {
		t.Fatal("live StreamSnapshot did not expose its detached router")
	}
	snapshot.revoke()
	if snapshot.Revision() != 0 || snapshot.Router() != nil {
		t.Fatal("revoked StreamSnapshot exposed candidate data")
	}
}

func TestPreparedGenerationAttachStreamSerializesWithClose(t *testing.T) {
	for range 50 {
		prepared, _, _ := preparedGenerationFixture(t)
		snapshot := &StreamSnapshot{
			artifact: generation.GenerationArtifact{
				Domain: generation.DomainStream, Revision: 1,
			},
			router: &streamruntime.Router{},
		}
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		var attachErr error
		go func() {
			defer wait.Done()
			<-start
			attachErr = prepared.attachStream(snapshot)
		}()
		go func() {
			defer wait.Done()
			<-start
			_ = prepared.Close(context.Background())
		}()
		close(start)
		wait.Wait()
		if prepared.Stream() != nil {
			t.Fatal("closed generation published a stream snapshot")
		}
		if attachErr == nil && (snapshot.Revision() != 0 || snapshot.Router() != nil) {
			t.Fatal("snapshot attached before Close was not revoked")
		}
	}
}
