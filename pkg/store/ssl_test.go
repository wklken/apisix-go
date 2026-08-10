package store

import (
	"strconv"
	"testing"
)

func TestParseSSL(t *testing.T) {
	ssl, err := ParseSSL([]byte(`{
		"id": "ssl-1",
		"cert": "CERT",
		"key": "KEY"
	}`))
	if err != nil {
		t.Fatalf("ParseSSL() error = %v", err)
	}
	if ssl.ID != "ssl-1" || ssl.Cert != "CERT" || ssl.Key != "KEY" {
		t.Fatalf("ssl = %#v, want id/cert/key preserved", ssl)
	}
}

func TestGetSSLReturnsNotFoundForMissingResource(t *testing.T) {
	if _, err := GetSSL("missing"); err != ErrNotFound {
		t.Fatalf("GetSSL() error = %v, want %v", err, ErrNotFound)
	}
}

// sslIndexTestStore opens a store with an events channel and registers it as
// the global store so package-level lookups read the published index.
func sslIndexTestStore(t *testing.T) (*Store, chan *Event) {
	t.Helper()
	events := make(chan *Event)
	storage, err := Open(t.TempDir()+"/ssl-index.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	previous := s
	s = storage
	t.Cleanup(func() { s = previous })
	return storage, events
}

func putSSL(t *testing.T, events chan *Event, id string, value []byte) {
	t.Helper()
	event := NewEvent()
	event.Type = EventTypePut
	event.Key = []byte("/apisix/ssls/" + id)
	event.Value = value
	events <- event
}

func deleteSSL(t *testing.T, events chan *Event, id string) {
	t.Helper()
	event := NewEvent()
	event.Type = EventTypeDelete
	event.Key = []byte("/apisix/ssls/" + id)
	events <- event
}

func sslValue(id string, snis []string, cert, key string, status int) []byte {
	snisJSON := "[]"
	if len(snis) > 0 {
		snisJSON = "["
		for i, sni := range snis {
			if i > 0 {
				snisJSON += ","
			}
			snisJSON += `"` + sni + `"`
		}
		snisJSON += "]"
	}
	return []byte(`{"id":"` + id + `","snis":` + snisJSON + `,"cert":"` +
		cert + `","key":"` + key + `","status":` + strconv.Itoa(status) + `}`)
}

func TestSSLCertificateIndexPublishesExactAndWildcard(t *testing.T) {
	storage, events := sslIndexTestStore(t)
	cert, key := testCertificatePEM(t, "api.example.test")
	putSSL(
		t,
		events,
		"ssl-exact",
		sslValue("ssl-exact", []string{"api.example.test", "*.wild.example.test"}, cert, key, 1),
	)
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	certificate, err := GetSSLCertificateForSNI("api.example.test")
	if err != nil {
		t.Fatalf("GetSSLCertificateForSNI(exact) error = %v", err)
	}
	if certificate == nil || len(certificate.Certificate) == 0 {
		t.Fatal("GetSSLCertificateForSNI(exact) returned an empty certificate")
	}

	wildcard, err := GetSSLCertificateForSNI("a.wild.example.test")
	if err != nil {
		t.Fatalf("GetSSLCertificateForSNI(wildcard) error = %v", err)
	}
	if wildcard == nil || len(wildcard.Certificate) == 0 {
		t.Fatal("GetSSLCertificateForSNI(wildcard) returned an empty certificate")
	}

	if _, err := GetSSLCertificateForSNI("bare.example.test"); err == nil {
		t.Fatal("GetSSLCertificateForSNI(bare domain under wildcard) error = nil")
	}
	if _, err := GetSSLCertificateForSNI("other.example.test"); err == nil {
		t.Fatal("GetSSLCertificateForSNI(unrelated) error = nil")
	}
}

func TestSSLCertificateIndexExactBeatsWildcard(t *testing.T) {
	storage, events := sslIndexTestStore(t)
	cert, key := testCertificatePEM(t, "api.example.test")
	putSSL(t, events, "ssl-wild", sslValue("ssl-wild", []string{"*.example.test"}, cert, key, 1))
	putSSL(t, events, "ssl-exact", sslValue("ssl-exact", []string{"api.example.test"}, cert, key, 1))
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	if _, err := GetSSLCertificateForSNI("api.example.test"); err != nil {
		t.Fatalf("GetSSLCertificateForSNI() error = %v", err)
	}
	index := s.sslCerts.Load()
	if got := index.exact["api.example.test"].id; got != "ssl-exact" {
		t.Fatalf("exact SNI owner = %q, want ssl-exact", got)
	}
}

func TestSSLCertificateIndexRejectsInvalidCertificate(t *testing.T) {
	storage, events := sslIndexTestStore(t)
	cert, key := testCertificatePEM(t, "valid.example.test")
	putSSL(t, events, "ssl-valid", sslValue("ssl-valid", []string{"valid.example.test"}, cert, key, 1))
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() after valid SSL error = %v", err)
	}

	putSSL(t, events, "ssl-bad", sslValue("ssl-bad", []string{"bad.example.test"}, "not-a-cert", "not-a-key", 1))
	if err := storage.Sync(); err == nil {
		t.Fatal("Sync() after invalid SSL returned nil")
	}

	if _, err := GetSSLCertificateForSNI("valid.example.test"); err != nil {
		t.Fatalf("last valid snapshot lost after invalid publication: %v", err)
	}
	if _, err := GetSSLCertificateForSNI("bad.example.test"); err == nil {
		t.Fatal("invalid certificate was published to the index")
	}
}

