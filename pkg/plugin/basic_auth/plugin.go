package basic_auth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 2520
	name     = "basic-auth"
)

const schema = `
{
	"type": "object",
	"title": "work with route or service object",
	"properties": {
	  "hide_credentials": {
		"type": "boolean",
		"default": false
	  },
	  "realm": {
		"type": "string",
		"default": "basic"
	  },
	  "anonymous_consumer": {
		"type": "string",
		"minLength": 1
	  }
	}
}`

type Config struct {
	HideCredentials   *bool  `json:"hide_credentials"`
	Realm             string `json:"realm"`
	AnonymousConsumer string `json:"anonymous_consumer,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.HideCredentials == nil {
		hideCredentials := false
		p.config.HideCredentials = &hideCredentials
	}
	if p.config.Realm == "" {
		p.config.Realm = "basic"
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

type basicAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return base.AdaptRequestPhase(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx.AttachConsumerFromAuthenticationState(r)
		next.ServeHTTP(w, r)
	}))
}

func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	authHeader := r.Header.Get("Authorization")
	if *p.config.HideCredentials {
		r.Header.Del("Authorization")
	}
	if authHeader == "" {
		if result, ok := p.anonymousConsumerResult(w, r); ok {
			return result
		}
		p.writeAuthError(w, `{"message":"Missing authorization in request"}`)
		return base.StopRequest(r)
	}

	user, pass, err := parseBasicAuthorization(authHeader)
	if err != nil {
		if !ctx.RecordAuthProbeDiagnostic(r, err.Error()) {
			logger.Warn(err.Error())
		}
		if result, ok := p.anonymousConsumerResult(w, r); ok {
			return result
		}
		p.writeAuthError(w, `{"message":"Invalid authorization in request"}`)
		return base.StopRequest(r)
	}

	consumer, err := store.GetConsumerByPluginKey("basic-auth", user)
	if err != nil {
		ctx.RecordAuthProbeDiagnostic(r, "failed to find user: invalid user")
		if result, ok := p.anonymousConsumerResult(w, r); ok {
			return result
		}
		p.writeAuthError(w, `{"message":"Invalid user authorization"}`)
		return base.StopRequest(r)
	}
	logger.Info("find consumer " + consumer.Username)

	consumerPluginConfig, exists := consumer.Plugins["basic-auth"]
	if !exists {
		if result, ok := p.anonymousConsumerResult(w, r); ok {
			return result
		}
		p.writeAuthError(w, `{"message":"Missing authorization config in consumer settings"}`)
		return base.StopRequest(r)
	}

	var ba basicAuth
	err = util.Parse(consumerPluginConfig, &ba)
	if err != nil {
		if result, ok := p.anonymousConsumerResult(w, r); ok {
			return result
		}
		p.writeAuthError(w, `{"message":"Invalid authorization config in consumer settings"}`)
		return base.StopRequest(r)
	}

	if subtle.ConstantTimeCompare([]byte(pass), []byte(ba.Password)) != 1 {
		if result, ok := p.anonymousConsumerResult(w, r); ok {
			return result
		}
		p.writeAuthError(w, `{"message":"Invalid user authorization"}`)
		return base.StopRequest(r)
	}

	r = ctx.WithAuthenticationState(r, ctx.NewAuthenticationState(name, consumer))
	return base.ContinueRequest(r)
}

func (p *Plugin) anonymousConsumerResult(w http.ResponseWriter, r *http.Request) (base.RequestPhaseResult, bool) {
	if p.config.AnonymousConsumer == "" {
		return base.RequestPhaseResult{}, false
	}

	consumer, err := store.GetConsumer(p.config.AnonymousConsumer)
	if err != nil {
		message := fmt.Sprintf("failed to get anonymous consumer %s", p.config.AnonymousConsumer)
		if !ctx.RecordAuthProbeDiagnostic(r, message) {
			logger.Error(message)
		}
		p.writeAuthError(w, `{"message":"Invalid user authorization"}`)
		return base.StopRequest(r), true
	}

	return base.ContinueRequest(ctx.WithAuthenticationState(r, ctx.NewAuthenticationState(name, consumer))), true
}

func (p *Plugin) writeAuthError(w http.ResponseWriter, body string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s"`, p.config.Realm))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(body))
}

type authorizationError string

func (e authorizationError) Error() string {
	return string(e)
}

var (
	errInvalidBasicEncoding = errors.New("invalid Basic authorization encoding")
	errInvalidBasicValue    = errors.New("invalid Basic authorization value")
)

func parseBasicAuthorization(header string) (string, string, error) {
	scheme, encoded, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "basic") || encoded == "" {
		return "", "", authorizationError("Invalid authorization header format")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", errInvalidBasicEncoding
	}
	user, pass, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", "", errInvalidBasicValue
	}
	return normalizeBasicCredential(user), normalizeBasicCredential(pass), nil
}

func normalizeBasicCredential(value string) string {
	return strings.Map(func(char rune) rune {
		switch char {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			return -1
		}
		return char
	}, value)
}
