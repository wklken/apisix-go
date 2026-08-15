package oas_validator

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	maxOASExternalRefs = 64
	maxOASRefDepth     = 16
	maxOASRedirects    = 3
)

type documentFetcher func(context.Context, *url.URL) ([]byte, error)

// preloadDocumentGraph fetches every external reference before kin-openapi
// sees the document. The compiler can therefore only read from this bounded
// in-memory graph.
func preloadDocumentGraph(
	ctx context.Context,
	root []byte,
	origin *url.URL,
	fetch documentFetcher,
) (map[string][]byte, error) {
	graph := &documentGraph{
		fetch:  fetch,
		docs:   make(map[string][]byte),
		active: make(map[string]bool),
		total:  len(root),
	}
	if graph.total > maxOASTotalBytes {
		return nil, fmt.Errorf("openapi document graph exceeds %d bytes", maxOASTotalBytes)
	}
	if origin != nil {
		graph.active[documentURL(origin)] = true
	}
	if err := graph.walk(ctx, root, origin, 0); err != nil {
		return nil, err
	}
	return graph.docs, nil
}

type documentGraph struct {
	fetch      documentFetcher
	docs       map[string][]byte
	active     map[string]bool
	total      int
	references int
}

func (g *documentGraph) walk(ctx context.Context, raw []byte, baseURL *url.URL, depth int) error {
	refs, err := externalDocumentRefs(raw)
	if err != nil {
		return fmt.Errorf("parse openapi reference graph: %w", err)
	}
	for _, ref := range refs {
		g.references++
		if g.references > maxOASExternalRefs {
			return fmt.Errorf("openapi document graph exceeds %d external references", maxOASExternalRefs)
		}
		if depth+1 > maxOASRefDepth {
			return fmt.Errorf("openapi document graph exceeds reference depth %d", maxOASRefDepth)
		}
		refURL, err := resolveDocumentURL(baseURL, ref)
		if err != nil {
			return err
		}
		key := documentURL(refURL)
		if g.active[key] {
			return fmt.Errorf("openapi external reference cycle at %q", key)
		}
		if _, ok := g.docs[key]; ok {
			continue
		}
		if g.fetch == nil {
			return errors.New("openapi external reference fetcher is unavailable")
		}
		data, err := g.fetch(ctx, refURL)
		if err != nil {
			return fmt.Errorf("fetch openapi external reference %q: %w", key, err)
		}
		g.total += len(data)
		if g.total > maxOASTotalBytes {
			return fmt.Errorf("openapi document graph exceeds %d bytes", maxOASTotalBytes)
		}
		g.docs[key] = append([]byte(nil), data...)
		g.active[key] = true
		if err := g.walk(ctx, data, refURL, depth+1); err != nil {
			return err
		}
		delete(g.active, key)
	}
	return nil
}

func externalDocumentRefs(raw []byte) ([]string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	refs := make([]string, 0)
	stack := []*yaml.Node{&document}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if node == nil {
			continue
		}
		if node.Kind == yaml.AliasNode {
			return nil, errors.New("yaml aliases are not allowed in OpenAPI documents")
		}
		if node.Kind == yaml.MappingNode {
			for index := 0; index+1 < len(node.Content); index += 2 {
				key, value := node.Content[index], node.Content[index+1]
				if key.Value == "$ref" && value.Kind == yaml.ScalarNode &&
					value.Value != "" && !strings.HasPrefix(value.Value, "#") {
					refs = append(refs, value.Value)
				}
				stack = append(stack, value)
			}
			continue
		}
		stack = append(stack, node.Content...)
	}
	return refs, nil
}

func resolveDocumentURL(baseURL *url.URL, ref string) (*url.URL, error) {
	parsed, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("parse openapi external reference %q: %w", ref, err)
	}
	if !parsed.IsAbs() {
		if baseURL == nil {
			return nil, fmt.Errorf("relative openapi external reference %q requires spec_url", ref)
		}
		parsed = baseURL.ResolveReference(parsed)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("openapi external reference %q must use HTTP or HTTPS", ref)
	}
	if parsed.Hostname() == "" || parsed.User != nil {
		return nil, fmt.Errorf("openapi external reference %q has an invalid host", ref)
	}
	parsed.Fragment = ""
	return parsed, nil
}

