package kafka_proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 508
	name     = "kafka-proxy"

	ctxSASLEnabled  contextKey = "kafka_consumer_enable_sasl"
	ctxSASLUsername contextKey = "kafka_consumer_sasl_username"
	ctxSASLPassword contextKey = "kafka_consumer_sasl_password"
)

const schema = `
{
  "type": "object",
  "properties": {
    "sasl": {
      "type": "object",
      "properties": {
        "username": {
          "type": "string"
        },
        "password": {
          "type": "string"
        }
      },
      "required": ["username", "password"]
    }
  }
}
`

type Config struct {
	SASL *SASL `json:"sasl,omitempty"`
}

type SASL struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type contextKey string

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if !p.DataEncryption().Configured() {
		return errors.New("data-encryption resolver is required")
	}
	if p.config.SASL != nil {
		resolved, err := p.DataEncryption().ResolveForContext(
			p.config.SASL.Password,
			"kafka-proxy.sasl.password",
		)
		if err != nil {
			return fmt.Errorf("kafka-proxy sasl.password: %w", err)
		}
		p.config.SASL.Password = resolved
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) != nil {
			base.AdaptRequestPhase(p, next).ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, p.prepareRequest(r))
	}
	return http.HandlerFunc(fn)
}

// RunRequestPhase prepares Kafka consumer credentials for the route-owned
// protocol terminal. It intentionally does not upgrade, hijack, or write.
func (p *Plugin) RunRequestPhase(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	return base.ContinueRequest(p.prepareRequest(r))
}

func (p *Plugin) prepareRequest(r *http.Request) *http.Request {
	if p.config.SASL != nil {
		ctx := context.WithValue(r.Context(), ctxSASLEnabled, true)
		ctx = context.WithValue(ctx, ctxSASLUsername, p.config.SASL.Username)
		ctx = context.WithValue(ctx, ctxSASLPassword, p.config.SASL.Password)
		r = r.WithContext(ctx)
	}
	return r
}

func SASLEnabled(r *http.Request) bool {
	enabled, _ := r.Context().Value(ctxSASLEnabled).(bool)
	return enabled
}

func SASLUsername(r *http.Request) string {
	username, _ := r.Context().Value(ctxSASLUsername).(string)
	return username
}

func SASLPassword(r *http.Request) string {
	password, _ := r.Context().Value(ctxSASLPassword).(string)
	return password
}
