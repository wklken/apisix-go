package plugin

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/consumer"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestSchemaWitnessRejectsUnknownFactory(t *testing.T) {
	_, err := SchemaWitnessForFactory("unknown")
	if err == nil || !strings.Contains(err.Error(), `factory "unknown" is not registered`) {
		t.Fatalf("SchemaWitnessForFactory(unknown) error = %v", err)
	}
}

func TestSchemaWitnessCallsInitExactlyOnce(t *testing.T) {
	instance := &schemaWitnessSentinel{}
	witness, err := schemaWitnessFromFactory("schema-sentinel", func() Plugin { return instance })
	if err != nil {
		t.Fatal(err)
	}
	if instance.initCalls != 1 {
		t.Fatalf("Init calls = %d, want 1", instance.initCalls)
	}
	if witness != (SchemaWitness{
		Factory:  "schema-sentinel",
		Config:   `{"type":"object"}`,
		Metadata: `{"type":"object","title":"metadata"}`,
	}) {
		t.Fatalf("witness = %#v", witness)
	}
}

func TestSchemaWitnessRejectsInvalidFactoryResults(t *testing.T) {
	if _, err := schemaWitnessFromFactory("nil-factory", nil); err == nil ||
		!strings.Contains(err.Error(), `factory "nil-factory" is nil`) {
		t.Fatalf("nil factory error = %v", err)
	}
	if _, err := schemaWitnessFromFactory("nil-instance", func() Plugin { return nil }); err == nil ||
		!strings.Contains(err.Error(), `factory "nil-instance" returned nil`) {
		t.Fatalf("nil instance error = %v", err)
	}

	want := errors.New("init failed")
	instance := &schemaWitnessSentinel{initErr: want}
	_, err := schemaWitnessFromFactory("init-error", func() Plugin { return instance })
	if !errors.Is(err, want) || instance.initCalls != 1 {
		t.Fatalf("Init error = %v, calls = %d", err, instance.initCalls)
	}
}

func TestSchemaWitnessCoversGeneratedFactories(t *testing.T) {
	keys := make([]string, 0, len(pluginRegistry))
	for key := range pluginRegistry {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		witness, err := SchemaWitnessForFactory(key)
		if err != nil {
			t.Fatalf("SchemaWitnessForFactory(%q): %v", key, err)
		}
		if witness.Factory != key {
			t.Fatalf("factory %q witness key = %q", key, witness.Factory)
		}
		if witness.HasConsumer != consumer.Supports(key) {
			t.Fatalf("factory %q HasConsumer = %v, want %v", key, witness.HasConsumer, consumer.Supports(key))
		}
		for schemaName, schema := range map[string]string{
			"config": witness.Config, "metadata": witness.Metadata,
		} {
			if schema == "" {
				continue
			}
			if _, err := util.CompileSchema(schema); err != nil {
				t.Fatalf("factory %q %s schema: %v", key, schemaName, err)
			}
		}
	}

	for _, factory := range consumer.Factories() {
		if _, ok := pluginRegistry[factory]; !ok {
			t.Fatalf("consumer factory %q is absent from generated plugin registry", factory)
		}
		witness, err := SchemaWitnessForFactory(factory)
		if err != nil {
			t.Fatal(err)
		}
		if !witness.HasConsumer {
			t.Fatalf("consumer factory %q is not marked by its witness", factory)
		}
	}
}

func TestSchemaWitnessRegisteredInitMethodsAvoidRuntimeDependencies(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	packagePaths := make(map[string]struct{})
	for _, entry := range manifest.Plugins {
		for _, factory := range entry.Factories {
			const prefix = "github.com/wklken/apisix-go/pkg/plugin/"
			relative, ok := strings.CutPrefix(factory.ImportPath, prefix)
			if !ok || relative == "" || strings.Contains(relative, "..") {
				t.Fatalf("factory %q import path = %q", factory.Key, factory.ImportPath)
			}
			packagePaths[filepath.FromSlash(relative)] = struct{}{}
		}
	}

	forbidden := map[string]struct{}{
		"StaticConfig": {}, "DataEncryption": {}, "ScopedSecrets": {},
		"MetadataView": {}, "ConsumerLookup": {}, "TaskRegistry": {},
		"PostInit": {}, "Handler": {}, "MaterializeSecrets": {},
		"MaterializeScopedSecrets": {}, "MaterializePluginSecrets": {},
		"MaterializeScopedPluginSecrets": {}, "NewTaskRegistry": {},
		"NewResourceRegistry": {}, "Acquire": {}, "Go": {},
	}
	fset := token.NewFileSet()
	var violations []string
	for packagePath := range packagePaths {
		entries, err := os.ReadDir(packagePath)
		if err != nil {
			t.Fatalf("read registered plugin package %q: %v", packagePath, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(packagePath, entry.Name())
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Name.Name != "Init" || function.Recv == nil || function.Body == nil {
					continue
				}
				ast.Inspect(function.Body, func(node ast.Node) bool {
					switch typed := node.(type) {
					case *ast.GoStmt:
						violations = append(violations, fset.Position(typed.Pos()).String()+": goroutine")
					case *ast.CallExpr:
						name := calledFunctionName(typed.Fun)
						if _, denied := forbidden[name]; denied {
							violations = append(violations, fset.Position(typed.Pos()).String()+": "+name)
						}
					}
					return true
				})
			}
		}
	}
	if len(violations) != 0 {
		sort.Strings(violations)
		t.Fatalf("registered plugin Init methods cross the schema-only boundary: %s", strings.Join(violations, ", "))
	}
}

func calledFunctionName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return ""
	}
}

type schemaWitnessSentinel struct {
	initCalls int
	initErr   error
}

func (plugin *schemaWitnessSentinel) Init() error {
	plugin.initCalls++
	return plugin.initErr
}

func (*schemaWitnessSentinel) PostInit() error { panic("PostInit called by schema witness") }

func (*schemaWitnessSentinel) Handler(http.Handler) http.Handler {
	panic("Handler called by schema witness")
}

func (*schemaWitnessSentinel) Config() any       { return nil }
func (*schemaWitnessSentinel) GetSchema() string { return `{"type":"object"}` }
func (*schemaWitnessSentinel) GetMetadataSchema() string {
	return `{"type":"object","title":"metadata"}`
}
func (*schemaWitnessSentinel) GetPriority() int { return 0 }
func (*schemaWitnessSentinel) GetName() string  { return "schema-sentinel" }

func (*schemaWitnessSentinel) SetDependencies(base.Dependencies) {
	panic("SetDependencies called by schema witness")
}

func (*schemaWitnessSentinel) MaterializeSecrets() error {
	panic("legacy materializer called by schema witness")
}

func (*schemaWitnessSentinel) MaterializeScopedSecrets(context.Context, base.ScopedSecretAccess) error {
	panic("scoped materializer called by schema witness")
}
