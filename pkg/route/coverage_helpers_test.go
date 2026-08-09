package route

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
)

type routeHealthRecorder struct {
	tcpCalls int
	timeout  bool
}

func (*routeHealthRecorder) ReportHTTP(string, int) {}

func (r *routeHealthRecorder) ReportTCPFailure(_ string, timeout bool) {
	r.tcpCalls++
	r.timeout = timeout
}

type routeNetError struct {
	timeout bool
}

func (e routeNetError) Error() string { return "upstream network failure" }
func (e routeNetError) Timeout() bool { return e.timeout }
func (routeNetError) Temporary() bool { return false }

func TestParsePluginPriority(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int
		ok    bool
	}{
		{name: "int", value: 3, want: 3, ok: true},
		{name: "int8", value: int8(4), want: 4, ok: true},
		{name: "int16", value: int16(5), want: 5, ok: true},
		{name: "int32", value: int32(6), want: 6, ok: true},
		{name: "int64", value: int64(7), want: 7, ok: true},
		{name: "uint", value: uint(8), want: 8, ok: true},
		{name: "uint8", value: uint8(9), want: 9, ok: true},
		{name: "uint16", value: uint16(10), want: 10, ok: true},
		{name: "uint32", value: uint32(11), want: 11, ok: true},
		{name: "uint64", value: uint64(12), want: 12, ok: true},
		{name: "integral float", value: float64(13), want: 13, ok: true},
		{name: "fractional float", value: 1.5},
		{name: "json number", value: json.Number("14"), want: 14, ok: true},
		{name: "string", value: "15"},
		{name: "nil", value: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parsePluginPriority(test.value)
			if ok := err == nil; ok != test.ok || got != test.want {
				t.Fatalf("parsePluginPriority(%v) = %d/%v, want %d/%t", test.value, got, err, test.want, test.ok)
			}
		})
	}
}

func TestNormalizeKafkaSSLNumber(t *testing.T) {
	if got, err := normalizeKafkaSSLNumber("16"); err != nil || got != "16" {
		t.Fatalf("normalizeKafkaSSLNumber(16) = %q/%v, want 16", got, err)
	}
	if _, err := normalizeKafkaSSLNumber("not-a-number"); err == nil {
		t.Fatal("normalizeKafkaSSLNumber(invalid) error = nil")
	}
}

func TestBatchRequestsURIResolvesConfiguredValue(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = nil
	if got := batchRequestsURI(); got != "/apisix/batch-requests" {
		t.Fatalf("batchRequestsURI() = %q, want default URI", got)
	}

	config.GlobalConfig = &config.Config{
		PluginAttr: map[string]map[string]any{"batch-requests": {"uri": "/internal/batch"}},
	}
	if got := batchRequestsURI(); got != "/internal/batch" {
		t.Fatalf("batchRequestsURI() = %q, want configured URI", got)
	}
}

func TestErrorHandlerHidesUpstreamDetails(t *testing.T) {
	reporter := &routeHealthRecorder{}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/orders?debug=true", nil)
	request = apisixctx.WithRequestVars(request)
	request = pxy.WithHealthReporter(request, reporter)
	pxy.SetSelectedTarget(request, "http://upstream.test:80")
	response := httptest.NewRecorder()

	newErrorHandler()(response, request, errors.New("dial tcp 10.0.0.7:9443: connection refused"))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	body := response.Body.String()
	if strings.Contains(body, "10.0.0.7") || strings.Contains(body, "connection refused") {
		t.Fatalf("body = %q, leaks upstream error details", body)
	}
	if !strings.Contains(body, "upstream request failed") {
		t.Fatalf("body = %q, want generic upstream failure message", body)
	}
}

func TestProxyErrorHandlerMapsFailuresAndRecordsResponseSource(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantStatus   int
		wantTCPCalls int
		wantTimeout  bool
	}{
		{
			name:         "timeout",
			err:          routeNetError{timeout: true},
			wantStatus:   http.StatusGatewayTimeout,
			wantTCPCalls: 1,
			wantTimeout:  true,
		},
		{
			name:         "network",
			err:          routeNetError{},
			wantStatus:   http.StatusBadGateway,
			wantTCPCalls: 1,
		},
		{name: "eof", err: io.EOF, wantStatus: http.StatusBadGateway, wantTCPCalls: 1},
		{name: "canceled", err: context.Canceled, wantStatus: StatusClientClosedRequest},
		{
			name:         "unexpected eof",
			err:          io.ErrUnexpectedEOF,
			wantStatus:   StatusClientClosedRequest,
			wantTCPCalls: 1,
		},
		{
			name:         "generic",
			err:          errors.New("upstream failed"),
			wantStatus:   http.StatusInternalServerError,
			wantTCPCalls: 1,
		},
		{
			name:       "cluster overloaded",
			err:        pxy.ErrClusterOverloaded,
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reporter := &routeHealthRecorder{}
			request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
			request = apisixctx.WithRequestVars(request)
			request = pxy.WithHealthReporter(request, reporter)
			pxy.SetSelectedTarget(request, "http://upstream.test:80")
			response := httptest.NewRecorder()

			newErrorHandler()(response, request, test.err)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if !strings.Contains(response.Body.String(), "upstream request failed") {
				t.Fatalf("body = %q, want generic upstream failure message", response.Body.String())
			}
			if got := apisixctx.GetRequestVar(request, "$response_source"); got != "apisix" {
				t.Fatalf("$response_source = %v, want apisix", got)
			}
			if reporter.tcpCalls != test.wantTCPCalls || reporter.timeout != test.wantTimeout {
				t.Fatalf(
					"TCP report = %d/%t, want %d/%t",
					reporter.tcpCalls,
					reporter.timeout,
					test.wantTCPCalls,
					test.wantTimeout,
				)
			}
		})
	}
}
