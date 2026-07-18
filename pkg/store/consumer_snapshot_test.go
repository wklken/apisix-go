package store

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
	bolt "go.etcd.io/bbolt"
)

func TestConsumerSnapshotRejectsInvalidBasicAuthUpdateAndKeepsLastGood(t *testing.T) {
	consumerStore := &Store{
		consumerKV:     make(map[string][]byte),
		consumerToKeys: make(map[string][]string),
	}
	valid := []byte(`{"username":"foo","plugins":{"basic-auth":{"username":"foo","password":"bar"}}}`)
	if err := consumerStore.consumerKVAdd([]byte("foo"), valid); err != nil {
		t.Fatalf("seed consumerKVAdd() error = %v", err)
	}

	invalid := []byte(`{"username":"foo","plugins":{"basic-auth":{"username":"foo"}}}`)
	err := consumerStore.consumerKVAdd([]byte("foo"), invalid)
	if err == nil || !strings.Contains(err.Error(), `missing properties: 'password'`) {
		t.Fatalf("consumerKVAdd() error = %v, want missing password diagnostic", err)
	}
	if got := string(consumerStore.consumerKV["basic-auth:foo"]); got != "foo" {
		t.Fatalf("last-good basic-auth index = %q, want foo", got)
	}
	if got := strings.Join(consumerStore.consumerToKeys["foo"], ","); got != "basic-auth:foo" {
		t.Fatalf("consumerToKeys[foo] = %q, want last-good basic-auth index", got)
	}
}

func TestResolveConsumerSecretsFromEnvironment(t *testing.T) {
	t.Setenv("BASIC_AUTH_PASSWORD", "bar")
	consumer := resource.Consumer{Plugins: map[string]resource.PluginConfig{
		"basic-auth": map[string]any{"username": "foo", "password": "$ENV://BASIC_AUTH_PASSWORD"},
	}}
	if err := (&Store{}).resolveConsumerSecrets(&consumer); err != nil {
		t.Fatalf("resolveConsumerSecrets() error = %v", err)
	}
	config := consumer.Plugins["basic-auth"].(map[string]any)
	if got := config["password"]; got != "bar" {
		t.Fatalf("resolved password = %#v, want bar", got)
	}
}

func TestConsumerKVAddCachesResolvedEnvironmentCredential(t *testing.T) {
	t.Setenv("BASIC_AUTH_PASSWORD", "bar")
	consumerStore := &Store{
		consumerKV:     make(map[string][]byte),
		consumerToKeys: make(map[string][]string),
	}
	value := []byte(
		`{"username":"foo","plugins":{"basic-auth":{"username":"foo","password":"$ENV://BASIC_AUTH_PASSWORD"}}}`,
	)
	if err := consumerStore.consumerKVAdd([]byte("foo"), value); err != nil {
		t.Fatalf("consumerKVAdd() error = %v", err)
	}
	consumer, ok := consumerStore.consumerValues["foo"]
	if !ok {
		t.Fatal("resolved consumer was not cached")
	}
	config := consumer.Plugins["basic-auth"].(map[string]any)
	if got := config["password"]; got != "bar" {
		t.Fatalf("cached password = %#v, want bar", got)
	}
}

func TestResolveConsumerSecretsFromVaultWithEnvironmentToken(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "root")
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Vault method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/kv/apisix/foo" {
			t.Errorf("Vault path = %q, want /v1/kv/apisix/foo", r.URL.Path)
		}
		if got := r.Header.Get("X-Vault-Token"); got != "root" {
			t.Errorf("X-Vault-Token = %q, want root", got)
		}
		if got := r.Header.Get("X-Vault-Namespace"); got != "team-a" {
			t.Errorf("X-Vault-Namespace = %q, want team-a", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{"passwd":"bar"}}`)
	}))
	defer vault.Close()

	consumerStore := newConsumerSnapshotStore(t)
	vaultConfig := []byte(
		`{"id":"vault/test1","uri":"` + vault.URL +
			`","prefix":"kv/apisix","token":"$ENV://VAULT_TOKEN","namespace":"team-a"}`,
	)
	if err := consumerStore.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("secrets")).Put(
			[]byte("vault/test1"),
			vaultConfig,
		)
	}); err != nil {
		t.Fatalf("store Vault resource: %v", err)
	}
	consumer := resource.Consumer{Plugins: map[string]resource.PluginConfig{
		"basic-auth": map[string]any{
			"username": "foo",
			"password": "$secret://vault/test1/foo/passwd",
		},
	}}
	if err := consumerStore.resolveConsumerSecrets(&consumer); err != nil {
		t.Fatalf("resolveConsumerSecrets() error = %v", err)
	}
	config := consumer.Plugins["basic-auth"].(map[string]any)
	if got := config["password"]; got != "bar" {
		t.Fatalf("resolved password = %#v, want bar", got)
	}
}

func TestResolveConsumerSecretsRejectsNonHTTPVaultURI(t *testing.T) {
	consumerStore := newConsumerSnapshotStore(t)
	if err := consumerStore.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("secrets")).Put(
			[]byte("vault/test1"),
			[]byte(`{"id":"vault/test1","uri":"file:///tmp/vault","prefix":"kv/apisix","token":"root"}`),
		)
	}); err != nil {
		t.Fatalf("store Vault resource: %v", err)
	}
	consumer := resource.Consumer{Plugins: map[string]resource.PluginConfig{
		"basic-auth": map[string]any{
			"username": "foo",
			"password": "$secret://vault/test1/foo/passwd",
		},
	}}
	err := consumerStore.resolveConsumerSecrets(&consumer)
	if err == nil || !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("resolveConsumerSecrets() error = %v, want HTTP(S)-only rejection", err)
	}
}

func TestConsumerEventRejectsInvalidUpdateBeforePersistingIt(t *testing.T) {
	consumerStore := newConsumerSnapshotStore(t)
	events := consumerStore.events

	valid := []byte(`{"username":"foo","plugins":{"basic-auth":{"username":"foo","password":"bar"}}}`)
	events <- &Event{Type: EventTypePut, Key: []byte("/apisix/consumers/foo"), Value: valid}
	consumerStore.Sync()
	invalid := []byte(`{"username":"foo","plugins":{"basic-auth":{"username":"foo"}}}`)
	events <- &Event{Type: EventTypePut, Key: []byte("/apisix/consumers/foo"), Value: invalid}
	consumerStore.Sync()

	if got := consumerStore.GetFromBucket("consumers", []byte("foo")); !bytes.Equal(got, valid) {
		t.Fatalf("persisted consumer = %s, want last-good %s", got, valid)
	}
}

func newConsumerSnapshotStore(t *testing.T) *Store {
	t.Helper()
	db, err := bolt.Open(filepath.Join(t.TempDir(), "consumer-snapshot.db"), 0o600, nil)
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	events := make(chan *Event, 4)
	consumerStore := &Store{
		events:         events,
		db:             db,
		consumerKV:     make(map[string][]byte),
		consumerToKeys: make(map[string][]string),
	}
	consumerStore.InitBuckets()
	consumerStore.Start()
	t.Cleanup(consumerStore.Stop)
	return consumerStore
}
