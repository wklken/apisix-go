package hmac_auth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 2530
	name     = "hmac-auth"
)

var (
	errInvalidKeyID      = errors.New("Invalid key_id")          //nolint:staticcheck // APISIX-compatible diagnostic.
	errInvalidAlgorithm  = errors.New("Invalid algorithm")       //nolint:staticcheck // APISIX-compatible diagnostic.
	errInvalidDigest     = errors.New("Invalid digest")          //nolint:staticcheck // APISIX-compatible diagnostic.
	errDateHeaderMissing = errors.New("Date header missing")     //nolint:staticcheck // APISIX-compatible diagnostic.
	errInvalidGMTTime    = errors.New("Invalid GMT format time") //nolint:staticcheck // APISIX-compatible diagnostic.
	errClockSkewExceeded = errors.New("Clock skew exceeded")     //nolint:staticcheck // APISIX-compatible diagnostic.
	errInvalidSignature  = errors.New("Invalid signature")       //nolint:staticcheck // APISIX-compatible diagnostic.
)

const schema = `
{
  "type": "object",
  "properties": {
    "allowed_algorithms": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "enum": ["hmac-sha1", "hmac-sha256", "hmac-sha512"]
      },
      "default": ["hmac-sha1", "hmac-sha256", "hmac-sha512"]
    },
    "clock_skew": {
      "type": "integer",
      "default": 300,
      "minimum": 1
    },
    "signed_headers": {
      "type": "array",
      "default": ["date"],
      "items": {
        "type": "string",
        "minLength": 1,
        "maxLength": 50
      }
    },
    "validate_request_body": {
      "type": "boolean",
      "default": false
    },
    "max_req_body_size": {
      "type": "integer",
      "minimum": 1,
      "default": 67108864
    },
    "hide_credentials": {
      "type": "boolean",
      "default": false
    },
    "realm": {
      "type": "string",
      "default": "hmac"
    },
    "anonymous_consumer": {
      "type": "string",
      "minLength": 1
    }
  }
}
`

type Config struct {
	AllowedAlgorithms   []string `json:"allowed_algorithms,omitempty"`
	ClockSkew           int      `json:"clock_skew,omitempty"`
	SignedHeaders       []string `json:"signed_headers,omitempty"`
	ValidateRequestBody bool     `json:"validate_request_body,omitempty"`
	MaxReqBodySize      int64    `json:"max_req_body_size,omitempty"`
	HideCredentials     *bool    `json:"hide_credentials,omitempty"`
	Realm               string   `json:"realm,omitempty"`
	AnonymousConsumer   string   `json:"anonymous_consumer,omitempty"`

	validationBodyLimit int64
	captureBodyLimit    int64
	requestBodyTempDir  string
}

type consumerConfig struct {
	KeyID     string `json:"key_id"`
	SecretKey string `json:"secret_key"`
}

// BodyIsolation declares that hmac-auth consumes the request body during
// validation so multi-auth can isolate and replay it. Implementations of
// multi-auth's requestBodyIsolation contract advertise their body needs
// instead of being special-cased by type.
func (c *Config) BodyIsolation() (bool, int64) {
	return c.ValidateRequestBody, c.captureBodyLimit
}

func (c *Config) BodyIsolationTempDir() string { return c.requestBodyTempDir }

