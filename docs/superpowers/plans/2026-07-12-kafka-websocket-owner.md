# Kafka raw WebSocket compatibility bridge

> This plan is complete only for a bounded raw-frame compatibility extension.
> It is not the APISIX 3.17 Kafka parity implementation. The official follow-up
> is tracked in the execution TODO as the PubSub protobuf owner.

## Goal

Provide a bounded raw length-prefixed Kafka frame bridge over WebSocket for
compatibility clients, while keeping the official PubSub protobuf implementation
as a separate unfinished task.

## Steps

- [x] Confirm that `kafka-proxy` configures a `kafka` upstream and clients use
  WebSocket; the official payload is PubSub protobuf, not raw Kafka frames.
- [x] Add a failing route-owner fixture with a fake Kafka broker and a raw
  masked WebSocket client; assert handshake, exact request/response frame
  forwarding, and non-upgrade rejection.
- [x] Implement the plugin-owned WebSocket handshake/framing bridge and keep
  Kafka frame validation inside `pkg/plugin/kafka_proxy`.
- [x] Branch the route builder on `scheme: kafka`, select the existing
  weighted/health-aware upstream, and pass the request context into the owner.
- [x] Add cancellation, malformed-frame, backend-close, and bounded-message
  error coverage without exposing SASL credentials.
- [x] Update README, checklist, protocol design, remaining TODO, and execution
  TODO to label the raw bridge as a compatibility extension and preserve the
  official PubSub owner as unfinished.
- [x] Run focused/race/full tests, build cleanup, and `git diff --check` via
  `source .envrc`.

## Boundaries

- This compatibility bridge forwards Kafka protocol frames over a WebSocket
  route; it does not implement the official APISIX PubSub protobuf protocol.
- Kafka request/response correlation remains the Kafka client's responsibility;
  the bridge forwards complete length-prefixed frames in order.
- SASL/PLAIN negotiation and the official PubSub owner remain explicit next
  tasks; raw frame forwarding must not be counted as parity.
