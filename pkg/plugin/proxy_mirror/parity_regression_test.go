package proxy_mirror

import (
	"net/http/httptest"
	"testing"
)

func TestParityMirrorPrefixPreservesConcatenation(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "http://mirror.example.test", Path: "/archive/", PathConcatMode: "prefix"})
	got, err := p.mirrorURL(httptest.NewRequest("GET", "http://gateway.example.test/item?q=1", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := "http://mirror.example.test/archive//item?q=1"
	if got != want {
		t.Fatalf("got %q; APISIX literal prefix wants %q", got, want)
	}
}
