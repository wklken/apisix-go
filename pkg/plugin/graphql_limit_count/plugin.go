package graphql_limit_count

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu       sync.Mutex
	counters map[string]*counter
	now      func() time.Time

	redisLimiter countLimiter
}

const (
	priority = 1004
	name     = "graphql-limit-count"
)

const schema = `
{
  "type": "object",
  "properties": {
    "count": {
      "type": "integer",
      "exclusiveMinimum": 0
    },
    "time_window": {
      "type": "integer",
      "exclusiveMinimum": 0
    },
    "group": {
      "type": "string"
    },
    "key": {
      "type": "string",
      "default": "remote_addr"
    },
    "key_type": {
      "type": "string",
      "enum": ["var", "var_combination", "constant"],
      "default": "var"
    },
    "rejected_code": {
      "type": "integer",
      "minimum": 200,
      "maximum": 599,
      "default": 503
    },
    "rejected_msg": {
      "type": "string",
      "minLength": 1
    },
    "policy": {
      "type": "string",
      "enum": ["local", "redis", "redis-cluster"],
      "default": "local"
    },
    "redis_host": {
      "type": "string",
      "minLength": 2
    },
    "redis_port": {
      "type": "integer",
      "minimum": 1,
      "default": 6379
    },
    "redis_username": {
      "type": "string",
      "minLength": 1
    },
    "redis_password": {
      "type": "string",
      "minLength": 0
    },
    "redis_database": {
      "type": "integer",
      "minimum": 0,
      "default": 0
    },
    "redis_timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 1000
    },
    "redis_ssl": {
      "type": "boolean",
      "default": false
    },
    "redis_ssl_verify": {
      "type": "boolean",
      "default": false
    },
    "redis_keepalive_timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 10000
    },
    "redis_keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 100
    },
    "allow_degradation": {
      "type": "boolean",
      "default": false
    },
    "show_limit_quota_header": {
      "type": "boolean",
      "default": true
    }
  },
  "required": ["count", "time_window"]
}
`

type Config struct {
	Count                 int64  `json:"count"`
	TimeWindow            int64  `json:"time_window"`
	Group                 string `json:"group,omitempty"`
	Key                   string `json:"key,omitempty"`
	KeyType               string `json:"key_type,omitempty"`
	RejectedCode          int    `json:"rejected_code,omitempty"`
	RejectedMsg           string `json:"rejected_msg,omitempty"`
	Policy                string `json:"policy,omitempty"`
	RedisHost             string `json:"redis_host,omitempty"`
	RedisPort             int    `json:"redis_port,omitempty"`
	RedisUsername         string `json:"redis_username,omitempty"`
	RedisPassword         string `json:"redis_password,omitempty"`
	RedisDatabase         int    `json:"redis_database,omitempty"`
	RedisTimeout          int    `json:"redis_timeout,omitempty"`
	RedisSSL              *bool  `json:"redis_ssl,omitempty"`
	RedisSSLVerify        *bool  `json:"redis_ssl_verify,omitempty"`
	RedisKeepaliveTimeout int    `json:"redis_keepalive_timeout,omitempty"`
	RedisKeepalivePool    int    `json:"redis_keepalive_pool,omitempty"`
	AllowDegradation      *bool  `json:"allow_degradation,omitempty"`
	ShowLimitQuotaHeader  *bool  `json:"show_limit_quota_header,omitempty"`
}

type counter struct {
	used    int64
	resetAt time.Time
}

const redisLimitCountScript = `
local current = redis.call("INCRBY", KEYS[1], ARGV[1])
local ttl = redis.call("TTL", KEYS[1])
if ttl < 0 then
  redis.call("EXPIRE", KEYS[1], ARGV[3])
  ttl = tonumber(ARGV[3])
end

local limit = tonumber(ARGV[2])
local remaining = limit - current
if remaining < 0 then
  remaining = 0
end

local allowed = 1
if current > limit then
  allowed = 0
end

return {allowed, remaining, ttl}
`

type countLimiter interface {
	incoming(r *http.Request, key string, cost int64) (int64, int64, bool, error)
}

