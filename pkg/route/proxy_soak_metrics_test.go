package route

import (
	"fmt"
	"math"
	runtimemetrics "runtime/metrics"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

var soakLatencyBounds = [...]uint64{
	50, 100, 200, 500, 1_000, 2_000, 5_000, 10_000,
	20_000, 50_000, 100_000, 200_000, 500_000, 1_000_000,
	2_000_000, 5_000_000, 10_000_000, 30_000_000, 60_000_000,
}

var soakRuntimeMetricNames = [...]string{
	"/gc/heap/allocs:bytes",
	"/cpu/classes/gc/total:cpu-seconds",
	"/sched/pauses/total/gc:seconds",
	"/sched/pauses/total/other:seconds",
}

type soakLatencyHistogram struct {
	buckets [len(soakLatencyBounds) + 1]atomic.Uint64
}

type soakLatencySnapshot struct {
	buckets [len(soakLatencyBounds) + 1]uint64
}

func (h *soakLatencyHistogram) Observe(duration time.Duration) {
	var micros uint64
	if duration > 0 {
		micros = uint64(duration / time.Microsecond)
		if duration%time.Microsecond != 0 {
			micros++
		}
	}
	bucket := sort.Search(len(soakLatencyBounds), func(index int) bool {
		return micros <= soakLatencyBounds[index]
	})
	h.buckets[bucket].Add(1)
}

func (h *soakLatencyHistogram) Snapshot() soakLatencySnapshot {
	var snapshot soakLatencySnapshot
	for index := range h.buckets {
		snapshot.buckets[index] = h.buckets[index].Load()
	}
	return snapshot
}

func (s soakLatencySnapshot) Delta(warm soakLatencySnapshot) (soakLatencySnapshot, error) {
	var delta soakLatencySnapshot
	for index := range s.buckets {
		if s.buckets[index] < warm.buckets[index] {
			return soakLatencySnapshot{}, fmt.Errorf(
				"latency bucket %d decreased from %d to %d",
				index,
				warm.buckets[index],
				s.buckets[index],
			)
		}
		delta.buckets[index] = s.buckets[index] - warm.buckets[index]
	}
	return delta, nil
}

func (s soakLatencySnapshot) Count() uint64 {
	var total uint64
	for _, count := range s.buckets {
		total += count
	}
	return total
}

func (s soakLatencySnapshot) Quantile(quantile float64) time.Duration {
	if quantile < 0 || quantile > 1 {
		return 0
	}
	total := s.Count()
	if total == 0 {
		return 0
	}
	rank := uint64(math.Ceil(quantile * float64(total)))
	if rank == 0 {
		rank = 1
	}
	var cumulative uint64
	for index, count := range s.buckets {
		cumulative += count
		if rank > cumulative {
			continue
		}
		if index == len(soakLatencyBounds) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(soakLatencyBounds[index]) * time.Microsecond
	}
	return time.Duration(math.MaxInt64)
}

func formatSoakLatency(duration time.Duration) string {
	if duration == time.Duration(math.MaxInt64) {
		return "+Inf"
	}
	return duration.String()
}

type soakRuntimeHistogram struct {
	buckets []float64
	counts  []uint64
}

type soakRuntimeSnapshot struct {
	allocatedBytes      uint64
	gcCPUSeconds        float64
	gcPause             soakRuntimeHistogram
	schedulerOtherPause soakRuntimeHistogram
}

func readSoakRuntimeMetrics() (soakRuntimeSnapshot, error) {
	samples := make([]runtimemetrics.Sample, len(soakRuntimeMetricNames))
	for index, name := range soakRuntimeMetricNames {
		samples[index].Name = name
	}
	runtimemetrics.Read(samples)

	if samples[0].Value.Kind() != runtimemetrics.KindUint64 {
		return soakRuntimeSnapshot{}, fmt.Errorf(
			"runtime metric %q has kind %v, want uint64",
			soakRuntimeMetricNames[0],
			samples[0].Value.Kind(),
		)
	}
	if samples[1].Value.Kind() != runtimemetrics.KindFloat64 {
		return soakRuntimeSnapshot{}, fmt.Errorf(
			"runtime metric %q has kind %v, want float64",
			soakRuntimeMetricNames[1],
			samples[1].Value.Kind(),
		)
	}
	gcPause, err := copySoakRuntimeHistogram(soakRuntimeMetricNames[2], samples[2].Value)
	if err != nil {
		return soakRuntimeSnapshot{}, err
	}
	schedulerOtherPause, err := copySoakRuntimeHistogram(soakRuntimeMetricNames[3], samples[3].Value)
	if err != nil {
		return soakRuntimeSnapshot{}, err
	}
	return soakRuntimeSnapshot{
		allocatedBytes:      samples[0].Value.Uint64(),
		gcCPUSeconds:        samples[1].Value.Float64(),
		gcPause:             gcPause,
		schedulerOtherPause: schedulerOtherPause,
	}, nil
}

func copySoakRuntimeHistogram(
	name string,
	value runtimemetrics.Value,
) (soakRuntimeHistogram, error) {
	if value.Kind() != runtimemetrics.KindFloat64Histogram {
		return soakRuntimeHistogram{}, fmt.Errorf(
			"runtime metric %q has kind %v, want float64 histogram",
			name,
			value.Kind(),
		)
	}
	histogram := value.Float64Histogram()
	if histogram == nil || len(histogram.Buckets) != len(histogram.Counts)+1 {
		return soakRuntimeHistogram{}, fmt.Errorf("runtime metric %q has invalid histogram shape", name)
	}
	for index := 1; index < len(histogram.Buckets); index++ {
		if !(histogram.Buckets[index] > histogram.Buckets[index-1]) {
			return soakRuntimeHistogram{}, fmt.Errorf("runtime metric %q has non-increasing histogram bounds", name)
		}
	}
	return soakRuntimeHistogram{
		buckets: append([]float64(nil), histogram.Buckets...),
		counts:  append([]uint64(nil), histogram.Counts...),
	}, nil
}

func (s soakRuntimeSnapshot) Delta(warm soakRuntimeSnapshot) (soakRuntimeSnapshot, error) {
	if s.allocatedBytes < warm.allocatedBytes {
		return soakRuntimeSnapshot{}, fmt.Errorf(
			"allocated bytes counter decreased from %d to %d",
			warm.allocatedBytes,
			s.allocatedBytes,
		)
	}
	if s.gcCPUSeconds < warm.gcCPUSeconds {
		return soakRuntimeSnapshot{}, fmt.Errorf(
			"GC CPU counter decreased from %g to %g",
			warm.gcCPUSeconds,
			s.gcCPUSeconds,
		)
	}
	gcPause, err := s.gcPause.Delta(warm.gcPause)
	if err != nil {
		return soakRuntimeSnapshot{}, fmt.Errorf("GC pause delta: %w", err)
	}
	schedulerOtherPause, err := s.schedulerOtherPause.Delta(warm.schedulerOtherPause)
	if err != nil {
		return soakRuntimeSnapshot{}, fmt.Errorf("scheduler-other pause delta: %w", err)
	}
	return soakRuntimeSnapshot{
		allocatedBytes:      s.allocatedBytes - warm.allocatedBytes,
		gcCPUSeconds:        s.gcCPUSeconds - warm.gcCPUSeconds,
		gcPause:             gcPause,
		schedulerOtherPause: schedulerOtherPause,
	}, nil
}

func (h soakRuntimeHistogram) Delta(warm soakRuntimeHistogram) (soakRuntimeHistogram, error) {
	if len(h.buckets) != len(warm.buckets) || len(h.counts) != len(warm.counts) ||
		len(h.buckets) != len(h.counts)+1 {
		return soakRuntimeHistogram{}, fmt.Errorf("histogram shape mismatch")
	}
	for index := range h.buckets {
		if h.buckets[index] != warm.buckets[index] {
			return soakRuntimeHistogram{}, fmt.Errorf("histogram bucket boundaries changed")
		}
	}
	delta := soakRuntimeHistogram{
		buckets: append([]float64(nil), h.buckets...),
		counts:  make([]uint64, len(h.counts)),
	}
	for index := range h.counts {
		if h.counts[index] < warm.counts[index] {
			return soakRuntimeHistogram{}, fmt.Errorf(
				"histogram bucket %d decreased from %d to %d",
				index,
				warm.counts[index],
				h.counts[index],
			)
		}
		delta.counts[index] = h.counts[index] - warm.counts[index]
	}
	return delta, nil
}

func (h soakRuntimeHistogram) Count() uint64 {
	var total uint64
	for _, count := range h.counts {
		total += count
	}
	return total
}

func (h soakRuntimeHistogram) Quantile(quantile float64) float64 {
	if quantile < 0 || quantile > 1 {
		return 0
	}
	total := h.Count()
	if total == 0 || len(h.buckets) != len(h.counts)+1 {
		return 0
	}
	rank := uint64(math.Ceil(quantile * float64(total)))
	if rank == 0 {
		rank = 1
	}
	var cumulative uint64
	for index, count := range h.counts {
		cumulative += count
		if rank <= cumulative {
			return h.buckets[index+1]
		}
	}
	return math.Inf(1)
}

func TestSoakLatencyHistogramQuantiles(t *testing.T) {
	var histogram soakLatencyHistogram
	for range 500 {
		histogram.Observe(50 * time.Microsecond)
	}
	for range 450 {
		histogram.Observe(100 * time.Microsecond)
	}
	for range 40 {
		histogram.Observe(200 * time.Microsecond)
	}
	for range 10 {
		histogram.Observe(500 * time.Microsecond)
	}
	snapshot := histogram.Snapshot()
	if snapshot.Count() != 1_000 {
		t.Fatalf("latency sample count = %d, want 1000", snapshot.Count())
	}
	for _, test := range []struct {
		name     string
		quantile float64
		want     time.Duration
	}{
		{name: "p50", quantile: 0.50, want: 50 * time.Microsecond},
		{name: "p95", quantile: 0.95, want: 100 * time.Microsecond},
		{name: "p99", quantile: 0.99, want: 200 * time.Microsecond},
		{name: "p999", quantile: 0.999, want: 500 * time.Microsecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := snapshot.Quantile(test.quantile); got != test.want {
				t.Fatalf("latency %s = %s, want %s", test.name, got, test.want)
			}
		})
	}
}

