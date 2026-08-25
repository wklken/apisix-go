package compiler

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	c6ModulePath     = "github.com/wklken/apisix-go"
	c6CompilerPath   = c6ModulePath + "/pkg/compiler"
	c6RuntimePath    = c6ModulePath + "/pkg/runtime"
	c6GenerationPath = c6ModulePath + "/pkg/generation"

	c6ApplyTicketOwner = "pkg/store/journal_apply.go"
	c6StoreDirectory   = "pkg/store"
)

var c6ProductionRoots = []string{
	"cmd",
	"pkg/config",
	"pkg/etcd",
	"pkg/store",
	"pkg/server",
	"pkg/route",
	"pkg/stream",
}

type c6BoundaryAudit struct {
	diagnostics                []string
	allowedTicketConstructions map[c6TicketConstruction]int
	inspectedFiles             int
}

type c6TicketConstruction struct {
	file     string
	function string
	form     string
}

var c6AllowedTicketConstructions = map[c6TicketConstruction]int{
	{c6ApplyTicketOwner, "(*Store).ApplyDesired", "zero-value composite"}:   3,
	{c6ApplyTicketOwner, "(*Store).ApplyDesired", "composite literal"}:      1,
	{c6ApplyTicketOwner, "(*Store).ApplyDesired", "zero-value declaration"}: 1,
	{c6ApplyTicketOwner, "loadCursorTx", "zero-value composite"}:            3,
	{c6ApplyTicketOwner, "applyTicketFromWire", "composite literal"}:        1,
}

