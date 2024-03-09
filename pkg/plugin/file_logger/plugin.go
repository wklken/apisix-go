package file_logger

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	plugin_config "github.com/wklken/apisix-go/pkg/plugin/config"
	"go.uber.org/zap"
)

const (
	// version  = "0.1"
	priority = 103
	name     = "file_logger"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "level": {
		"type": "string"
	  },
	  "filename": {
		"type": "string"
	  }
	},
	"required": [
	  "level"
	]
  }
`

type Plugin struct {
	config Config
}

// FIXME: use jsonschema to unmarshal the config dynamic

type Config struct {
	Level    string `json:"level"`
	Filename string `json:"filename"`
}

func (p *Plugin) Name() string {
	return name
}

func (p *Plugin) Priority() int {
	return priority
}

func (p *Plugin) Schema() string {
	return schema
}

func (p *Plugin) Init(pc interface{}) error {
	// j, err := json.Marshal(pc)
	// if err != nil {
	// 	return err
	// }

	// var c Config
	// err = json.Unmarshal(j, &c)
	// if err != nil {
	// 	return err
	// }

	var c Config
	err := plugin_config.Parse(pc, &c)
	fmt.Printf("%s config before parse %+v, err=%+v\n", name, pc, err)

	p.config = c
	fmt.Printf("%s parsed config %+v\n", name, c)

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// TODO: path/filename/rotate
		// TODO: custom fields

		// logger, _ := zap.NewProduction()
		// logger := zap.NewExample()
		// defer logger.Sync()
		// ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		status := ww.Status()

		config := zap.NewProductionConfig()
		config.OutputPaths = []string{"stdout"}
		config.DisableCaller = true
		logger, _ := config.Build()

		fmt.Println("file_logger getting")

		// https://pkg.go.dev/go.uber.org/zap#hdr-Configuring_Zap

		fields := BuildLogFields(r, zap.Int("status", status))

		logger.Info("-",
			// Structured context as strongly typed Field values.
			fields...,
		)
	}
	return http.HandlerFunc(fn)
}

func BuildLogFields(r *http.Request, extraFields ...zap.Field) []zap.Field {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	ctx := r.Context()

	matchedURI := chi.RouteContext(ctx).RoutePattern()
	requestIDValue := ctx.Value("request_id")
	requestID := ""
	if requestIDValue != nil {
		requestID = requestIDValue.(string)
	}

	fields := []zap.Field{
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.String("remoteIP", r.RemoteAddr),
		zap.String("proto", r.Proto),
		zap.String("scheme", scheme),
		zap.String("requestID", requestID),
		zap.String("matchedURI", matchedURI),
	}
	for _, f := range extraFields {
		fields = append(fields, f)
	}

	return fields
}
