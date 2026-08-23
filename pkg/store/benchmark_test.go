package store

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/resource"
)

// BenchmarkVerifiedSmallPath measures plugin-config decryption at resource
// publication for a route carrying a few plugins.
func BenchmarkVerifiedSmallPath(b *testing.B) {
	key := "qeddd145sfvddff3"
	storage := &Store{dataEncryption: data_encryption.NewService(true, []string{key})}

	configs := map[string]resource.PluginConfig{
		"key-auth":    map[string]any{"key": "api-secret"},
		"basic-auth":  map[string]any{"password": "pw"},
		"limit-count": map[string]any{"redis_password": "redis-secret"},
	}

	b.Run("decrypt-configs", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			storage.decryptPluginConfigs(configs)
		}
	})
}
