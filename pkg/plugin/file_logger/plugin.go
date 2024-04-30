package file_logger

import (
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"go.uber.org/zap"
)

const (
	// version  = "0.1"
	priority = 399
	name     = "file-logger"
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

	logger *zap.Logger

	logFormat map[string]string
}

type Config struct {
	Path      string            `json:"path"`
	LogFormat map[string]string `json:"log_format,omitempty"`
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
	cfg := zap.NewProductionConfig()
	cfg.DisableCaller = true
	cfg.OutputPaths = []string{p.config.Path}
	cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)

	// TODO: add buffered here?

	var logger *zap.Logger
	logger, _ = cfg.Build()
	p.logger = logger

	if p.config.LogFormat == nil || len(p.config.LogFormat) == 0 {
		var metadata pluginMetadata
		store.GetPluginMetadata("file-logger", &metadata)
		p.logFormat = metadata.LogFormat
	} else {
		p.logFormat = p.config.LogFormat
	}

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		fmt.Println("status:", ctx.GetRequestVar(r, "$status"))

		logFields := log.GetFields(r, p.logFormat)
		fields := make([]zap.Field, 0, len(logFields))
		for k, v := range logFields {
			fields = append(fields, zap.Any(k, v))
		}

		p.logger.Info("", fields...)
	}
	return http.HandlerFunc(fn)
}
