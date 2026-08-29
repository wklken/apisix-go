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

type Resource struct {
	Key   ResourceKey `json:"key"`
	Value []byte      `json:"value"`
}

type Tombstone struct {
	Key      ResourceKey `json:"key"`
	Revision uint64      `json:"revision"`
}

type Snapshot struct {
	revision   uint64
	resources  []Resource
	tombstones []Tombstone
	digest     [32]byte
}

type MutationType string

const (
	MutationPut    MutationType = "put"
	MutationDelete MutationType = "delete"
)

type Mutation struct {
	Type  MutationType
	Key   ResourceKey
	Value []byte
}

type ProviderCursor struct {
	Provider string
	Revision string
}

type DesiredBatch struct {
	Cursor          ProviderCursor
	ReplaceManaged  bool
	Mutations       []Mutation
	RequiredDomains []Domain
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

type PublicationToken string

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
	// CommittedReplay is transient delivery metadata set only when Coordinator
	// returns an acknowledgement already committed for the requested cursor.
	CommittedReplay bool
}

type RecoveryFailure struct {
	Domain Domain
	Code   string
}

type RecoveryState struct {
	Revisions RevisionSet
	Desired   Snapshot
	Published map[Domain]PublishedGeneration
	Failures  []RecoveryFailure
}
