package cas_auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client *http.Client
}

const (
	priority = 2597
	name     = "cas-auth"

	requestURICookie = "CAS_REQUEST_URI"
	sessionPrefix    = "CAS_SESSION_"
	sessionLifetime  = time.Hour
)

const schema = `
{
  "type": "object",
  "properties": {
    "idp_uri": {
      "type": "string"
    },
    "cas_callback_uri": {
      "type": "string"
    },
    "logout_uri": {
      "type": "string"
    },
    "cookie": {
      "type": "object",
      "properties": {
        "secret": {
          "type": "string",
          "minLength": 32
        },
        "secure": {
          "type": "boolean",
          "default": true
        },
        "samesite": {
          "type": "string",
          "enum": ["Lax", "None"],
          "default": "Lax"
        }
      },
      "required": ["secret"]
    }
  },
  "required": ["idp_uri", "cas_callback_uri", "logout_uri", "cookie"],
  "allOf": [
    {
      "not": {
        "properties": {
          "cookie": {
            "properties": {
              "samesite": {"const": "None"},
              "secure": {"const": false}
            },
            "required": ["samesite", "secure"]
          }
        },
        "required": ["cookie"]
      }
    }
  ]
}
`

type Config struct {
	IDPURI         string       `json:"idp_uri"`
	CASCallbackURI string       `json:"cas_callback_uri"`
	LogoutURI      string       `json:"logout_uri"`
	Cookie         CookieConfig `json:"cookie"`
}

type CookieConfig struct {
	Secret   string `json:"secret"`
	Secure   *bool  `json:"secure,omitempty"`
	SameSite string `json:"samesite,omitempty"`
}

type sessionEntry struct {
	fingerprint string
	user        string
	expiresAt   time.Time
}

type sessionOptions struct {
	cookieName  string
	fingerprint string
}

var processSessions = struct {
	sync.Mutex
	entries map[string]sessionEntry
}{entries: make(map[string]sessionEntry)}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Cookie.Secure == nil {
		secure := true
		p.config.Cookie.Secure = &secure
	}
	if p.config.Cookie.SameSite == "" {
		p.config.Cookie.SameSite = "Lax"
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: 10 * time.Second}
	}
	if parsed, err := url.Parse(p.config.IDPURI); err == nil && parsed.Scheme == "http" {
		logger.Warn("Using cas-auth idp_uri with no TLS is a security risk")
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == p.config.LogoutURI {
			p.logout(w, r)
			return
		}

		opts := p.sessionOptions()
		if sessionID := cookieValue(r, opts.cookieName); sessionID != "" {
			if p.refreshSession(sessionID) {
				next.ServeHTTP(w, r)
				return
			}
			p.deleteCookie(w, opts.cookieName)
			p.firstAccess(w, r)
			return
		}

		if r.Method == http.MethodGet && r.URL.Path == callbackPath(p.config.CASCallbackURI) &&
			r.URL.Query().Get("ticket") != "" {
			p.validateWithCAS(w, r, r.URL.Query().Get("ticket"))
			return
		}

		if r.Method == http.MethodPost && r.URL.Path == callbackPath(p.config.CASCallbackURI) {
			if p.handleIDPLogout(r) {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Error(
				w,
				util.BuildMessageResponse("invalid logout request from IdP, no ticket"),
				http.StatusBadRequest,
			)
			return
		}

		p.firstAccess(w, r)
	})
}

func (p *Plugin) handleIDPLogout(r *http.Request) bool {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		start, ok := token.(xml.StartElement)
		if !ok || localXMLName(start.Name) != "SessionIndex" {
			continue
		}

		var sessionID string
		if err := decoder.DecodeElement(&sessionID, &start); err != nil || sessionID == "" {
			return false
		}
		p.deleteSession(sessionID)
		return true
	}
}

func (p *Plugin) firstAccess(w http.ResponseWriter, r *http.Request) {
	originalURI := r.URL.RequestURI()
	signed, err := p.signValue(originalURI)
	if err == nil {
		p.setCookie(w, requestURICookie, signed)
	}

	values := url.Values{}
	values.Set("service", p.serviceURL(r))
	http.Redirect(w, r, strings.TrimRight(p.config.IDPURI, "/")+"/login?"+values.Encode(), http.StatusFound)
}

func (p *Plugin) validateWithCAS(w http.ResponseWriter, r *http.Request, ticket string) {
	requestURI := p.verifyValue(cookieValue(r, requestURICookie))
	if !safeRedirect(requestURI) {
		http.Error(w, util.BuildMessageResponse("invalid callback state"), http.StatusUnauthorized)
		return
	}

	user, err := p.validateTicket(r, ticket)
	if err != nil || user == "" {
		http.Error(w, util.BuildMessageResponse("invalid ticket"), http.StatusUnauthorized)
		return
	}

	p.storeSession(ticket, user)
	p.setCookie(w, p.sessionOptions().cookieName, ticket)
	p.deleteCookie(w, requestURICookie)
	http.Redirect(w, r, requestURI, http.StatusFound)
}

func (p *Plugin) logout(w http.ResponseWriter, r *http.Request) {
	opts := p.sessionOptions()
	sessionID := cookieValue(r, opts.cookieName)
	if sessionID == "" {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}

	p.deleteSession(sessionID)

	p.deleteCookie(w, opts.cookieName)
	http.Redirect(w, r, strings.TrimRight(p.config.IDPURI, "/")+"/logout", http.StatusFound)
}

