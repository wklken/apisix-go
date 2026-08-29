package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCompositePostInitDoesNotOwnChildPreparation(t *testing.T) {
	tests := []struct {
		path      string
		forbidden map[string]struct{}
	}{
		{
			path: "workflow/plugin.go",
			forbidden: map[string]struct{}{
				"PostInit": {},
			},
		},
		{
			path: "multi_auth/plugin.go",
			forbidden: map[string]struct{}{
				"Parse":         {},
				"newAuthPlugin": {},
			},
		},
	}

	for _, test := range tests {
		t.Run(filepath.Dir(test.path), func(t *testing.T) {
			fset := token.NewFileSet()
			packageDir := filepath.Dir(test.path)
			files := compositeProductionFiles(t, fset, packageDir)
			roots, violations, err := compositeReachableForbiddenCalls(
				fset,
				"github.com/wklken/apisix-go/pkg/plugin/"+packageDir,
				files,
				"PostInit",
				test.forbidden,
			)
			if err != nil {
				t.Fatalf("inspect %s PostInit call graph: %v", test.path, err)
			}
			if roots != 1 {
				t.Fatalf("%s Plugin.PostInit roots = %d, want 1", test.path, roots)
			}
			if len(violations) != 0 {
				t.Fatalf("%s PostInit reaches child preparation: %s", test.path, strings.Join(violations, ", "))
			}
		})
	}
}

func TestCompositePostInitGuardFollowsReachableHelpers(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", `package fixture
type Plugin struct{}
func (*Plugin) PostInit() error { helper(); return nil }
func helper() { newAuthPlugin() }
func newAuthPlugin() {}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	roots, violations, err := compositeReachableForbiddenCalls(
		fset,
		"fixture",
		map[string]*ast.File{"fixture.go": file},
		"PostInit",
		map[string]struct{}{"newAuthPlugin": {}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if roots != 1 || len(violations) != 1 {
		t.Fatalf("roots = %d, violations = %v, want one reachable helper violation", roots, violations)
	}
}

func TestWorkflowProductionHasNoStoreImport(t *testing.T) {
	const source = "workflow/plugin.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, source, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}

	if violations := productionStoreImportViolations(fset, file); len(violations) != 0 {
		t.Fatalf("workflow production Store imports = %v, want none", violations)
	}
}

func compositeProductionFiles(t *testing.T, fset *token.FileSet, packageDir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		t.Fatalf("read %s: %v", packageDir, err)
	}
	files := make(map[string]*ast.File)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		filePath := filepath.Join(packageDir, entry.Name())
		file, err := parser.ParseFile(fset, filePath, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filePath, err)
		}
		files[filePath] = file
	}
	return files
}

func compositeReachableForbiddenCalls(
	fset *token.FileSet,
	packagePath string,
	files map[string]*ast.File,
	rootName string,
	forbidden map[string]struct{},
) (int, []string, error) {
	info := &types.Info{
		Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object),
	}
	parsed := make([]*ast.File, 0, len(files))
	for _, file := range files {
		parsed = append(parsed, file)
	}
	sort.Slice(parsed, func(i, j int) bool {
		return fset.Position(parsed[i].Pos()).Filename < fset.Position(parsed[j].Pos()).Filename
	})
	configuration := types.Config{Importer: secretGuardImporter{}, Error: func(error) {}}
	typedPackage, _ := configuration.Check(packagePath, fset, parsed, info)
	if typedPackage == nil {
		return 0, nil, types.Error{Fset: fset, Msg: "package has no type identity"}
	}

	declarations := make(map[*types.Func]*ast.FuncDecl)
	var queue []*types.Func
	for _, file := range parsed {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			object, ok := info.Defs[function.Name].(*types.Func)
			if !ok {
				return 0, nil, types.Error{Fset: fset, Pos: function.Pos(), Msg: "function has no type identity"}
			}
			declarations[object] = function
			if function.Recv != nil && function.Name.Name == rootName {
				queue = append(queue, object)
			}
		}
	}
	roots := len(queue)
	seen := make(map[*types.Func]struct{})
	violationSet := make(map[string]struct{})
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		if _, exists := seen[current]; exists {
			continue
		}
		seen[current] = struct{}{}
		ast.Inspect(declarations[current].Body, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			called, ok := info.Uses[identifier].(*types.Func)
			if !ok {
				return true
			}
			if _, rejected := forbidden[called.Name()]; rejected {
				violationSet[fset.Position(identifier.Pos()).String()+": "+called.FullName()] = struct{}{}
			}
			if called.Pkg() == typedPackage {
				if _, exists := declarations[called]; exists {
					queue = append(queue, called)
				}
			}
			return true
		})
	}
	violations := make([]string, 0, len(violationSet))
	for violation := range violationSet {
		violations = append(violations, violation)
	}
	sort.Strings(violations)
	return roots, violations, nil
}
