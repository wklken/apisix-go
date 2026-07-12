# APISIX 3.17 Plugin Parity: Execution TODO

> Status snapshot: 2026-07-12
> Source of truth: upstream APISIX `release/3.17` default-plugin list, local `pkg/plugin/init.go`, and the current parity checklist.

This is the execution backlog for making the Go implementation support most of the valuable APISIX 3.17 plugins. It is intentionally separate from the status matrix in [`apisix-3.17-plugin-parity-checklist.md`](apisix-3.17-plugin-parity-checklist.md) and the per-plugin gap notes in [`apisix-3.17-remaining-plugin-todo.md`](apisix-3.17-remaining-plugin-todo.md).

## 1. Baseline and target

- [x] Compare against the APISIX 3.17 default-plugin inventory.
- [x] Register every default plugin that is in Go scope.
- [x] Raise partial implementations from “registered/config accepted” to “usable for the main official behavior” for the current Go-native scope; remaining gaps are explicitly native/runtime, separate-subsystem, or concrete-mismatch-only deferments.
- [x] Keep the status matrix, README percentages, and this execution list consistent after every implementation slice.

Current baseline:

| Measure | Current value |
|---|---:|
| APISIX 3.17 default plugins | 104 |
| Locally registered default plugins | 100 |
| Registration coverage | 96.2% |
| Missing registrations | 4 |
| Checklist entries marked `implement` | 0 |
| Checklist entries marked `monitor` | 89 |
| Checklist entries deferred (`defer-native` or `defer-large`) | 9 |

The four missing registrations are `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`, and `inspect`. They depend on an external plugin runner or Lua/OpenResty runtime and remain explicitly out of normal Go-native parity scope.

### Definition of “supported”

For this project, a plugin is supported when all of the following are true:

1. `plugin.New(name)` returns a real implementation or an explicitly documented native-runtime deferment exists.
2. The APISIX 3.17 user-facing schema is accepted for the supported configuration surface.
3. The main request/response behavior works through the Go route and proxy pipeline.
4. Focused tests cover the important success, rejection, and boundary paths.
5. README and the parity checklist state the remaining limitations honestly.

Registration alone is not sufficient. Configuration-only or context-propagation stubs must remain marked partial until their runtime behavior is implemented or intentionally classified as native-only.

## 2. Delivery rules for every task

The boxes in this section are a reusable per-slice gate, not unfinished
backlog items. Each implementation slice must copy them into its review record
and complete them with its own evidence.

- [ ] Start from the corresponding APISIX `release/3.17` plugin source and schema.
- [ ] Read the current local plugin package, route/proxy interfaces, and nearby tests before editing.
- [ ] Add a focused failing test for each new behavior before implementation.
- [ ] Keep each change to one plugin or one tightly coupled subsystem slice.
- [ ] Reuse existing Go dependencies and project patterns; do not add a dependency without documenting why.
- [ ] Update the plugin README entry, the checklist percentage/Next column, and this file in the same slice.
- [ ] Run focused tests, `go test ./...`, `make build`, remove the generated `apisix` binary, and run `git diff --check` for code changes.
- [ ] Do not claim parity for behavior that is only schema acceptance or a no-op handler.

## 3. Milestone M0 — make the backlog authoritative

These are documentation and measurement tasks that must be completed before the implementation queue grows further.

- [x] Reconcile the checklist `Next=implement` labels with the current remaining-TODO document.
  - `wolf-rbac`, `dingtalk-auth`, and `feishu-auth` have already had their normal Go behavior substantially implemented; classify only concrete remaining gaps as tasks.
  - `ai` is explicitly deferred because the APISIX plugin replaces runtime/router/balancer internals rather than exposing a bounded ordinary plugin behavior.
- [x] Add a separate “registered”, “usable”, and “native-deferred” count to the checklist summary.
- [x] Replace percentage-only tracking with a short list of verified supported behaviors and explicit gaps.
- [x] Add a date reference to the status snapshot so stale estimates are detectable.
- [x] Treat this file as the ordered execution backlog; keep the existing remaining-TODO document as the detailed gap catalog.

Acceptance: a reviewer can determine from the three docs whether a plugin is registered, usable, partial, or intentionally deferred without reading every package.

## 4. Milestone M1 — highest-value practical plugin parity

These tasks have the best impact on the stated goal and do not require a new L4 or external-runtime subsystem.

### M1.1 `workflow`

- [x] Inventory every APISIX 3.17 delegated action whose target plugin is already implemented: upstream 3.17 registers `return`, `limit-count`, and `limit-conn`; Go also retains its existing `limit-req` adapter.
- [x] Preserve delegated action configuration through compilation and `PostInit` for the supported adapters.
- [x] Expand condition support through the existing Go expression layer, including `in`, numeric comparisons, regex, `ipmatch`, nested logic, and APISIX request variables.
- [x] Ensure the real downstream handler runs when an action wraps a plugin with deferred cleanup (`limit-conn`); the regression test already covers this path.
- [x] Reject unsupported actions and invalid `return` status codes during `PostInit` instead of silently falling through.
- [x] Add tests for first-match/first-action semantics, action config propagation, expression matching, invalid actions, invalid return codes, and downstream execution.
- [x] Add a route-builder chain pair covering workflow initialization plus non-matching fallback success and matching `return` rejection.

Acceptance: supported delegated actions execute the target plugin with the same request context and configuration shape; unsupported Lua/runtime actions return a documented limitation rather than silently succeeding.

### M1.2 `oas-validator`