func TestSSLCertificateIndexDeleteAndDisable(t *testing.T) {
	storage, events := sslIndexTestStore(t)
	cert, key := testCertificatePEM(t, "gone.example.test")
	putSSL(t, events, "ssl-gone", sslValue("ssl-gone", []string{"gone.example.test"}, cert, key, 1))
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() after SSL PUT error = %v", err)
	}
	if _, err := GetSSLCertificateForSNI("gone.example.test"); err != nil {
		t.Fatalf("GetSSLCertificateForSNI() error = %v", err)
	}

	deleteSSL(t, events, "ssl-gone")
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() after SSL DELETE error = %v", err)
	}
	if _, err := GetSSLCertificateForSNI("gone.example.test"); err == nil {
		t.Fatal("deleted SSL still selectable")
	}

	putSSL(t, events, "ssl-disabled", sslValue("ssl-disabled", []string{"disabled.example.test"}, cert, key, 0))
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() after disabled SSL error = %v", err)
	}
	if _, err := GetSSLCertificateForSNI("disabled.example.test"); err == nil {
		t.Fatal("disabled SSL (status 0) still selectable")
	}
}

func TestSSLCertificateIndexUpdateReplacesOldSNIs(t *testing.T) {
	storage, events := sslIndexTestStore(t)
	cert, key := testCertificatePEM(t, "moved.example.test")
	putSSL(t, events, "ssl-moved", sslValue("ssl-moved", []string{"old.example.test"}, cert, key, 1))
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() after first SSL PUT error = %v", err)
	}
	putSSL(t, events, "ssl-moved", sslValue("ssl-moved", []string{"new.example.test"}, cert, key, 1))
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() after second SSL PUT error = %v", err)
	}

	if _, err := GetSSLCertificateForSNI("old.example.test"); err == nil {
		t.Fatal("old SNI still selectable after update")
	}
	if _, err := GetSSLCertificateForSNI("new.example.test"); err != nil {
		t.Fatalf("new SNI not selectable after update: %v", err)
	}
}

func TestSSLCertificateIndexDecodesOnce(t *testing.T) {
	storage, events := sslIndexTestStore(t)
	cert, key := testCertificatePEM(t, "shared.example.test")
	putSSL(t, events, "ssl-1", sslValue("ssl-1", []string{"a.example.test", "*.b.example.test"}, cert, key, 1))
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	exact, err := GetSSLCertificateForSNI("a.example.test")
	if err != nil {
		t.Fatalf("GetSSLCertificateForSNI(exact) error = %v", err)
	}
	wildcard, err := GetSSLCertificateForSNI("x.b.example.test")
	if err != nil {
		t.Fatalf("GetSSLCertificateForSNI(wildcard) error = %v", err)
	}
	if exact != wildcard {
		t.Fatal("certificate decoded separately for SNIs of the same resource")
	}
}
