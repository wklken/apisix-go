package basic_auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
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
			p.writeAuthError(w, `{"message": "Missing authorization in request"}`)
			return
		}

		user, pass, ok := r.BasicAuth()
		if !ok {
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message": "Invalid authorization in request"}`)
			return
		}
		user = normalizeCredential(user)
		pass = normalizeCredential(pass)

		consumer, err := store.GetConsumerByPluginKey("basic-auth", user)
		if errors.Is(err, store.ErrNotFound) {
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message": "Invalid user authorization"}`)
			return
		}

		consumerPluginConfig, exists := consumer.Plugins["basic-auth"]
		if !exists {
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message": "Missing authorization config in consumer settings"}`)
			return
		}

		var ba basicAuth
		err = util.Parse(consumerPluginConfig, &ba)
		if err != nil {
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message": "Invalid authorization config in consumer settings"}`)
			return
		}

		if pass != ba.Password {
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			p.writeAuthError(w, `{"message": "Invalid user authorization"}`)
			return
		}

		if *p.config.HideCredentials {
			r.Header.Del("Authorization")
		}

		ctx.AttachConsumer(r, consumer)

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) attachAnonymousConsumer(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if p.config.AnonymousConsumer == "" {
		return false
	}

	consumer, err := store.GetConsumer(p.config.AnonymousConsumer)
	if err != nil {
		p.writeAuthError(w, `{"message": "Invalid user authorization"}`)
		return true
	}

	ctx.AttachConsumer(r, consumer)
	next.ServeHTTP(w, r)
	return true
}

func (p *Plugin) writeAuthError(w http.ResponseWriter, body string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s"`, p.config.Realm))
	http.Error(w, body, http.StatusUnauthorized)
}

func normalizeCredential(value string) string {
	return strings.Join(strings.Fields(value), "")
}
