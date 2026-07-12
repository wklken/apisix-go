# APISIX 3.17 Protocol Bridge Design

> Status: design baseline plus bounded Dubbo/Kafka slices and a TCP/MQTT stream owner, 2026-07-12
> This document is the contract for implementing the protocol plugins in
> [`apisix-3.17-plugin-parity-execution-todo.md`](apisix-3.17-plugin-parity-execution-todo.md).

The current Go runtime has an HTTP `http.Handler` pipeline and now also owns a
bounded TCP stream listener/route snapshot with cancellation and result/log
callbacks. It does not yet expose a general stream-variable/plugin-chain API,
active health probes, TLS/UDP stream owner, or Kafka-specific stream
binding. Protocol-owned bounded transport and stream boundaries therefore have
different integration states:

| Plugin | Current local behavior | Design consequence |
|---|---|---|
| `kafka-proxy` | Stores SASL/PLAIN settings in request context, owns the official PubSub protobuf WebSocket command loop for list-offset/fetch, and uses a bounded `kafka-go` consumer abstraction with upstream TLS verification plus inline or local SSL-resource client certificates; an in-process TLS wire fixture verifies the actual PLAIN handshake and broker auth error; the raw-frame WebSocket bridge remains a compatibility extension. | Keep external broker smoke coverage optional; the raw bridge must not be counted as APISIX 3.17 Kafka parity. |
| `dubbo-proxy` | Stores service name/version/method in request context and now has a Hessian2 HTTP-to-Dubbo terminal with route upstream selection, bounded connect-only retries, and passive health outcome reporting through `pkg/proxy`. | Keep persistent shared-connection multiplexing, response-ID matching, retry after request write, active probes, and native health lifecycle separate from the bounded per-target gate. |
| `http-dubbo` | Builds a Dubbo 2.x fastjson request and calls a selected TCP upstream, with bounded connect-only retries and passive health outcome reporting through `pkg/proxy`. | Keep this fastjson adapter separate from the hessian2 adapter used by `dubbo-proxy`; never retry after request bytes are written or invent active probe state in the codec. |
| `mqtt-proxy` | Validates config, exposes a bounded MQTT 3.1.1/5.0 CONNECT parser, provides plugin-owned `ServeStream`/`ServeListener`, and is now bound to the main TCP stream-route owner with `server_addr`/`server_port`/`remote_addr` matching, bounded cancellation/backpressure, and `StreamInfo` result callbacks; the HTTP handler remains a compatibility no-op. | Retain TLS/mTLS, UDP, and general stream-plugin-chain behavior as separate scope. |

The official APISIX documentation confirms these boundaries: `kafka-proxy`
configures a `kafka` upstream and currently supports SASL/PLAIN, `dubbo-proxy`
uses hessian2 with `Map<String, Object>` request/response data, `http-dubbo`
uses fastjson for Dubbo 2.x, and `mqtt-proxy` is an L4 stream plugin that
exposes `mqtt_client_id` for consistent hashing.

