package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTypedWaitGroupGoSelectionsCoversReceiverForms(t *testing.T) {
	tests := map[string]string{
		"pointer variable":            `package fixture; import "sync"; func f() { var wg *sync.WaitGroup; wg.Go(func(){}) }`,
		"type alias":                  `package fixture; import "sync"; type WG = sync.WaitGroup; func f() { var wg WG; wg.Go(func(){}) }`,
		"defined embedding":           `package fixture; import "sync"; type WG struct { sync.WaitGroup }; func f() { var wg WG; wg.Go(func(){}) }`,
		"anonymous pointer embedding": `package fixture; import "sync"; type WG struct { *sync.WaitGroup }; func f(wg WG) { wg.Go(func(){}) }`,
		"new expression":              `package fixture; import "sync"; func f() { new(sync.WaitGroup).Go(func(){}) }`,
		"import alias":                `package fixture; import s "sync"; func f() { var wg s.WaitGroup; wg.Go(func(){}) }`,
		"dot import":                  `package fixture; import . "sync"; func f() { var wg WaitGroup; wg.Go(func(){}) }`,
		"paren star selector":         `package fixture; import "sync"; type holder struct { wg *sync.WaitGroup }; func f(h holder) { ((*h.wg)).Go(func(){}) }`,
		"method value":                `package fixture; import "sync"; func f() { var wg sync.WaitGroup; start := wg.Go; start(func(){}) }`,
		"method expression":           `package fixture; import "sync"; func f() { var wg sync.WaitGroup; (*sync.WaitGroup).Go(&wg, func(){}) }`,
	}

	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			files, info := typeCheckWaitGroupFixture(t, source)
			if got := len(typedWaitGroupGoSelections(files, info)); got != 1 {
				t.Fatalf("typedWaitGroupGoSelections() = %d, want 1", got)
			}
		})
	}
}

func TestTypedWaitGroupGoSelectionsDoesNotFlagOtherGoMethods(t *testing.T) {
	const source = `package fixture; type owner struct{}; func (*owner) Go(func()) {}; func f() { new(owner).Go(func(){}) }`
	files, info := typeCheckWaitGroupFixture(t, source)
	if got := len(typedWaitGroupGoSelections(files, info)); got != 0 {
		t.Fatalf("typedWaitGroupGoSelections() = %d, want 0", got)
	}
}

func typedWaitGroupGoSelections(files []*ast.File, info *types.Info) []token.Pos {
	var positions []token.Pos
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Go" {
				return true
			}
			selection := info.Selections[selector]
			if selection == nil {
				return true
			}
			method, ok := selection.Obj().(*types.Func)
			if !ok || !isSyncWaitGroupGoMethod(method) {
				return true
			}
			positions = append(positions, selector.Sel.Pos())
			return true
		})
	}
	return positions
}

func isSyncWaitGroupGoMethod(method *types.Func) bool {
	if method.Name() != "Go" || method.Pkg() == nil || method.Pkg().Path() != "sync" {
		return false
	}
	signature, ok := method.Type().(*types.Signature)
	if !ok || signature.Recv() == nil {
		return false
	}
	receiver := signature.Recv().Type()
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = pointer.Elem()
	}
	named, ok := receiver.(*types.Named)
	return ok && named.Obj().Pkg() != nil &&
		named.Obj().Pkg().Path() == "sync" && named.Obj().Name() == "WaitGroup"
}

func typeCheckWaitGroupFixture(t *testing.T, source string) ([]*ast.File, *types.Info) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	configuration := types.Config{Importer: importer.Default()}
	if _, err := configuration.Check("fixture", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("type-check fixture: %v", err)
	}
	return []*ast.File{file}, info
}

type taskContractGoListPackage struct {
	Dir        string
	ImportPath string
	Export     string
	GoFiles    []string
	CgoFiles   []string
	DepOnly    bool
}

type taskContractTypedPackage struct {
	importPath string
	files      []*ast.File
	info       *types.Info
}

