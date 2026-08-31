package metrics

import (
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/wklken/apisix-go/pkg/generation"
)

func serviceableConfigApplyAcknowledgement(
	domains ...generation.Domain,
) map[generation.Domain][]generation.ResourceDecision {
	decisions := make(map[generation.Domain][]generation.ResourceDecision, len(domains))
	for _, domain := range domains {
		decisions[domain] = nil
	}
	return decisions
}

func TestConfigApplyMetricsAreNilSafeBeforeInit(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures, ConfigApplyReady = nil, nil
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	RecordConfigApplyStageFailure(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageFailure(ConfigApplyStageHTTPRoutes)
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
}

func TestConfigApplyMetricsUseFixedNoLabelCardinality(t *testing.T) {
	registry := prometheus.NewRegistry()
	failures, ready := newConfigApplyMetrics(registry, "apisix_")
	failures.Inc()
	ready.Set(1)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != 2 {
		t.Fatalf("metric families = %d, want 2", len(families))
	}
	for _, family := range families {
		if got := len(family.GetMetric()); got != 1 {
			t.Fatalf("%s series = %d, want 1", family.GetName(), got)
		}
		if labels := family.GetMetric()[0].GetLabel(); len(labels) != 0 {
			t.Fatalf("%s labels = %v, want none", family.GetName(), labels)
		}
	}
}

func TestRecordConfigApplyUpdatesFailureAndReady(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_config_apply_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_config_apply_ready"})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	if got := gaugeValue(t, ConfigApplyReady); got != 0 {
		t.Fatalf("ready after provider-only success = %v, want 0", got)
	}
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := gaugeValue(t, ConfigApplyReady); got != 1 {
		t.Fatalf("ready after provider and HTTP success = %v, want 1", got)
	}
	RecordConfigApplyStageFailure(ConfigApplyStageProvider)
	if got := counterValue(t, ConfigApplyFailures); got != 1 {
		t.Fatalf("failure count = %v, want 1", got)
	}
	if got := gaugeValue(t, ConfigApplyReady); got != 0 {
		t.Fatalf("ready after failure = %v, want 0", got)
	}
}

func TestRecordConfigApplyAttemptFailureIncrementsOnlyCounter(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_config_apply_attempt_failures_total",
	})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_config_apply_attempt_ready",
	})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if !GetReadiness().ConfigApplyReady {
		t.Fatal("readiness = false before failed retry")
	}

	RecordConfigApplyAttemptFailure("standalone", "apply")
	if got := counterValue(t, ConfigApplyFailures); got != 1 {
		t.Fatalf("failure count = %v, want 1", got)
	}
	if !GetReadiness().ConfigApplyReady {
		t.Fatal("failed retry overwrote last acknowledged readiness")
	}
}

func TestRecordConfigApplyAcknowledgementKeepsQuarantineDiagnostic(t *testing.T) {
	oldFailures, oldReady, oldQuarantine := ConfigApplyFailures, ConfigApplyReady, ConfigApplyQuarantined
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_config_apply_ack_failures_total",
	})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_config_apply_ack_ready",
	})
	ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_config_apply_ack_quarantine",
	})
	t.Cleanup(func() {
		SetConfigApplyStreamRequired(false)
		ConfigApplyFailures, ConfigApplyReady, ConfigApplyQuarantined = oldFailures, oldReady, oldQuarantine
	})

	SetConfigApplyStreamRequired(true)
	decisions := serviceableConfigApplyAcknowledgement(generation.DomainHTTP, generation.DomainStream)
	RecordConfigApplyAcknowledgement(decisions, 0)
	if !GetReadiness().ConfigApplyReady {
		t.Fatal("acknowledged provider/http/stream publication is not ready")
	}

	RecordConfigApplyAcknowledgement(decisions, 2)
	if !GetReadiness().ConfigApplyReady {
		t.Fatal("acknowledged serviceable generation became unready because of quarantine")
	}
	if got := gaugeValue(t, ConfigApplyQuarantined); got != 2 {
		t.Fatalf("quarantine count = %v, want 2", got)
	}

	RecordConfigApplyAcknowledgement(decisions, 0)
	if !GetReadiness().ConfigApplyReady {
		t.Fatal("cleared acknowledged quarantine changed readiness")
	}
	if got := counterValue(t, ConfigApplyFailures); got != 0 {
		t.Fatalf("acknowledgements changed failure count to %v", got)
	}
}

