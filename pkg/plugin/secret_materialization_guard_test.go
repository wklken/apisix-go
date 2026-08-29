package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSecretMaterializationGuardRejectsDirectResolution(t *testing.T) {
	fset := token.NewFileSet()
	var violations []string
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "ResolveSecretReference" {
				return true
			}
			violations = append(violations, fset.Position(call.Pos()).String())
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk plugin sources: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf(
			"direct ResolveSecretReference calls bypass secret materialization: %s",
			strings.Join(violations, ", "),
		)
	}
}

func TestProductionPluginsDoNotReadGlobalStore(t *testing.T) {
	fset := token.NewFileSet()
	var violations []string
	err := filepath.WalkDir(".", func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(filePath) != ".go" || strings.HasSuffix(filePath, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, filePath, nil, 0)
		if err != nil {
			return err
		}
		violations = append(violations, productionStoreImportViolations(fset, file)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk plugin sources: %v", err)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("production plugins import pkg/store: %s", strings.Join(violations, ", "))
	}
}

func TestProductionStoreImportGuardRejectsAliasesDotImportsAndFunctionValues(t *testing.T) {
	tests := map[string]string{
		"default import": `package fixture
import "github.com/wklken/apisix-go/pkg/store"
var _ = store.GetConsumer`,
		"alias function value": `package fixture
import state "github.com/wklken/apisix-go/pkg/store"
var lookup = state.GetConsumer`,
		"dot import": `package fixture
import . "github.com/wklken/apisix-go/pkg/store"
var lookup = GetConsumer`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			violations := productionStoreImportViolations(fset, file)
			if len(violations) != 1 {
				t.Fatalf("store import violations = %v, want one", violations)
			}
		})
	}
}

func productionStoreImportViolations(fset *token.FileSet, file *ast.File) []string {
	var violations []string
	for _, spec := range file.Imports {
		if strings.Trim(spec.Path.Value, `"`) == "github.com/wklken/apisix-go/pkg/store" {
			violations = append(violations, fset.Position(spec.Pos()).String()+": imports pkg/store")
		}
	}
	return violations
}

func TestSecretMaterializationGuardRejectsScopedLegacyResolution(t *testing.T) {
	packages := []string{
		"ai_rate_limiting", "csrf", "kafka_proxy", "response_rewrite",
		"elasticsearch_logger", "error_log_logger", "google_cloud_logging",
		"http_logger", "kafka_logger", "lago", "loggly", "rocketmq_logger",
		"sls_logger", "splunk_hec_logging", "tencent_cloud_cls", "workflow", "multi_auth",
	}
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	var violations []string
	for _, packageName := range packages {
		entries, err := os.ReadDir(packageName)
		if err != nil {
			t.Fatalf("read plugin package %q: %v", packageName, err)
		}
		packageFiles := make(map[string]*ast.File)
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(packageName, entry.Name())
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			packageFiles[path] = file
			files[path] = file
		}
		materializers, packageViolations, err := scopedLegacyResolutionViolations(
			fset,
			"github.com/wklken/apisix-go/pkg/plugin/"+packageName,
			packageFiles,
		)
		if err != nil {
			t.Fatalf("type-check plugin package %q: %v", packageName, err)
		}
		if materializers != 1 {
			t.Errorf("plugin package %q scoped materializers = %d, want 1", packageName, materializers)
		}
		violations = append(violations, packageViolations...)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("scoped secret materializers call legacy/raw helpers: %s", strings.Join(violations, ", "))
	}
	if len(files) == 0 {
		t.Fatal("no S2 production files were inspected")
	}
}