func loadTaskContractPackages(t *testing.T, root string) ([]taskContractTypedPackage, *token.FileSet) {
	t.Helper()
	root = filepath.Clean(root)
	command := exec.Command(
		"go",
		"list",
		"-deps",
		"-export",
		"-json",
		"./pkg/plugin/...",
		"./pkg/proxy/...",
		"./pkg/route/...",
		"./pkg/stream/...",
	)
	command.Dir = root
	command.Env = taskContractGoEnvironment()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("go list failed: %v\nstderr:\n%s", err, stderr.String())
	}

	var listed []taskContractGoListPackage
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var record taskContractGoListPackage
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		listed = append(listed, record)
	}

	exports := make(map[string]string, len(listed))
	for _, record := range listed {
		if record.ImportPath != "" && record.Export != "" {
			exports[record.ImportPath] = record.Export
		}
	}

	fileSet := token.NewFileSet()
	packages := make([]taskContractTypedPackage, 0)
	seenPackages := make(map[string]struct{})
	loadedFiles := make(map[string]struct{})
	for _, record := range listed {
		if record.DepOnly || !taskContractPathUnderScannedRoot(root, record.Dir) {
			continue
		}
		if record.ImportPath == "" {
			t.Fatalf("selected package has no import path: dir=%q", record.Dir)
		}
		if _, seen := seenPackages[record.ImportPath]; seen {
			continue
		}
		seenPackages[record.ImportPath] = struct{}{}

		packageDirectory := taskContractAbsolutePath(root, record.Dir)
		files := make([]*ast.File, 0, len(record.GoFiles)+len(record.CgoFiles))
		for _, filename := range append(append([]string(nil), record.GoFiles...), record.CgoFiles...) {
			path := filepath.Clean(filepath.Join(packageDirectory, filename))
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read production file %s: %v", taskContractRelative(root, path), err)
			}
			file, err := parser.ParseFile(fileSet, path, source, parser.AllErrors)
			if err != nil {
				t.Fatalf("parse production file %s: %v", taskContractRelative(root, path), err)
			}
			files = append(files, file)
			loadedFiles[path] = struct{}{}
		}
		packages = append(packages, taskContractTypedPackage{
			importPath: record.ImportPath,
			files:      files,
		})
	}

	walkedFiles := taskContractProductionFiles(t, root)
	for path := range loadedFiles {
		if _, walked := walkedFiles[path]; !walked {
			t.Fatalf("typed production file was not walked: %s", taskContractRelative(root, path))
		}
	}
	for path := range walkedFiles {
		if _, loaded := loadedFiles[path]; !loaded {
			t.Fatalf("walked production file was not type-loaded: %s", taskContractRelative(root, path))
		}
	}

	lookup := func(importPath string) (io.ReadCloser, error) {
		export, ok := exports[importPath]
		if !ok || export == "" {
			return nil, fmt.Errorf("no export data for %q", importPath)
		}
		return os.Open(export)
	}
	compiledImporter := importer.ForCompiler(fileSet, "gc", lookup)
	for index := range packages {
		info := &types.Info{
			Types:      make(map[ast.Expr]types.TypeAndValue),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
		}
		configuration := types.Config{Importer: compiledImporter}
		if _, err := configuration.Check(packages[index].importPath, fileSet, packages[index].files, info); err != nil {
			t.Fatalf("type-check production package %s: %v", packages[index].importPath, err)
		}
		packages[index].info = info
	}
	return packages, fileSet
}

func taskContractProductionFiles(t *testing.T, root string) map[string]struct{} {
	t.Helper()
	files := make(map[string]struct{})
	for _, scannedRoot := range taskContractScannedRoots(root) {
		if err := filepath.WalkDir(scannedRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
				files[filepath.Clean(path)] = struct{}{}
			}
			return nil
		}); err != nil {
			t.Fatalf("walk production root %s: %v", taskContractRelative(root, scannedRoot), err)
		}
	}
	return files
}

func taskContractGoEnvironment() []string {
	environment := os.Environ()
	result := make([]string, 0, len(environment)+1)
	for _, value := range environment {
		if strings.HasPrefix(value, "GOFLAGS=") {
			continue
		}
		result = append(result, value)
	}
	return append(result, "GOFLAGS=-mod=readonly")
}

func taskContractRepositoryRoot(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	directory, err := filepath.Abs(workingDirectory)
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	for {
		candidate := filepath.Join(directory, "go.mod")
		if information, err := os.Stat(candidate); err == nil && !information.IsDir() {
			return filepath.Clean(directory)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("could not find go.mod above %s", workingDirectory)
		}
		directory = parent
	}
}

func taskContractRelative(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func taskContractScannedRoots(root string) []string {
	return []string{
		filepath.Join(root, "pkg", "plugin"),
		filepath.Join(root, "pkg", "proxy"),
		filepath.Join(root, "pkg", "route"),
		filepath.Join(root, "pkg", "stream"),
	}
}

func taskContractAbsolutePath(root, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return filepath.Clean(path)
}

func taskContractPathUnderScannedRoot(root, path string) bool {
	path = taskContractAbsolutePath(root, path)
	for _, scannedRoot := range taskContractScannedRoots(root) {
		relative, err := filepath.Rel(scannedRoot, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		return true
	}
	return false
}

func TestProductionGoroutinesUseOwnedRuntime(t *testing.T) {
	root := taskContractRepositoryRoot(t)
	packages, fileSet := loadTaskContractPackages(t, root)
	for _, loaded := range packages {
		for _, file := range loaded.files {
			ast.Inspect(file, func(node ast.Node) bool {
				statement, ok := node.(*ast.GoStmt)
				if !ok {
					return true
				}
				position := fileSet.Position(statement.Go)
				t.Errorf("unowned go statement: %s:%d", taskContractRelative(root, position.Filename), position.Line)
				return true
			})
			for _, position := range typedWaitGroupGoSelections([]*ast.File{file}, loaded.info) {
				location := fileSet.Position(position)
				t.Errorf(
					"unowned sync.WaitGroup.Go: %s:%d",
					taskContractRelative(root, location.Filename),
					location.Line,
				)
			}
		}
	}
}
