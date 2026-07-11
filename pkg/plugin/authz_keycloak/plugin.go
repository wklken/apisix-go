package authz_keycloak

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client *resty.Client

	mu                  sync.Mutex
	discovery           discoveryData
	discoveryExpiresAt  time.Time
	serviceAccountToken tokenCache
}

const (
	priority = 2000
	name     = "authz-keycloak"

	defaultGrantType = "urn:ietf:params:oauth:grant-type:uma-ticket"
)

const schema = `
{
  "type": "object",
  "properties": {
    "discovery": {
      "type": "string",
      "minLength": 1,
      "maxLength": 4096
    },
    "token_endpoint": {
      "type": "string",
      "minLength": 1,
      "maxLength": 4096
    },
    "resource_registration_endpoint": {
      "type": "string",
      "minLength": 1,
      "maxLength": 4096
    },
    "client_id": {
      "type": "string",
      "minLength": 1,
      "maxLength": 100
    },
    "client_secret": {
      "type": "string",
      "minLength": 1,
      "maxLength": 100
    },
    "grant_type": {
      "type": "string",
      "default": "urn:ietf:params:oauth:grant-type:uma-ticket",
      "enum": ["urn:ietf:params:oauth:grant-type:uma-ticket"],
      "minLength": 1,
      "maxLength": 100
    },
    "policy_enforcement_mode": {
      "type": "string",
      "enum": ["ENFORCING", "PERMISSIVE"],
      "default": "ENFORCING"
    },
    "permissions": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1,
        "maxLength": 100
      },
      "uniqueItems": true,
      "default": []
    },
    "lazy_load_paths": {
      "type": "boolean",
      "default": false
    },
    "http_method_as_scope": {
      "type": "boolean",
      "default": false
    },
    "timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 3000
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "cache_ttl_seconds": {
      "type": "integer",
      "minimum": 1,
      "default": 86400
    },
    "keepalive": {
      "type": "boolean",
      "default": true
    },
    "keepalive_timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 60000
    },
    "keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    },
    "access_denied_redirect_uri": {
      "type": "string",
      "minLength": 1,
      "maxLength": 2048
    },
    "access_token_expires_in": {
      "type": "integer",
      "minimum": 1,
      "default": 300
    },
    "access_token_expires_leeway": {
      "type": "integer",
      "minimum": 0,
      "default": 0
    },
    "refresh_token_expires_in": {
      "type": "integer",
      "minimum": 1,
      "default": 3600
    },
    "refresh_token_expires_leeway": {
      "type": "integer",
      "minimum": 0,
      "default": 0
    },
    "password_grant_token_generation_incoming_uri": {
      "type": "string",
      "minLength": 1,
      "maxLength": 4096
    }
  },
  "required": ["client_id"],
  "allOf": [
    {
      "anyOf": [
        {"required": ["discovery"]},
        {"required": ["token_endpoint"]}
      ]
    },
    {
      "anyOf": [
        {
          "properties": {
            "lazy_load_paths": {"enum": [false]}
          }
        },
        {
          "properties": {
            "lazy_load_paths": {"enum": [true]}
          },
          "anyOf": [
            {"required": ["discovery"]},
            {"required": ["resource_registration_endpoint"]}
          ]
        }
      ]
    }
  ]
}
`

