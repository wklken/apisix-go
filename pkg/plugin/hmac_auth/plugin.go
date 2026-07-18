package hmac_auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
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
}

type consumerConfig struct {
	KeyID     string `json:"key_id"`
	SecretKey string `json:"secret_key"`
}

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
	fn := func(w http.ResponseWriter, r *http.Request) {
		consumer, statusCode, err := p.authenticate(r)
		if err != nil {
			logMessage := err.Error()
			if !strings.HasPrefix(logMessage, "client request can't be validated:") {
				logMessage = "client request can't be validated: " + logMessage
			}
			if !ctx.RecordAuthProbeDiagnostic(r, logMessage) {
				logger.Warn(logMessage)
			}
			if statusCode == http.StatusUnauthorized && p.attachAnonymousConsumer(w, r, next) {
				return
			}
			message := "client request can't be validated"
			if strings.HasPrefix(err.Error(), "client request can't be validated:") ||
				statusCode != http.StatusUnauthorized {
				message = err.Error()
			}
			p.writeAuthError(w, statusCode, message)
			return
		}

		if *p.config.HideCredentials {
			r.Header.Del("Authorization")
		}

		ctx.AttachConsumer(r, consumer)
		ctx.RunConsumerPlugins(w, r, next)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) attachAnonymousConsumer(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if p.config.AnonymousConsumer == "" {
		return false
	}

	consumer, err := store.GetConsumer(p.config.AnonymousConsumer)
	if err != nil {
		message := fmt.Sprintf("failed to get anonymous consumer %s", p.config.AnonymousConsumer)
		if !ctx.RecordAuthProbeDiagnostic(r, message) {
			logger.Error(message)
		}
		p.writeAuthError(w, http.StatusUnauthorized, "Invalid user authorization")
		return true
	}

	if *p.config.HideCredentials {
		r.Header.Del("Authorization")
	}
	ctx.AttachConsumer(r, consumer)
	ctx.RunConsumerPlugins(w, r, next)
	return true
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

	consumer, err := store.GetConsumerByPluginKey(name, params.KeyID)
	if err != nil {
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
	if err := p.validateSignedHeaders(params.Headers); err != nil {
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

func retrieveSignatureParams(r *http.Request) (signatureParams, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return signatureParams{}, errors.New("missing Authorization header")
	}
	if !strings.HasPrefix(auth, "Signature") {
		return signatureParams{}, errors.New("authorization header does not start with 'Signature'")
	}

	fields := strings.Split(strings.TrimSpace(strings.TrimPrefix(auth, "Signature")), ",")
	params := signatureParams{
		Date:       r.Header.Get("Date"),
		BodyDigest: r.Header.Get("Digest"),
	}
	for _, field := range fields {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"`)
		switch key {
		case "keyId":
			params.KeyID = value
		case "algorithm":
			params.Algorithm = value
		case "headers":
			params.Headers = strings.Fields(value)
		case "signature":
			params.Signature = value
		}
	}

	return params, nil
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

func (p *Plugin) validateSignedHeaders(headers []string) error {
	paramsHeaders := map[string]struct{}{}
	for _, header := range headers {
		paramsHeaders[header] = struct{}{}
	}

	for _, header := range p.config.SignedHeaders {
		if _, ok := paramsHeaders[header]; !ok {
			return fmt.Errorf("expected header %q missing in signing", header)
		}
	}
	return nil
}

func validateSignature(r *http.Request, secretKey string, params signatureParams) error {
	requestSignature, err := base64.StdEncoding.DecodeString(params.Signature)
	if err != nil {
		return errInvalidSignature
	}

	generatedSignature, err := generateSignature(r, secretKey, params)
	if err != nil {
		return errInvalidSignature
	}
	if subtle.ConstantTimeCompare(requestSignature, generatedSignature) != 1 {
		return errInvalidSignature
	}
	return nil
}

func generateSignature(r *http.Request, secretKey string, params signatureParams) ([]byte, error) {
	var signingString strings.Builder
	signingString.WriteString(params.KeyID + "\n")
	for _, header := range params.Headers {
		if header == "@request-target" {
			signingString.WriteString(r.Method + " " + requestURI(r) + "\n")
			continue
		}
		if value := r.Header.Get(header); value != "" {
			signingString.WriteString(header + ": " + value + "\n")
		}
	}

	hashFunc, err := hashForAlgorithm(params.Algorithm)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(hashFunc, []byte(secretKey))
	mac.Write([]byte(signingString.String()))
	return mac.Sum(nil), nil
}

func hashForAlgorithm(algorithm string) (func() hash.Hash, error) {
	switch algorithm {
	case "hmac-sha1":
		return sha1.New, nil
	case "hmac-sha256":
		return sha256.New, nil
	case "hmac-sha512":
		return sha512.New, nil
	default:
		return nil, fmt.Errorf("unsupported algorithm %q", algorithm)
	}
}

func requestURI(r *http.Request) string {
	if r.URL == nil || r.URL.RequestURI() == "" {
		return "/"
	}
	return r.URL.RequestURI()
}

var errBodyTooLarge = errors.New("request body too large")

func (p *Plugin) validateBodyDigest(r *http.Request, digestHeader string) error {
	if digestHeader == "" {
		return errInvalidDigest
	}

	body, err := readAndRestoreBody(r, p.config.MaxReqBodySize)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	expected := "SHA-256=" + base64.StdEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(expected), []byte(digestHeader)) != 1 {
		return errInvalidDigest
	}
	return nil
}

func readAndRestoreBody(r *http.Request, maxSize int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxSize {
		return nil, errBodyTooLarge
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
