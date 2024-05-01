package udp_logger

import (
	"fmt"
	"net"
	"net/http"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

const (
	// version  = "0.1"
	priority = 400
	name     = "udp-logger"
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
	  }
	},
	"required": ["host", "port"]
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
	Host      string            `json:"host"`
	Port      int               `json:"port"`
	Timeout   int               `json:"timeout,omitempty"`    // 使用指针以区分默认值和未设置
	LogFormat map[string]string `json:"log_format,omitempty"` // 使用指针类型以便跳过默认空值

	// FIXME: not support
	// IncludeReqBody      bool              `json:"include_req_body"`
	// IncludeReqBodyExpr  [][]interface{}   `json:"include_req_body_expr,omitempty"`
	// IncludeRespBody     bool              `json:"include_resp_body"`
	// IncludeRespBodyExpr [][]interface{}   `json:"include_resp_body_expr,omitempty"`

	addr string
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

	// p.client.SetTimeout(time.Duration(p.config.Timeout) * time.Second)
	// p.client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.config.SslVerify})

	if p.config.LogFormat == nil || len(p.config.LogFormat) == 0 {
		var metadata pluginMetadata
		store.GetPluginMetadata("file-logger", &metadata)
		p.logFormat = metadata.LogFormat
	} else {
		p.logFormat = p.config.LogFormat
	}

	p.config.addr = fmt.Sprintf("%s:%d", p.config.Host, p.config.Port)

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
	// FIXME: support batch-processor features like: send every 5 seconds or 1000 logs
	conn, err := net.Dial("udp", p.config.addr)
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
