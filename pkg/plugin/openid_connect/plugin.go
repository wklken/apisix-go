package openid_connect

import (
	"crypto"
	"crypto/rsa"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "crypto/sha512"

	"github.com/redis/go-redis/v9"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client              *http.Client
	clientRSAPrivateKey *rsa.PrivateKey
	staticPublicKey     crypto.PublicKey
	sessionStore        sessionStore
	httpProxy           *url.URL
	httpsProxy          *url.URL
	noProxy             []string
	claimSchema         *util.CompiledSchema
	now                 func() time.Time

	clientRelease func()

	mu              sync.Mutex
	discovery       discoveryData
	discoveryLoaded bool

	providerMu sync.Mutex
	provider   *providerClient
}

const (
	priority = 2599
	name     = "openid-connect"

	defaultSessionIdlingTimeout   = 3600
	defaultSessionRollingTimeout  = 86400
	defaultSessionAbsoluteTimeout = 604800
)

const schema = `
{
  "type": "object",
	"additionalProperties": false,
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
      "additionalProperties": false,
      "properties": {
        "secret": {"type": "string", "minLength": 16},
        "cookie_name": {"type": "string"},
        "cookie_path": {"type": "string"},
        "cookie_domain": {"type": "string"},
        "cookie_secure": {"type": "boolean"},
        "cookie_http_only": {"type": "boolean"},
        "cookie_same_site": {"type": "string", "enum": ["Strict", "Lax", "None", "Default"]},
        "idling_timeout": {"type": "integer", "minimum": 1},
        "rolling_timeout": {"type": "integer", "minimum": 1},
        "absolute_timeout": {"type": "integer", "minimum": 1},
        "cookie": {
          "type": "object",
          "additionalProperties": false,
          "properties": {"lifetime": {"type": "integer"}}
        },
        "storage": {"type": "string", "enum": ["cookie", "redis"]},
        "redis": {
          "type": "object",
          "additionalProperties": false,
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
      },
      "allOf": [
        {
          "if": {
            "properties": {"cookie_same_site": {"const": "None"}},
            "required": ["cookie_same_site"]
          },
          "then": {
            "properties": {"cookie_secure": {"const": true}},
            "required": ["cookie_secure"]
          }
        }
      ]
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
      "type": "string",
      "enum": ["RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "EdDSA"]
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
      "type": "object",
      "properties": {
        "audience": {
          "type": "object",
          "properties": {
            "claim": {"type": "string"},
            "required": {"type": "boolean"},
            "match_with_client_id": {"type": "boolean"}
          }
        },
        "issuer": {
          "type": "object",
          "properties": {
            "valid_issuers": {
              "type": "array",
              "items": {"type": "string"}
            }
          }
        }
      }
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

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.now == nil {
		p.now = time.Now
	}
	if p.config.TokenSigningAlgValuesExpected != "" &&
		!validTokenSigningAlgorithm(p.config.TokenSigningAlgValuesExpected) {
		return fmt.Errorf(
			"unsupported token_signing_alg_values_expected %q",
			p.config.TokenSigningAlgValuesExpected,
		)
	}
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
	if p.config.ClientSecret == "" {
		if p.config.BearerOnly {
			if p.config.PublicKey == "" && !p.config.UseJWKS &&
				p.config.IntrospectionEndpointAuthMethod != "private_key_jwt" {
				return errors.New("client_secret is required for bearer introspection")
			}
		} else if !p.config.UsePKCE && p.config.TokenEndpointAuthMethod != "private_key_jwt" {
			return errors.New("client_secret is required for code flow")
		}
	}
	if _, err := p.configuredIssuers(); err != nil {
		return err
	}
	if len(p.config.ClaimSchema) > 0 {
		encoded, err := json.Marshal(p.config.ClaimSchema)
		if err != nil {
			return errors.New("failed to encode claim_schema")
		}
		compiled, err := util.CompileSchema(string(encoded))
		if err != nil {
			return fmt.Errorf("check claim_schema failed: %w", err)
		}
		p.claimSchema = compiled
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
		if p.config.Session.IdlingTimeout == 0 {
			p.config.Session.IdlingTimeout = defaultSessionIdlingTimeout
		}
		if p.config.Session.RollingTimeout == 0 {
			p.config.Session.RollingTimeout = defaultSessionRollingTimeout
		}
		if p.config.Session.AbsoluteTimeout == 0 {
			p.config.Session.AbsoluteTimeout = defaultSessionAbsoluteTimeout
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

func (p *Plugin) MaterializeSecrets() error {
	if p.config.PublicKey == "" {
		return nil
	}
	key, err := store.MaterializeSecret(p.config.PublicKey)
	if err != nil {
		return errors.New("resolve openid-connect public_key reference: credential unavailable")
	}
	defer key.Destroy()
	encoded := key.Bytes()
	defer clear(encoded)
	p.staticPublicKey, err = parsePublicKey(encoded)
	if err != nil {
		return errors.New("failed to parse public key")
	}
	p.config.PublicKey = key.Descriptor()
	return nil
}

func (p *Plugin) currentTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func validTokenSigningAlgorithm(algorithm string) bool {
	switch algorithm {
	case "RS256", "RS384", "RS512",
		"ES256", "ES384", "ES512",
		"PS256", "PS384", "PS512",
		"EdDSA":
		return true
	default:
		return false
	}
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
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name+"-session", configUID),
		func() (any, error) { return client, nil },
		shared.CloseRedisClient,
	)
	if err != nil {
		return err
	}
	p.sessionStore = &redisSessionStore{
		client: value.(*redis.Client),
	}
	p.clientRelease = release

	return nil
}

func (p *Plugin) Stop() {
	if p.clientRelease != nil {
		p.clientRelease()
		p.clientRelease = nil
	}
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		clientXAccessToken := ""
		if p.config.BearerOnly {
			clientXAccessToken = r.Header.Get("X-Access-Token")
		}
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
			if p.config.UnauthAction == "deny" {
				w.WriteHeader(http.StatusUnauthorized)
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
			_, _ = w.Write(
				util.StringToBytes(
					`{"error":"required scopes ` + strings.Join(p.config.RequiredScopes, ", ") + ` not present"}`,
				),
			)
			return
		}
		if statusCode, responseBody := p.validateConfiguredClaims(claims); statusCode != 0 {
			w.WriteHeader(statusCode)
			_, _ = w.Write(util.StringToBytes(responseBody))
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