type graphqlRequest struct {
	Query string `json:"query"`
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
	if p.config.Count <= 0 {
		return fmt.Errorf("count must be greater than 0")
	}
	if p.config.TimeWindow <= 0 {
		return fmt.Errorf("time_window must be greater than 0")
	}
	if p.config.Key == "" {
		p.config.Key = "remote_addr"
	}
	if p.config.KeyType == "" {
		p.config.KeyType = "var"
	}
	if p.config.Policy == "" {
		p.config.Policy = "local"
	}
	if p.config.Policy != "local" && p.config.Policy != "redis" {
		return fmt.Errorf("not supported policy: %s", p.config.Policy)
	}
	if p.config.Policy == "redis" {
		if p.config.RedisHost == "" {
			return fmt.Errorf("redis_host is required")
		}
		if p.config.RedisPort == 0 {
			p.config.RedisPort = 6379
		}
		if p.config.RedisTimeout == 0 {
			p.config.RedisTimeout = 1000
		}
		if p.config.RedisSSL == nil {
			value := false
			p.config.RedisSSL = &value
		}
		if p.config.RedisSSLVerify == nil {
			value := false
			p.config.RedisSSLVerify = &value
		}
		if p.config.RedisKeepalivePool == 0 {
			p.config.RedisKeepalivePool = 100
		}
		if p.redisLimiter == nil {
			p.redisLimiter = p.newRedisLimiter()
		}
	}
	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = http.StatusServiceUnavailable
	}
	if p.config.AllowDegradation == nil {
		value := false
		p.config.AllowDegradation = &value
	}
	if p.config.ShowLimitQuotaHeader == nil {
		value := true
		p.config.ShowLimitQuotaHeader = &value
	}
	if p.counters == nil {
		p.counters = make(map[string]*counter)
	}
	if p.now == nil {
		p.now = time.Now
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		query, ok := p.graphqlQuery(w, r)
		if !ok {
			return
		}

		depth, err := queryDepth(query)
		if err != nil {
			http.Error(w, "Invalid graphql request: failed to parse graphql query", http.StatusBadRequest)
			return
		}

		remaining, reset, allowed, err := p.incoming(r, p.resolveKey(r), int64(depth))
		if err != nil {
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "failed to limit graphql count", http.StatusInternalServerError)
			return
		}
		if *p.config.ShowLimitQuotaHeader {
			w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(p.config.Count, 10))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		}
		if !allowed {
			rejectedMsg := "Limit exceeded"
			if p.config.RejectedMsg != "" {
				rejectedMsg = p.config.RejectedMsg
			}
			http.Error(w, rejectedMsg, p.config.RejectedCode)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) graphqlQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return "", false
	}

	body, err := readBody(r)
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		http.Error(w, "Invalid graphql request: can't get graphql request body", http.StatusBadRequest)
		return "", false
	}

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var req graphqlRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid graphql request, "+err.Error(), http.StatusBadRequest)
			return "", false
		}
		if req.Query == "" {
			http.Error(w, "invalid graphql request, json body[query] is nil", http.StatusBadRequest)
			return "", false
		}
		return req.Query, true
	}

	if strings.HasPrefix(contentType, "application/graphql") {
		return string(body), true
	}

	http.Error(w, "invalid graphql request, error content-type: "+contentType, http.StatusBadRequest)
	return "", false
}

