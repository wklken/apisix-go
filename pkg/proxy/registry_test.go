package proxy

import (
	"crypto/tls"
	"errors"
	"testing"
	"time"
)

func TestClusterRegistrySeparatesChangedResponseHeaderTimeout(t *testing.T) {
	registry := NewClusterRegistry(NopClusterObserver{})
	t.Cleanup(registry.Close)
	base := testClusterConfig()
	changed := testClusterConfig()
	changed.Transport = (&TransportOptionBuilder{}).
		WithDialTimeout(time.Second).
		WithResponseHeaderTimeout(2 * time.Second).
		Build()

	first, err := registry.Acquire(base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Acquire(changed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cluster() == second.Cluster() {
		t.Fatal("changed response header timeout reused the same cluster")
	}
	if got := registry.Len(); got != 2 {
		t.Fatalf("registry.Len() = %d, want 2", got)
	}
	first.Stop()
	if second.Cluster().Closed() {
		t.Fatal("stopping one lease closed the other cluster")
	}
	second.Stop()
	if !second.Cluster().Closed() {
		t.Fatal("final release did not close the changed cluster")
	}
}

func TestClusterRegistrySeparatesRotatedTLSClientCertificate(t *testing.T) {
	registry := NewClusterRegistry(NopClusterObserver{})
	t.Cleanup(registry.Close)
	firstConfig := testClusterConfig()
	firstConfig.Transport = (&TransportOptionBuilder{}).
		WithTLSClientCertificate(tls.Certificate{Certificate: [][]byte{[]byte("leaf-a")}}).
		Build()
	sameConfig := testClusterConfig()
	sameConfig.Transport = (&TransportOptionBuilder{}).
		WithTLSClientCertificate(tls.Certificate{Certificate: [][]byte{[]byte("leaf-a")}}).
		Build()
	rotatedConfig := testClusterConfig()
	rotatedConfig.Transport = (&TransportOptionBuilder{}).
		WithTLSClientCertificate(tls.Certificate{Certificate: [][]byte{[]byte("leaf-b")}}).
		Build()

	first, err := registry.Acquire(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	same, err := registry.Acquire(sameConfig)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := registry.Acquire(rotatedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cluster() == rotated.Cluster() {
		t.Fatal("rotated client certificate reused the old cluster")
	}
	if first.Cluster() != same.Cluster() {
		t.Fatal("identical client certificate created different clusters")
	}
	if got := registry.Len(); got != 2 {
		t.Fatalf("registry.Len() = %d, want 2", got)
	}
	first.Stop()
	if first.Cluster().Closed() {
		t.Fatal("old cluster closed while an identical certificate remained leased")
	}
	same.Stop()
	if !first.Cluster().Closed() {
		t.Fatal("old cluster remained open after its final lease stopped")
	}
	if rotated.Cluster().Closed() {
		t.Fatal("rotated cluster closed while still leased")
	}
	rotated.Stop()
	if !rotated.Cluster().Closed() {
		t.Fatal("rotated cluster remained open after its final lease stopped")
	}
}

func TestClusterRegistryLenTracksFinalRelease(t *testing.T) {
	registry := NewClusterRegistry(NopClusterObserver{})
	first, err := registry.Acquire(testClusterConfig())
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Acquire(testClusterConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Len(); got != 1 {
		t.Fatalf("registry.Len() = %d, want 1", got)
	}
	first.Stop()
	second.Stop()
	if got := registry.Len(); got != 0 {
		t.Fatalf("registry.Len() after final release = %d, want 0", got)
	}
}

func TestClusterRegistryCloseClosesEveryCluster(t *testing.T) {
	registry := NewClusterRegistry(NopClusterObserver{})
	base := testClusterConfig()
	changed := testClusterConfig()
	changed.Transport = (&TransportOptionBuilder{}).
		WithDialTimeout(time.Second).
		WithResponseHeaderTimeout(2 * time.Second).
		Build()
	first, err := registry.Acquire(base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Acquire(changed)
	if err != nil {
		t.Fatal(err)
	}
	registry.Close()
	if !first.Cluster().Closed() {
		t.Fatal("registry.Close() did not close the first cluster")
	}
	if !second.Cluster().Closed() {
		t.Fatal("registry.Close() did not close the second cluster")
	}
	if got := registry.Len(); got != 0 {
		t.Fatalf("registry.Len() after Close = %d, want 0", got)
	}
	if _, err := registry.Acquire(testClusterConfig()); !errors.Is(err, ErrClusterRegistryClosed) {
		t.Fatalf("Acquire after Close() error = %v, want ErrClusterRegistryClosed", err)
	}
}

func TestClusterLeaseStopIsIdempotent(t *testing.T) {
	registry := NewClusterRegistry(NopClusterObserver{})
	lease, err := registry.Acquire(testClusterConfig())
	if err != nil {
		t.Fatal(err)
	}
	lease.Stop()
	lease.Stop()
	if got := registry.Len(); got != 0 {
		t.Fatalf("registry.Len() after repeated Stop = %d, want 0", got)
	}
}