- [x] Add local and external `$ref` resolution across operation parameters, request bodies, and schemas, with bounded HTTP(S) fetches, JSON Pointer selection, relative URL resolution from `spec_url`, document caching, and cycle/depth/count rejection.
- [x] Add metadata `spec_url_ttl` refresh semantics for cached `spec_url` validators; the default is 3600 seconds and inline `spec` remains cached for the plugin lifetime.
- [x] Cover bounded query parameter styles used by common clients: `form` arrays, repeated values, exploded `form` objects, `spaceDelimited`, `pipeDelimited`, and `deepObject`, with schema-guided coercion.
- [x] Cover non-exploded `form` object serialization (`name,value,name,value`) and reject malformed odd-length pairs.
- [x] Cover matrix, label, and simple `style`/`explode` serialization for path parameters plus simple object headers; malformed structured values fail validation.
- [x] Preserve commas in single exploded `form` array values and collect repeated `deepObject` array properties with schema-guided coercion.
- [x] Apply OpenAPI's default `explode=true` to `form` parameters when `explode` is omitted, including array query parameters; explicit `explode=false` remains supported.
- [x] Flatten repeated `spaceDelimited` and `pipeDelimited` query-array values before schema coercion; preserve the existing comma-preserving behavior for exploded `form` arrays.
- [x] Cover Cookie parameters using OpenAPI's default `form`/`explode=true` rules, including primitive values, repeated arrays, and exploded/non-exploded objects; reject unsupported Cookie styles explicitly.
- [x] Cover `spaceDelimited`/`pipeDelimited` object parameters with alternating key/value pairs and reject malformed odd-length pairs.
- [x] Validate `application/x-www-form-urlencoded` request bodies with schema-guided scalar and repeated-field coercion.
- [x] Validate `multipart/form-data` and `text/plain` request bodies with bounded extraction and schema-guided scalar/repeated-field coercion.
- [x] Validate `application/octet-stream` bodies as schema-constrained raw strings and reuse JSON validation for `application/*+json` media types, including specificity-ordered OpenAPI wildcard content keys.
- [x] Validate bounded `application/xml`, `text/xml`, and `application/*+xml` request bodies as schema-guided objects, nested values, attributes, and wrapped/unwrapped arrays; match wildcard content keys and reject malformed XML.
- [x] Validate bounded YAML request bodies for `application/yaml`, `text/yaml`, `application/x-yaml`, and `application/*+yaml` using the existing YAML dependency, matching wildcard content keys, normalizing string-keyed mappings, and rejecting malformed or non-string-key YAML values.
- [x] Validate other non-JSON request bodies where the local schema stack can do so safely: arbitrary media-type entries with a scalar `string` schema are preserved as opaque body bytes; structured custom codecs remain explicitly rejected instead of being guessed as JSON. APISIX 3.17 delegates body decoding to `resty.openapi_validator`, so adding CBOR/MessagePack or other codecs would be a separate dependency/runtime decision.
- [x] Confirm APISIX 3.17's official plugin is request-only; response validation is not a normal parity task. Treat it as a separate project extension if explicitly requested.
- [x] Add tests for malformed bodies (including XML), Cookie parameters, implemented matrix/label/simple boundaries, and location-specific style rejection.
- [x] Reject parameter styles outside the OpenAPI location matrix, reject `deepObject`/delimited styles when the schema type is incompatible, and reject explicit `deepObject` `explode=false` because OpenAPI fixes that style to `explode=true`; other uncommon nested/explode combinations remain explicitly bounded.
- [x] Reject repeated query occurrences for non-exploded `form` arrays; a non-exploded array must be represented by one comma-delimited parameter value.
- [x] Reject duplicate fields in structured `form`, `simple`, `label`, and `matrix` object values, plus repeated scalar properties in `deepObject` query parameters, instead of silently overwriting or truncating the earlier value.
- [x] Accept free-form exploded `form` query objects backed by `additionalProperties`, including schema-guided coercion for arbitrary property names; duplicate scalar properties remain rejected.
- [x] Support bounded Parameter Object `content` serialization for one `application/json`/`application/*+json` or `text/plain` media type, including schema validation, JSON decode failures, duplicate-value rejection, and explicit unsupported-media errors; custom parameter codecs remain deferred.
- [x] Add a URL-encoded form body validation test, including integer field coercion.
- [x] Add a deterministic test for `spec_url` cache reuse and TTL expiry.
- [x] Add absolute/relative external `$ref` tests, missing-reference rejection, and cyclic-reference rejection tests.
- [x] Add `spaceDelimited` array and `deepObject` query parameter tests.
- [x] Add an exploded `form` object query parameter test.
- [x] Add non-exploded `form` object and malformed-pair rejection tests.
- [x] Add a multipart form body validation test.
- [x] Add a `text/plain` body validation test.
- [x] Add a route-builder chain pair covering OAS validator initialization, valid query forwarding, and required-parameter rejection.

Acceptance: the plugin validates the main APISIX 3.17 request paths without unbounded remote fetches or silent external-ref failures; response validation remains an explicitly separate extension.

### M1.3 `grpc-transcode` and `grpc-web`

