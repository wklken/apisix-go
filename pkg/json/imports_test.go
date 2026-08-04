package json_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var legacyEncodingJSONImports = map[string]struct{}{
	"cmd/root.go":                               {},
	"pkg/config/standalone.go":                  {},
	"pkg/plugin/base/jwt.go":                    {},
	"pkg/plugin/base/oauth_session.go":          {},
	"pkg/plugin/brotli/plugin.go":               {},
	"pkg/plugin/data_mask/plugin.go":            {},
	"pkg/plugin/elasticsearch_logger/plugin.go": {},
	"pkg/plugin/exit_transformer/plugin.go":     {},
	"pkg/plugin/expr/expression.go":             {},
	"pkg/plugin/forward_auth/plugin.go":         {},
	"pkg/plugin/grpc_transcode/plugin.go":       {},
	"pkg/plugin/limit_count/plugin.go":          {},
	"pkg/plugin/log_rotate/plugin.go":           {},
	"pkg/plugin/mocking/plugin.go":              {},
	"pkg/plugin/proxy_rewrite/plugin.go":        {},
	"pkg/plugin/response_rewrite/plugin.go":     {},
	"pkg/plugin/server_info/plugin.go":          {},
	"pkg/plugin/splunk_hec_logging/plugin.go":   {},
	"pkg/plugin/tcp_logger/plugin.go":           {},
	"pkg/plugin/traffic_label/plugin.go":        {},
	"pkg/plugin/traffic_split/plugin.go":        {},
	"pkg/plugin/wolf_rbac/plugin.go":            {},
	"pkg/plugin/workflow/plugin.go":             {},
	"pkg/resource/route.go":                     {},
	"pkg/route/builder.go":                      {},
	"pkg/server/server.go":                      {},
}

func TestProductionCodeUsesProjectJSON(t *testing.T) {
	root := repositoryRoot(t)
	var offenders []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".cache", ".worktrees", "vendor":
				return filepath.SkipDir
			}
			if path == filepath.Join(root, "t") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "pkg/json/types.go" {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if importPath == "encoding/json" {
				if _, allowed := legacyEncodingJSONImports[relative]; !allowed {
					offenders = append(offenders, relative)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(offenders)
	if len(offenders) != 0 {
		t.Fatalf("production files must use pkg/json; direct encoding/json imports: %s", strings.Join(offenders, ", "))
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve current test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
