package capability

type SecretDeclarationSource string

type SecretMaterializationTarget string

const (
	SecretPluginConfig   SecretDeclarationSource = "plugin_config"
	SecretPluginMetadata SecretDeclarationSource = "plugin_metadata"
	SecretConsumerConfig SecretDeclarationSource = "consumer_config"

	SecretMaterializationPlugin          SecretMaterializationTarget = "plugin"
	SecretMaterializationCompilerDiscard SecretMaterializationTarget = "compiler_discard"
)

type SecretDeclaration struct {
	Factory string                      `yaml:"factory"`
	Source  SecretDeclarationSource     `yaml:"source"`
	Field   string                      `yaml:"field"`
	Target  SecretMaterializationTarget `yaml:"target,omitempty"`
}

func (d SecretDeclaration) EffectiveTarget() SecretMaterializationTarget {
	if d.Target == "" {
		return SecretMaterializationPlugin
	}
	return d.Target
}

type (
	Namespace string
	Domain    string
)

const (
	NamespaceAPISIX Namespace = "apisix"
	NamespaceGoV1   Namespace = "apisix-go/v1"
	DomainHTTP      Domain    = "http"
	DomainStream    Domain    = "stream"
)

type Target struct {
	Name         string `yaml:"name"`
	Version      string `yaml:"version"`
	SourceCommit string `yaml:"source_commit"`
	Image        string `yaml:"image"`
}

type Factory struct {
	Key         string `yaml:"key"`
	ImportPath  string `yaml:"import_path"`
	ImportAlias string `yaml:"import_alias"`
	Constructor string `yaml:"constructor"`
}

type PluginCapability struct {
	Name                string              `yaml:"name"`
	Implementation      string              `yaml:"implementation"`
	Namespace           Namespace           `yaml:"namespace"`
	Domains             []Domain            `yaml:"domains"`
	APISIXDefault       bool                `yaml:"apisix_default"`
	Factories           []Factory           `yaml:"factories"`
	Phases              []string            `yaml:"phases"`
	Priority            int                 `yaml:"priority"`
	Scopes              []string            `yaml:"scopes"`
	InstanceScope       string              `yaml:"instance_scope"`
	ConditionalTerminal bool                `yaml:"conditional_terminal"`
	SecretDeclarations  []SecretDeclaration `yaml:"secret_declarations"`
}

// SecretDeclarationCatalog is the immutable, manifest-owned index of encrypted
// plugin fields. Its contents are copied when constructed and enumerated.
type SecretDeclarationCatalog struct {
	declarations []SecretDeclaration
	lookup       map[secretDeclarationKey]SecretDeclaration
	digest       [32]byte
}

type Manifest struct {
	SchemaVersion int                `yaml:"schema_version"`
	Target        Target             `yaml:"target"`
	Plugins       []PluginCapability `yaml:"plugins"`
	pluginsByName map[string]int
}
