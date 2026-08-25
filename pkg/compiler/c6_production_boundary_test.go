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
	"strings"
	"testing"
)

const (
	c6ModulePath     = "github.com/wklken/apisix-go"
	c6GenerationPath = c6ModulePath + "/pkg/generation"

	c6ApplyTicketOwner = "pkg/store/journal_apply.go"
)

var c6ProductionRoots = []string{
	"cmd",
	"pkg",
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
	{c6ApplyTicketOwner, "(*Store).ApplyDesired", "zero-value composite"}:                         3,
	{c6ApplyTicketOwner, "(*Store).ApplyDesired", "composite literal"}:                            1,
	{c6ApplyTicketOwner, "(*Store).ApplyDesired", "zero-value declaration"}:                       1,
	{c6ApplyTicketOwner, "loadCursorTx", "zero-value composite"}:                                  3,
	{c6ApplyTicketOwner, "applyTicketFromWire", "composite literal"}:                              1,
	{c6ApplyTicketOwner, "decodeCursorRecord", "aggregate zero-value composite"}:                  10,
	{c6ApplyTicketOwner, "loadCursorRecordForTicketTx", "aggregate zero-value composite"}:         4,
	{"pkg/store/journal_publish.go", "loadStagedPublicationTx", "aggregate zero-value composite"}: 3,
	{"pkg/store/journal_publish.go", "decodeStagedPublication", "aggregate zero-value composite"}: 7,
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
		"pkg/route/ticket_elided_aggregate.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func forgeFromElidedAggregate() gen.ApplyTicket { return [1]gen.ApplyTicket{{}}[0] }
`,
		"pkg/route/ticket_zero_aggregate.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func forgeFromZeroAggregate() gen.ApplyTicket {
	var tickets [1]gen.ApplyTicket
	return tickets[0]
}
`,
		"pkg/route/ticket_zero_holder.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
type ticketHolder struct { Ticket gen.ApplyTicket }
func forgeFromZeroHolder() gen.ApplyTicket {
	var holder ticketHolder
	return holder.Ticket
}
`,
		"pkg/route/ticket_map_extraction.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
type mappedTicketHolder struct { Ticket gen.ApplyTicket }
func (holder mappedTicketHolder) ticket() gen.ApplyTicket { return holder.Ticket }
func forgeDirectFromMap(tickets map[string]gen.ApplyTicket) gen.ApplyTicket { return tickets["missing"] }
func forgeHolderFieldFromMap(holders map[string]mappedTicketHolder) gen.ApplyTicket { return holders["missing"].Ticket }
func forgeHolderMethodFromMap(holders map[string]mappedTicketHolder) gen.ApplyTicket { return holders["missing"].ticket() }
func forgeGenericFromMap[M ~map[string]mappedTicketHolder](holders M) gen.ApplyTicket { return holders["missing"].ticket() }
`,
		"pkg/route/ticket_channel_receive.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
type receivedTicketHolder struct { Ticket gen.ApplyTicket }
func (holder receivedTicketHolder) ticket() gen.ApplyTicket { return holder.Ticket }
func forgeFromClosedChannel(tickets <-chan gen.ApplyTicket) gen.ApplyTicket { return <-tickets }
func forgeHolderFromClosedChannel(tickets <-chan receivedTicketHolder) gen.ApplyTicket { return (<-tickets).ticket() }
func forgeGenericFromClosedChannel[C ~<-chan receivedTicketHolder](tickets C) gen.ApplyTicket { return (<-tickets).ticket() }
`,
		"pkg/route/ticket_generic.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func zero[T any]() (value T) { return }
func forgeFromGeneric() gen.ApplyTicket { return zero[gen.ApplyTicket]() }
`,
		"pkg/route/ticket_inferred_generic.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func zeroLike[T any](_ T) (value T) { return }
func forgeFromInferredGeneric(ticket gen.ApplyTicket) gen.ApplyTicket { return zeroLike(ticket) }
`,
		"pkg/route/ticket_inferred_generic_pointer.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func zeroPointer[T any](_ T) *T { return new(T) }
func forgeFromInferredPointer(ticket gen.ApplyTicket) gen.ApplyTicket { return *zeroPointer(ticket) }
`,
		"pkg/route/ticket_inferred_generic_slice.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func zeroSlice[T any](_ T) []T { return make([]T, 1) }
func forgeFromInferredSlice(ticket gen.ApplyTicket) gen.ApplyTicket { return zeroSlice(ticket)[0] }
`,
		"pkg/route/ticket_inferred_generic_transport.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
func identity[T any](value T) T { return value }
func genericTransport(ticket gen.ApplyTicket) gen.ApplyTicket { return identity(ticket) }
`,
		"pkg/route/ticket_generic_type.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
type ticketBox[T any] struct { Value T }
func genericTypeTransport(box ticketBox[gen.ApplyTicket]) gen.ApplyTicket { return box.Value }
`,
		"pkg/helper/generic.go": `package helper
func Identity[T any](value T) T { return value }
`,
		"pkg/server/ticket_cross_package_generic.go": `package server
import gen "github.com/wklken/apisix-go/pkg/generation"
import "github.com/wklken/apisix-go/pkg/helper"
func crossPackageGeneric(ticket gen.ApplyTicket) gen.ApplyTicket { return helper.Identity(ticket) }
`,
		"pkg/route/ticket_generic_containers.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
type containerBox[T any] struct { Value T }
type genericContainers struct {
	Slice containerBox[[]gen.ApplyTicket]
	Map containerBox[map[string]gen.ApplyTicket]
	Channel containerBox[chan gen.ApplyTicket]
	Function containerBox[func() gen.ApplyTicket]
}
`,
		"pkg/route/ticket_generic_interface.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
type ticketSource interface { Next() gen.ApplyTicket }
type sourceBox[T any] struct { Value T }
type genericInterface struct { Source sourceBox[ticketSource] }
type nestedSourceBox[T ticketSource] struct { Value sourceBox[T] }
`,
		"pkg/route/ticket_holder_construction.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
type constructedTicketHolder struct { Ticket gen.ApplyTicket }
func (holder constructedTicketHolder) ticket() gen.ApplyTicket { return holder.Ticket }
func forgeFromHolderLiteral() gen.ApplyTicket { holder := constructedTicketHolder{}; return holder.Ticket }
func forgeFromHolderNew() gen.ApplyTicket { return new(constructedTicketHolder).Ticket }
func forgeFromHolderMethod() gen.ApplyTicket { return constructedTicketHolder{}.ticket() }
`,
		"pkg/route/ticket_alias.go": `package route
import gen "github.com/wklken/apisix-go/pkg/generation"
type TicketAlias = gen.ApplyTicket
`,
		"pkg/route/ticket_dot_import.go": `package route
import . "github.com/wklken/apisix-go/pkg/generation"
type DotTicketAlias = ApplyTicket
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
		"pkg/route/ticket_aggregate.go",
		"pkg/route/ticket_elided_aggregate.go",
		"pkg/route/ticket_zero_aggregate.go", "aggregate zero-value declaration",
		"pkg/route/ticket_zero_holder.go",
		"pkg/route/ticket_map_extraction.go", "map zero-capable extraction",
		"forgeDirectFromMap", "forgeHolderFieldFromMap", "forgeHolderMethodFromMap", "forgeGenericFromMap",
		"pkg/route/ticket_channel_receive.go", "channel zero-capable receive",
		"forgeFromClosedChannel", "forgeHolderFromClosedChannel", "forgeGenericFromClosedChannel",
		"pkg/route/ticket_generic.go", "ApplyTicket authority type use",
		"pkg/route/ticket_inferred_generic.go", "generic type argument",
		"pkg/route/ticket_inferred_generic_pointer.go",
		"pkg/route/ticket_inferred_generic_slice.go",
		"pkg/route/ticket_inferred_generic_transport.go",
		"pkg/route/ticket_generic_type.go",
		"pkg/server/ticket_cross_package_generic.go",
		"pkg/route/ticket_generic_containers.go",
		"pkg/route/ticket_generic_interface.go",
		"pkg/route/ticket_holder_construction.go", "aggregate zero-value composite", "aggregate new",
		"pkg/route/ticket_alias.go",
		"pkg/route/ticket_dot_import.go",
		"pkg/store/journal_apply.go", "forgedInJournalFile",
		"pkg/store/ticket_aggregate.go", "aggregate extraction",
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
func identityPointer[T any](value T) T { return value }
type pointerEnvelope[T any] struct { Value T }
type pointerTicketSource interface { Next() *gen.ApplyTicket }
func pointerTypes() {
	_ = new(*gen.ApplyTicket)
	_ = (*gen.ApplyTicket)(nil)
	_ = identityPointer((*gen.ApplyTicket)(nil))
	_ = pointerEnvelope[*gen.ApplyTicket]{}
	_ = pointerEnvelope[pointerTicketSource]{}
}
`,
		"pkg/server/transport.go": `package server
import gen "github.com/wklken/apisix-go/pkg/generation"
type envelope struct { Ticket gen.ApplyTicket }
func transport(ticket gen.ApplyTicket) gen.ApplyTicket { return ticket }
`,
		"pkg/server/compiler_import.go": `package server
import build "github.com/wklken/apisix-go/pkg/compiler"
var factory *build.WorkerCompilerFactory
`,
		"pkg/stream/runtime_import.go": `package stream
import lifecycle "github.com/wklken/apisix-go/pkg/runtime"
var registry *lifecycle.ResourceRegistry
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
	boundaryImporter := newC6BoundaryImporter(fset, packages)
	for _, key := range keys {
		pkgFiles := packages[key]
		c6AuditTypedPackage(fset, boundaryImporter.check(pkgFiles), pkgFiles, &audit)
	}
	sort.Strings(audit.diagnostics)
	return audit
}

func c6AuditTypedPackage(
	fset *token.FileSet,
	info *types.Info,
	pkgFiles *c6PackageFiles,
	audit *c6BoundaryAudit,
) {
	filePaths := make([]string, 0, len(pkgFiles.files))
	for filePath := range pkgFiles.files {
		filePaths = append(filePaths, filePath)
	}
	sort.Strings(filePaths)

	for _, filePath := range filePaths {
		file := pkgFiles.files[filePath]
		c6AuditTicketTypeAuthority(fset, info, filePath, file, audit)
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

func c6AuditTicketTypeAuthority(
	fset *token.FileSet,
	info *types.Info,
	filePath string,
	file *ast.File,
	audit *c6BoundaryAudit,
) {
	parents := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})

	ast.Inspect(file, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok {
			if instance, instantiated := info.Instances[identifier]; instantiated &&
				c6InstanceUsesTicketValue(instance) {
				audit.diagnostics = append(audit.diagnostics, fmt.Sprintf(
					"%s: forbidden generation.ApplyTicket generic type argument",
					fset.Position(identifier.Pos()),
				))
			}
		}
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
		if !ok || !typeAndValue.IsType() || !isC6ApplyTicketValueType(typeAndValue.Type) ||
			c6TicketTypeUseIsTransport(expression, parents, info) {
			return true
		}
		audit.diagnostics = append(audit.diagnostics, fmt.Sprintf(
			"%s: forbidden generation.ApplyTicket authority type use",
			fset.Position(expression.Pos()),
		))
		return true
	})
}

func c6TicketTypeUseIsTransport(
	expression ast.Expr,
	parents map[ast.Node]ast.Node,
	info *types.Info,
) bool {
	for node := ast.Node(expression); node != nil; node = parents[node] {
		switch parent := parents[node].(type) {
		case *ast.StarExpr:
			return true
		case *ast.Field:
			fieldList, ok := parents[parent].(*ast.FieldList)
			if !ok {
				continue
			}
			switch owner := parents[fieldList].(type) {
			case *ast.StructType:
				return owner.Fields == fieldList
			case *ast.FuncType:
				return owner.Params == fieldList || owner.Results == fieldList
			}
		case *ast.CompositeLit:
			if isC6ApplyTicketValueType(info.TypeOf(parent)) {
				return true
			}
		case *ast.CallExpr:
			if _, constructed := c6ApplyTicketConstructionCall(info, parent); constructed {
				return true
			}
		case *ast.ValueSpec:
			if len(parent.Values) == 0 && isC6ApplyTicketValueType(info.TypeOf(parent.Type)) {
				return true
			}
		}
	}
	return false
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
			valueType := info.TypeOf(expression)
			if isC6ApplyTicketValueType(valueType) {
				form := "composite literal"
				if len(expression.Elts) == 0 {
					form = "zero-value composite"
				}
				c6RecordTicketConstruction(fset, filePath, function, form, expression.Pos(), audit)
			} else if c6TypeContainsApplyTicketValue(valueType) &&
				c6CompositeLeavesTicketZero(valueType, expression) {
				c6RecordTicketConstruction(
					fset, filePath, function, "aggregate zero-value composite", expression.Pos(), audit,
				)
			}
		case *ast.CallExpr:
			if form, constructed := c6ApplyTicketConstructionCall(info, expression); constructed {
				c6RecordTicketConstruction(fset, filePath, function, form, expression.Pos(), audit)
			}
		case *ast.IndexExpr:
			if c6MapIndexElementContainsTicketValue(info, expression) {
				c6RecordTicketConstruction(
					fset, filePath, function, "map zero-capable extraction", expression.Pos(), audit,
				)
			}
			c6AuditAggregateTicketExtraction(fset, info, filePath, function, expression, audit)
		case *ast.SelectorExpr:
			c6AuditAggregateTicketExtraction(fset, info, filePath, function, expression, audit)
		case *ast.UnaryExpr:
			if expression.Op == token.ARROW &&
				c6ChannelElementContainsTicketValue(info.TypeOf(expression.X)) {
				c6RecordTicketConstruction(
					fset, filePath, function, "channel zero-capable receive", expression.Pos(), audit,
				)
			}
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
	if !ok || !typeAndValue.IsValue() || !isC6ApplyTicketValueType(typeAndValue.Type) {
		return
	}
	if !c6ContainsZeroTicketAggregate(info, expression) {
		return
	}
	c6RecordTicketConstruction(
		fset, filePath, function, "aggregate extraction", expression.Pos(), audit,
	)
}

func c6MapIndexElementContainsTicketValue(info *types.Info, expression *ast.IndexExpr) bool {
	containerType := info.TypeOf(expression.X)
	return c6CoreTypeMatches(containerType, func(core types.Type) bool {
		mapType, ok := types.Unalias(core).Underlying().(*types.Map)
		return ok && c6TypeContainsApplyTicketValue(mapType.Elem())
	})
}

func c6ChannelElementContainsTicketValue(value types.Type) bool {
	return c6CoreTypeMatches(value, func(core types.Type) bool {
		channel, ok := types.Unalias(core).Underlying().(*types.Chan)
		return ok && c6TypeContainsApplyTicketValue(channel.Elem())
	})
}

func c6CoreTypeMatches(value types.Type, match func(types.Type) bool) bool {
	return c6VisitCoreTypes(value, make(map[types.Type]struct{}), match)
}

func c6VisitCoreTypes(
	value types.Type,
	seen map[types.Type]struct{},
	visit func(types.Type) bool,
) bool {
	if value == nil {
		return false
	}
	value = types.Unalias(value)
	if _, visited := seen[value]; visited {
		return false
	}
	seen[value] = struct{}{}

	switch value := value.(type) {
	case *types.TypeParam:
		return c6VisitCoreTypes(value.Constraint(), seen, visit)
	case *types.Interface:
		for embedded := range value.EmbeddedTypes() {
			if c6VisitCoreTypes(embedded, seen, visit) {
				return true
			}
		}
		return false
	case *types.Union:
		for term := range value.Terms() {
			if c6VisitCoreTypes(term.Type(), seen, visit) {
				return true
			}
		}
		return false
	case *types.Named:
		return c6VisitCoreTypes(value.Underlying(), seen, visit)
	default:
		return visit(value)
	}
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
	case *types.Tuple:
		for variable := range value.Variables() {
			if c6TypeContainsApplyTicketValue(variable.Type()) {
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
			!c6TypeContainsApplyTicketValue(info.TypeOf(value.Type)) {
			continue
		}
		form := "aggregate zero-value declaration"
		if isC6ApplyTicketValueType(info.TypeOf(value.Type)) {
			form = "zero-value declaration"
		}
		for _, name := range value.Names {
			c6RecordTicketConstruction(
				fset, filePath, function, form, name.Pos(), audit,
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
		if len(result.Names) == 0 || !c6TypeContainsApplyTicketValue(info.TypeOf(result.Type)) {
			continue
		}
		form := "aggregate named result"
		if isC6ApplyTicketValueType(info.TypeOf(result.Type)) {
			form = "named result"
		}
		for _, name := range result.Names {
			c6RecordTicketConstruction(fset, filePath, function, form, name.Pos(), audit)
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
	if !ok || builtin.Name() != "new" {
		return "", false
	}
	argumentType := info.TypeOf(call.Args[0])
	if isC6ApplyTicketValueType(argumentType) {
		return "new", true
	}
	return "aggregate new", c6TypeContainsApplyTicketValue(argumentType)
}

func c6InstanceUsesTicketValue(instance types.Instance) bool {
	for argument := range instance.TypeArgs.Types() {
		if c6GenericTypeArgumentContainsTicket(argument, make(map[types.Type]struct{})) {
			return true
		}
	}
	return false
}

func c6GenericTypeArgumentContainsTicket(value types.Type, seen map[types.Type]struct{}) bool {
	if value == nil {
		return false
	}
	value = types.Unalias(value)
	if isC6ApplyTicketValueType(value) {
		return true
	}
	if _, visited := seen[value]; visited {
		return false
	}
	seen[value] = struct{}{}

	switch value := value.(type) {
	case *types.Named:
		for argument := range value.TypeArgs().Types() {
			if c6GenericTypeArgumentContainsTicket(argument, seen) {
				return true
			}
		}
		return c6GenericTypeArgumentContainsTicket(value.Underlying(), seen)
	case *types.Pointer:
		// A pointer is transport-only at the generic boundary. Do not walk the
		// pointed-to object graph and mistake ownership for value construction.
		return false
	case *types.Array:
		return value.Len() > 0 && c6GenericTypeArgumentContainsTicket(value.Elem(), seen)
	case *types.Slice:
		return c6GenericTypeArgumentContainsTicket(value.Elem(), seen)
	case *types.Map:
		return c6GenericTypeArgumentContainsTicket(value.Key(), seen) ||
			c6GenericTypeArgumentContainsTicket(value.Elem(), seen)
	case *types.Chan:
		return c6GenericTypeArgumentContainsTicket(value.Elem(), seen)
	case *types.Struct:
		for field := range value.Fields() {
			if c6GenericTypeArgumentContainsTicket(field.Type(), seen) {
				return true
			}
		}
	case *types.Tuple:
		for variable := range value.Variables() {
			if c6GenericTypeArgumentContainsTicket(variable.Type(), seen) {
				return true
			}
		}
	case *types.Signature:
		return c6GenericTypeArgumentContainsTicket(value.Params(), seen) ||
			c6GenericTypeArgumentContainsTicket(value.Results(), seen)
	case *types.Interface:
		for method := range value.ExplicitMethods() {
			if c6GenericTypeArgumentContainsTicket(method.Type(), seen) {
				return true
			}
		}
		for embedded := range value.EmbeddedTypes() {
			if c6GenericTypeArgumentContainsTicket(embedded, seen) {
				return true
			}
		}
	case *types.TypeParam:
		return c6GenericTypeArgumentContainsTicket(value.Constraint(), seen)
	case *types.Union:
		for term := range value.Terms() {
			if c6GenericTypeArgumentContainsTicket(term.Type(), seen) {
				return true
			}
		}
	}
	return false
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
	fset          *token.FileSet
	standard      types.Importer
	localPackages map[string]*c6PackageFiles
	packages      map[string]*types.Package
	infos         map[string]*types.Info
	checking      map[string]bool
}

func newC6BoundaryImporter(
	fset *token.FileSet,
	packageFiles map[string]*c6PackageFiles,
) *c6BoundaryImporter {
	boundaryImporter := &c6BoundaryImporter{
		fset:          fset,
		standard:      importer.Default(),
		localPackages: make(map[string]*c6PackageFiles),
		packages:      make(map[string]*types.Package),
		infos:         make(map[string]*types.Info),
		checking:      make(map[string]bool),
	}
	for _, files := range packageFiles {
		boundaryImporter.localPackages[c6ModulePath+"/"+files.directory] = files
	}
	boundaryImporter.packages[c6GenerationPath] = c6GenerationTypesPackage()
	return boundaryImporter
}

func (boundaryImporter *c6BoundaryImporter) check(pkgFiles *c6PackageFiles) *types.Info {
	importPath := c6ModulePath + "/" + pkgFiles.directory
	if checked := boundaryImporter.infos[importPath]; checked != nil {
		return checked
	}
	info := &types.Info{
		Types:     make(map[ast.Expr]types.TypeAndValue),
		Uses:      make(map[*ast.Ident]types.Object),
		Instances: make(map[*ast.Ident]types.Instance),
	}
	boundaryImporter.infos[importPath] = info
	if boundaryImporter.checking[importPath] {
		return info
	}
	boundaryImporter.checking[importPath] = true
	defer delete(boundaryImporter.checking, importPath)

	filePaths := make([]string, 0, len(pkgFiles.files))
	for filePath := range pkgFiles.files {
		filePaths = append(filePaths, filePath)
	}
	sort.Strings(filePaths)
	parsed := make([]*ast.File, 0, len(filePaths))
	for _, filePath := range filePaths {
		parsed = append(parsed, pkgFiles.files[filePath])
	}
	config := types.Config{Importer: boundaryImporter, Error: func(error) {}}
	checked, _ := config.Check(importPath, boundaryImporter.fset, parsed, info)
	if importPath != c6GenerationPath && checked != nil {
		boundaryImporter.packages[importPath] = checked
	}
	return info
}

func (boundaryImporter *c6BoundaryImporter) Import(importPath string) (*types.Package, error) {
	if imported := boundaryImporter.packages[importPath]; imported != nil {
		return imported, nil
	}
	if local := boundaryImporter.localPackages[importPath]; local != nil {
		boundaryImporter.check(local)
		if imported := boundaryImporter.packages[importPath]; imported != nil {
			return imported, nil
		}
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
