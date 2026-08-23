package store

import (
	"errors"
	"strconv"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
	bolt "go.etcd.io/bbolt"
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
	storage, err := Open(t.TempDir()+"/ssl-index.db", events, testDataEncryption())
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

func TestSSLCertificateIndexSupportsSingularSNIAndOneLabelWildcard(t *testing.T) {
	storage, events := sslIndexTestStore(t)
	cert, key := testCertificatePEM(t, "singular.example.test")
	singular, err := json.Marshal(resource.SSL{
		ID:   "ssl-singular",
		Sni:  "singular.example.test",
		Cert: cert,
		Key:  key,
	})
	if err != nil {
		t.Fatalf("marshal singular SSL: %v", err)
	}
	putSSL(t, events, "ssl-singular", singular)
	putSSL(t, events, "ssl-wild", sslValue("ssl-wild", []string{"*.wild.example.test"}, cert, key, 1))
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if _, err := GetSSLCertificateForSNI("singular.example.test"); err != nil {
		t.Fatalf("singular SNI lookup error = %v", err)
	}
	if _, err := GetSSLCertificateForSNI("one.wild.example.test"); err != nil {
		t.Fatalf("one-label wildcard lookup error = %v", err)
	}
	if _, err := GetSSLCertificateForSNI("two.labels.wild.example.test"); err == nil {
		t.Fatal("multi-label wildcard lookup succeeded")
	}
}

func TestSSLCertificateIndexRejectsMalformedClientCAAndRetainsLastGood(t *testing.T) {
	storage, events := sslIndexTestStore(t)
	cert, key := testCertificatePEM(t, "last-good.example.test")
	putSSL(t, events, "ssl-good", sslValue("ssl-good", []string{"last-good.example.test"}, cert, key, 1))
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() after valid SSL error = %v", err)
	}

	badValue, err := json.Marshal(resource.SSL{
		ID:   "ssl-bad-client",
		Sni:  "bad-client.example.test",
		Cert: cert,
		Key:  key,
		Client: &resource.SSLClient{
			CA: "not-a-certificate",
		},
	})
	if err != nil {
		t.Fatalf("marshal malformed client SSL: %v", err)
	}
	putSSL(t, events, "ssl-bad-client", badValue)
	err = storage.Sync()
	var validationErr *ResourceValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("malformed client CA error = %v, want ResourceValidationError", err)
	}
	if _, err := GetSSLCertificateForSNI("last-good.example.test"); err != nil {
		t.Fatalf("last-good SSL lost after malformed client CA: %v", err)
	}
	if _, err := GetSSLCertificateForSNI("bad-client.example.test"); err == nil {
		t.Fatal("malformed client CA resource was published")
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

func TestSSLCertificateIndexRebuildSkipsCorruptPersistedRow(t *testing.T) {
	path := t.TempDir() + "/ssl-corrupt-rebuild.db"
	first, err := Open(path, make(chan *Event), testDataEncryption())
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	cert, key := testCertificatePEM(t, "persisted-good.example.test")
	if err := first.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("ssls"))
		if err := bucket.Put([]byte("ssl-corrupt"), []byte(`{"id":"ssl-corrupt","cert":`)); err != nil {
			return err
		}
		return bucket.Put(
			[]byte("ssl-good"),
			sslValue("ssl-good", []string{"persisted-good.example.test"}, cert, key, 1),
		)
	}); err != nil {
		t.Fatalf("persist corrupt and valid SSL rows: %v", err)
	}
	if err := first.Stop(); err != nil {
		t.Fatalf("first Store.Stop() error = %v", err)
	}

	second, err := Open(path, make(chan *Event), testDataEncryption())
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	t.Cleanup(func() { _ = second.Stop() })
	if _, err := second.GetSSLCertificateForSNI("persisted-good.example.test"); err != nil {
		t.Fatalf("valid persisted SSL was suppressed by corrupt row: %v", err)
	}
	if _, err := second.GetSSLCertificateForSNI("corrupt.example.test"); err == nil {
		t.Fatal("corrupt persisted SSL was published")
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