func TestScopedLegacyResolutionGuardRejectsAliasesAndReachableHelpers(t *testing.T) {
	tests := map[string]string{
		"method value alias": `package fixture
type Plugin struct{}
func (p *Plugin) DataEncryption() {}
func (p *Plugin) MaterializeScopedSecrets() error {
	resolve := p.DataEncryption
	_ = resolve
	return nil
}`,
		"reachable helper": `package fixture
type Plugin struct{}
type Resolver struct{}
func (Resolver) ResolveForContext(string, string) (string, error) { return "", nil }
func (p *Plugin) DataEncryption() Resolver { return Resolver{} }
func (p *Plugin) MaterializeScopedSecrets() error { return p.prepare() }
func (p *Plugin) prepare() error {
	resolver := p.DataEncryption()
	call := resolver.ResolveForContext
	_ = call
	return nil
}`,
		"receiver alias": `package fixture
type Plugin struct{}
func (p *Plugin) DataEncryption() {}
func (p *Plugin) MaterializeScopedSecrets() error {
	alias := p
	return alias.prepare()
}
func (p *Plugin) prepare() error {
	p.DataEncryption()
	return nil
}`,
		"package function alias": `package fixture
import "os"
type Plugin struct{}
func (p *Plugin) MaterializeScopedSecrets() error {
	alias := prepare
	return alias()
}
func prepare() error {
	_ = os.Getenv("SECRET")
	return nil
}`,
		"package variable alias": `package fixture
import "os"
var readSecret = os.Getenv
type Plugin struct{}
func (p *Plugin) MaterializeScopedSecrets() error {
	_ = readSecret("SECRET")
	return nil
}`,
		"interface dispatch": `package fixture
type preparer interface { prepare() error }
type Plugin struct{}
func (p *Plugin) DataEncryption() {}
func (p *Plugin) MaterializeScopedSecrets() error {
	var owner preparer = p
	return owner.prepare()
}
func (p *Plugin) prepare() error {
	p.DataEncryption()
	return nil
}`,
		"dot import": `package fixture
import . "os"
type Plugin struct{}
func (p *Plugin) MaterializeScopedSecrets() error {
	_ = Getenv("SECRET")
	return nil
}`,
		"vault dot import": `package fixture
import . "github.com/hashicorp/vault/api"
type Plugin struct{}
func (p *Plugin) MaterializeScopedSecrets() error {
	_, _ = NewClient()
	return nil
}`,
		"raw resolver entry": `package fixture
type Plugin struct{}
func (p *Plugin) ResolveOptionalForContext() {}
func (p *Plugin) MaterializeScopedSecrets() error {
	call := p.ResolveOptionalForContext
	_ = call
	return nil
}`,
		"method expression": `package fixture
type Plugin struct{}
func (p *Plugin) ResolveDeclared() {}
func (p *Plugin) MaterializeScopedSecrets() error {
	call := (*Plugin).ResolveDeclared
	_ = call
	return nil
}`,
		"data encryption import alias": `package fixture
import de "github.com/wklken/apisix-go/pkg/data_encryption"
type Plugin struct{}
func (p *Plugin) MaterializeScopedSecrets() error {
	_ = de.DecryptPluginConfigWithResolver
	return nil
}`,
		"store import alias": `package fixture
import state "github.com/wklken/apisix-go/pkg/store"
type Plugin struct{}
func (p *Plugin) MaterializeScopedSecrets() error {
	_ = state.GetPluginMetadata
	return nil
}`,
		"environment import alias": `package fixture
import environment "os"
type Plugin struct{}
func (p *Plugin) MaterializeScopedSecrets() error {
	_ = environment.Getenv
	return nil
}`,
		"vault owner import alias": `package fixture
import secrets "github.com/wklken/apisix-go/pkg/store"
type Plugin struct{}
func (p *Plugin) MaterializeScopedSecrets() error {
	_ = secrets.GetPluginMetadata
	return nil
}`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			materializers, violations, err := scopedLegacyResolutionViolations(
				fset, "fixture", map[string]*ast.File{"fixture.go": file},
			)
			if err != nil {
				t.Fatal(err)
			}
			if materializers != 1 || len(violations) == 0 {
				t.Fatalf("guard result = %d/%v, want one materializer and a violation", materializers, violations)
			}
		})
	}
}

