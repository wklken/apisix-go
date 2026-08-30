package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestDifferentialObservationRetainsPinnedExternalAndLoggerSemanticHeaders(t *testing.T) {
	extra := []string{
		"Content-Type",
		"X-ClickHouse-Database",
		"X-ClickHouse-Key",
		"X-ClickHouse-User",
		"X-Functions-Key",
		"X-Functions-Clientid",
		"X-Scope-OrgID",
		"X-Userinfo",
	}
	headers := differentialSemanticUpstreamHeaders(http.Header{
		"Authorization":         {"Bearer token"},
		"Content-Type":          {"application/json"},
		"X-ClickHouse-Database": {"default"},
		"X-ClickHouse-Key":      {"clickhouse-key"},
		"X-ClickHouse-User":     {"default"},
		"X-Functions-Key":       {"function-key"},
		"X-Functions-Clientid":  {"function-client"},
		"X-Scope-Orgid":         {"tenant-a"},
		"X-Userinfo":            {"encoded-user"},
		"X-Unrelated":           {"ignore"},
	}, extra...)
	want := map[string][]string{
		"Authorization":         {"Bearer token"},
		"Content-Type":          {"application/json"},
		"X-ClickHouse-Database": {"default"},
		"X-ClickHouse-Key":      {"clickhouse-key"},
		"X-ClickHouse-User":     {"default"},
		"X-Functions-Key":       {"function-key"},
		"X-Functions-Clientid":  {"function-client"},
		"X-Scope-Orgid":         {"tenant-a"},
		"X-Userinfo":            {"encoded-user"},
	}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("semantic upstream headers = %#v, want exactly %#v", headers, want)
	}
}

func TestC6DifferentialCasesDeclareOnlyRequiredExtraSemanticHeaders(t *testing.T) {
	tests := []struct {
		name  string
		cases []DifferentialCase
		want  []string
	}{
		{
			name:  "azure-functions",
			cases: differentialCasesForPlugin("azure-functions"),
			want:  []string{"X-Functions-Key"},
		},
		{name: "opa", cases: differentialCasesForPlugin("opa"), want: []string{"Content-Type"}},
		{
			name:  "dingtalk-auth",
			cases: differentialCasesForPlugin("dingtalk-auth"),
			want:  []string{"Content-Type", "X-Userinfo"},
		},
		{name: "data-mask", cases: differentialCasesForPlugin("data-mask"), want: []string{"Content-Type"}},
		{name: "error-log-logger", cases: differentialCasesForPlugin("error-log-logger"), want: []string{
			"Content-Type", "X-ClickHouse-Database", "X-ClickHouse-Key", "X-ClickHouse-User", "X-Consumer-Username",
		}},
		{
			name:  "feishu-auth",
			cases: differentialCasesForPlugin("feishu-auth"),
			want:  []string{"Content-Type", "X-Userinfo"},
		},
		{name: "clickhouse-logger", cases: differentialCasesForPlugin("clickhouse-logger"), want: []string{
			"Content-Type", "X-ClickHouse-Database", "X-ClickHouse-Key", "X-ClickHouse-User",
		}},
		{
			name:  "elasticsearch-logger",
			cases: differentialCasesForPlugin("elasticsearch-logger"),
			want:  []string{"Content-Type"},
		},
		{name: "http-logger", cases: differentialCasesForPlugin("http-logger"), want: []string{"Content-Type"}},
		{name: "lago", cases: differentialCasesForPlugin("lago"), want: []string{"Authorization", "Content-Type"}},
		{name: "loggly", cases: differentialCasesForPlugin("loggly"), want: []string{"Content-Type", "X-LOGGLY-TAG"}},
		{
			name:  "loki-logger",
			cases: differentialCasesForPlugin("loki-logger"),
			want:  []string{"Content-Type", "X-Scope-OrgID"},
		},
		{name: "opentelemetry", cases: differentialCasesForPlugin("opentelemetry"), want: []string{
			"Content-Encoding", "Content-Type", "X-Differential-OTel", "X-Request-Id", "X-Tenant",
		}},
		{
			name:  "splunk-hec-logging",
			cases: differentialCasesForPlugin("splunk-hec-logging"),
			want:  []string{"Content-Type"},
		},
		{
			name:  "tencent-cloud-cls",
			cases: differentialCasesForPlugin("tencent-cloud-cls"),
			want:  []string{"Content-Type"},
		},
		{name: "zipkin", cases: differentialCasesForPlugin("zipkin"), want: []string{
			"Content-Type", "X-B3-Flags", "X-B3-ParentSpanId", "X-B3-Sampled", "X-B3-SpanId", "X-B3-TraceId",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.cases) != 1 {
				t.Fatalf("case count = %d, want 1", len(test.cases))
			}
			if !reflect.DeepEqual(test.cases[0].Fixture.SemanticHeaders, test.want) {
				t.Fatalf(
					"semantic headers = %#v, want %#v",
					test.cases[0].Fixture.SemanticHeaders,
					test.want,
				)
			}
		})
	}
}

