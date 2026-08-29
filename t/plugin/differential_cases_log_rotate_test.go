package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialLogRotateCaseMapsAPISIX317SizeCompressionAndRetentionBlocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	cases := differentialLogRotateCases()
	if len(cases) != 1 {
		t.Fatalf("differentialLogRotateCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "log-rotate-size-compress-prune-reopen" || spec.Plugin != "log-rotate" ||
		spec.RouteID != "differential-log-rotate-size-compress-prune-reopen" ||
		spec.ComparisonPolicy != differentialLogRotatePolicy || spec.File != nil {
		t.Fatalf("case identity = %#v", spec)
	}
	if len(spec.Steps) != 2 || spec.Steps[0].Request.Method != http.MethodGet ||
		spec.Steps[0].Request.Path != "/rotate" ||
		spec.Steps[1].Request.Path != "/after-rotate" ||
		spec.Steps[0].DelayBeforeMillis != 0 || spec.Steps[1].DelayBeforeMillis != 0 {
		t.Fatalf("case steps = %#v", spec.Steps)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 2 ||
		!spec.Fixture.CaptureAllCalls || spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Body != "ok" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	route := spec.Config["routes"].([]any)[0].(map[string]any)
	plugins := route["plugins"].(map[string]any)
	fileLogger := plugins["file-logger"].(map[string]any)
	if fileLogger["path"] != differentialLogRotateSideDirectoryPlaceholder+"/logs/access.log" {
		t.Fatalf("file-logger path = %#v", fileLogger["path"])
	}
	format := fileLogger["log_format"].(map[string]any)
	if format["path"] != "$uri" {
		t.Fatalf("file-logger format = %#v", format)
	}
	if got, want := differentialRequiredPluginNames(
		cases,
	), []string{
		"file-logger",
		"log-rotate",
	}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("narrow log-rotate runtime plugins = %#v, want %#v", got, want)
	}
}