type signatureParams struct {
	KeyID      string
	Algorithm  string
	Headers    []string
	Signature  string
	Date       string
	BodyDigest string
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if len(p.config.AllowedAlgorithms) == 0 {
		p.config.AllowedAlgorithms = []string{"hmac-sha1", "hmac-sha256", "hmac-sha512"}
	}
	if p.config.ClockSkew == 0 {
		p.config.ClockSkew = 300
	}
	if p.config.SignedHeaders == nil {
		p.config.SignedHeaders = []string{"date"}
	}
	if p.config.MaxReqBodySize == 0 {
		p.config.MaxReqBodySize = 64 * 1024 * 1024
	}
	p.config.validationBodyLimit = p.config.MaxReqBodySize
	p.config.captureBodyLimit = max(p.config.MaxReqBodySize, int64(64*1024*1024))
	if effective := p.StaticConfig(); effective != nil {
		if ingressLimit := effective.Config.NginxConfig.HTTP.ClientMaxBodySize; ingressLimit > 0 {
			p.config.captureBodyLimit = ingressLimit
			p.config.validationBodyLimit = min(p.config.validationBodyLimit, ingressLimit)
		}
		p.config.requestBodyTempDir = effective.Paths.TempDir
	}
	if p.config.HideCredentials == nil {
		hideCredentials := false
		p.config.HideCredentials = &hideCredentials
	}
	if p.config.Realm == "" {
		p.config.Realm = "hmac"
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return base.AdaptRequestPhase(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx.AttachConsumerFromAuthenticationState(r)
		next.ServeHTTP(w, r)
	}))
}

func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	consumer, statusCode, err := p.authenticate(r)
	if err != nil {
		logMessage := err.Error()
		if !strings.HasPrefix(logMessage, "client request can't be validated:") {
			logMessage = "client request can't be validated: " + logMessage
		}
		if !ctx.RecordAuthProbeDiagnostic(r, logMessage) {
			logger.Warn(logMessage)
		}
		if statusCode == http.StatusUnauthorized {
			if result, ok := p.anonymousConsumerResult(w, r); ok {
				return result
			}
		}
		message := "client request can't be validated"
		if strings.HasPrefix(err.Error(), "client request can't be validated:") ||
			statusCode != http.StatusUnauthorized {
			message = err.Error()
		}
		p.writeAuthError(w, statusCode, message)
		return base.StopRequest(r)
	}

	if *p.config.HideCredentials {
		r.Header.Del("Authorization")
	}

	r = ctx.WithAuthenticationState(r, ctx.NewAuthenticationState(name, consumer))
	return base.ContinueRequest(r)
}

func (p *Plugin) anonymousConsumerResult(w http.ResponseWriter, r *http.Request) (base.RequestPhaseResult, bool) {
	if p.config.AnonymousConsumer == "" {
		return base.RequestPhaseResult{}, false
	}

	consumer, ok := p.consumerByID(p.config.AnonymousConsumer)
	if !ok {
		message := fmt.Sprintf("failed to get anonymous consumer %s", p.config.AnonymousConsumer)
		if !ctx.RecordAuthProbeDiagnostic(r, message) {
			logger.Error(message)
		}
		p.writeAuthError(w, http.StatusUnauthorized, "Invalid user authorization")
		return base.StopRequest(r), true
	}

	if *p.config.HideCredentials {
		r.Header.Del("Authorization")
	}
	return base.ContinueRequest(ctx.WithAuthenticationState(r, ctx.NewAuthenticationState(name, consumer))), true
}

func (p *Plugin) writeAuthError(w http.ResponseWriter, statusCode int, message string) {
	if statusCode == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`hmac realm="%s"`, p.config.Realm))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(util.BuildMessageResponse(message)))
}