type Config struct {
	Discovery                               string   `json:"discovery,omitempty"`
	TokenEndpoint                           string   `json:"token_endpoint,omitempty"`
	ResourceRegistrationEndpoint            string   `json:"resource_registration_endpoint,omitempty"`
	ClientID                                string   `json:"client_id"`
	ClientSecret                            string   `json:"client_secret,omitempty"`
	GrantType                               string   `json:"grant_type,omitempty"`
	PolicyEnforcementMode                   string   `json:"policy_enforcement_mode,omitempty"`
	Permissions                             []string `json:"permissions,omitempty"`
	LazyLoadPaths                           bool     `json:"lazy_load_paths,omitempty"`
	HTTPMethodAsScope                       bool     `json:"http_method_as_scope,omitempty"`
	Timeout                                 int      `json:"timeout,omitempty"`
	SSLVerify                               *bool    `json:"ssl_verify,omitempty"`
	CacheTTLSeconds                         int      `json:"cache_ttl_seconds,omitempty"`
	Keepalive                               *bool    `json:"keepalive,omitempty"`
	KeepaliveTimeout                        int      `json:"keepalive_timeout,omitempty"`
	KeepalivePool                           int      `json:"keepalive_pool,omitempty"`
	AccessDeniedRedirectURI                 string   `json:"access_denied_redirect_uri,omitempty"`
	AccessTokenExpiresIn                    int      `json:"access_token_expires_in,omitempty"`
	AccessTokenExpiresLeeway                int      `json:"access_token_expires_leeway,omitempty"`
	RefreshTokenExpiresIn                   int      `json:"refresh_token_expires_in,omitempty"`
	RefreshTokenExpiresLeeway               int      `json:"refresh_token_expires_leeway,omitempty"`
	PasswordGrantTokenGenerationIncomingURI string   `json:"password_grant_token_generation_incoming_uri,omitempty"`
}

type discoveryData struct {
	TokenEndpoint                string `json:"token_endpoint"`
	ResourceRegistrationEndpoint string `json:"resource_registration_endpoint"`
}

type tokenEndpointResponse struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
}

type tokenCache struct {
	value                 string
	expiresAt             time.Time
	refreshToken          string
	refreshTokenExpiresAt time.Time
	cacheExpiresAt        time.Time
}

type discoveryCacheEntry struct {
	value     discoveryData
	expiresAt time.Time
}

var sharedCache = struct {
	sync.Mutex
	discovery           map[string]discoveryCacheEntry
	serviceAccountToken map[string]tokenCache
}{
	discovery:           make(map[string]discoveryCacheEntry),
	serviceAccountToken: make(map[string]tokenCache),
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	p.applyDefaults()

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.Discovery)
	configUID.Add(p.config.TokenEndpoint)
	configUID.Add(p.config.ResourceRegistrationEndpoint)
	configUID.Add(p.config.ClientID)
	configUID.Add(p.config.Timeout)
	configUID.Add(p.sslVerify())
	configUID.Add(*p.config.Keepalive)
	configUID.Add(p.config.KeepaliveTimeout)
	configUID.Add(p.config.KeepalivePool)

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Millisecond)
	client.SetTransport(p.transport())
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.isPasswordGrantRequest(r) {
			p.generateTokenUsingPasswordGrant(w, r)
			return
		}

		token := fetchJWTToken(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Missing JWT token in request"})
			return
		}

		status, body, headers := p.evaluatePermissions(r, token)
		if status != 0 {
			for key, value := range headers {
				w.Header().Set(key, value)
			}
			if body != "" {
				w.WriteHeader(status)
				w.Write([]byte(body))
				return
			}
			w.WriteHeader(status)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (p *Plugin) applyDefaults() {
	if p.config.GrantType == "" {
		p.config.GrantType = defaultGrantType
	}
	if p.config.PolicyEnforcementMode == "" {
		p.config.PolicyEnforcementMode = "ENFORCING"
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3000
	}
	if p.config.SSLVerify == nil {
		verify := true
		p.config.SSLVerify = &verify
	}
	if p.config.CacheTTLSeconds == 0 {
		p.config.CacheTTLSeconds = 24 * 60 * 60
	}
	if p.config.Keepalive == nil {
		keepalive := true
		p.config.Keepalive = &keepalive
	}
	if p.config.KeepaliveTimeout == 0 {
		p.config.KeepaliveTimeout = 60000
	}
	if p.config.KeepalivePool == 0 {
		p.config.KeepalivePool = 5
	}
	if p.config.AccessTokenExpiresIn == 0 {
		p.config.AccessTokenExpiresIn = 300
	}
	if p.config.RefreshTokenExpiresIn == 0 {
		p.config.RefreshTokenExpiresIn = 3600
	}
}

func (p *Plugin) sslVerify() bool {
	return p.config.SSLVerify == nil || *p.config.SSLVerify
}

func (p *Plugin) transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = !*p.config.Keepalive
	transport.IdleConnTimeout = time.Duration(p.config.KeepaliveTimeout) * time.Millisecond
	transport.MaxIdleConnsPerHost = p.config.KeepalivePool
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: !p.sslVerify()}

	return transport
}

