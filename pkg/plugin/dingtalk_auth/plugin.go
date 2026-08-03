package dingtalk_auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client     *http.Client
	tokenCache map[string]tokenCacheEntry
	mu         sync.Mutex
}

const (
	priority = 2430
	name     = "dingtalk-auth"

	defaultUserInfoURL    = "https://oapi.dingtalk.com/topapi/v2/user/getuserinfo"
	defaultAccessTokenURL = "https://api.dingtalk.com/v1.0/oauth2/accessToken"
	sessionCookieName     = "dingtalk_session"
	tokenCacheTTL         = 7000 * time.Second
)

const schema = `
{
  "type": "object",
  "properties": {
    "app_key": {
      "type": "string",
      "minLength": 1
    },
    "app_secret": {
      "type": "string",
      "minLength": 1
    },
    "code_header": {
      "type": "string",
      "minLength": 1,
      "default": "X-DingTalk-Code"
    },
    "code_query": {
      "type": "string",
      "minLength": 1,
      "default": "code"
    },
    "userinfo_url": {
      "type": "string",
      "minLength": 1,
      "default": "https://oapi.dingtalk.com/topapi/v2/user/getuserinfo"
    },
    "access_token_url": {
      "type": "string",
      "minLength": 1,
      "default": "https://api.dingtalk.com/v1.0/oauth2/accessToken"
    },
    "set_userinfo_header": {
      "type": "boolean",
      "default": true
    },
    "redirect_uri": {
      "type": "string",
      "minLength": 1
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 6000
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "secret": {
      "type": "string",
      "minLength": 8,
      "maxLength": 32
    },
    "secret_fallbacks": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 8,
        "maxLength": 32
      }
    },
    "cookie_expires_in": {
      "type": "integer",
      "minimum": 1,
      "default": 86400
    }
  },
  "required": ["app_key", "app_secret", "secret", "redirect_uri"]
}
`

type Config struct {
	AppKey            string   `json:"app_key"`
	AppSecret         string   `json:"app_secret"`
	CodeHeader        string   `json:"code_header,omitempty"`
	CodeQuery         string   `json:"code_query,omitempty"`
	UserInfoURL       string   `json:"userinfo_url,omitempty"`
	AccessTokenURL    string   `json:"access_token_url,omitempty"`
	SetUserInfoHeader *bool    `json:"set_userinfo_header,omitempty"`
	RedirectURI       string   `json:"redirect_uri"`
	Timeout           int      `json:"timeout,omitempty"`
	SSLVerify         *bool    `json:"ssl_verify,omitempty"`
	Secret            string   `json:"secret"`
	SecretFallbacks   []string `json:"secret_fallbacks,omitempty"`
	CookieExpiresIn   int      `json:"cookie_expires_in,omitempty"`
}

type tokenCacheEntry struct {
	accessToken string
	expiresAt   time.Time
}

type tokenResponse struct {
	AccessToken string `json:"accessToken"`
}

type userInfoResponse struct {
	ErrCode int            `json:"errcode"`
	ErrMsg  string         `json:"errmsg"`
	Result  map[string]any `json:"result"`
}

type sessionPayload struct {
	UserInfo  map[string]any `json:"userinfo"`
	ExpiresAt int64          `json:"expires_at"`
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.CodeHeader == "" {
		p.config.CodeHeader = "X-DingTalk-Code"
	}
	if p.config.CodeQuery == "" {
		p.config.CodeQuery = "code"
	}
	if p.config.UserInfoURL == "" {
		p.config.UserInfoURL = defaultUserInfoURL
	}
	if p.config.AccessTokenURL == "" {
		p.config.AccessTokenURL = defaultAccessTokenURL
	}
	if p.config.SetUserInfoHeader == nil {
		setUserInfoHeader := true
		p.config.SetUserInfoHeader = &setUserInfoHeader
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 6000
	}
	if p.config.SSLVerify == nil {
		sslVerify := true
		p.config.SSLVerify = &sslVerify
	}
	if p.config.CookieExpiresIn == 0 {
		p.config.CookieExpiresIn = 86400
	}
	if p.client == nil {
		p.client = &http.Client{
			Timeout:   time.Duration(p.config.Timeout) * time.Millisecond,
			Transport: p.transport(),
		}
	}
	if p.tokenCache == nil {
		p.tokenCache = make(map[string]tokenCacheEntry)
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("X-Userinfo")

		if userinfo, ok := p.userInfoFromSession(r); ok {
			p.attachUserInfo(r, userinfo)
			next.ServeHTTP(w, r)
			return
		}

		code := p.codeFromRequest(r)
		if code == "" {
			http.Redirect(w, r, p.config.RedirectURI, http.StatusFound)
			return
		}

		accessToken, err := p.accessToken(r)
		if err != nil {
			http.Error(w, util.BuildMessageResponse("Failed to obtain access token"), http.StatusInternalServerError)
			return
		}

		userinfo, authErr, err := p.fetchUserInfo(r, accessToken, code)
		if err != nil {
			if authErr {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(util.BuildMessageResponse("Invalid authorization code")))
				return
			}
			http.Error(
				w,
				util.BuildMessageResponse("Failed to obtain user info from DingTalk"),
				http.StatusServiceUnavailable,
			)
			return
		}

		cookie, err := p.sessionCookie(userinfo)
		if err != nil {
			http.Error(w, util.BuildMessageResponse("Invalid userinfo"), http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, cookie)
		p.attachUserInfo(r, userinfo)
		next.ServeHTTP(w, r)
	})
}

