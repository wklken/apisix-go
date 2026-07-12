package authz_casdoor

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

	client   *http.Client
	sessions map[string]sessionData
	mu       sync.Mutex
	newState func() string
}

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
    }
  },
  "required": ["callback_url", "endpoint_addr", "client_id", "client_secret"]
}
`

type Config struct {
	EndpointAddr string `json:"endpoint_addr"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	CallbackURL  string `json:"callback_url"`
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
	if p.client == nil {
		p.client = &http.Client{Timeout: 10 * time.Second}
	}
	if p.sessions == nil {
		p.sessions = make(map[string]sessionData)
	}
	if p.newState == nil {
		p.newState = randomState
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == callbackPath(p.config.CallbackURL) {
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
		http.Error(w, util.BuildMessageResponse("invalid state"), http.StatusBadRequest)
		return
	}

	accessToken, lifetime, err := p.fetchAccessToken(r, code)
	if err != nil {
		http.Error(w, util.BuildMessageResponse(err.Error()), http.StatusServiceUnavailable)
		return
	}
	if session.OriginalURI == "" {
		http.Error(w, util.BuildMessageResponse("no original_url found in session"), http.StatusServiceUnavailable)
		return
	}

	session.AccessToken = accessToken
	session.ClientID = p.config.ClientID
	session.ExpiresAt = time.Now().Add(time.Duration(lifetime) * time.Second)
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
		ExpiresAt:   time.Now().Add(10 * time.Minute),
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
		time.Now().Before(session.ExpiresAt)
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
	defer resp.Body.Close()

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
	p.mu.Lock()
	defer p.mu.Unlock()

	session, ok := p.sessions[sessionID]
	if !ok || time.Now().After(session.ExpiresAt) {
		delete(p.sessions, sessionID)
		return sessionData{}, false
	}
	return session, true
}

func (p *Plugin) saveSession(sessionID string, session sessionData) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.sessions[sessionID] = session
}

func (p *Plugin) setSessionCookie(w http.ResponseWriter, sessionID string, lifetime time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     p.cookieName(),
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   int(lifetime.Seconds()),
	})
}

func (p *Plugin) cookieName() string {
	return "authz_casdoor_session_" + sha256Hex(p.config.ClientID)
}

func callbackPath(callbackURL string) string {
	parsed, err := url.Parse(callbackURL)
	if err != nil || !parsed.IsAbs() {
		return callbackURL
	}
	if parsed.Path == "" {
		return "/"
	}
	return parsed.Path
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func randomState() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(raw)
}