func (p *Plugin) incoming(r *http.Request, key string, cost int64) (int64, int64, bool, error) {
	if p.config.Policy == "redis" {
		return p.redisLimiter.incoming(r, key, cost)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	c, ok := p.counters[key]
	if !ok || !now.Before(c.resetAt) {
		c = &counter{resetAt: now.Add(time.Duration(p.config.TimeWindow) * time.Second)}
		p.counters[key] = c
	}

	reset := int64(time.Until(c.resetAt).Seconds())
	if p.now != nil {
		reset = int64(c.resetAt.Sub(now).Seconds())
	}
	if reset < 0 {
		reset = 0
	}

	if c.used+cost > p.config.Count {
		return 0, reset, false, nil
	}
	c.used += cost
	return p.config.Count - c.used, reset, true, nil
}

type redisCountLimiter struct {
	client     *redis.Client
	count      int64
	timeWindow int64
}

func (p *Plugin) newRedisLimiter() countLimiter {
	configUID := shared.NewConfigUID()
	configUID.Add(
		p.config.RedisHost,
		p.config.RedisPort,
		p.config.RedisUsername,
		p.config.RedisPassword,
		p.config.RedisDatabase,
		p.config.RedisTimeout,
		*p.config.RedisSSL,
		*p.config.RedisSSLVerify,
	)

	options := &redis.Options{
		Addr:         fmt.Sprintf("%s:%d", p.config.RedisHost, p.config.RedisPort),
		Username:     p.config.RedisUsername,
		Password:     p.config.RedisPassword,
		DB:           p.config.RedisDatabase,
		DialTimeout:  time.Duration(p.config.RedisTimeout) * time.Millisecond,
		ReadTimeout:  time.Duration(p.config.RedisTimeout) * time.Millisecond,
		WriteTimeout: time.Duration(p.config.RedisTimeout) * time.Millisecond,
		PoolSize:     p.config.RedisKeepalivePool,
	}
	if p.config.RedisKeepaliveTimeout > 0 {
		options.ConnMaxIdleTime = time.Duration(p.config.RedisKeepaliveTimeout) * time.Millisecond
	}
	if p.config.RedisSSL != nil && *p.config.RedisSSL {
		options.TLSConfig = &tls.Config{InsecureSkipVerify: !*p.config.RedisSSLVerify}
	}

	client := shared.LoadOrStoreClient(name, configUID, redis.NewClient(options)).(*redis.Client)
	return &redisCountLimiter{client: client, count: p.config.Count, timeWindow: p.config.TimeWindow}
}

func (l *redisCountLimiter) incoming(r *http.Request, key string, cost int64) (int64, int64, bool, error) {
	result, err := l.client.Eval(
		r.Context(),
		redisLimitCountScript,
		[]string{"plugin-graphql-limit-count:" + key},
		cost,
		l.count,
		l.timeWindow,
	).Result()
	if err != nil {
		return 0, 0, false, err
	}

	values, ok := result.([]any)
	if !ok || len(values) != 3 {
		return 0, 0, false, fmt.Errorf("unexpected redis graphql-limit-count result: %v", result)
	}
	allowed, ok := redisInt(values[0])
	if !ok {
		return 0, 0, false, fmt.Errorf("unexpected redis graphql-limit-count allowed value: %v", values[0])
	}
	remaining, ok := redisInt(values[1])
	if !ok {
		return 0, 0, false, fmt.Errorf("unexpected redis graphql-limit-count remaining value: %v", values[1])
	}
	reset, ok := redisInt(values[2])
	if !ok {
		return 0, 0, false, fmt.Errorf("unexpected redis graphql-limit-count reset value: %v", values[2])
	}

	return remaining, reset, allowed == 1, nil
}

func redisInt(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		return int64(v), true
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func (p *Plugin) resolveKey(r *http.Request) string {
	switch p.config.KeyType {
	case "constant":
		return p.config.Key
	case "var_combination":
		key := p.config.Key
		resolved := 0
		for _, name := range templateVariables(key) {
			value := requestVar(r, name)
			if value != "" {
				resolved++
			}
			key = strings.ReplaceAll(key, "${"+name+"}", value)
			key = strings.ReplaceAll(key, "$"+name, value)
		}
		if resolved > 0 {
			return key
		}
	}

	if value := requestVar(r, p.config.Key); value != "" {
		return value
	}
	return requestVar(r, "remote_addr")
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

func requestVar(r *http.Request, key string) string {
	key = strings.TrimPrefix(key, "$")
	if strings.HasPrefix(key, "http_") {
		header := strings.ReplaceAll(strings.TrimPrefix(key, "http_"), "_", "-")
		return r.Header.Get(header)
	}

	if key == "remote_addr" && r.RemoteAddr != "" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			return host
		}
		return r.RemoteAddr
	}

	value := variable.GetNginxVar(r, "$"+key)
	if key == "remote_addr" {
		if host, _, err := net.SplitHostPort(value); err == nil {
			return host
		}
	}
	return value
}

func queryDepth(query string) (int, error) {
	tokens := tokenize(query)
	parser := graphQLParser{tokens: tokens}
	doc, err := parser.parseDocument()
	if err != nil {
		return 0, err
	}
	return doc.depth(), nil
}

func templateVariables(template string) []string {
	var variables []string
	for i := 0; i < len(template); i++ {
		if template[i] != '$' {
			continue
		}
		start := i + 1
		end := start
		if start < len(template) && template[start] == '{' {
			start++
			end = start
			for end < len(template) && template[end] != '}' {
				end++
			}
			if end < len(template) {
				variables = append(variables, template[start:end])
				i = end
			}
			continue
		}
		for end < len(template) && isNameChar(template[end]) {
			end++
		}
		if end > start {
			variables = append(variables, template[start:end])
			i = end - 1
		}
	}
	return variables
}

type graphQLDocument struct {
	operations []selectionSet
	fragments  map[string]selectionSet
}

func (d graphQLDocument) depth() int {
	depth := 0
	for _, op := range d.operations {
		depth = max(depth, op.depth(d.fragments, map[string]bool{}))
	}
	return max(depth, 1)
}

type selectionSet []selection

func (s selectionSet) depth(fragments map[string]selectionSet, visited map[string]bool) int {
	depth := 0
	for _, item := range s {
		depth = max(depth, item.depth(fragments, visited))
	}
	return depth
}

type selection struct {
	name     string
	child    selectionSet
	fragment string
	inline   bool
}

func (s selection) depth(fragments map[string]selectionSet, visited map[string]bool) int {
	if s.fragment != "" {
		if visited[s.fragment] {
			return 0
		}
		fragment, ok := fragments[s.fragment]
		if !ok {
			return 0
		}
		visited[s.fragment] = true
		depth := fragment.depth(fragments, visited)
		delete(visited, s.fragment)
		return depth
	}
	if s.inline {
		return s.child.depth(fragments, visited)
	}
	if len(s.child) == 0 {
		return 1
	}
	return 1 + s.child.depth(fragments, visited)
}

type graphQLParser struct {
	tokens []string
	pos    int
}

func (p *graphQLParser) parseDocument() (graphQLDocument, error) {
	doc := graphQLDocument{fragments: map[string]selectionSet{}}
	for p.hasNext() {
		if p.peek() == "fragment" {
			name, set, err := p.parseFragment()
			if err != nil {
				return doc, err
			}
			doc.fragments[name] = set
			continue
		}

		set, err := p.parseOperation()
		if err != nil {
			return doc, err
		}
		doc.operations = append(doc.operations, set)
	}
	if len(doc.operations) == 0 {
		return doc, fmt.Errorf("empty graphql query")
	}
	return doc, nil
}

func (p *graphQLParser) parseFragment() (string, selectionSet, error) {
	p.next()
	if !p.hasNext() {
		return "", nil, fmt.Errorf("missing fragment name")
	}
	name := p.next()
	set, err := p.skipToSelectionSet()
	return name, set, err
}

func (p *graphQLParser) parseOperation() (selectionSet, error) {
	if p.peek() == "{" {
		return p.parseSelectionSet()
	}
	return p.skipToSelectionSet()
}

func (p *graphQLParser) skipToSelectionSet() (selectionSet, error) {
	for p.hasNext() && p.peek() != "{" {
		p.next()
	}
	if !p.hasNext() {
		return nil, fmt.Errorf("missing selection set")
	}
	return p.parseSelectionSet()
}

func (p *graphQLParser) parseSelectionSet() (selectionSet, error) {
	if !p.consume("{") {
		return nil, fmt.Errorf("missing opening selection")
	}

	var selections selectionSet
	for p.hasNext() && p.peek() != "}" {
		if p.peek() == "..." {
			p.next()
			if !p.hasNext() {
				return nil, fmt.Errorf("missing fragment spread")
			}
			if p.peek() == "on" {
				p.next()
				if p.hasNext() {
					p.next()
				}
				child, err := p.skipToSelectionSet()
				if err != nil {
					return nil, err
				}
				selections = append(selections, selection{inline: true, child: child})
				continue
			}
			selections = append(selections, selection{fragment: p.next()})
			continue
		}

		field := selection{name: p.next()}
		p.skipArgumentsAndDirectives()
		if p.hasNext() && p.peek() == "{" {
			child, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			field.child = child
		}
		selections = append(selections, field)
	}
	if !p.consume("}") {
		return nil, fmt.Errorf("missing closing selection")
	}
	return selections, nil
}

func (p *graphQLParser) skipArgumentsAndDirectives() {
	depth := 0
	for p.hasNext() {
		tok := p.peek()
		switch tok {
		case "(":
			depth++
		case ")":
			if depth > 0 {
				depth--
			}
		case "{", "}":
			if depth == 0 {
				return
			}
		}
		p.next()
	}
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
