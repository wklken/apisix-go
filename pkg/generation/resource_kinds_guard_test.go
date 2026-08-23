package generation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const generationImportPath = "github.com/wklken/apisix-go/pkg/generation"

type managedAuditFile struct {
	path  string
	file  *ast.File
	alias string
}

type managedComposite struct {
	file *managedAuditFile
	name string
	node *ast.CompositeLit
}

type managedClassifier struct {
	file *managedAuditFile
	node *ast.FuncDecl
}

func TestManagedResourceKindsASTGuardFixtures(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]string
		required   []string
		wantErrors []string
	}{
		{
			name: "explicit alias is consumed across another file",
			files: map[string]string{
				"consumer.go": `package fixture
import gen "github.com/wklken/apisix-go/pkg/generation"
func managed(kind string) bool { return gen.IsManagedResourceKind(kind) }
func domains(kind string) []gen.Domain { return gen.DomainsForResourceKind(kind) }`,
				"other.go": `package fixture
func helper() string { return "routes" }`,
			},
			required: []string{"IsManagedResourceKind", "DomainsForResourceKind"},
		},
		{
			name: "dot import is rejected",
			files: map[string]string{
				"dot.go": `package fixture
import . "github.com/wklken/apisix-go/pkg/generation"
func managed(kind string) bool { return IsManagedResourceKind(kind) }`,
			},
			wantErrors: []string{"dot-imports generation"},
		},
		{
			name: "assignment switch in another file is rejected",
			files: map[string]string{
				"consumer.go": `package fixture
import gen "github.com/wklken/apisix-go/pkg/generation"
func managed(kind string) bool { return gen.IsManagedResourceKind(kind) }`,
				"duplicate.go": `package fixture
import gen "github.com/wklken/apisix-go/pkg/generation"
func domains(kind string) []gen.Domain {
 var result []gen.Domain
 switch kind {
 case "routes": result = []gen.Domain{gen.DomainHTTP}
 case "services": result = append(result, gen.DomainStream)
 }
 return result
}`,
			},
			wantErrors: []string{"duplicate.go", "switch classifier"},
		},
		{
			name: "map taxonomy is rejected",
			files: map[string]string{
				"duplicate.go": `package fixture
var managed = map[string]bool{"routes": true, "services": true}
func isManaged(kind string) bool { return managed[kind] }`,
			},
			wantErrors: []string{"duplicate.go", "editable managed-kind map"},
		},
		{
			name: "slice taxonomy consumed by classifier is rejected",
			files: map[string]string{
				"list.go": `package fixture
var managedKinds = []string{"routes", "services"}`,
				"classifier.go": `package fixture
import "slices"
func managed(kind string) bool { return slices.Contains(managedKinds, kind) }`,
			},
			wantErrors: []string{"list.go", "classifier list"},
		},
		{
			name: "semantic exceptions and shape parser are allowed",
			files: map[string]string{
				"pkg/config/standalone.go": `package config
var standaloneBuckets = []string{"routes", "services"}`,
				"pkg/store/store.go": `package store
func IsHTTPRouteReloadBucket(bucket string) bool {
 switch bucket { case "routes", "services": return true; default: return false }
}
func IsStreamReloadBucket(bucket string) bool { return bucket == "services" }
func parse(kind string) (string, string, bool) {
 switch kind { case "plugins": return "plugins", "plugins", true; case "secrets": return "", "", false }
 return kind, kind, true
}`,
			},
		},
		{
			name: "semantic exceptions are bound to declaration identity",
			files: map[string]string{
				"pkg/store/other.go": `package store
var standaloneBuckets = []string{"routes", "services"}
func IsHTTPRouteReloadBucket(bucket string) bool {
 switch bucket { case "routes", "services": return true; default: return false }
}`,
			},
			wantErrors: []string{"other.go", "standaloneBuckets outside config standalone", "switch classifier"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			diagnostics := auditManagedResourceSources(test.files, test.required)
			joined := strings.Join(diagnostics, "\n")
			for _, want := range test.wantErrors {
				if !strings.Contains(joined, want) {
					t.Errorf("diagnostics = %q, want substring %q", joined, want)
				}
			}
			if len(test.wantErrors) == 0 && len(diagnostics) != 0 {
				t.Fatalf("diagnostics = %v, want none", diagnostics)
			}
		})
	}
}

