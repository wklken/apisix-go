package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/generation"
)

func TestHTTPPublicationAuthorityFollowsCommittedDomainOwner(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	ticket, desired := generationEngineInput(t, 1, generation.DomainHTTP)
	prepared, err := fixture.factory.PrepareGeneration(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.HTTPPublished() {
		t.Fatal("prepared candidate acquired process-log authority")
	}
	if err := prepared.DiscardPrepared(context.Background(), prepared.PublicationSet()); err != nil {
		t.Fatal(err)
	}

	prepareEngineGeneration(t, fixture.engine, 2, generation.DomainHTTP, generation.DomainStream)
	predecessor := fixture.engine.active.Load().http
	lease, ok := fixture.engine.acquireHTTP()
	if !ok {
		t.Fatal("missing predecessor lease")
	}
	defer lease.Release()
	if !predecessor.prepared.HTTPPublished() {
		t.Fatal("published owner has no authority")
	}

	wantErr := errors.New("rollback publication")
	fixture.engine.checkpoint = func(string) error {
		candidate := fixture.engine.active.Load().http
		if candidate.prepared.HTTPPublished() {
			t.Error("candidate acquired authority before publication accepted")
		}
		return wantErr
	}
	ticket, desired = generationEngineInput(t, 3, generation.DomainHTTP)
	if _, err := fixture.engine.Publish(context.Background(), ticket, desired, nil); !errors.Is(err, wantErr) {
		t.Fatalf("Publish = %v", err)
	}
	fixture.engine.checkpoint = nil
	if !predecessor.prepared.HTTPPublished() {
		t.Fatal("rollback revoked predecessor authority")
	}

	prepareEngineGeneration(t, fixture.engine, 4, generation.DomainHTTP)
	currentPrepared := fixture.engine.active.Load().http.prepared
	if predecessor.prepared.HTTPPublished() {
		t.Fatal("draining HTTP predecessor retained authority while stream and request leases remain")
	}
	if !currentPrepared.HTTPPublished() {
		t.Fatal("committed replacement has no authority")
	}
	prepareEngineGeneration(t, fixture.engine, 5, generation.DomainStream)
	if !currentPrepared.HTTPPublished() {
		t.Fatal("stream-only replacement revoked HTTP authority")
	}
	lease.Release()
	if err := fixture.engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if currentPrepared.HTTPPublished() {
		t.Fatal("closed engine retained HTTP authority")
	}
}

func TestHTTPPublicationWaitsForAdmittedMaintenanceBeforeSwap(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	prepareEngineGeneration(t, fixture.engine, 1, generation.DomainHTTP)
	predecessor := fixture.engine.active.Load()
	predecessorPrepared := predecessor.http.prepared
	entered, release, actionDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	go func() {
		actionDone <- predecessorPrepared.WithHTTPPublication(func() error { close(entered); <-release; return nil })
	}()
	<-entered
	ticket, desired := generationEngineInput(t, 2, generation.DomainHTTP)
	published := make(chan error, 1)
	go func() { _, err := fixture.engine.Publish(context.Background(), ticket, desired, nil); published <- err }()
	select {
	case err := <-published:
		t.Fatalf("publication completed before maintenance: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if fixture.engine.active.Load() != predecessor {
		t.Fatal("active bundle changed while predecessor maintenance was still executing")
	}
	finish()
	if err := <-actionDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-published:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("publication did not resume after maintenance")
	}
	ran := false
	if err := predecessorPrepared.WithHTTPPublication(func() error { ran = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("retired predecessor admitted new maintenance")
	}
}
