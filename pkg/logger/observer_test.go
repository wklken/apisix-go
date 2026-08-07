package logger

import (
	"fmt"
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

// countingFormatter records how many times it is formatted, so the test can
// distinguish a single shared formatting pass from duplicated one.
type countingFormatter struct {
	calls *int
}

func (f countingFormatter) String() string {
	*f.calls++
	return "value"
}

func TestObserverSeesZapFormattedMessageOnce(t *testing.T) {
	var calls int
	var got Entry
	stop := ReplaceObserver("single-format-test", func(entry Entry) {
		got = entry
	})
	t.Cleanup(stop)

	Infof("entry %s %d", countingFormatter{calls: &calls}, 7)

	if calls != 1 {
		t.Fatalf("formatter invoked %d times, want exactly once", calls)
	}
	// zap's sugared logger produces fmt.Sprintf(template, args...); the
	// observer must see the same final message without a second formatting.
	if want := fmt.Sprintf("entry %s %d", "value", 7); got.Message != want {
		t.Fatalf("observer message = %q, want %q", got.Message, want)
	}
}

func TestObserverFormattingNotDuplicated(t *testing.T) {
	t.Cleanup(func() { _ = ConfigureLevel("info") })
	var calls int
	formatter := countingFormatter{calls: &calls}

	if err := ConfigureLevel("error"); err != nil {
		t.Fatalf("configure error level: %v", err)
	}
	Infof("entry %s", formatter)
	if calls != 0 {
		t.Fatalf("formatter invoked %d times at a disabled level, want 0", calls)
	}

	if err := ConfigureLevel("info"); err != nil {
		t.Fatalf("configure info level: %v", err)
	}
	Infof("entry %s", formatter)
	if calls != 1 {
		t.Fatalf("formatter invoked %d times with no observers, want 1 shared format for zap", calls)
	}
}
