package openid_connect

import (
	"crypto"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "crypto/sha256"
	_ "crypto/sha512"

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
	JWKSURI               string `json:"jwks_uri"`
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
