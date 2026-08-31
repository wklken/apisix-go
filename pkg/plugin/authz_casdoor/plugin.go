package authz_casdoor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/httpclient"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client   *http.Client
	newState func() (string, error)
	now      func() time.Time

	lifecycleMu           sync.RWMutex
	clientSecret          secret.Value
	clientSecretSet       bool
	clientSecretFallbacks []secret.Value
	secretsPrepared       bool
	retired               bool
}

const (
	priority               = 2559
	name                   = "authz-casdoor"
	minSessionSecretLength = 32
)

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint_addr": {
      "type": "string",
      "pattern": "^[^%?]+[^/]$"
    },
    "client_id": {
      "type": "string"
    },
    "client_secret": {
	  "type": "string"
    },
	"client_secret_fallbacks": {
	  "type": "array",
	  "items": {"type": "string"}
	},
    "callback_url": {
      "type": "string",
      "pattern": "^[^%?]+[^/]$"
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
  "required": ["callback_url", "endpoint_addr", "client_id", "client_secret"]
}
`

type Config struct {
	EndpointAddr          string   `json:"endpoint_addr"`
	ClientID              string   `json:"client_id"`
	ClientSecret          string   `json:"client_secret"`
	ClientSecretFallbacks []string `json:"client_secret_fallbacks,omitempty"`
	CallbackURL           string   `json:"callback_url"`
	CookieSecure          *bool    `json:"cookie_secure,omitempty"`
	CookieSameSite        string   `json:"cookie_same_site,omitempty"`
}

type sessionData struct {
	OriginalURI string `json:"original_uri,omitempty"`
	State       string `json:"state,omitempty"`
	AccessToken string `json:"access_token,omitempty"`
	ClientID    string `json:"client_id,omitempty"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
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
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.config.EndpointAddr)), "http://") {
		logger.Warn("Using authz-casdoor endpoint_addr with no TLS is a security risk")
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.config.CallbackURL)), "http://") {
		logger.Warn("Using authz-casdoor callback_url with no TLS is a security risk")
	}
	if p.config.CookieSecure == nil {
		cookieSecure := true
		p.config.CookieSecure = &cookieSecure
	}
	if p.config.CookieSameSite == "" {
		p.config.CookieSameSite = "Lax"
	}
	if p.client == nil {
		p.client = &http.Client{Transport: httpclient.NewTransport(), Timeout: 10 * time.Second}
	}
	if p.newState == nil {
		p.newState = func() (string, error) { return randomState(rand.Reader) }
	}
	if p.now == nil {
		p.now = time.Now
	}
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

// MaterializeScopedSecrets admits the current Casdoor client/session secret
// and every configured rotation fallback for one immutable generation. All
// owners and public descriptors are installed only after the last value has
// resolved and passed the session-key length contract.
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

	current, currentDescriptor, err := materializeScopedCasdoorSecret(
		ctx, access, "client_secret", p.config.ClientSecret,
	)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	fallbacks := make([]secret.Value, len(p.config.ClientSecretFallbacks))
	fallbackDescriptors := make([]string, len(p.config.ClientSecretFallbacks))
	installed := false
	defer func() {
		if installed {
			return
		}
		current = secret.Value{}
		for i := range fallbacks {
			fallbacks[i] = secret.Value{}
		}
	}()
	for i, raw := range p.config.ClientSecretFallbacks {
		fallback, descriptor, materializeErr := materializeScopedCasdoorSecret(
			ctx, access, "client_secret_fallbacks", raw,
		)
		if materializeErr != nil {
			return secret.ErrCredentialUnavailable
		}
		fallbacks[i] = fallback
		fallbackDescriptors[i] = descriptor
	}

	p.clientSecret = current
	p.clientSecretSet = true
	p.clientSecretFallbacks = fallbacks
	p.config.ClientSecret = currentDescriptor
	p.config.ClientSecretFallbacks = fallbackDescriptors
	p.secretsPrepared = true
	installed = true
	return nil
}

func materializeScopedCasdoorSecret(
	ctx context.Context,
	access base.ScopedSecretAccess,
	field string,
	raw string,
) (secret.Value, string, error) {
	value, err := access.Materialize(ctx, field, raw)
	if err != nil {
		return secret.Value{}, "", secret.ErrCredentialUnavailable
	}
	if err := value.Use(validateCasdoorSessionSecret); err != nil {
		return secret.Value{}, "", secret.ErrCredentialUnavailable
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return secret.Value{}, "", secret.ErrCredentialUnavailable
	}
	return value, descriptor.String(), nil
}

