package proxy

// loadbalance
import (
	"sync"

	"github.com/smallnest/weighted"
)

// TO: performance, generate the list at startup, then loop over
// currently:
// BenchmarkRRLoadBalance-12    	50000000	        32.9 ns/op	       0 B/op	       0 allocs/op

type LoadBalancer interface {
	Next() string
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
	value := rr.w.Next().(string)
	rr.lock.Unlock()
	return value
}

func NewWeightedRRLoadBalance(servers map[string]int) *RRLoadBalance {
	w := &weighted.SW{}
	for server, weight := range servers {
		w.Add(server, weight)
	}

	return &RRLoadBalance{
		w: w,
	}
}
