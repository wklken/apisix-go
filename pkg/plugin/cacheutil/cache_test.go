package cacheutil

import (
	"net/http"
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

func TestVarySignaturePreservesOrderDelimiterAndMD5(t *testing.T) {
	r := &http.Request{
		Header: http.Header{
			"Accept-Encoding": {"gzip"},
			"Accept-Language": {"en-US"},
		},
		URL: &url.URL{},
	}

	if got := VarySignature(
		[]string{"accept-encoding", "accept-language"},
		r,
	); got != "36a6aff3ac5f3f6e1d36839831d8e6d6" {
		t.Fatalf("signature = %q, want the existing NUL-separated MD5 output", got)
	}
	if got := VarySignature(
		[]string{"accept-language", "accept-encoding"},
		r,
	); got == "36a6aff3ac5f3f6e1d36839831d8e6d6" {
		t.Fatal("signature ignored header order")
	}
}
