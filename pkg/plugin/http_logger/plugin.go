package http_logger

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-resty/resty/v2"
	c "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
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
	_ = resty.New()

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

	requestID := c.GetString(ctx, "request_id")
	routeID := c.GetString(ctx, "route_id")
	routeName := c.GetString(ctx, "route_name")
	serviceID := c.GetString(ctx, "service_id")

	fields := []zap.Field{
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.String("remoteIP", r.RemoteAddr),
		zap.String("proto", r.Proto),
		zap.String("scheme", scheme),
		zap.String("requestID", requestID),
		zap.String("matchedURI", matchedURI),
		zap.String("route_id", routeID),
		zap.String("route_name", routeName),
		zap.String("service_id", serviceID),
	}
	for _, f := range extraFields {
		fields = append(fields, f)
	}

	return fields
}

func Observe() {
	metrics.Requests.Inc()
	metrics.HostInfo.WithLabelValues("demo").Set(1)
}
