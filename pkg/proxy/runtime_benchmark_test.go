package proxy

import (
	"fmt"
	"runtime"
	"testing"
)

// Benchmark corpus for weighted round-robin selection under serial and
// parallel load across increasing node counts. Rows are recorded before and
// after the Upstream Cluster Runtime change; the accepted comparison is the
// immutable pre-cluster baseline.

func benchmarkServers(count int) map[string]int {
	servers := make(map[string]int, count)
	for index := range count {
		servers[fmt.Sprintf("http://127.0.0.1:%d", 10000+index)] = index%5 + 1
	}
	return servers
}

func BenchmarkRRLoadBalance(b *testing.B) {
	for _, nodes := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("nodes=%d/serial", nodes), func(b *testing.B) {
			lb := NewWeightedRRLoadBalance(benchmarkServers(nodes))
			b.ReportAllocs()
			var target string
			for b.Loop() {
				target = lb.Next()
			}
			runtime.KeepAlive(target)
		})
		b.Run(fmt.Sprintf("nodes=%d/parallel", nodes), func(b *testing.B) {
			lb := NewWeightedRRLoadBalance(benchmarkServers(nodes))
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				var target string
				for pb.Next() {
					target = lb.Next()
				}
				runtime.KeepAlive(target)
			})
		})
	}
}
