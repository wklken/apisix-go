package resource

// SSL describes the APISIX SSL resource fields needed by upstream TLS.
// The metadata fields are retained so snapshots can be parsed without
// discarding the resource shape, while Kafka currently consumes Cert and Key.
type SSL struct {
	ID     string            `json:"id,omitempty" yaml:"id,omitempty"`
	Snis   []string          `json:"snis,omitempty" yaml:"snis,omitempty"`
	Cert   string            `json:"cert,omitempty" yaml:"cert,omitempty"`
	Key    string            `json:"key,omitempty" yaml:"key,omitempty"`
	Status int               `json:"status,omitempty" yaml:"status,omitempty"`
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
}
