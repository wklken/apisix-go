package httpclient

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestProductionHTTPClientsDoNotUseEnvironmentProxyDefaults(t *testing.T) {
	t.Parallel()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve policy test path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	files := token.NewFileSet()
	var violations []string
	for _, root := range []string{"pkg/plugin", "pkg/secret"} {
		err := filepath.WalkDir(
			filepath.Join(repoRoot, root),
			func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				parsed, err := parser.ParseFile(files, path, nil, 0)
				if err != nil {
					return err
				}
				httpPackage := ""
				for _, imported := range parsed.Imports {
					if imported.Path.Value != `"net/http"` {
						continue
					}
					httpPackage = "http"
					if imported.Name != nil {
						httpPackage = imported.Name.Name
					}
					break
				}
				if httpPackage == "" || httpPackage == "_" || httpPackage == "." {
					return nil
				}
				ast.Inspect(parsed, func(node ast.Node) bool {
					switch value := node.(type) {
					case *ast.SelectorExpr:
						identifier, ok := value.X.(*ast.Ident)
						if !ok || identifier.Name != httpPackage {
							return true
						}
						switch value.Sel.Name {
						case "DefaultClient", "DefaultTransport", "Get", "Head", "Post", "PostForm":
							violations = append(
								violations,
								policyViolation(
									repoRoot,
									files,
									value.Pos(),
									"uses net/http environment-proxy default",
								),
							)
						}
					case *ast.CompositeLit:
						selector, ok := value.Type.(*ast.SelectorExpr)
						if !ok || selector.Sel.Name != "Client" {
							return true
						}
						identifier, ok := selector.X.(*ast.Ident)
						if !ok || identifier.Name != httpPackage {
							return true
						}
						for _, element := range value.Elts {
							field, ok := element.(*ast.KeyValueExpr)
							if !ok {
								continue
							}
							name, ok := field.Key.(*ast.Ident)
							if ok && name.Name == "Transport" {
								return true
							}
						}
						violations = append(
							violations,
							policyViolation(repoRoot, files, value.Pos(), "http.Client omits Transport"),
						)
					}
					return true
				})
				return nil
			},
		)
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
	}
	slices.Sort(violations)
	if len(violations) > 0 {
		t.Fatalf("production HTTP clients may inherit environment proxies:\n%s", strings.Join(violations, "\n"))
	}
}

func policyViolation(repoRoot string, files *token.FileSet, position token.Pos, message string) string {
	location := files.Position(position)
	relative, err := filepath.Rel(repoRoot, location.Filename)
	if err != nil {
		relative = location.Filename
	}
	return fmt.Sprintf("%s:%d: %s", filepath.ToSlash(relative), location.Line, message)
}
