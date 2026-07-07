package skywalking_logger

import (
	"encoding/base64"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/apisix/log"
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
	priority = 408
	name     = "skywalking-logger"

	internalSkyWalkingEndpoint     = "_skywalking_endpoint"
	internalSkyWalkingTraceContext = "_skywalking_trace_context"
)

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint_addr": {
      "type": "string",
      "format": "uri"
    },
    "service_name": {
      "type": "string",
      "default": "APISIX"
    },
    "service_instance_name": {
      "type": "string",
      "default": "APISIX Instance Name"
    },
    "log_format": {
      "type": "object"
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 3
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
  "required": ["endpoint_addr"]
}
`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Config struct {
	EndpointAddr        string            `json:"endpoint_addr"`
	ServiceName         string            `json:"service_name,omitempty"`
	ServiceInstanceName string            `json:"service_instance_name,omitempty"`
	LogFormat           map[string]string `json:"log_format,omitempty"`
	Timeout             int               `json:"timeout,omitempty"`
	IncludeReqBody      bool              `json:"include_req_body,omitempty"`
	IncludeRespBody     bool              `json:"include_resp_body,omitempty"`
	MaxReqBodyBytes     int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int               `json:"max_resp_body_bytes,omitempty"`
}

type skyWalkingEntry struct {
	TraceContext    *traceContext `json:"traceContext,omitempty"`
	Body            logBody       `json:"body"`
	Service         string        `json:"service"`
	ServiceInstance string        `json:"serviceInstance"`
	Endpoint        string        `json:"endpoint"`
}

type traceContext struct {
	TraceID        string `json:"traceId"`
	TraceSegmentID string `json:"traceSegmentId"`
	SpanID         int    `json:"spanId"`
}

type logBody struct {
	JSON jsonWrapper `json:"json"`
}

type jsonWrapper struct {
	JSON string `json:"json"`
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
	if p.config.ServiceName == "" {
		p.config.ServiceName = "APISIX"
	}
	if p.config.ServiceInstanceName == "" {
		p.config.ServiceInstanceName = "APISIX Instance Name"
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3
	}

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.EndpointAddr)
	configUID.Add(p.config.Timeout)

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Second)
	client.SetHeader("Content-Type", "application/json")
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
	fn := func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		logFields := log.GetFields(r, p.LogFormat)
		logFields[internalSkyWalkingEndpoint] = r.URL.Path
		if trace, ok := parseTraceContext(r.Header.Get("sw8")); ok {
			logFields[internalSkyWalkingTraceContext] = trace
		}
		p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Send(log map[string]any) {
	resp, err := p.client.R().SetBody([]skyWalkingEntry{p.buildEntry(log)}).Post(p.endpointURL())
	if err != nil {
		logger.Errorf("failed to send log to SkyWalking endpoint %s: %s", p.endpointURL(), err)
		return
	}

	if resp.StatusCode() >= 400 {
		logger.Errorf(
			"SkyWalking endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			p.endpointURL(),
			resp.String(),
		)
	}
}

func (p *Plugin) buildEntry(log map[string]any) skyWalkingEntry {
	payload := make(map[string]any, len(log))
	for key, value := range log {
		if key == internalSkyWalkingEndpoint || key == internalSkyWalkingTraceContext {
			continue
		}
		payload[key] = value
	}

	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{}`)
	}

	entry := skyWalkingEntry{
		Body: logBody{
			JSON: jsonWrapper{
				JSON: string(body),
			},
		},
		Service:         p.config.ServiceName,
		ServiceInstance: p.serviceInstanceName(),
		Endpoint:        endpointFromLog(log),
	}
	if trace, ok := log[internalSkyWalkingTraceContext].(*traceContext); ok {
		entry.TraceContext = trace
	}
	return entry
}

func endpointFromLog(log map[string]any) string {
	if endpoint, ok := log[internalSkyWalkingEndpoint].(string); ok {
		return endpoint
	}
	return ""
}

func (p *Plugin) serviceInstanceName() string {
	if p.config.ServiceInstanceName != "$hostname" {
		return p.config.ServiceInstanceName
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "$hostname"
	}
	return hostname
}

func (p *Plugin) endpointURL() string {
	return strings.TrimRight(p.config.EndpointAddr, "/") + "/v3/logs"
}

func parseTraceContext(header string) (*traceContext, bool) {
	if header == "" {
		return nil, false
	}
	parts := strings.Split(header, "-")
	if len(parts) != 8 {
		return nil, false
	}

	traceID, err := decodeBase64URL(parts[1])
	if err != nil {
		return nil, false
	}
	segmentID, err := decodeBase64URL(parts[2])
	if err != nil {
		return nil, false
	}
	spanID, err := strconv.Atoi(parts[3])
	if err != nil {
		return nil, false
	}

	return &traceContext{
		TraceID:        traceID,
		TraceSegmentID: segmentID,
		SpanID:         spanID,
	}, true
}

func decodeBase64URL(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return string(decoded), nil
	}
	decoded, err = base64.URLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
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