func TestAcknowledgedDomainServiceable(t *testing.T) {
	httpKey := generation.ResourceKey{Kind: "routes", ID: "route"}
	tests := []struct {
		name      string
		decisions map[generation.Domain][]generation.ResourceDecision
		want      bool
	}{
		{name: "absent domain", decisions: nil},
		{
			name:      "empty valid domain",
			decisions: serviceableConfigApplyAcknowledgement(generation.DomainHTTP),
			want:      true,
		},
		{
			name: "fail closed only",
			decisions: map[generation.Domain][]generation.ResourceDecision{generation.DomainHTTP: {{
				Key: httpKey, Disposition: generation.DispositionFailClosed,
			}}},
		},
		{
			name: "quarantined only",
			decisions: map[generation.Domain][]generation.ResourceDecision{generation.DomainHTTP: {{
				Key: httpKey, Disposition: generation.DispositionQuarantined,
			}}},
		},
		{
			name: "published",
			decisions: map[generation.Domain][]generation.ResourceDecision{generation.DomainHTTP: {{
				Key: httpKey, Disposition: generation.DispositionPublished,
			}}},
			want: true,
		},
		{
			name: "last good",
			decisions: map[generation.Domain][]generation.ResourceDecision{generation.DomainHTTP: {{
				Key: httpKey, Disposition: generation.DispositionLastGood,
			}}},
			want: true,
		},
		{
			name: "deleted",
			decisions: map[generation.Domain][]generation.ResourceDecision{generation.DomainHTTP: {{
				Key: httpKey, Disposition: generation.DispositionDeleted,
			}}},
			want: true,
		},
		{
			name: "mixed rejected and published",
			decisions: map[generation.Domain][]generation.ResourceDecision{generation.DomainHTTP: {
				{Key: httpKey, Disposition: generation.DispositionFailClosed},
				{Key: generation.ResourceKey{Kind: "routes", ID: "kept"}, Disposition: generation.DispositionPublished},
			}},
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := acknowledgedDomainServiceable(test.decisions, generation.DomainHTTP); got != test.want {
				t.Fatalf("acknowledgedDomainServiceable() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRecordConfigApplyAcknowledgementRequiresServiceableFirstPublication(t *testing.T) {
	oldFailures, oldReady, oldQuarantine := ConfigApplyFailures, ConfigApplyReady, ConfigApplyQuarantined
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_first_ack_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_first_ack_ready"})
	ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_first_ack_quarantine"})
	t.Cleanup(func() {
		ConfigApplyFailures, ConfigApplyReady, ConfigApplyQuarantined = oldFailures, oldReady, oldQuarantine
	})

	rejected := map[generation.Domain][]generation.ResourceDecision{generation.DomainHTTP: {{
		Key:         generation.ResourceKey{Kind: "routes", ID: "invalid"},
		Disposition: generation.DispositionFailClosed,
	}}}
	RecordConfigApplyAcknowledgement(rejected, 1)
	if GetReadiness().ConfigApplyReady {
		t.Fatal("rejected-only first acknowledgement established readiness")
	}
	if got := gaugeValue(t, ConfigApplyQuarantined); got != 1 {
		t.Fatalf("quarantine count = %v, want 1", got)
	}

	RecordConfigApplyAcknowledgement(serviceableConfigApplyAcknowledgement(generation.DomainHTTP), 0)
	if !GetReadiness().ConfigApplyReady {
		t.Fatal("valid empty acknowledgement did not establish readiness")
	}
	RecordConfigApplyAcknowledgement(rejected, 1)
	if !GetReadiness().ConfigApplyReady {
		t.Fatal("later rejected-only acknowledgement erased serviceable readiness")
	}
}

func TestConfigApplyStreamReadinessIsOptional(t *testing.T) {
	oldFailures, oldReady, oldQuarantine := ConfigApplyFailures, ConfigApplyReady, ConfigApplyQuarantined
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_stream_optional_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_stream_optional_ready"})
	ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_stream_optional_quarantine"})
	t.Cleanup(func() {
		ConfigApplyFailures, ConfigApplyReady, ConfigApplyQuarantined = oldFailures, oldReady, oldQuarantine
		SetConfigApplyStreamRequired(false)
	})

	SetConfigApplyStreamRequired(false)
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("HTTP-only config apply readiness = false after provider and HTTP success")
	}

	SetConfigApplyStreamRequired(true)
	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("stream-required config apply readiness = true before stream success")
	}
	RecordConfigApplyStageSuccess(ConfigApplyStageStreams)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("stream-required config apply readiness = false after all stages succeeded")
	}

	SetConfigApplyStreamRequired(false)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("HTTP-only config apply readiness = false after stream requirement disabled")
	}
}

func TestConfigApplyStreamFailureBlocksAndRecoversReadiness(t *testing.T) {
	oldFailures, oldReady, oldQuarantine := ConfigApplyFailures, ConfigApplyReady, ConfigApplyQuarantined
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_stream_failure_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_stream_failure_ready"})
	ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_stream_failure_quarantine"})
	t.Cleanup(func() {
		ConfigApplyFailures, ConfigApplyReady, ConfigApplyQuarantined = oldFailures, oldReady, oldQuarantine
		SetConfigApplyStreamRequired(false)
	})

	SetConfigApplyStreamRequired(true)
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	RecordConfigApplyStageSuccess(ConfigApplyStageStreams)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("config apply readiness = false after initial stream success")
	}

	RecordConfigApplyStageFailure(ConfigApplyStageStreams)
	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("config apply readiness = true after stream failure")
	}
	if got := gaugeValue(t, ConfigApplyReady); got != 0 {
		t.Fatalf("ready gauge after stream failure = %v, want 0", got)
	}
	if got := counterValue(t, ConfigApplyFailures); got != 1 {
		t.Fatalf("failure counter after stream failure = %v, want 1", got)
	}

	RecordConfigApplyStageSuccess(ConfigApplyStageStreams)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("config apply readiness = false after stream recovery")
	}
	if got := counterValue(t, ConfigApplyFailures); got != 1 {
		t.Fatalf("failure counter after stream recovery = %v, want unchanged 1", got)
	}
}

