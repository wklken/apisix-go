package base

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadRequestBodyLimited(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		maxSize int
		want    string
		errText string
	}{
		{name: "under limit", body: `{"query":"{a}"}`, maxSize: 100, want: `{"query":"{a}"}`},
		{name: "exactly at limit", body: "12345", maxSize: 5, want: "12345"},
		{
			name:    "over limit",
			body:    "123456",
			maxSize: 5,
			want:    "123456",
			errText: "body exceeds maximum size 5",
		},
		{name: "empty body", body: "", maxSize: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(tt.body))

			got, err := ReadRequestBodyLimited(req, tt.maxSize)
			if tt.errText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errText) {
					t.Fatalf("ReadRequestBodyLimited() error = %v, want substring %q", err, tt.errText)
				}
				if !IsBodyTooLarge(err) {
					t.Fatalf("ReadRequestBodyLimited() error = %v, want typed size error", err)
				}
			} else if err != nil {
				t.Fatalf("ReadRequestBodyLimited() error = %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("ReadRequestBodyLimited() = %q, want %q", got, tt.want)
			}

			restored, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read restored body: %v", err)
			}
			if string(restored) != tt.body {
				t.Fatalf("restored body = %q, want %q", restored, tt.body)
			}
		})
	}
}

func TestReadBodyLimitedContracts(t *testing.T) {
	body, err := ReadResponseBodyLimited(strings.NewReader("12345"), 5)
	if err != nil || string(body) != "12345" {
		t.Fatalf("exact-limit read = %q, %v", body, err)
	}

	body, err = ReadResponseBodyLimited(strings.NewReader("123456789"), 5)
	if !IsBodyTooLarge(err) || string(body) != "123456" {
		t.Fatalf("limit+1 read = %q, %v; want six retained bytes and typed size error", body, err)
	}

	readErr := errors.New("read failed")
	body, err = ReadResponseBodyLimited(io.MultiReader(strings.NewReader("12"), errorReader{err: readErr}), 5)
	if !errors.Is(err, readErr) || string(body) != "12" {
		t.Fatalf("failed read = %q, %v; want partial bytes and source error", body, err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestReadRequestBodyLimitedNilAndNoBody(t *testing.T) {
	for name, req := range map[string]*http.Request{
		"nil body":    {Body: nil},
		"http NoBody": {Body: http.NoBody},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReadRequestBodyLimited(req, 10)
			if err != nil {
				t.Fatalf("ReadRequestBodyLimited() error = %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("ReadRequestBodyLimited() = %q, want empty", got)
			}
		})
	}
}

func TestResolveRequestVariables(t *testing.T) {
	lookup := map[string]string{
		"remote_addr": "192.0.2.1",
		"http_host":   "example.com",
	}

	got := ResolveRequestVariables("$remote_addr:$http_host", func(name string) string {
		return lookup[name]
	})
	if want := "192.0.2.1:example.com"; got != want {
		t.Fatalf("ResolveRequestVariables() = %q, want %q", got, want)
	}
}

func TestResolveRequestVariablesMissingAndLiteral(t *testing.T) {
	got := ResolveRequestVariables("$missing /static/$path", func(string) string {
		return ""
	})
	if want := " /static/"; got != want {
		t.Fatalf("ResolveRequestVariables() = %q, want %q", got, want)
	}

	if got := ResolveRequestVariables("plain text", func(string) string { return "x" }); got != "plain text" {
		t.Fatalf("ResolveRequestVariables(no variables) = %q, want input", got)
	}
}

func TestResolveRequestVariablesRespectsVariableNaming(t *testing.T) {
	var seen []string
	got := ResolveRequestVariables("$remote_addr-${unsupported-$uri}", func(name string) string {
		seen = append(seen, name)
		return ""
	})
	if len(seen) != 2 || seen[0] != "remote_addr" || seen[1] != "uri" {
		t.Fatalf("resolved names = %v, want [remote_addr uri]", seen)
	}
	if want := "-${unsupported-}"; got != want {
		t.Fatalf("ResolveRequestVariables() = %q, want %q", got, want)
	}
}
