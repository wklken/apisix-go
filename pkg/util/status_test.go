package util

import "testing"

func TestTerminalStatusAcceptsHTTPTerminalCodes(t *testing.T) {
	t.Parallel()

	for _, code := range []int{200, 404, 502, 599} {
		got, ok := TerminalStatus(code)
		if !ok || got != code {
			t.Fatalf("TerminalStatus(%d) = (%d, %t), want (%d, true)", code, got, ok, code)
		}
	}
}

func TestTerminalStatusRejectsNonTerminalCodes(t *testing.T) {
	t.Parallel()

	for _, code := range []int{0, 99, 100, 199, 600, -1} {
		if got, ok := TerminalStatus(code); ok {
			t.Fatalf("TerminalStatus(%d) = (%d, true), want rejected", code, got)
		}
	}
}
