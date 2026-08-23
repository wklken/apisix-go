package store

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/resource"
	bolt "go.etcd.io/bbolt"
)

func TestParseConsumerDecryptsEncryptedAuthPluginFields(t *testing.T) {
	key := "qeddd145sfvddff3"
	storage := &Store{dataEncryption: data_encryption.NewService(true, []string{key})}

	consumer, err := storage.ParseConsumer([]byte(`{
        "username":"alice",
        "plugins":{"key-auth":{"key":"` + encryptForTest(t, key, "api-secret") + `"}}
    }`))
	if err != nil {
		t.Fatalf("ParseConsumer() error = %v", err)
	}
	keyAuth := consumer.Plugins["key-auth"].(map[string]any)
	if got := keyAuth["key"]; got != "api-secret" {
		t.Fatalf("key-auth.key = %v, want decrypted value", got)
	}
}

func TestGetConsumerReturnsDeepCloneOnCacheHit(t *testing.T) {
	consumerStore := &Store{
		consumerValues: map[string]resource.Consumer{
			"alice": {
				Username: "alice",
				Plugins: map[string]resource.PluginConfig{
					"key-auth": map[string]any{
						"key":    "original",
						"nested": map[string]any{"value": "original"},
					},
				},
				Labels: map[string]any{"team": map[string]any{"name": "platform"}},
			},
		},
	}
	previous := ReplaceGlobalStoreForTest(consumerStore)
	t.Cleanup(func() { ReplaceGlobalStoreForTest(previous) })

	got, err := GetConsumer("alice")
	if err != nil {
		t.Fatalf("first GetConsumer() error = %v", err)
	}
	got.Plugins["key-auth"].(map[string]any)["key"] = "mutated"
	got.Plugins["key-auth"].(map[string]any)["nested"].(map[string]any)["value"] = "mutated"
	got.Labels["team"].(map[string]any)["name"] = "mutated"

	again, err := GetConsumer("alice")
	if err != nil {
		t.Fatalf("second GetConsumer() error = %v", err)
	}
	plugin := again.Plugins["key-auth"].(map[string]any)
	if plugin["key"] != "original" || plugin["nested"].(map[string]any)["value"] != "original" {
		t.Fatalf("cached plugin config was mutated: %#v", plugin)
	}
	if again.Labels["team"].(map[string]any)["name"] != "platform" {
		t.Fatalf("cached labels were mutated: %#v", again.Labels)
	}
}

func TestParseRoutePreservesStrictLoggerFieldsForPluginBoundary(t *testing.T) {
	key := "qeddd145sfvddff3"
	storage := &Store{dataEncryption: data_encryption.NewService(true, []string{key})}

	encrypted := encryptForTest(t, key, "Bearer secret")
	route, err := storage.ParseRoute([]byte(`{
        "uri":"/logs",
        "plugins":{"http-logger":{"uri":"http://127.0.0.1/logs","auth_header":"` + encrypted + `"}}
    }`))
	if err != nil {
		t.Fatalf("ParseRoute() error = %v", err)
	}
	loggerConfig := route.Plugins["http-logger"].(map[string]any)
	if got := loggerConfig["auth_header"]; got != encrypted {
		t.Fatalf("http-logger.auth_header = %v, want ciphertext preserved for strict plugin resolution", got)
	}
}

func TestDecodePluginMetadataDecryptsAzureMasterAPIKey(t *testing.T) {
	key := "qeddd145sfvddff3"
	storage := &Store{dataEncryption: data_encryption.NewService(true, []string{key})}

	var metadata struct {
		MasterAPIKey   string `json:"master_apikey"`
		MasterClientID string `json:"master_clientid"`
	}
	err := storage.decodePluginMetadata([]byte(`{
        "master_apikey":"`+encryptForTest(t, key, "master-key")+`",
        "master_clientid":"master-client"
    }`), "azure-functions", &metadata)
	if err != nil {
		t.Fatalf("decodePluginMetadata() error = %v", err)
	}
	if metadata.MasterAPIKey != "master-key" || metadata.MasterClientID != "master-client" {
		t.Fatalf("metadata = %#v, want decrypted master key", metadata)
	}
}

