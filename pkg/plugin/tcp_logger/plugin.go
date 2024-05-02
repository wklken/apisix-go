package tcp_logger

import (
	"encoding/json"
	"fmt"
	"net"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

const (
	// version  = "0.1"
	priority = 405
	name     = "tcp-logger"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "host": {
		"type": "string"
	  },
	  "port": {
		"type": "integer",
		"minimum": 0
	  },
	  "tls": {
		"type": "boolean",
		"default": false
	  },
	  "tls_options": {
		"type": "string"
	  },
	  "timeout": {
		"type": "integer",
		"minimum": 1,
		"default": 1000
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
	  }
	},
	"required": ["host", "port"]
}`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Plugin struct {
	base.BaseLoggerPlugin
	config Config
}

type Config struct {
	Host      string            `json:"host"`
	Port      int               `json:"port"`
	Timeout   int               `json:"timeout,omitempty"`
	LogFormat map[string]string `json:"log_format,omitempty"`

	// FIXME: not support
	// TLS        bool    `json:"tls"`
	// TLSOptions *string `json:"tls_options,omitempty"`
	// IncludeReqBody      bool                `json:"include_req_body"`
	// IncludeReqBodyExpr  [][]interface{}     `json:"include_req_body_expr,omitempty"`
	// IncludeRespBody     bool                `json:"include_resp_body"`
	// IncludeRespBodyExpr [][]interface{}     `json:"include_resp_body_expr,omitempty"`

	addr string
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
		p.config.Timeout = 1
	}

	if p.config.LogFormat == nil || len(p.config.LogFormat) == 0 {
		var metadata pluginMetadata
		store.GetPluginMetadata(name, &metadata)
		p.LogFormat = metadata.LogFormat
	} else {
		p.LogFormat = p.config.LogFormat
	}
	fmt.Printf("log format: %v\n", p.LogFormat)

	p.config.addr = fmt.Sprintf("%s:%d", p.config.Host, p.config.Port)

	// start the consumer
	p.Consume()

	return nil
}

func (p *Plugin) Send(log map[string]any) {
	// FIXME: support batch-processor features like: send every 5 seconds or 1000 logs
	conn, err := net.Dial("tcp", p.config.addr)
	if err != nil {
		logger.Errorf("failed to connect to udp server: %s", err)
		return
	}

	defer conn.Close()

	logMessage, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in udp-logger", err)
		return
	}

	_, err = fmt.Fprintf(conn, util.BytesToString(logMessage))
	if err != nil {
		logger.Errorf("failed to send log message: %s in udp-logger", err)
		return
	}
}
