package server_info

import (
	"net/http"
	"os"
	"time"

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

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func InfoHandler(w http.ResponseWriter, r *http.Request) {
	hostname := hostname()
	resp := Response{
		EtcdVersion: "unknown",
		Hostname:    hostname,
		ID:          hostname,
		Version:     version,
		BootTime:    bootTime,
	}

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
