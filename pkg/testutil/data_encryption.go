package testutil

import (
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
)

// DataEncryptionService builds test encryption state from the embedded
// manifest so test fixtures cannot drift from the runtime declaration catalog.
func DataEncryptionService(enabled bool, keyring []string) data_encryption.Service {
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		panic(err)
	}
	return data_encryption.NewService(enabled, keyring, catalog)
}

func DataEncryptionResolver(enabled bool, keyring []string) data_encryption.Resolver {
	return DataEncryptionService(enabled, keyring).Resolver()
}

func UnconfiguredDataEncryptionService() data_encryption.Service {
	return data_encryption.Service{}
}
