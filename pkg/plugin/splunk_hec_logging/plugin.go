package splunk_hec_logging

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
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
    },
    "max_pending_entries": {
      "type": "integer",
      "minimum": 1
    }
  },
  "required": ["endpoint"]
}
`

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
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

	Name              string `json:"name,omitempty"`
	BatchMaxSize      int    `json:"batch_max_size,omitempty"`
	InactiveTimeout   int    `json:"inactive_timeout,omitempty"`
	BufferDuration    int    `json:"buffer_duration,omitempty"`
	RetryDelay        int    `json:"retry_delay,omitempty"`
	MaxRetryCount     int    `json:"max_retry_count,omitempty"`
	MaxPendingEntries int    `json:"max_pending_entries,omitempty"`
}

type splunkEvent struct {
	Time       float64        `json:"time"`
	Host       string         `json:"host"`
	Source     string         `json:"source"`
	SourceType string         `json:"sourcetype"`
	Event      map[string]any `json:"event"`
}

func (p *Plugin) Config() any {
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
	keyring, enabled := data_encryption.Keyring()
	resolved, err := data_encryption.NewResolver(enabled, keyring).Resolve(p.config.Endpoint.Token)
	if err != nil {
		return fmt.Errorf("splunk-hec-logging endpoint.token: %w", err)
	}
	p.config.Endpoint.Token = resolved

	if p.config.Endpoint.Timeout == 0 {
		p.config.Endpoint.Timeout = 10
	}
	if p.config.Endpoint.KeepaliveTimeout == 0 {
		p.config.Endpoint.KeepaliveTimeout = 60000
	}
	if p.config.BatchMaxSize == 0 {
		p.config.BatchMaxSize = logger_batch.DefaultBatchMaxSize
	}
	if p.config.RetryDelay == 0 {
		p.config.RetryDelay = int(logger_batch.DefaultRetryDelay / time.Second)
	}
	if p.config.BufferDuration == 0 {
		p.config.BufferDuration = int(logger_batch.DefaultBufferDuration / time.Second)
	}
	if p.config.InactiveTimeout == 0 {
		p.config.InactiveTimeout = int(logger_batch.DefaultInactiveTimeout / time.Second)
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

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:              name,
		BatchMaxSize:      p.config.BatchMaxSize,
		MaxRetryCount:     p.config.MaxRetryCount,
		RetryDelay:        time.Duration(p.config.RetryDelay) * time.Second,
		BufferDuration:    time.Duration(p.config.BufferDuration) * time.Second,
		InactiveTimeout:   time.Duration(p.config.InactiveTimeout) * time.Second,
		MaxPendingEntries: p.config.MaxPendingEntries,
		RouteID:           p.RouteID,
		ServerAddr:        p.ServerAddr,
	}, p.SendBatch)
	return nil
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch([]map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(entries []map[string]any, _ int) (int, error) {
	body, err := p.encodeBatch(entries)
	if err != nil {
		return 0, err
	}

	resp, err := p.client.R().SetBody(body).Post(p.config.Endpoint.URI)
	if err != nil {
		return 0, fmt.Errorf("failed to send log to Splunk HEC endpoint %s: %w", p.config.Endpoint.URI, err)
	}

	if resp.StatusCode() != 200 {
		message := resp.String()
		var errorBody struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(resp.Body(), &errorBody); err == nil && errorBody.Text != "" {
			message = errorBody.Text
		}
		return 0, fmt.Errorf("splunk HEC endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(), p.config.Endpoint.URI, message)
	}
	return 0, nil
}

func (p *Plugin) encodeBatch(entries []map[string]any) ([]byte, error) {
	var body bytes.Buffer
	for _, entry := range entries {
		event, err := json.Marshal(p.buildEvent(entry))
		if err != nil {
			return nil, fmt.Errorf("failed to marshal Splunk HEC event: %w", err)
		}
		body.Write(event)
	}
	return body.Bytes(), nil
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
