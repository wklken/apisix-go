package plugin

import "testing"

func TestEnabledSetClonesSourceAndSupportsStrictEmpty(t *testing.T) {
	names := []string{"request-id", "native-only"}
	set := NewEnabledSet(names)

	if !set.Contains("request-id") || !set.Contains("native-only") {
		t.Fatalf("enabled set membership = request-id:%t native-only:%t, want both enabled", set.Contains("request-id"), set.Contains("native-only"))
	}
	if set.Contains("gzip") {
		t.Fatal("enabled set contains gzip, want it disabled")
	}

	names[0] = "gzip"
	names = append(names, "late-mutation")
	if !set.Contains("request-id") {
		t.Fatal("mutating the source slice removed request-id from the cloned set")
	}
	if set.Contains("gzip") || set.Contains("late-mutation") {
		t.Fatal("mutating the source slice changed cloned set membership")
	}

	empty := NewEnabledSet([]string{})
	if empty.Contains("request-id") || empty.Contains("native-only") {
		t.Fatal("strict empty set allows a plugin, want all membership denied")
	}
}