- [x] Inventory the supported protobuf descriptor source and HTTP-to-gRPC mapping rules: Go currently accepts base64 `FileDescriptorSet` resources, maps unary JSON bodies and scalar/repeated query fields, and rewrites the request to the configured service/method path. Plain `.proto` text and imported source resolution remain explicit gaps.
- [x] Add descriptor loading/cache behavior and clear invalid-descriptor errors; unchanged descriptor content reuses the parsed service binding.
- [x] Complete the bounded unary path/query/body mapping and gRPC status-to-HTTP error mapping, including `grpc-status-details-bin` decoding with optional `status_detail_type`.
- [x] Map dotted query fields into bounded nested protobuf messages and scalar-valued map fields, and reject unsupported repeated-message query forms explicitly.
- [x] Add plain `.proto` compilation with the pure-Go `github.com/bufbuild/protocompile` dependency and resolve imported source files through the project proto-resource lookup without shelling out to `protoc`.
- [x] Implement `pb_option` enum output selection for `enum_as_name` and `enum_as_value`, with a dynamic-protobuf response test.
- [x] Implement `pb_option` int64 output selection for `int64_as_number`, `int64_as_string`, and `int64_as_hexstring`, including Lua-compatible `#`/`#0x` large-value markers and descriptor-aware nested/list/map handling.
- [x] Implement JSON-visible `auto_default_values`, `use_default_values`, and `no_default_values` behavior for proto3 responses; preserve sparse output when explicitly disabled.
- [x] Accept Lua-compatible `#` decimal/hex int64 inputs in query parameters and JSON bodies, including signed values.
- [x] Cover repeated nested-message mapping in JSON request bodies and reject unsupported dotted query repeated-message forms explicitly.
- [x] Expand bounded repeated-message query forms using contiguous numeric dotted/bracket indexes and repeated JSON-object values; malformed, sparse, and unindexed dotted forms remain rejected.
- [x] Reject client- or server-streaming method descriptors explicitly before request transformation; the unary HTTP-to-gRPC contract no longer silently treats a streaming method as unary.
- [x] Audit the HTTP annotation boundary: the Go route layer exposes explicit `proto_id`/`service`/`method` binding and no annotation source, so descriptor annotations are not inferred; implement a separate contract only if the route model later exposes one.
- [x] Explicitly defer `pb_option` hook execution and exact Lua `use_default_metatable` semantics as Lua-runtime-native; JSON-visible enum/int64/default-value modes remain implemented and tested.
- [x] Add bounded `grpc-web` response streaming through the existing Go `http.Flusher`/request-context boundary: binary chunks pass through immediately, text chunks are encoded per chunk, and the final gRPC-Web trailer is emitted after upstream completion; `grpc-transcode` remains unary-only.
- [x] For `grpc-web`, preserve trailers-only status/message metadata when the upstream sends headers without a body; existing CORS preflight coverage remains in place.
- [x] For `grpc-web`, promote `http.TrailerPrefix` `grpc-status`/`grpc-message` values exposed by the Go reverse proxy, consume their `Trailer` declaration, and encode them into the gRPC-Web trailer chunk; unknown trailer fields remain untouched.
- [x] Cover the bounded CORS behavior, including preserving an existing origin and trailers-only metadata; streaming chunk filters remain deferred.
- [x] Add integration tests with an in-process gRPC server for unary success and gRPC error-to-HTTP mapping; the test exercises the transformed frame through a real dynamic protobuf service.
- [x] Add route-builder chain pairs covering grpc-web request transformation/CORS success and unsupported-method rejection, plus unary grpc-transcode transformation through an explicit store-backed proto resource and missing-resource rejection; streaming remains deferred.
- [x] Audit `grpc-transcode` streaming/cancellation: APISIX 3.17's Go-native contract remains explicit unary `proto_id`/`service`/`method`, and the current route/proxy layer has no protobuf stream owner. Keep streaming descriptors explicitly rejected and defer integration until a separate streaming binding/cancellation contract is approved.

Acceptance: unary transcode and the supported grpc-web request/response paths work end to end; unsupported streaming or descriptor modes are explicit and tested.

### M1.4 `body-transformer` and `data-mask`

- [x] Expand body-transformer template lookup to nested JSON/XML values and bounded array/index access.
- [x] Add XML (including repeated-element indexes), URL-encoded form, and bounded multipart field/file-name extraction for request and response transforms where the content-type boundary is available.
- [x] Preserve repeated `args` and URL-encoded form values with bounded numeric indexes (`name.0`, `name.1`) while retaining the first value at the base key.
- [x] Match APISIX 3.17's template decoding contract: non-`encoded`/`args` formats attempt Base64 decoding and fall back to the original template when it is not valid Base64.
- [x] Support the common APISIX template expressions `..` for bounded string concatenation, `+` for numeric addition, single- and double-quoted string literals, bracket paths such as `items[1]`, raw `{* expr *}` output, and bounded `{% if ... then %}` / `{% elseif ... then %}` / `{% else %}` / `{% end %}` branches with equality, nil, boolean, numeric, and `and`/`or` conditions; loops, arbitrary Lua, and function execution remain deferred.
- [x] Resolve `_ctx.var.<name>` through the shared APISIX/request variable layer so registered values such as `$status` remain available to templates; arbitrary Lua context traversal remains deferred.
- [x] Preserve bounded XML attribute lookup through `_attr.<name>` paths while
  normalizing element and attribute prefixes to local names; full namespace
  URI/prefix fidelity remains deferred.
- [x] Expand data-mask JSONPath support for documented array/nested paths, root-array selectors such as `$[*].token` and `$[0].token`, quoted bracket fields, recursive descent, and both `$.users[*].token` and `users[*].token` forms.
- [x] Match bounded `max_req_post_args` URL-encoded parsing: when the body exceeds the configured limit, mask the parsed prefix instead of silently skipping all fields; zero remains unlimited.
- [x] Expose `$request_line` through the Go logging variable layer so data-mask's query/header/body mutations are visible to downstream logger fields; exact OpenResty log-phase timing remains deferred.
- [x] Match the APISIX 3.17 conditional schema requirements for `body_format`, regex `regex/value`, and replace `value` fields.
- [x] Fail closed on malformed JSON/XML/multipart input and malformed templates with focused rejection tests.
- [x] Remove decoded top-level `_ctx`, `_body`, `_escape_json`, `_escape_xml`, and `_multipart` fields before template rendering so request data cannot shadow reserved helpers.
- [x] Verify the body-size boundary through the existing route/server contract: APISIX 3.17 `body-transformer` has no plugin-level size field, so high-priority `client-control.max_body_size` rejects oversized bodies with 413 before the transformer reads them; keep the plugin itself free of a duplicate limit.
- [x] Add tests for nested values, arrays, content types, malformed templates, and size limits.
- [x] Add route-builder chain pairs covering body-transformer request forwarding/rejection and data-mask query masking/malformed-body rejection.

Acceptance: common JSON/form/XML transformations and masking work without reading unbounded request/response bodies or leaking an unmasked value on an error path.

### M1.5 `chaitin-waf`

- [x] Add a Go-native round-robin picker with bounded failure quarantine.
- [x] Audit the APISIX 3.17 source: `chaitin-waf` has no user-configurable active probe/health-check contract; its picker consumes configured metadata nodes, so do not invent a separate Go probe subsystem for normal plugin parity.
- [x] Expand bounded nested expression/operator support used by the WAF decision request through the shared Go expression layer.
- [x] Forward supported WAF response headers and block response-body decisions through the existing Go response pipeline.
- [x] Add timeout, unhealthy-backend, WAF-deny, and upstream-error tests; the bounded failure quarantine covers the local runtime failure path.
- [x] Add a route-builder chain pair proving configured WAF-node allow and block decisions reach the route response.