func TestConfigApplyCollectorReplacementResetsStreamState(t *testing.T) {
	oldFailures, oldReady, oldQuarantine := ConfigApplyFailures, ConfigApplyReady, ConfigApplyQuarantined
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_stream_reset_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_stream_reset_ready"})
	ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_stream_reset_quarantine"})
	t.Cleanup(func() {
		ConfigApplyFailures, ConfigApplyReady, ConfigApplyQuarantined = oldFailures, oldReady, oldQuarantine
		SetConfigApplyStreamRequired(false)
	})

	SetConfigApplyStreamRequired(true)
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	RecordConfigApplyStageSuccess(ConfigApplyStageStreams)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("config apply readiness = false before collector replacement")
	}

	ConfigApplyFailures = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "test_stream_reset_replaced_failures_total"},
	)
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_stream_reset_replaced_ready"})
	ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_stream_reset_replaced_quarantine"})
	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("config apply readiness = true after collector replacement")
	}

	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("HTTP-only config apply readiness = false after collector replacement reset")
	}
}

func TestConfigApplyStagesKeepReadinessBlockedIndependently(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_config_apply_stage_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_config_apply_stage_ready"})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	RecordConfigApplyStageFailure(ConfigApplyStageHTTPRoutes)
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	if got := gaugeValue(t, ConfigApplyReady); got != 0 {
		t.Fatalf("ready after provider success = %v, want 0", got)
	}
	if got := counterValue(t, ConfigApplyFailures); got != 1 {
		t.Fatalf("failure count after HTTP stage failure = %v, want 1", got)
	}

	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := gaugeValue(t, ConfigApplyReady); got != 1 {
		t.Fatalf("ready after both stages recover = %v, want 1", got)
	}

	RecordConfigApplyStageFailure(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := gaugeValue(t, ConfigApplyReady); got != 0 {
		t.Fatalf("ready after provider failure and HTTP success = %v, want 0", got)
	}
	if got := counterValue(t, ConfigApplyFailures); got != 2 {
		t.Fatalf("failure count after provider stage failure = %v, want 2", got)
	}
}

func TestConfigApplyStagesAreConcurrencySafe(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_config_apply_stage_concurrent_failures_total",
	})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_config_apply_stage_concurrent_ready"})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	const calls = 64
	var group sync.WaitGroup
	group.Add(calls)
	for index := range calls {
		go func(index int) {
			defer group.Done()
			stage := ConfigApplyStageProvider
			if index%2 == 0 {
				stage = ConfigApplyStageHTTPRoutes
			}
			RecordConfigApplyStageFailure(stage)
		}(index)
	}
	group.Wait()

	if got := counterValue(t, ConfigApplyFailures); got != calls {
		t.Fatalf("concurrent failure count = %v, want %d", got, calls)
	}
	if got := gaugeValue(t, ConfigApplyReady); got != 0 {
		t.Fatalf("ready after concurrent failures = %v, want 0", got)
	}

	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := gaugeValue(t, ConfigApplyReady); got != 1 {
		t.Fatalf("ready after concurrent stage recovery = %v, want 1", got)
	}
}

func TestGetReadinessRequiresBothConfigApplyStages(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_readiness_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_readiness_ready"})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("config apply readiness = true before either stage was observed")
	}
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("config apply readiness = true before HTTP route stage was observed")
	}
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("config apply readiness = false after both stages succeeded")
	}

	RecordConfigApplyStageFailure(ConfigApplyStageProvider)
	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("config apply readiness = true after provider stage failed")
	}
}

func TestGetReadinessResetsWhenConfigMetricsAreReplaced(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_readiness_reset_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_readiness_reset_ready"})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("config apply readiness = false before collector replacement")
	}

	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_readiness_reset_failures_replaced_total",
	})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_readiness_reset_ready_replaced"})
	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("config apply readiness = true after collector replacement")
	}
}

func TestConfigApplyQuarantineMetricHasNoLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	gauge := newConfigApplyQuarantineMetric(registry, "apisix_")
	gauge.Set(3)
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != 1 || len(families[0].GetMetric()) != 1 {
		t.Fatalf(
			"quarantine metric families = %d, samples = %d, want one each",
			len(families),
			len(families[0].GetMetric()),
		)
	}
	if labels := families[0].GetMetric()[0].GetLabel(); len(labels) != 0 {
		t.Fatalf("quarantine metric labels = %v, want none", labels)
	}
}
