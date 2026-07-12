package openid_connect

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "crypto/sha512"

	"github.com/redis/go-redis/v9"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client              *http.Client
	clientRSAPrivateKey *rsa.PrivateKey
	sessionStore        sessionStore
	httpProxy           *url.URL
	httpsProxy          *url.URL
	noProxy             []string

	mu              sync.Mutex
	discovery       discoveryData
	discoveryLoaded bool
}

const (
	priority = 2599
	name     = "openid-connect"
)

const schema = `
{
  "type": "object",
  "properties": {
    "client_id": {
      "type": "string",
      "minLength": 1
    },
    "client_secret": {
      "type": "string"
    },
    "discovery": {
      "type": "string",
      "minLength": 1
    },
    "scope": {
      "type": "string",
      "default": "openid"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 3
    },
    "introspection_endpoint": {
      "type": "string"
    },
    "introspection_endpoint_auth_method": {
      "type": "string",
      "default": "client_secret_basic"
    },
    "token_endpoint_auth_method": {
      "type": "string",
      "default": "client_secret_basic"
    },
    "client_rsa_private_key": {
      "type": "string"
    },
    "client_rsa_private_key_id": {
      "type": "string"
    },
    "client_jwt_assertion_expires_in": {
      "type": "integer",
      "default": 60
    },
    "bearer_only": {
      "type": "boolean",
      "default": false
    },
    "session": {
      "type": "object",
      "properties": {
        "secret": {"type": "string", "minLength": 16},
        "cookie_name": {"type": "string"},
        "cookie_path": {"type": "string"},
        "cookie_domain": {"type": "string"},
        "cookie_secure": {"type": "boolean"},
        "cookie_http_only": {"type": "boolean"},
        "cookie_same_site": {"type": "string", "enum": ["Strict", "Lax", "None", "Default"]},
        "idling_timeout": {"type": "integer"},
        "rolling_timeout": {"type": "integer"},
        "absolute_timeout": {"type": "integer"},
        "cookie": {"type": "object", "properties": {"lifetime": {"type": "integer"}}},
        "storage": {"type": "string", "enum": ["cookie", "redis"]},
        "redis": {
          "type": "object",
          "properties": {
            "host": {"type": "string", "minLength": 2},
            "port": {"type": "integer", "minimum": 1},
            "username": {"type": "string", "minLength": 1},
            "password": {"type": "string"},
            "database": {"type": "integer", "minimum": 0},
            "prefix": {"type": "string"},
            "ssl": {"type": "boolean"},
            "ssl_verify": {"type": "boolean"},
            "server_name": {"type": "string"},
            "connect_timeout": {"type": "integer", "minimum": 1},
            "send_timeout": {"type": "integer", "minimum": 1},
            "read_timeout": {"type": "integer", "minimum": 1},
            "keepalive_timeout": {"type": "integer", "minimum": 1000}
          }
        }
      }
    },
    "proxy_opts": {
      "type": "object",
      "properties": {
        "http_proxy": {"type": "string"},
        "https_proxy": {"type": "string"},
        "http_proxy_authorization": {"type": "string"},
        "https_proxy_authorization": {"type": "string"},
        "no_proxy": {"type": "string"}
      }
    },
    "realm": {
      "type": "string",
      "default": "apisix"
    },
    "required_scopes": {
      "type": "array",
      "items": {
        "type": "string"
      }
    },
    "logout_path": {
      "type": "string",
      "default": "/logout"
    },
    "redirect_uri": {
      "type": "string"
    },
    "post_logout_redirect_uri": {
      "type": "string"
    },
    "unauth_action": {
      "type": "string",
      "default": "auth",
      "enum": ["auth", "deny", "pass"]
    },
    "public_key": {
      "type": "string"
    },
    "use_jwks": {
      "type": "boolean",
      "default": false
    },
    "token_signing_alg_values_expected": {
      "type": "string"
    },
    "use_pkce": {
      "type": "boolean",
      "default": false
    },
    "authorization_params": {
      "type": "object"
    },
    "force_reauthorize": {
      "type": "boolean",
      "default": false
    },
    "set_access_token_header": {
      "type": "boolean",
      "default": true
    },
    "access_token_in_authorization_header": {
      "type": "boolean",
      "default": false
    },
    "set_id_token_header": {
      "type": "boolean",
      "default": true
    },
    "set_userinfo_header": {
      "type": "boolean",
      "default": true
    },
    "set_refresh_token_header": {
      "type": "boolean",
      "default": false
    },
    "renew_access_token_on_expiry": {
      "type": "boolean",
      "default": true
    },
    "access_token_expires_in": {
      "type": "integer"
    },
    "access_token_expires_leeway": {
      "type": "integer",
      "default": 0
    },
    "refresh_session_interval": {
      "type": "integer"
    },
    "revoke_tokens_on_logout": {
      "type": "boolean",
      "default": false
    },
    "introspection_addon_headers": {
      "type": "array",
      "items": {
        "type": "string",
        "pattern": "^[^:]+$"
      }
    },
    "claim_validator": {
      "type": "object"
    },
    "claim_schema": {
      "type": "object"
    }
  },
  "required": ["client_id", "discovery"]
}
`

type Config struct {
	ClientID                         string         `json:"client_id"`
	ClientSecret                     string         `json:"client_secret,omitempty"`
	Discovery                        string         `json:"discovery"`
	Scope                            string         `json:"scope,omitempty"`
	SSLVerify                        *bool          `json:"ssl_verify,omitempty"`
	Timeout                          int            `json:"timeout,omitempty"`
	IntrospectionEndpoint            string         `json:"introspection_endpoint,omitempty"`
	IntrospectionEndpointAuthMethod  string         `json:"introspection_endpoint_auth_method,omitempty"`
	TokenEndpointAuthMethod          string         `json:"token_endpoint_auth_method,omitempty"`
	ClientRSAPrivateKey              string         `json:"client_rsa_private_key,omitempty"`
	ClientRSAPrivateKeyID            string         `json:"client_rsa_private_key_id,omitempty"`
	ClientJWTAssertionExpiresIn      int            `json:"client_jwt_assertion_expires_in,omitempty"`
	BearerOnly                       bool           `json:"bearer_only,omitempty"`
	Session                          SessionConfig  `json:"session"`
	ProxyOpts                        *ProxyOptions  `json:"proxy_opts,omitempty"`
	Realm                            string         `json:"realm,omitempty"`
	RequiredScopes                   []string       `json:"required_scopes,omitempty"`
	LogoutPath                       string         `json:"logout_path,omitempty"`
	RedirectURI                      string         `json:"redirect_uri,omitempty"`
	PostLogoutRedirectURI            string         `json:"post_logout_redirect_uri,omitempty"`
	UnauthAction                     string         `json:"unauth_action,omitempty"`
	PublicKey                        string         `json:"public_key,omitempty"`
	UseJWKS                          bool           `json:"use_jwks,omitempty"`
	TokenSigningAlgValuesExpected    string         `json:"token_signing_alg_values_expected,omitempty"`
	UsePKCE                          bool           `json:"use_pkce,omitempty"`
	AuthorizationParams              map[string]any `json:"authorization_params,omitempty"`
	ForceReauthorize                 bool           `json:"force_reauthorize,omitempty"`
	SetAccessTokenHeader             *bool          `json:"set_access_token_header,omitempty"`
	AccessTokenInAuthorizationHeader bool           `json:"access_token_in_authorization_header,omitempty"`
	SetIDTokenHeader                 *bool          `json:"set_id_token_header,omitempty"`
	SetUserinfoHeader                *bool          `json:"set_userinfo_header,omitempty"`
	SetRefreshTokenHeader            *bool          `json:"set_refresh_token_header,omitempty"`
	RenewAccessTokenOnExpiry         *bool          `json:"renew_access_token_on_expiry,omitempty"`
	AccessTokenExpiresIn             int            `json:"access_token_expires_in,omitempty"`
	AccessTokenExpiresLeeway         int            `json:"access_token_expires_leeway,omitempty"`
	RefreshSessionInterval           *int           `json:"refresh_session_interval,omitempty"`
	RevokeTokensOnLogout             bool           `json:"revoke_tokens_on_logout,omitempty"`
	IntrospectionAddonHeaders        []string       `json:"introspection_addon_headers,omitempty"`
	ClaimValidator                   map[string]any `json:"claim_validator,omitempty"`
	ClaimSchema                      map[string]any `json:"claim_schema,omitempty"`
}

