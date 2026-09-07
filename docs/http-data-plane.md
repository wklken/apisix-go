# HTTP Data-Plane Compatibility

APISIX-Go implements one Apache APISIX 3.17 HTTP data-plane contract. Runtime
configuration does not select a compatibility, security, plugin, or evidence
mode.

## Before replacing an APISIX image

1. Validate the existing configuration with
   `apisix config test -c /path/to/config.yaml`, then check the compatibility
   exceptions below. Successful configuration admission does not establish
   equivalent behavior for an excluded feature.
2. Replace the image and keep the existing etcd connection and APISIX static
   configuration.
3. Restart the data plane and verify liveness, readiness, and representative
   authenticated routes.

The container reads `/usr/local/apisix/conf/config.yaml` by default. No
APISIX-Go-specific migration selector is required.

## Included scope

- HTTP routes, services, consumers, upstreams, frontend TLS, WebSocket, and
  implemented APISIX 3.17 HTTP plugins.
- Initial provider synchronization, immutable generation activation, graceful
  termination, and serviceability-based readiness.
- The same runtime behavior for unit, integration, and candidate validation.

## Excluded scope

- The APISIX stream subsystem. Existing raw TCP and `mqtt-proxy` support is not
  qualified by this release candidate; UDP, stream TLS/mTLS, PROXY protocol, and
  general stream-plugin chaining remain unimplemented.
- OpenResty/Lua internals beyond explicitly bounded implementations.
- External plugin runners, WASM, XRPC, QUIC, HTTP/3, and unsupported discovery.
- Operator-specific external services, ingress, capacity, observability, and
  deployment automation.

## Accepted compatibility exceptions

These differences are accepted within the bounded HTTP claim; they are not
implemented equivalents or fixed defects. A deployment that relies on one of
them needs a separate migration decision.

| Area | Difference and concrete impact |
| --- | --- |
| PCRE and `data-mask` | Go regular expressions do not support every PCRE expression. An unsupported `data-mask` rule is skipped and can leave the original value, including credentials, in logs. Configuration admission does not prove that masking occurred. Check the expressions and actual log output before relying on masking; see [ADR-0008](architecture/adr/0008-data-mask-regex-boundary.md). Other regex consumers can reject an unsupported pattern at configuration admission. |
| NGINX/OpenResty/Lua-native behavior | Exact native phases, APIs, buffering internals, caches, and a general Lua runtime are excluded beyond explicitly bounded implementations. The serverless plugins do not imply full OpenResty compatibility. |
| GM/NTLS | SSL resource fields and certificate-shape validation do not provide a Tongsuo/NTLS dual-certificate handshake. |
| Frontend cipher suites | Finite-field DHE suites unavailable in Go TLS remain unsupported; see [ADR-0006](architecture/adr/0006-frontend-tls-cipher-boundary.md). |
| `node-status` | Request counters are available, but NGINX `reading`, `writing`, and `waiting` connection-state semantics are not implemented. |
| Kafka `cluster_name` | Logger producers retain their configuration and generation ownership instead of reproducing Lua's process-wide producer cache by numeric cluster name. Configurations relying on that sharing can select different brokers or queues; see [ADR-0007](architecture/adr/0007-kafka-logger-producer-ownership.md). |
| `ai-prompt-template` | A `template_name` supplied as `true` or an object is rejected with 400 instead of reproducing the upstream Lua 500 error path. This exception does not generalize to other status-code differences. |
| `request-validation` Schema dialect | Schema dialect, regex syntax, supported formats, diagnostics, and admission stage are not an exact Lua JSON Schema implementation. For example, a boolean `exclusiveMinimum` without a legacy dialect declaration is rejected during Go schema compilation instead of reaching the Lua request-time error path. External document references are unavailable. These limits do not excuse ignoring a supported constraint: required properties, value constraints, and format checks must still reject invalid requests, including when `$schema` is explicit. |

## Qualification claim

Plugin behavior is tested by plugin unit tests and standalone real-process
cases. Runtime stability is qualified separately by the
[HTTP candidate qualification](runbooks/http-candidate-qualification.md).
The [HTTP functional acceptance index](runbooks/http-functional-acceptance.md)
maps the implemented scope to behavioral tests and defines when a bounded
acceptance stage can close. It is not an exhaustive proof of all plugin
combinations.

A green release-candidate run promotes the recorded revision to release-candidate
status only for the documented Apache APISIX 3.17 HTTP data-plane scope. All
APISIX stream-subsystem behavior is excluded from this qualification.

> The recorded source revision is a release candidate for the documented Apache
> APISIX 3.17 HTTP data-plane scope. The APISIX stream subsystem is excluded, and
> production deployment still requires environment-specific validation.

This qualification does not cover the APISIX stream subsystem, excluded
features, or any specific production environment.
