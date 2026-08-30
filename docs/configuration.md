# Configuration

APISIX-Go reads Apache APISIX static configuration. APISIX-Go-only fields
control local process behavior; they do not select different route or plugin
semantics.

## Load order

Configuration is merged in this order:

1. built-in defaults;
2. `conf/config-default.yaml`;
3. an optional `-c/--config` file;
4. APISIX 3.17 reserved environment overrides.

Lists replace earlier lists. Unknown APISIX fields do not gain runtime
behavior.

`${{NAME}}` and `${{NAME:=fallback}}` expressions are expanded inside each
file before that layer is merged. Absent, null, false, zero, and empty string
remain distinct.

APISIX 3.17 reserves `APISIX_DEPLOYMENT_ETCD_HOST` for replacing
`deployment.etcd.host`. Its value must be a JSON array, for example
`["http://etcd:2379"]`. Invalid JSON is ignored, matching APISIX 3.17. There is
no generic environment-to-field mapping or `--set` configuration layer.

## Runtime paths

Only apisix-go process paths use the `apisix_go` extension:

```yaml
apisix_go:
  runtime_paths:
    data_dir: /usr/local/apisix/data
    runtime_dir: /usr/local/apisix/run
    log_dir: /usr/local/apisix/logs
    temp_dir: /usr/local/apisix/tmp
```

These fields control local files only and do not alter APISIX route or plugin
behavior.

## Validate and inspect

The container reads `/usr/local/apisix/conf/config.yaml` by default.

Use `apisix config test -c /path/to/config.yaml` before restart. See
[HTTP data-plane compatibility](http-data-plane.md) before replacing an
existing APISIX image.
