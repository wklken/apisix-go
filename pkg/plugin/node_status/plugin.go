package node_status

import (
	"net/http"
	"sync/atomic"

	apisixid "github.com/wklken/apisix-go/pkg/apisix/id"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 1000
	name     = "node-status"
)

const schema = `{"type":"object"}`

var (
	activeRequests   atomic.Uint64
	acceptedRequests atomic.Uint64
	handledRequests  atomic.Uint64
	totalRequests    atomic.Uint64
)

type Config struct{}

type Response struct {
	ID     string            `json:"id"`
	Status map[string]string `json:"status"`
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

func Track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptedRequests.Add(1)
		totalRequests.Add(1)
		activeRequests.Add(1)
		defer func() {
			activeRequests.Add(^uint64(0))
			handledRequests.Add(1)
		}()

		next.ServeHTTP(w, r)
	})
}

func StatusHandler(w http.ResponseWriter, r *http.Request) {
	resp := Response{
		ID: apisixid.Get(),
		Status: map[string]string{
			"active":   formatUint(activeRequests.Load()),
			"accepted": formatUint(acceptedRequests.Load()),
			"handled":  formatUint(handledRequests.Load()),
			"total":    formatUint(totalRequests.Load()),
			"reading":  "0",
			"writing":  "0",
			"waiting":  "0",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func formatUint(v uint64) string {
	return stringUint(v)
}

func stringUint(v uint64) string {
	if v == 0 {
		return "0"
	}

	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
