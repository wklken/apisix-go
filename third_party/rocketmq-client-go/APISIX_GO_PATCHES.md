# apisix-go RocketMQ client patches

This directory is a source copy of
`github.com/apache/rocketmq-client-go/v2` at
`v2.1.3-0.20231106021916-c9e197c3af45`. The Apache-2.0 `LICENSE` and
`NOTICE` files are preserved.

apisix-go carries six narrowly scoped runtime-safety patches:

1. `internal.DefaultClientOptions` copies `DefaultRemotingClientConfig` for
   each client. Upstream returns a pointer to one global value, so applying
   `producer.WithTls` to a prepared generation changes existing producers.
2. Name-server route lookup propagates the caller context through the request,
   including the route lock. Upstream replaces it with `context.Background`,
   allowing route lookup and lock waits to outlive the plugin deadline.
3. TLS connection setup uses `tls.Dialer.DialContext`, derives the TLS
   `ServerName` from the target address, and supports explicit CA-backed
   verification. The default remains APISIX-compatible verification-off;
   strict plugin configuration enables verification.
4. Remoting send paths propagate the caller context through interceptors,
   connection locking, per-connection write locking, and blocked writes.
   Cancellation sets an immediate write deadline and returns the context error.
5. Producer route/send retry loops stop immediately when their caller context
   is canceled instead of wrapping the cancellation and continuing retries.
6. Client background tasks use cancelable initial delays and a `WaitGroup`;
   `Shutdown` cancels and joins them before releasing the client and remoting
   resources.

The exact production-file boundary for these patches is:

- `internal/client.go`
- `internal/mock_namesrv.go`
- `internal/namesrv.go`
- `internal/remote/remote_client.go`
- `internal/remote/tcp_conn.go`
- `internal/route.go`
- `producer/option.go`
- `producer/producer.go`

The focused test-only additions are:

- `internal/client_lifecycle_test.go`
- `internal/client_options_isolation_test.go`
- `internal/remote/remote_context_test.go`
- `internal/remote/tcp_conn_context_test.go`
- `internal/route_test.go`
- `internal/route_context_test.go`
- `producer/context_test.go`

`LICENSE` and `NOTICE` must remain byte-for-byte identical to the pinned
upstream module. No other production or test file differences are permitted.

Remove these replacements when an official remoting client release contains
all fixes and the focused apisix-go generation-isolation, cancellation, TLS,
and lifecycle tests pass against it.
