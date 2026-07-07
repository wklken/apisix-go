package openid_connect

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client *http.Client

	mu        sync.Mutex
	discovery discoveryData
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
    "bearer_only": {
      "type": "boolean",
      "default": false
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
	BearerOnly                       bool           `json:"bearer_only,omitempty"`
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
	SetAccessTokenHeader             *bool          `json:"set_access_token_header,omitempty"`
	AccessTokenInAuthorizationHeader bool           `json:"access_token_in_authorization_header,omitempty"`
	SetIDTokenHeader                 *bool          `json:"set_id_token_header,omitempty"`
	SetUserinfoHeader                *bool          `json:"set_userinfo_header,omitempty"`
	SetRefreshTokenHeader            *bool          `json:"set_refresh_token_header,omitempty"`
	IntrospectionAddonHeaders        []string       `json:"introspection_addon_headers,omitempty"`
	ClaimValidator                   map[string]any `json:"claim_validator,omitempty"`
	ClaimSchema                      map[string]any `json:"claim_schema,omitempty"`
}

type discoveryData struct {
	Issuer                string `json:"issuer"`
	IntrospectionEndpoint string `json:"introspection_endpoint"`
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
	if p.config.Realm == "" {
		p.config.Realm = "apisix"
	}
	if p.config.LogoutPath == "" {
		p.config.LogoutPath = "/logout"
	}
	if p.config.UnauthAction == "" {
		p.config.UnauthAction = "auth"
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

	p.client = &http.Client{
		Timeout:   time.Duration(p.config.Timeout) * time.Second,
		Transport: p.transport(),
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		clientXAccessToken := r.Header.Get("X-Access-Token")
		clearOutputHeaders(r)

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
			p.writeBearerUnauthorized(w, "unauthorized request")
			return
		}

		claims, err := p.introspect(r, token)
		if err != nil {
			p.writeInvalidToken(w, err.Error())
			return
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

func (p *Plugin) introspect(r *http.Request, token string) (map[string]any, error) {
	endpoint, err := p.introspectionEndpoint()
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("token", token)
	if p.config.IntrospectionEndpointAuthMethod == "client_secret_post" {
		form.Set("client_id", p.config.ClientID)
		form.Set("client_secret", p.config.ClientSecret)
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.config.IntrospectionEndpointAuthMethod == "client_secret_basic" {
		req.SetBasicAuth(p.config.ClientID, p.config.ClientSecret)
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
	if p.discovery.IntrospectionEndpoint != "" {
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
	return transport
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
		for _, item := range strings.Fields(scope) {
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
