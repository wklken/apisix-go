package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
)

func TestHTTPSeriesBudgetAdmitsExactLimitAndReusesExistingTuples(t *testing.T) {
	overflow := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_http_series_overflow"})
	budget := newHTTPSeriesBudget(2, overflow, []int{1})

	first := []string{"bounded-a", "dynamic-a"}
	second := []string{"bounded-b", "dynamic-b"}
	if got := budget.admit(first); !reflect.DeepEqual(got, first) {
		t.Fatalf("first admission = %#v, want %#v", got, first)
	}
	if got := budget.admit(second); !reflect.DeepEqual(got, second) {
		t.Fatalf("second admission = %#v, want %#v", got, second)
	}
	if got := budget.admit(first); !reflect.DeepEqual(got, first) {
		t.Fatalf("existing tuple admission = %#v, want %#v", got, first)
	}
	if len(budget.seen) != 2 {
		t.Fatalf("seen tuples = %d, want 2", len(budget.seen))
	}
}

func TestHTTPSeriesBudgetOverflowSubstitutesOnlyDynamicIndexes(t *testing.T) {
	overflow := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_http_series_overflow_dynamic"})
	budget := newHTTPSeriesBudget(1, overflow, []int{1, 3})
	if got := budget.admit([]string{"bounded", "dynamic", "bounded", "dynamic"}); got[0] != "bounded" {
		t.Fatalf("first tuple = %#v, want admitted", got)
	}

	input := []string{"bounded-2", "dynamic-2", "bounded-2b", "dynamic-2b"}
	got := budget.admit(input)
	want := []string{"bounded-2", overflowLabel, "bounded-2b", overflowLabel}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overflow tuple = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(input, []string{"bounded-2", "dynamic-2", "bounded-2b", "dynamic-2b"}) {
		t.Fatalf("overflow mutated input = %#v", input)
	}
	if len(budget.seen) != 1 {
		t.Fatalf("seen tuples after overflow = %d, want 1", len(budget.seen))
	}
}

func TestHTTPSeriesBudgetOverflowSubstitutesAllExtraLabels(t *testing.T) {
	overflow := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_http_series_overflow_extra"})
	budget := newHTTPSeriesBudgetWithTail(1, overflow, []int{1}, 3)
	budget.admit([]string{"bounded", "dynamic", "bounded", "extra-a", "extra-b"})
	got := budget.admit([]string{"bounded-2", "dynamic-2", "bounded-2b", "extra-c", "extra-d"})
	want := []string{"bounded-2", overflowLabel, "bounded-2b", overflowLabel, overflowLabel}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overflow extra labels = %#v, want %#v", got, want)
	}
}

func TestHTTPSeriesBudgetPreservesRequestTypeForLatencyAndBandwidth(t *testing.T) {
	overflow := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_http_series_overflow_request_type"})
	for _, family := range []string{httpLatencyMetric, bandwidthMetric} {
		budget := newHTTPSeriesBudgetWithTail(1, overflow, []int{1, 2, 3, 4, 6, 7}, 8)
		budget.admit([]string{"request", "route-a", "service-a", "consumer-a", "node-a", "type-a", "model-a", "llm-a"})
		got := budget.admit(
			[]string{"request", "route-b", "service-b", "consumer-b", "node-b", "type-b", "model-b", "llm-b"},
		)
		if got[5] != "type-b" {
			t.Fatalf("%s request_type = %q, want bounded type-b", family, got[5])
		}
	}
}

func TestHTTPSeriesBudgetTupleKeyIsCollisionResistant(t *testing.T) {
	overflow := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_http_series_overflow_key"})
	budget := newHTTPSeriesBudget(2, overflow, nil)
	left := []string{"a", "bc"}
	right := []string{"ab", "c"}
	budget.admit(left)
	budget.admit(right)
	if len(budget.seen) != 2 {
		t.Fatalf("seen tuples = %d, want collision-free two tuples", len(budget.seen))
	}
}

func TestHTTPSeriesBudgetsAreIndependent(t *testing.T) {
	overflow := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_http_series_overflow_independent"})
	first := newHTTPSeriesBudget(1, overflow, nil)
	second := newHTTPSeriesBudget(1, overflow, nil)
	first.admit([]string{"first"})
	first.admit([]string{"overflow"})
	if got := second.admit([]string{"second"}); !reflect.DeepEqual(got, []string{"second"}) {
		t.Fatalf("second family admission = %#v, want second tuple", got)
	}
}

