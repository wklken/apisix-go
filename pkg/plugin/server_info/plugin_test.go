package server_info

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestReportTTLReadsAndBoundsPluginAttribute(t *testing.T) {
	attr := map[string]any{"report_ttl": 45}
	if got := ReportTTL(attr); got != 45*time.Second {
		t.Fatalf("ReportTTL() = %s, want 45s", got)
	}

	attr["report_ttl"] = 1
	if got := ReportTTL(attr); got != 3*time.Second {
		t.Fatalf("ReportTTL() below minimum = %s, want 3s", got)
	}

	attr["report_ttl"] = 90000
	if got := ReportTTL(attr); got != 86400*time.Second {
		t.Fatalf("ReportTTL() above maximum = %s, want 86400s", got)
	}
}

func TestPluginInitMetadata(t *testing.T) {
	plugin := &Plugin{}
	if err := plugin.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if plugin.Name != name || plugin.Priority != priority || plugin.Schema != schema {
		t.Fatalf(
			"metadata = %q/%d/%q, want %q/%d/%q",
			plugin.Name,
			plugin.Priority,
			plugin.Schema,
			name,
			priority,
			schema,
		)
	}
}

func TestPluginConfigReturnsPointerIdentity(t *testing.T) {
	plugin := &Plugin{}
	if got := plugin.Config(); got != &plugin.config {
		t.Fatal("Config() does not return the plugin config pointer")
	}
}

func TestPluginHandlerPassesThrough(t *testing.T) {
	called := false
	plugin := &Plugin{}
	handler := plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/server-info", nil))
	if !called || response.Code != http.StatusNoContent {
		t.Fatalf("called/status = %t/%d, want true/204", called, response.Code)
	}
}

func TestCurrentInfoReportsRequiredFields(t *testing.T) {
	info := CurrentInfo("node-test")
	if info.EtcdVersion != "unknown" || info.Version != version {
		t.Fatalf("versions = %q/%q, want unknown/apisix-go", info.EtcdVersion, info.Version)
	}
	if info.Hostname == "" {
		t.Fatal("hostname is empty")
	}
	if info.ID != "node-test" {
		t.Fatalf("id = %q, want node-test", info.ID)
	}
	if info.BootTime == 0 {
		t.Fatal("boot time is zero")
	}
}

func TestInfoHandlerWritesJSON(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/server-info", nil)
	response := httptest.NewRecorder()
	InfoHandler("node-test")(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=UTF-8" {
		t.Fatalf("Content-Type = %q, want application/json with UTF-8 charset", got)
	}
	if strings.HasSuffix(response.Body.String(), "\n") {
		t.Fatalf("body has trailing newline: %q", response.Body.String())
	}
	var decoded Response
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := CurrentInfo("node-test")
	if decoded != want {
		t.Fatalf("decoded = %+v, want %+v", decoded, want)
	}
}

func TestReportTTLValue(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int64
		ok   bool
	}{
		{name: "int8", in: int8(3), want: 3, ok: true},
		{name: "uint32", in: uint32(4), want: 4, ok: true},
		{name: "integral float", in: float64(5), want: 5, ok: true},
		{name: "fractional float", in: 1.5, want: 1, ok: false},
		{name: "json number", in: json.Number("6"), want: 6, ok: true},
		{name: "overflow uint64", in: uint64(math.MaxUint64), ok: false},
		{name: "string", in: "7", ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := reportTTLValue(test.in)
			if ok != test.ok || got != test.want {
				t.Fatalf("reportTTLValue(%v) = %d/%t, want %d/%t", test.in, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestPluginPostInitSucceeds(t *testing.T) {
	plugin := &Plugin{}
	if err := plugin.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := plugin.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
}
