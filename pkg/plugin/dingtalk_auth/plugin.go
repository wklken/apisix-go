package dingtalk_auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	lifecycleMu sync.RWMutex
	client      *http.Client

	tokenMu    sync.Mutex
	tokenCache map[tokenCacheKey]tokenCacheEntry

	oauthStateReplay *base.OAuthStateReplayCache

	appSecret       secret.Value
	appSecretSet    bool
	legacyAppSecret *store.ResolvedSecret
	appSecretDigest [sha256.Size]byte

	sessionSecret          secret.Value
	sessionSecretSet       bool
	sessionSecretFallbacks []secret.Value
	legacySessionSecret    *store.ResolvedSecret
	legacySessionFallbacks []*store.ResolvedSecret

	secretsPrepared bool
	retired         bool
}

const (
	priority = 2430
	name     = "dingtalk-auth"

	defaultUserInfoURL    = "https://oapi.dingtalk.com/topapi/v2/user/getuserinfo"
	defaultAccessTokenURL = "https://api.dingtalk.com/v1.0/oauth2/accessToken"
	sessionCookieName     = "dingtalk_session"
	oauthStateCookieName  = "dingtalk_oauth_state"
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
      "type": "string"
    },
    "secret_fallbacks": {
      "type": "array",
      "items": {
        "type": "string"
      }
    },
    "cookie_expires_in": {
      "type": "integer",
      "minimum": 1,
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
	CookieSecure      *bool    `json:"cookie_secure,omitempty"`
	CookieSameSite    string   `json:"cookie_same_site,omitempty"`
}

type tokenCacheEntry struct {
	accessToken string
	expiresAt   time.Time
}

type tokenCacheKey struct {
	accessTokenURL string
	appKey         string
	appSecret      [sha256.Size]byte
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
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
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
	if p.tokenCache == nil {
		p.tokenCache = make(map[tokenCacheKey]tokenCacheEntry)
	}
	if p.oauthStateReplay == nil {
		p.oauthStateReplay = &base.OAuthStateReplayCache{}
	}
	return nil
}

// MaterializeScopedSecrets admits the exact DingTalk OAuth and session fields
// for one immutable generation. Public config is replaced only after every
// current and fallback value has resolved and passed its semantic contract.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return secret.ErrCredentialUnavailable
	}
	if p.secretsPrepared {
		return nil
	}

	appSecret, appDescriptor, err := materializeScopedDingTalkSecret(
		ctx, access, "app_secret", p.config.AppSecret, validateDingTalkAppSecret,
	)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	sessionSecret, sessionDescriptor, err := materializeScopedDingTalkSecret(
		ctx, access, "secret", p.config.Secret, validateDingTalkSessionSecret,
	)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	fallbacks := make([]secret.Value, len(p.config.SecretFallbacks))
	fallbackDescriptors := make([]string, len(p.config.SecretFallbacks))
	installed := false
	defer func() {
		if installed {
			return
		}
		appSecret = secret.Value{}
		sessionSecret = secret.Value{}
		for i := range fallbacks {
			fallbacks[i] = secret.Value{}
		}
	}()
	for i, raw := range p.config.SecretFallbacks {
		fallback, descriptor, materializeErr := materializeScopedDingTalkSecret(
			ctx, access, "secret_fallbacks", raw, validateDingTalkSessionSecret,
		)
		if materializeErr != nil {
			return secret.ErrCredentialUnavailable
		}
		fallbacks[i] = fallback
		fallbackDescriptors[i] = descriptor
	}

	p.appSecret = appSecret
	p.appSecretSet = true
	p.appSecretDigest = appSecret.Digest()
	p.sessionSecret = sessionSecret
	p.sessionSecretSet = true
	p.sessionSecretFallbacks = fallbacks
	p.config.AppSecret = appDescriptor
	p.config.Secret = sessionDescriptor
	p.config.SecretFallbacks = fallbackDescriptors
	p.secretsPrepared = true
	installed = true
	return nil
}

func materializeScopedDingTalkSecret(
	ctx context.Context,
	access base.ScopedSecretAccess,
	field string,
	raw string,
	validate func(string) error,
) (secret.Value, string, error) {
	value, err := access.Materialize(ctx, field, raw)
	if err != nil || value.Use(validate) != nil {
		return secret.Value{}, "", secret.ErrCredentialUnavailable
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return secret.Value{}, "", secret.ErrCredentialUnavailable
	}
	return value, descriptor.String(), nil
}

