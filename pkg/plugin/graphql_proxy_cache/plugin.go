package graphql_proxy_cache

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	entries map[string]cacheEntry
	lock    sync.RWMutex
	now     func() time.Time
}

const (
	priority = 1009
	name     = "graphql-proxy-cache"

	cacheStatusHeader = "Apisix-Cache-Status"
	cacheKeyHeader    = "APISIX-Cache-Key"
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
	CacheZone         string `json:"cache_zone,omitempty"`
	CacheStrategy     string `json:"cache_strategy,omitempty"`
	CacheTTL          int    `json:"cache_ttl,omitempty"`
	ConsumerIsolation *bool  `json:"consumer_isolation,omitempty"`
	CacheSetCookie    bool   `json:"cache_set_cookie,omitempty"`
}

type cacheEntry struct {
	header    http.Header
	body      []byte
	status    int
	expiresAt time.Time
}

type graphqlRequest struct {
	Query string `json:"query"`
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
	if p.config.CacheZone == "" {
		p.config.CacheZone = "disk_cache_one"
	}
	if p.config.CacheStrategy == "" {
		p.config.CacheStrategy = "disk"
	}
	if p.config.CacheTTL == 0 {
		p.config.CacheTTL = 300
	}
	if p.config.ConsumerIsolation == nil {
		value := true
		p.config.ConsumerIsolation = &value
	}
	if p.entries == nil {
		p.entries = make(map[string]cacheEntry)
	}
	if p.now == nil {
		p.now = time.Now
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		body, query, ok := p.graphqlRequest(w, r)
		if !ok {
			return
		}

		isMutation, err := graphqlHasMutation(query)
		if err != nil {
			http.Error(w, "Invalid graphql request: failed to parse graphql query", http.StatusBadRequest)
			return
		}
		if isMutation {
			w.Header().Set(cacheStatusHeader, "BYPASS")
			next.ServeHTTP(w, r)
			return
		}

		key := p.cacheKey(r, body)
		w.Header().Set(cacheKeyHeader, key)
		if entry, status := p.lookup(key); status == "HIT" {
			writeCachedResponse(w, entry, status, key)
			return
		} else if status == "EXPIRED" {
			p.fetchAndStore(w, r, next, key, status)
			return
		}

		p.fetchAndStore(w, r, next, key, "MISS")
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) graphqlRequest(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil, "", false
	}

	if r.Method == http.MethodGet {
		query := r.URL.Query().Get("query")
		if query == "" {
			http.Error(w, "invalid graphql request, args[query] is nil", http.StatusBadRequest)
			return nil, "", false
		}
		return []byte(r.URL.RawQuery), query, true
	}

	body, err := readBody(r)
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		http.Error(w, "Invalid graphql request: can't get graphql request body", http.StatusBadRequest)
		return nil, "", false
	}

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var req graphqlRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid graphql request, "+err.Error(), http.StatusBadRequest)
			return nil, "", false
		}
		if req.Query == "" {
			http.Error(w, "invalid graphql request, json body[query] is nil", http.StatusBadRequest)
			return nil, "", false
		}
		return body, req.Query, true
	}

	if strings.HasPrefix(contentType, "application/graphql") {
		return body, string(body), true
	}

	http.Error(w, "invalid graphql request, error content-type: "+contentType, http.StatusBadRequest)
	return nil, "", false
}

func (p *Plugin) fetchAndStore(w http.ResponseWriter, r *http.Request, next http.Handler, key string, status string) {
	recorder := newResponseRecorder()
	next.ServeHTTP(recorder, r)
	if recorder.statusCode == 0 {
		recorder.statusCode = http.StatusOK
	}

	if recorder.statusCode == http.StatusOK && (p.config.CacheSetCookie || recorder.header.Get("Set-Cookie") == "") {
		p.store(key, recorder)
	}
	recorder.header.Set(cacheStatusHeader, status)
	recorder.header.Set(cacheKeyHeader, key)
	recorder.writeTo(w)
}

func (p *Plugin) lookup(key string) (cacheEntry, string) {
	p.lock.RLock()
	entry, ok := p.entries[key]
	p.lock.RUnlock()
	if !ok {
		return cacheEntry{}, "MISS"
	}
	if p.now().After(entry.expiresAt) {
		return cacheEntry{}, "EXPIRED"
	}
	return entry, "HIT"
}

