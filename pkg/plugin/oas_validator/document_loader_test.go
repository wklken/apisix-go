package oas_validator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDocumentGraphEnforcesTotalByteLimit(t *testing.T) {
	origin := mustParseURL(t, "https://spec.example/openapi.json")
	exact := []byte(strings.Repeat(" ", maxOASTotalBytes-len(`{"$ref":"child.json"}`)) + `{"$ref":"child.json"}`)
	if _, err := preloadDocumentGraph(context.Background(), exact, origin, func(
		context.Context, *url.URL,
	) ([]byte, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("exact-size graph rejected: %v", err)
	}

	oversized := append(append([]byte(nil), exact...), ' ')
	if _, err := preloadDocumentGraph(context.Background(), oversized, origin, nil); err == nil {
		t.Fatal("oversized graph accepted")
	}
}

func TestDocumentGraphEnforcesReferenceCount(t *testing.T) {
	origin := mustParseURL(t, "https://spec.example/openapi.json")
	for _, count := range []int{maxOASExternalRefs, maxOASExternalRefs + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			refs := make([]string, count)
			for i := range refs {
				refs[i] = fmt.Sprintf(`{"$ref":"ref-%d.json"}`, i)
			}
			root := []byte("[" + strings.Join(refs, ",") + "]")
			_, err := preloadDocumentGraph(context.Background(), root, origin, func(
				context.Context, *url.URL,
			) ([]byte, error) {
				return []byte(`{}`), nil
			})
			if count == maxOASExternalRefs && err != nil {
				t.Fatalf("%d refs rejected: %v", count, err)
			}
			if count > maxOASExternalRefs && err == nil {
				t.Fatalf("%d refs accepted", count)
			}
		})
	}
}

func TestDocumentGraphEnforcesDepthAndCycles(t *testing.T) {
	origin := mustParseURL(t, "https://spec.example/root.json")
	for _, depth := range []int{maxOASRefDepth, maxOASRefDepth + 1} {
		t.Run(fmt.Sprintf("depth-%d", depth), func(t *testing.T) {
			root := []byte(`{"$ref":"level-1.json"}`)
			_, err := preloadDocumentGraph(context.Background(), root, origin, func(
				_ context.Context, ref *url.URL,
			) ([]byte, error) {
				var level int
				_, _ = fmt.Sscanf(strings.TrimPrefix(ref.Path, "/level-"), "%d.json", &level)
				if level == depth {
					return []byte(`{}`), nil
				}
				return fmt.Appendf(nil, `{"$ref":"level-%d.json"}`, level+1), nil
			})
			if depth == maxOASRefDepth && err != nil {
				t.Fatalf("depth %d rejected: %v", depth, err)
			}
			if depth > maxOASRefDepth && err == nil {
				t.Fatalf("depth %d accepted", depth)
			}
		})
	}

	_, err := preloadDocumentGraph(context.Background(), []byte(`{"$ref":"a.json"}`), origin, func(
		_ context.Context, ref *url.URL,
	) ([]byte, error) {
		if strings.HasSuffix(ref.Path, "/a.json") {
			return []byte(`{"$ref":"b.json"}`), nil
		}
		return []byte(`{"$ref":"a.json"}`), nil
	})
	if err == nil {
		t.Fatal("external reference cycle accepted")
	}
}

func TestDocumentGraphRejectsYAMLAliases(t *testing.T) {
	origin := mustParseURL(t, "https://spec.example/openapi.yaml")
	root := []byte("schema: &shared\n  $ref: child.json\nalias: *shared\n")
	_, err := preloadDocumentGraph(context.Background(), root, origin, func(
		context.Context, *url.URL,
	) ([]byte, error) {
		return []byte(`{}`), nil
	})
	if err == nil {
		t.Fatal("YAML alias accepted in untrusted OpenAPI reference graph")
	}
}

func TestDocumentHTTPClientAllowsLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	root := mustParseURL(t, server.URL+"/openapi.json")

	client := newDocumentHTTPClient(true, 1000)
	if _, err := fetchDocument(context.Background(), client, root, nil, root); err != nil {
		t.Fatalf("loopback spec URL rejected: %v", err)
	}
}

func TestDocumentHTTPClientDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()

	rootServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/external", http.StatusFound)
	}))
	defer rootServer.Close()

	root := mustParseURL(t, rootServer.URL+"/root")
	client := newDocumentHTTPClient(true, 1000)
	if _, err := fetchDocument(context.Background(), client, root, nil, root); err == nil ||
		!strings.Contains(err.Error(), "status 302") {
		t.Fatalf("redirect fetch error = %v, want status 302", err)
	}
	if followed.Load() {
		t.Fatal("openapi document redirect was followed")
	}
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
