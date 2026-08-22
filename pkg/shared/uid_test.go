package shared

import "testing"

func TestConfigUIDIsDeterministic(t *testing.T) {
	first := NewConfigUID()
	first.Add("consumer", "alice", 1)
	second := NewConfigUID()
	second.Add("consumer", "alice", 1)
	if first.String() != second.String() {
		t.Fatalf("digests differ for equal Add sequences: %q vs %q", first.String(), second.String())
	}

	reordered := NewConfigUID()
	reordered.Add("alice", 1, "consumer")
	if first.String() == reordered.String() {
		t.Fatalf("digest %q unchanged when Add order changes", first.String())
	}
}

func TestConfigUIDEmptyIsDeterministic(t *testing.T) {
	first := NewConfigUID().String()
	second := NewConfigUID().String()
	if first == "" || first != second {
		t.Fatalf("empty UIDs = %q/%q, want equal non-empty digests", first, second)
	}
}

func TestConfigUIDLengthPrefixSeparatesColonPairs(t *testing.T) {
	first := NewConfigUID()
	first.Add("a:b", "c")
	second := NewConfigUID()
	second.Add("a", "b:c")
	if first.String() == second.String() {
		t.Fatalf("digests collided for (\"a:b\",\"c\") and (\"a\",\"b:c\"): %q", first.String())
	}
}