References: [kafka-proxy](https://apisix.apache.org/docs/apisix/plugins/kafka-proxy/),
[dubbo-proxy](https://apisix.apache.org/docs/apisix/plugins/dubbo-proxy/),
[http-dubbo](https://apisix.apache.org/docs/apisix/3.11/plugins/http-dubbo/),
[mqtt-proxy](https://apisix.apache.org/docs/apisix/plugins/mqtt-proxy/).

## Shared transport contract

Protocol-owned boundaries are introduced in the plugin packages; do not make
`pkg/proxy` know about Kafka, Dubbo, or MQTT. The request/response boundary
should provide:

```text
Dial(ctx, target, options) -> Conn
Conn.RoundTrip(ctx, request) -> response       # request/response protocols
Conn.Copy(ctx, client, upstream)                # bidirectional stream protocols
Conn.Close()                                    # idempotent cleanup
```

The concrete Go API may differ, but these invariants are required:

1. Every dial, write, read, and queue wait has a bounded deadline derived from
   route/upstream timeout settings.
2. Cancellation closes both directions and releases the connection slot.
3. Protocol errors are distinguishable from transport errors so the route can
   return a deterministic client (4xx) or upstream (5xx) response.
4. Raw request bytes are replayed exactly when a preread parser is used; the
   parser may inspect bytes but must not consume data that the upstream should
   receive.
5. No plaintext credential is included in errors, logs, context dumps, or
   serialized route status.

Each protocol implementation owns its fake server/client fixture. Do not add a
real broker or external service to the default test suite.

## Kafka proxy (`kafka-proxy`)

### Contract decision

The APISIX plugin is an upstream Kafka consumer bridge, not a REST producer
facade. The official Go scope is therefore:

- support `scheme: kafka` upstream nodes and the APISIX WebSocket PubSub
  protobuf protocol;
- decode `PubSubReq` and preserve `sequence` in `PubSubResp`;
- implement `cmd_kafka_list_offset` and `cmd_kafka_fetch`, including Kafka
  message offset/timestamp/key/value fields;
- attach the resolved SASL/PLAIN credentials to the Kafka consumer dialer;
- map malformed PubSub messages, Kafka errors, timeouts, and authentication
  failures deterministically without leaking credentials;
- defer HTTP-to-Kafka REST semantics and SASL mechanisms beyond PLAIN.

The existing raw length-prefixed Kafka WebSocket bridge remains available only
as a bounded compatibility extension. It is not the official APISIX protocol
and must not be used as the parity acceptance criterion.

### Runtime flow

```text
HTTP WebSocket route
  -> upgrade and create the APISIX PubSub session
  -> decode one bounded PubSubReq per binary WebSocket message
  -> execute list-offset/fetch against the configured Kafka brokers
  -> encode PubSubResp with the original sequence
  -> map protocol, broker, timeout, and auth errors without leaking secrets
```

The plugin must not open a new connection for every message if the route is a
long-lived stream. Connection reuse is allowed only after request IDs and
concurrent in-flight limits are explicit; otherwise use one bounded connection
per proxied exchange.

### Acceptance tests

- [x] a fake broker receives the exact request frame and returns an exact
  response;
- [x] an oversized response frame is rejected before unbounded allocation;
- [x] context cancellation closes the transport without a goroutine leak;
- [x] a `scheme: kafka` route upgrades a WebSocket and forwards exact raw Kafka
  frames through the selected upstream as a compatibility extension;
- [x] the bounded PubSub protobuf codec round-trips command envelopes,
  sequence values, error/pong responses, and Kafka message fields;
- [x] a `scheme: kafka` route accepts a protobuf `cmd_kafka_list_offset` request
  and returns a matching `kafka_list_offset_resp`;
- [x] a `scheme: kafka` route accepts a protobuf `cmd_kafka_fetch` request and
  returns bounded Kafka messages with offset/timestamp/key/value;
- [x] a configured SASL/PLAIN consumer receives the resolved username/password
  through the consumer factory boundary;
- [x] malformed PubSub messages, fake broker/auth failures, and timeouts map
  deterministically without leaking the password;
- [x] upstream `tls.verify` and inline `client_cert/client_key` or local
  `client_cert_id` SSL resources configure the Kafka dialer, while
  mismatched/invalid pairs and missing resources fail explicitly;
- [x] an in-process TLS broker-wire fixture proves SASL/PLAIN negotiation,
  verifies the exact credentials payload, and maps a broker authentication
  error without exposing the password.

External broker smoke tests remain optional; mechanisms beyond PLAIN are not
part of the APISIX 3.17 plugin schema. The shared secret resolver is already
the credential boundary for the supported PLAIN path.

## Dubbo proxy and HTTP-Dubbo

### Adapter decision

Use one shared invocation lifecycle with two codecs:

| Adapter | Wire codec | Input/output contract |
|---|---|---|
| `http-dubbo` | Dubbo 2.x fastjson | Existing `params_type_desc`, newline-separated fastjson values, and serialized-body escape hatch. |
| `dubbo-proxy` | Dubbo hessian2 | HTTP headers/body map to `Map<String, Object>`; response map becomes HTTP status, headers, and body. |

Do not silently switch the existing `http-dubbo` implementation to hessian2;
that would break its documented fastjson contract. A future shared package can
own framing, request IDs, deadlines, and connection lifecycle while codecs stay
independent.

### Invocation lifecycle

1. The route selects an upstream target through the existing load balancer.
2. The adapter validates the method/service configuration and converts the HTTP
   request to a codec-specific invocation.
3. The transport assigns a request ID, writes one bounded frame, and waits for
   the matching response under connect/write/read deadlines.
4. The adapter maps a successful response to HTTP. Application exceptions,
   malformed frames, and transport failures remain distinguishable.
5. The request body is restored only when a downstream HTTP handler still needs
   it; terminal protocol handlers must not leave a consumed body in shared
   middleware state.

### Multiplexing, retry, and health

- `upstream_multiplex_count` is currently enforced as a per-target upper bound
  on in-flight request terminals; acquisition is cancellation-aware.
- The current Go terminal opens one connection per invocation, so persistent
  shared-connection multiplexing and response-ID matching are intentionally
  deferred until a reusable connection owner exists.
- Retries are opt-in and only allowed before bytes are written, or for a
  request explicitly classified idempotent. Never duplicate an unknown Dubbo
  invocation after a partial write.
- Passive health outcomes use the shared `pkg/proxy` load-balancer abstraction:
  HTTP status, TCP failure, and timeout thresholds can quarantine observed
  nodes, and an exhausted pool fails open. Active probe cadence, cross-worker
  state, and `/v1/healthcheck` remain outside the bounded protocol terminals.
  Do not emulate NGINX/Tengine health state inside a plugin.

### Acceptance tests

- [x] hessian2 map request/response round trip and route-terminal mapping for `dubbo-proxy`;
- existing fastjson request/response behavior remains green for `http-dubbo`;
- [x] concurrent requests stay within the configured per-target in-flight gate;
- persistent shared-connection multiplexing and response-ID matching remain
  deferred until a reusable connection owner exists;
- malformed frame, application exception, upstream close, connect timeout,
  read timeout, cancellation, and retry-after-partial-write cases;
- response headers/body/status mapping and error redaction.

The implementation uses the Apache `github.com/apache/dubbo-go-hessian2`
v1.13.1 module for the wire codec; its dependency and license are explicit in
`go.mod`. Persistent connection pooling remains a separate follow-up.

## MQTT proxy (`mqtt-proxy`)

### Stream prerequisite

`mqtt-proxy` must not be implemented as an HTTP handler. The plugin-owned
`ServeStream`/`ServeListener` boundary accepts a TCP connection, prereads
CONNECT, selects an upstream through a dialer, and owns the connection lifetime.
The main server now supplies the listener, route selection, weighted TCP or
deterministic `chash` upstream owner, cancellation, and result/log callback.
For `key=mqtt_client_id`, the parsed client ID is the hash input and the peer
address is the fallback. General stream variables and
other stream-plugin chaining remain separate contracts. The HTTP route builder
remains unchanged.

The future general stream-plugin interfaces are:

```text
StreamRoute.Match(listener, preread) -> route/config
StreamHandler.Serve(ctx, clientConn, upstreamTarget, config)
StreamVariable.Set("mqtt_client_id", value)
```

The current plugin-owned equivalents are `Plugin.ServeStream` and
`Plugin.ServeListener`; the main runtime wraps them with the listener, route
snapshot, upstream dialer, cancellation, and `Result` callback. The general
variable/plugin-chain interfaces remain intentionally unimplemented.

### CONNECT preread

The protocol-owned parser in `pkg/plugin/mqtt_proxy/connect.go` now validates the
fixed header, remaining-length encoding, protocol name/level, CONNECT flags,
MQTT 5 properties, UTF-8 fields, and payload boundaries. It returns the exact
CONNECT packet length and exposes `ClientIDOrPeer` for the stream dialer. The
parser is deliberately independent of `net.Conn`, so it can be unit-tested
without a broker and reused by the plugin-owned preread boundary.

The MQTT stream owner reads only a bounded prefix (enough for the fixed header,
remaining-length field, protocol name/level, and client ID), validates:

- protocol name (default `MQTT`);
- protocol level (`4` for MQTT 3.1.x or `5` for MQTT 5.0);
- remaining-length encoding and CONNECT flags;
- non-empty client ID when the protocol requires one.

The inspected bytes are replayed to the upstream unchanged. A missing client ID
falls back to the peer address for consistent hashing, matching the official
plugin behavior. `mqtt_client_id` is available to the stream load balancer and
stream log context, not to ordinary HTTP request variables.

### Connection lifecycle and errors

- connect/read/write deadlines are bounded by stream route settings;
- both `io.Copy` directions stop on client close, upstream close, or context
  cancellation;
- malformed CONNECT is rejected and the connection is closed without dialing an
  upstream;
- upstream selection failure and upstream disconnect are recorded as stream
  errors, with no HTTP status invented for a raw TCP client;
- protocol-level 4 and 5 are validated, but MQTT payloads are not decoded or
  rewritten by the gateway.

### Acceptance tests

- [x] MQTT 3.1.x and 5.0 CONNECT preread with byte-for-byte replay in the
  plugin-owned stream boundary;
- [x] malformed fixed header, invalid remaining length, wrong protocol level,
  and invalid flags are rejected before upstream dialing;
- [x] client ID is exposed to consistent hashing; absent ID uses peer address;
- [x] bidirectional payload forwarding, client/upstream close, and cancellation
  in the plugin-owned stream boundary;
- [x] main-server stream-route selection and MQTT `StreamInfo` result/log
  context;
- [x] explicit bounded-backpressure assertion: a large client write to a
  non-reading upstream is released by runtime cancellation;
- [x] no HTTP middleware or request-body assumptions are involved.

## Delivery order

- [x] Add fake network fixtures and the smallest shared deadline/cancellation
  helpers.
- [x] Complete the existing `http-dubbo` error/timeout branches without changing
  its fastjson wire contract.
- [x] Add the hessian2 `dubbo-proxy` adapter and route terminal integration.
- [x] Add the plugin-owned bounded Kafka raw-frame transport and fake-broker
  coverage as a compatibility extension; it is not the official PubSub owner.
- [x] Add the official PubSub protobuf WebSocket owner, Kafka list-offset/fetch
  consumer, SASL/PLAIN dialer, and deterministic error mapping.
- [x] Add the plugin-owned MQTT preread/consistent-hash dialer boundary and
  listener lifecycle.
- [x] Add a main-server TCP stream-route owner for listener configuration,
  route matching, upstream selection, MQTT binding, cancellation, and result
  callbacks; Kafka uses the separate HTTP WebSocket owner above.
- [ ] Only then consider multiplexing optimizations, provider-response retries,
  and optional real broker integration tests; extra Kafka SASL mechanisms are
  outside the APISIX 3.17 plugin schema, while the bounded `http-dubbo`
  connect-only retry is already part of the route terminal.

No plugin should be marked complete while it only accepts schema, stores
context metadata, or runs an HTTP no-op.
