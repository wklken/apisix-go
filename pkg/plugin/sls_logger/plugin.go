package sls_logger

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	addr string
}

const (
	priority = 406
	name     = "sls-logger"
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
    "project": {
      "type": "string"
    },
    "logstore": {
      "type": "string"
    },
    "access_key_id": {
      "type": "string"
    },
    "access_key_secret": {
      "type": "string"
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5000
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
  "required": ["host", "port", "project", "logstore", "access_key_id", "access_key_secret"]
}
`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Config struct {
	Host            string            `json:"host"`
	Port            int               `json:"port"`
	Project         string            `json:"project"`
	Logstore        string            `json:"logstore"`
	AccessKeyID     string            `json:"access_key_id"`
	AccessKeySecret string            `json:"access_key_secret"`
	Timeout         int               `json:"timeout,omitempty"`
	LogFormat       map[string]string `json:"log_format,omitempty"`

	IncludeReqBody   bool `json:"include_req_body,omitempty"`
	IncludeRespBody  bool `json:"include_resp_body,omitempty"`
	MaxReqBodyBytes  int  `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes int  `json:"max_resp_body_bytes,omitempty"`
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
		p.config.Timeout = 5000
	}
	p.addr = net.JoinHostPort(p.config.Host, fmt.Sprint(p.config.Port))

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = loadMetadataLogFormat()
	}

	p.Consume()
	return nil
}

func (p *Plugin) Send(log map[string]any) {
	dialer := &net.Dialer{Timeout: time.Duration(p.config.Timeout) * time.Millisecond}
	conn, err := tls.DialWithDialer(dialer, "tcp", p.addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		logger.Errorf("failed to connect to SLS TLS endpoint %s: %s", p.addr, err)
		return
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(p.buildMessage(log))); err != nil {
		logger.Errorf("failed to send SLS log message: %s", err)
	}
}

func (p *Plugin) buildMessage(log map[string]any) string {
	payload, err := json.Marshal(log)
	if err != nil {
		payload = []byte(`{}`)
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "-"
	}

	return strings.Join([]string{
		"<46>1",
		time.Now().UTC().Format(time.RFC3339Nano),
		hostname,
		"apisix",
		fmt.Sprint(os.Getpid()),
		"-",
		p.structuredData(),
		string(payload),
	}, " ") + "\n"
}

func (p *Plugin) structuredData() string {
	return fmt.Sprintf(
		`[logservice project="%s" logstore="%s" access-key-id="%s" access-key-secret="%s"]`,
		escapeStructuredDataValue(p.config.Project),
		escapeStructuredDataValue(p.config.Logstore),
		escapeStructuredDataValue(p.config.AccessKeyID),
		escapeStructuredDataValue(p.config.AccessKeySecret),
	)
}

func escapeStructuredDataValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, `]`, `\]`)
	return value
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
