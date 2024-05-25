package http_logger

import (
	"crypto/tls"
	"fmt"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
)

const (
	// version  = "0.1"
	priority = 410
	name     = "http-logger"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "uri": {
		"type": "string",
		"format": "uri"
	  },
	  "auth_header": {
		"type": "string"
	  },
	  "timeout": {
		"type": "integer",
		"minimum": 1,
		"default": 3
	  },
	  "log_format": {
		"type": "object"
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
	  "concat_method": {
		"type": "string",
		"default": "json",
		"enum": ["json", "new_line"]
	  },
	  "ssl_verify": {
		"type": "boolean",
		"default": false
	  }
	},
	"required": ["uri"]
}`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
}

type Config struct {
	URI        string            `json:"uri"`
	AuthHeader *string           `json:"auth_header,omitempty"`
	Timeout    int               `json:"timeout"`
	LogFormat  map[string]string `json:"log_format,omitempty"`
	SslVerify  bool              `json:"ssl_verify"`

	// FIXME: not support
	// IncludeReqBody      bool                   `json:"include_req_body"`
	// IncludeReqBodyExpr  [][]interface{}        `json:"include_req_body_expr,omitempty"`
	// IncludeRespBody     bool                   `json:"include_resp_body"`
	// IncludeRespBodyExpr [][]interface{}        `json:"include_resp_body_expr,omitempty"`

	// NOTE: not needed
	ConcatMethod string `json:"concat_method"`

	contentType string
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
		p.config.Timeout = 3
	}

	// client
	configUID := shared.NewConfigUID()
	client := resty.New()

	configUID.Add(p.config.Timeout)
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Second)
	configUID.Add(p.config.SslVerify)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.config.SslVerify})

	configUID.Add(p.config.ConcatMethod)
	if p.config.ConcatMethod == "" || p.config.ConcatMethod == "json" {
		client.SetHeader("content-type", "application/json")
	} else {
		client.SetHeader("content-type", "text/plain")
	}
	client.SetHeader("User-Agent", "apisix-go-plugin-http-logger")

	configUID.Add(p.config.AuthHeader)
	if p.config.AuthHeader != nil {
		// we can't use  p.client.SetAuthToken here
		client.SetHeader("Authorization", *p.config.AuthHeader)
	}

	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	if p.config.LogFormat == nil || len(p.config.LogFormat) == 0 {
		var metadata pluginMetadata
		store.GetPluginMetadata("file-logger", &metadata)
		p.LogFormat = metadata.LogFormat
	} else {
		p.LogFormat = p.config.LogFormat
	}

	// start the consumer
	p.Consume()

	return nil
}

func (p *Plugin) Send(log map[string]any) {
	fmt.Println("send log to http logger", log)
	// FIXME: use our own json marshal? for better performance
	resp, err := p.client.R().SetBody(log).Post(p.config.URI)
	if err != nil {
		logger.Errorf("error while sending data to [%s] %s", p.config.URI, err)
		return
	}

	if resp.StatusCode() >= 400 {
		logger.Errorf(
			"server returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			p.config.URI,
			resp.String(),
		)
		return
	}
}
