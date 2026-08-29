# Apache APISIX HTTP data-plane compatibility

apisix-go exposes one runtime behavior: the Apache APISIX 3.17 HTTP data-plane
contract implemented by this repository. It has no apisix-go-only behavior
selector.

## Replacing an APISIX image

1. Review the incompatibilities in the generated [plugin status](plugins.md).
2. Keep the existing Apache APISIX configuration and data-plane connection to
   etcd.
3. Replace the container image with the apisix-go image.
4. Restart the data plane and run the normal route probes.

No apisix-go-specific migration setting is required. The image starts with
`/usr/local/apisix/conf/config.yaml`, the same configuration path used for a
normal image replacement.

## Scope

The supported user-facing contract is the ordinary HTTP data plane. Stream
features, OpenResty/Lua internals, external plugin runners, native NGINX/TLS
features, and other explicit gaps remain listed in [plugin status](plugins.md)
and the [design document](design.md).

The differential suite under `validation/` is developer evidence only. It
does not select or alter runtime behavior.

## Functional and stability verification

The current release milestone has two evidence groups:

1. HTTP functionality: the APISIX 3.17 differential corpus remains green for
   the resolved source commit and candidate-binary SHA-256, followed by
   real-process assembly smoke for representative authentication, rewrite,
   proxy-control, blocking, and standalone-provider paths.
2. Runtime stability: non-root container startup, an in-flight request that
   completes during graceful TERM, verified-TLS etcd last-good/recovery,
   compaction recovery, replica restart, live delete/re-add convergence, and
   the canonical 30-minute proxy soak all pass for the same resolved revision.

After a post-merge release-candidate run records those gates, the permitted
claim is: **the documented HTTP data plane is functionally and
runtime-stability verified within its plugin and dependency boundaries.** This
is not a repository-wide production-ready claim and does not verify stream
data-plane behavior.

Upgrade/rollback, registry publication, signing, release packaging, and
environment-specific Kubernetes/systemd, ingress, capacity, and observability
acceptance are separate future work. They do not block this bounded functional
and stability result. Real external services remain conditional boundaries for
the plugins that use them; local fixture parity does not verify an operator's
Kafka, Redis, cloud-provider, or logging environment.
