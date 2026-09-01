package generation

type Domain string

const (
	DomainHTTP   Domain = "http"
	DomainStream Domain = "stream"
)

type RevisionSet struct {
	Desired uint64
	HTTP    uint64
	Stream  uint64
}

type GenerationArtifact struct {
	Domain   Domain
	Revision uint64
	Digest   [32]byte
	Snapshot string
}

type ResourceKey struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ResourceOrigin preserves the provider identity APISIX uses for plugin
// configuration versioning. ResourceKey is the exact provider key; it must not
// be reconstructed from a normalized prefix.
type ResourceOrigin struct {
	Provider      string `json:"provider,omitempty"`
	ResourceKey   string `json:"resource_key,omitempty"`
	ModifiedIndex string `json:"modified_index,omitempty"`
}

type Resource struct {
	Key    ResourceKey    `json:"key"`
	Origin ResourceOrigin `json:"origin,omitzero"`
	Value  []byte         `json:"value"`
}

type Tombstone struct {
	Key      ResourceKey `json:"key"`
	Revision uint64      `json:"revision"`
}

type Snapshot struct {
	revision           uint64
	resources          []Resource
	tombstones         []Tombstone
	collectionVersions map[string]string
	digest             [32]byte
}

type MutationType string

const (
	MutationPut    MutationType = "put"
	MutationDelete MutationType = "delete"
)

type Mutation struct {
	Type   MutationType
	Key    ResourceKey
	Origin ResourceOrigin
	Value  []byte
}

type ProviderCursor struct {
	Provider string
	Revision string
}

type DesiredBatch struct {
	Cursor             ProviderCursor
	ReplaceManaged     bool
	Mutations          []Mutation
	CollectionVersions map[string]string
	RequiredDomains    []Domain
}

type ApplyTicket struct {
	DesiredRevision uint64
	DesiredDigest   [32]byte
	Cursor          ProviderCursor
	RequiredDomains []Domain
}

type ResourceDisposition string

const (
	DispositionPublished   ResourceDisposition = "published"
	DispositionLastGood    ResourceDisposition = "last-good"
	DispositionQuarantined ResourceDisposition = "quarantined"
	DispositionFailClosed  ResourceDisposition = "fail-closed"
	DispositionDeleted     ResourceDisposition = "deleted"
)

type ResourceDecision struct {
	Key         ResourceKey
	Disposition ResourceDisposition
	Code        string
}

type PublicationCandidate struct {
	Artifact  GenerationArtifact
	Snapshot  Snapshot
	Closure   []ResourceKey
	Decisions []ResourceDecision
}

type PublicationSet struct {
	DesiredRevision uint64
	Domains         map[Domain]PublicationCandidate
}

type PublishedGeneration struct {
	Artifact  GenerationArtifact
	Snapshot  Snapshot
	Closure   []ResourceKey
	Decisions []ResourceDecision
}

type Acknowledgement struct {
	Cursor    ProviderCursor
	Revisions RevisionSet
	Decisions map[Domain][]ResourceDecision
}