func (p *Plugin) accessToken(r *http.Request) (string, error) {
	cacheKey := strings.Join([]string{p.config.AccessTokenURL, p.config.AppKey, p.config.AppSecret}, "#")

	p.mu.Lock()
	cached, ok := p.tokenCache[cacheKey]
	if ok && time.Now().Before(cached.expiresAt) {
		p.mu.Unlock()
		return cached.accessToken, nil
	}
	p.mu.Unlock()

	accessToken, err := p.fetchAccessToken(r)
	if err != nil {
		return "", err
	}

	p.mu.Lock()
	p.tokenCache[cacheKey] = tokenCacheEntry{
		accessToken: accessToken,
		expiresAt:   time.Now().Add(tokenCacheTTL),
	}
	p.mu.Unlock()
	return accessToken, nil
}

func (p *Plugin) fetchAccessToken(r *http.Request) (string, error) {
	body, err := json.Marshal(map[string]string{
		"appKey":    p.config.AppKey,
		"appSecret": p.config.AppSecret,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.config.AccessTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected response status: %d", resp.StatusCode)
	}

	var token tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return "", err
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("dingtalk token response missing accessToken")
	}
	return token.AccessToken, nil
}

func (p *Plugin) fetchUserInfo(r *http.Request, accessToken string, code string) (map[string]any, bool, error) {
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return nil, false, err
	}

	endpoint, err := url.Parse(p.config.UserInfoURL)
	if err != nil {
		return nil, false, err
	}
	query := endpoint.Query()
	query.Set("access_token", accessToken)
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("unexpected http response status: %d", resp.StatusCode)
	}

	var userinfo userInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&userinfo); err != nil {
		return nil, false, err
	}
	if userinfo.ErrCode != 0 {
		return nil, true, fmt.Errorf("unexpected error code: %d, errmsg: %s", userinfo.ErrCode, userinfo.ErrMsg)
	}
	if userinfo.Result == nil {
		return nil, false, fmt.Errorf("dingtalk userinfo response missing result")
	}
	return userinfo.Result, false, nil
}

func (p *Plugin) userInfoFromSession(r *http.Request) (map[string]any, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, false
	}

	payload, ok := p.verifySignedValue(cookie.Value)
	if !ok {
		return nil, false
	}

	var session sessionPayload
	if err := json.Unmarshal(payload, &session); err != nil {
		return nil, false
	}
	if session.ExpiresAt <= time.Now().Unix() || session.UserInfo == nil {
		return nil, false
	}
	return session.UserInfo, true
}

func (p *Plugin) sessionCookie(userinfo map[string]any) (*http.Cookie, error) {
	payload, err := json.Marshal(sessionPayload{
		UserInfo:  userinfo,
		ExpiresAt: time.Now().Add(time.Duration(p.config.CookieExpiresIn) * time.Second).Unix(),
	})
	if err != nil {
		return nil, err
	}

	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    p.signValue(payload),
		Path:     "/",
		HttpOnly: true,
		MaxAge:   p.config.CookieExpiresIn,
	}, nil
}

func (p *Plugin) attachUserInfo(r *http.Request, userinfo map[string]any) {
	if vars := apisixctx.GetApisixVars(r); vars != nil {
		vars["$external_user"] = userinfo
	}
	if p.config.SetUserInfoHeader != nil && !*p.config.SetUserInfoHeader {
		return
	}
	raw, err := json.Marshal(userinfo)
	if err != nil {
		return
	}
	r.Header.Set("X-Userinfo", base64.StdEncoding.EncodeToString(raw))
}

func (p *Plugin) codeFromRequest(r *http.Request) string {
	if code := r.Header.Get(p.config.CodeHeader); code != "" {
		return code
	}
	return r.URL.Query().Get(p.config.CodeQuery)
}

func (p *Plugin) signValue(value []byte) string {
	payload := base64.RawURLEncoding.EncodeToString(value)
	mac := hmac.New(sha256.New, []byte(p.config.Secret))
	mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + signature
}

func (p *Plugin) verifySignedValue(signed string) ([]byte, bool) {
	dot := strings.LastIndexByte(signed, '.')
	if dot < 0 {
		return nil, false
	}
	payload := signed[:dot]
	signature, err := base64.RawURLEncoding.DecodeString(signed[dot+1:])
	if err != nil {
		return nil, false
	}
	for _, secret := range append([]string{p.config.Secret}, p.config.SecretFallbacks...) {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(payload))
		expected := mac.Sum(nil)
		if subtle.ConstantTimeCompare(signature, expected) == 1 {
			decoded, err := base64.RawURLEncoding.DecodeString(payload)
			return decoded, err == nil
		}
	}
	return nil, false
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p.config.SSLVerify != nil && !*p.config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return transport
}
