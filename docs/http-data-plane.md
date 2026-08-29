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