Acceptance: a healthy WAF pool can make a decision, unhealthy endpoints are not selected indefinitely, and deny/allow response metadata is preserved.

### M1.6 `traffic-split`

- [x] Map inline/upstream-ID `hash_on` and `key` to a deterministic Go selection path for `chash` with `vars`, `header`, `cookie`, `consumer`, and APISIX-compatible `vars_combinations` templates such as `$request_uri$remote_addr` (including bounded `??` defaults); missing values fall back to remote address.
- [x] Support `pass_host` (`pass`, `node`, `rewrite`) and `upstream_host` for inline and referenced upstreams, including route-level Host-header application and bracketed IPv6 node targets.
- [x] Carry selected upstream connect/send/read timeout settings into a bounded request context; phase-specific transport deadlines remain a Go transport limitation.
- [x] Audit APISIX 3.17's generated upstream contract: `traffic-split.lua` copies nodes, scheme, host mode, key, and timeout but does not propagate `retries`; do not invent per-selected retry behavior for this release.
- [x] Reject invalid referenced `upstream_id` values before evaluating rule matches, matching APISIX's access-phase validation of every configured rule.
- [x] Add passive `checks` behavior through the shared Go proxy abstraction: HTTP-status, TCP-failure, and timeout thresholds quarantine observed failing nodes, and selection fails open when every node is unhealthy; active probes remain explicitly deferred.
- [x] Preserve explicit `weight: 0` semantics when loading referenced `upstream_id` resources while retaining the legacy default weight of 1 when the field is absent; deterministic tests cover hash selection, zero weights, host rewriting, fallback, and selected-upstream timeout propagation; no retry test is required after the upstream contract audit.
- [x] Add a route-builder chain success pair proving the selected inline upstream override reaches the downstream handler; invalid rule/config rejection remains covered by initialization tests.

Acceptance: inline upstream configurations behave consistently with route-defined upstreams and do not bypass existing proxy safety checks.

### M1.7 Smaller high-value HTTP/admin slices

- [x] Add route-builder chain pairs for the core local `limit-req` and `limit-conn` policies, covering same-key success/rejection and concurrent admission rejection.
- [x] Add route-builder chain coverage for `response-rewrite` response mutation, `forward-auth` allow/reject handling, and `proxy-mirror` main-request preservation with asynchronous mirroring.
- [x] Add a route-builder chain pair for `jwt-auth`, covering anonymous-consumer forwarding and missing-token rejection.
- [x] `proxy-buffering`: implement practical Go response flushing for writers that support `http.Flusher`; keep NGINX buffering internals and disk-backed controls explicitly deferred.
- [x] `proxy-control`: map `request_buffering` to the Go route's request-body buffering boundary; APISIX-Runtime/NGINX dynamic controls remain explicitly unsupported.
- [x] Add route-builder chain success coverage for the proxy-buffering flush flag and proxy-control request-buffering flag; invalid boolean configuration remains rejected during route initialization.
- [x] `request-validation`: validate nested header/body schemas during plugin initialization, preserve repeated header values as arrays, and honor `rejected_msg` for decode/read/normalization failures; exact OpenResty header normalization and secret references remain deferred.
- [x] Add a route-builder chain pair covering request-validation required-header forwarding and rejection.
- [x] `public-api`: register local runtime-owned public APIs, including batch requests, node status, server info, and Prometheus metrics; arbitrary internal API discovery remains out of scope.
- [x] `degraphql`: add bounded GraphQL syntax/structure validation, reject malformed queries, and require `operation_name` for multiple operations while preserving GET/POST variable rewriting.
- [x] Add a route-builder chain pair covering degraphql GET rewriting and unsupported-method rejection.
- [x] `error-page`: mark upstream responses in the Go route and restrict rewrites to responses whose provenance is not known to be upstream; exact filter-phase timing remains deferred.
- [x] `exit-transformer`: preserve documented status/normalized-error transformations while skipping known upstream responses using the Go response provenance marker; arbitrary Lua callbacks remain out of scope.
- [x] Add route-builder chain coverage for configured error-page metadata rewriting and exit-transformer status remapping.

Acceptance: each slice has a focused package test and a checklist entry that distinguishes implemented behavior from runtime-native gaps.

## 5. Milestone M2 — protocol bridge designs before implementation

These plugins cannot be completed safely by adding isolated handlers. Each needs a design note and an end-to-end test harness first.

### M2.1 `kafka-proxy`

- [x] Confirm that APISIX 3.17 uses a `scheme: kafka` upstream plus a WebSocket
  PubSub protobuf protocol, not a REST producer/consumer API.
- [x] Define the bounded WebSocket lifecycle, cancellation, and error rules in
  [`apisix-3.17-protocol-bridge-design.md`](apisix-3.17-protocol-bridge-design.md).
- [x] Add a plugin-owned bounded raw Kafka frame bridge and fake-broker tests as
  a compatibility extension; do not count this as official Kafka parity.
- [x] Add the official `PubSubReq`/`PubSubResp` protobuf codec with sequence
  preservation, bounded binary message validation, Kafka command envelopes,
  and response message conversion tests.
- [x] Implement `cmd_kafka_list_offset` and `cmd_kafka_fetch` against a real
  `kafka-go` consumer abstraction with offset/timestamp/key/value conversion;
  broker/TLS integration remains outside the default test suite.
- [x] Bind the official PubSub owner to the `scheme: kafka` route and test
  WebSocket handshake, command responses, malformed requests, and sequence
  preservation with a fake consumer.
- [x] Add owner-level SASL/PLAIN consumer configuration and deterministic
  broker-auth/timeout response mapping tests, including sanitized error
  messages and route/build rejection for missing TLS resource IDs.
- [x] Parse upstream TLS `verify` and inline `client_cert/client_key` fields
  into the Kafka consumer dialer; reject mismatched or invalid certificate
  pairs before serving the route.
