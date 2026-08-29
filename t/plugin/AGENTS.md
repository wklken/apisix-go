# Plugin Integration Corpus Instructions

This file inherits the repository root `AGENTS.md` and applies to `t/plugin`.
The detailed manifest schema and runner usage remain in `README.md`.

## Evidence ownership

- `corpus_scope.yaml` owns exact upstream source-block accounting and effective
  source commits. `<plugin>.yaml` manifests own executable standalone cases;
  `validation/differential-cases.yaml` owns candidate-vs-oracle
  obligations. Do not create a second profile-specific corpus or status table.
- Plugin-name or config-key presence is not coverage. A claimed case must
  activate and observe the target plugin on its declared route; an intentional
  negative case requires the exact `target_plugin_exempt_reason` contract.
- Map every upstream test number exactly once. Migrate all ledger rows for one
  source file to the same commit in one change; never mix commits for an
  overlapping source block.
- Do not replace an upstream behavior or schema rejection with a weaker 200/404
  stub. Configuration-rejection cases must exercise the rejected path and assert
  both `output.status` and a field-specific initialization log.
- Reused setup blocks may be paired with the request that exercises them. Do
  not let byte-identical placeholder config/fixture/assertion bodies credit
  unrelated source labels or conceal a missing behavior.

## Real-process and differential execution

- Run only one real-process `TestPluginIntegration` command at a time; cases use
  shared resources and fixed ports. Prefer the exact manifest/case selector.
- Missing dependencies, skipped blocks, pending source accounting, mutable
  oracle identities, or stale target commits are evidence failures, not passes.
- Differential artifacts are append-only per attempt and bind the exact source
  commit, oracle digest, catalog digest, and candidate binary SHA-256. Do not
  overwrite a failed attempt or reuse evidence from another binary.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make test-plugin-harness'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./t/plugin -run "^TestPluginIntegration/<plugin>/<case>$" -count=1 -v'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestUpstreamCorpusAccounting|TestCorpusEvidenceMatchesCompatibilityTarget|TestDifferentialCatalogCoverage)$" -count=1'
```
