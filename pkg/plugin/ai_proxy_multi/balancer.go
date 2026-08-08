package ai_proxy_multi

import (
	"fmt"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixvar "github.com/wklken/apisix-go/pkg/apisix/variable"
	"hash/fnv"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

func (p *Plugin) pickInstance(r *http.Request, tried map[int]bool) (int, bool) {
	priorities := p.priority
	if len(priorities) == 0 {
		return 0, false
	}

	// Advance each priority's rotation slot exactly once per pick, then reuse
	// the starts across the healthy and fallback passes.
	starts := make([]int, len(priorities))
	for i, priority := range priorities {
		sel := p.selection[priority]
		if sel == nil || sel.total == 0 {
			starts[i] = -1
			continue
		}
		starts[i] = p.nextWeightedSlot(r, priority, sel.total)
	}
	for _, requireHealthy := range []bool{true, false} {
		for i, priority := range priorities {
			sel := p.selection[priority]
			if starts[i] < 0 {
				continue
			}
			// Walk distinct instances in weight order starting at the slot's
			// instance; the weight run of a rejected instance is skipped in
			// one step instead of one slot at a time.
			first := weightInstanceAtSlot(sel, starts[i])
			for offset := range len(sel.ids) {
				index := sel.ids[(first+offset)%len(sel.ids)]
				if !tried[index] && (!requireHealthy || p.instanceHealthy(index)) {
					return index, true
				}
			}
		}
	}
	return 0, false
}

// weightInstanceAtSlot maps a slot in [0, sel.total) to the index of its
// distinct instance. When every instance has weight 1 the cumulative array is
// the identity and the slot itself is the instance (O(1)); otherwise a binary
// search over cumulative weights resolves the slot.
func weightInstanceAtSlot(sel *weightSelection, slot int) int {
	if sel.total == len(sel.ids) {
		return slot
	}
	return sort.Search(len(sel.cumulative), func(i int) bool {
		return sel.cumulative[i] > slot
	})
}

func (p *Plugin) nextWeightedSlot(r *http.Request, priority int, size int) int {
	if p.config.Balancer.Algorithm == "chash" {
		key := p.hashKey(r)
		hasher := fnv.New32a()
		_, _ = hasher.Write([]byte(key))
		return int(hasher.Sum32() % uint32(size))
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	slot := p.nextSlot[priority] % size
	p.nextSlot[priority]++
	return slot
}

func (p *Plugin) hashKey(r *http.Request) string {
	var key string
	switch p.config.Balancer.HashOn {
	case "header":
		key = r.Header.Get(p.config.Balancer.Key)
	case "cookie":
		cookie, err := r.Cookie(p.config.Balancer.Key)
		if err == nil {
			key = cookie.Value
		}
	case "consumer":
		key = hashVariable(r, "consumer_name")
	case "vars":
		key = hashVariable(r, p.config.Balancer.Key)
	case "vars_combinations":
		key = resolveHashVariableCombination(r, p.config.Balancer.Key)
	default:
		key = p.config.Balancer.Key
	}
	if key == "" {
		key = hashVariable(r, "remote_addr")
	}
	return key
}

func hashVariable(r *http.Request, name string) string {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "query_string":
		return r.URL.RawQuery
	case name == "host" || name == "server_name":
		host, _, err := net.SplitHostPort(r.Host)
		if err == nil {
			return host
		}
		return r.Host
	case name == "hostname":
		hostname, _ := os.Hostname()
		return hostname
	case name == "remote_addr":
		if value := apisixctx.GetString(r.Context(), "remote_addr"); value != "" {
			return value
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case name == "remote_port":
		_, port, _ := net.SplitHostPort(r.RemoteAddr)
		return port
	case name == "server_addr":
		local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
		if local == nil {
			return ""
		}
		host, _, err := net.SplitHostPort(local.String())
		if err == nil {
			return host
		}
		return local.String()
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "cookie_"):
		cookie, err := r.Cookie(strings.TrimPrefix(name, "cookie_"))
		if err == nil {
			return cookie.Value
		}
		return ""
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	}

	key := "$" + name
	if value := apisixvar.GetNginxVar(r, key); value != "" {
		return value
	}
	if value := fmt.Sprint(apisixctx.GetApisixVar(r, key)); value != "" {
		return value
	}
	if value := apisixctx.GetRequestVar(r, key); value != nil {
		return fmt.Sprint(value)
	}
	return ""
}

func resolveHashVariableCombination(r *http.Request, expression string) string {
	var resolved strings.Builder
	resolvedVariables := 0
	for position := 0; position < len(expression); {
		if expression[position] != '$' {
			resolved.WriteByte(expression[position])
			position++
			continue
		}
		end := position + 1
		for end < len(expression) && isHashVariableCharacter(expression[end]) {
			end++
		}
		if end == position+1 {
			resolved.WriteByte('$')
			position++
			continue
		}
		value := hashVariable(r, expression[position+1:end])
		if value != "" {
			resolvedVariables++
		}
		resolved.WriteString(value)
		position = end
	}
	if resolvedVariables == 0 {
		return ""
	}
	return resolved.String()
}

func isHashVariableCharacter(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9'
}

func (p *Plugin) canRetry(code int, elapsed time.Duration, retries int) bool {
	if p.config.MaxRetries != nil && retries >= *p.config.MaxRetries {
		return false
	}
	if p.config.RetryOnFailureWithinMS > 0 &&
		elapsed > time.Duration(p.config.RetryOnFailureWithinMS)*time.Millisecond {
		return false
	}
	if code == http.StatusTooManyRequests {
		return fallbackStrategyHas(p.config.FallbackStrategy, "http_429")
	}
	return code >= http.StatusInternalServerError &&
		code < http.StatusNetworkAuthenticationRequired &&
		fallbackStrategyHas(p.config.FallbackStrategy, "http_5xx")
}