- [x] Resolve Kafka `tls.client_cert_id` through the local `ssls` resource
  bucket, load its certificate/key pair, and reject mixed inline/resource
  credentials or malformed IDs before serving the route.
- [x] Add an in-process Kafka wire fixture for TLS + SASL/PLAIN handshake and
  broker authentication-error mapping; it verifies the actual `kafka-go`
  dialer payload without adding an external broker to the default suite.
- [x] Audit the APISIX 3.17 `kafka-proxy` schema: it exposes only SASL
  username/password, so mechanisms beyond PLAIN are outside this release's
  normal parity scope.

### M2.2 `dubbo-proxy` and `http-dubbo`

- [x] Decide on a shared invocation lifecycle with separate fastjson (`http-dubbo`) and hessian2 (`dubbo-proxy`) codecs.
- [x] Define request IDs, multiplex bounds, cancellation, retry safety, timeout, health, and response/error mapping in [`apisix-3.17-protocol-bridge-design.md`](apisix-3.17-protocol-bridge-design.md).
- [x] Complete the existing `http-dubbo` route terminal's bounded connect/write/read deadlines, request-context cancellation, malformed-frame rejection, response-size bound, transport-error mapping, and application-response branches without changing its fastjson wire contract.
- [x] Match APISIX 3.17's fastjson string escaping for invocation parameters, including control characters without Go JSON's HTML escaping.
- [x] Add Hessian2 `Map<String,Object>` request/response serialization, route-terminal integration, response status/header/body mapping, and cancellation-aware per-target in-flight multiplex limiting; persistent shared-connection response matching remains deferred.
- [x] Add bounded connect-only retry for `http-dubbo` from the selected upstream's `retries` setting; never retry after any Dubbo request bytes are written, including read timeout or malformed-response paths.
- [x] Add bounded connect-only retry for `dubbo-proxy` from the selected upstream's `retries` setting; never retry after any Dubbo request bytes are written, including read timeout or malformed-response paths.
- [x] Add passive `checks` integration for `dubbo-proxy` and `http-dubbo` through the shared upstream/proxy abstraction; active probes, persistent health status, and NGINX/Tengine lifecycle remain explicitly deferred.
- [x] Test the bounded `http-dubbo` path for malformed frames, application payload/exception branches, transport errors, read timeout, oversized response payloads, and request cancellation; add Hessian2 map round-trip, route-terminal, provider-exception, static multiplex-limit, and cancellation-aware target-gate coverage for `dubbo-proxy`.

### M2.3 `mqtt-proxy`

- [x] Design the stream/L4 route model before plugin changes, including bounded deadlines and bidirectional cancellation.
- [x] Define bounded MQTT CONNECT preread, byte-for-byte replay, `mqtt_client_id` consistent-hash input, and peer-address fallback in [`apisix-3.17-protocol-bridge-design.md`](apisix-3.17-protocol-bridge-design.md).
- [x] Define stream log-phase ownership and connection shutdown behavior in the design note.
- [x] Implement the bounded MQTT 3.1.1/5.0 CONNECT parser with variable-length validation, protocol/flags/property checks, UTF-8 validation, packet-length reporting, client-ID extraction, and peer-address fallback.
- [x] Add a plugin-owned `ServeStream` boundary that prereads CONNECT, replays all inspected bytes, passes `mqtt_client_id`/peer fallback to a dialer, and owns bidirectional copy/cancellation without involving HTTP handlers.
- [x] Add in-process broker-fixture tests for exact preread replay, client-ID selection, malformed CONNECT rejection before dialing, disconnect, and cancellation.
- [x] Add a plugin-owned `ServeListener` accept loop that owns TCP connection lifetime and publishes `StreamInfo`/errors through a callback; the main server now supplies the TCP listener, route snapshot, weighted upstream dialer, cancellation, and result/log callback.
- [x] Bind the stream owner into the main server's `proxy_mode`/`stream_proxy.tcp` configuration, load `stream_routes` from the store, match `server_addr`/`server_port`/`remote_addr`, resolve `upstream_id`, select weighted TCP upstreams or deterministic `chash` targets keyed by `mqtt_client_id` with peer fallback, and expose MQTT `StreamInfo` through the runtime result/log callback.
- [x] Extend the stream runtime fixture with an explicit bounded-backpressure assertion: a large client write to a non-reading upstream is released by runtime cancellation.

Acceptance for M2: the design and test harness exist before implementation commits; no plugin is marked complete while it only validates schema or stores request context.

## 6. Milestone M3 — project-level secret handling

Do not implement encrypted fields independently in each logger. First establish one secret/reference contract.

- [x] Decide how encrypted values are represented in route, consumer, plugin metadata, and local config; retain APISIX's untagged base64 AES-128-CBC representation and document the compatibility/strict split in [`apisix-3.17-secret-resolution-design.md`](apisix-3.17-secret-resolution-design.md).
- [x] Define key source, rotation behavior, startup failure behavior, and redaction rules in [`apisix-3.17-secret-resolution-design.md`](apisix-3.17-secret-resolution-design.md).
- [x] Add `pkg/data_encryption.Resolver` with strict and compatibility resolution paths plus fixed-value redaction.
- [x] Add tests for valid rotated ciphertext, invalid ciphertext, missing key, encryption-disabled plaintext, and redaction.
- [x] Register all remaining normal-parity secret field paths in the shared store decryption registry; compatibility remains for fields not yet migrated to a plugin boundary.
  - [x] `http-logger.auth_header` (strict plugin-boundary resolution)
  - [x] `kafka-logger` broker password (strict plugin-boundary resolution)
  - [x] `rocketmq-logger.secret_key` (strict plugin-boundary resolution)
  - [x] `clickhouse-logger.password` (strict plugin-boundary resolution)
  - [x] `sls-logger.access_key_secret` (strict plugin-boundary resolution)
  - [x] `google-cloud-logging.auth_config.private_key` (strict plugin-boundary resolution)
  - [x] `splunk-hec-logging.endpoint.token` (strict plugin-boundary resolution)
  - [x] `elasticsearch-logger.auth.password` (strict plugin-boundary resolution)
  - [x] `loggly.customer_token` (strict plugin-boundary resolution)
  - [x] `tencent-cloud-cls.secret_key` (strict plugin-boundary resolution)
  - [x] `lago.token` (strict plugin-boundary resolution)
  - [x] `error-log-logger` metadata fields (strict ClickHouse/Kafka credential resolution)
  - [x] `response-rewrite.body` compatibility field plus explicit `body_secret` strict opt-in registration
  - [x] `kafka-proxy.sasl.password` is registered and migrated to strict `Resolver.Resolve` in the plugin boundary; invalid ciphertext/missing keys fail before request-context propagation.
  - [x] `csrf.key` is registered from the official `encrypt_fields` schema and migrated to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext/missing keys fail before token generation.
