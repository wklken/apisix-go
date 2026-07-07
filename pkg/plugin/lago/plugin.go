package lago

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
}

const (
	priority = 415
	name     = "lago"
)

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint_addrs": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "format": "uri"
      }
    },
    "endpoint_uri": {
      "type": "string",
      "minLength": 1,
      "default": "/api/v1/events/batch"
    },
    "token": {
      "type": "string"
    },
    "event_transaction_id": {
      "type": "string"
    },
    "event_subscription_id": {
      "type": "string"
    },
    "event_code": {
      "type": "string"
    },
    "event_properties": {
      "type": "object",
      "additionalProperties": {
        "type": "string",
        "minLength": 1
      }
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "maximum": 60000,
      "default": 3000
    },
    "keepalive": {
      "type": "boolean",
      "default": true
    },
    "keepalive_timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 60000
    },
    "keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    }
  },
  "required": ["endpoint_addrs", "token", "event_transaction_id", "event_subscription_id", "event_code"]
}
`

type Config struct {
	EndpointAddrs       []string          `json:"endpoint_addrs"`
	EndpointURI         string            `json:"endpoint_uri,omitempty"`
	Token               string            `json:"token"`
	EventTransactionID  string            `json:"event_transaction_id"`
	EventSubscriptionID string            `json:"event_subscription_id"`
	EventCode           string            `json:"event_code"`
	EventProperties     map[string]string `json:"event_properties,omitempty"`
	SSLVerify           *bool             `json:"ssl_verify,omitempty"`
	Timeout             int               `json:"timeout,omitempty"`
	Keepalive           *bool             `json:"keepalive,omitempty"`
	KeepaliveTimeout    int               `json:"keepalive_timeout,omitempty"`
	KeepalivePool       int               `json:"keepalive_pool,omitempty"`
}

type lagoPayload struct {
	Events []lagoEvent `json:"events"`
}

type lagoEvent struct {
	TransactionID          string            `json:"transaction_id"`
	ExternalSubscriptionID string            `json:"external_subscription_id"`
	Code                   string            `json:"code"`
	Timestamp              float64           `json:"timestamp"`
	Properties             map[string]string `json:"properties,omitempty"`
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (w *responseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

var templatePattern = regexp.MustCompile(`\$\{([^}]+)\}`)

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
	if p.config.EndpointURI == "" {
		p.config.EndpointURI = "/api/v1/events/batch"
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3000
	}
	if p.config.Keepalive == nil {
		value := true
		p.config.Keepalive = &value
	}
	if p.config.KeepaliveTimeout == 0 {
		p.config.KeepaliveTimeout = 60000
	}
	if p.config.KeepalivePool == 0 {
		p.config.KeepalivePool = 5
	}
	if p.config.SSLVerify == nil {
		value := true
		p.config.SSLVerify = &value
	}

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.EndpointAddrs)
	configUID.Add(p.config.EndpointURI)
	configUID.Add(p.config.Timeout)
	configUID.Add(*p.config.SSLVerify)
	configUID.Add(p.keepalive())

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Millisecond)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !*p.config.SSLVerify})
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	p.Consume()
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}

		p.Fire(p.logFields(r, recorder.status))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Send(fields map[string]any) {
	if len(p.config.EndpointAddrs) == 0 {
		return
	}

	resp, err := p.client.R().
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", "Bearer "+p.config.Token).
		SetBody(lagoPayload{Events: []lagoEvent{p.buildEvent(fields)}}).
		Post(p.endpointURL())
	if err != nil {
		logger.Errorf("failed to send Lago event to endpoint %s: %s", p.endpointURL(), err)
		return
	}
	if resp.StatusCode() >= 300 {
		logger.Errorf(
			"Lago endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			p.endpointURL(),
			resp.String(),
		)
	}
}

func (p *Plugin) buildEvent(fields map[string]any) lagoEvent {
	entry := lagoEvent{
		TransactionID:          resolveTemplate(p.config.EventTransactionID, fields),
		ExternalSubscriptionID: resolveTemplate(p.config.EventSubscriptionID, fields),
		Code:                   p.config.EventCode,
		Timestamp:              float64(time.Now().UnixNano()) / float64(time.Second),
	}

	if len(p.config.EventProperties) > 0 {
		entry.Properties = make(map[string]string, len(p.config.EventProperties))
		for key, value := range p.config.EventProperties {
			entry.Properties[key] = resolveTemplate(value, fields)
		}
	}

	return entry
}

func (p *Plugin) logFields(r *http.Request, status int) map[string]any {
	fields := map[string]any{"status": status}
	for _, template := range p.templates() {
		for _, name := range templateVariables(template) {
			if _, ok := fields[name]; ok {
				continue
			}
			fields[name] = requestVariable(r, name, status)
		}
	}
	return fields
}

func (p *Plugin) templates() []string {
	templates := []string{p.config.EventTransactionID, p.config.EventSubscriptionID}
	for _, value := range p.config.EventProperties {
		templates = append(templates, value)
	}
	return templates
}

func (p *Plugin) endpointURL() string {
	return strings.TrimRight(p.config.EndpointAddrs[0], "/") + p.config.EndpointURI
}

func (p *Plugin) keepalive() bool {
	return p.config.Keepalive == nil || *p.config.Keepalive
}

func resolveTemplate(template string, fields map[string]any) string {
	return templatePattern.ReplaceAllStringFunc(template, func(match string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(match, "${"), "}")
		if fields[name] == nil {
			return ""
		}
		return fmt.Sprint(fields[name])
	})
}

func templateVariables(template string) []string {
	matches := templatePattern.FindAllStringSubmatch(template, -1)
	variables := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) == 2 {
			variables = append(variables, match[1])
		}
	}
	return variables
}

func requestVariable(r *http.Request, name string, status int) any {
	if name == "status" {
		return status
	}
	if strings.HasPrefix(name, "http_") {
		return r.Header.Get(strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-"))
	}

	return log.GetField(r, "$"+name)
}
