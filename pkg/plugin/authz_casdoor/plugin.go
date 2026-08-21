package authz_casdoor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client   *http.Client
	newState func() (string, error)
	now      func() time.Time
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
	  "type": "string",
	  "minLength": 32
    },
	"client_secret_fallbacks": {
	  "type": "array",
	  "items": {"type": "string", "minLength": 32}
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
	if utf8.RuneCountInString(p.config.ClientSecret) < minSessionSecretLength {
		return fmt.Errorf("client_secret must contain at least %d characters", minSessionSecretLength)
	}
	for _, fallback := range p.config.ClientSecretFallbacks {
		if utf8.RuneCountInString(fallback) < minSessionSecretLength {
			return fmt.Errorf(
				"client_secret_fallbacks entries must contain at least %d characters",
				minSessionSecretLength,
			)
		}
	}
	if p.config.CookieSecure == nil {
		cookieSecure := true
		p.config.CookieSecure = &cookieSecure
	}
	if p.config.CookieSameSite == "" {
		p.config.CookieSameSite = "Lax"
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: 10 * time.Second}
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

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == base.CallbackPath(p.config.CallbackURL) {
			p.handleCallback(w, r)
			return
		}

		if p.authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}

		p.redirectToAuthorize(w, r)
	})
}

func (p *Plugin) handleCallback(w http.ResponseWriter, r *http.Request) {
	apisixctx.RegisterSensitiveQueryName(r, "code")
	session, err := p.openSession(r)
	if err != nil {
		logger.Error("no session found")
		http.Error(w, util.BuildMessageResponse("no session found"), http.StatusServiceUnavailable)
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

	accessToken, lifetime, err := p.fetchAccessToken(r, code)
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
	if err := p.setSessionCookie(w, session, time.Duration(lifetime)*time.Second); err != nil {
		logger.Error(err.Error())
		http.Error(w, util.BuildMessageResponse("failed to store session"), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, session.OriginalURI, http.StatusFound)
}

func (p *Plugin) redirectToAuthorize(w http.ResponseWriter, r *http.Request) {
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
	if err := p.setSessionCookie(w, sessionData{
		OriginalURI: r.URL.RequestURI(),
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

func (p *Plugin) authenticated(r *http.Request) bool {
	session, err := p.openSession(r)
	return err == nil &&
		session.AccessToken != "" &&
		session.ClientID == p.config.ClientID
}

func (p *Plugin) fetchAccessToken(r *http.Request, code string) (string, int, error) {
	values := url.Values{}
	values.Set("code", code)
	values.Set("grant_type", "authorization_code")
	values.Set("client_id", p.config.ClientID)
	values.Set("client_secret", p.config.ClientSecret)

	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodPost,
		strings.TrimRight(p.config.EndpointAddr, "/")+"/api/login/oauth/access_token",
		strings.NewReader(values.Encode()),
	)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	var token tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return "", 0, fmt.Errorf("failed to parse casdoor response data: %w", err)
	}
	if token.AccessToken == "" {
		return "", 0, errors.New("failed when accessing token: no access_token contained")
	}
	if token.ExpiresIn == 0 {
		return "", 0, errors.New("failed when accessing token: invalid access_token")
	}
	return token.AccessToken, token.ExpiresIn, nil
}

func (p *Plugin) openSession(r *http.Request) (sessionData, error) {
	value := cookieValue(r, p.cookieName())
	payload, err := base.OpenOAuthSession(
		value,
		p.config.ClientSecret,
		p.config.ClientSecretFallbacks,
		p.sessionFingerprint(),
		p.now(),
	)
	if err != nil {
		return sessionData{}, err
	}
	var session sessionData
	if err := json.Unmarshal(payload, &session); err != nil {
		return sessionData{}, err
	}
	return session, nil
}

func (p *Plugin) setSessionCookie(w http.ResponseWriter, session sessionData, lifetime time.Duration) error {
	payload, err := json.Marshal(session)
	if err != nil {
		return err
	}
	now := p.now()
	value, err := base.SealOAuthSession(
		payload,
		p.config.ClientSecret,
		p.sessionFingerprint(),
		now,
		now.Add(lifetime),
	)
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