func TestHTTPMetricSeriesOverflowMetricUsesFamilyLabel(t *testing.T) {
	registry := prometheus.NewRegistry()
	metric := newHTTPMetricSeriesOverflow("apisix_")
	registry.MustRegister(metric)
	metric.WithLabelValues(httpStatusMetric).Inc()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "apisix_http_metric_series_overflow_total" {
		t.Fatalf("metric families = %#v, want default overflow metric", families)
	}
	if got := families[0].Metric[0].GetLabel()[0].GetValue(); got != httpStatusMetric {
		t.Fatalf("metric label = %q, want %q", got, httpStatusMetric)
	}
}

func TestHTTPSeriesBudgetIsSafeAtConcurrentBoundary(t *testing.T) {
	overflow := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_http_series_overflow_race"})
	budget := newHTTPSeriesBudget(10, overflow, []int{0})
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			budget.admit([]string{fmt.Sprintf("tuple-%d", i)})
		}(i)
	}
	wg.Wait()
	if len(budget.seen) > 10 {
		t.Fatalf("seen tuples = %d, want no more than 10", len(budget.seen))
	}
}

func TestPrometheusMetricConfigHTTPSeriesLimit(t *testing.T) {
	tests := []struct {
		name    string
		raw     any
		want    int
		wantErr bool
	}{
		{name: "default", want: defaultMaxMetricSeries},
		{name: "minimum", raw: minMetricSeries, want: minMetricSeries},
		{name: "maximum", raw: maxMetricSeries, want: maxMetricSeries},
		{name: "int64", raw: int64(250), want: 250},
		{name: "invalid string", raw: "1000", wantErr: true},
		{name: "invalid bool", raw: true, wantErr: true},
		{name: "invalid fractional", raw: 100.5, wantErr: true},
		{name: "below minimum", raw: minMetricSeries - 1, wantErr: true},
		{name: "above maximum", raw: maxMetricSeries + 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attr := map[string]any{}
			if test.raw != nil {
				attr["max_http_series"] = test.raw
			}
			cfg, err := newPrometheusMetricConfig(attr)
			if test.wantErr {
				if err == nil {
					t.Fatal("newPrometheusMetricConfig() error = nil")
				}
				if !strings.Contains(err.Error(), "plugin_attr.prometheus.max_http_series") {
					t.Fatalf("error = %v, want full config field", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("newPrometheusMetricConfig() error = %v", err)
			}
			if cfg.MaxHTTPSeries != test.want {
				t.Fatalf("MaxHTTPSeries = %d, want %d", cfg.MaxHTTPSeries, test.want)
			}
		})
	}
}

func installBudgetedHTTPVectors(t *testing.T, limit int, extraLabels map[string][]prometheusExtraLabel) func() {
	t.Helper()
	restoreVectors := installMetricVectors(t, "test_budget_")
	old := struct {
		status    *httpSeriesBudget
		latency   *httpSeriesBudget
		bandwidth *httpSeriesBudget
		overflow  *prometheus.CounterVec
	}{httpStatusBudget, httpLatencyBudget, bandwidthBudget, httpSeriesOverflow}
	prometheusExtraLabels = extraLabels
	HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_budget_http_status"},
		metricLabelNames(httpStatusMetric, []string{
			"code", "route", "matched_uri", "matched_host", "service", "consumer", "node",
			"request_type", "request_llm_model", "llm_model", "response_source",
		}),
	)
	HttpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_budget_http_latency"},
		metricLabelNames(httpLatencyMetric, []string{
			"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model",
		}),
	)
	Bandwidth = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_budget_bandwidth"},
		metricLabelNames(bandwidthMetric, []string{
			"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model",
		}),
	)
	httpSeriesOverflow = newHTTPMetricSeriesOverflow("test_budget_")
	httpStatusBudget = newHTTPSeriesBudgetWithTail(
		limit,
		httpSeriesOverflow.WithLabelValues(httpStatusMetric),
		[]int{1, 2, 3, 4, 5, 6, 8, 9},
		11,
	)
	httpLatencyBudget = newHTTPSeriesBudgetWithTail(
		limit,
		httpSeriesOverflow.WithLabelValues(httpLatencyMetric),
		[]int{1, 2, 3, 4, 6, 7},
		8,
	)
	bandwidthBudget = newHTTPSeriesBudgetWithTail(
		limit,
		httpSeriesOverflow.WithLabelValues(bandwidthMetric),
		[]int{1, 2, 3, 4, 6, 7},
		8,
	)
	return func() {
		httpStatusBudget, httpLatencyBudget, bandwidthBudget, httpSeriesOverflow = old.status, old.latency, old.bandwidth, old.overflow
		restoreVectors()
	}
}

