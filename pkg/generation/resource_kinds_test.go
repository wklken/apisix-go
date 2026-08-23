package generation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestManagedResourceKindsReturnsSortedDefensiveCopy(t *testing.T) {
	want := []string{
		"consumer_groups",
		"consumers",
		"global_rules",
		"plugin_configs",
		"plugin_metadata",
		"plugins",
		"protos",
		"routes",
		"secrets",
		"services",
		"ssls",
		"stream_routes",
		"upstreams",
	}

	first := ManagedResourceKinds()
	if !slices.Equal(first, want) {
		t.Fatalf("ManagedResourceKinds() = %v, want %v", first, want)
	}
	first[0] = "mutated"
	if got := ManagedResourceKinds(); !slices.Equal(got, want) {
		t.Fatalf("ManagedResourceKinds() after caller mutation = %v, want %v", got, want)
	}
}

func TestManagedResourceKindsExactMembership(t *testing.T) {
	for _, kind := range ManagedResourceKinds() {
		if !IsManagedResourceKind(kind) {
			t.Errorf("IsManagedResourceKind(%q) = false, want true", kind)
		}
	}
	for _, kind := range []string{"", "route", "data_plane", "unknown"} {
		if IsManagedResourceKind(kind) {
			t.Errorf("IsManagedResourceKind(%q) = true, want false", kind)
		}
	}
}

func TestDomainsForResourceKindExactMappingAndDefensiveCopy(t *testing.T) {
	wantHTTP := []Domain{DomainHTTP}
	wantStream := []Domain{DomainStream}
	wantBoth := []Domain{DomainHTTP, DomainStream}

	for _, kind := range ManagedResourceKinds() {
		want := wantHTTP
		switch kind {
		case "stream_routes":
			want = wantStream
		case "services", "upstreams", "secrets":
			want = wantBoth
		}
		if got := DomainsForResourceKind(kind); !slices.Equal(got, want) {
			t.Errorf("DomainsForResourceKind(%q) = %v, want %v", kind, got, want)
		}
	}

	first := DomainsForResourceKind("services")
	first[0] = DomainStream
	if got := DomainsForResourceKind("services"); !slices.Equal(got, wantBoth) {
		t.Fatalf("DomainsForResourceKind after caller mutation = %v, want %v", got, wantBoth)
	}
	if got := DomainsForResourceKind("unknown"); got != nil {
		t.Fatalf("DomainsForResourceKind(unknown) = %v, want nil", got)
	}
}

func TestManagedResourceKindsConsumerASTGuard(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate resource_kinds_test.go")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
	managedKinds := make(map[string]struct{}, len(managedResources))
	for _, kind := range ManagedResourceKinds() {
		managedKinds[kind] = struct{}{}
	}

	tests := []struct {
		path          string
		requiredCalls []string
	}{
		{path: "pkg/etcd/watcher.go", requiredCalls: []string{"IsManagedResourceKind", "DomainsForResourceKind"}},
		{path: "pkg/config/standalone.go", requiredCalls: []string{"DomainsForResourceKind"}},
		{path: "pkg/store/store.go", requiredCalls: []string{"ManagedResourceKinds", "IsManagedResourceKind"}},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			guardManagedResourceConsumer(t, filepath.Join(repositoryRoot, test.path), test.requiredCalls, managedKinds)
		})
	}
}

func guardManagedResourceConsumer(
	t *testing.T,
	filename string,
	requiredCalls []string,
	managedKinds map[string]struct{},
) {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	forbiddenFunctions := map[string]struct{}{
		"desiredDomains":      {},
		"isManagedEtcdBucket": {},
		"isManagedBucket":     {},
	}
	allowedClassifierFunctions := map[string]struct{}{
		"IsHTTPRouteReloadBucket": {},
		"IsStreamReloadBucket":    {},
	}
	if !strings.HasSuffix(filepath.ToSlash(filename), "/pkg/store/store.go") {
		clear(allowedClassifierFunctions)
	}
	required := make(map[string]bool, len(requiredCalls))
	for _, call := range requiredCalls {
		required[call] = false
	}
	allowedComposites := make(map[token.Pos]struct{})

	for _, declaration := range parsed.Decls {
		switch node := declaration.(type) {
		case *ast.FuncDecl:
			if _, forbidden := forbiddenFunctions[node.Name.Name]; forbidden {
				t.Errorf("%s defines forbidden taxonomy helper %s", filename, node.Name.Name)
			}
			guardManagedKindClassifierSwitches(
				t,
				fset,
				filename,
				node,
				managedKinds,
				allowedClassifierFunctions,
			)
		case *ast.GenDecl:
			for _, specification := range node.Specs {
				value, ok := specification.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range value.Names {
					if name.Name == "builtInBuckets" {
						t.Errorf("%s defines forbidden taxonomy list builtInBuckets", filename)
					}
					if name.Name == "standaloneBuckets" &&
						strings.HasSuffix(filepath.ToSlash(filename), "/pkg/config/standalone.go") &&
						index < len(value.Values) {
						if composite, ok := value.Values[index].(*ast.CompositeLit); ok {
							allowedComposites[composite.Pos()] = struct{}{}
						}
					}
				}
			}
		}
	}

	ast.Inspect(parsed, func(node ast.Node) bool {
		switch candidate := node.(type) {
		case *ast.CallExpr:
			selector, ok := candidate.Fun.(*ast.SelectorExpr)
			if !ok {
				break
			}
			packageName, ok := selector.X.(*ast.Ident)
			if ok && packageName.Name == "generation" {
				if _, expected := required[selector.Sel.Name]; expected {
					required[selector.Sel.Name] = true
				}
			}
		case *ast.CompositeLit:
			if _, allowed := allowedComposites[candidate.Pos()]; allowed {
				break
			}
			if countManagedKindLiterals(candidate, managedKinds) >= 2 {
				t.Errorf("%s:%d defines a second editable managed-kind list",
					filename, fset.Position(candidate.Pos()).Line)
			}
		}
		return true
	})
	for call, found := range required {
		if !found {
			t.Errorf("%s does not consume generation.%s", filename, call)
		}
	}
}

func guardManagedKindClassifierSwitches(
	t *testing.T,
	fset *token.FileSet,
	filename string,
	function *ast.FuncDecl,
	managedKinds map[string]struct{},
	allowedFunctions map[string]struct{},
) {
	t.Helper()
	if function.Body == nil {
		return
	}
	if _, allowed := allowedFunctions[function.Name.Name]; allowed {
		return
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switchStatement, ok := node.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		kindCount := 0
		classifier := false
		for _, statement := range switchStatement.Body.List {
			clause, ok := statement.(*ast.CaseClause)
			if !ok {
				continue
			}
			clauseKindCount := 0
			for _, expression := range clause.List {
				clauseKindCount += countManagedKindLiterals(expression, managedKinds)
			}
			kindCount += clauseKindCount
			for _, bodyStatement := range clause.Body {
				if _, returns := bodyStatement.(*ast.ReturnStmt); returns && clauseKindCount > 0 {
					classifier = true
				}
			}
		}
		if classifier && kindCount >= 2 {
			t.Errorf("%s:%d function %s defines a second managed-kind switch classifier",
				filename, fset.Position(switchStatement.Pos()).Line, function.Name.Name)
		}
		return true
	})
}

func countManagedKindLiterals(node ast.Node, managedKinds map[string]struct{}) int {
	count := 0
	ast.Inspect(node, func(candidate ast.Node) bool {
		literal, ok := candidate.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil {
			if _, managed := managedKinds[value]; managed {
				count++
			}
		}
		return true
	})
	return count
}
