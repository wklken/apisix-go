package logger

import (
	"strings"
	"testing"
)

func TestReplaceObserverAtomicallyReplacesNamedObserver(t *testing.T) {
	var first []Entry
	stopFirst := ReplaceObserver("error-log-logger-test", func(entry Entry) {
		first = append(first, entry)
	})
	t.Cleanup(stopFirst)

	Warn("first observer marker")
	if len(first) != 1 || first[0].Level != "WARN" ||
		!strings.Contains(first[0].Line, "[warn] first observer marker") {
		t.Fatalf("first observer entries = %#v", first)
	}

	var second []Entry
	stopSecond := ReplaceObserver("error-log-logger-test", func(entry Entry) {
		second = append(second, entry)
	})
	t.Cleanup(stopSecond)

	stopFirst()
	Errorf("second observer %s", "marker")
	if len(first) != 1 {
		t.Fatalf("stale observer received replacement entry: %#v", first)
	}
	if len(second) != 1 || second[0].Level != "ERROR" ||
		!strings.Contains(second[0].Line, "[error] second observer marker") {
		t.Fatalf("second observer entries = %#v", second)
	}

	stopSecond()
	Info("observer removed marker")
	if len(second) != 1 {
		t.Fatalf("removed observer received entry: %#v", second)
	}
}

func TestReplaceObserverWithNilClearsNamedObserver(t *testing.T) {
	var entries []Entry
	stop := ReplaceObserver("error-log-logger-clear-test", func(entry Entry) {
		entries = append(entries, entry)
	})
	t.Cleanup(stop)

	clear := ReplaceObserver("error-log-logger-clear-test", nil)
	t.Cleanup(clear)
	Warn("cleared observer marker")
	if len(entries) != 0 {
		t.Fatalf("cleared observer entries = %#v, want none", entries)
	}
}

func TestObserversHonorConfiguredRuntimeLevel(t *testing.T) {
	t.Cleanup(func() { _ = ConfigureLevel("info") })
	if err := ConfigureLevel("error"); err != nil {
		t.Fatalf("configure error level: %v", err)
	}

	var entries []Entry
	stop := ReplaceObserver("runtime-level-test", func(entry Entry) {
		entries = append(entries, entry)
	})
	t.Cleanup(stop)

	Debug("filtered debug marker")
	Info("filtered info marker")
	Warn("filtered warning marker")
	Error("visible error marker")

	if len(entries) != 1 || entries[0].Level != "ERROR" || entries[0].Message != "visible error marker" {
		t.Fatalf("observer entries = %#v, want only the error marker", entries)
	}
}
