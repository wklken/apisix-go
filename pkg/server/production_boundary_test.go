package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const productionBoundaryModule = "github.com/wklken/apisix-go"

var productionBoundaryRoots = []string{
	"pkg/compiler",
	"pkg/route",
	"pkg/plugin",
	"pkg/server",
	"pkg/stream",
}

func TestProductionRuntimeHasNoGlobalStoreReads(t *testing.T) {
	repositoryRoot, err := productionBoundaryRepositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files, err := loadProductionBoundaryFiles(fset, repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	violations := auditProductionBoundary(fset, files)
	if len(files) == 0 {
		t.Fatal("production runtime boundary inspected no Go files")
	}
	if len(violations) != 0 {
		t.Fatalf("production runtime retains mutable/global owners:\n%s", strings.Join(violations, "\n"))
	}
}

func TestProductionBoundaryGuardDetectsAliasDotAndCrossPackageOwners(t *testing.T) {
	violations := auditProductionBoundaryFixture(t, map[string]string{
		"pkg/server/alias_fixture.go": `package server
import legacy "github.com/wklken/apisix-go/pkg/store"
func aliasRead() { _ = legacy.GetConsumer }`,
		"pkg/server/dot_fixture.go": `package server
import . "github.com/wklken/apisix-go/pkg/store"
func dotRead() { _, _ = GetProto("contract") }`,
		"pkg/server/stream_fixture.go": `package server
import streamlegacy "github.com/wklken/apisix-go/pkg/stream"
type streamRuntimeOwner interface { Reload([]int) error }
type fixtureServer struct {
	runtime streamRuntimeOwner
	concrete *streamlegacy.Runtime
}
func reloadFixture(server *fixtureServer) {
	_ = server.runtime.Reload(nil)
	reload := server.concrete.Reload
	_ = reload
}`,
	})
	joined := strings.Join(violations, "\n")
	for _, required := range []string{
		"global store selector legacy.GetConsumer",
		"global dot-imported store call GetProto",
		"legacy streamRuntimeOwner.Reload declaration",
		"legacy mutable publication selector streamRuntimeOwner.Reload",
		"legacy mutable publication selector Runtime.Reload",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("guard violations missing %q:\n%s", required, joined)
		}
	}
}

func TestProductionBoundaryGuardAllowsJournalStoreOwnership(t *testing.T) {
	violations := auditProductionBoundaryFixture(t, map[string]string{
		"pkg/compiler/journal_fixture.go": `package compiler
import journal "github.com/wklken/apisix-go/pkg/store"
type journalOwner struct { store *journal.Store }
func closeJournal(owner *journalOwner) { _ = owner.store.Close() }`,
	})
	if len(violations) != 0 {
		t.Fatalf("journal Store ownership violations = %v", violations)
	}
}

func auditProductionBoundaryFixture(t *testing.T, sources map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(sources))
	for fileName, source := range sources {
		parsed, err := parser.ParseFile(fset, fileName, source, 0)
		if err != nil {
			t.Fatalf("parse boundary fixture %s: %v", fileName, err)
		}
		files[fileName] = parsed
	}
	return auditProductionBoundary(fset, files)
}

func productionBoundaryRepositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		moduleFile := filepath.Join(directory, "go.mod")
		source, readErr := os.ReadFile(moduleFile)
		if readErr == nil && strings.Contains(string(source), "module "+productionBoundaryModule) {
			return directory, nil
		}
		if readErr != nil && !os.IsNotExist(readErr) {
			return "", readErr
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("locate repository root for %s", productionBoundaryModule)
		}
		directory = parent
	}
}