- [x] Migrate `http-logger.auth_header` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext/missing keys fail before client creation.
- [x] Migrate `kafka-logger.brokers[*].sasl_config.password` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; rotated ciphertext resolves before the Kafka writer uses the credential and invalid ciphertext fails initialization.
- [x] Migrate `clickhouse-logger.password` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext fails before the HTTP client is created and rotated ciphertext is accepted.
- [x] Migrate `sls-logger.access_key_secret` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext fails before the log sender is configured and rotated ciphertext is accepted.
- [x] Migrate `rocketmq-logger.secret_key` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext fails before producer setup and rotated ciphertext is accepted.
- [x] Migrate `elasticsearch-logger.auth.password` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext fails before the logger batch processor starts and rotated ciphertext is accepted.
- [x] Migrate `loggly.customer_token` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext fails before the logger batch processor starts and rotated ciphertext is accepted.
- [x] Migrate `tencent-cloud-cls.secret_key` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext fails before the shared HTTP client is created and rotated ciphertext is accepted.
- [x] Migrate `lago.token` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext fails before the logger batch processor starts and rotated ciphertext is accepted.
- [x] Migrate `splunk-hec-logging.endpoint.token` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext fails before the HTTP client is created and rotated ciphertext is accepted.
- [x] Migrate `google-cloud-logging.auth_config.private_key` from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext fails before the logger batch processor starts and rotated ciphertext is accepted.
- [x] Migrate `error-log-logger` ClickHouse/Kafka credentials from store compatibility fallback to strict `Resolver.Resolve` in `PostInit`; invalid ciphertext fails before the corresponding sender is created and rotated ciphertext is accepted.
- [x] Migrate every credential-bearing logger boundary with an actionable initialization failure to strict `Resolver.Resolve`; invalid ciphertext and key rotation are covered for each migrated field.
- [x] Define the `response-rewrite.body_secret` Go extension: it accepts an explicitly opted-in APISIX data-encryption ciphertext, resolves it strictly in `PostInit`, rejects invalid/missing-key ciphertext and mixed `body`/`filters` configurations, while ordinary `body` remains compatibility-oriented.

Acceptance: all integrated plugins use the same resolver and redaction rules; no plugin-specific plaintext secret workaround remains.

## 7. Milestone M4 — targeted parity and quality improvements

These tasks should be selected from concrete APISIX compatibility failures, not percentage chasing.

