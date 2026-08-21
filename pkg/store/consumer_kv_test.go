package store

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestConsumerPluginLookupKey(t *testing.T) {
	tests := []struct {
		name       string
		pluginName string
		config     resource.PluginConfig
		want       string
		wantErr    bool
	}{
		{name: "key-auth", pluginName: "key-auth", config: map[string]any{"key": "k1"}, want: "k1"},
		{name: "basic-auth", pluginName: "basic-auth", config: map[string]any{"username": "alice"}, want: "alice"},
		{name: "jwt-auth", pluginName: "jwt-auth", config: map[string]any{"key": "j1"}, want: "j1"},
		{name: "hmac-auth", pluginName: "hmac-auth", config: map[string]any{"key_id": "h1"}, want: "h1"},
		{name: "ldap-auth", pluginName: "ldap-auth", config: map[string]any{"user_dn": "cn=bob"}, want: "cn=bob"},
		{name: "jwe-decrypt", pluginName: "jwe-decrypt", config: map[string]any{"key": "jwe-key"}, want: "jwe-key"},
		{
			name:       "jwe-decrypt non-string key",
			pluginName: "jwe-decrypt",
			config:     map[string]any{"key": 3},
			wantErr:    true,
		},
		{name: "wolf-rbac", pluginName: "wolf-rbac", config: map[string]any{"appid": "app-1"}, want: "app-1"},
		{name: "unsupported", pluginName: "unknown", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := consumerPluginLookupKey(test.pluginName, test.config)
			if (err != nil) != test.wantErr || (!test.wantErr && got != test.want) {
				t.Fatalf("consumerPluginLookupKey() = %q/%v, want %q/err=%t", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestConsumerKVDeleteRemovesAllLookupEntries(t *testing.T) {
	consumerStore := &Store{
		consumerKV:           map[string][]byte{"key-auth:k1": []byte("alice")},
		consumerIDs:          map[string][]byte{"alice": []byte("alice")},
		consumerToKeys:       map[string][]string{"alice": {"key-auth:k1"}},
		consumerReferenceKV:  map[string]map[string][]byte{"jwt-auth": {"alice": []byte("alice")}},
		consumerToReferences: map[string][]string{"alice": {"jwt-auth"}},
		consumerValues:       map[string]resource.Consumer{"alice": {Username: "alice"}},
	}

	if err := consumerStore.consumerKVDelete([]byte("alice")); err != nil {
		t.Fatalf("consumerKVDelete() error = %v", err)
	}
	if len(consumerStore.consumerKV) != 0 || len(consumerStore.consumerToKeys) != 0 ||
		len(consumerStore.consumerReferenceKV) != 0 || len(consumerStore.consumerToReferences) != 0 ||
		len(consumerStore.consumerValues) != 0 {
		t.Fatalf("consumer maps not cleared: %#v", consumerStore)
	}
}

func TestPrepareConsumerSnapshotRejectsNonStringJWEKey(t *testing.T) {
	storage := &Store{
		consumerKV:     map[string][]byte{},
		consumerToKeys: map[string][]string{},
		consumerValues: map[string]resource.Consumer{},
	}
	value := []byte(
		`{"username":"jwe-user","plugins":{"jwe-decrypt":{"key":123,"secret":"01234567890123456789012345678901"}}}`,
	)

	snapshot, err := storage.prepareConsumerSnapshot([]byte("jwe-user"), value)
	if err == nil {
		t.Fatalf("prepareConsumerSnapshot() = %+v, nil; want jwe-decrypt key type error", snapshot)
	}
}