func TestRecordHTTPRequestUsesIndependentFamilyBudgets(t *testing.T) {
	oldStatusBudget, oldLatencyBudget, oldBandwidthBudget, oldOverflow := httpStatusBudget, httpLatencyBudget, bandwidthBudget, httpSeriesOverflow
	defer func() {
		httpStatusBudget, httpLatencyBudget, bandwidthBudget, httpSeriesOverflow = oldStatusBudget, oldLatencyBudget, oldBandwidthBudget, oldOverflow
	}()
	installMetricVectors(t, "test_budget_record_")
	request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodGet, "/", nil))
	apisixctx.RegisterRequestVar(request, "$request_type", "bounded-request-type")
	httpSeriesOverflow = newHTTPMetricSeriesOverflow("test_budget_record_")
	httpStatusBudget = newHTTPSeriesBudgetWithTail(
		1,
		httpSeriesOverflow.WithLabelValues(httpStatusMetric),
		[]int{1, 2, 3, 4, 5, 6, 8, 9},
		11,
	)
	httpLatencyBudget = newHTTPSeriesBudgetWithTail(
		1,
		httpSeriesOverflow.WithLabelValues(httpLatencyMetric),
		[]int{1, 2, 3, 4, 6, 7},
		8,
	)
	bandwidthBudget = newHTTPSeriesBudgetWithTail(
		1,
		httpSeriesOverflow.WithLabelValues(bandwidthMetric),
		[]int{1, 2, 3, 4, 6, 7},
		8,
	)

	RecordHTTPRequest(
		request,
		HTTPRequestMetrics{Status: 200, Route: "route-a", RequestLatency: 10, UpstreamLatency: 5},
	)
	RecordHTTPRequest(
		request,
		HTTPRequestMetrics{Status: 201, Route: "route-b", RequestLatency: 11, UpstreamLatency: 5},
	)

	if got := len(httpStatusBudget.seen); got != 1 {
		t.Fatalf("status admitted tuples = %d, want 1", got)
	}
	if got := len(httpLatencyBudget.seen); got != 1 {
		t.Fatalf("latency admitted tuples = %d, want 1", got)
	}
	if got := len(bandwidthBudget.seen); got != 1 {
		t.Fatalf("bandwidth admitted tuples = %d, want 1", got)
	}
	statusOverflow := httpSeriesOverflow.WithLabelValues(httpStatusMetric)
	if got := counterValue(t, statusOverflow); got == 0 {
		t.Fatal("status overflow counter = 0, want observations")
	}
	latencyOverflow := httpSeriesOverflow.WithLabelValues(httpLatencyMetric)
	if got := counterValue(t, latencyOverflow); got == 0 {
		t.Fatal("latency overflow counter = 0, want observations")
	}
	bandwidthOverflow := httpSeriesOverflow.WithLabelValues(bandwidthMetric)
	if got := counterValue(t, bandwidthOverflow); got == 0 {
		t.Fatal("bandwidth overflow counter = 0, want observations")
	}
}

