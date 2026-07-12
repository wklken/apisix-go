# APISIX 3.17 Secret Resolution Design

> Status: resolver API/field registry implemented; migrated logger credentials and the explicit `response-rewrite.body_secret` extension use strict plugin-boundary resolution, while ordinary `response-rewrite.body` remains compatibility-oriented, 2026-07-12

## Existing contract

APISIX data-encryption values are base64-encoded AES-128-CBC ciphertext. The
configured `apisix.data_encryption.keyring` is ordered newest-first; existing
route/consumer/plugin-metadata parsing already tries every key and replaces a
registered encrypted field with plaintext before the plugin is initialized.

The Go implementation keeps that wire/storage format and does not add a new
ciphertext prefix. This avoids rewriting existing etcd data and keeps rotation
compatible with APISIX.

## Resolver API

`pkg/data_encryption` now exposes:

```go
resolver := data_encryption.NewResolver(enabled, keyring)
plain, err := resolver.Resolve(ciphertext)       // strict encrypted field
plain := resolver.ResolveOptional(value)         // legacy plaintext compatible
redacted := data_encryption.Redact(value)       // fixed "[REDACTED]" marker
```

Strict `Resolve` has explicit failure classes:

| Condition | Result |
|---|---|
| Encryption disabled | Return the configured value unchanged. |
| Encryption enabled with no keyring | `ErrKeyUnavailable`; do not attempt a network call. |
| No key decrypts the value | `ErrInvalidCiphertext`; the error contains no value or key. |
| Any key in the ordered ring decrypts the value | Return plaintext. |
| Empty value | Return empty value without error. |

`ResolveOptional` is retained only for compatibility with existing plaintext
configurations. It preserves the input when strict resolution fails. New
encrypted fields should use strict resolution at the boundary where the caller
can return a configuration error; the store's historical registered-field
decryption remains compatibility-oriented until each plugin is migrated.

## Key source and rotation

- Source: `apisix.data_encryption.keyring` loaded by `pkg/config.Load`.
- Read path: all configured keys are tried newest-first.
- Rotation: add the new key at index 0 and retain old keys until all stored
  values have been rewritten; no old-key deletion is performed automatically.
- Write path: this repository currently does not expose a generic encryption
  API, so plugins never log or persist newly generated ciphertext themselves.
- Startup/runtime failure: a strict resolver error is returned to the owning
  config/route boundary; it must not be downgraded to a network request with an
  empty credential.

## Redaction rules

- Never include plaintext or ciphertext in an error, log line, metric label,
  serialized status response, or `Config.String` implementation.
- Use the fixed `[REDACTED]` value for non-empty secret display fields.
- Keep secret values in local variables only for the duration of the outbound
  request/codec operation.
- Tests must assert both successful use and that diagnostic output does not
  contain the secret.

## Field registry and migration order

The shared `pluginFields` registry now includes the remaining normal-parity
logger and response fields. Store parsing uses the same resolver's optional
compatibility path for fields not yet migrated to a plugin boundary:

1. Kafka logger, RocketMQ/ClickHouse/SLS logger credentials;
2. Google Cloud, Splunk HEC, Elasticsearch, Loggly, Tencent CLS, and Lago
   credentials;
3. `error-log-logger` nested ClickHouse/Kafka credentials;
4. `csrf.key` and `response-rewrite.body` compatibility values plus the strict
   `body_secret` opt-in extension.

`csrf.key`, `http-logger.auth_header`, `kafka-logger.brokers[*].sasl_config.password`,
`clickhouse-logger.password`, `sls-logger.access_key_secret`, and
`rocketmq-logger.secret_key`, `elasticsearch-logger.auth.password`,
`loggly.customer_token`, `tencent-cloud-cls.secret_key`, `lago.token`,
`splunk-hec-logging.endpoint.token`, and
`google-cloud-logging.auth_config.private_key` and `response-rewrite.body_secret`
now stay encrypted through store parsing and are resolved in `PostInit`;
`error-log-logger` applies the same strict resolution to its nested
ClickHouse/Kafka credentials, and `kafka-proxy.sasl.password` is resolved at
its plugin boundary before request-context propagation. Invalid ciphertext or
a missing key prevents the owning client/writer/sender/producer or batch
processor from being created. The migration gate is complete for all
integrated credential-bearing boundaries. Ordinary `response-rewrite.body`
remains the one explicitly compatibility-oriented field because it is a
general-purpose response body rather than an unambiguous credential; callers
that need strict handling must use the `body_secret` extension. Each future
secret-bearing field must add valid-ciphertext, invalid-ciphertext,
missing-key, key-rotation, and redaction tests before changing the
README/checklist percentage.

`response-rewrite.body` remains a compatibility field because it is a
general-purpose response body, not an unambiguous credential. The Go extension
`response-rewrite.body_secret` is the explicit opt-in contract: store parsing
leaves it encrypted, `PostInit` calls strict `Resolver.Resolve`, and invalid or
missing-key ciphertext fails before response handling. `body_secret` cannot be
combined with ordinary `body` or `filters`.
