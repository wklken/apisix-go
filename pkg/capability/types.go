package capability

type SecretDeclarationSource string

const (
	SecretPluginConfig   SecretDeclarationSource = "plugin_config"
	SecretPluginMetadata SecretDeclarationSource = "plugin_metadata"
	SecretConsumerConfig SecretDeclarationSource = "consumer_config"
	SecretSSLConfig      SecretDeclarationSource = "ssl_config"
)

// SSLResourceFactory identifies the built-in SSL resource owner, not a plugin.
const SSLResourceFactory = "ssl"

type SecretDeclaration struct {
	Factory string
	Source  SecretDeclarationSource
	Field   string
}

// SecretDeclarationCatalog is the immutable index of declared secret fields.
// Its contents are copied when constructed and enumerated.
type SecretDeclarationCatalog struct {
	declarations []SecretDeclaration
	lookup       map[secretDeclarationKey]SecretDeclaration
	digest       [32]byte
}
