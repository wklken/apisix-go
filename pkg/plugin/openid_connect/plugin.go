package openid_connect

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
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
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "crypto/sha512"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client *http.Client

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
        "storage": {"type": "string", "enum": ["cookie", "redis"]}
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
	Session                          SessionConfig  `json:"session,omitempty"`
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
}

type SessionCookieConfig struct {
	Lifetime int `json:"lifetime,omitempty"`
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
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	FlowState     string `json:"flow_state,omitempty"`
	FlowExpiresAt int64  `json:"flow_expires_at,omitempty"`
	OriginalURI   string `json:"original_uri,omitempty"`
	CodeVerifier  string `json:"code_verifier,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	IDToken       string `json:"id_token,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	Userinfo      string `json:"userinfo,omitempty"`
	ExpiresAt     int64  `json:"expires_at,omitempty"`
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
	if p.config.TokenEndpointAuthMethod != "client_secret_basic" &&
		p.config.TokenEndpointAuthMethod != "client_secret_post" {
		return fmt.Errorf("unsupported token_endpoint_auth_method %q", p.config.TokenEndpointAuthMethod)
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
		if p.config.Session.Storage != "cookie" {
			return fmt.Errorf("openid-connect session storage %q is not supported", p.config.Session.Storage)
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

	session, err := p.readSession(r)
	if err == nil && session != nil && p.sessionValid(*session, time.Now()) {
		session.UpdatedAt = time.Now().Unix()
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

	p.beginAuthorization(w, r, redirectURI)
}

func (p *Plugin) beginAuthorization(w http.ResponseWriter, r *http.Request, redirectURI string) {
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

	parameters := url.Values{}
	parameters.Set("client_id", p.config.ClientID)
	parameters.Set("scope", p.config.Scope)
	parameters.Set("response_type", "code")
	parameters.Set("redirect_uri", redirectURI)
	parameters.Set("state", state)
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
	for key, values := range parameters {
		query[key] = values
	}
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
		CreatedAt:    now.Unix(),
		UpdatedAt:    now.Unix(),
		AccessToken:  tokens.AccessToken,
		IDToken:      tokens.IDToken,
		RefreshToken: tokens.RefreshToken,
	}
	if tokens.ExpiresIn > 0 {
		newSession.ExpiresAt = now.Add(time.Duration(tokens.ExpiresIn) * time.Second).Unix()
	}
	if userinfo, err := p.userinfo(r, tokens.AccessToken); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	} else {
		newSession.Userinfo = userinfo
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
	discovery, err := p.discoveryDoc()
	if err != nil {
		return tokenResponse{}, err
	}
	if discovery.TokenEndpoint == "" {
		return tokenResponse{}, errors.New("openid discovery document has no token_endpoint")
	}

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
	if p.config.TokenEndpointAuthMethod == "client_secret_post" {
		form.Set("client_id", p.config.ClientID)
		form.Set("client_secret", p.config.ClientSecret)
	}
	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodPost,
		discovery.TokenEndpoint,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.config.TokenEndpointAuthMethod == "client_secret_basic" {
		req.SetBasicAuth(p.config.ClientID, p.config.ClientSecret)
	}
	resp, err := p.client.Do(req)
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
	p.clearSession(w)
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

func (p *Plugin) clearSession(w http.ResponseWriter) {
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

func (p *Plugin) sessionValid(session sessionData, now time.Time) bool {
	if session.AccessToken == "" {
		return false
	}
	if session.ExpiresAt > 0 && session.ExpiresAt <= now.Unix() {
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
		for _, item := range typed {
			if item == clientID {
				return true
			}
		}
	}
	return false
}

func (p *Plugin) validateClaimSchema(claims map[string]any) error {
	if len(p.config.ClaimSchema) == 0 {
		return nil
	}

	encoded, err := json.Marshal(p.config.ClaimSchema)
	if err != nil {
		return fmt.Errorf("failed to encode claim schema")
	}
	if err := util.Validate(claims, string(encoded)); err != nil {
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