func TestSoakLatencyHistogramDeltaAndOverflow(t *testing.T) {
	var histogram soakLatencyHistogram
	histogram.Observe(100 * time.Microsecond)
	warm := histogram.Snapshot()
	histogram.Observe(time.Duration(math.MaxInt64))
	end := histogram.Snapshot()
	delta, err := end.Delta(warm)
	if err != nil {
		t.Fatalf("latency delta error = %v", err)
	}
	if delta.Count() != 1 {
		t.Fatalf("latency delta count = %d, want 1", delta.Count())
	}
	if got := delta.Quantile(0.99); got != time.Duration(math.MaxInt64) {
		t.Fatalf("latency overflow quantile = %d, want MaxInt64", got)
	}
	if got := (soakLatencySnapshot{}).Quantile(0.99); got != 0 {
		t.Fatalf("empty latency quantile = %d, want 0", got)
	}
	if _, err := warm.Delta(end); err == nil {
		t.Fatal("latency counter reset was accepted")
	}
}

func TestSoakRuntimeHistogramDeltaAndQuantiles(t *testing.T) {
	warm := soakRuntimeHistogram{
		buckets: []float64{math.Inf(-1), 0.001, 0.01, 0.1, math.Inf(1)},
		counts:  []uint64{1, 2, 3, 4},
	}
	end := soakRuntimeHistogram{
		buckets: append([]float64(nil), warm.buckets...),
		counts:  []uint64{2, 4, 6, 8},
	}
	delta, err := end.Delta(warm)
	if err != nil {
		t.Fatalf("runtime histogram delta error = %v", err)
	}
	if delta.Count() != 10 {
		t.Fatalf("runtime histogram delta count = %d, want 10", delta.Count())
	}
	if got := delta.Quantile(0.50); got != 0.1 {
		t.Fatalf("runtime histogram p50 = %g, want 0.1", got)
	}
	if got := delta.Quantile(0.99); !math.IsInf(got, 1) {
		t.Fatalf("runtime histogram p99 = %g, want +Inf", got)
	}

	badShape := soakRuntimeHistogram{buckets: []float64{0, 1}, counts: []uint64{1, 2}}
	if _, err := end.Delta(badShape); err == nil {
		t.Fatal("runtime histogram shape mismatch was accepted")
	}
	reset := soakRuntimeHistogram{
		buckets: append([]float64(nil), warm.buckets...),
		counts:  []uint64{0, 4, 6, 8},
	}
	if _, err := reset.Delta(warm); err == nil {
		t.Fatal("runtime histogram counter reset was accepted")
	}
}

func TestSoakRuntimeSnapshotDeltaRejectsCounterReset(t *testing.T) {
	histogram := soakRuntimeHistogram{
		buckets: []float64{math.Inf(-1), 1, math.Inf(1)},
		counts:  []uint64{1, 1},
	}
	warm := soakRuntimeSnapshot{
		allocatedBytes:      100,
		gcCPUSeconds:        2,
		gcPause:             histogram,
		schedulerOtherPause: histogram,
	}
	end := soakRuntimeSnapshot{
		allocatedBytes:      99,
		gcCPUSeconds:        1,
		gcPause:             histogram,
		schedulerOtherPause: histogram,
	}
	if _, err := end.Delta(warm); err == nil {
		t.Fatal("runtime counter reset was accepted")
	}
}
