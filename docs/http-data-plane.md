# HTTP Data-Plane Compatibility

APISIX-Go implements one Apache APISIX 3.17 HTTP data-plane contract. Runtime
configuration does not select a compatibility, security, plugin, or evidence
mode.

## Before replacing an APISIX image

1. Validate the existing configuration with
   `apisix config test -c /path/to/config.yaml`.
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

- UDP, stream TLS/mTLS, PROXY protocol, and general stream-plugin chaining.
- OpenResty/Lua internals beyond explicitly bounded implementations.
- External plugin runners, WASM, XRPC, QUIC, HTTP/3, and unsupported discovery.
- Operator-specific external services, ingress, capacity, observability, and
  deployment automation.

## Qualification claim

Plugin behavior is tested by plugin unit tests and standalone real-process
cases. Runtime stability is qualified separately by the
[HTTP candidate qualification](runbooks/http-candidate-qualification.md).

A green release-candidate run supports only this bounded claim:

> The documented HTTP data plane is functionally and runtime-stability verified
> for the recorded source revision, candidate identity, tested plugin behavior, and
> dependency boundaries.

It is not a repository-wide or environment-specific production-readiness claim.
