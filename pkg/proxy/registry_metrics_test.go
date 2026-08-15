package proxy

import (
	"sync"
	"testing"
	"time"
)

type retiringClusterObserver struct {
	NopClusterObserver
	mu      sync.Mutex
	deleted []string
}

type blockingRetiringObserver struct {
	NopClusterObserver
	started chan struct{}
	release chan struct{}
}

func (o *blockingRetiringObserver) DeleteCluster(string) {
	close(o.started)
	<-o.release
}

func TestClusterRegistryDoesNotPublishReplacementBeforeOldMetricsAreDeleted(t *testing.T) {
	observer := &blockingRetiringObserver{started: make(chan struct{}), release: make(chan struct{})}
	registry := NewClusterRegistry(observer)
	config := testClusterConfig()
	config.Name = "orders"
	first, err := registry.Acquire(config)
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		first.Stop()
		close(stopped)
	}()
	<-observer.started

	acquired := make(chan error, 1)
	go func() {
		_, acquireErr := registry.Acquire(config)
		acquired <- acquireErr
	}()
	select {
	case err := <-acquired:
		t.Fatalf("replacement acquired before prior metric deletion completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(observer.release)
	<-stopped
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("replacement Acquire() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement Acquire() remained blocked after metric deletion")
	}
}

func (o *retiringClusterObserver) DeleteCluster(name string) {
	o.mu.Lock()
	o.deleted = append(o.deleted, name)
	o.mu.Unlock()
}

func (o *retiringClusterObserver) deletedNames() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.deleted...)
}

func TestClusterRegistryDeletesMetricsAfterFinalSameNameClusterRetires(t *testing.T) {
	observer := &retiringClusterObserver{}
	registry := NewClusterRegistry(observer)
	firstConfig := testClusterConfig()
	firstConfig.Name = "orders"
	secondConfig := testClusterConfig()
	secondConfig.Name = "orders"
	secondConfig.MaxInFlight++

	first, err := registry.Acquire(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Acquire(secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	first.Stop()
	if got := observer.deletedNames(); len(got) != 0 {
		t.Fatalf("deleted names while replacement remained = %#v", got)
	}
	second.Stop()
	if got := observer.deletedNames(); len(got) != 1 || got[0] != "orders" {
		t.Fatalf("deleted names after final retirement = %#v, want [orders]", got)
	}
}
