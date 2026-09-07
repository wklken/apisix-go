package tlsconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync/atomic"
)

type clientVerificationContextKey struct{}

type clientVerification struct {
	verified   bool
	serverName string
	skip       []*regexp.Regexp
	hosts      *clientHostPolicy
}

type connectionVerification struct {
	result atomic.Pointer[clientVerification]
}

// ConnectionContext gives one connection an owned place for its handshake result.
// net/http passes this context to both TLS handshakes and subsequent requests.
func ConnectionContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, clientVerificationContextKey{}, &connectionVerification{})
}

// VerifyClientRequests enforces URI-conditional mTLS using the connection's
// handshake policy, including on keep-alive requests after a generation swap.
func VerifyClientRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			if connection, ok := r.Context().Value(clientVerificationContextKey{}).(*connectionVerification); ok {
				if result := connection.result.Load(); result != nil && !result.permits(r) {
					http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (result *clientVerification) permits(r *http.Request) bool {
	host := r.Host
	if value, _, err := net.SplitHostPort(host); err == nil {
		host = value
	}
	host = normalizeSNI(host)
	if result.hosts.protected(host) && result.serverName != host {
		return false
	}
	if !result.verified {
		uri := path.Clean(r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/") && uri != "/" {
			uri += "/"
		}
		for _, pattern := range result.skip {
			if pattern.MatchString(uri) {
				return true
			}
		}
		return false
	}
	return true
}

// Retain only immutable host protection flags on a connection, never server keys.
type clientHostPolicy struct {
	exact    map[string]bool
	wildcard []clientWildcardHost
	catchAll bool
}

type clientWildcardHost struct {
	suffix    string
	protected bool
}

func (index *certificateIndex) clientHosts() *clientHostPolicy {
	policy := &clientHostPolicy{exact: make(map[string]bool, len(index.exact))}
	for name, entry := range index.exact {
		policy.exact[name] = entry.clientCAs != nil
	}
	for _, entry := range index.wildcard {
		policy.wildcard = append(policy.wildcard, clientWildcardHost{entry.suffix, entry.clientCAs != nil})
	}
	policy.catchAll = index.catchAll != nil && index.catchAll.clientCAs != nil
	return policy
}

func (policy *clientHostPolicy) protected(host string) bool {
	if flag, found := policy.exact[host]; found {
		return flag
	}
	for _, entry := range policy.wildcard {
		if strings.HasSuffix(host, entry.suffix) {
			return entry.protected && wildcardMatches(host, entry.suffix)
		}
	}
	return policy.catchAll
}

func recordClientVerification(
	selected *tls.Config,
	hello *tls.ClientHelloInfo,
	entry certificateEntry,
	hosts *clientHostPolicy,
	effectiveServerName string,
) {
	var connection *connectionVerification
	if hello != nil && hello.Context() != nil {
		connection, _ = hello.Context().Value(clientVerificationContextKey{}).(*connectionVerification)
	}
	previous := selected.VerifyConnection
	selected.VerifyConnection = func(state tls.ConnectionState) error {
		if previous != nil {
			if err := previous(state); err != nil {
				return err
			}
		}
		if connection == nil {
			if entry.skipMTLS != nil {
				return fmt.Errorf("deferred client verification requires HTTP connection context")
			}
			return nil
		}
		if entry.skipMTLS == nil {
			connection.result.Store(
				&clientVerification{verified: true, hosts: hosts, serverName: normalizeSNI(effectiveServerName)},
			)
			return nil
		}
		verified := false
		if len(state.PeerCertificates) > 0 {
			intermediates := x509.NewCertPool()
			for _, cert := range state.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}
			chains, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         entry.clientCAs,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			})
			if err == nil {
				for _, chain := range chains {
					if len(chain)-2 <= entry.clientDepth {
						verified = true
						break
					}
				}
			}
		}
		connection.result.Store(
			&clientVerification{
				verified:   verified,
				skip:       entry.skipMTLS,
				hosts:      hosts,
				serverName: normalizeSNI(effectiveServerName),
			},
		)
		return nil
	}
}
