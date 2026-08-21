package feishu_auth

import (
	"bytes"
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client           *http.Client
	oauthStateReplay base.OAuthStateReplayCache
}

const (
	priority = 2420
	name     = "feishu-auth"

	defaultAccessTokenURL = "https://open.feishu.cn/open-apis/authen/v2/oauth/token"
	defaultUserInfoURL    = "https://open.feishu.cn/open-apis/authen/v1/user_info"
	sessionCookieName     = "feishu_session"
	oauthStateCookieName  = "feishu_oauth_state"
)

const schema = `
{
  "type": "object",
  "properties": {
    "app_id": {
      "type": "string",
      "minLength": 1
    },
    "app_secret": {
      "type": "string",
      "minLength": 1
    },
    "code_header": {
      "type": "string",
      "default": "X-Feishu-Code"
    },
    "code_query": {
      "type": "string",
      "default": "code"
    },
    "userinfo_url": {
      "type": "string",
      "default": "https://open.feishu.cn/open-apis/authen/v1/user_info"
    },
    "access_token_url": {
      "type": "string",
      "default": "https://open.feishu.cn/open-apis/authen/v2/oauth/token"
    },
    "set_userinfo_header": {
      "type": "boolean",
      "default": true
    },
    "auth_redirect_uri": {
      "type": "string"
    },
    "redirect_uri": {
      "type": "string"
    },
    "timeout": {
      "type": "integer",
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
      "default": 86400
    },
    "cookie_secure": {
      "type": "boolean",
      "default": true
    },
    "cookie_same_site": {
      "type": "string",
      "enum": ["Default", "Lax", "Strict", "None"],
      "default": "Lax"
    }
  },
  "allOf": [
    {
      "anyOf": [
        {
          "not": {
            "properties": {"cookie_same_site": {"enum": ["None"]}},
            "required": ["cookie_same_site"]
          }
        },
        {
          "properties": {"cookie_secure": {"enum": [true]}},
          "required": ["cookie_secure"]
        }
      ]
    }
  ],
  "required": ["app_id", "app_secret", "secret", "auth_redirect_uri", "redirect_uri"]
}
`

