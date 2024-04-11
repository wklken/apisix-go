package file_logger

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/log"
	"github.com/wklken/apisix-go/pkg/store"
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
	  "path": {
		"type": "string"
	  }
	},
	"required": [
	  "path"
	]
  }
`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Plugin struct {
	base.BasePlugin
	config Config
}

type Config struct {
	Path string `json:"path"`
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
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
		var metadata pluginMetadata
		store.GetPluginMetadata("file-logger", &metadata)
		fmt.Printf("metadata: %+v\n", metadata)

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		// FIXME: status should be set in proxy, here we just get it from context
		status := ww.Status()

		config := zap.NewProductionConfig()
		config.OutputPaths = []string{"stdout"}
		config.DisableCaller = true
		logger, _ := config.Build()

		fmt.Println("file_logger getting")

		// https://pkg.go.dev/go.uber.org/zap#hdr-Configuring_Zap

		logFields := log.GetFields(r, []string{"method", "path", "remoteIP", "proto", "scheme", "request_id", "matched_uri", "route_id", "route_name", "service_id"})
		fields := make([]zap.Field, 0, len(logFields)+2)
		for k, v := range logFields {
			fields = append(fields, zap.String(k, v))
		}
		fields = append(fields, zap.Int("status", status))

		logger.Info("-",
			// Structured context as strongly typed Field values.
			fields...,
		)
	}
	return http.HandlerFunc(fn)
}

func Observe() {
	metrics.Requests.Inc()
	metrics.HostInfo.WithLabelValues("demo").Set(1)
}
