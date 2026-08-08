package ai_common

import (
	"net/http"
	"testing"
	"time"
)

func TestCloneJSONValueDoesNotAlias(t *testing.T) {
	source := map[string]any{
		"a": "1",
		"nested": map[string]any{
			"list": []any{1, 2, map[string]any{"x": true}},
		},
	}
	clone := CloneJSONValue(source).(map[string]any)

	nestedClone := clone["nested"].(map[string]any)
	nestedClone["list"].([]any)[2].(map[string]any)["x"] = false

	if source["nested"].(map[string]any)["list"].([]any)[2].(map[string]any)["x"] != true {
		t.Fatal("mutating the clone changed the source")
	}
}

func TestAsAnyMap(t *testing.T) {
	if _, ok := AsAnyMap(map[string]any{"a": 1}); !ok {
		t.Fatal("AsAnyMap(map) = false, want true")
	}
	if _, ok := AsAnyMap([]any{1}); ok {
		t.Fatal("AsAnyMap(slice) = true, want false")
	}
}

func TestMergeBodyMap(t *testing.T) {
	dst := map[string]any{
		"a": "old",
		"nested": map[string]any{
			"x": 1,
			"y": 2,
		},
	}

	MergeBodyMap(dst, map[string]any{
		"a": "new",
		"b": "added",
		"nested": map[string]any{
			"y": 20,
		},
	}, false)

	if got := dst["a"]; got != "old" {
		t.Fatalf("a = %v, want old (force=false keeps existing)", got)
	}
	if got := dst["b"]; got != "added" {
		t.Fatalf("b = %v, want added", got)
	}
	if got := dst["nested"].(map[string]any)["y"]; got != 2 {
		t.Fatalf("nested.y = %v, want 2 (non-force merge keeps existing)", got)
	}

	MergeBodyMap(dst, map[string]any{"a": "forced"}, true)
	if got := dst["a"]; got != "forced" {
		t.Fatalf("a = %v, want forced", got)
	}
}

func TestCopyForwardHeadersSkipsHopByHop(t *testing.T) {
	src := http.Header{
		"Host":            {"example.com"},
		"Content-Length":  {"10"},
		"Accept-Encoding": {"gzip"},
		"X-Custom":        {"v1", "v2"},
	}
	dst := http.Header{}

	CopyForwardHeaders(dst, src)

	if len(dst) != 1 {
		t.Fatalf("copied headers = %#v, want only X-Custom", dst)
	}
	if values := dst.Values("X-Custom"); len(values) != 2 || values[0] != "v1" || values[1] != "v2" {
		t.Fatalf("X-Custom values = %v, want [v1 v2]", values)
	}
}

func TestApplyTransportOptionsDoNotMutateDefaultTransport(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	verify := false
	keepalive := false

	ApplyTransportKeepalive(transport, 42, 3000, &keepalive)
	ApplyTransportSSLVerify(transport, &verify)

	if !transport.DisableKeepAlives {
		t.Fatal("keepalive disabled option not applied to the clone")
	}
	if transport.MaxIdleConnsPerHost != 42 || transport.IdleConnTimeout != 3*time.Second {
		t.Fatalf("keepalive options = %d/%v, want 42/3s", transport.MaxIdleConnsPerHost, transport.IdleConnTimeout)
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("SSL verify option not applied to the clone")
	}

	if http.DefaultTransport.(*http.Transport).DisableKeepAlives {
		t.Fatal("DefaultTransport mutated by transport options")
	}
	if cfg := http.DefaultTransport.(*http.Transport).TLSClientConfig; cfg != nil && cfg.InsecureSkipVerify {
		t.Fatal("DefaultTransport TLS verification disabled by transport options")
	}
}

func TestApplyTransportOptionsLeaveVerifyEnabled(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	verify := true
	ApplyTransportSSLVerify(transport, &verify)
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify set while verification was enabled")
	}

	ApplyTransportSSLVerify(transport, nil)
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify set for nil verify")
	}
}

func TestHasProtocolRequestBodyOverride(t *testing.T) {
	if !HasProtocolRequestBodyOverride(map[string]any{"openai-chat": map[string]any{}}) {
		t.Fatal("protocol key not detected")
	}
	if !HasProtocolRequestBodyOverride(map[string]any{"passthrough": map[string]any{}}) {
		t.Fatal("passthrough key not detected")
	}
	if HasProtocolRequestBodyOverride(map[string]any{"other": map[string]any{}}) {
		t.Fatal("non-protocol key detected as override")
	}
	if HasProtocolRequestBodyOverride(map[string]any{}) {
		t.Fatal("empty map detected as override")
	}
}