func TestProductionGoSourcesScansEveryNonTestFile(t *testing.T) {
	directory := t.TempDir()
	for name, source := range map[string]string{
		"first.go":        "package fixture\n",
		"second.go":       "package fixture\n",
		"ignored_test.go": "package fixture\n",
		"README.md":       "not Go",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := productionGoSources(directory)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(directory, "first.go"), filepath.Join(directory, "second.go")}
	got := make([]string, 0, len(sources))
	for path := range sources {
		got = append(got, path)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("productionGoSources() paths = %v, want %v", got, want)
	}
}

func TestManagedResourceKindsConsumerASTGuard(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate resource_kinds_guard_test.go")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
	tests := []struct {
		directory string
		required  []string
	}{
		{directory: "pkg/etcd", required: []string{"IsManagedResourceKind", "DomainsForResourceKind"}},
		{directory: "pkg/config", required: []string{"DomainsForResourceKind"}},
		{directory: "pkg/store", required: []string{"ManagedResourceKinds", "IsManagedResourceKind"}},
	}
	for _, test := range tests {
		t.Run(test.directory, func(t *testing.T) {
			sources, err := productionGoSources(filepath.Join(repositoryRoot, test.directory))
			if err != nil {
				t.Fatal(err)
			}
			if diagnostics := auditManagedResourceSources(sources, test.required); len(diagnostics) != 0 {
				t.Fatalf("managed-resource taxonomy drift:\n%s", strings.Join(diagnostics, "\n"))
			}
		})
	}
}

func productionGoSources(directory string) (map[string]string, error) {
	sources := make(map[string]string)
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sources[path] = string(content)
		return nil
	})
	return sources, err
}