func (p *Plugin) store(key string, recorder *responseRecorder) {
	entry := cacheEntry{
		header:    cloneHeader(recorder.header),
		body:      append([]byte(nil), recorder.body.Bytes()...),
		status:    recorder.statusCode,
		expiresAt: p.now().Add(time.Duration(p.config.CacheTTL) * time.Second),
	}
	entry.header.Del(cacheStatusHeader)
	entry.header.Del(cacheKeyHeader)

	p.lock.Lock()
	p.entries[key] = entry
	p.lock.Unlock()
}

func (p *Plugin) cacheKey(r *http.Request, body []byte) string {
	parts := []string{
		p.config.CacheStrategy,
		p.config.CacheZone,
		r.Host,
		"",
		"",
		"",
		string(body),
	}
	if p.config.ConsumerIsolation != nil && *p.config.ConsumerIsolation {
		parts[5] = r.Header.Get("X-Consumer-Username")
	}
	sum := md5.Sum([]byte(strings.Join(parts, "\x01")))
	return hex.EncodeToString(sum[:])
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func graphqlHasMutation(query string) (bool, error) {
	tokens := tokenize(query)
	parser := graphQLParser{tokens: tokens}
	return parser.hasMutation()
}

type graphQLParser struct {
	tokens []string
	pos    int
}

func (p *graphQLParser) hasMutation() (bool, error) {
	foundOperation := false
	for p.hasNext() {
		switch p.peek() {
		case "query", "subscription":
			foundOperation = true
			if err := p.skipOperation(); err != nil {
				return false, err
			}
		case "mutation":
			return true, nil
		case "fragment":
			if err := p.skipOperation(); err != nil {
				return false, err
			}
		case "{":
			foundOperation = true
			if err := p.skipSelectionSet(); err != nil {
				return false, err
			}
		default:
			p.next()
		}
	}
	if !foundOperation {
		return false, fmt.Errorf("empty graphql query")
	}
	return false, nil
}

func (p *graphQLParser) skipOperation() error {
	for p.hasNext() && p.peek() != "{" {
		p.next()
	}
	if !p.hasNext() {
		return fmt.Errorf("missing selection set")
	}
	return p.skipSelectionSet()
}

func (p *graphQLParser) skipSelectionSet() error {
	if !p.consume("{") {
		return fmt.Errorf("missing opening selection")
	}
	depth := 1
	for p.hasNext() && depth > 0 {
		switch p.next() {
		case "{":
			depth++
		case "}":
			depth--
		}
	}
	if depth != 0 {
		return fmt.Errorf("missing closing selection")
	}
	return nil
}

func (p *graphQLParser) consume(token string) bool {
	if !p.hasNext() || p.peek() != token {
		return false
	}
	p.next()
	return true
}

func (p *graphQLParser) peek() string {
	return p.tokens[p.pos]
}

func (p *graphQLParser) next() string {
	token := p.tokens[p.pos]
	p.pos++
	return token
}

func (p *graphQLParser) hasNext() bool {
	return p.pos < len(p.tokens)
}

func tokenize(query string) []string {
	var tokens []string
	for i := 0; i < len(query); {
		switch ch := query[i]; {
		case ch == '#':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case ch == '"':
			i = skipString(query, i)
		case strings.HasPrefix(query[i:], "..."):
			tokens = append(tokens, "...")
			i += 3
		case strings.ContainsRune("{}()", rune(ch)):
			tokens = append(tokens, string(ch))
			i++
		case isNameChar(ch):
			start := i
			for i < len(query) && isNameChar(query[i]) {
				i++
			}
			tokens = append(tokens, query[start:i])
		default:
			i++
		}
	}
	return tokens
}

func skipString(query string, start int) int {
	i := start + 1
	for i < len(query) {
		if query[i] == '\\' {
			i += 2
			continue
		}
		if query[i] == '"' {
			return i + 1
		}
		i++
	}
	return i
}

func isNameChar(ch byte) bool {
	return ch == '_' || ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z'
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

func writeCachedResponse(w http.ResponseWriter, entry cacheEntry, cacheStatus string, cacheKey string) {
	for field, values := range entry.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.Header().Set(cacheStatusHeader, cacheStatus)
	w.Header().Set(cacheKeyHeader, cacheKey)
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