type ProxyOptions struct {
	HTTPProxy               string `json:"http_proxy,omitempty"`
	HTTPSProxy              string `json:"https_proxy,omitempty"`
	HTTPProxyAuthorization  string `json:"http_proxy_authorization,omitempty"`
	HTTPSProxyAuthorization string `json:"https_proxy_authorization,omitempty"`
	NoProxy                 string `json:"no_proxy,omitempty"`
}

type SessionConfig struct {
	Secret          string               `json:"secret,omitempty"`
	CookieName      string               `json:"cookie_name,omitempty"`
	CookiePath      string               `json:"cookie_path,omitempty"`
	CookieDomain    string               `json:"cookie_domain,omitempty"`
	CookieSecure    bool                 `json:"cookie_secure,omitempty"`
	CookieHTTPOnly  *bool                `json:"cookie_http_only,omitempty"`
	CookieSameSite  string               `json:"cookie_same_site,omitempty"`
	IdlingTimeout   int                  `json:"idling_timeout,omitempty"`
	RollingTimeout  int                  `json:"rolling_timeout,omitempty"`
	AbsoluteTimeout int                  `json:"absolute_timeout,omitempty"`
	Cookie          *SessionCookieConfig `json:"cookie,omitempty"`
	Storage         string               `json:"storage,omitempty"`
	Redis           *SessionRedisConfig  `json:"redis,omitempty"`
}

type SessionCookieConfig struct {
	Lifetime int `json:"lifetime,omitempty"`
}

type SessionRedisConfig struct {
	Host             string `json:"host,omitempty"`
	Port             int    `json:"port,omitempty"`
	Username         string `json:"username,omitempty"`
	Password         string `json:"password,omitempty"`
	Database         int    `json:"database,omitempty"`
	Prefix           string `json:"prefix,omitempty"`
	SSL              bool   `json:"ssl,omitempty"`
	SSLVerify        *bool  `json:"ssl_verify,omitempty"`
	ServerName       string `json:"server_name,omitempty"`
	ConnectTimeout   int    `json:"connect_timeout,omitempty"`
	SendTimeout      int    `json:"send_timeout,omitempty"`
	ReadTimeout      int    `json:"read_timeout,omitempty"`
	KeepaliveTimeout int    `json:"keepalive_timeout,omitempty"`
}

type discoveryData struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
	IntrospectionEndpoint string `json:"introspection_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

type sessionData struct {
	RedisID           string `json:"-"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
	LastAuthenticated int64  `json:"last_authenticated,omitempty"`
	FlowState         string `json:"flow_state,omitempty"`
	FlowExpiresAt     int64  `json:"flow_expires_at,omitempty"`
	OriginalURI       string `json:"original_uri,omitempty"`
	CodeVerifier      string `json:"code_verifier,omitempty"`
	AccessToken       string `json:"access_token,omitempty"`
	IDToken           string `json:"id_token,omitempty"`
	RefreshToken      string `json:"refresh_token,omitempty"`
	Userinfo          string `json:"userinfo,omitempty"`
	ExpiresAt         int64  `json:"expires_at,omitempty"`
}

var errSessionNotFound = errors.New("openid-connect session not found")

type sessionStore interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string, time.Duration) error
	Delete(context.Context, string) error
}

type redisSessionStore struct {
	client *redis.Client
}

func (s *redisSessionStore) Get(ctx context.Context, key string) (string, error) {
	value, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", errSessionNotFound
	}
	return value, err
}

func (s *redisSessionStore) Set(ctx context.Context, key string, value string, ttl time.Duration) error {
	return s.client.Set(ctx, key, value, ttl).Err()
}

