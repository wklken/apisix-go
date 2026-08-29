# Configuration

APISIX-Go reads Apache APISIX static configuration. APISIX-Go-only fields
control local process behavior; they do not select different route or plugin
semantics.

## Load order

Configuration is merged in this order:

1. built-in defaults;
2. `conf/config-default.yaml`;
3. an optional `-c/--config` file;
4. recognized `APISIXGO_*` environment variables;
5. repeatable `--set path=value` arguments.

Lists replace earlier lists. Unknown APISIX fields are retained only as
provenance and do not gain runtime behavior. Unknown `APISIXGO_*` and `--set`
paths fail closed.

`${{NAME}}` and `${{NAME:=fallback}}` expressions are expanded inside each
file before that layer is merged. Absent, null, false, zero, and empty string
remain distinct, and effective fields retain their source.

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

These fields have matching `APISIXGO_RUNTIME_PATHS_*` environment variables.
They control local files only and do not alter APISIX route or plugin behavior.

## Validate and inspect

The container reads `/usr/local/apisix/conf/config.yaml` by default.

Use `apisix config test -c /path/to/config.yaml` before restart and
`apisix config dump --effective --redacted` when inspecting the merged result.
See [HTTP data-plane compatibility](http-data-plane.md) before replacing an
existing APISIX image.