func loadProductionBoundaryFiles(
	fset *token.FileSet,
	repositoryRoot string,
) (map[string]*ast.File, error) {
	files := make(map[string]*ast.File)
	for _, root := range productionBoundaryRoots {
		absoluteRoot := filepath.Join(repositoryRoot, filepath.FromSlash(root))
		err := filepath.WalkDir(absoluteRoot, func(filePath string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(repositoryRoot, filePath)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			source, err := os.ReadFile(filePath)
			if err != nil {
				return err
			}
			parsed, err := parser.ParseFile(fset, relative, source, 0)
			if err != nil {
				return err
			}
			files[relative] = parsed
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", root, err)
		}
	}
	return files, nil
}

func auditProductionBoundary(fset *token.FileSet, files map[string]*ast.File) []string {
	packages := make(map[string][]string)
	for fileName, file := range files {
		key := filepath.Dir(fileName) + "\x00" + file.Name.Name
		packages[key] = append(packages[key], fileName)
	}
	packageKeys := make([]string, 0, len(packages))
	for key := range packages {
		packageKeys = append(packageKeys, key)
	}
	sort.Strings(packageKeys)

	var violations []string
	for _, packageKey := range packageKeys {
		fileNames := packages[packageKey]
		sort.Strings(fileNames)
		parsed := make([]*ast.File, 0, len(fileNames))
		for _, fileName := range fileNames {
			parsed = append(parsed, files[fileName])
		}
		info := &types.Info{
			Defs:       make(map[*ast.Ident]types.Object),
			Uses:       make(map[*ast.Ident]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
		}
		configuration := types.Config{Importer: productionBoundaryImporter{}, Error: func(error) {}}
		_, _ = configuration.Check(
			productionBoundaryModule+"/"+strings.Split(packageKey, "\x00")[0],
			fset,
			parsed,
			info,
		)
		for _, fileName := range fileNames {
			violations = append(violations, auditProductionBoundaryFile(fset, fileName, files[fileName], info)...)
		}
	}
	sort.Strings(violations)
	return violations
}

func auditProductionBoundaryFile(
	fset *token.FileSet,
	fileName string,
	file *ast.File,
	info *types.Info,
) []string {
	imports := make(map[string]string)
	var violations []string
	for _, declaration := range file.Imports {
		importPath, err := strconv.Unquote(declaration.Path.Value)
		if err != nil {
			continue
		}
		alias := path.Base(importPath)
		if declaration.Name != nil {
			alias = declaration.Name.Name
		}
		imports[alias] = importPath
		if importPath == "go.etcd.io/bbolt" || importPath == "go.etcd.io/bbolt/v2" {
			violations = append(violations, fmt.Sprintf(
				"%s: direct bbolt import %s", fset.Position(declaration.Pos()), importPath,
			))
		}
	}
	// Imports are checked before the remaining AST because a bbolt receiver is
	// forbidden in these runtime packages even when type checking cannot load
	// its external method set.
	record := func(position token.Pos, reason string) {
		violations = append(violations, fmt.Sprintf("%s: %s", fset.Position(position), reason))
	}

	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if function.Name.Name == "startReloadScheduler" || function.Name.Name == "listenReloadEvent" {
			record(function.Name.Pos(), "legacy reload scheduler owner "+function.Name.Name)
		}
		if strings.HasPrefix(fileName, "pkg/route/") && strings.HasPrefix(function.Name.Name, "NewBuilder") {
			record(function.Name.Pos(), "legacy route builder constructor "+function.Name.Name)
		}
		if function.Recv == nil {
			continue
		}
		receiver := productionBoundaryReceiverName(info, function)
		if function.Name.Name == "Replace" && receiver == "routeHandler" {
			record(function.Name.Pos(), "legacy routeHandler.Replace declaration")
		}
		if function.Name.Name == "Reload" && (receiver == "Runtime" || receiver == "Router") &&
			strings.HasPrefix(fileName, "pkg/stream/") {
			record(function.Name.Pos(), "legacy stream "+receiver+".Reload declaration")
		}
	}

	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CallExpr:
			productionBoundaryAuditCall(fset, imports, info, node, record)
		case *ast.TypeSpec:
			if node.Name.Name == "Builder" && strings.HasPrefix(fileName, "pkg/route/") {
				record(node.Name.Pos(), "legacy runtime owner type "+node.Name.Name)
			}
			productionBoundaryAuditInterface(node, record)
		case *ast.ChanType:
			if productionBoundaryContainsStoreEvent(node.Value, imports) {
				record(node.Pos(), "legacy Store Event channel")
			}
		case *ast.SelectorExpr:
			productionBoundaryAuditOwnerSelector(file, imports, info, node, record)
		}
		return true
	})
	return violations
}

func productionBoundaryAuditCall(
	fset *token.FileSet,
	imports map[string]string,
	info *types.Info,
	call *ast.CallExpr,
	record func(token.Pos, string),
) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if ok {
		if productionBoundaryReloadOwnerName(selector.Sel.Name) {
			record(selector.Pos(), "legacy reload scheduler call "+selector.Sel.Name)
		}
		return
	}
	identifier, ok := call.Fun.(*ast.Ident)
	if !ok {
		return
	}
	if imports["."] == productionBoundaryModule+"/pkg/store" &&
		productionBoundaryForbiddenStoreCall(identifier.Name) {
		record(identifier.Pos(), "global dot-imported store call "+identifier.Name)
	}
	if strings.HasPrefix(identifier.Name, "NewBuilder") {
		record(identifier.Pos(), "legacy runtime constructor "+identifier.Name)
	}
	if productionBoundaryReloadOwnerName(identifier.Name) {
		record(identifier.Pos(), "legacy reload scheduler call "+identifier.Name)
	}
	_ = fset
}