func (s *redisSessionStore) Delete(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
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
	if p.config.Scope == "" {
		p.config.Scope = "openid"
	}
	if p.config.SSLVerify == nil {
		b := true
		p.config.SSLVerify = &b
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3
	}
	if p.config.IntrospectionEndpointAuthMethod == "" {
		p.config.IntrospectionEndpointAuthMethod = "client_secret_basic"
	}
	if p.config.TokenEndpointAuthMethod == "" {
		p.config.TokenEndpointAuthMethod = "client_secret_basic"
	}
	if !validClientAuthMethod(p.config.TokenEndpointAuthMethod) {
		return fmt.Errorf("unsupported token_endpoint_auth_method %q", p.config.TokenEndpointAuthMethod)
	}
	if !validClientAuthMethod(p.config.IntrospectionEndpointAuthMethod) {
		return fmt.Errorf("unsupported introspection_endpoint_auth_method %q", p.config.IntrospectionEndpointAuthMethod)
	}
	if p.config.ClientJWTAssertionExpiresIn == 0 {
		p.config.ClientJWTAssertionExpiresIn = 60
	}
	if p.config.TokenEndpointAuthMethod == "private_key_jwt" ||
		p.config.IntrospectionEndpointAuthMethod == "private_key_jwt" {
		privateKey, err := parseRSAPrivateKey([]byte(p.config.ClientRSAPrivateKey))
		if err != nil {
			return fmt.Errorf("invalid client_rsa_private_key: %w", err)
		}
		p.clientRSAPrivateKey = privateKey
	}
	if (p.config.TokenEndpointAuthMethod == "client_secret_jwt" ||
		p.config.IntrospectionEndpointAuthMethod == "client_secret_jwt") && p.config.ClientSecret == "" {
		return errors.New("client_secret is required for client_secret_jwt")
	}
	if p.config.Realm == "" {
		p.config.Realm = "apisix"
	}
	if p.config.LogoutPath == "" {
		p.config.LogoutPath = "/logout"
	}
	if p.config.UnauthAction == "" {
		p.config.UnauthAction = "auth"
	}
	if !p.config.BearerOnly {
		if len(p.config.Session.Secret) < 16 {
			return errors.New("openid-connect session.secret must be at least 16 characters for code flow")
		}
		if p.config.Session.Storage == "" {
			p.config.Session.Storage = "cookie"
		}
		if p.config.Session.Storage != "cookie" && p.config.Session.Storage != "redis" {
			return fmt.Errorf("openid-connect session storage %q is not supported", p.config.Session.Storage)
		}
		if p.config.Session.Storage == "redis" {
			if err := p.configureRedisSessionStore(); err != nil {
				return err
			}
		}
		if p.config.Session.CookieName == "" {
			p.config.Session.CookieName = "session"
		}
		if p.config.Session.CookiePath == "" {
			p.config.Session.CookiePath = "/"
		}
		if p.config.Session.CookieHTTPOnly == nil {
			b := true
			p.config.Session.CookieHTTPOnly = &b
		}
		if p.config.Session.CookieSameSite == "" {
			p.config.Session.CookieSameSite = "Default"
		}
		if p.config.Session.Cookie != nil && p.config.Session.Cookie.Lifetime > 0 &&
			p.config.Session.AbsoluteTimeout == 0 {
			p.config.Session.AbsoluteTimeout = p.config.Session.Cookie.Lifetime
		}
		if !validSameSite(p.config.Session.CookieSameSite) {
			return fmt.Errorf("openid-connect session cookie_same_site %q is invalid", p.config.Session.CookieSameSite)
		}
	}
	if p.config.SetAccessTokenHeader == nil {
		b := true
		p.config.SetAccessTokenHeader = &b
	}
	if p.config.SetIDTokenHeader == nil {
		b := true
		p.config.SetIDTokenHeader = &b
	}
	if p.config.SetUserinfoHeader == nil {
		b := true
		p.config.SetUserinfoHeader = &b
	}
	if p.config.SetRefreshTokenHeader == nil {
		b := false
		p.config.SetRefreshTokenHeader = &b
	}
	if p.config.RenewAccessTokenOnExpiry == nil {
		b := true
		p.config.RenewAccessTokenOnExpiry = &b
	}
	if err := p.configureProxy(); err != nil {
		return err
	}

	p.client = &http.Client{
		Timeout:   time.Duration(p.config.Timeout) * time.Second,
		Transport: p.transport(),
	}

	return nil
}

