package basic_auth

import (
	"encoding/base64"
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
	fn := func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message":"Missing authorization in request"}`)
			return
		}

		user, pass, err := parseBasicAuthorization(authHeader)
		if err != nil {
			if !ctx.RecordAuthProbeDiagnostic(r, err.Error()) {
				logger.Warn(err.Error())
			}
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message":"Invalid authorization in request"}`)
			return
		}
		user = normalizeCredential(user)
		pass = normalizeCredential(pass)

		consumer, err := store.GetConsumerByPluginKey("basic-auth", user)
		if err != nil {
			ctx.RecordAuthProbeDiagnostic(r, "failed to find user: invalid user")
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message":"Invalid user authorization"}`)
			return
		}
		logger.Info("find consumer " + consumer.Username)

		consumerPluginConfig, exists := consumer.Plugins["basic-auth"]
		if !exists {
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message":"Missing authorization config in consumer settings"}`)
			return
		}

		var ba basicAuth
		err = util.Parse(consumerPluginConfig, &ba)
		if err != nil {
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message":"Invalid authorization config in consumer settings"}`)
			return
		}

		if pass != ba.Password {
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message":"Invalid user authorization"}`)
			return
		}

		if *p.config.HideCredentials {
			r.Header.Del("Authorization")
		}

		ctx.AttachConsumer(r, consumer)

		ctx.RunConsumerPlugins(w, r, next)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) attachAnonymousConsumer(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if p.config.AnonymousConsumer == "" {
		return false
	}

	consumer, err := store.GetConsumer(p.config.AnonymousConsumer)
	if err != nil {
		message := fmt.Sprintf("failed to get anonymous consumer %s", p.config.AnonymousConsumer)
		if !ctx.RecordAuthProbeDiagnostic(r, message) {
			logger.Error(message)
		}
		p.writeAuthError(w, `{"message":"Invalid user authorization"}`)
		return true
	}

	ctx.AttachConsumer(r, consumer)
	ctx.RunConsumerPlugins(w, r, next)
	return true
}

func (p *Plugin) writeAuthError(w http.ResponseWriter, body string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s"`, p.config.Realm))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(body))
}

func normalizeCredential(value string) string {
	return strings.Join(strings.Fields(value), "")
}

type authorizationError string

func (e authorizationError) Error() string {
	return string(e)
}

func parseBasicAuthorization(header string) (string, string, error) {
	scheme, encoded, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "basic") || encoded == "" {
		return "", "", authorizationError("Invalid authorization header format")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", authorizationError(fmt.Sprintf("Failed to decode authentication header: %s", encoded))
	}
	user, pass, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", "", authorizationError(fmt.Sprintf("Split authorization err: invalid decoded data: %s", decoded))
	}
	return user, pass, nil
}
