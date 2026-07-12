package kafka_proxy

import (
	"context"
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/data_encryption"
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

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.SASL != nil {
		keyring, enabled := data_encryption.Keyring()
		resolved, err := data_encryption.NewResolver(enabled, keyring).Resolve(p.config.SASL.Password)
		if err != nil {
			return fmt.Errorf("kafka-proxy sasl.password: %w", err)
		}
		p.config.SASL.Password = resolved
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.SASL != nil {
			ctx := context.WithValue(r.Context(), ctxSASLEnabled, true)
			ctx = context.WithValue(ctx, ctxSASLUsername, p.config.SASL.Username)
			ctx = context.WithValue(ctx, ctxSASLPassword, p.config.SASL.Password)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
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
