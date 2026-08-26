package kafka_proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

type Plugin struct {
	base.BasePlugin
	config Config

	secretMu           sync.RWMutex
	saslPassword       *secret.Value
	legacySASLPassword *string
	stopped            bool
	stopBeforeLock     func()
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

type requestSASLCredentials struct {
	mu       sync.RWMutex
	password string
}

func (credentials *requestSASLCredentials) Password() string {
	if credentials == nil {
		return ""
	}
	credentials.mu.RLock()
	defer credentials.mu.RUnlock()
	return credentials.password
}

func (credentials *requestSASLCredentials) clear() {
	if credentials == nil {
		return
	}
	credentials.mu.Lock()
	credentials.password = ""
	credentials.mu.Unlock()
}

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
	if p.config.SASL == nil {
		return nil
	}
	p.secretMu.RLock()
	prepared := !p.stopped && (p.saslPassword != nil || p.legacySASLPassword != nil)
	p.secretMu.RUnlock()
	if !prepared {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

// MaterializeSecrets is the transitional process-local compatibility path.
// Scoped generation preparation uses MaterializeScopedSecrets instead.
func (p *Plugin) MaterializeSecrets() error {
	if p.config.SASL == nil {
		return nil
	}

	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.stopped {
		return secret.ErrCredentialUnavailable
	}
	if p.saslPassword != nil || p.legacySASLPassword != nil {
		return nil
	}

	resolver := p.DataEncryption()
	if !resolver.Configured() {
		return errors.New("data-encryption resolver is required")
	}
	resolved, err := resolver.ResolveForContext(
		p.config.SASL.Password,
		"kafka-proxy.sasl.password",
	)
	if err != nil || strings.TrimSpace(resolved) == "" {
		return fmt.Errorf("kafka-proxy sasl.password: %w", secret.ErrCredentialUnavailable)
	}
	digest := sha256.Sum256([]byte(resolved))
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		return fmt.Errorf("kafka-proxy sasl.password: %w", secret.ErrCredentialUnavailable)
	}

	p.config.SASL.Password = descriptor.String()
	p.legacySASLPassword = &resolved
	return nil
}

// MaterializeScopedSecrets admits only the manifest-owned sasl.password for
// this generation attempt. Public config retains only a content descriptor.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	if p.config.SASL == nil {
		return nil
	}

	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.stopped {
		return secret.ErrCredentialUnavailable
	}
	if p.saslPassword != nil || p.legacySASLPassword != nil {
		return nil
	}

	value, err := access.Materialize(ctx, "sasl.password", p.config.SASL.Password)
	if err != nil {
		return fmt.Errorf("kafka-proxy sasl.password: %w", secret.ErrCredentialUnavailable)
	}
	if err := value.Use(func(password string) error {
		if strings.TrimSpace(password) == "" {
			return secret.ErrCredentialUnavailable
		}
		return nil
	}); err != nil {
		return fmt.Errorf("kafka-proxy sasl.password: %w", secret.ErrCredentialUnavailable)
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return fmt.Errorf("kafka-proxy sasl.password: %w", secret.ErrCredentialUnavailable)
	}

	p.config.SASL.Password = descriptor.String()
	p.saslPassword = &value
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.SASL == nil {
			next.ServeHTTP(w, r)
			return
		}
		err := p.useSASLPassword(func(password string) error {
			credentials := &requestSASLCredentials{password: password}
			defer credentials.clear()
			next.ServeHTTP(w, p.prepareRequest(r, credentials))
			return nil
		})
		if err != nil {
			http.Error(w, "Kafka SASL credentials unavailable", http.StatusInternalServerError)
		}
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) useSASLPassword(use func(string) error) error {
	if use == nil {
		return secret.ErrCredentialUnavailable
	}
	p.secretMu.RLock()
	defer p.secretMu.RUnlock()
	if p.stopped {
		return secret.ErrCredentialUnavailable
	}
	if p.saslPassword != nil {
		return p.saslPassword.Use(use)
	}
	if p.legacySASLPassword != nil {
		return use(*p.legacySASLPassword)
	}
	return secret.ErrCredentialUnavailable
}

func (p *Plugin) prepareRequest(
	r *http.Request, credentials *requestSASLCredentials,
) *http.Request {
	ctx := context.WithValue(r.Context(), ctxSASLEnabled, true)
	ctx = context.WithValue(ctx, ctxSASLUsername, p.config.SASL.Username)
	ctx = context.WithValue(ctx, ctxSASLPassword, credentials)
	return r.WithContext(ctx)
}

func (p *Plugin) Stop() {
	if p.stopBeforeLock != nil {
		p.stopBeforeLock()
	}
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	if p.saslPassword != nil {
		*p.saslPassword = secret.Value{}
		p.saslPassword = nil
	}
	if p.legacySASLPassword != nil {
		*p.legacySASLPassword = ""
		p.legacySASLPassword = nil
	}
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
	credentials, _ := r.Context().Value(ctxSASLPassword).(*requestSASLCredentials)
	return credentials.Password()
}
