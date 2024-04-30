package http_logger

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
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
	base.BasePlugin
	config Config

	fireChan   chan map[string]any
	asyncBlock bool

	logFormat map[string]string
	client    *resty.Client
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

	p.fireChan = make(chan map[string]any, 1000)
	p.asyncBlock = true

	p.client = resty.New()

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Timeout == 0 {
		p.config.Timeout = 3
	}

	p.client.SetTimeout(time.Duration(p.config.Timeout) * time.Second)
	p.client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.config.SslVerify})

	if p.config.LogFormat == nil || len(p.config.LogFormat) == 0 {
		var metadata pluginMetadata
		store.GetPluginMetadata("file-logger", &metadata)
		p.logFormat = metadata.LogFormat
	} else {
		p.logFormat = p.config.LogFormat
	}

	if p.config.ConcatMethod == "" || p.config.ConcatMethod == "json" {
		p.client.SetHeader("content-type", "application/json")
	} else {
		p.client.SetHeader("content-type", "text/plain")
	}
	p.client.SetHeader("User-Agent", "apisix-go-plugin-http-logger")

	if p.config.AuthHeader != nil {
		// we can't use  p.client.SetAuthToken here
		p.client.SetHeader("Authorization", *p.config.AuthHeader)
	}

	// start the consumer
	p.consume()

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		logFields := log.GetFields(r, p.logFormat)
		p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Fire(entry map[string]any) error {
	select {
	case p.fireChan <- entry: // try and put into chan, if fail will to default
	default:
		if p.asyncBlock {
			fmt.Println("the log buffered chan is full! will block")
			p.fireChan <- entry // Blocks the goroutine because buffer is full.
			return nil
		}
		fmt.Println("the log buffered chan is full! will drop")
		// Drop message by default.
	}
	return nil
}

// add a http log consumer here, to consume the log via a channel
func (p *Plugin) consume() {
	go func() {
		for {
			select {
			case log := <-p.fireChan:
				p.send(log)
				// consume the log
			}
		}
	}()
}

func (p *Plugin) send(log map[string]any) {
	fmt.Println("send log to http logger", log)
	// FIXME: use our own json marshal? for better performance
	resp, err := p.client.R().SetBody(log).Post(p.config.URI)
	if err != nil {
		logger.Errorf("error while sending data to [%s] %s", p.config.URI, err)
		return
	}

	if resp.StatusCode() >= 400 {
		logger.Errorf("server returned status code [%d] uri [%s], body [%s]", resp.StatusCode(), p.config.URI, resp.String())
		return
	}
}
