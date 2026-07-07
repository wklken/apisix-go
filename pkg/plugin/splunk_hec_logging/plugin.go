package splunk_hec_logging

import (
	"crypto/tls"
	"os"
	"time"

	"github.com/go-resty/resty/v2"
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
	priority = 409
	name     = "splunk-hec-logging"

	defaultSource     = "apache-apisix-splunk-hec-logging"
	defaultSourceType = "_json"
)

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint": {
      "type": "object",
      "properties": {
        "uri": {
          "type": "string",
          "format": "uri"
        },
        "token": {
          "type": "string"
        },
        "channel": {
          "type": "string"
        },
        "timeout": {
          "type": "integer",
          "minimum": 1,
          "default": 10
        },
        "keepalive_timeout": {
          "type": "integer",
          "minimum": 1000,
          "default": 60000
        }
      },
      "required": ["uri", "token"]
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "log_format": {
      "type": "object"
    },
    "name": {
      "type": "string"
    },
    "batch_max_size": {
      "type": "integer",
      "minimum": 1,
      "default": 1000
    },
    "inactive_timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    },
    "buffer_duration": {
      "type": "integer",
      "minimum": 1,
      "default": 60
    },
    "retry_delay": {
      "type": "integer",
      "minimum": 0,
      "default": 1
    },
    "max_retry_count": {
      "type": "integer",
      "minimum": 0,
      "default": 0
    }
  },
  "required": ["endpoint"]
}
`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Endpoint struct {
	URI              string `json:"uri"`
	Token            string `json:"token"`
	Channel          string `json:"channel,omitempty"`
	Timeout          int    `json:"timeout,omitempty"`
	KeepaliveTimeout int    `json:"keepalive_timeout,omitempty"`
}

type Config struct {
	Endpoint  Endpoint          `json:"endpoint"`
	SSLVerify *bool             `json:"ssl_verify,omitempty"`
	LogFormat map[string]string `json:"log_format,omitempty"`

	Name            string `json:"name,omitempty"`
	BatchMaxSize    int    `json:"batch_max_size,omitempty"`
	InactiveTimeout int    `json:"inactive_timeout,omitempty"`
	BufferDuration  int    `json:"buffer_duration,omitempty"`
	RetryDelay      int    `json:"retry_delay,omitempty"`
	MaxRetryCount   int    `json:"max_retry_count,omitempty"`
}

type splunkEvent struct {
	Time       float64        `json:"time"`
	Host       string         `json:"host"`
	Source     string         `json:"source"`
	SourceType string         `json:"sourcetype"`
	Event      map[string]any `json:"event"`
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
	if p.config.Endpoint.Timeout == 0 {
		p.config.Endpoint.Timeout = 10
	}
	if p.config.Endpoint.KeepaliveTimeout == 0 {
		p.config.Endpoint.KeepaliveTimeout = 60000
	}

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.Endpoint.URI)
	configUID.Add(p.config.Endpoint.Token)
	configUID.Add(p.config.Endpoint.Channel)
	configUID.Add(p.config.Endpoint.Timeout)
	configUID.Add(p.config.Endpoint.KeepaliveTimeout)
	configUID.Add(p.sslVerify())

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Endpoint.Timeout) * time.Second)
	client.SetHeader("Content-Type", "application/json")
	client.SetHeader("Authorization", "Splunk "+p.config.Endpoint.Token)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.sslVerify()})
	if p.config.Endpoint.Channel != "" {
		client.SetHeader("X-Splunk-Request-Channel", p.config.Endpoint.Channel)
	}
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
	resp, err := p.client.R().SetBody(p.buildEvent(log)).Post(p.config.Endpoint.URI)
	if err != nil {
		logger.Errorf("failed to send log to Splunk HEC endpoint %s: %s", p.config.Endpoint.URI, err)
		return
	}

	if resp.StatusCode() >= 400 {
		logger.Errorf(
			"Splunk HEC endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			p.config.Endpoint.URI,
			resp.String(),
		)
	}
}

func (p *Plugin) buildEvent(log map[string]any) splunkEvent {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "-"
	}

	return splunkEvent{
		Time:       float64(time.Now().UnixNano()) / float64(time.Second),
		Host:       hostname,
		Source:     defaultSource,
		SourceType: defaultSourceType,
		Event:      log,
	}
}

func (p *Plugin) sslVerify() bool {
	return p.config.SSLVerify == nil || *p.config.SSLVerify
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