func (p *Plugin) authenticate(r *http.Request) (resource.Consumer, int, error) {
	params, err := retrieveSignatureParams(r)
	if err != nil {
		return resource.Consumer{}, http.StatusUnauthorized, fmt.Errorf("client request can't be validated: %w", err)
	}

	if params.KeyID == "" || params.Signature == "" {
		return resource.Consumer{}, http.StatusUnauthorized, errors.New("keyId or signature missing")
	}
	if params.Algorithm == "" {
		return resource.Consumer{}, http.StatusUnauthorized, errors.New("algorithm missing")
	}

	consumer, ok := p.consumerByPluginKey(params.KeyID)
	if !ok {
		return resource.Consumer{}, http.StatusUnauthorized, errInvalidKeyID
	}
	if !p.algorithmAllowed(params.Algorithm) {
		return resource.Consumer{}, http.StatusUnauthorized, errInvalidAlgorithm
	}

	consumerPluginConfig, exists := consumer.Plugins[name]
	if !exists {
		return resource.Consumer{}, http.StatusUnauthorized, errInvalidKeyID
	}

	var cfg consumerConfig
	if err := util.Parse(consumerPluginConfig, &cfg); err != nil {
		return resource.Consumer{}, http.StatusUnauthorized, errInvalidKeyID
	}

	if err := p.validateClockSkew(params.Date); err != nil {
		return resource.Consumer{}, http.StatusUnauthorized, err
	}
	if err := p.validateSignedHeaders(r, params.Headers); err != nil {
		return resource.Consumer{}, http.StatusUnauthorized, err
	}
	if err := validateSignature(r, cfg.SecretKey, params); err != nil {
		return resource.Consumer{}, http.StatusUnauthorized, err
	}
	if p.config.ValidateRequestBody {
		if err := p.validateBodyDigest(r, params.BodyDigest); err != nil {
			if errors.Is(err, errBodyTooLarge) {
				return resource.Consumer{}, http.StatusRequestEntityTooLarge, err
			}
			return resource.Consumer{}, http.StatusUnauthorized, errInvalidDigest
		}
	}

	return consumer, 0, nil
}

func (p *Plugin) consumerByPluginKey(key string) (resource.Consumer, bool) {
	lookup := p.ConsumerLookup()
	if lookup == nil {
		return resource.Consumer{}, false
	}
	return lookup.ConsumerByPluginKey(name, key)
}

func (p *Plugin) consumerByID(id string) (resource.Consumer, bool) {
	lookup := p.ConsumerLookup()
	if lookup == nil {
		return resource.Consumer{}, false
	}
	return lookup.ConsumerByID(id)
}

func (p *Plugin) algorithmAllowed(algorithm string) bool {
	return slices.Contains(p.config.AllowedAlgorithms, algorithm)
}

func (p *Plugin) validateClockSkew(date string) error {
	if p.config.ClockSkew <= 0 {
		return nil
	}
	if date == "" {
		return errDateHeaderMissing
	}

	parsed, err := http.ParseTime(date)
	if err != nil {
		return errInvalidGMTTime
	}
	if time.Since(parsed).Abs() > time.Duration(p.config.ClockSkew)*time.Second {
		return errClockSkewExceeded
	}
	return nil
}

func (p *Plugin) validateSignedHeaders(r *http.Request, headers []string) error {
	paramsHeaders := map[string]struct{}{}
	for _, header := range headers {
		paramsHeaders[header] = struct{}{}
	}

	for _, header := range p.config.SignedHeaders {
		if _, ok := paramsHeaders[header]; !ok {
			return fmt.Errorf("expected header %q missing in signing", header)
		}
		if header != "@request-target" && signedHeaderValue(r, header) == "" {
			return fmt.Errorf("expected header %q missing in request", header)
		}
	}
	return nil
}

func (p *Plugin) validateBodyDigest(r *http.Request, digestHeader string) error {
	if digestHeader == "" {
		return errInvalidDigest
	}

	snapshot, err := base.EnsureRequestBodySnapshot(
		r,
		p.config.captureBodyLimit,
		base.DefaultRequestBodySnapshotMemoryLimit,
		p.config.requestBodyTempDir,
	)
	if err != nil {
		if errors.Is(err, base.ErrRequestBodyTooLarge) {
			return errBodyTooLarge
		}
		return err
	}
	if snapshot.Size() > p.config.validationBodyLimit {
		return errBodyTooLarge
	}
	sum := snapshot.SHA256()
	expected := "SHA-256=" + base64.StdEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(expected), []byte(digestHeader)) != 1 {
		return errInvalidDigest
	}
	return nil
}
