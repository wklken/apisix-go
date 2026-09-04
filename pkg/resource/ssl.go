package resource

import "github.com/wklken/apisix-go/pkg/json"

// SSL describes the APISIX SSL resource fields needed by upstream TLS.
// The metadata fields are retained so snapshots can be parsed without
// discarding the resource shape, while Kafka currently consumes Cert and Key.
type SSLClient struct {
	CA               string   `json:"ca,omitempty" yaml:"ca,omitempty"`
	Depth            int      `json:"depth,omitempty" yaml:"depth,omitempty"`
	SkipMTLSURIRegex []string `json:"skip_mtls_uri_regex,omitempty" yaml:"skip_mtls_uri_regex,omitempty"`
}

func (c *SSLClient) UnmarshalJSON(data []byte) error {
	type sslClientJSON SSLClient
	aux := struct {
		sslClientJSON
		Depth *int `json:"depth"`
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*c = SSLClient(aux.sslClientJSON)
	if aux.Depth == nil {
		c.Depth = 1
	} else {
		c.Depth = *aux.Depth
	}
	return nil
}

type SSL struct {
	ID           string            `json:"id,omitempty" yaml:"id,omitempty"`
	Type         string            `json:"type,omitempty" yaml:"type,omitempty"`
	Sni          string            `json:"sni,omitempty" yaml:"sni,omitempty"`
	Snis         []string          `json:"snis,omitempty" yaml:"snis,omitempty"`
	Cert         string            `json:"cert,omitempty" yaml:"cert,omitempty"`
	Key          string            `json:"key,omitempty" yaml:"key,omitempty"`
	Client       *SSLClient        `json:"client,omitempty" yaml:"client,omitempty"`
	SSLProtocols []string          `json:"ssl_protocols,omitempty" yaml:"ssl_protocols,omitempty"`
	Status       int               `json:"status,omitempty" yaml:"status,omitempty"`
	Labels       map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

func (s *SSL) UnmarshalJSON(data []byte) error {
	type sslJSON SSL
	aux := struct {
		sslJSON
		Status *int `json:"status"`
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*s = SSL(aux.sslJSON)
	if aux.Status == nil {
		s.Status = 1
	} else {
		s.Status = *aux.Status
	}
	return nil
}
