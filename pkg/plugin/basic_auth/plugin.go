package basic_auth

import (
	"errors"
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
	  }
	}
}`

type Config struct {
	HideCredentials *bool `json:"hide_credentials"`
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
			w.Header().Add("WWW-Authenticate", `Basic realm='.'`)
			writeError(w, `{"message":"Missing authorization in request"}`, http.StatusUnauthorized)
			return
		}

		user, pass, ok := r.BasicAuth()
		if !ok {
			writeError(w, `{"message":"Invalid authorization in request"}`, http.StatusUnauthorized)
			return
		}
		user = normalizeCredential(user)
		pass = normalizeCredential(pass)

		consumer, err := store.GetConsumerByPluginKey("basic-auth", user)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, `{"message":"Invalid user authorization"}`, http.StatusUnauthorized)
			return
		}

		consumerPluginConfig, exists := consumer.Plugins["basic-auth"]
		if !exists {
			writeError(w, `{"message":"Missing authorization config in consumer settings"}`, http.StatusUnauthorized)
			return
		}

		var ba basicAuth
		err = util.Parse(consumerPluginConfig, &ba)
		if err != nil {
			writeError(w, `{"message":"Invalid authorization config in consumer settings"}`, http.StatusUnauthorized)
			return
		}

		if pass != ba.Password {
			writeError(w, `{"message":"Invalid user authorization"}`, http.StatusUnauthorized)
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

func writeError(w http.ResponseWriter, body string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func normalizeCredential(value string) string {
	return strings.Join(strings.Fields(value), "")
}
