package store

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
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

func TestConsumerKVAddCachesRawEnvironmentCredential(t *testing.T) {
	t.Setenv("BASIC_AUTH_PASSWORD", "late-value")
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
		t.Fatal("raw consumer was not cached")
	}
	config := consumer.Plugins["basic-auth"].(map[string]any)
	if got := config["password"]; got != "$ENV://BASIC_AUTH_PASSWORD" {
		t.Fatalf("cached password = %#v, want unresolved environment reference", got)
	}
}

func TestGetConsumerByPluginKeyRetriesVaultAfterLateProvisioning(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "root")
	var requests atomic.Int32
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
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
		if requestNumber == 1 {
			http.Error(w, "not provisioned", http.StatusNotFound)
			return
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
	value := []byte(
		`{"username":"foo","plugins":{"basic-auth":{"username":"foo","password":"$secret://vault/test1/foo/passwd"}}}`,
	)
	if err := consumerStore.consumerKVAdd([]byte("foo"), value); err != nil {
		t.Fatalf("consumerKVAdd() error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("Vault requests during consumer add = %d, want 0", got)
	}

	if _, err := consumerStore.getConsumerByPluginKey("basic-auth", "foo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("first lookup error = %v, want fail-closed ErrNotFound", err)
	}
	consumer, err := consumerStore.getConsumerByPluginKey("basic-auth", "foo")
	if err != nil {
		t.Fatalf("second lookup error = %v", err)
	}
	config := consumer.Plugins["basic-auth"].(map[string]any)
	if got := config["password"]; got != "bar" {
		t.Fatalf("resolved password = %#v, want bar", got)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("Vault requests after two lookups = %d, want retry count 2", got)
	}
	raw := consumerStore.consumerValues["foo"].Plugins["basic-auth"].(map[string]any)
	if got := raw["password"]; got != "$secret://vault/test1/foo/passwd" {
		t.Fatalf("raw cached password = %#v, want managed secret reference", got)
	}
}

func TestGetConsumerByPluginKeyRejectsNonHTTPVaultURI(t *testing.T) {
	consumerStore := newConsumerSnapshotStore(t)
	if err := consumerStore.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("secrets")).Put(
			[]byte("vault/test1"),
			[]byte(`{"id":"vault/test1","uri":"file:///tmp/vault","prefix":"kv/apisix","token":"root"}`),
		)
	}); err != nil {
		t.Fatalf("store Vault resource: %v", err)
	}
	value := []byte(
		`{"username":"foo","plugins":{"basic-auth":{"username":"foo","password":"$secret://vault/test1/foo/passwd"}}}`,
	)
	if err := consumerStore.consumerKVAdd([]byte("foo"), value); err != nil {
		t.Fatalf("consumerKVAdd() error = %v", err)
	}
	_, err := consumerStore.getConsumerByPluginKey("basic-auth", "foo")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("lookup error = %v, want fail-closed HTTP(S)-only rejection", err)
	}
}

func TestConsumerEventPersistsRawManagedSecretWithoutVaultFetch(t *testing.T) {
	var requests atomic.Int32
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "not provisioned", http.StatusNotFound)
	}))
	defer vault.Close()

	consumerStore := newConsumerSnapshotStore(t)
	if err := consumerStore.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("secrets")).Put(
			[]byte("vault/test1"),
			[]byte(`{"id":"vault/test1","uri":"`+vault.URL+`","prefix":"kv/apisix","token":"root"}`),
		)
	}); err != nil {
		t.Fatalf("store Vault resource: %v", err)
	}
	raw := []byte(
		`{"username":"foo","plugins":{"basic-auth":{"username":"foo","password":"$secret://vault/test1/foo/passwd"}}}`,
	)
	consumerStore.events <- &Event{
		Type: EventTypePut, Key: []byte("/apisix/consumers/foo"), Value: raw,
	}
	consumerStore.Sync()

	if got := requests.Load(); got != 0 {
		t.Fatalf("Vault requests while persisting consumer = %d, want 0", got)
	}
	if got := consumerStore.GetFromBucket("consumers", []byte("foo")); !bytes.Equal(got, raw) {
		t.Fatalf("persisted consumer = %s, want raw snapshot %s", got, raw)
	}
	cached := consumerStore.consumerValues["foo"].Plugins["basic-auth"].(map[string]any)
	if got := cached["password"]; got != "$secret://vault/test1/foo/passwd" {
		t.Fatalf("cached password = %#v, want unresolved managed reference", got)
	}
}