func TestScopedLegacyResolutionGuardDistinguishesReceiverIdentity(t *testing.T) {
	const source = `package fixture
import "os"
type Plugin struct{}
type Other struct{}
func (p *Plugin) MaterializeScopedSecrets() error { return p.prepare() }
func (p *Plugin) prepare() error { return nil }
func prepare() error {
	_ = os.Getenv("SECRET")
	return nil
}
func (p *Other) DataEncryption() {}
func (p *Other) prepare() error {
	p.DataEncryption()
	return nil
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	materializers, violations, err := scopedLegacyResolutionViolations(
		fset, "fixture", map[string]*ast.File{"fixture.go": file},
	)
	if err != nil {
		t.Fatal(err)
	}
	if materializers != 1 || len(violations) != 0 {
		t.Fatalf("guard result = %d/%v, want exact clean Plugin receiver path", materializers, violations)
	}
}

func scopedLegacyResolutionViolations(
	fset *token.FileSet,
	packagePath string,
	files map[string]*ast.File,
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
	configuration := types.Config{
		Importer: secretGuardImporter{},
		Error:    func(error) {},
	}
	typedPackage, _ := configuration.Check(packagePath, fset, parsed, info)
	if typedPackage == nil {
		return 0, nil, types.Error{Fset: fset, Msg: "package has no type identity"}
	}

	declarations := make(map[*types.Func]secretGuardDeclaration)
	values := make(map[*types.Var]secretGuardExpression)
	var roots []*types.Func
	for _, file := range parsed {
		for _, declaration := range file.Decls {
			if generated, ok := declaration.(*ast.GenDecl); ok && generated.Tok == token.VAR {
				secretGuardCollectValues(info, file, generated, values)
			}
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			object, ok := info.Defs[function.Name].(*types.Func)
			if !ok {
				return 0, nil, types.Error{Fset: fset, Pos: function.Pos(), Msg: "function has no type identity"}
			}
			declarations[object] = secretGuardDeclaration{function: function, file: file}
			if function.Name.Name == "MaterializeScopedSecrets" && function.Recv != nil {
				roots = append(roots, object)
			}
		}
	}

	queue := append([]*types.Func(nil), roots...)
	seen := make(map[*types.Func]struct{})
	seenValues := make(map[*types.Var]struct{})
	violationSet := make(map[string]struct{})
	var inspectNode func(ast.Node, *ast.File)
	inspectNode = func(root ast.Node, file *ast.File) {
		imports, dotImports := secretGuardImports(file)
		if len(dotImports) > 0 {
			violationSet[fset.Position(root.Pos()).String()+": forbidden dot import "+strings.Join(dotImports, ",")] = struct{}{}
		}
		ast.Inspect(root, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok {
				if secretGuardForbiddenName(selector.Sel.Name) {
					violationSet[fset.Position(selector.Sel.Pos()).String()+": "+selector.Sel.Name] = struct{}{}
				}
				if alias, aliasOK := selector.X.(*ast.Ident); aliasOK {
					if importPath := imports[alias.Name]; secretGuardForbiddenImport(importPath) {
						violationSet[fset.Position(selector.Pos()).String()+": "+importPath+"."+selector.Sel.Name] = struct{}{}
					}
				}
			}

			identifier, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			switch object := info.Uses[identifier].(type) {
			case *types.Func:
				if secretGuardForbiddenFunction(object) {
					violationSet[fset.Position(identifier.Pos()).String()+": "+object.FullName()] = struct{}{}
				}
				if object.Pkg() == typedPackage {
					if _, exists := declarations[object]; exists {
						queue = append(queue, object)
					}
				}
				queue = append(queue, secretGuardInterfaceImplementations(object, declarations)...)
			case *types.Var:
				initializer, exists := values[object]
				if !exists {
					return true
				}
				if _, inspected := seenValues[object]; inspected {
					return true
				}
				seenValues[object] = struct{}{}
				inspectNode(initializer.expression, initializer.file)
			}
			return true
		})
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if _, ok := seen[current]; ok {
			continue
		}
		seen[current] = struct{}{}
		declaration := declarations[current]
		inspectNode(declaration.function.Body, declaration.file)
	}

	violations := make([]string, 0, len(violationSet))
	for violation := range violationSet {
		violations = append(violations, violation)
	}
	sort.Strings(violations)
	return len(roots), violations, nil
}

type secretGuardDeclaration struct {
	function *ast.FuncDecl
	file     *ast.File
}

type secretGuardExpression struct {
	expression ast.Expr
	file       *ast.File
}

func secretGuardCollectValues(
	info *types.Info,
	file *ast.File,
	declaration *ast.GenDecl,
	values map[*types.Var]secretGuardExpression,
) {
	for _, specification := range declaration.Specs {
		value, ok := specification.(*ast.ValueSpec)
		if !ok || len(value.Values) == 0 {
			continue
		}
		for index, name := range value.Names {
			object, ok := info.Defs[name].(*types.Var)
			if !ok {
				continue
			}
			valueIndex := index
			if len(value.Values) == 1 {
				valueIndex = 0
			}
			if valueIndex >= len(value.Values) {
				continue
			}
			values[object] = secretGuardExpression{expression: value.Values[valueIndex], file: file}
		}
	}
}

func secretGuardInterfaceImplementations(
	method *types.Func,
	declarations map[*types.Func]secretGuardDeclaration,
) []*types.Func {
	signature, ok := method.Type().(*types.Signature)
	if !ok || signature.Recv() == nil {
		return nil
	}
	contract, ok := signature.Recv().Type().Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	contract.Complete()
	var implementations []*types.Func
	for candidate := range declarations {
		if candidate.Name() != method.Name() {
			continue
		}
		candidateSignature, ok := candidate.Type().(*types.Signature)
		if !ok || candidateSignature.Recv() == nil || !types.Implements(candidateSignature.Recv().Type(), contract) {
			continue
		}
		implementations = append(implementations, candidate)
	}
	return implementations
}

type secretGuardImporter struct{}

func (secretGuardImporter) Import(importPath string) (*types.Package, error) {
	imported := types.NewPackage(importPath, path.Base(importPath))
	imported.MarkComplete()
	return imported, nil
}

func secretGuardImports(file *ast.File) (map[string]string, []string) {
	aliases := make(map[string]string)
	var dotImports []string
	for _, declaration := range file.Imports {
		importPath := strings.Trim(declaration.Path.Value, `"`)
		alias := path.Base(importPath)
		if declaration.Name != nil {
			alias = declaration.Name.Name
		}
		if alias == "." {
			if secretGuardForbiddenImport(importPath) {
				dotImports = append(dotImports, importPath)
			}
			continue
		}
		aliases[alias] = importPath
	}
	return aliases, dotImports
}

