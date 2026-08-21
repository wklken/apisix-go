package jwt_auth

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
	now    func() time.Time
}

const (
	priority = 2510
	name     = "jwt-auth"
)

const schema = `
{
  "type": "object",
  "properties": {
    "header": {
      "type": "string",
      "default": "authorization"
    },
    "query": {
      "type": "string",
      "default": "jwt"
    },
    "cookie": {
      "type": "string",
      "default": "jwt"
    },
    "hide_credentials": {
      "type": "boolean",
      "default": false
    },
    "key_claim_name": {
      "type": "string",
      "default": "key",
      "minLength": 1
    },
    "store_in_ctx": {
      "type": "boolean",
      "default": false
    },
    "realm": {
      "type": "string",
      "default": "jwt"
    },
    "anonymous_consumer": {
      "type": "string",
      "minLength": 1
    },
    "claims_to_verify": {
      "type": "array",
      "items": {
        "type": "string",
        "enum": ["exp", "nbf"]
      },
      "uniqueItems": true
    }
  }
}
`

type Config struct {
	Header            string   `json:"header,omitempty"`
	Query             string   `json:"query,omitempty"`
	Cookie            string   `json:"cookie,omitempty"`
	HideCredentials   *bool    `json:"hide_credentials,omitempty"`
	KeyClaimName      string   `json:"key_claim_name,omitempty"`
	StoreInCtx        *bool    `json:"store_in_ctx,omitempty"`
	Realm             string   `json:"realm,omitempty"`
	AnonymousConsumer string   `json:"anonymous_consumer,omitempty"`
	ClaimsToVerify    []string `json:"claims_to_verify,omitempty"`
}

type consumerConfig struct {
	Key                 string `json:"key"`
	Secret              string `json:"secret"`
	PublicKey           string `json:"public_key,omitempty"`
	Algorithm           string `json:"algorithm,omitempty"`
	Base64Secret        *bool  `json:"base64_secret,omitempty"`
	LifetimeGracePeriod int64  `json:"lifetime_grace_period,omitempty"`
}

type jwtToken = base.JWTToken

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Header == "" {
		p.config.Header = "authorization"
	}
	if p.config.Query == "" {
		p.config.Query = "jwt"
	}
	if p.config.Cookie == "" {
		p.config.Cookie = "jwt"
	}
	if p.config.HideCredentials == nil {
		b := false
		p.config.HideCredentials = &b
	}
	if p.config.KeyClaimName == "" {
		p.config.KeyClaimName = "key"
	}
	if p.config.StoreInCtx == nil {
		b := false
		p.config.StoreInCtx = &b
	}
	if p.config.Realm == "" {
		p.config.Realm = "jwt"
	}
	if p.now == nil {
		p.now = time.Now
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
	consumer, token, errMsg := p.findConsumer(r)
	if errMsg != "" {
		if result, ok := p.anonymousConsumerResult(w, r); ok {
			return result
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, p.config.Realm))
		http.Error(w, util.BuildMessageResponse(errMsg), http.StatusUnauthorized)
		return base.StopRequest(r)
	}

	if *p.config.StoreInCtx {
		ctx.RegisterApisixVar(r, "$jwt_auth_payload", token.Payload)
	}
	r = ctx.WithAuthenticationState(r, ctx.NewAuthenticationState(name, consumer))
	return base.ContinueRequest(r)
}

func (p *Plugin) anonymousConsumerResult(w http.ResponseWriter, r *http.Request) (base.RequestPhaseResult, bool) {
	if p.config.AnonymousConsumer == "" {
		return base.RequestPhaseResult{}, false
	}

	consumer, err := store.GetConsumer(p.config.AnonymousConsumer)
	if err != nil {
		ctx.RecordAuthProbeDiagnostic(r, fmt.Sprintf("failed to get anonymous consumer %s", p.config.AnonymousConsumer))
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, p.config.Realm))
		http.Error(w, util.BuildMessageResponse("Invalid user authorization"), http.StatusUnauthorized)
		return base.StopRequest(r), true
	}

	return base.ContinueRequest(ctx.WithAuthenticationState(r, ctx.NewAuthenticationState(name, consumer))), true
}

