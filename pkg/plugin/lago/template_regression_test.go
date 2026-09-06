package lago

import "testing"

func TestBareDollarTemplatesResolveVariables(t *testing.T) {
	fields := map[string]any{"request_id": "abc"}
	if got := resolveTemplate("${request_id}", fields); got != "abc" {
		t.Fatalf("${request_id}=%q, want abc", got)
	}
	if got := resolveTemplate("$request_id", fields); got != "abc" {
		t.Fatalf("$request_id=%q, want abc", got)
	}
	if got := resolveTemplate("req_$request_id", fields); got != "req_abc" {
		t.Fatalf("req_$request_id=%q, want req_abc", got)
	}
}
