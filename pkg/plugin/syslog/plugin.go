package syslog

import (
	"fmt"
	"log/syslog"
	"net"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

const (
	// version  = "0.1"
	priority = 401
	name     = "syslog"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "host": {
		"type": "string"
	  },
	  "port": {
		"type": "integer"
	  },
	  "flush_limit": {
		"type": "integer",
		"minimum": 1,
		"default": 4096
	  },
	  "drop_limit": {
		"type": "integer",
		"default": 1048576
	  },
	  "timeout": {
		"type": "integer",
		"minimum": 1,
		"default": 3000
	  },
	  "sock_type": {
		"type": "string",
		"default": "tcp",
		"enum": ["tcp", "udp"]
	  },
	  "pool_size": {
		"type": "integer",
		"minimum": 5,
		"default": 5
	  },
	  "tls": {
		"type": "boolean",
		"default": false
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
	Host             string            `json:"host"`
	Port             int               `json:"port"`
	FlushLimit       int               `json:"flush_limit,omitempty"`
	DropLimit        int               `json:"drop_limit,omitempty"`
	Timeout          int               `json:"timeout,omitempty"`
	LogFormat        map[string]string `json:"log_format,omitempty"`
	SockType         string            `json:"sock_type,omitempty"`
	PoolSize         int               `json:"pool_size,omitempty"`
	TLS              bool              `json:"tls,omitempty"`
	MaxReqBodyBytes  int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes int               `json:"max_resp_body_bytes,omitempty"`

	// FIXME: not support
	// IncludeReqBody      bool            `json:"include_req_body"`
	// IncludeReqBodyExpr  [][]interface{} `json:"include_req_body_expr,omitempty"`
	// IncludeRespBody     bool            `json:"include_resp_body"`
	// IncludeRespBodyExpr [][]interface{} `json:"include_resp_body_expr,omitempty"`
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
		p.config.Timeout = 3000
	}
	if p.config.FlushLimit == 0 {
		p.config.FlushLimit = 4096
	}
	if p.config.DropLimit == 0 {
		p.config.DropLimit = 1048576
	}
	if p.config.PoolSize == 0 {
		p.config.PoolSize = 5
	}

	if p.config.LogFormat == nil || len(p.config.LogFormat) == 0 {
		p.LogFormat = loadMetadataLogFormat()
	} else {
		p.LogFormat = p.config.LogFormat
	}

	if p.config.SockType == "" {
		p.config.SockType = "tcp"
	}

	p.config.addr = net.JoinHostPort(p.config.Host, fmt.Sprint(p.config.Port))

	// start the consumer
	p.Consume()

	return nil
}

func (p *Plugin) Send(log map[string]any) {
	sysLog, err := syslog.Dial(p.config.SockType, p.config.addr,
		syslog.LOG_INFO|syslog.LOG_DAEMON, "apisix")
	if err != nil {
		logger.Errorf("failed to connect to syslog server: %s", err)
		return
	}

	defer sysLog.Close()

	logMessage, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in syslog", err)
		return
	}
	_, err = sysLog.Write(logMessage)
	if err != nil {
		logger.Errorf("failed to send log message: %s in udp-logger", err)
		return
	}
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