type Config struct {
	AppID             string   `json:"app_id"`
	AppSecret         string   `json:"app_secret"`
	CodeHeader        string   `json:"code_header,omitempty"`
	CodeQuery         string   `json:"code_query,omitempty"`
	UserInfoURL       string   `json:"userinfo_url,omitempty"`
	AccessTokenURL    string   `json:"access_token_url,omitempty"`
	SetUserInfoHeader *bool    `json:"set_userinfo_header,omitempty"`
	AuthRedirectURI   string   `json:"auth_redirect_uri"`
	RedirectURI       string   `json:"redirect_uri"`
	Timeout           int      `json:"timeout,omitempty"`
	SSLVerify         *bool    `json:"ssl_verify,omitempty"`
	Secret            string   `json:"secret"`
	SecretFallbacks   []string `json:"secret_fallbacks,omitempty"`
	CookieExpiresIn   int      `json:"cookie_expires_in,omitempty"`
	CookieSecure      *bool    `json:"cookie_secure,omitempty"`
	CookieSameSite    string   `json:"cookie_same_site,omitempty"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

type userInfoResponse struct {
	Code int            `json:"code"`
	Msg  string         `json:"msg"`
	Data map[string]any `json:"data"`
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
		p.config.CodeHeader = "X-Feishu-Code"
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
	if p.config.CookieSecure == nil {
		cookieSecure := true
		p.config.CookieSecure = &cookieSecure
	}
	if p.config.CookieSameSite == "" {
		p.config.CookieSameSite = "Lax"
	}
	if p.client == nil {
		p.client = &http.Client{
			Timeout:   time.Duration(p.config.Timeout) * time.Millisecond,
			Transport: p.transport(),
		}
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("X-Userinfo")

		if userinfo, ok := p.userInfoFromSession(r); ok {
			base.AttachExternalUser(r, userinfo, p.config.SetUserInfoHeader)
			next.ServeHTTP(w, r)
			return
		}

		code := base.CodeFromRequest(r, p.config.CodeHeader, p.config.CodeQuery)
		if r.Header.Get(p.config.CodeHeader) == "" {
			if code == "" {
				p.redirectToProvider(w, r)
				return
			}
			if !p.verifyAndConsumeOAuthState(r) {
				http.Error(w, util.BuildMessageResponse("Invalid OAuth state"), http.StatusUnauthorized)
				return
			}
			p.deleteOAuthStateCookie(w)
		}

		accessToken, err := p.fetchAccessToken(r, code)
		if err != nil {
			http.Error(w, util.BuildMessageResponse("Invalid authorization code"), http.StatusUnauthorized)
			return
		}

		userinfo, err := p.fetchUserInfo(r, accessToken)
		if err != nil {
			http.Error(w, util.BuildMessageResponse("Invalid authorization code"), http.StatusUnauthorized)
			return
		}

		cookie, err := p.sessionCookie(userinfo)
		if err != nil {
			http.Error(w, util.BuildMessageResponse("Invalid userinfo"), http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, cookie)
		base.AttachExternalUser(r, userinfo, p.config.SetUserInfoHeader)
		next.ServeHTTP(w, r)
	})
}

func (p *Plugin) redirectToProvider(w http.ResponseWriter, r *http.Request) {
	state, err := base.NewOAuthState()
	if err != nil {
		http.Error(w, util.BuildMessageResponse("Failed to create OAuth state"), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	sealed, err := base.SealOAuthSession(
		[]byte(state),
		p.config.Secret,
		p.oauthStateFingerprint(),
		now,
		now.Add(base.OAuthStateLifetime),
	)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("Failed to create OAuth state"), http.StatusInternalServerError)
		return
	}
	redirectURI, err := oauthRedirectWithState(p.config.RedirectURI, state)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("Invalid OAuth redirect URI"), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, p.oauthStateCookie(sealed))
	http.Redirect(w, r, redirectURI, http.StatusFound)
}

func (p *Plugin) verifyAndConsumeOAuthState(r *http.Request) bool {
	cookie, err := r.Cookie(oauthStateCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := time.Now()
	state, err := base.OpenOAuthSession(
		cookie.Value,
		p.config.Secret,
		p.config.SecretFallbacks,
		p.oauthStateFingerprint(),
		now,
	)
	stateValues, ok := r.URL.Query()["state"]
	if err != nil || !ok || len(stateValues) != 1 || stateValues[0] == "" ||
		subtle.ConstantTimeCompare(state, []byte(stateValues[0])) != 1 {
		return false
	}
	return p.oauthStateReplay.Consume(cookie.Value, now)
}

func (p *Plugin) oauthStateFingerprint() string {
	return base.Sha256Hex(strings.Join([]string{
		name,
		p.config.AppID,
		p.config.AppSecret,
		p.config.AuthRedirectURI,
		p.config.RedirectURI,
		p.config.AccessTokenURL,
		p.config.UserInfoURL,
		p.config.CodeHeader,
		p.config.CodeQuery,
	}, "\x00"))
}

func (p *Plugin) oauthStateCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   *p.config.CookieSecure,
		SameSite: base.CookieSameSite(p.config.CookieSameSite),
		MaxAge:   int(base.OAuthStateLifetime / time.Second),
	}
}

func (p *Plugin) deleteOAuthStateCookie(w http.ResponseWriter) {
	cookie := p.oauthStateCookie("deleted")
	cookie.MaxAge = -1
	cookie.Expires = time.Unix(1, 0).UTC()
	http.SetCookie(w, cookie)
}

func oauthRedirectWithState(rawURI string, state string) (string, error) {
	redirectURI, err := url.Parse(rawURI)
	if err != nil {
		return "", err
	}
	query := redirectURI.Query()
	query.Set("state", state)
	redirectURI.RawQuery = query.Encode()
	return redirectURI.String(), nil
}

func (p *Plugin) fetchAccessToken(r *http.Request, code string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     p.config.AppID,
		"client_secret": p.config.AppSecret,
		"redirect_uri":  p.config.AuthRedirectURI,
		"code":          code,
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
	if token.AccessToken == "" || token.ExpiresIn == 0 {
		return "", fmt.Errorf("missing access_token or expires_in in response")
	}
	return token.AccessToken, nil
}

func (p *Plugin) fetchUserInfo(r *http.Request, accessToken string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.config.UserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected http response status: %d", resp.StatusCode)
	}

	var userinfo userInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&userinfo); err != nil {
		return nil, err
	}
	if userinfo.Code != 0 {
		return nil, fmt.Errorf("unexpected error code: %d, errmsg: %s", userinfo.Code, userinfo.Msg)
	}
	if userinfo.Data == nil {
		return nil, fmt.Errorf("feishu userinfo response missing data")
	}
	return userinfo.Data, nil
}

func (p *Plugin) userInfoFromSession(r *http.Request) (map[string]any, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, false
	}

	payload, ok := base.VerifySessionValue(cookie.Value, p.config.Secret, p.config.SecretFallbacks)
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
		Value:    base.SignSessionValue(payload, p.config.Secret),
		Path:     "/",
		HttpOnly: true,
		Secure:   *p.config.CookieSecure,
		SameSite: base.CookieSameSite(p.config.CookieSameSite),
		MaxAge:   p.config.CookieExpiresIn,
	}, nil
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	ai_common.ApplyTransportSSLVerify(transport, p.config.SSLVerify)
	return transport
}
