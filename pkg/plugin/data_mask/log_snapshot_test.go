package data_mask

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestDataMaskRoutePreservesLiveRequestAndSanitizesDetachedSnapshot(t *testing.T) {
	const rawBody = " {\n  \"token\": \"secret\", \"number\": 1.00\n} "
	p := newTestPlugin(t, Config{Request: []MaskRule{
		{Type: "query", Name: "token", Action: "replace", Value: "***"},
		{Type: "header", Name: "Authorization", Action: "remove"},
		{Type: "body", BodyFormat: "json", Name: "$.token", Action: "replace", Value: "***"},
	}})
	request := httptest.NewRequest(
		http.MethodPost,
		"http://gateway.test/orders?token=one&keep=%2f&token=two",
		strings.NewReader(rawBody),
	)
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	originalQuery := request.URL.RawQuery
	originalRequestURI := request.RequestURI

	recorder := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read downstream body: %v", err)
		}
		if r.URL.RawQuery != originalQuery || r.RequestURI != originalRequestURI {
			t.Fatalf("live query changed: RawQuery=%q RequestURI=%q", r.URL.RawQuery, r.RequestURI)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("live Authorization = %q, want original", got)
		}
		if string(body) != rawBody {
			t.Fatalf("live body = %q, want byte-identical %q", body, rawBody)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}

	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
		Method: "POST",
		URI:    "/orders?token=one&keep=%2f&token=two",
		URL:    "http://gateway.test/orders?token=one&keep=%2f&token=two",
		Header: http.Header{
			"Authorization": {"Bearer secret"},
			"Content-Type":  {"application/json"},
		},
		Query: url.Values{"token": {"one", "two"}, "keep": {"/"}},
		Body:  []byte(rawBody),
	}}
	if err := p.SanitizeLogSnapshot(&snapshot); err != nil {
		t.Fatalf("SanitizeLogSnapshot() error = %v", err)
	}
	if got := snapshot.Request.Header.Get("Authorization"); got != "" {
		t.Fatalf("snapshot Authorization = %q, want removed", got)
	}
	if got := snapshot.Request.Query["token"]; !reflect.DeepEqual(got, []string{"***"}) {
		t.Fatalf("snapshot token values = %#v, want masked", got)
	}
	if strings.Contains(snapshot.Request.URI, "one") || strings.Contains(snapshot.Request.URL, "two") {
		t.Fatalf("snapshot URI/URL leaked raw query: %q / %q", snapshot.Request.URI, snapshot.Request.URL)
	}
	if bytes.Contains(snapshot.Request.Body, []byte("secret")) ||
		!bytes.Contains(snapshot.Request.Body, []byte(`"token":"***"`)) {
		t.Fatalf("snapshot body = %q, want masked JSON", snapshot.Request.Body)
	}
}

func TestDataMaskLogSnapshotMasksFormWithoutChangingSourceBytes(t *testing.T) {
	p := newTestPlugin(t, Config{Request: []MaskRule{
		{Type: "body", BodyFormat: "urlencoded", Name: "token", Action: "replace", Value: "***"},
	}})
	raw := []byte("keep=first&token=secret&keep=second")
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{Body: append([]byte(nil), raw...)}}
	if err := p.SanitizeLogSnapshot(&snapshot); err != nil {
		t.Fatalf("SanitizeLogSnapshot() error = %v", err)
	}
	if bytes.Contains(snapshot.Request.Body, []byte("secret")) {
		t.Fatalf("sanitized form leaked secret: %q", snapshot.Request.Body)
	}
	if string(raw) != "keep=first&token=secret&keep=second" {
		t.Fatalf("source body changed = %q", raw)
	}
}

func TestDataMaskLogSnapshotFailsClosedForIncompleteOrMalformedBody(t *testing.T) {
	p := newTestPlugin(t, Config{MaxBodySize: 8, Request: []MaskRule{
		{Type: "body", BodyFormat: "json", Name: "$.token", Action: "replace", Value: "***"},
	}})
	for _, test := range []struct {
		name      string
		body      string
		truncated bool
	}{
		{name: "truncated", body: `{"token":"secret"}`, truncated: true},
		{name: "configured limit", body: `{"token":"secret"}`},
		{name: "malformed", body: `{"token":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
				Body: []byte(test.body), BodyTruncated: test.truncated,
			}}
			if err := p.SanitizeLogSnapshot(&snapshot); err == nil {
				t.Fatal("SanitizeLogSnapshot() error = nil, want fail-closed error")
			}
			if len(snapshot.Request.Body) != 0 {
				t.Fatalf("snapshot retained unsafe body %q", snapshot.Request.Body)
			}
		})
	}
}

func TestDataMaskLogSnapshotFailsClosedWhenFormExceedsArgumentLimit(t *testing.T) {
	maxArgs := 1
	p := newTestPlugin(t, Config{MaxReqPostArgs: &maxArgs, Request: []MaskRule{
		{Type: "body", BodyFormat: "urlencoded", Name: "token", Action: "replace", Value: "***"},
	}})
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
		Body: []byte("keep=visible&token=secret"),
	}}
	if err := p.SanitizeLogSnapshot(&snapshot); err == nil {
		t.Fatal("SanitizeLogSnapshot() error = nil, want incomplete form rejection")
	}
	if len(snapshot.Request.Body) != 0 || !snapshot.Request.BodyTruncated {
		t.Fatalf("unsafe form snapshot = %q/truncated=%v", snapshot.Request.Body, snapshot.Request.BodyTruncated)
	}
}

func TestDataMaskLogSnapshotCapturePolicyIsBodyRuleBounded(t *testing.T) {
	queryOnly := newTestPlugin(t, Config{Request: []MaskRule{
		{Type: "query", Name: "token", Action: "remove"},
	}})
	if got := queryOnly.LogCapturePolicy(); got != (base.LogCapturePolicy{}) {
		t.Fatalf("query-only policy = %#v, want zero", got)
	}
	body := newTestPlugin(t, Config{MaxBodySize: base.MAX_REQ_BODY + 1, Request: []MaskRule{
		{Type: "body", BodyFormat: "json", Name: "$.token", Action: "remove"},
	}})
	if got := body.LogCapturePolicy().RequestBodyBytes; got != base.MAX_REQ_BODY {
		t.Fatalf("body policy = %d, want hard cap %d", got, base.MAX_REQ_BODY)
	}
}
