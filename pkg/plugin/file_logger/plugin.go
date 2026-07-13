package file_logger

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	  },
	  "log_format": {
		"type": "object"
	  },
	  "include_req_body": {
		"type": "boolean",
		"default": false
	  },
	  "include_req_body_expr": {
		"type": "array",
		"minItems": 1,
		"items": {
		  "type": "array"
		}
	  },
	  "include_resp_body": {
		"type": "boolean",
		"default": false
	  },
	  "include_resp_body_expr": {
		"type": "array",
		"minItems": 1,
		"items": {
		  "type": "array"
		}
	  },
	  "max_req_body_bytes": {
		"type": "integer",
		"minimum": 1,
		"default": 524288
	  },
	  "max_resp_body_bytes": {
		"type": "integer",
		"minimum": 1,
		"default": 524288
	  },
	  "match": {
		"type": "array",
		"maxItems": 20,
		"items": {
		  "anyOf": [
			{
			  "type": "array"
			},
			{
			  "type": "string"
			}
		  ]
		}
	  }
	}
  }
`

type pluginMetadata struct {
	Path      string            `json:"path"`
	LogFormat map[string]string `json:"log_format"`
}

type Plugin struct {
	base.BasePlugin
	config Config

	logger *zap.Logger

	logFormat map[string]string
}

type Config struct {
	Path                string            `json:"path"`
	LogFormat           map[string]string `json:"log_format,omitempty"`
	IncludeReqBody      bool              `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  []any             `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool              `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr []any             `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int               `json:"max_resp_body_bytes,omitempty"`
	Match               []any             `json:"match,omitempty"`
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if p.config.Path == "" {
		p.config.Path = metadata.Path
	}
	if p.config.Path == "" {
		return fmt.Errorf("file-logger path is not set in plugin config or metadata")
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}

	cfg := zap.NewProductionConfig()
	cfg.DisableCaller = true
	cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)

	enc := zapcore.NewJSONEncoder(cfg.EncoderConfig)

	syncWriter := &zapcore.BufferedWriteSyncer{
		WS:            &appendFileWriteSyncer{path: p.config.Path},
		Size:          4096,
		FlushInterval: 5 * time.Second,
	}

	p.logger = zap.New(
		zapcore.NewCore(enc, syncWriter, cfg.Level),
	)

	if len(p.config.LogFormat) == 0 {
		p.logFormat = metadata.LogFormat
	} else {
		p.logFormat = p.config.LogFormat
	}

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		var requestBody string
		if p.config.IncludeReqBody && base.ExprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := base.ReadAndRestoreRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *base.ResponseRecorder
		if p.config.IncludeRespBody {
			recorder = base.NewResponseRecorder(w, p.config.MaxRespBodyBytes)
			writer = recorder
		}

		next.ServeHTTP(writer, r)
		if !p.match(r) {
			return
		}
		status := 0
		if recorder != nil {
			status = recorder.StatusCode()
		}

		logFields := log.GetFields(r, p.logFormat)
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() && base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.Body()
		}

		fields := make([]zap.Field, 0, len(logFields))
		for k, v := range logFields {
			fields = append(fields, zap.Any(k, v))
		}

		p.logger.Info("", fields...)
	}
	return http.HandlerFunc(fn)
}

type appendFileWriteSyncer struct {
	path string
	mu   sync.Mutex
}

func (w *appendFileWriteSyncer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return 0, err
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()

	return file.Write(data)
}

func (w *appendFileWriteSyncer) Sync() error {
	return nil
}

func (p *Plugin) match(r *http.Request) bool {
	return base.ExprMatched(r, p.config.Match, 0)
}
