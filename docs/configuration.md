# Configuration

APISIX-Go reads Apache APISIX static configuration.

## Load order

Configuration is merged in this order:

1. built-in defaults;
2. `conf/config-default.yaml`;
3. an optional `-c/--config` file;
4. APISIX 3.17 reserved environment overrides.

Lists replace earlier lists. APISIX 3.17 static fields without a Go data-plane
equivalent are ignored and do not gain runtime behavior. Configurations that
actively enable an unsupported admin API, discovery, external plugin, WASM,
XRPC, QUIC, or HTTP/3 subsystem still fail validation.

`${{NAME}}` and `${{NAME:=fallback}}` expressions are expanded inside each
file before that layer is merged. Absent, null, false, zero, and empty string
remain distinct.

APISIX 3.17 reserves `APISIX_DEPLOYMENT_ETCD_HOST` for replacing
`deployment.etcd.host`. Its value must be a JSON array, for example
`["http://etcd:2379"]`. Invalid JSON is ignored, matching APISIX 3.17. There is
no generic environment-to-field mapping or `--set` configuration layer.

## Validate and inspect

The container reads `/usr/local/apisix/conf/config.yaml` by default.

Use `apisix config test -c /path/to/config.yaml` before restart. See
[HTTP data-plane compatibility](http-data-plane.md) before replacing an
existing APISIX image.
