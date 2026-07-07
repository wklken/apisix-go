package http_logger

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
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
	  "max_req_body_bytes": {
		"type": "integer",
		"minimum": 1,
		"default": 524288
	  },
	  "max_resp_body_bytes": {
		"type": "integer",
		"minimum": 1,
		"default": 524288
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
	URI              string            `json:"uri"`
	AuthHeader       *string           `json:"auth_header,omitempty"`
	Timeout          int               `json:"timeout"`
	LogFormat        map[string]string `json:"log_format,omitempty"`
	SslVerify        bool              `json:"ssl_verify"`
	MaxReqBodyBytes  int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes int               `json:"max_resp_body_bytes,omitempty"`
	IncludeReqBody   bool              `json:"include_req_body,omitempty"`
	IncludeRespBody  bool              `json:"include_resp_body,omitempty"`

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
	if p.config.ConcatMethod == "" {
		p.config.ConcatMethod = "json"
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}

	// client
	configUID := shared.NewConfigUID()
	client := resty.New()

	configUID.Add(p.config.Timeout)
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Second)
	configUID.Add(p.config.SslVerify)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.config.SslVerify})

	configUID.Add(p.config.ConcatMethod)
	if p.config.ConcatMethod == "json" {
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
		p.LogFormat = loadMetadataLogFormat()
	} else {
		p.LogFormat = p.config.LogFormat
	}

	// start the consumer
	p.Consume()

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	if !p.config.IncludeReqBody && !p.config.IncludeRespBody {
		return p.BaseLoggerPlugin.Handler(next)
	}

	fn := func(w http.ResponseWriter, r *http.Request) {
		var requestBody string
		if p.config.IncludeReqBody {
			body, err := readAndRestoreRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *httpLogResponseRecorder
		if p.config.IncludeRespBody {
			recorder = &httpLogResponseRecorder{
				ResponseWriter: w,
				limit:          p.config.MaxRespBodyBytes,
			}
			writer = recorder
		}

		next.ServeHTTP(writer, r)

		logFields := make(map[string]any)
		if len(p.LogFormat) > 0 {
			logFields = apisixlog.GetFields(r, p.LogFormat)
		}
		if requestBody != "" {
			nestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.body.Len() > 0 {
			nestedLogMap(logFields, "response")["body"] = recorder.body.String()
		}

		p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Send(log map[string]any) {
	body, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in http-logger", err)
		return
	}

	resp, err := p.client.R().SetBody(body).Post(p.config.URI)
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

type httpLogResponseRecorder struct {
	http.ResponseWriter
	body  bytes.Buffer
	limit int
}

func (w *httpLogResponseRecorder) Write(body []byte) (int, error) {
	w.capture(body)
	return w.ResponseWriter.Write(body)
}

func (w *httpLogResponseRecorder) capture(body []byte) {
	if w.limit <= 0 || w.body.Len() >= w.limit {
		return
	}
	remaining := w.limit - w.body.Len()
	if len(body) > remaining {
		body = body[:remaining]
	}
	_, _ = w.body.Write(body)
}

func readAndRestoreRequestBody(r *http.Request, limit int) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if limit > 0 && len(body) > limit {
		body = body[:limit]
	}
	return string(body), nil
}

func nestedLogMap(fields map[string]any, key string) map[string]any {
	if value, ok := fields[key].(map[string]any); ok {
		return value
	}
	value := map[string]any{}
	fields[key] = value
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
