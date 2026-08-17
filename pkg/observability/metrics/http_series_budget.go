package metrics

import (
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	overflowLabel = "__overflow__"
)

// httpSeriesBudget admits complete HTTP metric tuples up to a fixed family
// limit. Once full, only the dynamic labels are replaced so bounded protocol
// dimensions remain useful in the overflow series.
type httpSeriesBudget struct {
	mu              sync.Mutex
	limit           int
	seen            map[string]struct{}
	dynamicIndexes  []int
	dynamicTailFrom int
	overflowCounter prometheus.Counter
}

func newHTTPSeriesBudget(limit int, overflowCounter prometheus.Counter, dynamicIndexes []int) *httpSeriesBudget {
	return newHTTPSeriesBudgetWithTail(limit, overflowCounter, dynamicIndexes, -1)
}

func newHTTPSeriesBudgetWithTail(
	limit int,
	overflowCounter prometheus.Counter,
	dynamicIndexes []int,
	dynamicTailFrom int,
) *httpSeriesBudget {
	return &httpSeriesBudget{
		limit:           limit,
		seen:            make(map[string]struct{}, limit),
		dynamicIndexes:  append([]int(nil), dynamicIndexes...),
		dynamicTailFrom: dynamicTailFrom,
		overflowCounter: overflowCounter,
	}
}

func (b *httpSeriesBudget) admit(values []string) []string {
	if b == nil {
		return values
	}
	key := httpSeriesTupleKey(values)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[key]; ok {
		return values
	}
	if len(b.seen) < b.limit {
		b.seen[key] = struct{}{}
		return values
	}
	overflow := append([]string(nil), values...)
	for _, index := range b.dynamicIndexes {
		if index >= 0 && index < len(overflow) {
			overflow[index] = overflowLabel
		}
	}
	for index := b.dynamicTailFrom; index >= 0 && index < len(overflow); index++ {
		overflow[index] = overflowLabel
	}
	if b.overflowCounter != nil {
		b.overflowCounter.Inc()
	}
	return overflow
}

func httpSeriesTupleKey(values []string) string {
	var builder strings.Builder
	for _, value := range values {
		builder.WriteString(strconv.Itoa(len(value)))
		builder.WriteByte(':')
		builder.WriteString(value)
	}
	return builder.String()
}

func newHTTPMetricSeriesOverflow(prefix string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prefix + "http_metric_series_overflow_total",
			Help: "HTTP metric series observations rejected by the family cardinality budget",
		},
		[]string{"metric"},
	)
}
