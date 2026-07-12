package server_info

import (
	stdjson "encoding/json"
	"net/http"
	"os"
	"time"

	apisixid "github.com/wklken/apisix-go/pkg/apisix/id"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 990
	name     = "server-info"
	version  = "apisix-go"

	defaultReportTTL = 60 * time.Second
	minReportTTL     = 3 * time.Second
	maxReportTTL     = 24 * time.Hour
)

const schema = `{"type":"object"}`

var bootTime = time.Now().Unix()

type Config struct{}

type Response struct {
	EtcdVersion string `json:"etcd_version"`
	Hostname    string `json:"hostname"`
	ID          string `json:"id"`
	Version     string `json:"version"`
	BootTime    int64  `json:"boot_time"`
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

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func CurrentInfo() Response {
	hostname := hostname()
	return Response{
		EtcdVersion: "unknown",
		Hostname:    hostname,
		ID:          apisixid.Get(),
		Version:     version,
		BootTime:    bootTime,
	}
}

func ReportTTL() time.Duration {
	ttl := defaultReportTTL
	if config.GlobalConfig == nil || config.GlobalConfig.PluginAttr == nil {
		return ttl
	}
	attr := config.GlobalConfig.PluginAttr[name]
	if attr == nil {
		return ttl
	}
	value, ok := reportTTLValue(attr["report_ttl"])
	if !ok {
		return ttl
	}
	ttl = time.Duration(value) * time.Second
	if ttl < minReportTTL {
		return minReportTTL
	}
	if ttl > maxReportTTL {
		return maxReportTTL
	}
	return ttl
}

func reportTTLValue(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), true
	case uint8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		if v > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(v), true
	case float32:
		return int64(v), float32(int64(v)) == v
	case float64:
		return int64(v), float64(int64(v)) == v
	case stdjson.Number:
		parsed, err := v.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func InfoHandler(w http.ResponseWriter, r *http.Request) {
	resp := CurrentInfo()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func hostname() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "apisix-go"
	}
	return hostname
}
