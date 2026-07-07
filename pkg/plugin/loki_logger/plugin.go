package loki_logger

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
}

const (
	priority = 414
	name     = "loki-logger"
)

var randomEndpointIndex = rand.Intn

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint_addrs": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "format": "uri"
      }
    },
    "endpoint_uri": {
      "type": "string",
      "minLength": 1,
      "default": "/loki/api/v1/push"
    },
    "tenant_id": {
      "type": "string",
      "default": "fake"
    },
    "headers": {
      "type": "object"
    },
    "log_labels": {
      "type": "object",
      "default": {
        "job": "apisix"
      }
    },
    "ssl_verify": {
      "type": "boolean",
      "default": false
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "maximum": 60000,
      "default": 3000
    },
    "keepalive": {
      "type": "boolean",
      "default": true
    },
    "keepalive_timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 60000
    },
    "keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    },
    "log_format": {
      "type": "object"
    },
    "include_req_body": {
      "type": "boolean",
      "default": false
    },
    "include_resp_body": {
      "type": "boolean",
      "default": false
    },
    "max_req_body_bytes": {
      "type": "integer",
      "minimum": 1,
      "default": 524288
    },
    "max_resp_body_bytes": {
      "type": "integer",
      "minimum": 1,
      "default": 524288
    }
  },
  "required": ["endpoint_addrs"]
}
`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Config struct {
	EndpointAddrs    []string          `json:"endpoint_addrs"`
	EndpointURI      string            `json:"endpoint_uri,omitempty"`
	TenantID         string            `json:"tenant_id,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	LogLabels        map[string]string `json:"log_labels,omitempty"`
	SSLVerify        bool              `json:"ssl_verify"`
	Timeout          int               `json:"timeout,omitempty"`
	Keepalive        *bool             `json:"keepalive,omitempty"`
	KeepaliveTimeout int               `json:"keepalive_timeout,omitempty"`
	KeepalivePool    int               `json:"keepalive_pool,omitempty"`
	LogFormat        map[string]string `json:"log_format,omitempty"`

	IncludeReqBody    bool `json:"include_req_body,omitempty"`
	IncludeRespBody   bool `json:"include_resp_body,omitempty"`
	MaxReqBodyBytes   int  `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes  int  `json:"max_resp_body_bytes,omitempty"`
	MaxPendingEntries int  `json:"max_pending_entries,omitempty"`
}

type lokiPayload struct {
	Streams []lokiStream `json:"streams"`
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	p.FireChan = make(chan map[string]any, 1000)
	p.AsyncBlock = true
	p.SendFunc = p.Send

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.EndpointURI == "" {
		p.config.EndpointURI = "/loki/api/v1/push"
	}
	if p.config.TenantID == "" {
		p.config.TenantID = "fake"
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3000
	}
	if p.config.KeepaliveTimeout == 0 {
		p.config.KeepaliveTimeout = 60000
	}
	if p.config.KeepalivePool == 0 {
		p.config.KeepalivePool = 5
	}
	if len(p.config.LogLabels) == 0 {
		p.config.LogLabels = map[string]string{"job": "apisix"}
	}

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.EndpointAddrs)
	configUID.Add(p.config.EndpointURI)
	configUID.Add(p.config.TenantID)
	configUID.Add(p.config.Headers)
	configUID.Add(p.config.Timeout)
	configUID.Add(p.config.SSLVerify)
	configUID.Add(p.keepalive())

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Millisecond)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.config.SSLVerify})
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = loadMetadataLogFormat()
	}

	p.Consume()
	return nil
}

func (p *Plugin) Send(log map[string]any) {
	if len(p.config.EndpointAddrs) == 0 {
		logger.Errorf("loki-logger endpoint_addrs is empty")
		return
	}

	endpoint := p.endpointURL()
	resp, err := p.client.R().
		SetHeaders(p.headers()).
		SetBody(p.buildPayload(log)).
		Post(endpoint)
	if err != nil {
		logger.Errorf("failed to send log to Loki endpoint %s: %s", endpoint, err)
		return
	}

	if resp.StatusCode() >= 300 {
		logger.Errorf(
			"Loki endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			endpoint,
			resp.String(),
		)
	}
}

func (p *Plugin) buildPayload(log map[string]any) lokiPayload {
	entry, err := json.Marshal(log)
	if err != nil {
		entry = []byte(`{}`)
	}

	return lokiPayload{
		Streams: []lokiStream{
			{
				Stream: p.resolveLabels(log),
				Values: [][2]string{
					{fmt.Sprintf("%d", time.Now().UnixNano()), string(entry)},
				},
			},
		},
	}
}

func (p *Plugin) resolveLabels(log map[string]any) map[string]string {
	labels := make(map[string]string, len(p.config.LogLabels))
	for key, value := range p.config.LogLabels {
		if strings.HasPrefix(value, "$") {
			if resolved, ok := log[strings.TrimPrefix(value, "$")]; ok {
				labels[key] = fmt.Sprint(resolved)
				continue
			}
		}
		labels[key] = value
	}
	return labels
}

func (p *Plugin) headers() map[string]string {
	headers := make(map[string]string, len(p.config.Headers)+2)
	for key, value := range p.config.Headers {
		if strings.EqualFold(key, "X-Scope-OrgID") || strings.EqualFold(key, "Content-Type") {
			continue
		}
		headers[key] = value
	}
	headers["X-Scope-OrgID"] = p.config.TenantID
	headers["Content-Type"] = "application/json"
	return headers
}

func (p *Plugin) endpointURL() string {
	baseURL := p.config.EndpointAddrs[randomEndpointIndex(len(p.config.EndpointAddrs))]
	if strings.HasSuffix(baseURL, "/") && strings.HasPrefix(p.config.EndpointURI, "/") {
		return baseURL[:len(baseURL)-1] + p.config.EndpointURI
	}
	return baseURL + p.config.EndpointURI
}

func (p *Plugin) keepalive() bool {
	return p.config.Keepalive == nil || *p.config.Keepalive
}

func loadMetadataLogFormat() (format map[string]string) {
	defer func() {
		if recover() != nil {
			format = nil
		}
	}()

	var metadata pluginMetadata
	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		return nil
	}
	return metadata.LogFormat
}