- [x] `real-ip`: broaden common variable-source coverage (`remote_*`, `realip_*`, `cookie_*`, request variables), reject invalid trusted CIDRs at initialization, and enforce valid source port bounds; full NGINX variable catalog and APISIX-Base `set_real_ip` remain deferred.
- [x] `server-info`: use the shared configured/persisted APISIX node ID provider for `/v1/server_info`.
- [x] `server-info`: add periodic etcd reporting and lease keepalive for traditional etcd configuration, using the configured prefix, bounded `report_ttl`, the shared config client, and server shutdown cleanup; data-plane mode remains excluded and dynamic etcd-version lookup remains deferred.
- [x] `attach-consumer-label`: preserve non-string label types by deterministic JSON serialization when the local consumer model provides them.
- [x] `proxy-rewrite`: preserve encoded path segments and raw query values when applying a rewritten URI through the Go reverse proxy; exact OpenResty normalization remains deferred.
- [x] `proxy-cache`: define disk-zone configuration, shared storage lifecycle, versioned disk entries, and explicit fresh/expired/stale semantics in [`apisix-3.17-proxy-cache-design.md`](apisix-3.17-proxy-cache-design.md).
- [x] `proxy-cache`: implement the first disk-zone slice: resolve configured absolute `disk_path`, persist versioned response envelopes atomically, reload entries across plugin instances, persist `Vary` indexes, and purge persisted entries; unconfigured zones retain the compatibility memory fallback.
- [x] `proxy-cache`: remove expired disk entries and their in-memory copies when lookup observes expiration, preventing accessed expired files from accumulating.
- [x] `proxy-cache`: apply the configured disk-zone `disk_size` limit on stores by removing expired files and oldest cache files until the zone is within its bound; malformed persisted entries are removed rather than reused.
- [x] `proxy-cache`: add a bounded traffic-triggered disk expiry sweep (at most once per minute) so unrelated expired files are removed during lookups while the requested expired entry still reports `EXPIRED`.
- [x] `proxy-cache`: add a lifecycle-owned background expiry sweep for configured disk zones and stop it through the route builder's plugin lifecycle; the interval is bounded and the compatibility memory fallback is not changed.
- [x] `proxy-cache`: share configured `memory` zones across plugin instances with a synchronized registry and reference-counted lifecycle; unconfigured zones retain the compatibility per-instance fallback.
- [x] `proxy-cache`: reject duplicate/empty zone names, invalid memory/disk sizes, relative disk paths, and plugin references to undeclared zones when a zone registry is configured.
- [x] `proxy-cache`: validate configured `cache_levels` using the bounded NGINX hierarchy shape (one to three levels, each one or two characters).
- [x] `proxy-cache` + `graphql-proxy-cache`: enforce APISIX's cache strategy/zone storage match (`memory` requires a zone without `disk_path`, `disk` requires one), reject `$request_method` cache keys before cache initialization, and apply unique/method/status/variable-name checks to cache filters.
- [x] Route builder: strict proxy-cache/graphql-proxy-cache initialization errors abort construction of the replacement route handler instead of silently omitting the plugin; existing skip behavior for ordinary plugin initialization errors remains unchanged.
- [x] `proxy-cache` + `graphql-proxy-cache`: share configured memory-zone entries and the versioned disk envelope, including expiry removal, disk cleanup, and plugin-specific purge/key contracts; unconfigured fallbacks remain isolated.
- [x] `proxy-cache`: keep configured memory-zone references alive across route-builder refresh/old-builder stop; [`TestBuilderRefreshKeepsConfiguredProxyCacheZoneAlive`](../pkg/route/builder_lifecycle_test.go) covers the replacement lifecycle.
- [x] `proxy-cache`: isolate configured memory-zone generations when a zone definition changes during refresh, while allowing the old generation to drain independently.
- [x] `proxy-cache`: match strategy-specific cache controls: request `cache_control` freshness/TTL directives apply only to effective memory zones, and disk zones never store `Set-Cookie` even when `cache_set_cookie` is enabled.
- [x] `proxy-cache`: disable `cache_control` request freshness and response TTL overrides when `cache_key` already contains an identity-bearing variable, matching APISIX's per-consumer cache contract.
- [x] `graphql-proxy-cache`: apply the same disk-zone `Set-Cookie` safety boundary while retaining the documented memory-zone opt-in.
- [x] `graphql-proxy-cache`: use upstream `Cache-Control: s-maxage/max-age` or `Expires` as the configured disk-zone TTL, with `cache_ttl` as the fallback; memory strategy TTL remains plugin-configured.
- [x] `graphql-proxy-cache`: honor upstream `Cache-Control: private`, `no-store`, and `no-cache` for both memory and disk strategies, regardless of `cache_set_cookie`.
- [x] `proxy-cache` + `graphql-proxy-cache`: return an RFC-style integer `Age` header on cache hits, including entries loaded from configured disk zones.
- [x] `proxy-cache`: validate the complete static zone registry before route replacement, so an invalid unused zone aborts the new handler build instead of silently leaving a partial cache configuration.
- [x] `proxy-cache` + `graphql-proxy-cache`: audit the cross-plugin stale-request boundary; the official GraphQL schema exposes no `cache_control` request policy, shared entries retain the common expiry envelope, and neither plugin enables implicit stale-if-error serving.
- [x] `proxy-cache`: complete the process-local dynamic zone-registry refresh boundary with `RefreshConfiguredZones`: validate the complete snapshot before atomically publishing it, retain the last valid snapshot after rejection, and let existing plugin instances drain their old memory-zone generation independently; the unconfigured compatibility fallback remains distinct from disk parity.
- [x] `graphql-proxy-cache`: add bounded GraphQL grammar parsing for operation definitions, variables/types, arguments, directives, fragments, aliases, input values, strings, numbers, and trailing-token rejection while preserving current cache-key and purge behavior; full schema/type validation remains out of scope.
- [x] `graphql-limit-count`: reject undefined/cyclic fragments and cover aliases, arguments, and directives in depth parsing.
- [x] `graphql-limit-count`: reject mismatched static group configurations before sharing local quota state, matching the existing `limit-count` group contract.
- [x] `request-id`: add APISIX 3.17 `uuidv7` and `ksuid` algorithms with focused format tests.
- [x] `cors`: reject wildcard CORS options when credentials are enabled and leave `Access-Control-Expose-Headers` unset unless explicitly configured.
- [x] `referer-restriction`: add APISIX `host_def` wildcard/character schema validation with a rejection test.
- [x] `consumer-restriction`: enforce the official HTTP method enum in `allowed_by_methods` and propagate a consumer's `group_id` into `$consumer_group_id` for group-based restrictions.
- [x] `csrf`: register the official `encrypt_fields = ["key"]` contract and resolve the key strictly at plugin initialization, including invalid, missing-key, and rotated-key tests.
- [x] `acl`: preserve numeric, boolean, JSON/segmented-text, and array consumer-label values, and read external-user labels from the local `$external_user` context with bounded dotted-field and `$..field[.suffix]` recursive lookup plus `json`/`segmented_text`/`table` parsers; full OpenResty JSONPath/parser behavior remains deferred.
- [x] `ip-restriction`: match the APISIX `response_code` bounds/default and reject invalid IP/CIDR definitions during plugin initialization; shared OpenResty LRU matcher behavior remains deferred.
- [x] `ua-restriction`: match the APISIX 3.17 `oneOf` schema by requiring exactly one of `allowlist` or `denylist`; preserve the local multi-value header handling and defer exact OpenResty regex/cache fidelity.
- [x] Audit `response-rewrite`, `fault-injection`, and `uri-blocker`: no concrete APISIX-vs-Go mismatch is currently reproducible; retain their documented Go-native behavior and monitor only for a future regression.
- [x] `example-plugin`: expose the official metadata schema and retain the bounded demonstration/control behavior without treating the upstream example as production functionality.

## 8. Explicitly deferred or not required

Do not create ordinary implementation tasks for these unless the project explicitly chooses a new runtime scope:

- `ext-plugin-pre-req`, `ext-plugin-post-req`, `ext-plugin-post-resp`
- `inspect`
- `serverless-pre-function`, `serverless-post-function`
- arbitrary Lua execution, `ngx_lua` APIs, and Lua callback fidelity
- exact OpenResty phase timing and NGINX body-filter timing
- NGINX buffering internals, native connection-state counters, and OCSP/TLS stapling internals
- exact shared-dict/lrucache/batch-manager cross-worker behavior
- exact PCRE/JIT behavior when Go's regular-expression engine has different semantics
- OpenResty cosocket pooling and external plugin-runner protocol compatibility

When one of these differences affects a user-visible behavior, document it in the plugin's README/checklist entry and add a regression test for the chosen Go-native behavior where practical.

## 8.1 Remaining active queue and prerequisites

The following items are the only remaining implementation work that requires a
new runtime or protocol contract. They stay unchecked above until the stated
owner exists; plugin-local helpers must not be mistaken for end-to-end support.

