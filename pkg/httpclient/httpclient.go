// Package httpclient provides outbound HTTP clients that do not inherit
// process environment proxy settings.
package httpclient

import "net/http"

// NewTransport clones Go's default transport without its environment proxy.
func NewTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}

// New returns an HTTP client that does not use environment proxies.
func New() *http.Client {
	return &http.Client{Transport: NewTransport()}
}