func validateCasdoorSessionSecret(plaintext string) error {
	if utf8.RuneCountInString(plaintext) < minSessionSecretLength {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originalURL := originalRequestURL(r)
		p.lifecycleMu.RLock()
		if originalURL.Path == base.CallbackPath(p.config.CallbackURL) {
			if p.retired {
				p.lifecycleMu.RUnlock()
				http.Error(w, util.BuildMessageResponse("credential unavailable"), http.StatusServiceUnavailable)
				return
			}
			p.handleCallbackLocked(w, r)
			p.lifecycleMu.RUnlock()
			return
		}

		if p.retired {
			p.lifecycleMu.RUnlock()
			http.Error(w, util.BuildMessageResponse("credential unavailable"), http.StatusServiceUnavailable)
			return
		}
		if p.authenticatedLocked(r) {
			p.lifecycleMu.RUnlock()
			next.ServeHTTP(w, r)
			return
		}

		p.redirectToAuthorizeLocked(w, r, originalURL.RequestURI())
		p.lifecycleMu.RUnlock()
	})
}

func (p *Plugin) handleCallbackLocked(w http.ResponseWriter, r *http.Request) {
	apisixctx.RegisterSensitiveQueryName(r, "code")
	session, err := p.openSessionLocked(r)
	if err != nil {
		logger.Error("no session found")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(
			w,
			util.BuildMessageResponse("failed when accessing token. Invalid code or state"),
			http.StatusBadRequest,
		)
		return
	}
	if state != session.State {
		logger.Error("invalid state")
		http.Error(w, util.BuildMessageResponse("invalid state"), http.StatusBadRequest)
		return
	}

	accessToken, lifetime, err := p.fetchAccessTokenLocked(r, code)
	if err != nil {
		logger.Error(err.Error())
		http.Error(w, util.BuildMessageResponse(err.Error()), http.StatusServiceUnavailable)
		return
	}
	if session.OriginalURI == "" {
		http.Error(w, util.BuildMessageResponse("no original_url found in session"), http.StatusServiceUnavailable)
		return
	}

	session.AccessToken = accessToken
	session.ClientID = p.config.ClientID
	if err := p.setSessionCookieLocked(w, session, time.Duration(lifetime)*time.Second); err != nil {
		logger.Error(err.Error())
		http.Error(w, util.BuildMessageResponse("failed to store session"), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, session.OriginalURI, http.StatusFound)
}

func (p *Plugin) redirectToAuthorizeLocked(w http.ResponseWriter, r *http.Request, originalURI string) {
	state, err := p.newState()
	if err != nil {
		logger.Error(err.Error())
		http.Error(
			w,
			util.BuildMessageResponse("failed to generate authorization state"),
			http.StatusInternalServerError,
		)
		return
	}
	if err := p.setSessionCookieLocked(w, sessionData{
		OriginalURI: originalURI,
		State:       state,
	}, 10*time.Minute); err != nil {
		logger.Error(err.Error())
		http.Error(w, util.BuildMessageResponse("failed to store session"), http.StatusInternalServerError)
		return
	}

	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("scope", "read")
	values.Set("state", state)
	values.Set("client_id", p.config.ClientID)
	values.Set("redirect_uri", p.config.CallbackURL)
	http.Redirect(
		w,
		r,
		strings.TrimRight(p.config.EndpointAddr, "/")+"/login/oauth/authorize?"+values.Encode(),
		http.StatusFound,
	)
}

func originalRequestURL(r *http.Request) *url.URL {
	if r.RequestURI != "" {
		if original, err := url.ParseRequestURI(r.RequestURI); err == nil {
			return original
		}
	}
	return r.URL
}

func (p *Plugin) authenticatedLocked(r *http.Request) bool {
	session, err := p.openSessionLocked(r)
	return err == nil &&
		session.AccessToken != "" &&
		session.ClientID == p.config.ClientID
}

func (p *Plugin) fetchAccessTokenLocked(r *http.Request, code string) (string, int, error) {
	if p.client == nil {
		return "", 0, secret.ErrCredentialUnavailable
	}
	var token tokenResponse
	err := p.useClientSecretLocked(func(clientSecret string) error {
		values := url.Values{}
		values.Set("code", code)
		values.Set("grant_type", "authorization_code")
		values.Set("client_id", p.config.ClientID)
		values.Set("client_secret", clientSecret)
		body := []byte(values.Encode())
		defer clear(body)

		req, err := http.NewRequestWithContext(
			r.Context(),
			http.MethodPost,
			strings.TrimRight(p.config.EndpointAddr, "/")+"/api/login/oauth/access_token",
			bytes.NewReader(body),
		)
		if err != nil {
			return err
		}
		defer func() {
			req.Body = http.NoBody
			req.GetBody = nil
		}()
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := p.client.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
			return fmt.Errorf("failed to parse casdoor response data: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	if token.AccessToken == "" {
		return "", 0, errors.New("failed when accessing token: no access_token contained")
	}
	if token.ExpiresIn == 0 {
		return "", 0, errors.New("failed when accessing token: invalid access_token")
	}
	return token.AccessToken, token.ExpiresIn, nil
}

func (p *Plugin) openSessionLocked(r *http.Request) (sessionData, error) {
	value := cookieValue(r, p.cookieName())
	var payload []byte
	err := p.useSessionSecretsLocked(func(current string, fallbacks []string) error {
		var openErr error
		payload, openErr = base.OpenOAuthSession(
			value,
			current,
			fallbacks,
			p.sessionFingerprint(),
			p.now(),
		)
		return openErr
	})
	if err != nil {
		return sessionData{}, err
	}
	var session sessionData
	if err := json.Unmarshal(payload, &session); err != nil {
		return sessionData{}, err
	}
	return session, nil
}

func (p *Plugin) setSessionCookieLocked(w http.ResponseWriter, session sessionData, lifetime time.Duration) error {
	payload, err := json.Marshal(session)
	if err != nil {
		return err
	}
	now := p.now()
	var value string
	err = p.useClientSecretLocked(func(current string) error {
		var sealErr error
		value, sealErr = base.SealOAuthSession(
			payload,
			current,
			p.sessionFingerprint(),
			now,
			now.Add(lifetime),
		)
		return sealErr
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     p.cookieName(),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   *p.config.CookieSecure,
		SameSite: base.CookieSameSite(p.config.CookieSameSite),
		MaxAge:   int(lifetime.Seconds()),
	})
	return nil
}

func (p *Plugin) useClientSecretLocked(use func(string) error) error {
	if use == nil || p.retired || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.clientSecretSet {
		return p.clientSecret.Use(use)
	}
	return secret.ErrCredentialUnavailable
}

func (p *Plugin) useSessionSecretsLocked(use func(string, []string) error) error {
	if use == nil || p.retired || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.clientSecretSet {
		return p.clientSecret.Use(func(current string) error {
			fallbacks := make([]string, len(p.clientSecretFallbacks))
			defer func() {
				for i := range fallbacks {
					fallbacks[i] = ""
				}
			}()
			return useScopedCasdoorFallbacks(p.clientSecretFallbacks, fallbacks, 0, func() error {
				return use(current, fallbacks)
			})
		})
	}
	return secret.ErrCredentialUnavailable
}

func useScopedCasdoorFallbacks(
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
		return useScopedCasdoorFallbacks(values, plaintext, index+1, use)
	})
}

// Stop waits for in-flight authentication/session callbacks, closes the
// generation-neutral client, then retires scoped secret owners.
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
	p.clientSecret = secret.Value{}
	p.clientSecretSet = false
	for i := range p.clientSecretFallbacks {
		p.clientSecretFallbacks[i] = secret.Value{}
	}
	p.clientSecretFallbacks = nil
	p.secretsPrepared = false
}

func (p *Plugin) sessionFingerprint() string {
	return base.Sha256Hex(strings.Join([]string{
		p.config.EndpointAddr,
		p.config.ClientID,
		p.config.CallbackURL,
	}, "\x00"))
}

func (p *Plugin) cookieName() string {
	return "authz_casdoor_session_" + base.Sha256Hex(p.config.ClientID)
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func randomState(reader io.Reader) (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", fmt.Errorf("generate random state: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
