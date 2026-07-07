package proxy_cache

import (
	"bytes"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	entries map[string]cacheEntry
	lock    sync.RWMutex
}

const (
	priority          = 1085
	name              = "proxy-cache"
	cacheStatusHeader = "Apisix-Cache-Status"
)

const schema = `
{
  "type": "object",
  "properties": {
    "cache_zone": {
      "type": "string",
      "minLength": 1,
      "maxLength": 100,
      "default": "disk_cache_one"
    },
    "cache_strategy": {
      "type": "string",
      "enum": ["disk", "memory"],
      "default": "disk"
    },
    "cache_key": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string"
      },
      "default": ["$host", "$request_uri"]
    },
    "cache_bypass": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string"
      }
    },
    "cache_method": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "enum": ["GET", "POST", "HEAD"]
      },
      "default": ["GET", "HEAD"]
    },
    "cache_http_status": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "integer",
        "minimum": 200,
        "maximum": 599
      },
      "default": [200, 301, 404]
    },
    "hide_cache_headers": {
      "type": "boolean",
      "default": false
    },
    "cache_control": {
      "type": "boolean",
      "default": false
    },
    "no_cache": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string"
      }
    },
    "cache_ttl": {
      "type": "integer",
      "minimum": 1,
      "default": 300
    },
    "consumer_isolation": {
      "type": "boolean",
      "default": true
    },
    "cache_set_cookie": {
      "type": "boolean",
      "default": false
    }
  }
}
`

type Config struct {
	CacheZone         string   `json:"cache_zone,omitempty"`
	CacheStrategy     string   `json:"cache_strategy,omitempty"`
	CacheKey          []string `json:"cache_key,omitempty"`
	CacheBypass       []string `json:"cache_bypass,omitempty"`
	CacheMethod       []string `json:"cache_method,omitempty"`
	CacheHTTPStatus   []int    `json:"cache_http_status,omitempty"`
	HideCacheHeaders  bool     `json:"hide_cache_headers,omitempty"`
	CacheControl      bool     `json:"cache_control,omitempty"`
	NoCache           []string `json:"no_cache,omitempty"`
	CacheTTL          int      `json:"cache_ttl,omitempty"`
	ConsumerIsolation bool     `json:"consumer_isolation,omitempty"`
	CacheSetCookie    bool     `json:"cache_set_cookie,omitempty"`
}

type cacheEntry struct {
	header    http.Header
	body      []byte
	status    int
	expiresAt time.Time
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
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
	if p.config.CacheStrategy == "" {
		p.config.CacheStrategy = "disk"
	}
	if p.config.CacheZone == "" {
		p.config.CacheZone = "disk_cache_one"
	}
	if len(p.config.CacheKey) == 0 {
		p.config.CacheKey = []string{"$host", "$request_uri"}
	}
	if len(p.config.CacheMethod) == 0 {
		p.config.CacheMethod = []string{http.MethodGet, http.MethodHead}
	}
	if len(p.config.CacheHTTPStatus) == 0 {
		p.config.CacheHTTPStatus = []int{http.StatusOK, http.StatusMovedPermanently, http.StatusNotFound}
	}
	if p.config.CacheTTL == 0 {
		p.config.CacheTTL = 300
	}
	if !p.config.ConsumerIsolation {
		p.config.ConsumerIsolation = true
	}
	p.entries = map[string]cacheEntry{}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if !p.cacheableMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		key := p.cacheKey(r)
		if p.hasTruthyValue(r, p.config.CacheBypass) {
			p.fetchAndMaybeStore(w, r, next, key, "BYPASS", false)
			return
		}

		if entry, status := p.lookup(key); status == "HIT" {
			writeCachedResponse(w, entry, status)
			return
		} else if status == "EXPIRED" {
			p.fetchAndMaybeStore(w, r, next, key, status, !p.hasTruthyValue(r, p.config.NoCache))
			return
		}

		p.fetchAndMaybeStore(w, r, next, key, "MISS", !p.hasTruthyValue(r, p.config.NoCache))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) fetchAndMaybeStore(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
	key string,
	cacheStatus string,
	shouldStore bool,
) {
	recorder := newResponseRecorder()
	next.ServeHTTP(recorder, r)
	if recorder.statusCode == 0 {
		recorder.statusCode = http.StatusOK
	}

	if p.hasTruthyValue(r, p.config.NoCache) {
		shouldStore = false
		cacheStatus = "EXPIRED"
	}
	if shouldStore && p.cacheableStatus(recorder.statusCode) &&
		(p.config.CacheSetCookie || recorder.header.Get("Set-Cookie") == "") {
		p.store(key, recorder)
	}
	recorder.header.Set(cacheStatusHeader, cacheStatus)
	recorder.writeTo(w)
}

func (p *Plugin) lookup(key string) (cacheEntry, string) {
	p.lock.RLock()
	entry, ok := p.entries[key]
	p.lock.RUnlock()
	if !ok {
		return cacheEntry{}, "MISS"
	}
	if time.Now().After(entry.expiresAt) {
		return cacheEntry{}, "EXPIRED"
	}
	return entry, "HIT"
}

func (p *Plugin) store(key string, recorder *responseRecorder) {
	entry := cacheEntry{
		header:    cloneHeader(recorder.header),
		body:      append([]byte(nil), recorder.body.Bytes()...),
		status:    recorder.statusCode,
		expiresAt: time.Now().Add(time.Duration(p.config.CacheTTL) * time.Second),
	}
	entry.header.Del(cacheStatusHeader)
	if p.config.HideCacheHeaders {
		entry.header.Del("Expires")
		entry.header.Del("Cache-Control")
	}

	p.lock.Lock()
	p.entries[key] = entry
	p.lock.Unlock()
}

func (p *Plugin) cacheKey(r *http.Request) string {
	var b strings.Builder
	for _, part := range p.config.CacheKey {
		if strings.HasPrefix(part, "$") {
			b.WriteString(requestVar(r, strings.TrimPrefix(part, "$")))
			continue
		}
		b.WriteString(part)
	}
	return b.String()
}

func (p *Plugin) cacheableMethod(method string) bool {
	for _, allowed := range p.config.CacheMethod {
		if method == allowed {
			return true
		}
	}
	return false
}

func (p *Plugin) cacheableStatus(status int) bool {
	for _, allowed := range p.config.CacheHTTPStatus {
		if status == allowed {
			return true
		}
	}
	return false
}

func (p *Plugin) hasTruthyValue(r *http.Request, values []string) bool {
	for _, value := range values {
		resolved := value
		if strings.HasPrefix(value, "$") {
			resolved = requestVar(r, strings.TrimPrefix(value, "$"))
		}
		if resolved != "" && resolved != "0" {
			return true
		}
	}
	return false
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     http.Header{},
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	_, _ = w.Write(r.body.Bytes())
}

func writeCachedResponse(w http.ResponseWriter, entry cacheEntry, cacheStatus string) {
	for field, values := range entry.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.Header().Set(cacheStatusHeader, cacheStatus)
	w.WriteHeader(entry.status)
	_, _ = w.Write(entry.body)
}

func cloneHeader(header http.Header) http.Header {
	cloned := make(http.Header, len(header))
	for field, values := range header {
		cloned[field] = append([]string(nil), values...)
	}
	return cloned
}

func requestVar(r *http.Request, name string) string {
	switch {
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "host":
		return r.Host
	case name == "request_method":
		return r.Method
	case name == "scheme":
		if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case name == "remote_addr":
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	default:
		return ""
	}
}