func TestC6ProductionBoundaryGuardRejectsForbiddenFixture(t *testing.T) {
	fixture := map[string]string{
		"pkg/route/ticket.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
type ticketAlias = gen.ApplyTicket
func forgeTicket() { _ = ticketAlias{} }
`,
		"pkg/route/ticket_zero.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
var forged gen.ApplyTicket
`,
		"pkg/route/ticket_named_result.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func forgedResult() (ticket gen.ApplyTicket) { return }
`,
		"pkg/route/ticket_literal_named_result.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func invoke() {
	_ = func() (ticket gen.ApplyTicket) { return }()
}
`,
		"pkg/route/ticket_conversion.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
type ticketShape struct{}
func convertTicket() { _ = gen.ApplyTicket(ticketShape{}) }
`,
		"pkg/route/ticket_new.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func allocateTicket() { _ = new(gen.ApplyTicket) }
`,
		"pkg/route/ticket_aggregate.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func forgeFromAggregate() gen.ApplyTicket { return [1]gen.ApplyTicket{}[0] }
`,
		"pkg/store/journal_apply.go": `package store
import gen "github.com/wklken/apisix-go/pkg/generation"
func forgedInJournalFile() gen.ApplyTicket { return gen.ApplyTicket{} }
`,
		"pkg/store/ticket_aggregate.go": `package store
import gen "github.com/wklken/apisix-go/pkg/generation"
func forgeFromAggregate() gen.ApplyTicket { return [1]gen.ApplyTicket{}[0] }
`,
		"pkg/server/compiler_import.go": `package server
import build "github.com/wklken/apisix-go/pkg/compiler"
var factory *build.WorkerCompilerFactory
`,
		"pkg/stream/runtime_import.go": `package stream
import lifecycle "github.com/wklken/apisix-go/pkg/runtime"
var _ lifecycle.ResourceRegistry
`,
		"pkg/route/unrelated.go": `package route
type localFactory struct{}
func (localFactory) PrepareGeneration() {}
func unrelated(factory localFactory) { factory.PrepareGeneration() }
`,
	}

	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(fixture))
	for name, source := range fixture {
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = file
	}
	audit := auditC6ProductionBoundary(fset, files)
	joined := strings.Join(audit.diagnostics, "\n")
	for _, want := range []string{
		"pkg/route/ticket.go", "ApplyTicket construction",
		"pkg/route/ticket_zero.go", "zero-value declaration",
		"pkg/route/ticket_named_result.go", "named result",
		"pkg/route/ticket_literal_named_result.go",
		"pkg/route/ticket_conversion.go",
		"pkg/route/ticket_new.go",
		"pkg/route/ticket_aggregate.go", "ApplyTicket authority outside pkg/store",
		"pkg/store/journal_apply.go", "forgedInJournalFile",
		"pkg/store/ticket_aggregate.go", "aggregate extraction",
		"pkg/server/compiler_import.go", c6CompilerPath,
		"pkg/stream/runtime_import.go", c6RuntimePath,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("diagnostics = %q, want substring %q", joined, want)
		}
	}
	if strings.Contains(joined, "unrelated.go") {
		t.Fatalf("unrelated same-name method was rejected: %s", joined)
	}
}

func TestC6ProductionBoundaryGuardAllowsTicketTransportAndPointerTypes(t *testing.T) {
	fixture := map[string]string{
		"pkg/store/transport.go": `package store
import gen "github.com/wklken/apisix-go/pkg/generation"
type envelope struct { Ticket gen.ApplyTicket }
func transport(ticket gen.ApplyTicket) gen.ApplyTicket { return ticket }
`,
		"pkg/store/pointers.go": `package store
import gen "github.com/wklken/apisix-go/pkg/generation"
func pointerTypes() {
	_ = new(*gen.ApplyTicket)
	_ = (*gen.ApplyTicket)(nil)
}
`,
	}
	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(fixture))
	for name, source := range fixture {
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = file
	}
	if diagnostics := auditC6ProductionBoundary(fset, files).diagnostics; len(diagnostics) != 0 {
		t.Fatalf("ticket transport or pointer-only types were rejected: %v", diagnostics)
	}
}

func TestC6ProductionBoundary(t *testing.T) {
	repositoryRoot, err := findC6RepositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files, err := loadC6ProductionFiles(fset, repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}

	audit := auditC6ProductionBoundary(fset, files)
	if audit.inspectedFiles == 0 {
		t.Fatal("C6 production boundary inspected no Go files")
	}
	if len(audit.diagnostics) != 0 {
		t.Fatalf("C6 production boundary violations:\n%s", strings.Join(audit.diagnostics, "\n"))
	}
	if !c6ConstructionCountsEqual(audit.allowedTicketConstructions, c6AllowedTicketConstructions) {
		t.Fatalf(
			"allowed ApplyTicket constructions = %v, want exact symbol/form allowlist %v",
			audit.allowedTicketConstructions,
			c6AllowedTicketConstructions,
		)
	}
}

func findC6RepositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		moduleFile := filepath.Join(directory, "go.mod")
		source, readErr := os.ReadFile(moduleFile)
		if readErr == nil {
			for line := range strings.SplitSeq(string(source), "\n") {
				if strings.TrimSpace(line) == "module "+c6ModulePath {
					return directory, nil
				}
			}
		} else if !os.IsNotExist(readErr) {
			return "", fmt.Errorf("read %s: %w", moduleFile, readErr)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return "", fmt.Errorf("locate repository root containing module %s", c6ModulePath)
}

func loadC6ProductionFiles(fset *token.FileSet, repositoryRoot string) (map[string]*ast.File, error) {
	files := make(map[string]*ast.File)
	for _, root := range c6ProductionRoots {
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
			source, err := os.ReadFile(filePath)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			file, err := parser.ParseFile(fset, relative, source, 0)
			if err != nil {
				return err
			}
			files[relative] = file
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan C6 production root %s: %w", root, err)
		}
	}
	return files, nil
}

type c6PackageFiles struct {
	directory string
	files     map[string]*ast.File
}

func auditC6ProductionBoundary(fset *token.FileSet, files map[string]*ast.File) c6BoundaryAudit {
	audit := c6BoundaryAudit{
		allowedTicketConstructions: make(map[c6TicketConstruction]int),
		inspectedFiles:             len(files),
	}
	packages := make(map[string]*c6PackageFiles)
	for filePath, file := range files {
		normalized := filepath.ToSlash(filepath.Clean(filePath))
		directory := filepath.ToSlash(filepath.Dir(normalized))
		key := directory + "\x00" + file.Name.Name
		if packages[key] == nil {
			packages[key] = &c6PackageFiles{directory: directory, files: make(map[string]*ast.File)}
		}
		packages[key].files[normalized] = file
	}

	keys := make([]string, 0, len(packages))
	for key := range packages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	boundaryImporter := newC6BoundaryImporter()
	for _, key := range keys {
		pkgFiles := packages[key]
		audit.diagnostics = append(
			audit.diagnostics,
			c6ForbiddenImportViolations(fset, pkgFiles)...,
		)
		c6AuditTypedPackage(fset, boundaryImporter, pkgFiles, &audit)
	}
	sort.Strings(audit.diagnostics)
	return audit
}

func c6ForbiddenImportViolations(fset *token.FileSet, pkgFiles *c6PackageFiles) []string {
	filePaths := make([]string, 0, len(pkgFiles.files))
	for filePath := range pkgFiles.files {
		filePaths = append(filePaths, filePath)
	}
	sort.Strings(filePaths)
	var diagnostics []string
	for _, filePath := range filePaths {
		file := pkgFiles.files[filePath]
		for _, declaration := range file.Imports {
			importPath, err := strconv.Unquote(declaration.Path.Value)
			if err != nil {
				continue
			}
			switch importPath {
			case c6CompilerPath, c6RuntimePath:
				diagnostics = append(diagnostics, fmt.Sprintf(
					"%s: forbidden pre-Task-9 production import %s",
					fset.Position(declaration.Pos()), importPath,
				))
			}
		}
	}
	return diagnostics
}

func c6AuditTypedPackage(
	fset *token.FileSet,
	boundaryImporter types.Importer,
	pkgFiles *c6PackageFiles,
	audit *c6BoundaryAudit,
) {
	filePaths := make([]string, 0, len(pkgFiles.files))
	parsed := make([]*ast.File, 0, len(pkgFiles.files))
	for filePath := range pkgFiles.files {
		filePaths = append(filePaths, filePath)
	}
	sort.Strings(filePaths)
	for _, filePath := range filePaths {
		parsed = append(parsed, pkgFiles.files[filePath])
	}
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	config := types.Config{
		Importer: boundaryImporter,
		Error:    func(error) {},
	}
	_, _ = config.Check(c6ModulePath+"/"+pkgFiles.directory, fset, parsed, info)

	for _, filePath := range filePaths {
		file := pkgFiles.files[filePath]
		if pkgFiles.directory != c6StoreDirectory {
			c6AuditForeignTicketTypeUses(fset, info, filePath, file, audit)
		}
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.GenDecl:
				c6AuditZeroValueDeclarations(fset, info, filePath, "<package>", declaration, audit)
				c6AuditConstructionExpressions(fset, info, filePath, "<package>", declaration, audit)
			case *ast.FuncDecl:
				function := c6FunctionSymbol(declaration)
				c6AuditNamedResults(fset, info, filePath, function, declaration.Type.Results, audit)
				c6AuditConstructionExpressions(fset, info, filePath, function, declaration.Body, audit)
			}
		}
	}
}

func c6AuditForeignTicketTypeUses(
	fset *token.FileSet,
	info *types.Info,
	filePath string,
	file *ast.File,
	audit *c6BoundaryAudit,
) {
	ast.Inspect(file, func(node ast.Node) bool {
		var expression ast.Expr
		switch node := node.(type) {
		case *ast.Ident:
			expression = node
		case *ast.SelectorExpr:
			expression = node
		default:
			return true
		}
		typeAndValue, ok := info.Types[expression]
		if !ok || !typeAndValue.IsType() || !isC6ApplyTicketValueType(typeAndValue.Type) {
			return true
		}
		audit.diagnostics = append(audit.diagnostics, fmt.Sprintf(
			"%s: forbidden generation.ApplyTicket authority outside pkg/store",
			fset.Position(expression.Pos()),
		))
		return true
	})
}

func c6AuditConstructionExpressions(
	fset *token.FileSet,
	info *types.Info,
	filePath string,
	function string,
	root ast.Node,
	audit *c6BoundaryAudit,
) {
	if root == nil {
		return
	}
	ast.Inspect(root, func(node ast.Node) bool {
		switch expression := node.(type) {
		case *ast.FuncLit:
			c6AuditNamedResults(
				fset, info, filePath, function+".<literal>", expression.Type.Results, audit,
			)
		case *ast.CompositeLit:
			if isC6ApplyTicketValueType(info.TypeOf(expression.Type)) {
				form := "composite literal"
				if len(expression.Elts) == 0 {
					form = "zero-value composite"
				}
				c6RecordTicketConstruction(fset, filePath, function, form, expression.Pos(), audit)
			}
		case *ast.CallExpr:
			if form, constructed := c6ApplyTicketConstructionCall(info, expression); constructed {
				c6RecordTicketConstruction(fset, filePath, function, form, expression.Pos(), audit)
			}
		case *ast.IndexExpr:
			c6AuditAggregateTicketExtraction(fset, info, filePath, function, expression, audit)
		case *ast.SelectorExpr:
			c6AuditAggregateTicketExtraction(fset, info, filePath, function, expression, audit)
		case *ast.DeclStmt:
			if declaration, ok := expression.Decl.(*ast.GenDecl); ok {
				c6AuditZeroValueDeclarations(fset, info, filePath, function, declaration, audit)
			}
		}
		return true
	})
}

func c6AuditAggregateTicketExtraction(
	fset *token.FileSet,
	info *types.Info,
	filePath string,
	function string,
	expression ast.Expr,
	audit *c6BoundaryAudit,
) {
	typeAndValue, ok := info.Types[expression]
	if !ok || !typeAndValue.IsValue() || !isC6ApplyTicketValueType(typeAndValue.Type) ||
		!c6ContainsZeroTicketAggregate(info, expression) {
		return
	}
	c6RecordTicketConstruction(
		fset, filePath, function, "aggregate extraction", expression.Pos(), audit,
	)
}

func c6ContainsZeroTicketAggregate(info *types.Info, root ast.Node) bool {
	found := false
	ast.Inspect(root, func(node ast.Node) bool {
		if found {
			return false
		}
		literal, ok := node.(*ast.CompositeLit)
		if !ok || isC6ApplyTicketValueType(info.TypeOf(literal.Type)) {
			return true
		}
		if c6CompositeLeavesTicketZero(info.TypeOf(literal.Type), literal) {
			found = true
			return false
		}
		return true
	})
	return found
}

func c6CompositeLeavesTicketZero(value types.Type, literal *ast.CompositeLit) bool {
	if value == nil {
		return false
	}
	value = types.Unalias(value)
	if named, ok := value.(*types.Named); ok {
		value = named.Underlying()
	}
	switch aggregate := value.(type) {
	case *types.Array:
		return c6TypeContainsApplyTicketValue(aggregate.Elem()) &&
			aggregate.Len() > int64(len(literal.Elts))
	case *types.Struct:
		keyed := len(literal.Elts) > 0
		if keyed {
			_, keyed = literal.Elts[0].(*ast.KeyValueExpr)
		}
		initialized := make(map[string]struct{}, len(literal.Elts))
		if keyed {
			for _, element := range literal.Elts {
				keyValue, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if name, ok := keyValue.Key.(*ast.Ident); ok {
					initialized[name.Name] = struct{}{}
				}
			}
		}
		for index := 0; index < aggregate.NumFields(); index++ {
			field := aggregate.Field(index)
			if !c6TypeContainsApplyTicketValue(field.Type()) {
				continue
			}
			if keyed {
				if _, ok := initialized[field.Name()]; !ok {
					return true
				}
				continue
			}
			if index >= len(literal.Elts) {
				return true
			}
		}
	}
	return false
}

func c6TypeContainsApplyTicketValue(value types.Type) bool {
	if value == nil {
		return false
	}
	value = types.Unalias(value)
	if isC6ApplyTicketValueType(value) {
		return true
	}
	if named, ok := value.(*types.Named); ok {
		value = named.Underlying()
	}
	switch value := value.(type) {
	case *types.Array:
		return value.Len() > 0 && c6TypeContainsApplyTicketValue(value.Elem())
	case *types.Struct:
		for field := range value.Fields() {
			if c6TypeContainsApplyTicketValue(field.Type()) {
				return true
			}
		}
	}
	return false
}

func c6AuditZeroValueDeclarations(
	fset *token.FileSet,
	info *types.Info,
	filePath string,
	function string,
	declaration *ast.GenDecl,
	audit *c6BoundaryAudit,
) {
	if declaration == nil || declaration.Tok != token.VAR {
		return
	}
	for _, specification := range declaration.Specs {
		value, ok := specification.(*ast.ValueSpec)
		if !ok || value.Type == nil || len(value.Values) != 0 ||
			!isC6ApplyTicketValueType(info.TypeOf(value.Type)) {
			continue
		}
		for _, name := range value.Names {
			c6RecordTicketConstruction(
				fset, filePath, function, "zero-value declaration", name.Pos(), audit,
			)
		}
	}
}

func c6AuditNamedResults(
	fset *token.FileSet,
	info *types.Info,
	filePath string,
	function string,
	results *ast.FieldList,
	audit *c6BoundaryAudit,
) {
	if results == nil {
		return
	}
	for _, result := range results.List {
		if len(result.Names) == 0 || !isC6ApplyTicketValueType(info.TypeOf(result.Type)) {
			continue
		}
		for _, name := range result.Names {
			c6RecordTicketConstruction(fset, filePath, function, "named result", name.Pos(), audit)
		}
	}
}

func c6ApplyTicketConstructionCall(info *types.Info, call *ast.CallExpr) (string, bool) {
	if info.Types[call.Fun].IsType() && isC6ApplyTicketValueType(info.TypeOf(call.Fun)) {
		return "conversion", true
	}
	identifier, ok := call.Fun.(*ast.Ident)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	builtin, ok := info.Uses[identifier].(*types.Builtin)
	return "new", ok && builtin.Name() == "new" &&
		isC6ApplyTicketValueType(info.TypeOf(call.Args[0]))
}

func c6RecordTicketConstruction(
	fset *token.FileSet,
	filePath string,
	function string,
	form string,
	position token.Pos,
	audit *c6BoundaryAudit,
) {
	construction := c6TicketConstruction{file: filePath, function: function, form: form}
	if audit.allowedTicketConstructions[construction] < c6AllowedTicketConstructions[construction] {
		audit.allowedTicketConstructions[construction]++
		return
	}
	audit.diagnostics = append(audit.diagnostics, fmt.Sprintf(
		"%s: forbidden generation.ApplyTicket construction in %s (%s)",
		fset.Position(position), function, form,
	))
}

func isC6ApplyTicketValueType(value types.Type) bool {
	if value == nil {
		return false
	}
	value = types.Unalias(value)
	named, ok := value.(*types.Named)
	return ok && named.Obj().Name() == "ApplyTicket" &&
		named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == c6GenerationPath
}

func c6FunctionSymbol(declaration *ast.FuncDecl) string {
	if declaration.Recv == nil || len(declaration.Recv.List) == 0 {
		return declaration.Name.Name
	}
	return "(" + c6ReceiverType(declaration.Recv.List[0].Type) + ")." + declaration.Name.Name
}

func c6ReceiverType(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.StarExpr:
		return "*" + c6ReceiverType(expression.X)
	case *ast.IndexExpr:
		return c6ReceiverType(expression.X)
	case *ast.IndexListExpr:
		return c6ReceiverType(expression.X)
	default:
		return "<unknown>"
	}
}

func c6ConstructionCountsEqual(
	left map[c6TicketConstruction]int,
	right map[c6TicketConstruction]int,
) bool {
	if len(left) != len(right) {
		return false
	}
	for construction, count := range left {
		if right[construction] != count {
			return false
		}
	}
	return true
}

type c6BoundaryImporter struct {
	standard types.Importer
	packages map[string]*types.Package
}

func newC6BoundaryImporter() *c6BoundaryImporter {
	boundaryImporter := &c6BoundaryImporter{
		standard: importer.Default(),
		packages: make(map[string]*types.Package),
	}
	boundaryImporter.packages[c6GenerationPath] = c6GenerationTypesPackage()
	return boundaryImporter
}

func (boundaryImporter *c6BoundaryImporter) Import(importPath string) (*types.Package, error) {
	if imported := boundaryImporter.packages[importPath]; imported != nil {
		return imported, nil
	}
	if imported, err := boundaryImporter.standard.Import(importPath); err == nil {
		boundaryImporter.packages[importPath] = imported
		return imported, nil
	}
	name := filepath.Base(importPath)
	imported := types.NewPackage(importPath, name)
	imported.MarkComplete()
	boundaryImporter.packages[importPath] = imported
	return imported, nil
}

func c6GenerationTypesPackage() *types.Package {
	imported := types.NewPackage(c6GenerationPath, "generation")
	object := types.NewTypeName(token.NoPos, imported, "ApplyTicket", nil)
	types.NewNamed(object, types.NewStruct(nil, nil), nil)
	imported.Scope().Insert(object)
	imported.MarkComplete()
	return imported
}