func (p *Plugin) configureRedisSessionStore() error {
	if p.config.Session.Redis == nil {
		return errors.New("openid-connect session.redis is required when session.storage is redis")
	}

	redisConfig := p.config.Session.Redis
	if redisConfig.Host == "" {
		redisConfig.Host = "127.0.0.1"
	}
	if redisConfig.Port == 0 {
		redisConfig.Port = 6379
	}
	if redisConfig.Prefix == "" {
		redisConfig.Prefix = "sessions"
	}
	if redisConfig.SSLVerify == nil {
		verify := true
		redisConfig.SSLVerify = &verify
	}
	if redisConfig.ConnectTimeout == 0 {
		redisConfig.ConnectTimeout = 1000
	}
	if redisConfig.SendTimeout == 0 {
		redisConfig.SendTimeout = 1000
	}
	if redisConfig.ReadTimeout == 0 {
		redisConfig.ReadTimeout = 1000
	}
	if redisConfig.KeepaliveTimeout == 0 {
		redisConfig.KeepaliveTimeout = 10000
	}

	configUID := shared.NewConfigUID()
	configUID.Add(redisConfig.Host)
	configUID.Add(redisConfig.Port)
	configUID.Add(redisConfig.Username)
	configUID.Add(redisConfig.Password)
	configUID.Add(redisConfig.Database)
	configUID.Add(redisConfig.SSL)
	configUID.Add(*redisConfig.SSLVerify)
	configUID.Add(redisConfig.ServerName)
	configUID.Add(redisConfig.ConnectTimeout)
	configUID.Add(redisConfig.SendTimeout)
	configUID.Add(redisConfig.ReadTimeout)
	configUID.Add(redisConfig.KeepaliveTimeout)

	options := &redis.Options{
		Addr:            net.JoinHostPort(redisConfig.Host, strconv.Itoa(redisConfig.Port)),
		Username:        redisConfig.Username,
		Password:        redisConfig.Password,
		DB:              redisConfig.Database,
		DialTimeout:     time.Duration(redisConfig.ConnectTimeout) * time.Millisecond,
		WriteTimeout:    time.Duration(redisConfig.SendTimeout) * time.Millisecond,
		ReadTimeout:     time.Duration(redisConfig.ReadTimeout) * time.Millisecond,
		ConnMaxIdleTime: time.Duration(redisConfig.KeepaliveTimeout) * time.Millisecond,
	}
	if redisConfig.SSL {
		options.TLSConfig = &tls.Config{
			ServerName:         redisConfig.ServerName,
			InsecureSkipVerify: !*redisConfig.SSLVerify,
		}
	}
	client := redis.NewClient(options)
	p.sessionStore = &redisSessionStore{
		client: shared.LoadOrStoreClient(name+"-session", configUID, client).(*redis.Client),
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		clientXAccessToken := r.Header.Get("X-Access-Token")
		clearOutputHeaders(r)
		if !p.config.BearerOnly && r.URL.Path == p.config.LogoutPath {
			p.handleLogout(w, r)
			return
		}

		hasToken, token, statusCode, errMsg := p.bearerToken(r, clientXAccessToken)
		if errMsg != "" {
			http.Error(w, errMsg, statusCode)
			return
		}
		if !hasToken {
			if p.config.BearerOnly {
				p.writeBearerUnauthorized(w, "No bearer token found in request.")
				return
			}
			if p.config.UnauthAction == "pass" {
				next.ServeHTTP(w, r)
				return
			}
			p.handleCodeFlow(w, r, next)
			return
		}

		var claims map[string]any
		var err error
		if p.usesLocalJWTVerification() {
			claims, err = p.verifyBearerJWT(r, token)
			if err != nil {
				p.writeInvalidToken(w, err.Error())
				return
			}
		} else {
			claims, err = p.introspect(r, token)
			if err != nil {
				p.writeInvalidToken(w, err.Error())
				return
			}
		}
		if !tokenActive(claims) {
			p.writeInvalidToken(w, "inactive token")
			return
		}
		if !requiredScopesPresent(p.config.RequiredScopes, claims) {
			w.WriteHeader(http.StatusForbidden)
			w.Write(
				util.StringToBytes(
					`{"error":"required scopes ` + strings.Join(p.config.RequiredScopes, ", ") + ` not present"}`,
				),
			)
			return
		}
		if statusCode, responseBody := p.validateConfiguredClaims(claims); statusCode != 0 {
			w.WriteHeader(statusCode)
			w.Write(util.StringToBytes(responseBody))
			return
		}
		if err := p.validateClaimSchema(claims); err != nil {
			p.writeInvalidToken(w, err.Error())
			return
		}

		p.setAccessTokenHeader(r, token)
		if *p.config.SetUserinfoHeader {
			body, err := json.Marshal(claims)
			if err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			r.Header.Set("X-Userinfo", base64.StdEncoding.EncodeToString(body))
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) handleCodeFlow(w http.ResponseWriter, r *http.Request, next http.Handler) {
	redirectURI := p.redirectURI(r)
	if p.isRedirectCallback(r, redirectURI) {
		p.handleCodeCallback(w, r, redirectURI)
		return
	}
	if p.config.ForceReauthorize {
		p.beginAuthorization(w, r, redirectURI, nil, "")
		return
	}

	session, err := p.readSession(r)
	now := time.Now()
	if err == nil && session != nil && p.sessionValid(*session, now) {
		if p.refreshSessionDue(*session, now) {
			p.beginAuthorization(w, r, redirectURI, session, "none")
			return
		}
		session.UpdatedAt = now.Unix()
		if p.config.Session.RollingTimeout > 0 || p.config.Session.IdlingTimeout > 0 {
			if err := p.writeSession(w, *session); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
		}
		p.setSessionHeaders(r, *session)
		next.ServeHTTP(w, r)
		return
	}
	if err == nil && session != nil && *p.config.RenewAccessTokenOnExpiry && p.sessionRefreshable(*session, now) {
		tokens, err := p.refreshAccessToken(r, session.RefreshToken)
		if err == nil {
			session.UpdatedAt = now.Unix()
			session.AccessToken = tokens.AccessToken
			if tokens.IDToken != "" {
				session.IDToken = tokens.IDToken
			}
			if tokens.RefreshToken != "" {
				session.RefreshToken = tokens.RefreshToken
			}
			session.ExpiresAt = p.tokenExpiresAt(now, tokens.ExpiresIn)
			if err := p.writeSession(w, *session); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			p.setSessionHeaders(r, *session)
			next.ServeHTTP(w, r)
			return
		}
	}

	p.beginAuthorization(w, r, redirectURI, nil, "")
}

func (p *Plugin) beginAuthorization(
	w http.ResponseWriter,
	r *http.Request,
	redirectURI string,
	previous *sessionData,
	prompt string,
) {
	discovery, err := p.discoveryDoc()
	if err != nil || discovery.AuthorizationEndpoint == "" {
		http.Error(w, "openid discovery document has no authorization_endpoint", http.StatusBadGateway)
		return
	}

	state, err := randomURLValue(32)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	session := sessionData{
		CreatedAt:     now.Unix(),
		UpdatedAt:     now.Unix(),
		FlowState:     state,
		FlowExpiresAt: p.flowExpiry(now).Unix(),
		OriginalURI:   r.URL.RequestURI(),
	}
	if previous != nil {
		session.CreatedAt = previous.CreatedAt
		session.RedisID = previous.RedisID
	}

	parameters := url.Values{}
	parameters.Set("client_id", p.config.ClientID)
	parameters.Set("scope", p.config.Scope)
	parameters.Set("response_type", "code")
	parameters.Set("redirect_uri", redirectURI)
	parameters.Set("state", state)
	for key, value := range p.config.AuthorizationParams {
		if value != nil {
			parameters.Set(key, fmt.Sprint(value))
		}
	}
	if prompt != "" {
		parameters.Set("prompt", prompt)
	}
	if p.config.UsePKCE {
		verifier, err := randomURLValue(32)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		session.CodeVerifier = verifier
		challenge := sha256.Sum256([]byte(verifier))
		parameters.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
		parameters.Set("code_challenge_method", "S256")
	}
	if err := p.writeSession(w, session); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	authorizationURL, err := url.Parse(discovery.AuthorizationEndpoint)
	if err != nil {
		http.Error(w, "invalid authorization endpoint", http.StatusBadGateway)
		return
	}
	query := authorizationURL.Query()
	maps.Copy(query, parameters)
	authorizationURL.RawQuery = query.Encode()
	http.Redirect(w, r, authorizationURL.String(), http.StatusFound)
}

func (p *Plugin) handleCodeCallback(w http.ResponseWriter, r *http.Request, redirectURI string) {
	session, err := p.readSession(r)
	state := r.URL.Query().Get("state")
	if err != nil || session == nil || state == "" || session.FlowState == "" ||
		session.FlowExpiresAt <= time.Now().Unix() ||
		subtle.ConstantTimeCompare([]byte(state), []byte(session.FlowState)) != 1 {
		http.Error(w, "invalid authorization state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	tokens, err := p.exchangeCode(r, code, redirectURI, session.CodeVerifier)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	now := time.Now()
	newSession := sessionData{
		RedisID:           session.RedisID,
		CreatedAt:         session.CreatedAt,
		UpdatedAt:         now.Unix(),
		LastAuthenticated: now.Unix(),
		AccessToken:       tokens.AccessToken,
		IDToken:           tokens.IDToken,
		RefreshToken:      tokens.RefreshToken,
	}
	newSession.ExpiresAt = p.tokenExpiresAt(now, tokens.ExpiresIn)
	if userinfo, err := p.userinfo(r, tokens.AccessToken); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	} else {
		newSession.Userinfo = userinfo
	}
	if err := p.validateSessionClaimSchema(tokens, newSession.Userinfo); err != nil {
		p.writeInvalidToken(w, err.Error())
		return
	}
	if err := p.writeSession(w, newSession); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	originalURI := session.OriginalURI
	if originalURI == "" || !strings.HasPrefix(originalURI, "/") {
		originalURI = "/"
	}
	http.Redirect(w, r, originalURI, http.StatusFound)
}

func (p *Plugin) exchangeCode(r *http.Request, code, redirectURI, verifier string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	if p.config.UsePKCE {
		if verifier == "" {
			return tokenResponse{}, errors.New("missing PKCE verifier")
		}
		form.Set("code_verifier", verifier)
	}
	return p.requestTokens(r, form)
}

func (p *Plugin) refreshAccessToken(r *http.Request, refreshToken string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("scope", p.config.Scope)
	return p.requestTokens(r, form)
}

func (p *Plugin) requestTokens(r *http.Request, form url.Values) (tokenResponse, error) {
	discovery, err := p.discoveryDoc()
	if err != nil {
		return tokenResponse{}, err
	}
	if discovery.TokenEndpoint == "" {
		return tokenResponse{}, errors.New("openid discovery document has no token_endpoint")
	}
	resp, err := p.postTokenForm(r, discovery.TokenEndpoint, form)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return tokenResponse{}, fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}

	var tokens tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return tokenResponse{}, fmt.Errorf("invalid token response: %w", err)
	}
	if tokens.AccessToken == "" {
		return tokenResponse{}, errors.New("token response has no access_token")
	}
	return tokens, nil
}

func (p *Plugin) postTokenForm(r *http.Request, endpoint string, form url.Values) (*http.Response, error) {
	req, err := p.authenticatedFormRequest(r, endpoint, form, p.config.TokenEndpointAuthMethod)
	if err != nil {
		return nil, err
	}
	return p.client.Do(req)
}

func (p *Plugin) authenticatedFormRequest(
	r *http.Request,
	endpoint string,
	form url.Values,
	authMethod string,
) (*http.Request, error) {
	switch authMethod {
	case "client_secret_post":
		form.Set("client_id", p.config.ClientID)
		form.Set("client_secret", p.config.ClientSecret)
	case "private_key_jwt", "client_secret_jwt":
		assertion, err := p.clientAssertion(endpoint, authMethod)
		if err != nil {
			return nil, err
		}
		form.Set("client_id", p.config.ClientID)
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", assertion)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authMethod == "client_secret_basic" {
		req.SetBasicAuth(p.config.ClientID, p.config.ClientSecret)
	}
	return req, nil
}

func (p *Plugin) clientAssertion(audience, authMethod string) (string, error) {
	jti, err := randomURLValue(16)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	header := map[string]any{"typ": "JWT"}
	claims := map[string]any{
		"iss": p.config.ClientID,
		"sub": p.config.ClientID,
		"aud": audience,
		"jti": jti,
		"iat": now,
		"exp": now + int64(p.config.ClientJWTAssertionExpiresIn),
	}
	if authMethod == "private_key_jwt" {
		header["alg"] = "RS256"
		if p.config.ClientRSAPrivateKeyID != "" {
			header["kid"] = p.config.ClientRSAPrivateKeyID
		}
	} else {
		header["alg"] = "HS256"
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	var signature []byte
	if authMethod == "private_key_jwt" {
		if p.clientRSAPrivateKey == nil {
			return "", errors.New("client_rsa_private_key is required for private_key_jwt")
		}
		digest := sha256.Sum256([]byte(unsigned))
		signature, err = rsa.SignPKCS1v15(rand.Reader, p.clientRSAPrivateKey, crypto.SHA256, digest[:])
		if err != nil {
			return "", err
		}
	} else {
		mac := hmac.New(sha256.New, []byte(p.config.ClientSecret))
		_, _ = mac.Write([]byte(unsigned))
		signature = mac.Sum(nil)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func validClientAuthMethod(method string) bool {
	return method == "client_secret_basic" || method == "client_secret_post" ||
		method == "private_key_jwt" || method == "client_secret_jwt"
}

func parseRSAPrivateKey(privateKeyBytes []byte) (*rsa.PrivateKey, error) {
	if block, _ := pem.Decode(privateKeyBytes); block != nil {
		privateKeyBytes = block.Bytes
	}
	if privateKey, err := x509.ParsePKCS1PrivateKey(privateKeyBytes); err == nil {
		return privateKey, nil
	}
	privateKey, err := x509.ParsePKCS8PrivateKey(privateKeyBytes)
	if err != nil {
		return nil, err
	}
	rsaPrivateKey, ok := privateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rsaPrivateKey, nil
}

func (p *Plugin) userinfo(r *http.Request, accessToken string) (string, error) {
	discovery, err := p.discoveryDoc()
	if err != nil || discovery.UserinfoEndpoint == "" {
		return "", err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, discovery.UserinfoEndpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("userinfo endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return "", errors.New("invalid userinfo response")
	}
	return string(body), nil
}

func (p *Plugin) handleLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := p.readSession(r)
	p.clearSession(w, session)
	if p.config.RevokeTokensOnLogout && session != nil {
		p.revokeTokens(r, *session)
	}
	discovery, err := p.discoveryDoc()
	if err == nil && discovery.EndSessionEndpoint != "" {
		logoutURL, parseErr := url.Parse(discovery.EndSessionEndpoint)
		if parseErr != nil {
			http.Error(w, "invalid end session endpoint", http.StatusBadGateway)
			return
		}
		if p.config.PostLogoutRedirectURI != "" {
			query := logoutURL.Query()
			query.Set("post_logout_redirect_uri", p.config.PostLogoutRedirectURI)
			logoutURL.RawQuery = query.Encode()
		}
		http.Redirect(w, r, logoutURL.String(), http.StatusFound)
		return
	}
	if p.config.PostLogoutRedirectURI != "" {
		http.Redirect(w, r, p.config.PostLogoutRedirectURI, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (p *Plugin) revokeTokens(r *http.Request, session sessionData) {
	discovery, err := p.discoveryDoc()
	if err != nil || discovery.RevocationEndpoint == "" {
		return
	}
	if session.RefreshToken != "" {
		_ = p.revokeToken(r, discovery.RevocationEndpoint, "refresh_token", session.RefreshToken)
	}
	if session.AccessToken != "" {
		_ = p.revokeToken(r, discovery.RevocationEndpoint, "access_token", session.AccessToken)
	}
}

func (p *Plugin) revokeToken(r *http.Request, endpoint, tokenTypeHint, token string) error {
	form := url.Values{}
	form.Set("token", token)
	form.Set("token_type_hint", tokenTypeHint)
	resp, err := p.postTokenForm(r, endpoint, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("revocation endpoint returned %d", resp.StatusCode)
	}
	return nil
}

func (p *Plugin) redirectURI(r *http.Request) string {
	if p.config.RedirectURI != "" {
		return p.config.RedirectURI
	}
	const suffix = "/.apisix/redirect"
	path := r.URL.Path
	if !strings.HasSuffix(path, suffix) {
		path = strings.TrimSuffix(path, "/") + suffix
	}
	scheme := r.URL.Scheme
	if scheme == "" {
		scheme = r.Header.Get("X-Forwarded-Proto")
	}
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host + path
}

func (p *Plugin) isRedirectCallback(r *http.Request, redirectURI string) bool {
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return false
	}
	return parsed.Path == r.URL.Path && parsed.Path != ""
}

func (p *Plugin) flowExpiry(now time.Time) time.Time {
	expiresAt := now.Add(10 * time.Minute)
	if p.config.Session.AbsoluteTimeout > 0 {
		absoluteExpiry := now.Add(time.Duration(p.config.Session.AbsoluteTimeout) * time.Second)
		if absoluteExpiry.Before(expiresAt) {
			expiresAt = absoluteExpiry
		}
	}
	return expiresAt
}

func (p *Plugin) readSession(r *http.Request) (*sessionData, error) {
	cookie, err := r.Cookie(p.config.Session.CookieName)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			return nil, nil
		}
		return nil, err
	}
	payload, err := p.openSession(cookie.Value)
	if err != nil {
		return nil, err
	}
	if p.config.Session.Storage == "redis" {
		if p.sessionStore == nil {
			return nil, errors.New("openid-connect Redis session store is not configured")
		}
		redisID := string(payload)
		if redisID == "" {
			return nil, errSessionNotFound
		}
		payloadValue, err := p.sessionStore.Get(r.Context(), p.redisSessionKey(redisID))
		if err != nil {
			return nil, err
		}
		payload, err = p.openSession(payloadValue)
		if err != nil {
			return nil, err
		}
		var session sessionData
		if err := json.Unmarshal(payload, &session); err != nil {
			return nil, err
		}
		session.RedisID = redisID
		return &session, nil
	}
	var session sessionData
	if err := json.Unmarshal(payload, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func (p *Plugin) writeSession(w http.ResponseWriter, session sessionData) error {
	payload, err := json.Marshal(session)
	if err != nil {
		return err
	}
	value, err := p.sealSession(payload)
	if err != nil {
		return err
	}
	if p.config.Session.Storage == "redis" {
		if p.sessionStore == nil {
			return errors.New("openid-connect Redis session store is not configured")
		}
		redisID := session.RedisID
		if redisID == "" {
			redisID, err = randomURLValue(32)
			if err != nil {
				return err
			}
		}
		if err := p.sessionStore.Set(
			context.Background(),
			p.redisSessionKey(redisID),
			value,
			p.sessionStorageTTL(session),
		); err != nil {
			return err
		}
		value, err = p.sealSession([]byte(redisID))
		if err != nil {
			return err
		}
	}
	cookie := &http.Cookie{
		Name:     p.config.Session.CookieName,
		Value:    value,
		Path:     p.config.Session.CookiePath,
		Domain:   p.config.Session.CookieDomain,
		Secure:   p.config.Session.CookieSecure,
		HttpOnly: *p.config.Session.CookieHTTPOnly,
		SameSite: sessionSameSite(p.config.Session.CookieSameSite),
	}
	if p.config.Session.AbsoluteTimeout > 0 {
		expiresAt := time.Unix(session.CreatedAt, 0).Add(time.Duration(p.config.Session.AbsoluteTimeout) * time.Second)
		if p.config.Session.RollingTimeout > 0 {
			rollingExpiry := time.Now().Add(time.Duration(p.config.Session.RollingTimeout) * time.Second)
			if rollingExpiry.Before(expiresAt) {
				expiresAt = rollingExpiry
			}
		}
		cookie.Expires = expiresAt
	}
	http.SetCookie(w, cookie)
	return nil
}

func (p *Plugin) clearSession(w http.ResponseWriter, session *sessionData) {
	if p.config.Session.Storage == "redis" && session != nil && session.RedisID != "" && p.sessionStore != nil {
		_ = p.sessionStore.Delete(context.Background(), p.redisSessionKey(session.RedisID))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     p.config.Session.CookieName,
		Value:    "",
		Path:     p.config.Session.CookiePath,
		Domain:   p.config.Session.CookieDomain,
		Secure:   p.config.Session.CookieSecure,
		HttpOnly: *p.config.Session.CookieHTTPOnly,
		SameSite: sessionSameSite(p.config.Session.CookieSameSite),
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

func (p *Plugin) redisSessionKey(redisID string) string {
	return p.config.Session.Redis.Prefix + ":" + redisID
}

func (p *Plugin) sessionStorageTTL(session sessionData) time.Duration {
	now := time.Now()
	var expiresAt time.Time
	if p.config.Session.AbsoluteTimeout > 0 {
		expiresAt = time.Unix(session.CreatedAt, 0).Add(time.Duration(p.config.Session.AbsoluteTimeout) * time.Second)
	}
	if p.config.Session.RollingTimeout > 0 {
		rollingExpiry := now.Add(time.Duration(p.config.Session.RollingTimeout) * time.Second)
		if expiresAt.IsZero() || rollingExpiry.Before(expiresAt) {
			expiresAt = rollingExpiry
		}
	}
	if p.config.Session.IdlingTimeout > 0 {
		idlingExpiry := now.Add(time.Duration(p.config.Session.IdlingTimeout) * time.Second)
		if expiresAt.IsZero() || idlingExpiry.Before(expiresAt) {
			expiresAt = idlingExpiry
		}
	}
	if expiresAt.IsZero() {
		return 0
	}
	if ttl := time.Until(expiresAt); ttl > 0 {
		return ttl
	}
	return time.Second
}

func (p *Plugin) sessionValid(session sessionData, now time.Time) bool {
	if session.AccessToken == "" {
		return false
	}
	if session.ExpiresAt > 0 && session.ExpiresAt <= now.Unix()+int64(p.config.AccessTokenExpiresLeeway) {
		return false
	}
	if p.config.Session.AbsoluteTimeout > 0 &&
		session.CreatedAt+int64(p.config.Session.AbsoluteTimeout) <= now.Unix() {
		return false
	}
	if p.config.Session.IdlingTimeout > 0 &&
		session.UpdatedAt+int64(p.config.Session.IdlingTimeout) <= now.Unix() {
		return false
	}
	return true
}

func (p *Plugin) sessionRefreshable(session sessionData, now time.Time) bool {
	if session.RefreshToken == "" || session.ExpiresAt == 0 {
		return false
	}
	if session.ExpiresAt > now.Unix()+int64(p.config.AccessTokenExpiresLeeway) {
		return false
	}
	if p.config.Session.AbsoluteTimeout > 0 &&
		session.CreatedAt+int64(p.config.Session.AbsoluteTimeout) <= now.Unix() {
		return false
	}
	if p.config.Session.IdlingTimeout > 0 &&
		session.UpdatedAt+int64(p.config.Session.IdlingTimeout) <= now.Unix() {
		return false
	}
	return true
}

func (p *Plugin) refreshSessionDue(session sessionData, now time.Time) bool {
	return p.config.RefreshSessionInterval != nil &&
		(session.LastAuthenticated == 0 || session.LastAuthenticated+int64(*p.config.RefreshSessionInterval) < now.Unix())
}

func (p *Plugin) tokenExpiresAt(now time.Time, expiresIn int64) int64 {
	if expiresIn <= 0 {
		expiresIn = int64(p.config.AccessTokenExpiresIn)
	}
	if expiresIn <= 0 {
		return 0
	}
	return now.Add(time.Duration(expiresIn) * time.Second).Unix()
}

func (p *Plugin) setSessionHeaders(r *http.Request, session sessionData) {
	p.setAccessTokenHeader(r, session.AccessToken)
	if *p.config.SetIDTokenHeader && session.IDToken != "" {
		r.Header.Set("X-ID-Token", session.IDToken)
	}
	if *p.config.SetRefreshTokenHeader && session.RefreshToken != "" {
		r.Header.Set("X-Refresh-Token", session.RefreshToken)
	}
	if *p.config.SetUserinfoHeader && session.Userinfo != "" {
		r.Header.Set("X-Userinfo", base64.StdEncoding.EncodeToString([]byte(session.Userinfo)))
	}
}

func (p *Plugin) sealSession(payload []byte) (string, error) {
	block, err := aes.NewCipher(p.sessionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, payload, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (p *Plugin) openSession(encoded string) ([]byte, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(p.sessionKey())
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("invalid session cookie")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}

func (p *Plugin) sessionKey() []byte {
	sum := sha256.Sum256([]byte(p.config.Session.Secret))
	return sum[:]
}

func randomURLValue(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func validSameSite(value string) bool {
	return value == "Strict" || value == "Lax" || value == "None" || value == "Default"
}

func sessionSameSite(value string) http.SameSite {
	switch value {
	case "Strict":
		return http.SameSiteStrictMode
	case "Lax":
		return http.SameSiteLaxMode
	case "None":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteDefaultMode
	}
}

func (p *Plugin) validateConfiguredClaims(claims map[string]any) (int, string) {
	audience, ok := p.audienceClaimValidator()
	if !ok {
		return 0, ""
	}

	value := claims[audience.claim]
	if audience.required && value == nil {
		return http.StatusForbidden, `{"error":"required audience claim not present"}`
	}
	if audience.matchWithClientID && value != nil && !audienceMatchesClientID(value, p.config.ClientID) {
		return http.StatusForbidden, `{"error":"mismatched audience"}`
	}
	return 0, ""
}

type audienceClaimValidator struct {
	claim             string
	required          bool
	matchWithClientID bool
}

func (p *Plugin) audienceClaimValidator() (audienceClaimValidator, bool) {
	raw, ok := p.config.ClaimValidator["audience"].(map[string]any)
	if !ok {
		return audienceClaimValidator{}, false
	}

	validator := audienceClaimValidator{claim: "aud"}
	if claim, ok := raw["claim"].(string); ok && claim != "" {
		validator.claim = claim
	}
	validator.required, _ = raw["required"].(bool)
	validator.matchWithClientID, _ = raw["match_with_client_id"].(bool)
	return validator, true
}

func audienceMatchesClientID(value any, clientID string) bool {
	switch typed := value.(type) {
	case string:
		return typed == clientID
	case []any:
		for _, item := range typed {
			if item == clientID {
				return true
			}
		}
	case []string:
		if slices.Contains(typed, clientID) {
			return true
		}
	}
	return false
}

func (p *Plugin) validateClaimSchema(claims map[string]any) error {
	return p.validateSchema(claims)
}

func (p *Plugin) validateSessionClaimSchema(tokens tokenResponse, userinfo string) error {
	if len(p.config.ClaimSchema) == 0 {
		return nil
	}
	var user any
	if userinfo != "" {
		if err := json.Unmarshal([]byte(userinfo), &user); err != nil {
			return fmt.Errorf("invalid userinfo response")
		}
	}
	var idToken any = tokens.IDToken
	if tokens.IDToken != "" {
		if token, err := parseJWT(tokens.IDToken); err == nil {
			idToken = token.payload
		}
	}
	return p.validateSchema(map[string]any{
		"user":         user,
		"access_token": tokens.AccessToken,
		"id_token":     idToken,
	})
}

func (p *Plugin) validateSchema(value any) error {
	if len(p.config.ClaimSchema) == 0 {
		return nil
	}

	encoded, err := json.Marshal(p.config.ClaimSchema)
	if err != nil {
		return fmt.Errorf("failed to encode claim schema")
	}
	if err := util.Validate(value, string(encoded)); err != nil {
		return err
	}
	return nil
}

func (p *Plugin) bearerToken(r *http.Request, clientXAccessToken string) (bool, string, int, string) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		if clientXAccessToken == "" {
			return false, "", 0, ""
		}
		return true, clientXAccessToken, 0, ""
	}

	parts := strings.SplitN(auth, " ", 2)
	if len(parts) < 2 {
		return false, "", http.StatusBadRequest, "Invalid Authorization header format."
	}
	if strings.EqualFold(parts[0], "Bearer") {
		return true, parts[1], 0, ""
	}
	return false, "", 0, ""
}

func (p *Plugin) usesLocalJWTVerification() bool {
	return p.config.PublicKey != "" || p.config.UseJWKS
}

func (p *Plugin) verifyBearerJWT(r *http.Request, rawToken string) (map[string]any, error) {
	token, err := parseJWT(rawToken)
	if err != nil {
		return nil, fmt.Errorf("JWT token invalid")
	}

	algorithm, _ := token.header["alg"].(string)
	if algorithm == "" {
		return nil, fmt.Errorf("JWT token missing alg")
	}
	if p.config.TokenSigningAlgValuesExpected != "" && algorithm != p.config.TokenSigningAlgValuesExpected {
		return nil, fmt.Errorf("JWT token alg mismatch")
	}

	var publicKey any
	if p.config.PublicKey != "" {
		publicKey, err = parsePublicKey([]byte(p.config.PublicKey))
		if err != nil {
			return nil, fmt.Errorf("failed to parse public key")
		}
	} else {
		publicKey, err = p.jwksPublicKey(r, token)
		if err != nil {
			return nil, err
		}
	}
	if !verifyJWTSignature(token, algorithm, publicKey) {
		return nil, fmt.Errorf("failed to verify jwt")
	}
	if err := verifyJWTTimeClaims(token.payload, time.Now()); err != nil {
		return nil, err
	}
	p.validateIssuer(token.payload)

	return token.payload, nil
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

func parsePublicKey(publicKeyBytes []byte) (any, error) {
	if block, _ := pem.Decode(publicKeyBytes); block != nil {
		publicKeyBytes = block.Bytes
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(publicKeyBytes)
			if err != nil {
				return nil, err
			}
			return cert.PublicKey, nil
		}
	}

	if publicKey, err := x509.ParsePKIXPublicKey(publicKeyBytes); err == nil {
		return publicKey, nil
	}
	if publicKey, err := x509.ParsePKCS1PublicKey(publicKeyBytes); err == nil {
		return publicKey, nil
	}
	return nil, fmt.Errorf("unsupported public key")
}

func verifyJWTSignature(token jwtToken, algorithm string, publicKey any) bool {
	rsaKey, ok := publicKey.(*rsa.PublicKey)
	if !ok {
		return false
	}

	hashAlg, digest, ok := jwtSigningDigest(algorithm, token.signing)
	if !ok {
		return false
	}

	switch algorithm {
	case "RS256", "RS384", "RS512":
		return rsa.VerifyPKCS1v15(rsaKey, hashAlg, digest, token.signature) == nil
	case "PS256", "PS384", "PS512":
		return rsa.VerifyPSS(rsaKey, hashAlg, digest, token.signature, nil) == nil
	default:
		return false
	}
}

func jwtSigningDigest(algorithm, signing string) (crypto.Hash, []byte, bool) {
	var hashAlg crypto.Hash
	switch algorithm {
	case "RS256", "PS256":
		hashAlg = crypto.SHA256
	case "RS384", "PS384":
		hashAlg = crypto.SHA384
	case "RS512", "PS512":
		hashAlg = crypto.SHA512
	default:
		return 0, nil, false
	}

	hashFunc := hashAlg.New()
	hashFunc.Write([]byte(signing))
	return hashAlg, hashFunc.Sum(nil), true
}

func verifyJWTTimeClaims(payload map[string]any, now time.Time) error {
	if exp, ok := numberClaim(payload["exp"]); ok && exp <= now.Unix() {
		return fmt.Errorf("JWT token expired")
	}
	if nbf, ok := numberClaim(payload["nbf"]); ok && nbf > now.Unix() {
		return fmt.Errorf("JWT token not valid yet")
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

func (p *Plugin) validateIssuer(payload map[string]any) {
	issuer, _ := payload["iss"].(string)
	if issuer == "" {
		return
	}
	discovery, err := p.discoveryDoc()
	if err != nil || discovery.Issuer == "" {
		return
	}
	if issuer != discovery.Issuer {
		payload["active"] = false
	}
}

func (p *Plugin) jwksPublicKey(r *http.Request, token jwtToken) (any, error) {
	discovery, err := p.discoveryDoc()
	if err != nil {
		return nil, err
	}
	if discovery.JWKSURI == "" {
		return nil, errors.New("openid discovery document has no jwks_uri")
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, discovery.JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("jwks endpoint returned %d", resp.StatusCode)
	}

	var jwks struct {
		Keys []jwkKey `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, err
	}

	kid, _ := token.header["kid"].(string)
	algorithm, _ := token.header["alg"].(string)
	for _, key := range jwks.Keys {
		if key.Kty != "RSA" {
			continue
		}
		if kid != "" && key.Kid != "" && key.Kid != kid {
			continue
		}
		if key.Alg != "" && key.Alg != algorithm {
			continue
		}
		return key.rsaPublicKey()
	}
	return nil, errors.New("no matching jwks key")
}

type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (k jwkKey) rsaPublicKey() (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, err
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, err
	}
	exponent := new(big.Int).SetBytes(exponentBytes).Int64()
	if exponent <= 0 {
		return nil, fmt.Errorf("invalid RSA exponent")
	}

	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(exponent)}, nil
}

func (p *Plugin) introspect(r *http.Request, token string) (map[string]any, error) {
	endpoint, err := p.introspectionEndpoint()
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("token", token)
	req, err := p.authenticatedFormRequest(r, endpoint, form, p.config.IntrospectionEndpointAuthMethod)
	if err != nil {
		return nil, err
	}
	for _, name := range p.config.IntrospectionAddonHeaders {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("introspection endpoint returned %d", resp.StatusCode)
	}

	var claims map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func (p *Plugin) introspectionEndpoint() (string, error) {
	if p.config.IntrospectionEndpoint != "" {
		return p.config.IntrospectionEndpoint, nil
	}
	discovery, err := p.discoveryDoc()
	if err != nil {
		return "", err
	}
	if discovery.IntrospectionEndpoint == "" {
		return "", errors.New("openid discovery document has no introspection_endpoint")
	}
	return discovery.IntrospectionEndpoint, nil
}

func (p *Plugin) discoveryDoc() (discoveryData, error) {
	p.mu.Lock()
	if p.discoveryLoaded {
		discovery := p.discovery
		p.mu.Unlock()
		return discovery, nil
	}
	p.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, p.config.Discovery, nil)
	if err != nil {
		return discoveryData{}, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return discoveryData{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return discoveryData{}, fmt.Errorf("discovery endpoint returned %d", resp.StatusCode)
	}

	var discovery discoveryData
	if err := json.NewDecoder(resp.Body).Decode(&discovery); err != nil {
		return discoveryData{}, err
	}

	p.mu.Lock()
	p.discovery = discovery
	p.discoveryLoaded = true
	p.mu.Unlock()

	return discovery, nil
}

func (p *Plugin) setAccessTokenHeader(r *http.Request, token string) {
	if !*p.config.SetAccessTokenHeader {
		return
	}
	if p.config.AccessTokenInAuthorizationHeader {
		r.Header.Set("Authorization", "Bearer "+token)
		return
	}
	r.Header.Set("X-Access-Token", token)
}

func (p *Plugin) writeBearerUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, p.config.Realm))
	http.Error(w, message, http.StatusUnauthorized)
}

func (p *Plugin) writeInvalidToken(w http.ResponseWriter, message string) {
	w.Header().
		Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s", error="invalid_token", error_description="%s"`, p.config.Realm, message))
	http.Error(w, message, http.StatusUnauthorized)
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p.config.SSLVerify != nil && !*p.config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if p.httpProxy != nil || p.httpsProxy != nil {
		transport.Proxy = p.proxyForRequest
	}
	return transport
}

func (p *Plugin) configureProxy() error {
	if p.config.ProxyOpts == nil {
		return nil
	}

	var err error
	p.httpProxy, err = parseProxyURL(p.config.ProxyOpts.HTTPProxy, p.config.ProxyOpts.HTTPProxyAuthorization)
	if err != nil {
		return fmt.Errorf("invalid proxy_opts.http_proxy: %w", err)
	}
	p.httpsProxy, err = parseProxyURL(p.config.ProxyOpts.HTTPSProxy, p.config.ProxyOpts.HTTPSProxyAuthorization)
	if err != nil {
		return fmt.Errorf("invalid proxy_opts.https_proxy: %w", err)
	}
	for host := range strings.SplitSeq(p.config.ProxyOpts.NoProxy, ",") {
		if host = strings.TrimSpace(strings.ToLower(host)); host != "" {
			p.noProxy = append(p.noProxy, strings.TrimPrefix(host, "."))
		}
	}
	return nil
}

func parseProxyURL(rawURL, authorization string) (*url.URL, error) {
	if rawURL == "" {
		return nil, nil
	}
	proxyURL, err := url.Parse(rawURL)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		if err == nil {
			err = errors.New("proxy URL must include scheme and host")
		}
		return nil, err
	}
	if authorization == "" {
		return proxyURL, nil
	}
	parts := strings.SplitN(authorization, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Basic") {
		return nil, errors.New("proxy authorization must use Basic credentials")
	}
	credentials, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode proxy authorization: %w", err)
	}
	username, password, found := strings.Cut(string(credentials), ":")
	if !found {
		return nil, errors.New("proxy authorization must contain username and password")
	}
	proxyURL.User = url.UserPassword(username, password)
	return proxyURL, nil
}

func (p *Plugin) proxyForRequest(request *http.Request) (*url.URL, error) {
	host := strings.ToLower(request.URL.Hostname())
	for _, bypassHost := range p.noProxy {
		if bypassHost == "*" || host == bypassHost || strings.HasSuffix(host, "."+bypassHost) {
			return nil, nil
		}
	}
	if request.URL.Scheme == "https" {
		return p.httpsProxy, nil
	}
	return p.httpProxy, nil
}

func clearOutputHeaders(r *http.Request) {
	r.Header.Del("X-Access-Token")
	r.Header.Del("X-Userinfo")
	r.Header.Del("X-ID-Token")
	r.Header.Del("X-Refresh-Token")
}

func tokenActive(claims map[string]any) bool {
	active, ok := claims["active"]
	if !ok {
		return true
	}
	switch v := active.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

func requiredScopesPresent(required []string, claims map[string]any) bool {
	if len(required) == 0 {
		return true
	}

	available := map[string]struct{}{}
	if scope, ok := claims["scope"].(string); ok {
		for item := range strings.FieldsSeq(scope) {
			available[item] = struct{}{}
		}
	}
	for _, scope := range required {
		if _, ok := available[scope]; !ok {
			return false
		}
	}
	return true
}
