package key_auth

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 2500
	name     = "key-auth"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "header": {
		"type": "string",
		"default": "apikey"
	  },
	  "query": {
		"type": "string",
		"default": "apikey"
	  },
	  "realm": {
		"type": "string",
		"default": "key",
		"minLength": 1,
		"maxLength": 128,
		"pattern": "^[\\x20-\\x21\\x23-\\x5B\\x5D-\\x7E]+$"
	  },
	  "hide_credentials": {
		"type": "boolean",
		"default": false
	  },
	  "anonymous_consumer": {
		"type": "string",
		"minLength": 1
	  }
	}
}`

type Config struct {
	Header            string `json:"header"`
	Query             string `json:"query"`
	Realm             string `json:"realm"`
	HideCredentials   *bool  `json:"hide_credentials"`
	AnonymousConsumer string `json:"anonymous_consumer,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Header == "" {
		p.config.Header = "apikey"
	}

	if p.config.Query == "" {
		p.config.Query = "apikey"
	}

	if p.config.Realm == "" {
		p.config.Realm = "key"
	}

	if p.config.HideCredentials == nil {
		hideCredentials := false
		p.config.HideCredentials = &hideCredentials
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return base.AdaptRequestPhase(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx.AttachConsumerFromAuthenticationState(r)
		next.ServeHTTP(w, r)
	}))
}

func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	ctx.RegisterSensitiveQueryName(r, p.config.Query)
	fromHeader := true
	key := ctx.RestoreTrustedRequestHeader(r, p.config.Header)
	if key == "" {
		key = r.URL.Query().Get(p.config.Query)
		fromHeader = false
	}

	if key == "" {
		if result, ok := p.anonymousConsumerResult(w, r); ok {
			return result
		}
		p.writeAuthError(w, http.StatusUnauthorized, `{"message":"Missing API key in request"}`)
		return base.StopRequest(r)
	}

	// note: here it's  unique key => consumer, it's different from basic-auth
	consumer, err := store.GetConsumerByPluginKey(name, key)
	if errors.Is(err, store.ErrNotFound) {
		if p.config.AnonymousConsumer != "" {
			p.hideAllCredentials(r)
			if result, ok := p.anonymousConsumerResult(w, r); ok {
				return result
			}
		}
		if !ctx.RecordAuthProbeDiagnostic(r, "Invalid API key in request") {
			logger.Warn("failed to find consumer: invalid api key")
		}
		p.writeAuthError(w, http.StatusUnauthorized, `{"message":"Invalid API key in request"}`)
		return base.StopRequest(r)
	}

	if err != nil {
		if !ctx.RecordAuthProbeDiagnostic(r, "failed to resolve API key consumer") {
			logger.Error("failed to resolve key-auth consumer")
		}
		p.writeAuthError(w, http.StatusUnauthorized, `{"message":"Invalid API key in request"}`)
		return base.StopRequest(r)
	}

	if *p.config.HideCredentials {
		if fromHeader {
			r.Header.Del(p.config.Header)
		} else {
			query := r.URL.Query()
			query.Del(p.config.Query)
			r.URL.RawQuery = query.Encode()
		}
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
		p.writeAuthError(w, http.StatusUnauthorized, `{"message":"Invalid user authorization"}`)
		return base.StopRequest(r), true
	}

	return base.ContinueRequest(ctx.WithAuthenticationState(r, ctx.NewAuthenticationState(name, consumer))), true
}

func (p *Plugin) writeAuthError(w http.ResponseWriter, status int, body string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`apikey realm="%s"`, p.config.Realm))
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (p *Plugin) hideAllCredentials(r *http.Request) {
	if !*p.config.HideCredentials {
		return
	}

	r.Header.Del(p.config.Header)
	query := r.URL.Query()
	query.Del(p.config.Query)
	r.URL.RawQuery = query.Encode()
}
