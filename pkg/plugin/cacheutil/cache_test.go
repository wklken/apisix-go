package cacheutil

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
)

func TestCloneHeaderDeepCopiesValues(t *testing.T) {
	original := http.Header{
		"Accept":     {"application/json", "text/plain"},
		"Set-Cookie": {"first=1"},
	}

	cloned := CloneHeader(original)
	cloned["Accept"][0] = "application/xml"
	cloned.Add("Set-Cookie", "second=2")

	if got := original.Values("Accept"); !slices.Equal(got, []string{"application/json", "text/plain"}) {
		t.Fatalf("original Accept = %v, want an unchanged value slice", got)
	}
	if got := original.Values("Set-Cookie"); !slices.Equal(got, []string{"first=1"}) {
		t.Fatalf("original Set-Cookie = %v, want [first=1]", got)
	}
}

func TestParseVaryHeader(t *testing.T) {
	tests := []struct {
		name      string
		header    http.Header
		want      []string
		cacheable bool
	}{
		{name: "missing", header: http.Header{}, cacheable: true},
		{
			name: "normalizes deduplicates and sorts across fields",
			header: http.Header{
				"Vary": {"Accept-Encoding, X-Device", "x-device, ACCEPT-LANGUAGE"},
			},
			want:      []string{"accept-encoding", "accept-language", "x-device"},
			cacheable: true,
		},
		{
			name:      "ignores empty names",
			header:    http.Header{"Vary": {" , Accept-Encoding, "}},
			want:      []string{"accept-encoding"},
			cacheable: true,
		},
		{
			name:      "rejects wildcard",
			header:    http.Header{"Vary": {"Accept-Encoding", "*"}},
			cacheable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, cacheable := ParseVaryHeader(tt.header)
			if cacheable != tt.cacheable {
				t.Fatalf("cacheable = %t, want %t", cacheable, tt.cacheable)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("headers = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequiredVaryIsRequestLocalAndCaseInsensitive(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	marked := WithRequiredVary(request, "Accept-Encoding")

	if RequiredVary(request, "accept-encoding") {
		t.Fatal("original request unexpectedly inherited required Vary")
	}
	if !RequiredVary(marked, "ACCEPT-ENCODING") {
		t.Fatal("marked request lost required Vary")
	}
	if RequiredVary(marked, "Accept-Language") {
		t.Fatal("unmarked Vary field reported as required")
	}
}

func TestVarySignaturePreservesOrderDelimiterAndMD5(t *testing.T) {
	r := &http.Request{
		Header: http.Header{
			"Accept-Encoding": {"gzip"},
			"Accept-Language": {"en-US"},
		},
		URL: &url.URL{},
	}

	const want = "31bef7af3976f782c9a2e9f718934893"
	if got := VarySignature(
		[]string{"accept-encoding", "accept-language"},
		r,
	); got != want {
		t.Fatalf("signature = %q, want the collision-safe framed MD5 output", got)
	}
	if got := VarySignature(
		[]string{"accept-language", "accept-encoding"},
		r,
	); got == want {
		t.Fatal("signature ignored header order")
	}
}

func TestVarySignatureUsesEveryHeaderValueAndCollisionSafeFraming(t *testing.T) {
	requestWithValues := func(values ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, value := range values {
			r.Header.Add("X-Variant", value)
		}
		return r
	}

	first := requestWithValues("gzip", "br")
	secondValueChanged := requestWithValues("gzip", "deflate")
	if got, want := VarySignature(
		[]string{"x-variant"},
		first,
	), VarySignature(
		[]string{"x-variant"},
		secondValueChanged,
	); got == want {
		t.Fatalf("signatures = %q/%q, want repeated second values to affect the signature", got, want)
	}

	ambiguousA := requestWithValues("ab", "c")
	ambiguousB := requestWithValues("a", "bc")
	if got, want := VarySignature(
		[]string{"x-variant"},
		ambiguousA,
	), VarySignature(
		[]string{"x-variant"},
		ambiguousB,
	); got == want {
		t.Fatalf("signatures = %q/%q, want value boundaries to remain distinguishable", got, want)
	}

	equal := requestWithValues("gzip", "br")
	if got, want := VarySignature(
		[]string{"x-variant"},
		first,
	), VarySignature(
		[]string{"x-variant"},
		equal,
	); got != want {
		t.Fatalf("equal semantic inputs produced signatures %q and %q", got, want)
	}
}