func (p *Plugin) findConsumer(r *http.Request) (resource.Consumer, jwtToken, string) {
	rawToken, ok := p.fetchToken(r)
	if !ok {
		return resource.Consumer{}, jwtToken{}, "Missing JWT token in request"
	}

	token, err := base.ParseJWT(rawToken)
	if err != nil {
		return resource.Consumer{}, jwtToken{}, "JWT token invalid"
	}

	userKey, ok := token.Payload[p.config.KeyClaimName].(string)
	if !ok || userKey == "" {
		return resource.Consumer{}, token, "missing user key in JWT token"
	}

	consumer, err := store.GetConsumerByPluginKey(name, userKey)
	if err != nil {
		return resource.Consumer{}, token, "Invalid user key in JWT token"
	}

	pluginConfig, ok := consumer.Plugins[name]
	if !ok {
		return resource.Consumer{}, token, "Missing jwt-auth config in consumer settings"
	}

	var authConfig consumerConfig
	if err := util.Parse(pluginConfig, &authConfig); err != nil {
		return resource.Consumer{}, token, "Invalid jwt-auth config in consumer settings"
	}
	if authConfig.Algorithm == "" {
		authConfig.Algorithm = "HS256"
	}

	now := p.now()
	claims, err := verifyToken(
		rawToken,
		authConfig,
		now,
		time.Duration(authConfig.LifetimeGracePeriod)*time.Second,
		p.config.ClaimsToVerify,
	)
	if err != nil {
		return resource.Consumer{}, token, "failed to verify jwt"
	}

	return consumer, jwtToken{
		Header:    token.Header,
		Payload:   claims,
		Signing:   token.Signing,
		Signature: token.Signature,
	}, ""
}

// verifyToken parses and verifies a raw JWT against a consumer configuration.
// The token algorithm must match the consumer algorithm, signatures are
// checked with the consumer secret or public key, and exp/nbf claims follow
// APISIX semantics with the configured grace period.
func (p *Plugin) fetchToken(r *http.Request) (string, bool) {
	if token := ctx.RestoreTrustedRequestHeader(r, p.config.Header); token != "" {
		if *p.config.HideCredentials {
			r.Header.Del(p.config.Header)
		}
		if strings.HasPrefix(token, "Bearer ") || strings.HasPrefix(token, "bearer ") {
			return token[7:], true
		}
		return token, true
	}

	query := r.URL.Query()
	if token := query.Get(p.config.Query); token != "" {
		if *p.config.HideCredentials {
			query.Del(p.config.Query)
			r.URL.RawQuery = query.Encode()
		}
		return token, true
	}

	cookie, err := r.Cookie(p.config.Cookie)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	if *p.config.HideCredentials {
		removeCookie(r, p.config.Cookie)
	}
	return cookie.Value, true
}

func (c consumerConfig) secret() ([]byte, bool) {
	if c.Secret == "" {
		return nil, false
	}

	if c.Base64Secret != nil && *c.Base64Secret {
		decoded, err := base64.StdEncoding.DecodeString(c.Secret)
		if err != nil {
			return nil, false
		}
		return decoded, true
	}

	return []byte(c.Secret), true
}

func (c consumerConfig) publicKey() (any, bool) {
	if c.PublicKey == "" {
		return nil, false
	}

	publicKeyBytes := []byte(c.PublicKey)
	if block, _ := pem.Decode(publicKeyBytes); block != nil {
		publicKeyBytes = block.Bytes
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(publicKeyBytes)
			if err != nil {
				return nil, false
			}
			return cert.PublicKey, true
		}
	}

	if publicKey, err := x509.ParsePKIXPublicKey(publicKeyBytes); err == nil {
		return publicKey, true
	}
	if publicKey, err := x509.ParsePKCS1PublicKey(publicKeyBytes); err == nil {
		return publicKey, true
	}

	return nil, false
}

func removeCookie(r *http.Request, name string) {
	cookieHeader := r.Header.Get("Cookie")
	if cookieHeader == "" {
		return
	}

	parts := strings.Split(cookieHeader, ";")
	kept := parts[:0]
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, name+"=") {
			continue
		}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		r.Header.Del("Cookie")
		return
	}
	r.Header.Set("Cookie", strings.Join(kept, "; "))
}
