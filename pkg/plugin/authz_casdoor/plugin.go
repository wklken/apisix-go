package authz_casdoor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client   *http.Client
	sessions *cacheutil.BoundedTTLMap[sessionData]
	newState func() string
	now      func() time.Time

	cleanupStop chan struct{}
	cleanupDone chan struct{}
	stopOnce    sync.Once
}

// sessionCleanupInterval drives the periodic purge of expired sessions; tests
// shorten it to exercise cleanup with a fake clock.
var sessionCleanupInterval = time.Minute

// defaultMaxSessions bounds the number of concurrently live sessions; the
// earliest expiring sessions are evicted once the bound is hit.
const defaultMaxSessions = 10000

const (
	priority = 2559
	name     = "authz-casdoor"
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
	EndpointAddr   string `json:"endpoint_addr"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	CallbackURL    string `json:"callback_url"`
	CookieSecure   *bool  `json:"cookie_secure,omitempty"`
	CookieSameSite string `json:"cookie_same_site,omitempty"`
}

type sessionData struct {
	OriginalURI string
	State       string
	AccessToken string
	ClientID    string
	ExpiresAt   time.Time
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
	if p.sessions == nil {
		p.sessions = cacheutil.NewBoundedTTLMap[sessionData](
			defaultMaxSessions,
			func() time.Time { return p.now() },
		)
	}
	if p.newState == nil {
		p.newState = randomState
	}
	if p.now == nil {
		p.now = time.Now
	}
	p.startSessionCleanup()

	return nil
}

func (p *Plugin) startSessionCleanup() {
	if p.cleanupStop != nil {
		return
	}
	p.cleanupStop = make(chan struct{})
	p.cleanupDone = make(chan struct{})
	interval := sessionCleanupInterval
	go func() {
		defer close(p.cleanupDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.sessions.PurgeExpired()
			case <-p.cleanupStop:
				return
			}
		}
	}()
}

func (p *Plugin) Stop() {
	p.stopOnce.Do(func() {
		if p.cleanupStop != nil {
			close(p.cleanupStop)
			<-p.cleanupDone
			p.cleanupStop = nil
		}
	})
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
	sessionID := cookieValue(r, p.cookieName())
	session, ok := p.getSession(sessionID)
	if !ok {
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
	session.ExpiresAt = p.now().Add(time.Duration(lifetime) * time.Second)
	p.saveSession(sessionID, session)
	p.setSessionCookie(w, sessionID, time.Duration(lifetime)*time.Second)
	http.Redirect(w, r, session.OriginalURI, http.StatusFound)
}

func (p *Plugin) redirectToAuthorize(w http.ResponseWriter, r *http.Request) {
	sessionID := randomState()
	state := p.newState()
	p.saveSession(sessionID, sessionData{
		OriginalURI: r.URL.RequestURI(),
		State:       state,
		ExpiresAt:   p.now().Add(10 * time.Minute),
	})
	p.setSessionCookie(w, sessionID, 10*time.Minute)

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
	sessionID := cookieValue(r, p.cookieName())
	session, ok := p.getSession(sessionID)
	return ok &&
		session.AccessToken != "" &&
		session.ClientID == p.config.ClientID &&
		p.now().Before(session.ExpiresAt)
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

func (p *Plugin) getSession(sessionID string) (sessionData, bool) {
	if sessionID == "" {
		return sessionData{}, false
	}
	session, ok := p.sessions.Get(sessionID)
	if !ok {
		return sessionData{}, false
	}
	return session, true
}

func (p *Plugin) saveSession(sessionID string, session sessionData) {
	p.sessions.Set(sessionID, session, session.ExpiresAt.Sub(p.now()))
}

func (p *Plugin) setSessionCookie(w http.ResponseWriter, sessionID string, lifetime time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     p.cookieName(),
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   *p.config.CookieSecure,
		SameSite: base.CookieSameSite(p.config.CookieSameSite),
		MaxAge:   int(lifetime.Seconds()),
	})
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

func randomState() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(raw)
}
