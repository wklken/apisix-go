package clickhouse_logger

import (
	"bytes"
	"crypto/tls"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
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
	priority = 398
	name     = "clickhouse-logger"
)

var randomEndpointIndex = rand.Intn

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint_addr": {
      "type": "string",
      "format": "uri"
    },
    "endpoint_addrs": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "format": "uri"
      }
    },
    "user": {
      "type": "string",
      "default": ""
    },
    "password": {
      "type": "string",
      "default": ""
    },
    "database": {
      "type": "string",
      "default": ""
    },
    "logtable": {
      "type": "string",
      "default": ""
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 3
    },
    "name": {
      "type": "string",
      "default": "clickhouse logger"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
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
  "oneOf": [
    {"required": ["endpoint_addr", "user", "password", "database", "logtable"]},
    {"required": ["endpoint_addrs", "user", "password", "database", "logtable"]}
  ]
}
`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Config struct {
	EndpointAddr  string            `json:"endpoint_addr,omitempty"`
	EndpointAddrs []string          `json:"endpoint_addrs,omitempty"`
	User          string            `json:"user"`
	Password      string            `json:"password"`
	Database      string            `json:"database"`
	LogTable      string            `json:"logtable"`
	Timeout       int               `json:"timeout,omitempty"`
	Name          string            `json:"name,omitempty"`
	SSLVerify     *bool             `json:"ssl_verify,omitempty"`
	LogFormat     map[string]string `json:"log_format,omitempty"`

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
		p.config.Timeout = 3
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}

	configUID := shared.NewConfigUID()
	configUID.Add(p.endpointUID())
	configUID.Add(p.config.User)
	configUID.Add(p.config.Password)
	configUID.Add(p.config.Database)
	configUID.Add(p.config.Timeout)
	configUID.Add(p.sslVerify())

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Second)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.sslVerify()})
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = loadMetadataLogFormat()
	}

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
		var recorder *clickHouseLogResponseRecorder
		if p.config.IncludeRespBody {
			recorder = &clickHouseLogResponseRecorder{
				ResponseWriter: w,
				limit:          p.config.MaxRespBodyBytes,
			}
			writer = recorder
		}

		next.ServeHTTP(writer, r)

		logFields := apisixlog.GetFields(r, p.LogFormat)
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

type clickHouseLogResponseRecorder struct {
	http.ResponseWriter
	body  bytes.Buffer
	limit int
}

func (w *clickHouseLogResponseRecorder) Write(body []byte) (int, error) {
	w.capture(body)
	return w.ResponseWriter.Write(body)
}

func (w *clickHouseLogResponseRecorder) capture(body []byte) {
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

func (p *Plugin) Send(log map[string]any) {
	endpoint := p.endpointURL()
	if endpoint == "" {
		logger.Errorf("clickhouse-logger endpoint is empty")
		return
	}

	resp, err := p.client.R().
		SetHeaders(map[string]string{
			"Content-Type":          "application/json",
			"X-ClickHouse-User":     p.config.User,
			"X-ClickHouse-Key":      p.config.Password,
			"X-ClickHouse-Database": p.config.Database,
		}).
		SetBody(p.buildInsertBody(log)).
		Post(endpoint)
	if err != nil {
		logger.Errorf("failed to send log to ClickHouse endpoint %s: %s", endpoint, err)
		return
	}

	if resp.StatusCode() >= 400 {
		logger.Errorf(
			"ClickHouse endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			endpoint,
			resp.String(),
		)
	}
}

func (p *Plugin) buildInsertBody(log map[string]any) string {
	payload, err := json.Marshal(log)
	if err != nil {
		payload = []byte(`{}`)
	}
	return "INSERT INTO " + p.config.LogTable + " FORMAT JSONEachRow " + string(payload)
}

func (p *Plugin) endpointURL() string {
	if p.config.EndpointAddr != "" {
		return p.config.EndpointAddr
	}
	if len(p.config.EndpointAddrs) == 0 {
		return ""
	}
	return p.config.EndpointAddrs[randomEndpointIndex(len(p.config.EndpointAddrs))]
}

func (p *Plugin) endpointUID() string {
	if p.config.EndpointAddr != "" {
		return p.config.EndpointAddr
	}
	return strings.Join(p.config.EndpointAddrs, "\x00")
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
