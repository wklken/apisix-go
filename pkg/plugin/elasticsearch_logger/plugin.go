package elasticsearch_logger

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"math/rand"
	"net"
	"net/http"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
)

const (
	// version  = "0.1"
	priority = 413
	name     = "elasticsearch-logger"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "endpoint_addr": {
		"type": "string",
		"pattern": "[^/]$"
	  },
	  "endpoint_addrs": {
		"type": "array",
		"minItems": 1,
		"items": {
		  "type": "string",
		  "pattern": "[^/]$"
		}
	  },
	  "field": {
		"type": "object",
		"properties": {
		  "index": {
			"type": "string"
		  },
		  "type": {
			"type": "string"
		  }
		},
		"required": ["index"]
	  },
	  "log_format": {
		"type": "object"
	  },
	  "auth": {
		"type": "object",
		"properties": {
		  "username": {
			"type": "string",
			"minLength": 1
		  },
		  "password": {
			"type": "string",
			"minLength": 1
		  }
		},
		"required": ["username", "password"]
	  },
	  "headers": {
		"type": "object",
		"minProperties": 1,
		"patternProperties": {
		  "^[^:]+$": {
			"type": "string",
			"minLength": 1
		  }
		},
		"additionalProperties": false
	  },
	  "timeout": {
		"type": "integer",
		"minimum": 1,
		"default": 10
	  },
	  "ssl_verify": {
		"type": "boolean",
		"default": true
	  },
	  "include_req_body": {
		"type": "boolean",
		"default": false
	  },
	  "include_req_body_expr": {
		"type": "array",
		"minItems": 1,
		"items": {
		  "type": "array"
		}
	  },
	  "include_resp_body": {
		"type": "boolean",
		"default": false
	  },
	  "include_resp_body_expr": {
		"type": "array",
		"minItems": 1,
		"items": {
		  "type": "array"
		}
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
	"oneOf": [
	  {"required": ["endpoint_addr", "field"]},
	  {"required": ["endpoint_addrs", "field"]}
	]
}`

// NOTE: not support
// "encrypt_fields": ["auth.password"],
// endpoint_addr is deprecated, use endpoint_addrs instead

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Plugin struct {
	base.BaseLoggerPlugin
	config Config
}

var randomEndpointIndex = rand.Intn

type Config struct {
	EndpointAddr     string            `json:"endpoint_addr,omitempty"`
	EndpointAddrs    []string          `json:"endpoint_addrs"`
	Field            FieldConfig       `json:"field"`
	LogFormat        map[string]string `json:"log_format,omitempty"`
	Auth             *AuthConfig       `json:"auth,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	Timeout          int               `json:"timeout,omitempty"`
	SslVerify        *bool             `json:"ssl_verify,omitempty"`
	MaxReqBodyBytes  int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes int               `json:"max_resp_body_bytes,omitempty"`

	// FIXME: not support
	// IncludeReqBody        bool                  `json:"include_req_body"`
	// IncludeReqBodyExpr    [][]any               `json:"include_req_body_expr,omitempty"`
	// IncludeRespBody       bool                  `json:"include_resp_body"`
	// IncludeRespBodyExpr   [][]any               `json:"include_resp_body_expr,omitempty"`
}

type FieldConfig struct {
	Index string  `json:"index"`
	Type  *string `json:"type,omitempty"`
}

type AuthConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
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
	if p.config.Timeout == 0 {
		p.config.Timeout = 10
	}
	if p.config.SslVerify == nil {
		sslVerify := true
		p.config.SslVerify = &sslVerify
	}
	if len(p.config.EndpointAddrs) == 0 && p.config.EndpointAddr != "" {
		p.config.EndpointAddrs = []string{p.config.EndpointAddr}
	}

	if p.config.LogFormat == nil || len(p.config.LogFormat) == 0 {
		p.LogFormat = loadMetadataLogFormat()
	} else {
		p.LogFormat = p.config.LogFormat
	}

	p.Consume()

	return nil
}

func (p *Plugin) Send(log map[string]any) {
	endpoint := p.endpointAddr()
	if endpoint == "" {
		return
	}
	client, err := p.clientForEndpoint(endpoint)
	if err != nil {
		logger.Errorf("failed to create Elasticsearch client: %s in elasticsearch-logger", err)
		return
	}

	// FIXME: support batch-processor features like: send every 5 seconds or 1000 logs
	body, err := p.bulkBody(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in elasticsearch-logger", err)
		return
	}

	resp, err := client.Bulk(
		bytes.NewReader(body),
		client.Bulk.WithTimeout(time.Duration(p.config.Timeout)*time.Second),
	)
	if err != nil {
		logger.Errorf("failed to send log message: %s in elasticsearch-logger", err)
		return
	}
	defer resp.Body.Close()
	if resp.IsError() {
		logger.Errorf("failed to send log message: elasticsearch returned status %s", resp.Status())
		return
	}
}

func (p *Plugin) endpointAddr() string {
	if p.config.EndpointAddr != "" {
		return p.config.EndpointAddr
	}
	if len(p.config.EndpointAddrs) == 0 {
		return ""
	}
	return p.config.EndpointAddrs[randomEndpointIndex(len(p.config.EndpointAddrs))]
}

func (p *Plugin) clientForEndpoint(endpoint string) (*elasticsearch.Client, error) {
	username := ""
	password := ""
	if p.config.Auth != nil {
		username = p.config.Auth.Username
		password = p.config.Auth.Password
	}

	c, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{endpoint},
		Username:  username,
		Password:  password,
		Header:    headerFromMap(p.config.Headers),
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: time.Duration(p.config.Timeout) * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: time.Duration(p.config.Timeout) * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: !*p.config.SslVerify},
		},
	})
	if err != nil {
		return nil, err
	}

	clientUID := shared.NewConfigUID()
	clientUID.Add(endpoint, username, password, p.config.Headers, p.config.Timeout, *p.config.SslVerify)
	return shared.LoadOrStoreClient(name, clientUID, c).(*elasticsearch.Client), nil
}

func (p *Plugin) bulkBody(log map[string]any) ([]byte, error) {
	index := p.config.Field.Index
	action := map[string]any{
		"index": map[string]any{
			"_index": index,
		},
	}
	if p.config.Field.Type != nil && *p.config.Field.Type != "" {
		action["index"].(map[string]any)["_type"] = *p.config.Field.Type
	}

	actionLine, err := json.Marshal(action)
	if err != nil {
		return nil, err
	}
	logLine, err := json.Marshal(log)
	if err != nil {
		return nil, err
	}

	body := make([]byte, 0, len(actionLine)+len(logLine)+2)
	body = append(body, actionLine...)
	body = append(body, '\n')
	body = append(body, logLine...)
	body = append(body, '\n')
	return body, nil
}

func headerFromMap(headers map[string]string) http.Header {
	if len(headers) == 0 {
		return nil
	}
	out := make(http.Header, len(headers))
	for key, value := range headers {
		out.Set(key, value)
	}
	return out
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
