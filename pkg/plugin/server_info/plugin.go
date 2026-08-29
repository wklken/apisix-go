package server_info

import (
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	apisixid "github.com/wklken/apisix-go/pkg/apisix/id"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
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

// View is the process-owned server-info snapshot shared by the control API
// and the etcd reporter. It owns no external client or background task.
type View struct {
	configuredID string
	etcdVersion  atomic.Value
}

func NewView(configuredID string) *View {
	view := &View{configuredID: configuredID}
	view.etcdVersion.Store("unknown")
	return view
}

func (view *View) SetEtcdVersion(value string) {
	if view == nil {
		return
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	view.etcdVersion.Store(value)
}

func (view *View) Current() Response {
	if view == nil {
		return CurrentInfo("")
	}
	etcdVersion, _ := view.etcdVersion.Load().(string)
	if etcdVersion == "" {
		etcdVersion = "unknown"
	}
	return currentInfo(view.configuredID, etcdVersion)
}

func (view *View) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = util.WriteJSON(w, http.StatusOK, view.Current())
	}
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

func CurrentInfo(configuredID string) Response {
	return currentInfo(configuredID, "unknown")
}

func currentInfo(configuredID string, etcdVersion string) Response {
	hostname := hostname()
	return Response{
		EtcdVersion: etcdVersion,
		Hostname:    hostname,
		ID:          apisixid.Get(configuredID),
		Version:     version,
		BootTime:    bootTime,
	}
}

func ReportTTL(attr map[string]any) time.Duration {
	ttl := defaultReportTTL
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
	case json.Number:
		parsed, err := v.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func InfoHandler(configuredID string) http.HandlerFunc {
	return NewView(configuredID).Handler()
}

func hostname() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "apisix-go"
	}
	return hostname
}
