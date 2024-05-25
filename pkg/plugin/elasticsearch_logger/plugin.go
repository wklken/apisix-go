package elasticsearch_logger

import (
	"bytes"
	"encoding/json"
	"fmt"

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
	  }
	},
	"required": ["endpoint_addrs", "field"]
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

	client *elasticsearch.Client
}

type Config struct {
	EndpointAddrs []string          `json:"endpoint_addrs"`
	Field         FieldConfig       `json:"field"`
	LogFormat     map[string]string `json:"log_format,omitempty"`
	Auth          *AuthConfig       `json:"auth,omitempty"`
	Timeout       int               `json:"timeout,omitempty"`
	SslVerify     *bool             `json:"ssl_verify,omitempty"`

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

	if p.config.LogFormat == nil || len(p.config.LogFormat) == 0 {
		var metadata pluginMetadata
		store.GetPluginMetadata(name, &metadata)
		p.LogFormat = metadata.LogFormat
	} else {
		p.LogFormat = p.config.LogFormat
	}
	fmt.Printf("log format: %v\n", p.LogFormat)

	// share the same client
	// FIXME: timeout and ssl_verify not support
	clientUID := shared.NewConfigUID()

	username := ""
	password := ""
	if p.config.Auth != nil {
		username = p.config.Auth.Username
		password = p.config.Auth.Password
	}

	c, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: p.config.EndpointAddrs,
		Username:  username,
		Password:  password,
	})
	if err != nil {
		return err
	}
	clientUID.Add(p.config.EndpointAddrs, username, password)

	client := shared.LoadOrStoreClient(name, clientUID, c).(*elasticsearch.Client)

	p.client = client

	// create the index
	_, err = client.Indices.Create(p.config.Field.Index)
	if err != nil {
		logger.Warnf("failed to create index in plugin elasticsearch-logger: %s", err)
	}

	p.Consume()

	return nil
}

func (p *Plugin) Send(log map[string]any) {
	// FIXME: support batch-processor features like: send every 5 seconds or 1000 logs
	// FIXME: use bulk api to send logs to elasticsearch
	fmt.Printf("send log: %v\n", log)

	logMessage, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in udp-logger", err)
		return
	}

	_, err = p.client.Index(p.config.Field.Index, bytes.NewReader(logMessage))
	if err != nil {
		logger.Errorf("failed to send log message: %s in elasticsearch-logger", err)
		return
	}
}