// MaterializeSecrets is the transitional process-local compatibility path.
// Immutable generation preparation uses MaterializeScopedSecrets instead.
func (p *Plugin) MaterializeSecrets() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return secret.ErrCredentialUnavailable
	}
	if p.secretsPrepared {
		return nil
	}

	appSecret, appDescriptor, appDigest, err := p.materializeLegacyDingTalkSecret(
		p.config.AppSecret, "app_secret", validateDingTalkAppSecret,
	)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	sessionSecret, sessionDescriptor, _, err := p.materializeLegacyDingTalkSecret(
		p.config.Secret, "secret", validateDingTalkSessionSecret,
	)
	if err != nil {
		appSecret.Destroy()
		return secret.ErrCredentialUnavailable
	}
	fallbacks := make([]*store.ResolvedSecret, len(p.config.SecretFallbacks))
	fallbackDescriptors := make([]string, len(p.config.SecretFallbacks))
	installed := false
	defer func() {
		if installed {
			return
		}
		appSecret.Destroy()
		sessionSecret.Destroy()
		for i, fallback := range fallbacks {
			if fallback != nil {
				fallback.Destroy()
				fallbacks[i] = nil
			}
		}
	}()
	for i, raw := range p.config.SecretFallbacks {
		fallback, descriptor, _, materializeErr := p.materializeLegacyDingTalkSecret(
			raw, "secret_fallbacks", validateDingTalkSessionSecret,
		)
		if materializeErr != nil {
			return secret.ErrCredentialUnavailable
		}
		fallbacks[i] = fallback
		fallbackDescriptors[i] = descriptor
	}

	p.legacyAppSecret = appSecret
	p.appSecretDigest = appDigest
	p.legacySessionSecret = sessionSecret
	p.legacySessionFallbacks = fallbacks
	p.config.AppSecret = appDescriptor
	p.config.Secret = sessionDescriptor
	p.config.SecretFallbacks = fallbackDescriptors
	p.secretsPrepared = true
	installed = true
	return nil
}

func (p *Plugin) materializeLegacyDingTalkSecret(
	raw string,
	field string,
	validate func(string) error,
) (*store.ResolvedSecret, string, [sha256.Size]byte, error) {
	if resolver := p.DataEncryption(); resolver.Configured() {
		raw = resolver.ResolveOptionalForContext(raw, name+"."+field)
	}
	value, err := store.MaterializeSecret(raw)
	if err != nil {
		return nil, "", [sha256.Size]byte{}, secret.ErrCredentialUnavailable
	}
	plaintext := value.Bytes()
	defer clear(plaintext)
	if err := validate(string(plaintext)); err != nil {
		value.Destroy()
		return nil, "", [sha256.Size]byte{}, secret.ErrCredentialUnavailable
	}
	digest := sha256.Sum256(plaintext)
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		value.Destroy()
		return nil, "", [sha256.Size]byte{}, secret.ErrCredentialUnavailable
	}
	return value, descriptor.String(), digest, nil
}