func TestDecodePluginMetadataPreservesUnregisteredLargeIntegers(t *testing.T) {
	storage := &Store{dataEncryption: data_encryption.NewService(true, []string{"qeddd145sfvddff3"})}

	var metadata struct {
		Sequence int64 `json:"sequence"`
	}
	if err := storage.decodePluginMetadata(
		[]byte(`{"sequence":9007199254740993}`),
		"example-plugin",
		&metadata,
	); err != nil {
		t.Fatalf("decodePluginMetadata() error = %v", err)
	}
	if metadata.Sequence != 9007199254740993 {
		t.Fatalf("sequence = %d, want exact large integer", metadata.Sequence)
	}
}

func TestConsumerKVConcurrentReadAndUpdate(t *testing.T) {
	consumerStore := &Store{
		consumerKV:     make(map[string][]byte),
		consumerToKeys: make(map[string][]string),
	}
	value := []byte(`{"username":"alice","plugins":{"key-auth":{"key":"api-key"}}}`)

	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for range 1000 {
			if err := consumerStore.consumerKVAdd([]byte("alice"), value); err != nil {
				t.Errorf("consumerKVAdd() error = %v", err)
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		for range 1000 {
			_, _ = consumerStore.GetConsumerNameByPluginKey("key-auth", "api-key")
		}
	}()
	group.Wait()
}

func TestConsumerKVDoesNotIndexUnresolvedKeyAuthReference(t *testing.T) {
	consumerStore := &Store{
		consumerKV:     make(map[string][]byte),
		consumerToKeys: make(map[string][]string),
	}
	value := []byte(`{"username":"alice","plugins":{"key-auth":{"key":"$env://MISSING_KEY"}}}`)

	if err := consumerStore.consumerKVAdd([]byte("alice"), value); err != nil {
		t.Fatalf("consumerKVAdd() error = %v", err)
	}
	if _, err := consumerStore.GetConsumerNameByPluginKey(
		"key-auth",
		"$env://MISSING_KEY",
	); !errors.Is(
		err,
		ErrNotFound,
	) {
		t.Fatalf("unresolved key lookup error = %v, want ErrNotFound", err)
	}
}

func encryptForTest(t *testing.T, key string, value string) string {
	t.Helper()
	padding := aes.BlockSize - len(value)%aes.BlockSize
	padded := append([]byte(value), make([]byte, padding)...)
	for i := len(padded) - padding; i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}

func seededGetterStore(t *testing.T) *Store {
	t.Helper()
	db, err := bolt.Open(t.TempDir()+"/getters.db", 0o600, nil)
	if err != nil {
		t.Fatalf("open getter database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	storage := &Store{
		db:             db,
		dataEncryption: data_encryption.NewService(false, nil),
		consumerValues: map[string]resource.Consumer{},
	}
	if err := storage.InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}

	seed := map[string]map[string]string{
		"routes":          {"route-1": `{"id":"route-1","uri":"/orders"}`},
		"services":        {"service-1": `{"id":"service-1","upstream_id":"upstream-1"}`},
		"upstreams":       {"upstream-1": `{"id":"upstream-1","scheme":"http","nodes":{"backend.test:80":1}}`},
		"global_rules":    {"rule-1": `{"id":"rule-1","plugins":{"limit-req":{}}}`},
		"plugin_configs":  {"config-1": `{"id":"config-1","plugins":{}}`},
		"plugin_metadata": {"metadata-1": `{"id":"metadata-1"}`},
		"consumers":       {"alice": `{"username":"alice","plugins":{}}`},
		"consumer_groups": {"group-1": `{"id":"group-1","plugins":{}}`},
		"protos":          {"proto-1": `{"id":"proto-1","content":"syntax = proto3;"}`},
		"ssls":            {"ssl-1": `{"id":"ssl-1","snis":["api.test"],"cert":"CERT","key":"KEY"}`},
		"stream_routes": {
			"stream-1": `{"id":"stream-1","server_addr":"127.0.0.1","server_port":9100,"upstream":{"nodes":{"127.0.0.1:9092":1}}}`,
		},
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for bucket, entries := range seed {
			b := tx.Bucket([]byte(bucket))
			for key, value := range entries {
				if err := b.Put([]byte(key), []byte(value)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed getter database: %v", err)
	}
	return storage
}

func TestStoreGettersResolveBucketResources(t *testing.T) {
	previous := s
	t.Cleanup(func() { s = previous })
	s = seededGetterStore(t)

	upstream, err := GetUpstream("upstream-1")
	if err != nil || upstream.Scheme != "http" {
		t.Fatalf("GetUpstream() = %+v/%v, want http scheme", upstream, err)
	}
	if _, err := GetUpstream("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUpstream(missing) error = %v, want ErrNotFound", err)
	}

	service, err := GetService("service-1")
	if err != nil || service.UpstreamID != "upstream-1" {
		t.Fatalf("GetService() = %+v/%v", service, err)
	}
	if _, err := GetService("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetService(missing) error = %v, want ErrNotFound", err)
	}

	consumer, err := GetConsumer("alice")
	if err != nil || consumer.Username != "alice" {
		t.Fatalf("GetConsumer() = %+v/%v", consumer, err)
	}
	if _, err := GetConsumer("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetConsumer(missing) error = %v, want ErrNotFound", err)
	}

	group, err := GetConsumerGroup("group-1")
	if err != nil || len(group.Plugins) != 0 {
		t.Fatalf("GetConsumerGroup() = %+v/%v", group, err)
	}
	if _, err := GetConsumerGroup("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetConsumerGroup(missing) error = %v, want ErrNotFound", err)
	}

	configRule, err := GetPluginConfigRule("config-1")
	if err != nil || len(configRule.Plugins) != 0 {
		t.Fatalf("GetPluginConfigRule() = %+v/%v", configRule, err)
	}
	if _, err := GetPluginConfigRule("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPluginConfigRule(missing) error = %v, want ErrNotFound", err)
	}

	proto, err := GetProto("proto-1")
	if err != nil || proto.Content == "" {
		t.Fatalf("GetProto() = %+v/%v", proto, err)
	}
	if _, err := GetProto("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProto(missing) error = %v, want ErrNotFound", err)
	}

	ssl, err := GetSSL("ssl-1")
	if err != nil || len(ssl.Snis) != 1 || ssl.Snis[0] != "api.test" {
		t.Fatalf("GetSSL() = %+v/%v", ssl, err)
	}
	if _, err := GetSSL("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSSL(missing) error = %v, want ErrNotFound", err)
	}

	var metadata resource.PluginConfig
	if err := GetPluginMetadata("metadata-1", &metadata); err != nil {
		t.Fatalf("GetPluginMetadata() error = %v", err)
	}
	if err := GetPluginMetadata("missing", &metadata); err == nil {
		t.Fatal("GetPluginMetadata(missing) error = nil, want decode failure")
	}
}

