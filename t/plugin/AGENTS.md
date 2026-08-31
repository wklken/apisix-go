# Plugin Integration Corpus Instructions

This file inherits the repository root `AGENTS.md` and applies to `t/plugin`.
The detailed manifest schema and runner usage remain in `README.md`.

## Test ownership

- Each `<plugin>.yaml` manifest owns its executable standalone cases and any
  source metadata retained for local traceability. There is no global upstream
  source-block ledger or checked-in oracle suite.
- The integration corpus is scoped to pinned APISIX 3.17.0 behavior. Do not add
  later-version upstream regression inventories to these manifests; keep local
  regressions in the owning plugin unit tests.
- Plugin-name or config-key presence is not coverage. A claimed case must
  activate and observe the target plugin on its declared route; an intentional
  negative case requires the exact `target_plugin_exempt_reason` contract.
- Keep source labels internally consistent within one manifest. They are
  traceability metadata, not a repository-wide coverage claim.
- Do not replace an upstream behavior or schema rejection with a weaker 200/404
  stub. Configuration-rejection cases must exercise the rejected path and assert
  both `output.status` and a field-specific initialization log.
- Reused setup blocks may be paired with the request that exercises them. Do
  not let byte-identical placeholder config/fixture/assertion bodies credit
  unrelated source labels or conceal a missing behavior.

## Real-process execution

- Run only one real-process `TestPluginIntegration` command at a time; cases use
  shared resources and fixed ports. Prefer the exact manifest/case selector.
- Missing dependencies and skipped cases are failures, not passes.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make test-plugin-harness'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./t/plugin -run "^TestPluginIntegration/<plugin>/<case>$" -count=1 -v'
```
