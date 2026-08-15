package chash

import "testing"

// This literal corpus was generated independently from Apache APISIX revision
// 9ef2ecab67f652d38365049613610ef649bb4ad0 and its resty.chash algorithm.
func TestKetamaMatchesPinnedAPISIX317Corpus(t *testing.T) {
	ring, err := New([]Node{
		{ID: "provider-c", Weight: 1},
		{ID: "provider-a", Weight: 1},
		{ID: "provider-b", Weight: 2},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	want := map[string]string{
		"alpha":                "provider-c",
		"beta":                 "provider-a",
		"gamma":                "provider-a",
		"delta":                "provider-c",
		"epsilon":              "provider-b",
		"tenant-001":           "provider-b",
		"tenant-002":           "provider-b",
		"203.0.113.9":          "provider-b",
		"/v1/chat/completions": "provider-b",
		"sticky-session":       "provider-c",
	}
	for key, node := range want {
		if got, ok := ring.Lookup(key); !ok || got != node {
			t.Errorf("Lookup(%q) = %q, %v; want %q, true", key, got, ok, node)
		}
	}
}

func TestKetamaIsDeterministicAndPreservesTargets(t *testing.T) {
	left, err := New([]Node{
		{ID: "10.0.0.2:80", Target: "node-b", Weight: 2},
		{ID: "10.0.0.1:80", Target: "node-a", Weight: 1},
	})
	if err != nil {
		t.Fatalf("New(left) error = %v", err)
	}
	right, err := New([]Node{
		{ID: "10.0.0.1:80", Target: "node-a", Weight: 1},
		{ID: "10.0.0.2:80", Target: "node-b", Weight: 2},
	})
	if err != nil {
		t.Fatalf("New(right) error = %v", err)
	}
	for i := range 1000 {
		key := string(rune(i))
		leftNode, _ := left.Lookup(key)
		rightNode, _ := right.Lookup(key)
		if leftNode != rightNode {
			t.Fatalf("Lookup(%q) differs by input order: %q != %q", key, leftNode, rightNode)
		}
	}
}

func TestKetamaRemovalOnlyRemapsRemovedOwnership(t *testing.T) {
	full, err := New([]Node{
		{ID: "provider-a", Weight: 1},
		{ID: "provider-b", Weight: 2},
		{ID: "provider-c", Weight: 1},
	})
	if err != nil {
		t.Fatalf("New(full) error = %v", err)
	}
	reduced, err := New([]Node{
		{ID: "provider-a", Weight: 1},
		{ID: "provider-c", Weight: 1},
	})
	if err != nil {
		t.Fatalf("New(reduced) error = %v", err)
	}

	remapped := 0
	for i := range 1000 {
		key := keyForIndex(i)
		before, _ := full.Lookup(key)
		after, _ := reduced.Lookup(key)
		if before == after {
			continue
		}
		remapped++
		if before != "provider-b" {
			t.Fatalf("key %q remapped retained owner %q to %q", key, before, after)
		}
	}
	if remapped != 493 {
		t.Fatalf("remapped keys = %d, want pinned 493/1000", remapped)
	}
}

func keyForIndex(index int) string {
	const digits = "0123456789"
	return "key-" + string([]byte{
		digits[index/1000%10],
		digits[index/100%10],
		digits[index/10%10],
		digits[index%10],
	})
}

func TestKetamaRejectsInvalidNodes(t *testing.T) {
	for _, nodes := range [][]Node{
		nil,
		{{ID: "", Weight: 1}},
		{{ID: "one", Weight: -1}},
		{{ID: "one", Weight: 0}},
		{{ID: "one", Weight: 1}, {ID: "one", Weight: 2}},
	} {
		if _, err := New(nodes); err == nil {
			t.Fatalf("New(%#v) error = nil", nodes)
		}
	}
}