func TestStoreListersReturnSeededResources(t *testing.T) {
	previous := s
	t.Cleanup(func() { s = previous })
	s = seededGetterStore(t)

	routes, err := ListRoutes()
	if err != nil || len(routes) != 1 || routes[0].ID != "route-1" {
		t.Fatalf("ListRoutes() = %+v/%v", routes, err)
	}
	streamRoutes, err := ListStreamRoutes()
	if err != nil || len(streamRoutes) != 1 || streamRoutes[0].ServerPort != 9100 {
		t.Fatalf("ListStreamRoutes() = %+v/%v", streamRoutes, err)
	}
	ssls, err := ListSSLs()
	if err != nil || len(ssls) != 1 || ssls[0].ID != "ssl-1" {
		t.Fatalf("ListSSLs() = %+v/%v", ssls, err)
	}
	globalRules, err := ListGlobalRules()
	if err != nil || len(globalRules) != 1 {
		t.Fatalf("ListGlobalRules() = %+v/%v", globalRules, err)
	}
}

func TestStoreListersFailOnUndecodableEntries(t *testing.T) {
	previous := s
	t.Cleanup(func() { s = previous })
	storage := seededGetterStore(t)
	s = storage

	if err := storage.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("routes")).Put([]byte("bad"), []byte("{not json"))
	}); err != nil {
		t.Fatalf("seed malformed route: %v", err)
	}
	if _, err := ListRoutes(); err == nil {
		t.Fatal("ListRoutes() error = nil with a malformed entry")
	}

	if err := storage.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("global_rules")).Put([]byte("bad"), []byte("{not json"))
	}); err != nil {
		t.Fatalf("seed malformed global rule: %v", err)
	}
	if _, err := ListGlobalRules(); err == nil {
		t.Fatal("ListGlobalRules() error = nil with a malformed entry")
	}
}

func TestRouteIDForDecodeError(t *testing.T) {
	if got := routeIDForDecodeError([]byte(`{"id":"route-9"}`)); got != "route-9" {
		t.Fatalf("routeIDForDecodeError() = %q, want route-9", got)
	}
	if got := routeIDForDecodeError([]byte(`{bad`)); got != "unknown" {
		t.Fatalf("routeIDForDecodeError(bad) = %q, want unknown", got)
	}
}
