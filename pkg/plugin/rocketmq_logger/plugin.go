package rocketmq_logger

import (
	"context"
	"net/http"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config
	sender rocketmqSender
}

const (
	priority = 402
	name     = "rocketmq-logger"
)

const schema = `
{
  "type": "object",
  "properties": {
    "meta_format": {
      "type": "string",
      "default": "default",
      "enum": ["default", "origin"]
    },
    "nameserver_list": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string"
      }
    },
    "topic": {
      "type": "string"
    },
    "key": {
      "type": "string"
    },
    "tag": {
      "type": "string"
    },
    "log_format": {
      "type": "object"
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 3
    },
    "use_tls": {
      "type": "boolean",
      "default": false
    },
    "access_key": {
      "type": "string",
      "default": ""
    },
    "secret_key": {
      "type": "string",
      "default": ""
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
  "required": ["nameserver_list", "topic"]
}
`

type Config struct {
	MetaFormat     string            `json:"meta_format,omitempty"`
	NameServerList []string          `json:"nameserver_list"`
	Topic          string            `json:"topic"`
	Key            string            `json:"key,omitempty"`
	Tag            string            `json:"tag,omitempty"`
	LogFormat      map[string]string `json:"log_format,omitempty"`
	Timeout        int               `json:"timeout,omitempty"`
	UseTLS         bool              `json:"use_tls,omitempty"`
	AccessKey      string            `json:"access_key,omitempty"`
	SecretKey      string            `json:"secret_key,omitempty"`

	IncludeReqBody      bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool    `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int     `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int     `json:"max_resp_body_bytes,omitempty"`
}

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type rocketmqMessage struct {
	Topic string
	Key   string
	Tag   string
	Body  []byte
}

type rocketmqSender interface {
	Send(ctx context.Context, message rocketmqMessage) error
}

type rocketmqClientSender struct {
	producer rocketmq.Producer
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
	p.applyDefaults()

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = loadMetadataLogFormat()
	}

	if p.sender == nil {
		sender, err := p.newSender()
		if err != nil {
			return err
		}
		p.sender = sender
	}

	p.Consume()
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return p.BaseLoggerPlugin.Handler(next)
}

func (p *Plugin) Send(log map[string]any) {
	message, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal rocketmq log message: %s", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.config.Timeout)*time.Second)
	defer cancel()

	err = p.sender.Send(ctx, rocketmqMessage{
		Topic: p.config.Topic,
		Key:   p.config.Key,
		Tag:   p.config.Tag,
		Body:  message,
	})
	if err != nil {
		logger.Errorf("failed to send data to RocketMQ topic %s: %s", p.config.Topic, err)
	}
}

func (p *Plugin) applyDefaults() {
	if p.config.MetaFormat == "" {
		p.config.MetaFormat = "default"
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}
}

func (p *Plugin) newSender() (rocketmqSender, error) {
	options := []producer.Option{
		producer.WithNameServer(p.config.NameServerList),
		producer.WithSendMsgTimeout(time.Duration(p.config.Timeout) * time.Second),
	}
	if p.config.AccessKey != "" {
		options = append(options, producer.WithCredentials(primitive.Credentials{
			AccessKey: p.config.AccessKey,
			SecretKey: p.config.SecretKey,
		}))
	}

	prod, err := rocketmq.NewProducer(options...)
	if err != nil {
		return nil, err
	}
	if err := prod.Start(); err != nil {
		return nil, err
	}

	return &rocketmqClientSender{producer: prod}, nil
}

func (s *rocketmqClientSender) Send(ctx context.Context, message rocketmqMessage) error {
	msg := primitive.NewMessage(message.Topic, message.Body)
	if message.Tag != "" {
		msg.WithTag(message.Tag)
	}
	if message.Key != "" {
		msg.WithKeys([]string{message.Key})
	}

	_, err := s.producer.SendSync(ctx, msg)
	return err
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
