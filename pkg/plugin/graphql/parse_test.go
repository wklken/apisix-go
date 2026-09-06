package graphql

import (
	"testing"
)

func TestMaxDepthExpandsFragmentsAndInlineFragments(t *testing.T) {
	doc, err := Parse(`
		query Q { viewer { ...Fields ... on User { profile { name } } } }
		fragment Fields on User { posts { nodes { id } } }
	`)
	if err != nil {
		t.Fatal(err)
	}
	depth, err := MaxDepth(doc, "Q")
	if err != nil {
		t.Fatal(err)
	}
	if depth != 4 {
		t.Fatalf("depth = %d, want 4", depth)
	}
}

func TestOperationRejectsAmbiguousUnnamedSelection(t *testing.T) {
	doc, err := Parse(`query A { a } query B { b }`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Operation(doc, ""); err == nil {
		t.Fatal("Operation() error = nil, want operation name requirement")
	}
	if operation, err := Operation(doc, "B"); err != nil || operation.Name != "B" {
		t.Fatalf("Operation(B) = %v/%v", operation, err)
	}
	if _, err := Operation(doc, "Missing"); err == nil {
		t.Fatal("Operation(Missing) error = nil, want not defined")
	}
}

func TestMaxDepthHandlesSyntaxFeatures(t *testing.T) {
	queries := map[string]int{
		`query ($id: ID!) { user(id: $id) { profile { name } } }`:                 3,
		`query { user: viewer { posts { id } } }`:                                 3,
		`query { viewer @include(if: true) { profile { name } } }`:                3,
		`query { search(q: """block string""") { id } }`:                          2,
		"# comment\nquery { viewer { # inline\n profile { name } } }":             3,
		`query { viewer { ... on User { posts { nodes { id } } } } }`:             4,
		`query { viewer { ...Fields } } fragment Fields on User { posts { id } }`: 3,
		`mutation { createPost { id } }`:                                          2,
	}
	for query, want := range queries {
		doc, err := Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", query, err)
		}
		depth, err := MaxDepth(doc, "")
		if err != nil {
			t.Fatalf("MaxDepth(%q) error = %v", query, err)
		}
		if depth != want {
			t.Fatalf("depth(%q) = %d, want %d", query, depth, want)
		}
	}
}

func TestMaxDepthIgnoresUndefinedFragment(t *testing.T) {
	doc, err := Parse(`query { viewer { ...Missing } }`)
	if err != nil {
		t.Fatal(err)
	}
	if depth, err := MaxDepth(doc, ""); err != nil || depth != 1 {
		t.Fatalf("MaxDepth() = %d, %v; want parent field depth 1", depth, err)
	}
}

func TestMaxDepthBoundsCyclicFragments(t *testing.T) {
	doc, err := Parse(`
		query { viewer { ...First } }
		fragment First on Viewer { ...Second }
		fragment Second on Viewer { ...First }
	`)
	if err != nil {
		t.Fatal(err)
	}
	depth, err := MaxDepth(doc, "")
	if err != nil {
		t.Fatalf("MaxDepth() error = %v", err)
	}
	if depth != 1 {
		t.Fatalf("depth = %d, want 1", depth)
	}
}

func TestParseRejectsMalformedSyntax(t *testing.T) {
	queries := []string{
		"",
		"query { viewer { id",
		"uery {}",
		"test { persons { name } }",
		"query { persons(filter) { id } }",
		"query { viewer } }",
	}
	for _, query := range queries {
		if _, err := Parse(query); err == nil {
			t.Fatalf("Parse(%q) error = nil, want syntax rejection", query)
		}
	}
}