func auditManagedResourceSources(sources map[string]string, requiredCalls []string) []string {
	fset := token.NewFileSet()
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	diagnostics := make([]string, 0)
	files := make([]*managedAuditFile, 0, len(paths))
	for _, path := range paths {
		parsed, err := parser.ParseFile(fset, path, sources[path], 0)
		if err != nil {
			diagnostics = append(diagnostics, path+": parse: "+err.Error())
			continue
		}
		file := &managedAuditFile{path: path, file: parsed}
		for _, imported := range parsed.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil || importPath != generationImportPath {
				continue
			}
			switch {
			case imported.Name == nil:
				file.alias = "generation"
			case imported.Name.Name == ".":
				diagnostics = append(diagnostics, path+": dot-imports generation")
			case imported.Name.Name != "_":
				file.alias = imported.Name.Name
			}
		}
		files = append(files, file)
	}

	managedKinds := make(map[string]struct{}, len(managedResources))
	for _, kind := range ManagedResourceKinds() {
		managedKinds[kind] = struct{}{}
	}
	forbiddenFunctions := map[string]struct{}{
		"desiredDomains":      {},
		"isManagedEtcdBucket": {},
		"isManagedBucket":     {},
	}
	required := make(map[string]bool, len(requiredCalls))
	for _, call := range requiredCalls {
		required[call] = false
	}

	composites := make(map[string]managedComposite)
	classifiers := make([]managedClassifier, 0)
	for _, file := range files {
		for _, declaration := range file.file.Decls {
			switch node := declaration.(type) {
			case *ast.FuncDecl:
				if _, forbidden := forbiddenFunctions[node.Name.Name]; forbidden {
					diagnostics = append(diagnostics, positionMessage(fset, file.path, node.Pos(),
						"forbidden taxonomy helper "+node.Name.Name))
				}
				if taxonomyClassifierResult(node, file.alias) {
					classifiers = append(classifiers, managedClassifier{file: file, node: node})
				}
			case *ast.GenDecl:
				for _, specification := range node.Specs {
					value, ok := specification.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for index, name := range value.Names {
						if name.Name == "builtInBuckets" {
							diagnostics = append(diagnostics, positionMessage(fset, file.path, name.Pos(),
								"forbidden taxonomy list builtInBuckets"))
						}
						if index >= len(value.Values) {
							continue
						}
						composite, ok := value.Values[index].(*ast.CompositeLit)
						if ok {
							composites[name.Name] = managedComposite{file: file, name: name.Name, node: composite}
						}
					}
				}
			}
		}

		if file.alias == "" {
			continue
		}
		ast.Inspect(file.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			qualifier, ok := selector.X.(*ast.Ident)
			if ok && qualifier.Name == file.alias {
				if _, expected := required[selector.Sel.Name]; expected {
					required[selector.Sel.Name] = true
				}
			}
			return true
		})
	}

	reportedComposites := make(map[token.Pos]struct{})
	for _, composite := range composites {
		if countManagedKindLiterals(composite.node, managedKinds) < 2 || allowedManagedComposite(composite) {
			continue
		}
		if composite.name == "standaloneBuckets" {
			diagnostics = append(diagnostics, positionMessage(fset, composite.file.path, composite.node.Pos(),
				"standaloneBuckets outside config standalone"))
			reportedComposites[composite.node.Pos()] = struct{}{}
			continue
		}
		if _, isMap := composite.node.Type.(*ast.MapType); isMap {
			diagnostics = append(diagnostics, positionMessage(fset, composite.file.path, composite.node.Pos(),
				"editable managed-kind map "+composite.name))
			reportedComposites[composite.node.Pos()] = struct{}{}
		}
	}

	for _, classifier := range classifiers {
		if allowedManagedClassifier(classifier) {
			continue
		}
		ast.Inspect(classifier.node.Body, func(node ast.Node) bool {
			switch candidate := node.(type) {
			case *ast.SwitchStmt:
				kindCount := 0
				for _, statement := range candidate.Body.List {
					clause, ok := statement.(*ast.CaseClause)
					if !ok {
						continue
					}
					for _, expression := range clause.List {
						kindCount += countManagedKindLiterals(expression, managedKinds)
					}
				}
				if kindCount >= 2 {
					diagnostics = append(diagnostics, positionMessage(fset, classifier.file.path, candidate.Pos(),
						"second managed-kind switch classifier "+classifier.node.Name.Name))
				}
			case *ast.CompositeLit:
				if countManagedKindLiterals(candidate, managedKinds) < 2 {
					break
				}
				if _, reported := reportedComposites[candidate.Pos()]; reported {
					break
				}
				diagnostics = append(diagnostics, positionMessage(fset, classifier.file.path, candidate.Pos(),
					"second editable managed-kind classifier literal"))
				reportedComposites[candidate.Pos()] = struct{}{}
			case *ast.Ident:
				composite, found := composites[candidate.Name]
				if !found || allowedManagedComposite(composite) ||
					countManagedKindLiterals(composite.node, managedKinds) < 2 {
					break
				}
				if _, reported := reportedComposites[composite.node.Pos()]; reported {
					break
				}
				diagnostics = append(diagnostics, positionMessage(fset, composite.file.path, composite.node.Pos(),
					"second editable managed-kind classifier list "+composite.name))
				reportedComposites[composite.node.Pos()] = struct{}{}
			}
			return true
		})
	}

	for call, found := range required {
		if !found {
			diagnostics = append(diagnostics, "missing generation."+call+" consumer")
		}
	}
	sort.Strings(diagnostics)
	return slices.Compact(diagnostics)
}

func taxonomyClassifierResult(function *ast.FuncDecl, generationAlias string) bool {
	if function.Type.Results == nil || len(function.Type.Results.List) != 1 {
		return false
	}
	result := function.Type.Results.List[0]
	if len(result.Names) > 1 {
		return false
	}
	switch resultType := result.Type.(type) {
	case *ast.Ident:
		return resultType.Name == "bool"
	case *ast.ArrayType:
		selector, ok := resultType.Elt.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		qualifier, ok := selector.X.(*ast.Ident)
		return ok && generationAlias != "" && qualifier.Name == generationAlias && selector.Sel.Name == "Domain"
	default:
		return false
	}
}

func allowedManagedComposite(composite managedComposite) bool {
	return composite.name == "standaloneBuckets" && hasPathSuffix(composite.file.path, "pkg/config/standalone.go")
}

func allowedManagedClassifier(classifier managedClassifier) bool {
	if !hasPathSuffix(classifier.file.path, "pkg/store/store.go") {
		return false
	}
	return classifier.node.Name.Name == "IsHTTPRouteReloadBucket" ||
		classifier.node.Name.Name == "IsStreamReloadBucket"
}

func hasPathSuffix(path, suffix string) bool {
	path = filepath.ToSlash(path)
	suffix = filepath.ToSlash(suffix)
	return path == suffix || strings.HasSuffix(path, "/"+suffix)
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

func positionMessage(fset *token.FileSet, path string, position token.Pos, message string) string {
	return path + ":" + strconv.Itoa(fset.Position(position).Line) + ": " + message
}
