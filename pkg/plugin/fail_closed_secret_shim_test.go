package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

func TestFailClosedSecretShimsAreAbsent(t *testing.T) {
	t.Helper()
	files := []string{
		"ai_aliyun_content_moderation/secrets.go",
		"ai_aws_content_moderation/secrets.go",
		"ai_rag/secrets.go",
		"authz_keycloak/secrets.go",
		"clickhouse_logger/secrets.go",
		"limit_count/secrets.go",
	}
	for _, path := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Clean(path), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Name.Name == "MaterializeSecrets" {
				t.Errorf("fail-closed legacy secret shim remains in %s", path)
			}
		}
	}
}