func (p *Plugin) evaluatePermissions(r *http.Request, token string) (int, string, map[string]string) {
	permissions, err := p.permissionsForRequest(r)
	if err != nil {
		return http.StatusServiceUnavailable, err.Error(), nil
	}

	if len(permissions) == 0 && p.config.PolicyEnforcementMode == "ENFORCING" {
		if p.config.AccessDeniedRedirectURI != "" {
			return http.StatusTemporaryRedirect, "", map[string]string{"Location": p.config.AccessDeniedRedirectURI}
		}
		return http.StatusForbidden, `{"error":"access_denied","error_description":"not_authorized"}`, nil
	}

	endpoint, err := p.tokenEndpoint()
	if err != nil {
		return http.StatusServiceUnavailable, err.Error(), nil
	}

	form := url.Values{}
	form.Set("grant_type", p.config.GrantType)
	form.Set("audience", p.config.ClientID)
	form.Set("response_mode", "decision")
	for _, permission := range scopedPermissions(permissions, r.Method, p.config.HTTPMethodAsScope) {
		form.Add("permission", permission)
	}

	resp, err := p.client.R().
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		SetHeader("Authorization", token).
		SetBody(form.Encode()).
		Post(endpoint)
	if err != nil {
		return http.StatusServiceUnavailable, "", nil
	}
	if resp.StatusCode() == http.StatusForbidden && p.config.AccessDeniedRedirectURI != "" {
		return http.StatusTemporaryRedirect, "", map[string]string{"Location": p.config.AccessDeniedRedirectURI}
	}
	if resp.StatusCode() >= http.StatusBadRequest {
		return resp.StatusCode(), resp.String(), nil
	}
	return 0, "", nil
}

func (p *Plugin) permissionsForRequest(r *http.Request) ([]string, error) {
	if !p.config.LazyLoadPaths {
		return append([]string(nil), p.config.Permissions...), nil
	}

	accessToken, err := p.serviceAccountAccessToken()
	if err != nil {
		return nil, err
	}

	endpoint, err := p.resourceRegistrationEndpoint()
	if err != nil {
		return nil, err
	}

	resp, err := p.client.R().
		SetHeader("Authorization", "Bearer "+accessToken).
		SetQueryParams(map[string]string{
			"uri":         r.URL.Path,
			"matchingUri": "true",
		}).
		Get(endpoint)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("resource registration endpoint returned %d", resp.StatusCode())
	}

	var permissions []string
	if err := json.Unmarshal(resp.Body(), &permissions); err != nil {
		return nil, err
	}
	return permissions, nil
}

