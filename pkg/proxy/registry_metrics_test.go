package proxy

import (
	"sync"
	"testing"
	"time"
)

type retiringClusterObserver struct {
	NopClusterObserver
	mu             sync.Mutex
	deleted        []string
	deletedTargets []string
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

func (o *retiringClusterObserver) SetUpstreamStatus(string, string, bool) {}

func (o *retiringClusterObserver) DeleteUpstreamStatus(name, target string) {
	o.mu.Lock()
	o.deletedTargets = append(o.deletedTargets, name+"|"+target)
	o.mu.Unlock()
}

func (o *retiringClusterObserver) deletedNames() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.deleted...)
}

func (o *retiringClusterObserver) retiredTargets() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.deletedTargets...)
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

func TestClusterRegistryDeletesOnlyRetiredTargetsAcrossSameNameReplacement(t *testing.T) {
	observer := &retiringClusterObserver{}
	registry := NewClusterRegistry(observer)
	firstConfig := testClusterConfig()
	firstConfig.Name = "orders"
	firstConfig.Targets = map[string]int{"http://127.0.0.1:8080": 1}
	secondConfig := testClusterConfig()
	secondConfig.Name = "orders"
	secondConfig.Targets = map[string]int{"http://127.0.0.1:8081": 1}

	first, err := registry.Acquire(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Acquire(secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	first.Stop()
	if got := observer.retiredTargets(); len(got) != 1 || got[0] != "orders|http://127.0.0.1:8080" {
		t.Fatalf("retired targets = %#v, want old orders target", got)
	}
	if got := observer.deletedNames(); len(got) != 0 {
		t.Fatalf("deleted clusters while replacement remained = %#v", got)
	}
	second.Stop()
}

func TestClusterRegistryRetainsTargetsSharedBySameNameGenerations(t *testing.T) {
	observer := &retiringClusterObserver{}
	registry := NewClusterRegistry(observer)
	firstConfig := testClusterConfig()
	firstConfig.Name = "orders"
	firstConfig.Targets = map[string]int{
		"http://127.0.0.1:8080": 1,
		"http://127.0.0.1:8082": 1,
	}
	secondConfig := testClusterConfig()
	secondConfig.Name = "orders"
	secondConfig.Targets = map[string]int{
		"http://127.0.0.1:8081": 1,
		"http://127.0.0.1:8082": 1,
	}

	first, err := registry.Acquire(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Acquire(secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	first.Stop()
	if got := observer.retiredTargets(); len(got) != 1 || got[0] != "orders|http://127.0.0.1:8080" {
		t.Fatalf("retired targets = %#v, want only unshared target", got)
	}
	second.Stop()
}
