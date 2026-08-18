package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	LoggerBatchOutcomeCapacityDropped = "capacity_dropped"
	LoggerBatchOutcomeStoppedDropped  = "stopped_dropped"
	LoggerBatchOutcomeDeliveryFailed  = "delivery_failed"
	LoggerBatchOutcomeDeliveryTimeout = "delivery_timeout"
	LoggerBatchOutcomeShutdownTimeout = "shutdown_timeout"
)

var (
	LoggerBatchPendingEntries *prometheus.GaugeVec
	LoggerBatchEvents         *prometheus.CounterVec
)

// LoggerBatchObserver owns the lifecycle of one logger batch processor's
// metric series. It is safe to use before metrics.Init(); calls are retained
// in the lifecycle state and become visible when vectors are initialized.
type LoggerBatchObserver interface {
	SetBuffered(int)
	AddPending(int)
	AddEvent(string) bool
	Close()
}

type loggerBatchObserver struct {
	pluginID   string
	batchName  string
	routeID    string
	serverAddr string

	mu       sync.Mutex
	closed   bool
	pending  int
	buffered int
}

type loggerBatchSeries struct {
	refs     int
	pending  int
	buffered int
}

var loggerBatchState = struct {
	sync.Mutex
	pending  map[string]*loggerBatchSeries
	buffered map[string]*loggerBatchSeries
}{
	pending:  make(map[string]*loggerBatchSeries),
	buffered: make(map[string]*loggerBatchSeries),
}

var loggerBatchPluginIDs = map[string]struct{}{
	"clickhouse-logger":    {},
	"datadog":              {},
	"elasticsearch-logger": {},
	"error-log-logger":     {},
	"file-logger":          {},
	"google-cloud-logging": {},
	"http-logger":          {},
	"kafka-logger":         {},
	"lago":                 {},
	"loggly":               {},
	"loki-logger":          {},
	"rocketmq-logger":      {},
	"skywalking-logger":    {},
	"sls-logger":           {},
	"splunk-hec-logging":   {},
	"syslog":               {},
	"tcp-logger":           {},
	"tencent-cloud-cls":    {},
	"udp-logger":           {},
	"zipkin":               {},
}

func AcquireLoggerBatchObserver(pluginID, batchName, routeID, serverAddr string) LoggerBatchObserver {
	if _, ok := loggerBatchPluginIDs[pluginID]; !ok {
		return &loggerBatchObserver{closed: true}
	}
	observer := &loggerBatchObserver{
		pluginID:   pluginID,
		batchName:  batchName,
		routeID:    routeID,
		serverAddr: serverAddr,
	}
	loggerBatchState.Lock()
	pendingKey := observer.pendingKey()
	series := loggerBatchState.pending[pendingKey]
	if series == nil {
		series = &loggerBatchSeries{}
		loggerBatchState.pending[pendingKey] = series
	}
	series.refs++
	if observer.bufferedLabelsValid() {
		bufferedKey := observer.bufferedKey()
		series = loggerBatchState.buffered[bufferedKey]
		if series == nil {
			series = &loggerBatchSeries{}
			loggerBatchState.buffered[bufferedKey] = series
		}
		series.refs++
	}
	loggerBatchState.Unlock()
	return observer
}

func (o *loggerBatchObserver) SetBuffered(value int) {
	if o == nil || !o.valid() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || !o.bufferedLabelsValid() {
		return
	}
	loggerBatchState.Lock()
	series := loggerBatchState.buffered[o.bufferedKey()]
	if series == nil {
		loggerBatchState.Unlock()
		return
	}
	series.buffered += value - o.buffered
	o.buffered = value
	if BatchProcessEntries != nil {
		BatchProcessEntries.WithLabelValues(o.batchName, o.routeID, o.serverAddr).Set(float64(series.buffered))
	}
	loggerBatchState.Unlock()
}

func (o *loggerBatchObserver) AddPending(delta int) {
	if o == nil || !o.valid() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	loggerBatchState.Lock()
	series := loggerBatchState.pending[o.pendingKey()]
	if series == nil {
		loggerBatchState.Unlock()
		return
	}
	updated := o.pending + delta
	updated = max(updated, 0)
	applied := updated - o.pending
	o.pending = updated
	series.pending += applied
	if series.pending < 0 {
		series.pending = 0
	}
	if LoggerBatchPendingEntries != nil {
		LoggerBatchPendingEntries.WithLabelValues(o.pluginID, o.routeID, o.serverAddr).Set(float64(series.pending))
	}
	loggerBatchState.Unlock()
}

