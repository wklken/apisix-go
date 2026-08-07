package route

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
)

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
