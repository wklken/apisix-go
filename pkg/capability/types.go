package capability

type SecretDeclarationSource string

const (
	SecretPluginConfig   SecretDeclarationSource = "plugin_config"
	SecretPluginMetadata SecretDeclarationSource = "plugin_metadata"
	SecretConsumerConfig SecretDeclarationSource = "consumer_config"
)

type SecretDeclaration struct {
	Factory string
	Source  SecretDeclarationSource
	Field   string
}

// SecretDeclarationCatalog is the immutable index of encrypted plugin fields.
// Its contents are copied when constructed and enumerated.
type SecretDeclarationCatalog struct {
	declarations []SecretDeclaration
	lookup       map[secretDeclarationKey]SecretDeclaration
	digest       [32]byte
}
