package request_id

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/gofrs/uuid"
	"github.com/spf13/viper"
)

const (
	// version  = "0.1"
	priority = 100
	name     = "request_id"
)

type Plugin struct {
	config Config
}

// FIXME: use jsonschema to unmarshal the config dynamic

type Config struct {
	HeaderName    string `mapstructure:"header_name"`
	SetInResponse bool   `mapstructure:"set_in_response"`
}

func (p *Plugin) Name() string {
	return name
}

func (p *Plugin) Priority() int {
	return priority
}

func (p *Plugin) Init(config string) error {
	fmt.Println("init the request_id plugin", config)
	v := viper.New()
	v.SetConfigType("json")

	// TODO: how to make the default value
	// v.SetDefault("header_name", "X-Request-ID")
	// v.SetDefault("set_in_response", true)

	v.ReadConfig(bytes.NewBuffer([]byte(config)))

	fmt.Println("config: ", v.AllSettings())

	var c Config
	err := v.Unmarshal(&c)
	if err != nil {
		return err
	}
	fmt.Printf("config: %+v\n", c)
	p.config = c

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		fmt.Println("call the Handler")

		requestID := r.Header.Get(p.config.HeaderName)
		if requestID == "" {
			requestID = uuid.Must(uuid.NewV4()).String()
		}

		fmt.Println("requestID: ", requestID)

		r.Header.Set(p.config.HeaderName, requestID)

		if p.config.SetInResponse {
			fmt.Println(
				"set the header in response",
			)
			w.Header().Set(p.config.HeaderName, requestID)
		}

		// ctx = plugin_ctx.WithPluginVar(ctx, name, "request_id", requestID)
		ctx = context.WithValue(ctx, "request_id", requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
	return http.HandlerFunc(fn)
}
