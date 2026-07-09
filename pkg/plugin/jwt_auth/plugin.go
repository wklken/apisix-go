package jwt_auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"hash"
	"net/http"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
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
	Algorithm           string `json:"algorithm,omitempty"`
	Base64Secret        *bool  `json:"base64_secret,omitempty"`
	LifetimeGracePeriod int64  `json:"lifetime_grace_period,omitempty"`
}

type jwtToken struct {
	header    map[string]any
	payload   map[string]any
	signing   string
	signature []byte
}

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

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		consumer, token, errMsg := p.findConsumer(r)
		if errMsg != "" {
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, p.config.Realm))
			http.Error(w, util.BuildMessageResponse(errMsg), http.StatusUnauthorized)
			return
		}

		if *p.config.StoreInCtx {
			ctx.RegisterApisixVar(r, "$jwt_auth_payload", token.payload)
		}
		ctx.AttachConsumer(r, consumer)

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) attachAnonymousConsumer(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if p.config.AnonymousConsumer == "" {
		return false
	}

	consumer, err := store.GetConsumer(p.config.AnonymousConsumer)
	if err != nil {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, p.config.Realm))
		http.Error(w, util.BuildMessageResponse("Invalid user authorization"), http.StatusUnauthorized)
		return true
	}

	ctx.AttachConsumer(r, consumer)
	next.ServeHTTP(w, r)
	return true
}

func (p *Plugin) findConsumer(r *http.Request) (resource.Consumer, jwtToken, string) {
	rawToken, ok := p.fetchToken(r)
	if !ok {
		return resource.Consumer{}, jwtToken{}, "Missing JWT token in request"
	}

	token, err := parseJWT(rawToken)
	if err != nil {
		return resource.Consumer{}, jwtToken{}, "JWT token invalid"
	}

	userKey, ok := token.payload[p.config.KeyClaimName].(string)
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

	tokenAlgorithm, _ := token.header["alg"].(string)
	if tokenAlgorithm != authConfig.Algorithm {
		return resource.Consumer{}, token, "failed to verify jwt"
	}

	secret, ok := authConfig.secret()
	if !ok {
		return resource.Consumer{}, token, "failed to verify jwt"
	}
	if !verifyHMAC(token, authConfig.Algorithm, secret) {
		return resource.Consumer{}, token, "failed to verify jwt"
	}
	if err := p.verifyClaims(token.payload, authConfig.LifetimeGracePeriod); err != nil {
		return resource.Consumer{}, token, "failed to verify jwt"
	}

	return consumer, token, ""
}

func (p *Plugin) fetchToken(r *http.Request) (string, bool) {
	if token := r.Header.Get(p.config.Header); token != "" {
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

func parseJWT(raw string) (jwtToken, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return jwtToken{}, fmt.Errorf("token must have three parts")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtToken{}, err
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtToken{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtToken{}, err
	}

	var header map[string]any
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return jwtToken{}, err
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return jwtToken{}, err
	}

	return jwtToken{
		header:    header,
		payload:   payload,
		signing:   parts[0] + "." + parts[1],
		signature: signature,
	}, nil
}

func verifyHMAC(token jwtToken, algorithm string, secret []byte) bool {
	hashFunc, ok := hmacHash(algorithm)
	if !ok {
		return false
	}

	mac := hmac.New(hashFunc, secret)
	mac.Write([]byte(token.signing))
	expected := mac.Sum(nil)

	return subtle.ConstantTimeCompare(token.signature, expected) == 1
}

func hmacHash(algorithm string) (func() hash.Hash, bool) {
	switch algorithm {
	case "HS256":
		return sha256.New, true
	case "HS384":
		return sha512.New384, true
	case "HS512":
		return sha512.New, true
	default:
		return nil, false
	}
}

func (p *Plugin) verifyClaims(payload map[string]any, gracePeriod int64) error {
	claims := p.config.ClaimsToVerify
	if len(claims) == 0 {
		claims = []string{"exp", "nbf"}
	}

	for _, claim := range claims {
		value, exists := payload[claim]
		if !exists {
			if len(p.config.ClaimsToVerify) == 0 {
				continue
			}
			return fmt.Errorf("claim %s is missing", claim)
		}

		ts, ok := numberClaim(value)
		if !ok {
			return fmt.Errorf("claim %s is not a number", claim)
		}

		now := p.now().Unix()
		switch claim {
		case "exp":
			if ts <= now-gracePeriod {
				return fmt.Errorf("claim exp expired")
			}
		case "nbf":
			if ts >= now+gracePeriod {
				return fmt.Errorf("claim nbf not valid yet")
			}
		}
	}

	return nil
}

func numberClaim(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
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

func removeCookie(r *http.Request, name string) {
	cookieHeader := r.Header.Get("Cookie")
	if cookieHeader == "" {
		return
	}

	parts := strings.Split(cookieHeader, ";")
	kept := parts[:0]
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if strings.HasPrefix(trimmed, name+"=") {
			continue
		}
		kept = append(kept, trimmed)
	}
	r.Header.Set("Cookie", strings.Join(kept, "; "))
}