func TestGetConsumerByPluginKeyDoesNotIndexMissingManagedReference(t *testing.T) {
	consumerStore := newConsumerSnapshotStore(t)
	const reference = "$secret://vault/missing/jack/key"
	value := []byte(`{"username":"jack","plugins":{"key-auth":{"key":"` + reference + `"}}}`)
	if err := consumerStore.consumerKVAdd([]byte("jack"), value); err != nil {
		t.Fatalf("consumerKVAdd() error = %v", err)
	}
	if _, err := consumerStore.GetConsumerNameByPluginKey("key-auth", reference); !errors.Is(err, ErrNotFound) {
		t.Fatalf("literal managed reference index error = %v, want ErrNotFound", err)
	}
	if _, err := consumerStore.getConsumerByPluginKey("key-auth", "late-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing managed secret lookup error = %v, want fail-closed ErrNotFound", err)
	}
}

func TestGetConsumerByPluginKeyFailsClosedUntilBasicEnvironmentSecretExists(t *testing.T) {
	unsetEnvForTest(t, "BASIC_AUTH_LATE_PASSWORD")
	consumerStore := newInMemoryConsumerStore()
	value := []byte(
		`{"username":"foo","plugins":{"basic-auth":{"username":"foo","password":"$ENV://BASIC_AUTH_LATE_PASSWORD"}}}`,
	)
	if err := consumerStore.consumerKVAdd([]byte("foo"), value); err != nil {
		t.Fatalf("consumerKVAdd() error = %v", err)
	}
	if _, err := consumerStore.getConsumerByPluginKey("basic-auth", "foo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup before provisioning error = %v, want ErrNotFound", err)
	}
	if err := os.Setenv("BASIC_AUTH_LATE_PASSWORD", "bar"); err != nil {
		t.Fatalf("Setenv() error = %v", err)
	}
	consumer, err := consumerStore.getConsumerByPluginKey("basic-auth", "foo")
	if err != nil {
		t.Fatalf("lookup after provisioning error = %v", err)
	}
	config := consumer.Plugins["basic-auth"].(map[string]any)
	if got := config["password"]; got != "bar" {
		t.Fatalf("resolved password = %#v, want bar", got)
	}
	raw := consumerStore.consumerValues["foo"].Plugins["basic-auth"].(map[string]any)
	if got := raw["password"]; got != "$ENV://BASIC_AUTH_LATE_PASSWORD" {
		t.Fatalf("raw password = %#v, want unresolved reference", got)
	}
}

func TestGetConsumerByPluginKeyFailsClosedForMissingEnvironmentPath(t *testing.T) {
	t.Setenv("BASIC_AUTH_JSON_PASSWORD", `{"other":"bar"}`)
	consumerStore := newInMemoryConsumerStore()
	value := []byte(
		`{"username":"foo","plugins":{"basic-auth":{"username":"foo","password":"$ENV://BASIC_AUTH_JSON_PASSWORD/password"}}}`,
	)
	if err := consumerStore.consumerKVAdd([]byte("foo"), value); err != nil {
		t.Fatalf("consumerKVAdd() error = %v", err)
	}
	if _, err := consumerStore.getConsumerByPluginKey("basic-auth", "foo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup error = %v, want fail-closed ErrNotFound", err)
	}
}

func TestGetConsumerByPluginKeyResolvesOnlySelectedPlugin(t *testing.T) {
	unsetEnvForTest(t, "UNRELATED_KEY_AUTH_KEY")
	consumerStore := newInMemoryConsumerStore()
	value := []byte(`{
		"username":"foo",
		"plugins":{
			"basic-auth":{"username":"foo","password":"bar"},
			"key-auth":{"key":"$ENV://UNRELATED_KEY_AUTH_KEY"}
		}
	}`)
	if err := consumerStore.consumerKVAdd([]byte("foo"), value); err != nil {
		t.Fatalf("consumerKVAdd() error = %v", err)
	}
	consumer, err := consumerStore.getConsumerByPluginKey("basic-auth", "foo")
	if err != nil {
		t.Fatalf("basic-auth lookup error = %v", err)
	}
	if _, ok := consumer.Plugins["key-auth"]; !ok {
		t.Fatal("returned consumer lost unrelated key-auth config")
	}
	consumer.Plugins["key-auth"].(map[string]any)["key"] = "mutated-return-value"
	rawKeyAuth := consumerStore.consumerValues["foo"].Plugins["key-auth"].(map[string]any)
	if got := rawKeyAuth["key"]; got != "$ENV://UNRELATED_KEY_AUTH_KEY" {
		t.Fatalf("raw unrelated plugin key = %#v, want unresolved reference", got)
	}
	if _, err := consumerStore.getConsumerByPluginKey("key-auth", "late-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unresolved key-auth lookup error = %v, want ErrNotFound", err)
	}
}