| Priority | Work item | Current evidence / prerequisite | Completion evidence |
|---|---|---|---|
| P1 | General stream-plugin context | `pkg/server` owns configured TCP listeners and MQTT stream routes; Kafka now has a separate HTTP WebSocket owner, while the runtime callback is not yet a general stream-variable/plugin-chain API. | The runtime exposes a documented stream context for every supported stream plugin without HTTP middleware, or each additional stream plugin has its own explicit owner. |
| P2 | Active upstream health probes and health-status API | The shared `pkg/proxy` selector now records passive HTTP/TCP/timeout outcomes for route, traffic-split, Dubbo, and http-dubbo; it intentionally has no background probe scheduler or `/v1/healthcheck` state API. | Add an explicit Go-owned probe lifecycle only if the control plane needs it; otherwise keep APISIX's OpenResty health state documented as deferred. |
| P2 | Kafka external broker smoke fixture | The owner parses upstream `tls.verify`, inline client cert/key, and `client_cert_id` through the local `ssls` resource bucket; it configures `kafka-go` TLS + PLAIN, maps fake auth/timeout failures to sanitized responses, and has an in-process TLS wire fixture that verifies the PLAIN payload plus broker auth failure. An external Kafka environment is intentionally outside the default suite. | If an external integration gate is required, prove successful TLS + SASL/PLAIN metadata/list-offset/fetch against a real broker without leaking credentials. |
| P2 | `grpc-transcode` streaming/cancellation | The route contract is explicit unary `proto_id`/`service`/`method`; streaming descriptors are rejected and no stream response owner exists. | The route model exposes a streaming binding and cancellation contract, then an in-process streaming server fixture proves request cancellation, bounded response forwarding, and error cleanup. |
| P3 | Concrete expression/regex/schema mismatches in `response-rewrite`, `fault-injection`, `uri-blocker` | No reproducible local mismatch is currently recorded. | Add the smallest regression test and patch only after a real APISIX-vs-Go mismatch is reproduced; do not spend percentage-only effort. |

## 9. Completion gates

The APISIX 3.17 parity goal is ready for a release-level review when:

- [x] All Go-scope default plugins are registered and instantiate successfully; `pkg/plugin/TestNewRegistersGoScopeAPISIX317Defaults` covers the 100 registered entries and excludes only the four documented native/deferred defaults.
- [x] Every P0/M1 plugin has at least one end-to-end success test and one rejection/error test; route-applicable pairs are consolidated in `pkg/route/plugin_parity_test.go`, while public API, route-terminal, and plugin-specific configuration/error tests cover specialized boundaries.
- [x] Protocol plugins are not counted as complete while they are schema-only or context-only.
- [x] The checklist separates registration coverage from usable behavior coverage.
- [x] The remaining-TODO document contains no task that is already completed in code; the 2026-07-12 audit moved completed `csrf` encryption work into the implemented notes and retained only concrete gaps or explicitly deferred native/runtime behavior.
- [x] Secret-bearing plugins share the project-level secret resolver, or the limitation is explicitly accepted. All integrated credential-bearing plugin boundaries now use strict `Resolver.Resolve`; ordinary `response-rewrite.body` remains explicitly compatibility-oriented, while `body_secret` is strict.
- [x] `go test ./...`, `make build`, generated-artifact cleanup, and `git diff --check` pass for the current code change set (2026-07-12).
- [x] README links to the current checklist and execution TODO.
- [x] Classify every remaining non-template item as implemented, monitor-only, native/runtime deferred, or separate-subsystem deferred; no normal Go-scope implementation item remains.

Current test-gate audit (2026-07-12): focused success/rejection coverage exists
for the workflow, OAS, gRPC, body-transformer/data-mask, WAF, traffic-split,
request-validation, public-api, GraphQL, error-page/exit-transformer, request
identity, real-IP, server-info, ACL, consumer restriction, CORS, referer, IP
restriction, and example-plugin slices. `proxy-buffering` and `proxy-control`
also now have route-initialization rejection coverage for invalid boolean
configuration. `workflow` now also has a route-builder chain success/rejection
pair, and `oas-validator` now has the same route-builder success/rejection pair
for a required query parameter. Body-transformer/data-mask now have route-chain
success/rejection pairs, while proxy-buffering/proxy-control have route-chain
success coverage plus initialization rejection coverage. Request-validation,
degraphql, and grpc-web now also have route-chain success/rejection pairs;
traffic-split now has a route-chain success/rejection pair. Core local limit-req/limit-conn now also have
route-chain success/rejection pairs, response-rewrite/forward-auth/proxy-mirror
have route-chain coverage, jwt-auth has an anonymous-consumer
success/rejection pair, chaitin-waf has a configured-node allow/block pair,
and unary grpc-transcode now has an explicit proto-resource success/rejection
pair; configured error-page metadata and exit-transformer status remapping also
have route-chain coverage. The P0/M1 success/rejection gate is now closed: the
route-applicable plugins have consolidated route-chain pairs, and specialized
public-API, route-terminal, metadata, configuration-rejection, or
plugin-handler tests cover the remaining boundaries. This gate does not imply
completion of the separate Kafka/MQTT stream subsystems or OpenResty-native
parity items.

## 10. Per-slice checklist

Copy this checklist into an implementation PR description for each plugin or subsystem slice:

The unchecked boxes in this section are a reusable template, not additional
unfinished backlog items; each implementation slice should copy and complete
its own instance before merging.

- [ ] Confirm the APISIX 3.17 source/schema and identify the exact behavior gap.
- [ ] Confirm the local owner: plugin package, route builder, proxy layer, store, or shared utility.
- [ ] Add failing focused tests.
- [ ] Implement the smallest Go-native behavior that closes the gap.
- [ ] Run focused tests and inspect failures rather than widening scope.
- [ ] Run repository tests/build and remove generated artifacts.
- [ ] Update README, parity checklist, remaining-TODO notes, and this execution list.
- [ ] Review the diff for unrelated changes and native-runtime overreach.
- [ ] Record any intentionally deferred behavior and the reason.
