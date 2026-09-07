package config

import (
	"reflect"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestReadStandaloneSnapshotPreservesConsumerSecretInputs(t *testing.T) {
	for _, test := range []struct {
		name    string
		plugins string
	}{
		{
			name: "literal Redis consumer configuration",
			plugins: `{"key-auth":{"key":"jack1"},"limit-count":{
"count":2,"time_window":60,"key":"remote_addr","policy":"redis","redis_host":"127.0.0.1"}}`,
		},
		{
			name:    "invalid raw JWE length remains visible to validation",
			plugins: `{"jwe-decrypt":{"key":"user-key","secret":"123456789012345678901234567890123"}}`,
		},
		{
			name:    "invalid base64 JWE length remains visible to validation",
			plugins: `{"jwe-decrypt":{"key":"user-key","secret":"MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIz","is_base64_encoded":true}}`,
		},
		{
			name: "explicit references remain owned by consumer materialization",
			plugins: `{"key-auth":{"key":"$ENV://CONSUMER_KEY"},"jwe-decrypt":{
"key":"$secret://vault/consumer/key","secret":"$encrypted://opaque"}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, provider := range []string{standaloneProviderYAML, standaloneProviderJSON} {
				t.Run(provider, func(t *testing.T) {
					raw := `{"username":"jack1","plugins":` + test.plugins + `}`
					document := `{"consumers":[` + raw + `]}`
					if provider == standaloneProviderYAML {
						document += "\n#END\n"
					}
					path := writeStandaloneTestConfig(t, document)
					snapshot, err := readStandaloneSnapshot(path, provider,
						testStandaloneDataEncryption(t, true, []string{"qeddd145sfvddff3"}))
					if err != nil {
						t.Fatalf("readStandaloneSnapshot() error = %v", err)
					}
					var want, got map[string]any
					if err := json.Unmarshal([]byte(raw), &want); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(snapshot["consumers"]["jack1"], &got); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatal(
							"standalone loader changed consumer inputs before consumer validation and materialization",
						)
					}
				})
			}
		})
	}
}
