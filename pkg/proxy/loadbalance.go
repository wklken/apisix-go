package proxy

// loadbalance
import (
	"net/http"
	"sort"
	"sync"

	"github.com/smallnest/weighted"
)

// TO: performance, generate the list at startup, then loop over
// currently:
// BenchmarkRRLoadBalance-12    	50000000	        32.9 ns/op	       0 B/op	       0 allocs/op

type LoadBalancer interface {
	Next() string
}

// NextTarget selects a target for one request attempt. Priority-aware
// balancers use the request to avoid retrying targets already attempted by the
// same request; other balancers retain their existing process-wide selection.
func NextTarget(loadBalancer LoadBalancer, request *http.Request) string {
	if requestAware, ok := loadBalancer.(interface {
		NextForRequest(*http.Request) string
	}); ok {
		return requestAware.NextForRequest(request)
	}
	return loadBalancer.Next()
}

// RecordSelectedTarget marks a target chosen outside the load balancer (for
// example by chash) as attempted in the request-local priority state.
func RecordSelectedTarget(loadBalancer LoadBalancer, request *http.Request, target string) {
	if recorder, ok := loadBalancer.(interface {
		RecordSelectedTarget(*http.Request, string)
	}); ok {
		recorder.RecordSelectedTarget(request, target)
	}
}

// SingleLoadBalance for the backend with only one host
type SingleLoadBalance struct {
	server string
}

func NewSingleLoadBalance(server string) *SingleLoadBalance {
	// log.Debugf("create a single lb: %s", server)
	return &SingleLoadBalance{
		server: server,
	}
}

func (lb *SingleLoadBalance) Next() string {
	return lb.server
}

// SingleLoadBalance for the backend with multi hosts(with weight or not), will do smooth-RR
type RRLoadBalance struct {
	w    *weighted.SW
	lock sync.RWMutex
}

func NewRRLoadBalance(servers []string) *RRLoadBalance {
	// log.Debugf("create a rr lb: %s", servers)
	w := &weighted.SW{}
	for _, server := range servers {
		w.Add(server, 1)
	}

	return &RRLoadBalance{
		w: w,
	}
}

func (rr *RRLoadBalance) Next() string {
	rr.lock.Lock()
	value := rr.w.Next()
	rr.lock.Unlock()
	if value == nil {
		return ""
	}
	return value.(string)
}

func NewWeightedRRLoadBalance(servers map[string]int) *RRLoadBalance {
	w := &weighted.SW{}
	// Map iteration order is random; sort keys so the first smooth-RR pick is
	// stable across processes (needed for smoke tests that assert one request).
	names := make([]string, 0, len(servers))
	for server := range servers {
		names = append(names, server)
	}
	sort.Strings(names)
	for _, server := range names {
		w.Add(server, servers[server])
	}

	return &RRLoadBalance{
		w: w,
	}
}
