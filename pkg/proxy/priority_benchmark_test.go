package proxy

import "testing"

func BenchmarkPriorityGroupNextUntriedFiltered(b *testing.B) {
	group := priorityGroup{
		targets: []string{"target-a", "target-b", "target-c", "target-d"},
		weights: map[string]int{"target-a": 8, "target-b": 5, "target-c": 3, "target-d": 1},
		selector: NewWeightedRRLoadBalance(map[string]int{
			"target-a": 8,
			"target-b": 5,
			"target-c": 3,
			"target-d": 1,
		}),
	}
	tried := map[string]struct{}{"target-a": {}}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := group.nextUntried(tried, nil); got == "" {
			b.Fatal("nextUntried returned no target")
		}
	}
}
