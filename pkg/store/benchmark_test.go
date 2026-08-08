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
	data_encryption.Configure(true, []string{key})
	b.Cleanup(func() { data_encryption.Configure(false, nil) })

	configs := map[string]resource.PluginConfig{
		"key-auth":    map[string]any{"key": "api-secret"},
		"basic-auth":  map[string]any{"password": "pw"},
		"limit-count": map[string]any{"redis_password": "redis-secret"},
	}

	b.Run("decrypt-configs", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			decryptPluginConfigs(configs)
		}
	})
}