func TestDifferentialSingleRequestCanCaptureMultipleSemanticCalls(t *testing.T) {
	fixture := DifferentialFixture{
		Name:            "oauth",
		CaptureAllCalls: true,
		SemanticHeaders: []string{"Content-Type"},
	}
	received := []differentialCapturedRequest{
		{
			Method:  http.MethodPost,
			Path:    "/token",
			Host:    "127.0.0.1:1980",
			Headers: http.Header{"Content-Type": {"application/json"}},
		},
		{Method: http.MethodGet, Path: "/hello", Host: "differential.example.test"},
	}
	observation := DifferentialObservation{Status: http.StatusOK, Body: "response"}
	applyDifferentialFixtureObservation(&observation, fixture, received, "127.0.0.1:1980")

	if len(observation.UpstreamCalls) != 2 || observation.RetryCount != 0 {
		t.Fatalf(
			"captured calls/retry count = %d/%d, want 2/0",
			len(observation.UpstreamCalls),
			observation.RetryCount,
		)
	}
	if observation.Upstream.Path != "/hello" || observation.UpstreamFixture != fixture.Name ||
		observation.UpstreamAddress != "127.0.0.1:1980" {
		t.Fatalf("last upstream observation = %#v", observation)
	}
	if got := http.Header(observation.UpstreamCalls[0].Headers).Get("Content-Type"); got != "application/json" {
		t.Fatalf("token Content-Type = %q", got)
	}
}

func TestDifferentialFixtureCollectTimeoutIsCaseScoped(t *testing.T) {
	if got := differentialCandidateFixtureCollectTimeout(DifferentialFixture{}); got != 350*time.Millisecond {
		t.Fatalf("default candidate collect timeout = %s", got)
	}
	if got := differentialOracleFixtureCollectTimeout(DifferentialFixture{}); got != 3*time.Second {
		t.Fatalf("default oracle collect timeout = %s", got)
	}
	fixture := DifferentialFixture{CollectTimeoutMillis: 6000}
	if got := differentialCandidateFixtureCollectTimeout(fixture); got != 6*time.Second {
		t.Fatalf("case candidate collect timeout = %s", got)
	}
	if got := differentialOracleFixtureCollectTimeout(fixture); got != 6*time.Second {
		t.Fatalf("case oracle collect timeout = %s", got)
	}
}

func TestHashObservationDoesNotMutateUpstreamCallHeaders(t *testing.T) {
	observation := DifferentialObservation{
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Upstream: DifferentialUpstreamObservation{
			Received: true,
			Headers:  map[string][]string{"X-Userinfo": {"encoded-user"}},
		},
		UpstreamCalls: []DifferentialUpstreamObservation{{
			Received: true,
			Headers:  map[string][]string{"X-Userinfo": {"encoded-user"}},
		}},
	}
	before := copyDifferentialObservation(observation)
	if _, err := hashObservation(observation, testNormalizationPolicy()); err != nil {
		t.Fatalf("hashObservation() error = %v", err)
	}
	if !reflect.DeepEqual(observation, before) {
		t.Fatalf("hashObservation() mutated observation: got %#v, want %#v", observation, before)
	}
}
