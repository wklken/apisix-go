package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionPluginPackageHasNoLegacyExecutorAPI(t *testing.T) {
	t.Helper()
	forbidden := map[string]struct{}{
		"BuildPluginChain":  {},
		"BindPlugin":        {},
		"Executor":          {},
		"NewExecutor":       {},
		"NewScopedExecutor": {},
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(files, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declaration := range parsed.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if declaration.Recv == nil {
					if _, found := forbidden[declaration.Name.Name]; found {
						t.Errorf("legacy production function remains: %s in %s", declaration.Name.Name, entry.Name())
					}
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if _, found := forbidden[typeSpec.Name.Name]; found {
						t.Errorf("legacy production type remains: %s in %s", typeSpec.Name.Name, entry.Name())
					}
				}
			}
		}
	}

	module, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(module), "github.com/justinas/alice") {
		t.Error("test-only middleware dependency remains in go.mod: github.com/justinas/alice")
	}
}