func documentURL(value *url.URL) string {
	cloned := *value
	cloned.Fragment = ""
	return cloned.String()
}

type addressPolicy struct {
	hosts     map[string]struct{}
	addresses map[netip.Addr]struct{}
	prefixes  []netip.Prefix
}

func newAddressPolicy(entries []string) (*addressPolicy, error) {
	policy := &addressPolicy{
		hosts:     make(map[string]struct{}),
		addresses: make(map[netip.Addr]struct{}),
	}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, errors.New("spec_url_allowed_addresses contains an empty entry")
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			policy.prefixes = append(policy.prefixes, prefix)
			continue
		}
		if address, err := netip.ParseAddr(entry); err == nil {
			policy.addresses[address.Unmap()] = struct{}{}
			continue
		}
		policy.hosts[normalizeHostname(entry)] = struct{}{}
	}
	return policy, nil
}

func (p *addressPolicy) hostAllowed(host string) bool {
	_, ok := p.hosts[normalizeHostname(host)]
	return ok
}

func (p *addressPolicy) addressAllowed(address netip.Addr) bool {
	address = address.Unmap()
	if _, ok := p.addresses[address]; ok {
		return true
	}
	for _, prefix := range p.prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func normalizeHostname(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func (p *addressPolicy) validateAddress(host string, address netip.Addr) error {
	address = address.Unmap()
	if p.hostAllowed(host) || p.addressAllowed(address) {
		return nil
	}
	if blockedDocumentAddress(address) {
		return fmt.Errorf("openapi document address %s is not allowed", address)
	}
	return nil
}

func blockedDocumentAddress(address netip.Addr) bool {
	address = address.Unmap()
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	metadata := []netip.Addr{
		netip.MustParseAddr("100.100.100.200"),
		netip.MustParseAddr("fd00:ec2::254"),
	}
	if !address.IsValid() || address.IsUnspecified() || address.IsLoopback() || address.IsPrivate() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() ||
		cgnat.Contains(address) {
		return true
	}
	return slices.Contains(metadata, address)
}

func newDocumentHTTPClient(
	root *url.URL,
	allowed []string,
	headers map[string]string,
	sslVerify bool,
	timeoutMilliseconds int,
) (*http.Client, error) {
	policy, err := newAddressPolicy(allowed)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: time.Duration(timeoutMilliseconds) * time.Millisecond}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !sslVerify},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			if len(addresses) == 0 {
				return nil, fmt.Errorf("openapi document host %q resolved to no addresses", host)
			}
			for _, candidate := range addresses {
				if err := policy.validateAddress(host, candidate); err != nil {
					return nil, err
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
		},
	}
	client := &http.Client{
		Timeout:   time.Duration(timeoutMilliseconds) * time.Millisecond,
		Transport: transport,
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxOASRedirects {
			return fmt.Errorf("openapi document fetch exceeds %d redirects", maxOASRedirects)
		}
		if err := validateDocumentURL(req.URL); err != nil {
			return err
		}
		if !sameOrigin(req.URL, root) {
			for name := range headers {
				req.Header.Del(name)
			}
		}
		return nil
	}
	return client, nil
}

func fetchDocument(
	ctx context.Context,
	client *http.Client,
	document *url.URL,
	headers map[string]string,
	root *url.URL,
) ([]byte, error) {
	if err := validateDocumentURL(document); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, document.String(), nil)
	if err != nil {
		return nil, err
	}
	if sameOrigin(document, root) {
		for name, value := range headers {
			req.Header.Set(name, value)
		}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openapi document URL returned status %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxOASTotalBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxOASTotalBytes {
		return nil, fmt.Errorf("openapi document exceeds %d bytes", maxOASTotalBytes)
	}
	return body, nil
}

func validateDocumentURL(value *url.URL) error {
	if value == nil || (value.Scheme != "http" && value.Scheme != "https") || value.Hostname() == "" ||
		value.User != nil {
		return errors.New("openapi document URL must be an HTTP(S) URL without user information")
	}
	return nil
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}
