# Child Plan: ai-proxy Missing Corpus Source

> Owner row of `docs/superpowers/plans/2026-08-02-full-test-nginx-corpus-coverage.md` Task 4.
> Source: `t/plugin/ai-proxy-kafka-log.t` (9 blocks).

## Source Contract Extraction

Upstream harness: `log_level("info")`; test-only `extra_init_by_lua` hook wraps the batch-processor manager to log `send data to kafka: <entry>` for each accepted entry. Observability reproduced in Go via the Kafka fixture record assertions.

| TEST | Title | Contract |
|---|---|---|
| 1, 2 | set route with logging summaries and payloads / send request | Route `/anything`: ai-proxy `provider=openai`, `auth.header.Authorization=Bearer token`, `options{model=gpt-35-turbo-instruct, max_tokens=512, temperature=1.0}`, `override.endpoint=http://127.0.0.1:1980`, `ssl_verify=false`, `logging{summaries=true, payloads=true}`; plus kafka-logger `broker_list {127.0.0.1:9092}`, `kafka_topic=test2`, `key=key1`, `timeout=1`, `batch_max_size=1`. POST chat request with `Authorization: Bearer token` + `X-AI-Fixture: openai/chat-basic.json`; response normal. Kafka entry contains `llm_request`, `llm_summary`, `tool_count`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `reasoning_tokens`, the system prompt text, `gpt-35-turbo-instruct`, `llm_response_text`. |
| 3, 4 | logging summary but no payload | Same route with `payloads=false`; entry contains `llm_summary` + model; must NOT contain `llm_request`/`llm_response_text`. |
| 5, 6 | no logging summary and payload (default) | `summaries=false, payloads=false`; entry must NOT contain `llm_request`/`llm_response_text`/`llm_summary`. |
| 7, 8 | stream=true (SSE) with ai-proxy-multi | **Uses `ai-proxy-multi` plugin** with `instances[0]` provider openai-compatible, `stream: true`, `override.endpoint=http://localhost:7737/v1/chat/completions`, `logging{summaries, payloads}`. SSE body with 6 chunks ending `data: [DONE]`; Kafka entry contains `llm_request`/`llm_summary` + prompt text. **Owned by ai-proxy-multi (Deferred separate subsystem).** |
| 9 | set_logging records every observability field in llm_summary | Lua unit test of `base.set_logging(ctx, true, false)` with fake ctx vars; asserts llm_summary keys `request_model/model/duration/prompt_tokens/completion_tokens/total_tokens/upstream_response_time/stream/tool_count/has_tool_calls/end_user_id/cache_read_input_tokens/cache_creation_input_tokens/reasoning_tokens/content_risk_level`. **Lua-internal unit test; not a real-process surface.** |

## Disposition Plan

- `converted` after evidence: tests 1-6 (6 blocks). The Go `ai_runtime.RegisterLogging` already registers `$llm_summary`/`$llm_request`; kafka-logger must emit them via `log_format` referencing the AI variables (verify field name mapping with the Go kafka-logger `GetFields`).
- `blocked_design` (ai-proxy-multi separate subsystem): tests 7, 8.
- `blocked_runtime` (Lua-internal unit test): test 9.

## Steps

1. Add cases to `t/plugin/ai-proxy.yaml` (which already uses multi-source form):
   - `kafka-log-summaries-and-payloads` (tests [1, 2]) — ai-proxy route + kafka-logger; Kafka fixture asserts records contain `llm_request`, `llm_summary`, `tool_count`, model name, `llm_response_text`, system prompt.
   - `kafka-log-summaries-no-payload` (tests [3, 4]) — assert `llm_summary` present, `llm_request`/`llm_response_text` absent.
   - `kafka-log-no-summary-no-payload` (tests [5, 6]) — assert none of the three keys present.
2. Configure kafka-logger with `log_format` including `$llm_summary`, `$llm_request`, `$llm_response_text`, `$llm_tool_count`, `$llm_cache_read_input_tokens`, `$llm_cache_creation_input_tokens`, `$llm_reasoning_tokens`, `$request_llm_model`, `$llm_model` so the Kafka payload carries the source-observed fields. This maps the upstream "default entry contains AI vars" behavior onto the Go explicit log_format contract.
3. Use the existing OpenAI fixture pattern from `ai-proxy.yaml` for the upstream chat response.
4. Run focused integration RED:
   ```bash
   source .envrc
   go test ./t/plugin -run 'TestPluginIntegration/ai-proxy/(kafka-log-summaries-and-payloads|kafka-log-summaries-no-payload|kafka-log-no-summary-no-payload)$' -count=1 -v
   ```
5. Focused package RED only for confirmed defects (e.g. missing `$llm_tool_count` registration).
6. Run `go test ./pkg/plugin/ai_proxy ./pkg/plugin/ai_runtime ./pkg/plugin/kafka_logger -count=1` + integration GREEN.
7. Update ledger: tests 1-6 `converted`; tests 7-8 `blocked_design`; test 9 `blocked_runtime`; record evidence.
