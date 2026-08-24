package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestMetadataDependencyGuardDetectsImportAliases(t *testing.T) {
	tests := map[string]string{
		"renamed base import": `package fixture
import metadata "github.com/wklken/apisix-go/pkg/plugin/base"
func f() { _ = metadata.LoadPluginMetadata[map[string]any]("x") }`,
		"parenthesized generic base call": `package fixture
import metadata "github.com/wklken/apisix-go/pkg/plugin/base"
func f() { _ = (metadata.LoadPluginMetadata[map[string]any])("x") }`,
		"renamed store import": `package fixture
import state "github.com/wklken/apisix-go/pkg/store"
func f() { var target any; _ = state.GetPluginMetadata("x", &target) }`,
		"parenthesized store call": `package fixture
import state "github.com/wklken/apisix-go/pkg/store"
func f() { var target any; _ = (state.GetPluginMetadata)("x", &target) }`,
		"dot store import": `package fixture
import . "github.com/wklken/apisix-go/pkg/store"
func f() { _ = GetPluginMetadataRaw("x") }`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if violations := metadataDependencyViolations(fset, file); len(violations) == 0 {
				t.Fatal("metadata dependency guard accepted a forbidden call")
			}
		})
	}
}

func TestMetadataDependencyGuardIgnoresUnrelatedSameNameMethods(t *testing.T) {
	const source = `package fixture
type localStore struct{}
func (localStore) GetPluginMetadata(string, any) error { return nil }
func f() { var target any; _ = (localStore{}).GetPluginMetadata("x", &target) }`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if violations := metadataDependencyViolations(fset, file); len(violations) != 0 {
		t.Fatalf("metadata dependency guard rejected unrelated method: %v", violations)
	}
}

func TestMetadataDependencyGuardRejectsOrdinaryGlobalAccess(t *testing.T) {
	specialMetadataOwners := map[string]struct{}{
		"authz_casbin":     {},
		"batch_requests":   {},
		"chaitin_waf":      {},
		"error_log_logger": {},
		"otel":             {},
	}
	fset := token.NewFileSet()
	var violations []string
	err := filepath.WalkDir(".", func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(filePath) != ".go" || strings.HasSuffix(filePath, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(".", filePath)
		if err != nil {
			return err
		}
		owner := strings.Split(filepath.ToSlash(relative), "/")[0]
		if _, allowed := specialMetadataOwners[owner]; allowed {
			return nil
		}
		file, err := parser.ParseFile(fset, filePath, nil, 0)
		if err != nil {
			return err
		}
		violations = append(violations, metadataDependencyViolations(fset, file)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk production plugin sources: %v", err)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("ordinary plugins access global metadata: %s", strings.Join(violations, ", "))
	}
}

func metadataDependencyViolations(fset *token.FileSet, file *ast.File) []string {
	const (
		basePath  = "github.com/wklken/apisix-go/pkg/plugin/base"
		storePath = "github.com/wklken/apisix-go/pkg/store"
	)
	baseAliases := make(map[string]struct{})
	storeAliases := make(map[string]struct{})
	baseDot := false
	storeDot := false
	for _, declaration := range file.Imports {
		importPath, err := strconv.Unquote(declaration.Path.Value)
		if err != nil {
			continue
		}
		alias := path.Base(importPath)
		if declaration.Name != nil {
			alias = declaration.Name.Name
		}
		switch importPath {
		case basePath:
			if alias == "." {
				baseDot = true
			} else {
				baseAliases[alias] = struct{}{}
			}
		case storePath:
			if alias == "." {
				storeDot = true
			} else {
				storeAliases[alias] = struct{}{}
			}
		}
	}

	var violations []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		called := metadataCalledExpression(call.Fun)
		switch expression := called.(type) {
		case *ast.SelectorExpr:
			alias, ok := expression.X.(*ast.Ident)
			if !ok {
				return true
			}
			_, baseImport := baseAliases[alias.Name]
			_, storeImport := storeAliases[alias.Name]
			if (baseImport && expression.Sel.Name == "LoadPluginMetadata") ||
				(storeImport && forbiddenStoreMetadataCall(expression.Sel.Name)) {
				violations = append(violations, fset.Position(call.Pos()).String())
			}
		case *ast.Ident:
			if (baseDot && expression.Name == "LoadPluginMetadata") ||
				(storeDot && forbiddenStoreMetadataCall(expression.Name)) {
				violations = append(violations, fset.Position(call.Pos()).String())
			}
		}
		return true
	})
	return violations
}

func metadataCalledExpression(expression ast.Expr) ast.Expr {
	for {
		switch wrapped := expression.(type) {
		case *ast.ParenExpr:
			expression = wrapped.X
		case *ast.IndexExpr:
			expression = wrapped.X
		case *ast.IndexListExpr:
			expression = wrapped.X
		default:
			return expression
		}
	}
}

func forbiddenStoreMetadataCall(name string) bool {
	switch name {
	case "GetPluginMetadata", "GetPluginMetadataRaw", "GetValidatedPluginMetadata":
		return true
	default:
		return false
	}
}
