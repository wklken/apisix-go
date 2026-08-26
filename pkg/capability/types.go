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
	Strict  bool                        `yaml:"strict"`
	Target  SecretMaterializationTarget `yaml:"target,omitempty"`
}

func (d SecretDeclaration) EffectiveTarget() SecretMaterializationTarget {
	if d.Target == "" {
		return SecretMaterializationPlugin
	}
	return d.Target
}

type (
	Namespace        string
	Domain           string
	BehaviorStatus   string
	EvidenceKind     string
	EvidenceState    string
	DivergenceStatus string
)

const (
	NamespaceAPISIX Namespace = "apisix"
	NamespaceGoV1   Namespace = "apisix-go/v1"
	DomainHTTP      Domain    = "http"
	DomainStream    Domain    = "stream"

	BehaviorFull          BehaviorStatus = "full"
	BehaviorPartial       BehaviorStatus = "partial"
	BehaviorNotApplicable BehaviorStatus = "not_applicable"
	BehaviorDeferred      BehaviorStatus = "deferred"

	EvidenceSchema         EvidenceKind = "schema"
	EvidenceUnit           EvidenceKind = "unit"
	EvidenceUpstream       EvidenceKind = "converted_upstream"
	EvidenceDifferential   EvidenceKind = "differential"
	EvidenceRealDependency EvidenceKind = "real_dependency"
	EvidenceFailure        EvidenceKind = "failure"
	EvidenceRecovery       EvidenceKind = "recovery"

	EvidenceVerified      EvidenceState = "verified"
	EvidenceMissing       EvidenceState = "missing"
	EvidenceDeferred      EvidenceState = "deferred"
	EvidenceFlaky         EvidenceState = "flaky"
	EvidenceStale         EvidenceState = "stale"
	EvidenceNotApplicable EvidenceState = "not_applicable"

	DivergenceProposed DivergenceStatus = "proposed"
	DivergenceAccepted DivergenceStatus = "accepted"
	DivergenceRetired  DivergenceStatus = "retired"
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

type EvidenceClaim struct {
	State  EvidenceState `yaml:"state"`
	Refs   []string      `yaml:"refs"`
	Owner  string        `yaml:"owner"`
	Reason string        `yaml:"reason"`
}

type Evidence struct {
	Schema         EvidenceClaim `yaml:"schema"`
	Unit           EvidenceClaim `yaml:"unit"`
	Upstream       EvidenceClaim `yaml:"converted_upstream"`
	Differential   EvidenceClaim `yaml:"differential"`
	RealDependency EvidenceClaim `yaml:"real_dependency"`
	Failure        EvidenceClaim `yaml:"failure"`
	Recovery       EvidenceClaim `yaml:"recovery"`
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
	Behavior            BehaviorStatus      `yaml:"behavior"`
	BehaviorSummary     string              `yaml:"behavior_summary"`
	KnownGaps           []string            `yaml:"known_gaps"`
	Evidence            Evidence            `yaml:"evidence"`
	DivergenceIDs       []string            `yaml:"divergence_ids"`
	SupportedPlatforms  []string            `yaml:"supported_platforms"`
	SecretDeclarations  []SecretDeclaration `yaml:"secret_declarations"`
}

// SecretDeclarationCatalog is the immutable, manifest-owned index of encrypted
// plugin fields. Its contents are copied when constructed and enumerated.
type SecretDeclarationCatalog struct {
	declarations []SecretDeclaration
	lookup       map[secretDeclarationKey]SecretDeclaration
	digest       [32]byte
}

type QualificationProfile struct {
	Name             string         `yaml:"name"`
	Domains          []string       `yaml:"domains"`
	RequiredPlugins  []string       `yaml:"required_plugins"`
	RequiredEvidence []EvidenceKind `yaml:"required_evidence"`
}

type Divergence struct {
	ID               string           `yaml:"id"`
	Status           DivergenceStatus `yaml:"status"`
	Compatibility    string           `yaml:"compatibility_target"`
	ADR              string           `yaml:"adr"`
	OwnerApprovalRef string           `yaml:"owner_approval_ref"`
}

type Manifest struct {
	SchemaVersion         int                    `yaml:"schema_version"`
	Target                Target                 `yaml:"target"`
	Plugins               []PluginCapability     `yaml:"plugins"`
	QualificationProfiles []QualificationProfile `yaml:"qualification_profiles"`
	Divergences           []Divergence           `yaml:"divergences"`
	pluginsByName         map[string]int
	profilesByName        map[string]int
}
