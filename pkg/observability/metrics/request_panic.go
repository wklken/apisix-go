package metrics

import "github.com/prometheus/client_golang/prometheus"

const requestPanicsMetric = "http_request_panics_total"

// RequestPanicOwner identifies one bounded panic ownership boundary.
type RequestPanicOwner string

const (
	RequestPanicPlugin          RequestPanicOwner = "plugin"
	RequestPanicCore            RequestPanicOwner = "core"
	RequestPanicPluginFinalizer RequestPanicOwner = "plugin_finalizer"
	RequestPanicCoreFinalizer   RequestPanicOwner = "core_finalizer"
)

// RequestPanicStage identifies one of the bounded request panic boundaries.
type RequestPanicStage string

const (
	RequestPanicPreCommit  RequestPanicStage = "pre_commit"
	RequestPanicPostCommit RequestPanicStage = "post_commit"
	RequestPanicPostFlush  RequestPanicStage = "post_flush"
	RequestPanicPostHijack RequestPanicStage = "post_hijack"
	RequestPanicFinalizer  RequestPanicStage = "finalizer"
)

// requestPanics counts recovered request and finalizer panics by bounded stage.
// It is private so all callers must pass through RecordRequestPanic's enum
// validation. It is initialized by Init and intentionally remains nil before then.
var requestPanics *prometheus.CounterVec

func newRequestPanicMetrics(registry *prometheus.Registry, prefix string) *prometheus.CounterVec {
	metric := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prefix + requestPanicsMetric,
			Help: "Recovered HTTP request panics by bounded owner and stage",
		},
		[]string{"owner", "stage"},
	)
	if registry != nil {
		registry.MustRegister(metric)
	}
	return metric
}

// RecordRequestPanic records a panic at a known boundary. Invalid owners or
// stages are ignored so arbitrary panic values never become metric labels.
func RecordRequestPanic(owner RequestPanicOwner, stage RequestPanicStage) {
	if !validRequestPanicOwner(owner) || !validRequestPanicStage(stage) || requestPanics == nil {
		return
	}
	requestPanics.WithLabelValues(string(owner), string(stage)).Inc()
}

func validRequestPanicOwner(owner RequestPanicOwner) bool {
	switch owner {
	case RequestPanicPlugin, RequestPanicCore, RequestPanicPluginFinalizer, RequestPanicCoreFinalizer:
		return true
	default:
		return false
	}
}

func validRequestPanicStage(stage RequestPanicStage) bool {
	switch stage {
	case RequestPanicPreCommit, RequestPanicPostCommit, RequestPanicPostFlush,
		RequestPanicPostHijack, RequestPanicFinalizer:
		return true
	default:
		return false
	}
}