func productionBoundaryAuditOwnerSelector(
	file *ast.File,
	imports map[string]string,
	info *types.Info,
	selector *ast.SelectorExpr,
	record func(token.Pos, string),
) {
	qualifier, ok := selector.X.(*ast.Ident)
	if ok {
		switch imports[qualifier.Name] {
		case productionBoundaryModule + "/pkg/store":
			if productionBoundaryForbiddenStoreCall(selector.Sel.Name) {
				record(selector.Pos(), "global store selector "+qualifier.Name+"."+selector.Sel.Name)
			}
		case "go.etcd.io/bbolt", "go.etcd.io/bbolt/v2":
			record(selector.Pos(), "direct bbolt selector "+qualifier.Name+"."+selector.Sel.Name)
		case productionBoundaryModule + "/pkg/route":
			if selector.Sel.Name == "Builder" || strings.HasPrefix(selector.Sel.Name, "NewBuilder") {
				record(selector.Pos(), "legacy route builder symbol "+qualifier.Name+"."+selector.Sel.Name)
			}
		case productionBoundaryModule + "/pkg/proxy":
			if selector.Sel.Name == "ClusterRegistry" || selector.Sel.Name == "NewClusterRegistry" {
				record(selector.Pos(), "legacy cluster registry symbol "+qualifier.Name+"."+selector.Sel.Name)
			}
		}
	}
	if receiver, forbidden := productionBoundaryLifecycleReceiver(file, imports, info, selector); forbidden {
		record(selector.Pos(), "legacy mutable publication selector "+receiver+"."+selector.Sel.Name)
	}
}

func productionBoundaryAuditInterface(
	specification *ast.TypeSpec,
	record func(token.Pos, string),
) {
	if specification == nil || specification.Name.Name != "streamRuntimeOwner" {
		return
	}
	owner, ok := specification.Type.(*ast.InterfaceType)
	if !ok || owner.Methods == nil {
		return
	}
	for _, method := range owner.Methods.List {
		for _, name := range method.Names {
			if name.Name == "Reload" {
				record(name.Pos(), "legacy streamRuntimeOwner.Reload declaration")
			}
		}
	}
}

type productionBoundaryTypeReference struct {
	packagePath string
	name        string
}

func productionBoundaryLifecycleReceiver(
	file *ast.File,
	imports map[string]string,
	info *types.Info,
	selector *ast.SelectorExpr,
) (string, bool) {
	if selector == nil || (selector.Sel.Name != "Replace" && selector.Sel.Name != "Reload") {
		return "", false
	}
	if productionBoundaryForbiddenLifecycleCall(info, selector) {
		receiver := productionBoundaryTypeReferenceFromTypes(info.Selections[selector].Recv())
		return receiver.name, true
	}
	receiver := productionBoundaryExpressionType(file, imports, info, selector.X)
	if !productionBoundaryForbiddenLifecycleReceiver(receiver, selector.Sel.Name) {
		return "", false
	}
	return receiver.name, true
}

func productionBoundaryForbiddenLifecycleReceiver(
	receiver productionBoundaryTypeReference,
	method string,
) bool {
	switch method {
	case "Replace":
		return receiver.name == "routeHandler"
	case "Reload":
		return receiver.name == "streamRuntimeOwner" ||
			receiver.packagePath == productionBoundaryModule+"/pkg/stream" &&
				(receiver.name == "Runtime" || receiver.name == "Router")
	default:
		return false
	}
}

func productionBoundaryExpressionType(
	file *ast.File,
	imports map[string]string,
	info *types.Info,
	expression ast.Expr,
) productionBoundaryTypeReference {
	if info != nil {
		if resolved := productionBoundaryTypeReferenceFromTypes(info.TypeOf(expression)); resolved.name != "" {
			return resolved
		}
	}
	switch expression := expression.(type) {
	case *ast.ParenExpr:
		return productionBoundaryExpressionType(file, imports, info, expression.X)
	case *ast.StarExpr:
		return productionBoundaryExpressionType(file, imports, info, expression.X)
	case *ast.Ident:
		if expression.Obj == nil {
			return productionBoundaryTypeReference{}
		}
		switch declaration := expression.Obj.Decl.(type) {
		case *ast.Field:
			return productionBoundaryTypeExpression(imports, declaration.Type)
		case *ast.ValueSpec:
			return productionBoundaryTypeExpression(imports, declaration.Type)
		}
	case *ast.SelectorExpr:
		if qualifier, ok := expression.X.(*ast.Ident); ok {
			if importPath := imports[qualifier.Name]; importPath != "" {
				return productionBoundaryTypeReference{packagePath: importPath, name: expression.Sel.Name}
			}
		}
		owner := productionBoundaryExpressionType(file, imports, info, expression.X)
		if owner.name == "" {
			return productionBoundaryTypeReference{}
		}
		fieldType := productionBoundaryLocalStructField(file, owner.name, expression.Sel.Name)
		return productionBoundaryTypeExpression(imports, fieldType)
	}
	return productionBoundaryTypeReference{}
}