func TestGetConsumerByPluginKeyResolvesReferencedAuthKeysLazily(t *testing.T) {
	tests := []struct {
		name           string
		plugin         string
		lookupField    string
		secretField    string
		lookupEnv      string
		secretEnv      string
		resolvedKey    string
		resolvedSecret string
	}{
		{
			name: "key auth", plugin: "key-auth", lookupField: "key",
			lookupEnv: "LAZY_KEY_AUTH_KEY", resolvedKey: "key-value",
		},
		{
			name: "jwt auth", plugin: "jwt-auth", lookupField: "key", secretField: "secret",
			lookupEnv: "LAZY_JWT_AUTH_KEY", secretEnv: "LAZY_JWT_AUTH_SECRET",
			resolvedKey: "jwt-key", resolvedSecret: "jwt-secret",
		},
		{
			name: "hmac auth", plugin: "hmac-auth", lookupField: "key_id", secretField: "secret_key",
			lookupEnv: "LAZY_HMAC_AUTH_KEY", secretEnv: "LAZY_HMAC_AUTH_SECRET",
			resolvedKey: "hmac-key", resolvedSecret: "hmac-secret",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unsetEnvForTest(t, test.lookupEnv)
			if test.secretEnv != "" {
				unsetEnvForTest(t, test.secretEnv)
			}
			consumerStore := newInMemoryConsumerStore()
			pluginConfig := map[string]any{test.lookupField: "$ENV://" + test.lookupEnv}
			if test.secretField != "" {
				pluginConfig[test.secretField] = "$ENV://" + test.secretEnv
			}
			consumer := resource.Consumer{Username: test.name, Plugins: map[string]resource.PluginConfig{
				test.plugin: pluginConfig,
			}}
			value, err := json.Marshal(consumer)
			if err != nil {
				t.Fatalf("marshal consumer: %v", err)
			}
			if err := consumerStore.consumerKVAdd([]byte(test.name), value); err != nil {
				t.Fatalf("consumerKVAdd() error = %v", err)
			}
			reference := "$ENV://" + test.lookupEnv
			_, err = consumerStore.GetConsumerNameByPluginKey(test.plugin, reference)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("literal reference index error = %v, want ErrNotFound", err)
			}
			_, err = consumerStore.getConsumerByPluginKey(test.plugin, test.resolvedKey)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("lookup before provisioning error = %v, want ErrNotFound", err)
			}
			if err := os.Setenv(test.lookupEnv, test.resolvedKey); err != nil {
				t.Fatalf("Setenv lookup key: %v", err)
			}
			if test.secretEnv != "" {
				if err := os.Setenv(test.secretEnv, test.resolvedSecret); err != nil {
					t.Fatalf("Setenv secret: %v", err)
				}
			}
			resolved, err := consumerStore.getConsumerByPluginKey(test.plugin, test.resolvedKey)
			if err != nil {
				t.Fatalf("lookup after provisioning error = %v", err)
			}
			config := resolved.Plugins[test.plugin].(map[string]any)
			if got := config[test.lookupField]; got != test.resolvedKey {
				t.Fatalf("resolved lookup field = %#v, want %q", got, test.resolvedKey)
			}
			if test.secretField != "" {
				if got := config[test.secretField]; got != test.resolvedSecret {
					t.Fatalf("resolved secret field = %#v, want %q", got, test.resolvedSecret)
				}
			}
			raw := consumerStore.consumerValues[test.name].Plugins[test.plugin].(map[string]any)
			if got := raw[test.lookupField]; got != reference {
				t.Fatalf("raw lookup field = %#v, want %q", got, reference)
			}
		})
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

func newInMemoryConsumerStore() *Store {
	return &Store{
		consumerKV:     make(map[string][]byte),
		consumerToKeys: make(map[string][]string),
		consumerValues: make(map[string]resource.Consumer),
	}
}

func unsetEnvForTest(t *testing.T, name string) {
	t.Helper()
	value, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("Unsetenv(%q) error = %v", name, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, value)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}
