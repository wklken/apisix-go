package openid_connect

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"github.com/redis/go-redis/v9"
	"github.com/wklken/apisix-go/pkg/json"
	"net/http"
	"time"
)

type sessionData struct {
	RedisID           string `json:"-"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
	LastAuthenticated int64  `json:"last_authenticated,omitempty"`
	FlowState         string `json:"flow_state,omitempty"`
	FlowExpiresAt     int64  `json:"flow_expires_at,omitempty"`
	OriginalURI       string `json:"original_uri,omitempty"`
	CodeVerifier      string `json:"code_verifier,omitempty"`
	AccessToken       string `json:"access_token,omitempty"`
	IDToken           string `json:"id_token,omitempty"`
	RefreshToken      string `json:"refresh_token,omitempty"`
	Userinfo          string `json:"userinfo,omitempty"`
	ExpiresAt         int64  `json:"expires_at,omitempty"`
}

var errSessionNotFound = errors.New("openid-connect session not found")

type sessionStore interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string, time.Duration) error
	Delete(context.Context, string) error
}

type redisSessionStore struct {
	client *redis.Client
}

func (s *redisSessionStore) Get(ctx context.Context, key string) (string, error) {
	value, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", errSessionNotFound
	}
	return value, err
}

func (s *redisSessionStore) Set(ctx context.Context, key string, value string, ttl time.Duration) error {
	return s.client.Set(ctx, key, value, ttl).Err()
}

func (s *redisSessionStore) Delete(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
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
	if p.config.Session.Storage == "redis" {
		if p.sessionStore == nil {
			return nil, errors.New("openid-connect Redis session store is not configured")
		}
		redisID := string(payload)
		if redisID == "" {
			return nil, errSessionNotFound
		}
		payloadValue, err := p.sessionStore.Get(r.Context(), p.redisSessionKey(redisID))
		if err != nil {
			return nil, err
		}
		payload, err = p.openSession(payloadValue)
		if err != nil {
			return nil, err
		}
		var session sessionData
		if err := json.Unmarshal(payload, &session); err != nil {
			return nil, err
		}
		session.RedisID = redisID
		return &session, nil
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
	if p.config.Session.Storage == "redis" {
		if p.sessionStore == nil {
			return errors.New("openid-connect Redis session store is not configured")
		}
		redisID := session.RedisID
		if redisID == "" {
			redisID, err = randomURLValue(32)
			if err != nil {
				return err
			}
		}
		if err := p.sessionStore.Set(
			context.Background(),
			p.redisSessionKey(redisID),
			value,
			p.sessionStorageTTL(session),
		); err != nil {
			return err
		}
		value, err = p.sealSession([]byte(redisID))
		if err != nil {
			return err
		}
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

func (p *Plugin) clearSession(w http.ResponseWriter, session *sessionData) {
	if p.config.Session.Storage == "redis" && session != nil && session.RedisID != "" && p.sessionStore != nil {
		_ = p.sessionStore.Delete(context.Background(), p.redisSessionKey(session.RedisID))
	}
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

func (p *Plugin) redisSessionKey(redisID string) string {
	return p.config.Session.Redis.Prefix + ":" + redisID
}

func (p *Plugin) sessionStorageTTL(session sessionData) time.Duration {
	now := time.Now()
	var expiresAt time.Time
	if p.config.Session.AbsoluteTimeout > 0 {
		expiresAt = time.Unix(session.CreatedAt, 0).Add(time.Duration(p.config.Session.AbsoluteTimeout) * time.Second)
	}
	if p.config.Session.RollingTimeout > 0 {
		rollingExpiry := now.Add(time.Duration(p.config.Session.RollingTimeout) * time.Second)
		if expiresAt.IsZero() || rollingExpiry.Before(expiresAt) {
			expiresAt = rollingExpiry
		}
	}
	if p.config.Session.IdlingTimeout > 0 {
		idlingExpiry := now.Add(time.Duration(p.config.Session.IdlingTimeout) * time.Second)
		if expiresAt.IsZero() || idlingExpiry.Before(expiresAt) {
			expiresAt = idlingExpiry
		}
	}
	if expiresAt.IsZero() {
		return 0
	}
	if ttl := time.Until(expiresAt); ttl > 0 {
		return ttl
	}
	return time.Second
}

func (p *Plugin) sessionValid(session sessionData, now time.Time) bool {
	if session.AccessToken == "" {
		return false
	}
	if session.ExpiresAt > 0 && session.ExpiresAt <= now.Unix()+int64(p.config.AccessTokenExpiresLeeway) {
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

func (p *Plugin) sessionRefreshable(session sessionData, now time.Time) bool {
	if session.RefreshToken == "" || session.ExpiresAt == 0 {
		return false
	}
	if session.ExpiresAt > now.Unix()+int64(p.config.AccessTokenExpiresLeeway) {
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

func (p *Plugin) refreshSessionDue(session sessionData, now time.Time) bool {
	return p.config.RefreshSessionInterval != nil &&
		(session.LastAuthenticated == 0 || session.LastAuthenticated+int64(*p.config.RefreshSessionInterval) < now.Unix())
}

func (p *Plugin) tokenExpiresAt(now time.Time, expiresIn int64) int64 {
	if expiresIn <= 0 {
		expiresIn = int64(p.config.AccessTokenExpiresIn)
	}
	if expiresIn <= 0 {
		return 0
	}
	return now.Add(time.Duration(expiresIn) * time.Second).Unix()
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