func validateDingTalkAppSecret(plaintext string) error {
	if strings.TrimSpace(plaintext) == "" {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func validateDingTalkSessionSecret(plaintext string) error {
	length := utf8.RuneCountInString(plaintext)
	if length < 8 || length > 32 {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.lifecycleMu.RLock()
		defer p.lifecycleMu.RUnlock()
		if p.retired || !p.secretsPrepared {
			http.Error(w, util.BuildMessageResponse("credential unavailable"), http.StatusServiceUnavailable)
			return
		}
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
	var sealed string
	err = p.useSessionSecretsLocked(func(current string, _ []string) error {
		var sealErr error
		sealed, sealErr = base.SealOAuthSession(
			[]byte(state), current, p.oauthStateFingerprint(), now, now.Add(base.OAuthStateLifetime),
		)
		return sealErr
	})
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
	var state []byte
	err = p.useSessionSecretsLocked(func(current string, fallbacks []string) error {
		var openErr error
		state, openErr = base.OpenOAuthSession(
			cookie.Value, current, fallbacks, p.oauthStateFingerprint(), now,
		)
		return openErr
	})
	stateValues, ok := r.URL.Query()["state"]
	if err != nil || !ok || len(stateValues) != 1 || stateValues[0] == "" ||
		subtle.ConstantTimeCompare(state, []byte(stateValues[0])) != 1 {
		return false
	}
	return p.oauthStateReplay != nil && p.oauthStateReplay.Consume(cookie.Value, now)
}

func (p *Plugin) oauthStateFingerprint() string {
	return base.Sha256Hex(strings.Join([]string{
		name,
		p.config.AppKey,
		p.config.AppSecret,
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

func (p *Plugin) accessToken(r *http.Request) (string, error) {
	cacheKey := tokenCacheKey{
		accessTokenURL: p.config.AccessTokenURL,
		appKey:         p.config.AppKey,
		appSecret:      p.appSecretDigest,
	}

	p.tokenMu.Lock()
	cached, ok := p.tokenCache[cacheKey]
	if ok && time.Now().Before(cached.expiresAt) {
		p.tokenMu.Unlock()
		return cached.accessToken, nil
	}
	p.tokenMu.Unlock()

	accessToken, err := p.fetchAccessToken(r)
	if err != nil {
		return "", err
	}

	p.tokenMu.Lock()
	p.tokenCache[cacheKey] = tokenCacheEntry{
		accessToken: accessToken,
		expiresAt:   time.Now().Add(tokenCacheTTL),
	}
	p.tokenMu.Unlock()
	return accessToken, nil
}

func (p *Plugin) fetchAccessToken(r *http.Request) (string, error) {
	var token string
	err := p.useAppSecretLocked(func(appSecret string) error {
		body, err := json.Marshal(map[string]string{
			"appKey": p.config.AppKey, "appSecret": appSecret,
		})
		if err != nil {
			return err
		}
		defer clear(body)
		req, err := http.NewRequestWithContext(
			r.Context(), http.MethodPost, p.config.AccessTokenURL, bytes.NewReader(body),
		)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.client.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected response status: %d", resp.StatusCode)
		}
		var response tokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return err
		}
		if response.AccessToken == "" {
			return fmt.Errorf("dingtalk token response missing accessToken")
		}
		token = response.AccessToken
		return nil
	})
	return token, err
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

	var payload []byte
	err = p.useSessionSecretsLocked(func(current string, fallbacks []string) error {
		var ok bool
		payload, ok = base.VerifySessionValue(cookie.Value, current, fallbacks)
		if !ok {
			return secret.ErrCredentialUnavailable
		}
		return nil
	})
	if err != nil {
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

	var value string
	if err := p.useSessionSecretsLocked(func(current string, _ []string) error {
		value = base.SignSessionValue(payload, current)
		return nil
	}); err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   *p.config.CookieSecure,
		SameSite: base.CookieSameSite(p.config.CookieSameSite),
		MaxAge:   p.config.CookieExpiresIn,
	}, nil
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p.config.SSLVerify != nil && !*p.config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return transport
}

func (p *Plugin) useAppSecretLocked(use func(string) error) error {
	if use == nil || p.retired || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.appSecretSet {
		return p.appSecret.Use(use)
	}
	if p.legacyAppSecret == nil {
		return secret.ErrCredentialUnavailable
	}
	plaintext := p.legacyAppSecret.Bytes()
	if len(plaintext) == 0 {
		return secret.ErrCredentialUnavailable
	}
	defer clear(plaintext)
	return use(string(plaintext))
}

func (p *Plugin) useSessionSecretsLocked(use func(string, []string) error) error {
	if use == nil || p.retired || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.sessionSecretSet {
		return p.sessionSecret.Use(func(current string) error {
			fallbacks := make([]string, len(p.sessionSecretFallbacks))
			defer clearStrings(fallbacks)
			return useScopedDingTalkFallbacks(p.sessionSecretFallbacks, fallbacks, 0, func() error {
				return use(current, fallbacks)
			})
		})
	}
	if p.legacySessionSecret == nil {
		return secret.ErrCredentialUnavailable
	}
	current := p.legacySessionSecret.Bytes()
	if len(current) == 0 {
		return secret.ErrCredentialUnavailable
	}
	defer clear(current)
	fallbackBytes := make([][]byte, len(p.legacySessionFallbacks))
	fallbacks := make([]string, len(p.legacySessionFallbacks))
	defer func() {
		for i := range fallbackBytes {
			clear(fallbackBytes[i])
		}
		clearStrings(fallbacks)
	}()
	for i, owner := range p.legacySessionFallbacks {
		if owner == nil {
			return secret.ErrCredentialUnavailable
		}
		fallbackBytes[i] = owner.Bytes()
		if len(fallbackBytes[i]) == 0 {
			return secret.ErrCredentialUnavailable
		}
		fallbacks[i] = string(fallbackBytes[i])
	}
	return use(string(current), fallbacks)
}

func useScopedDingTalkFallbacks(
	values []secret.Value,
	plaintext []string,
	index int,
	use func() error,
) error {
	if index == len(values) {
		return use()
	}
	return values[index].Use(func(value string) error {
		plaintext[index] = value
		defer func() { plaintext[index] = "" }()
		return useScopedDingTalkFallbacks(values, plaintext, index+1, use)
	})
}

func clearStrings(values []string) {
	for i := range values {
		values[i] = ""
	}
}

// Stop drains active handlers, closes the generation's idle connections,
// clears mutable caches, and then retires every private secret owner.
func (p *Plugin) Stop() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return
	}
	p.retired = true
	if p.client != nil {
		p.client.CloseIdleConnections()
		p.client = nil
	}
	p.tokenMu.Lock()
	clear(p.tokenCache)
	p.tokenCache = nil
	p.tokenMu.Unlock()
	p.oauthStateReplay = nil
	if p.legacyAppSecret != nil {
		p.legacyAppSecret.Destroy()
		p.legacyAppSecret = nil
	}
	if p.legacySessionSecret != nil {
		p.legacySessionSecret.Destroy()
		p.legacySessionSecret = nil
	}
	for i, fallback := range p.legacySessionFallbacks {
		if fallback != nil {
			fallback.Destroy()
		}
		p.legacySessionFallbacks[i] = nil
	}
	p.legacySessionFallbacks = nil
	p.appSecret = secret.Value{}
	p.appSecretSet = false
	p.appSecretDigest = [sha256.Size]byte{}
	p.sessionSecret = secret.Value{}
	p.sessionSecretSet = false
	for i := range p.sessionSecretFallbacks {
		p.sessionSecretFallbacks[i] = secret.Value{}
	}
	p.sessionSecretFallbacks = nil
	p.secretsPrepared = false
}