func TestRecordHTTPRequestPreservesBoundedLabelsAndOverflowsExtras(t *testing.T) {
	oldStatusBudget, oldLatencyBudget, oldBandwidthBudget, oldOverflow, oldExtras := httpStatusBudget, httpLatencyBudget, bandwidthBudget, httpSeriesOverflow, prometheusExtraLabels
	defer func() {
		httpStatusBudget, httpLatencyBudget, bandwidthBudget, httpSeriesOverflow, prometheusExtraLabels = oldStatusBudget, oldLatencyBudget, oldBandwidthBudget, oldOverflow, oldExtras
	}()
	extras := map[string][]prometheusExtraLabel{
		httpStatusMetric: {{Name: "tenant", Variable: "$tenant"}},
	}
	installBudgetedHTTPVectors(t, 1, extras)
	request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodGet, "/", nil))
	apisixctx.RegisterRequestVar(request, "$request_type", "request-type-a")
	apisixctx.RegisterRequestVar(request, "$tenant", "tenant-a")
	RecordHTTPRequest(request, HTTPRequestMetrics{Status: 200, Route: "route-a", RequestLatency: 10})
	apisixctx.RegisterRequestVar(request, "$request_type", "request-type-b")
	apisixctx.RegisterRequestVar(request, "$tenant", "tenant-b")
	RecordHTTPRequest(
		request,
		HTTPRequestMetrics{Status: 201, Route: "route-b", RequestLatency: 11, UpstreamLatency: 1},
	)

	status := HttpStatus.WithLabelValues(
		"201", overflowLabel, overflowLabel, overflowLabel, overflowLabel, overflowLabel, overflowLabel,
		"request-type-b", overflowLabel, overflowLabel, "upstream", overflowLabel,
	)
	if got := counterValue(t, status); got != 1 {
		t.Fatalf("overflow status count = %v, want 1", got)
	}
}

func TestRecordHTTPRequestNormalizesInvalidStatus(t *testing.T) {
	restore := installMetricVectors(t, "test_status_normalized_")
	defer restore()
	RecordHTTPRequest(httptest.NewRequest(http.MethodGet, "/", nil), HTTPRequestMetrics{Status: 700})
	status := HttpStatus.WithLabelValues("0", "", "", "", "", "", "", "", "", "", "apisix")
	if got := counterValue(t, status); got != 1 {
		t.Fatalf("normalized status count = %v, want 1", got)
	}
}

func TestRecordHTTPRequestNormalizesStatusExtraLabels(t *testing.T) {
	oldStatusBudget, oldLatencyBudget, oldBandwidthBudget, oldOverflow, oldExtras := httpStatusBudget, httpLatencyBudget, bandwidthBudget, httpSeriesOverflow, prometheusExtraLabels
	defer func() {
		httpStatusBudget, httpLatencyBudget, bandwidthBudget, httpSeriesOverflow, prometheusExtraLabels = oldStatusBudget, oldLatencyBudget, oldBandwidthBudget, oldOverflow, oldExtras
	}()
	installBudgetedHTTPVectors(t, 10, map[string][]prometheusExtraLabel{
		httpStatusMetric: {
			{Name: "status", Variable: "$status"},
			{Name: "upstream_status", Variable: "$upstream_status"},
		},
	})
	RecordHTTPRequest(
		httptest.NewRequest(http.MethodGet, "/", nil),
		HTTPRequestMetrics{Status: 700, UpstreamLatency: 1},
	)
	status := HttpStatus.WithLabelValues(
		"0", "", "", "", "", "", "", "", "", "", "upstream", "0", "0",
	)
	if got := counterValue(t, status); got != 1 {
		t.Fatalf("status with normalized extra labels = %v, want 1", got)
	}
}

func TestInitRetainsInvalidSeriesLimitErrorWithoutPublishingMetrics(t *testing.T) {
	const childEnv = "APISIX_GO_INVALID_PROMETHEUS_INIT_CHILD"
	if os.Getenv(childEnv) == "1" {
		config.GlobalConfig = &config.Config{PluginAttr: map[string]map[string]any{
			"prometheus": {"max_http_series": "not-an-integer"},
		}}
		firstErr := Init()
		secondErr := Init()
		if firstErr == nil || secondErr == nil || firstErr.Error() != secondErr.Error() {
			t.Fatalf("Init() errors = %v and %v, want retained identical errors", firstErr, secondErr)
		}
		if HttpStatus != nil || HttpLatency != nil || Bandwidth != nil ||
			httpStatusBudget != nil || httpLatencyBudget != nil || bandwidthBudget != nil {
			t.Fatal("invalid Init() published HTTP vectors or budgets")
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run", "^TestInitRetainsInvalidSeriesLimitErrorWithoutPublishingMetrics$")
	command.Env = append(os.Environ(), childEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("invalid Init() child failed: %v\n%s", err, output)
	}
}
