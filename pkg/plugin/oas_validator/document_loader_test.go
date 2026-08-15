package oas_validator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

func TestDocumentHTTPClientRejectsPrivateAddressesUnlessAllowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	root := mustParseURL(t, server.URL+"/openapi.json")

	client, err := newDocumentHTTPClient(root, nil, nil, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchDocument(context.Background(), client, root, nil, root); err == nil {
		t.Fatal("loopback spec URL accepted without allowlist")
	}

	client, err = newDocumentHTTPClient(root, []string{"127.0.0.1"}, nil, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchDocument(context.Background(), client, root, nil, root); err != nil {
		t.Fatalf("allowlisted loopback spec URL rejected: %v", err)
	}
}

func TestBlockedDocumentAddressCoversPrivateAndMetadataRanges(t *testing.T) {
	for _, address := range []string{
		"10.0.0.1",
		"100.64.0.1",
		"100.100.100.200",
		"169.254.169.254",
		"::1",
		"fd00:ec2::254",
	} {
		if !blockedDocumentAddress(netip.MustParseAddr(address)) {
			t.Errorf("blockedDocumentAddress(%s) = false", address)
		}
	}
	if blockedDocumentAddress(netip.MustParseAddr("93.184.216.34")) {
		t.Fatal("public address was blocked")
	}
}

func TestDocumentHTTPClientBoundsRedirectsAndStripsCrossOriginHeaders(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Spec-Secret") != "" {
			leaked.Store(true)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()

	redirects := atomic.Int32{}
	rootServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer root" {
			t.Errorf("root Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/root" {
			http.Redirect(w, r, target.URL+"/external", http.StatusFound)
			return
		}
		redirects.Add(1)
		http.Redirect(w, r, fmt.Sprintf("/loop?n=%d", redirects.Load()), http.StatusFound)
	}))
	defer rootServer.Close()

	root := mustParseURL(t, rootServer.URL+"/root")
	headers := map[string]string{"Authorization": "Bearer root", "X-Spec-Secret": "secret"}
	allowed := []string{"127.0.0.1"}
	client, err := newDocumentHTTPClient(root, allowed, headers, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchDocument(context.Background(), client, root, headers, root); err != nil {
		t.Fatalf("cross-origin redirect failed: %v", err)
	}
	if leaked.Load() {
		t.Fatal("configured root headers leaked across origin")
	}

	loop := mustParseURL(t, rootServer.URL+"/loop")
	if _, err := fetchDocument(context.Background(), client, loop, headers, root); err == nil {
		t.Fatal("more than three redirects accepted")
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
