# Configuration

apisix-go reads the ordinary Apache APISIX static configuration. There is one
HTTP data-plane behavior; no apisix-go-only selector changes route or plugin
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

## Image replacement

The container starts with `/usr/local/apisix/conf/config.yaml`. Existing APISIX
HTTP data-plane users can keep their configuration, review the documented
incompatibilities, replace the image, and restart. See
[HTTP data-plane compatibility](http-data-plane.md).

Use `apisix config test -c /path/to/config.yaml` before restart and
`apisix config dump --effective --redacted` when inspecting the merged result.