func (p *Plugin) serviceAccountAccessToken() (string, error) {
	endpoint, err := p.tokenEndpoint()
	if err != nil {
		return "", err
	}

	now := time.Now()
	p.mu.Lock()
	if validTokenCache(p.serviceAccountToken, now) {
		token := p.serviceAccountToken.value
		p.mu.Unlock()
		return token, nil
	}
	cachedToken := p.serviceAccountToken
	p.mu.Unlock()

	if cached, ok := loadSharedServiceAccountToken(endpoint, p.config.ClientID, now); ok {
		p.mu.Lock()
		p.serviceAccountToken = cached
		p.mu.Unlock()
		return cached.value, nil
	} else if cached.value != "" {
		cachedToken = cached
	}

	if cachedToken.refreshToken != "" && now.Before(cachedToken.refreshTokenExpiresAt) {
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("client_id", p.config.ClientID)
		form.Set("client_secret", p.config.ClientSecret)
		form.Set("refresh_token", cachedToken.refreshToken)

		refreshed, err := p.requestServiceAccountToken(endpoint, form)
		if err != nil {
			return "", err
		}
		if refreshed.AccessToken != "" {
			return p.cacheServiceAccountToken(endpoint, refreshed, cachedToken), nil
		}
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", p.config.ClientID)
	form.Set("client_secret", p.config.ClientSecret)

	response, err := p.requestServiceAccountToken(endpoint, form)
	if err != nil {
		return "", err
	}
	if response.AccessToken == "" {
		return "", errors.New("response does not contain access_token field")
	}

	return p.cacheServiceAccountToken(endpoint, response, tokenCache{}), nil
}

func (p *Plugin) requestServiceAccountToken(endpoint string, form url.Values) (tokenEndpointResponse, error) {
	resp, err := p.client.R().
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		SetBody(form.Encode()).
		Post(endpoint)
	if err != nil {
		return tokenEndpointResponse{}, err
	}
	if resp.StatusCode() != http.StatusOK {
		return tokenEndpointResponse{}, fmt.Errorf("token endpoint returned %d", resp.StatusCode())
	}

	var response tokenEndpointResponse
	if err := json.Unmarshal(resp.Body(), &response); err != nil {
		return tokenEndpointResponse{}, err
	}

	return response, nil
}

func (p *Plugin) cacheServiceAccountToken(endpoint string, response tokenEndpointResponse, previous tokenCache) string {
	now := time.Now()
	expiresIn := response.ExpiresIn
	if expiresIn == 0 {
		expiresIn = p.config.AccessTokenExpiresIn
	}
	expiresIn -= 1 + p.config.AccessTokenExpiresLeeway
	if expiresIn <= 0 {
		expiresIn = 1
	}

	cache := tokenCache{
		value:          response.AccessToken,
		expiresAt:      now.Add(time.Duration(expiresIn) * time.Second),
		cacheExpiresAt: now.Add(time.Duration(p.config.CacheTTLSeconds) * time.Second),
	}
	if response.RefreshToken == "" && previous.refreshToken != "" {
		cache.refreshToken = previous.refreshToken
		cache.refreshTokenExpiresAt = previous.refreshTokenExpiresAt
	} else if response.RefreshToken != "" {
		refreshExpiresIn := response.RefreshExpiresIn
		if refreshExpiresIn == 0 {
			refreshExpiresIn = p.config.RefreshTokenExpiresIn
		}
		refreshExpiresIn -= 1 + p.config.RefreshTokenExpiresLeeway
		if refreshExpiresIn <= 0 {
			refreshExpiresIn = 1
		}
		cache.refreshToken = response.RefreshToken
		cache.refreshTokenExpiresAt = now.Add(time.Duration(refreshExpiresIn) * time.Second)
	}

	p.mu.Lock()
	p.serviceAccountToken = cache
	p.mu.Unlock()
	storeSharedServiceAccountToken(endpoint, p.config.ClientID, cache)

	return response.AccessToken
}

func validTokenCache(cache tokenCache, now time.Time) bool {
	return cache.value != "" && now.Before(cache.expiresAt) &&
		(cache.cacheExpiresAt.IsZero() || now.Before(cache.cacheExpiresAt))
}

func serviceAccountCacheKey(endpoint string, clientID string) string {
	return endpoint + ":" + clientID
}

func loadSharedServiceAccountToken(endpoint string, clientID string, now time.Time) (tokenCache, bool) {
	sharedCache.Lock()
	defer sharedCache.Unlock()

	cache, ok := sharedCache.serviceAccountToken[serviceAccountCacheKey(endpoint, clientID)]
	return cache, ok && validTokenCache(cache, now)
}

func storeSharedServiceAccountToken(endpoint string, clientID string, cache tokenCache) {
	sharedCache.Lock()
	sharedCache.serviceAccountToken[serviceAccountCacheKey(endpoint, clientID)] = cache
	sharedCache.Unlock()
}

func (p *Plugin) tokenEndpoint() (string, error) {
	if p.config.TokenEndpoint != "" {
		return p.config.TokenEndpoint, nil
	}
	discovery, err := p.discover()
	if err != nil {
		return "", err
	}
	if discovery.TokenEndpoint == "" {
		return "", errors.New("unable to determine token endpoint")
	}
	return discovery.TokenEndpoint, nil
}

func (p *Plugin) resourceRegistrationEndpoint() (string, error) {
	if p.config.ResourceRegistrationEndpoint != "" {
		return p.config.ResourceRegistrationEndpoint, nil
	}
	discovery, err := p.discover()
	if err != nil {
		return "", err
	}
	if discovery.ResourceRegistrationEndpoint == "" {
		return "", errors.New("unable to determine registration endpoint")
	}
	return discovery.ResourceRegistrationEndpoint, nil
}

func (p *Plugin) discover() (discoveryData, error) {
	now := time.Now()
	p.mu.Lock()
	if (p.discovery.TokenEndpoint != "" || p.discovery.ResourceRegistrationEndpoint != "") &&
		(p.discoveryExpiresAt.IsZero() || now.Before(p.discoveryExpiresAt)) {
		discovery := p.discovery
		p.mu.Unlock()
		return discovery, nil
	}
	p.mu.Unlock()

	if p.config.Discovery == "" {
		return discoveryData{}, errors.New("discovery endpoint is not configured")
	}
	if discovery, ok := loadSharedDiscovery(p.config.Discovery, now); ok {
		p.mu.Lock()
		p.discovery = discovery
		p.discoveryExpiresAt = now.Add(time.Duration(p.config.CacheTTLSeconds) * time.Second)
		p.mu.Unlock()
		return discovery, nil
	}
	resp, err := p.client.R().Get(p.config.Discovery)
	if err != nil {
		return discoveryData{}, err
	}
	if resp.StatusCode() != http.StatusOK {
		return discoveryData{}, fmt.Errorf("discovery endpoint returned %d", resp.StatusCode())
	}

	var discovery discoveryData
	if err := json.Unmarshal(resp.Body(), &discovery); err != nil {
		return discoveryData{}, err
	}

	p.mu.Lock()
	p.discovery = discovery
	p.discoveryExpiresAt = now.Add(time.Duration(p.config.CacheTTLSeconds) * time.Second)
	p.mu.Unlock()
	storeSharedDiscovery(p.config.Discovery, discovery, p.config.CacheTTLSeconds, now)

	return discovery, nil
}

func loadSharedDiscovery(endpoint string, now time.Time) (discoveryData, bool) {
	sharedCache.Lock()
	defer sharedCache.Unlock()

	cache, ok := sharedCache.discovery[endpoint]
	return cache.value, ok && now.Before(cache.expiresAt)
}

func storeSharedDiscovery(endpoint string, discovery discoveryData, ttlSeconds int, now time.Time) {
	sharedCache.Lock()
	sharedCache.discovery[endpoint] = discoveryCacheEntry{
		value:     discovery,
		expiresAt: now.Add(time.Duration(ttlSeconds) * time.Second),
	}
	sharedCache.Unlock()
}

func (p *Plugin) isPasswordGrantRequest(r *http.Request) bool {
	return p.config.PasswordGrantTokenGenerationIncomingURI != "" &&
		r.URL.RequestURI() == p.config.PasswordGrantTokenGenerationIncomingURI &&
		r.Method == http.MethodPost &&
		r.Header.Get("Content-Type") == "application/x-www-form-urlencoded"
}

func (p *Plugin) generateTokenUsingPasswordGrant(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "Failed to get request body."})
		return
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "Failed to get request body."})
		return
	}
	username := values.Get("username")
	if username == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "username is missing."})
		return
	}
	password := values.Get("password")
	if password == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "password is missing."})
		return
	}

	endpoint, err := p.tokenEndpoint()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": err.Error()})
		return
	}

	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("client_id", p.config.ClientID)
	form.Set("client_secret", p.config.ClientSecret)
	form.Set("username", username)
	form.Set("password", password)

	resp, err := p.client.R().
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		SetBody(form.Encode()).
		Post(endpoint)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Accessing token endpoint URL failed."})
		return
	}

	for key, values := range resp.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode())
	w.Write(resp.Body())
}

func fetchJWTToken(r *http.Request) string {
	token := r.Header.Get("Authorization")
	if token == "" {
		return ""
	}
	if strings.HasPrefix(token, "Bearer ") || strings.HasPrefix(token, "bearer ") {
		return token
	}
	return "Bearer " + token
}

func scopedPermissions(permissions []string, method string, useMethodScope bool) []string {
	if !useMethodScope {
		return append([]string(nil), permissions...)
	}
	out := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		if strings.Contains(permission, "#") {
			out = append(out, permission+", "+method)
			continue
		}
		out = append(out, permission+"#"+method)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(value); err != nil {
		return
	}
	w.Write(buf.Bytes())
}
