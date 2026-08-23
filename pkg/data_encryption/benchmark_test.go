package data_encryption

import "testing"

// BenchmarkVerifiedSmallPath measures the encrypted-field lookup for a route
// carrying a few plugins against the full registered plugin field table.
func BenchmarkVerifiedSmallPath(b *testing.B) {
	keyring := []string{"qeddd145sfvddff3"}
	catalog := mustTestDeclarationCatalog()
	configs := map[string]any{
		"key-auth":    map[string]any{"key": "api-secret"},
		"basic-auth":  map[string]any{"password": "pw"},
		"limit-count": map[string]any{"redis_password": "redis-secret"},
	}

	b.Run("decrypt-configs", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			DecryptPluginConfigs(configs, keyring, catalog)
		}
	})
}
