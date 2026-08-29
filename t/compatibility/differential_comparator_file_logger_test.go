package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialFileLoggerCaseMapsAPISIX317RouteFormatWrite(t *testing.T) {
	cases := differentialCasesForPlugin("file-logger")
	if len(cases) != 1 {
		t.Fatalf("file-logger cases = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "file-logger-route-format-jsonl-write" || spec.Plugin != "file-logger" ||
		spec.RouteID != "differential-file-logger-route-format" ||
		spec.ComparisonPolicy != differentialFileLoggerJSONLPolicy ||
		spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
		spec.Request.Host != "127.0.0.1" || spec.Fixture.ExpectedCalls != 1 ||
		spec.File == nil || spec.File.Name != "access.log" || spec.File.ExpectedLines != 1 {
		t.Fatalf("file-logger case = %#v", spec)
	}
	routes := spec.Config["routes"].([]any)
	pluginConfig := routes[0].(map[string]any)["plugins"].(map[string]any)["file-logger"].(map[string]any)
	if pluginConfig["path"] != differentialSideFilePlaceholder {
		t.Fatalf("file-logger path = %#v", pluginConfig["path"])
	}
	format := pluginConfig["log_format"].(map[string]any)
	if format["host"] != "$host" || format["client_ip"] != "$remote_addr" {
		t.Fatalf("file-logger log_format = %#v", format)
	}
}

func TestCompareDifferentialFileLoggerJSONLValidatesPluginOutputSemantics(t *testing.T) {
	spec := differentialCasesForPlugin("file-logger")[0]
	candidate := differentialFileLoggerObservation(
		`{"level":"info","ts":123.5,"msg":"","host":"127.0.0.1","client_ip":"127.0.0.1","route_id":"differential-file-logger-route-format"}` + "\n",
	)
	oracle := differentialFileLoggerObservation(
		`{"host":"127.0.0.1","client_ip":"127.0.0.1","route_id":"differential-file-logger-route-format"}` + "\n",
	)
	passed, detail, err := compareDifferentialFileLoggerJSONL(spec, candidate, oracle, testNormalizationPolicy())
	if err != nil {
		t.Fatalf("compareDifferentialFileLoggerJSONL() error = %v", err)
	}
	if !passed {
		t.Fatalf("compareDifferentialFileLoggerJSONL() = false: %s", detail)
	}

	mutated := oracle
	mutatedContent := `{"host":"127.0.0.1","client_ip":"127.0.0.1","route_id":"wrong"}` + "\n"
	mutated.File = &DifferentialFileObservation{
		Name: "access.log", Exists: true, Size: int64(len(mutatedContent)),
		Content: mutatedContent,
	}
	passed, _, err = compareDifferentialFileLoggerJSONL(spec, candidate, mutated, testNormalizationPolicy())
	if err != nil {
		t.Fatalf("compare mutated output error = %v", err)
	}
	if passed {
		t.Fatal("compare mutated route_id = true, want false")
	}
}

func TestCompareDifferentialFileLoggerJSONLRejectsIncompleteLine(t *testing.T) {
	spec := differentialCasesForPlugin("file-logger")[0]
	candidate := differentialFileLoggerObservation(
		`{"level":"info","ts":123.5,"msg":"","host":"127.0.0.1","client_ip":"127.0.0.1","route_id":"differential-file-logger-route-format"}`,
	)
	oracle := differentialFileLoggerObservation(
		`{"host":"127.0.0.1","client_ip":"127.0.0.1","route_id":"differential-file-logger-route-format"}` + "\n",
	)
	if _, _, err := compareDifferentialFileLoggerJSONL(spec, candidate, oracle, testNormalizationPolicy()); err == nil {
		t.Fatal("compare incomplete candidate line error = nil")
	}
}

func differentialFileLoggerObservation(content string) DifferentialObservation {
	return DifferentialObservation{
		Status:           http.StatusOK,
		Headers:          map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:             "hello world\n",
		Host:             "127.0.0.1",
		SecurityDecision: "not_applicable",
		UpstreamFixture:  "primary",
		UpstreamAddress:  "127.0.0.1:1980",
		RetryCount:       0,
		Upstream: DifferentialUpstreamObservation{
			Received: true, Fixture: "primary", Method: http.MethodGet, Path: "/hello",
			Host: "differential.example.test",
		},
		File: &DifferentialFileObservation{
			Name: "access.log", Exists: true, Size: int64(len(content)), Content: content,
		},
	}
}