func productionBoundaryTypeExpression(
	imports map[string]string,
	expression ast.Expr,
) productionBoundaryTypeReference {
	switch expression := expression.(type) {
	case *ast.ParenExpr:
		return productionBoundaryTypeExpression(imports, expression.X)
	case *ast.StarExpr:
		return productionBoundaryTypeExpression(imports, expression.X)
	case *ast.Ident:
		return productionBoundaryTypeReference{name: expression.Name}
	case *ast.SelectorExpr:
		qualifier, ok := expression.X.(*ast.Ident)
		if !ok {
			return productionBoundaryTypeReference{}
		}
		return productionBoundaryTypeReference{packagePath: imports[qualifier.Name], name: expression.Sel.Name}
	default:
		return productionBoundaryTypeReference{}
	}
}

func productionBoundaryLocalStructField(file *ast.File, owner, fieldName string) ast.Expr {
	for _, declaration := range file.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok || generic.Tok != token.TYPE {
			continue
		}
		for _, raw := range generic.Specs {
			specification, ok := raw.(*ast.TypeSpec)
			if !ok || specification.Name.Name != owner {
				continue
			}
			structure, ok := specification.Type.(*ast.StructType)
			if !ok || structure.Fields == nil {
				return nil
			}
			for _, field := range structure.Fields.List {
				for _, name := range field.Names {
					if name.Name == fieldName {
						return field.Type
					}
				}
			}
		}
	}
	return nil
}

func productionBoundaryTypeReferenceFromTypes(value types.Type) productionBoundaryTypeReference {
	if pointer, ok := value.(*types.Pointer); ok {
		value = pointer.Elem()
	}
	named, ok := value.(*types.Named)
	if !ok || named.Obj() == nil {
		return productionBoundaryTypeReference{}
	}
	reference := productionBoundaryTypeReference{name: named.Obj().Name()}
	if named.Obj().Pkg() != nil {
		reference.packagePath = named.Obj().Pkg().Path()
	}
	return reference
}

func productionBoundaryForbiddenStoreCall(name string) bool {
	return name == "GetStore" || strings.HasPrefix(name, "Get") || strings.HasPrefix(name, "List") ||
		name == "MaterializeSecret" || name == "ResolveSecretReference"
}

func productionBoundaryForbiddenLifecycleCall(info *types.Info, selector *ast.SelectorExpr) bool {
	selection := info.Selections[selector]
	if selection == nil {
		return false
	}
	receiver := productionBoundaryTypeReferenceFromTypes(selection.Recv())
	return productionBoundaryForbiddenLifecycleReceiver(receiver, selector.Sel.Name)
}

func productionBoundaryReloadOwnerName(name string) bool {
	switch name {
	case "NewAcknowledgedEvent", "NewAcknowledgedBatch", "AddAcknowledgedEventUpdateHook",
		"startReloadScheduler", "listenReloadEvent":
		return true
	default:
		return false
	}
}

func productionBoundaryContainsStoreEvent(expression ast.Expr, imports map[string]string) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Event" {
			return true
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if ok && imports[qualifier.Name] == productionBoundaryModule+"/pkg/store" {
			found = true
			return false
		}
		return true
	})
	return found
}

func productionBoundaryReceiverName(info *types.Info, function *ast.FuncDecl) string {
	if function == nil || function.Recv == nil || len(function.Recv.List) == 0 {
		return ""
	}
	if object, ok := info.Defs[function.Name].(*types.Func); ok {
		if signature, ok := object.Type().(*types.Signature); ok && signature.Recv() != nil {
			return productionBoundaryNamedType(signature.Recv().Type())
		}
	}
	return productionBoundaryReceiverSyntax(function.Recv.List[0].Type)
}

func productionBoundaryNamedType(value types.Type) string {
	if pointer, ok := value.(*types.Pointer); ok {
		value = pointer.Elem()
	}
	named, ok := value.(*types.Named)
	if !ok {
		return ""
	}
	return named.Obj().Name()
}

func productionBoundaryReceiverSyntax(expression ast.Expr) string {
	if star, ok := expression.(*ast.StarExpr); ok {
		expression = star.X
	}
	identifier, _ := expression.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}

type productionBoundaryImporter struct{}

func (productionBoundaryImporter) Import(importPath string) (*types.Package, error) {
	imported := types.NewPackage(importPath, path.Base(importPath))
	imported.MarkComplete()
	return imported, nil
}