func (o *loggerBatchObserver) AddEvent(outcome string) bool {
	if o == nil || !o.valid() || !validLoggerBatchOutcome(outcome) {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return false
	}
	if LoggerBatchEvents != nil {
		LoggerBatchEvents.WithLabelValues(o.pluginID, outcome).Inc()
	}
	return true
}

func (o *loggerBatchObserver) Close() {
	if o == nil || !o.valid() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	loggerBatchState.Lock()
	pendingKey := o.pendingKey()
	if series := loggerBatchState.pending[pendingKey]; series != nil {
		series.pending -= o.pending
		if series.pending < 0 {
			series.pending = 0
		}
		series.refs--
		if series.refs <= 0 {
			delete(loggerBatchState.pending, pendingKey)
			if LoggerBatchPendingEntries != nil {
				LoggerBatchPendingEntries.DeleteLabelValues(o.pluginID, o.routeID, o.serverAddr)
			}
		} else if LoggerBatchPendingEntries != nil {
			LoggerBatchPendingEntries.WithLabelValues(o.pluginID, o.routeID, o.serverAddr).Set(float64(series.pending))
		}
	}
	if o.bufferedLabelsValid() {
		bufferedKey := o.bufferedKey()
		if series := loggerBatchState.buffered[bufferedKey]; series != nil {
			series.buffered -= o.buffered
			if series.buffered < 0 {
				series.buffered = 0
			}
			series.refs--
			if series.refs <= 0 {
				delete(loggerBatchState.buffered, bufferedKey)
				if BatchProcessEntries != nil {
					BatchProcessEntries.DeleteLabelValues(o.batchName, o.routeID, o.serverAddr)
				}
			} else if BatchProcessEntries != nil {
				BatchProcessEntries.WithLabelValues(o.batchName, o.routeID, o.serverAddr).Set(float64(series.buffered))
			}
		}
	}
	loggerBatchState.Unlock()
}

func (o *loggerBatchObserver) valid() bool {
	if o == nil {
		return false
	}
	_, ok := loggerBatchPluginIDs[o.pluginID]
	return ok
}

func (o *loggerBatchObserver) pendingKey() string {
	return o.pluginID + "\x00" + o.routeID + "\x00" + o.serverAddr
}

func (o *loggerBatchObserver) bufferedKey() string {
	return o.batchName + "\x00" + o.routeID + "\x00" + o.serverAddr
}

func (o *loggerBatchObserver) bufferedLabelsValid() bool {
	return o.batchName != "" && o.routeID != "" && o.serverAddr != ""
}

func validLoggerBatchOutcome(outcome string) bool {
	switch outcome {
	case LoggerBatchOutcomeCapacityDropped,
		LoggerBatchOutcomeStoppedDropped,
		LoggerBatchOutcomeDeliveryFailed,
		LoggerBatchOutcomeDeliveryTimeout,
		LoggerBatchOutcomeShutdownTimeout:
		return true
	default:
		return false
	}
}

func newLoggerBatchMetricVectors(
	registry *prometheus.Registry,
	prefix string,
) (*prometheus.GaugeVec, *prometheus.CounterVec) {
	pending := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: prefix + "logger_batch_pending_entries",
			Help: "Accepted nonterminal logger batch entries",
		}, []string{"plugin", "route_id", "server_addr"},
	)
	events := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prefix + "logger_batch_events_total",
			Help: "Logger batch terminal outcomes",
		}, []string{"plugin", "outcome"},
	)
	if registry != nil {
		registry.MustRegister(pending, events)
	}
	return pending, events
}

func initLoggerBatchMetrics(prefix string) {
	LoggerBatchPendingEntries, LoggerBatchEvents = newLoggerBatchMetricVectors(nil, prefix)
	loggerBatchState.Lock()
	defer loggerBatchState.Unlock()
	for key, series := range loggerBatchState.pending {
		pluginID, routeID, serverAddr := splitLoggerBatchKey(key)
		if series.pending > 0 {
			LoggerBatchPendingEntries.WithLabelValues(pluginID, routeID, serverAddr).Set(float64(series.pending))
		}
	}
	for key, series := range loggerBatchState.buffered {
		batchName, routeID, serverAddr := splitLoggerBatchKey(key)
		if series.buffered > 0 && BatchProcessEntries != nil {
			BatchProcessEntries.WithLabelValues(batchName, routeID, serverAddr).Set(float64(series.buffered))
		}
	}
}

func splitLoggerBatchKey(key string) (string, string, string) {
	parts := make([]string, 0, 3)
	for len(parts) < 3 {
		index := -1
		for i := 0; i < len(key); i++ {
			if key[i] == 0 {
				index = i
				break
			}
		}
		if index < 0 {
			parts = append(parts, key)
			break
		}
		parts = append(parts, key[:index])
		key = key[index+1:]
	}
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
}