func secretGuardForbiddenFunction(function *types.Func) bool {
	if secretGuardForbiddenName(function.Name()) {
		return true
	}
	if function.Pkg() == nil {
		return false
	}
	packagePath := function.Pkg().Path()
	return packagePath == "os" ||
		packagePath == "github.com/wklken/apisix-go/pkg/store" ||
		packagePath == "github.com/wklken/apisix-go/pkg/data_encryption" ||
		strings.Contains(strings.ToLower(packagePath), "/vault/") ||
		strings.HasSuffix(strings.ToLower(packagePath), "/vault")
}

func secretGuardForbiddenImport(importPath string) bool {
	return importPath == "os" ||
		importPath == "github.com/wklken/apisix-go/pkg/store" ||
		importPath == "github.com/wklken/apisix-go/pkg/data_encryption" ||
		strings.Contains(strings.ToLower(importPath), "/vault/") ||
		strings.HasSuffix(strings.ToLower(importPath), "/vault")
}

func secretGuardForbiddenName(name string) bool {
	switch name {
	case "DataEncryption", "DecryptPluginConfigWithResolver", "Environ", "ExpandEnv", "Getenv",
		"GetPluginMetadata", "GetPluginMetadataRaw", "GetValidatedPluginMetadata", "LookupEnv",
		"MaterializeSecret", "Resolve",
		"ResolveDeclared", "ResolveForContext", "ResolveScoped", "ResolveSecretReference":
		return true
	default:
		return strings.HasPrefix(name, "ResolveOptional")
	}
}