func (p *Plugin) validateTicket(r *http.Request, ticket string) (string, error) {
	values := url.Values{}
	values.Set("ticket", ticket)
	values.Set("service", p.serviceURL(r))

	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodGet,
		strings.TrimRight(p.config.IDPURI, "/")+"/serviceValidate?"+values.Encode(),
		nil,
	)
	if err != nil {
		return "", err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("CAS serviceValidate returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return parseCASUser(body), nil
}

func (p *Plugin) serviceURL(r *http.Request) string {
	if isAbsoluteCallback(p.config.CASCallbackURI) {
		return p.config.CASCallbackURI
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host, requestPort := splitRequestHost(r.Host)
	port := requestPort
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, listenerPort, err := net.SplitHostPort(local.String()); err == nil {
			port = listenerPort
		}
	}
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port) + p.config.CASCallbackURI
}

func (p *Plugin) sessionOptions() sessionOptions {
	fingerprint := sha256Hex(p.config.IDPURI + "|" + p.config.CASCallbackURI)
	return sessionOptions{
		cookieName:  sessionPrefix + fingerprint,
		fingerprint: fingerprint,
	}
}

func (p *Plugin) storeSession(sessionID string, user string) {
	processSessions.Lock()
	defer processSessions.Unlock()

	processSessions.entries[p.sessionKey(sessionID)] = sessionEntry{
		fingerprint: p.sessionOptions().fingerprint,
		user:        user,
		expiresAt:   time.Now().Add(sessionLifetime),
	}
}

func (p *Plugin) refreshSession(sessionID string) bool {
	processSessions.Lock()
	defer processSessions.Unlock()

	key := p.sessionKey(sessionID)
	entry, ok := processSessions.entries[key]
	if !ok || entry.fingerprint != p.sessionOptions().fingerprint || time.Now().After(entry.expiresAt) {
		delete(processSessions.entries, key)
		return false
	}
	entry.expiresAt = time.Now().Add(sessionLifetime)
	processSessions.entries[key] = entry
	return true
}

func (p *Plugin) deleteSession(sessionID string) {
	processSessions.Lock()
	delete(processSessions.entries, p.sessionKey(sessionID))
	processSessions.Unlock()
}

func (p *Plugin) sessionKey(sessionID string) string {
	return p.sessionOptions().fingerprint + ":" + sessionID
}

func (p *Plugin) setCookie(w http.ResponseWriter, name string, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   p.config.Cookie.Secure == nil || *p.config.Cookie.Secure,
		SameSite: sameSiteMode(p.config.Cookie.SameSite),
	})
}

func (p *Plugin) deleteCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "deleted",
		Path:     "/",
		HttpOnly: true,
		Secure:   p.config.Cookie.Secure == nil || *p.config.Cookie.Secure,
		SameSite: sameSiteMode(p.config.Cookie.SameSite),
		MaxAge:   -1,
	})
}

func (p *Plugin) signValue(value string) (string, error) {
	mac := hmac.New(sha256.New, []byte(p.config.Cookie.Secret))
	if _, err := mac.Write([]byte(value)); err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(value))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + signature, nil
}

func (p *Plugin) verifyValue(signed string) string {
	dot := strings.Index(signed, ".")
	if dot <= 0 {
		return ""
	}
	value, err := base64.RawURLEncoding.DecodeString(signed[:dot])
	if err != nil {
		return ""
	}
	signature, err := base64.RawURLEncoding.DecodeString(signed[dot+1:])
	if err != nil {
		return ""
	}

	mac := hmac.New(sha256.New, []byte(p.config.Cookie.Secret))
	mac.Write(value)
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(signature, expected) != 1 {
		return ""
	}
	return string(value)
}

func parseCASUser(body []byte) string {
	decoder := xml.NewDecoder(strings.NewReader(string(body)))
	inSuccess := false
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		switch value := token.(type) {
		case xml.StartElement:
			name := localXMLName(value.Name)
			if name == "authenticationSuccess" {
				inSuccess = true
			}
			if inSuccess && name == "user" {
				var user string
				if err := decoder.DecodeElement(&user, &value); err != nil {
					return ""
				}
				return user
			}
		case xml.EndElement:
			if localXMLName(value.Name) == "authenticationSuccess" {
				inSuccess = false
			}
		}
	}
}

func callbackPath(callbackURI string) string {
	parsed, err := url.Parse(callbackURI)
	if err != nil || !parsed.IsAbs() {
		return callbackURI
	}
	if parsed.Path == "" {
		return "/"
	}
	return parsed.Path
}

func isAbsoluteCallback(callbackURI string) bool {
	parsed, err := url.Parse(callbackURI)
	return err == nil && parsed.IsAbs()
}

func safeRedirect(uri string) bool {
	if uri == "" || !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") {
		return false
	}
	return !strings.ContainsAny(uri, "\\\r\n")
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func splitRequestHost(hostport string) (string, string) {
	host, port, err := net.SplitHostPort(hostport)
	if err == nil {
		return host, port
	}
	return strings.Trim(hostport, "[]"), ""
}

func sameSiteMode(value string) http.SameSite {
	switch value {
	case "None":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}

func localXMLName(name xml.Name) string {
	if name.Local != "" {
		return name.Local
	}
	return name.Space
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
