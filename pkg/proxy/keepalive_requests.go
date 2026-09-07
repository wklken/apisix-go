package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
)

// The counter belongs to the socket, so idle expiry and remote closes do not
// leave entries in a side table. TLS and progress-timeout wrappers retain it.
type keepaliveCountConn struct {
	net.Conn
	requests atomic.Int64
}

func installKeepaliveCounter(transport *http.Transport) {
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &keepaliveCountConn{Conn: conn}, nil
	}
}

func keepaliveCounter(conn net.Conn) *keepaliveCountConn {
	for conn != nil {
		switch wrapped := conn.(type) {
		case *keepaliveCountConn:
			return wrapped
		case *tls.Conn:
			conn = wrapped.NetConn()
		case *sendTimeoutConn:
			conn = wrapped.Conn
		default:
			return nil
		}
	}
	return nil
}

type keepaliveRequestTransport struct {
	base  http.RoundTripper
	limit int64
}

func (transport *keepaliveRequestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	outgoing := r.Clone(r.Context())
	var retiring net.Conn
	originalConnection, hadConnection := outgoing.Header["Connection"]
	originalConnection = append([]string(nil), originalConnection...)
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		retiring = nil
		outgoing.Close = r.Close
		if hadConnection {
			outgoing.Header["Connection"] = append([]string(nil), originalConnection...)
		} else {
			delete(outgoing.Header, "Connection")
		}
		counter := keepaliveCounter(info.Conn)
		if counter == nil || counter.requests.Add(1) < transport.limit {
			return
		}
		// net/http calls GotConn before writing headers. Its request-body rewind
		// copies share this private header map; setting Close alone would miss them.
		if outgoing.Header.Get("Upgrade") != "" {
			// Preserve the upgrade token while preventing a rejected handshake
			// from returning an ordinary HTTP connection to the idle pool.
			outgoing.Header.Set("Connection", outgoing.Header.Get("Connection")+", close")
		} else {
			outgoing.Header.Set("Connection", "close")
		}
		outgoing.Close = true
		retiring = info.Conn
	}}
	outgoing = outgoing.WithContext(httptrace.WithClientTrace(outgoing.Context(), trace))
	response, err := transport.base.RoundTrip(outgoing)
	if retiring != nil {
		if err != nil || response == nil || response.Body == nil || response.Body == http.NoBody {
			_ = retiring.Close()
		} else {
			response.Body = wrapReleaseBody(response.Body, func() { _ = retiring.Close() })
		}
	}
	return response, err
}
