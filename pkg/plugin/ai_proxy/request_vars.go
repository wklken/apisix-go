package ai_proxy

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_stream"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
)

func registerUpstreamTargetVars(r *http.Request, upstream *http.Request) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$upstream_addr", upstream.URL.Host)
	apisixctx.RegisterRequestVar(r, "$upstream_uri", upstream.URL.RequestURI())
	apisixctx.RegisterRequestVar(r, "$upstream_host", upstream.URL.Hostname())
}

func registerUpstreamResponseVars(r *http.Request, status int, elapsed time.Duration, responseLength int64) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$upstream_status", strconv.Itoa(status))
	registerUpstreamResponseTime(r, elapsed)
	apisixctx.RegisterRequestVar(r, "$upstream_response_length", responseLength)
}

func registerUpstreamResponseTime(r *http.Request, elapsed time.Duration) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	apisixctx.RegisterRequestVar(r, "$upstream_response_time", fmt.Sprintf("%.3f", elapsed.Seconds()))
}

func registerStreamingLLMRequestVars(
	r *http.Request,
	requestDocument ai_protocols.Document,
	usage ai_stream.Usage,
) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	ai_stream.RegisterStreamingLLMRequestVars(r, requestDocument, usage)
	apisixctx.RegisterRequestVar(r, "$llm_has_tool_calls", usage.HasToolCalls)
	apisixctx.RegisterRequestVar(r, "$llm_tool_count", usage.ToolCalls)
	registerLLMMetadataDocumentVars(r, requestDocument, ai_protocols.Document{}, usage.Raw)
}

func registerLLMRequestVars(
	r *http.Request,
	requestDocument ai_protocols.Document,
	protocol ai_protocols.Protocol,
	responseDocument ai_protocols.Document,
) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}

	ai_protocols.RegisterLLMRequestVars(r, requestDocument, protocol, responseDocument)
	registerLLMMetadataDocumentVars(r, requestDocument, responseDocument, nil)
}

func registerLLMMetadataVars(r *http.Request, requestBody []byte, responseBody []byte, streamUsage map[string]any) {
	requestDocument, _ := ai_protocols.DecodeDocument(requestBody)
	responseDocument, _ := ai_protocols.DecodeDocument(responseBody)
	registerLLMMetadataDocumentVars(r, requestDocument, responseDocument, streamUsage)
}

func registerLLMMetadataDocumentVars(
	r *http.Request,
	requestDocument ai_protocols.Document,
	responseDocument ai_protocols.Document,
	streamUsage map[string]any,
) {
	if apisixctx.GetRequestVars(r) == nil {
		return
	}
	if requestDocument.Raw != nil {
		endUserID, _ := requestDocument.Raw["user"].(string)
		if endUserID == "" {
			endUserID, _ = requestDocument.Raw["safety_identifier"].(string)
		}
		if endUserID == "" {
			metadata, _ := requestDocument.Raw["metadata"].(map[string]any)
			endUserID, _ = metadata["user_id"].(string)
		}
		if endUserID != "" {
			apisixctx.RegisterRequestVar(r, "$llm_end_user_id", endUserID)
		}
	}

	usage := streamUsage
	if responseDocument.Raw != nil {
		if usage == nil {
			usage, _ = responseDocument.Raw["usage"].(map[string]any)
			if usage == nil {
				response, _ := responseDocument.Raw["response"].(map[string]any)
				usage, _ = response["usage"].(map[string]any)
			}
		}
		toolCalls := responseDocumentToolCalls(responseDocument)
		apisixctx.RegisterRequestVar(r, "$llm_has_tool_calls", toolCalls > 0)
		apisixctx.RegisterRequestVar(r, "$llm_tool_count", toolCalls)
	}

	if usage != nil {
		registerLLMTokenDetailVars(r, usage)
	}
}

func responseDocumentToolCalls(document ai_protocols.Document) int {
	toolCalls := 0
	choices, _ := document.Raw["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		calls, _ := message["tool_calls"].([]any)
		toolCalls += len(calls)
	}
	response, _ := document.Raw["response"].(map[string]any)
	responseOutput, _ := response["output"].([]any)
	toolCalls += responsesOutputToolCalls(responseOutput)
	output, _ := document.Raw["output"].([]any)
	return toolCalls + responsesOutputToolCalls(output)
}

func responsesOutputToolCalls(output []any) int {
	toolCalls := 0
	for _, rawItem := range output {
		item, _ := rawItem.(map[string]any)
		switch item["type"] {
		case "function_call":
			toolCalls++
		case "message":
			if content, ok := item["content"].([]any); ok {
				for _, rawPart := range content {
					part, _ := rawPart.(map[string]any)
					if part["type"] == "function_call" {
						toolCalls++
					}
				}
			}
		}
	}
	return toolCalls
}

func registerLLMTokenDetailVars(r *http.Request, usage map[string]any) {
	var promptDetails map[string]any
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		promptDetails = details
	} else if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		promptDetails = details
	}
	if cached := numericToken(usage["cached_tokens"]); cached > 0 {
		apisixctx.RegisterRequestVar(r, "$llm_cache_read_input_tokens", cached)
	} else if cached := numericToken(usage["cache_read_input_tokens"]); cached > 0 {
		apisixctx.RegisterRequestVar(r, "$llm_cache_read_input_tokens", cached)
	} else if cached := numericToken(promptDetails["cached_tokens"]); cached > 0 {
		apisixctx.RegisterRequestVar(r, "$llm_cache_read_input_tokens", cached)
	}
	if created := numericToken(usage["cache_creation_input_tokens"]); created > 0 {
		apisixctx.RegisterRequestVar(r, "$llm_cache_creation_input_tokens", created)
	} else if created := numericToken(promptDetails["cache_creation_input_tokens"]); created > 0 {
		apisixctx.RegisterRequestVar(r, "$llm_cache_creation_input_tokens", created)
	}
	var completionDetails map[string]any
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		completionDetails = details
	} else if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		completionDetails = details
	}
	if reasoning := numericToken(completionDetails["reasoning_tokens"]); reasoning > 0 {
		apisixctx.RegisterRequestVar(r, "$llm_reasoning_tokens", reasoning)
	}
}

func numericToken(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return 0
	}
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	ai_common.ApplyTransportKeepalive(transport, p.config.KeepalivePool, p.config.KeepaliveTimeout, p.config.Keepalive)
	ai_common.ApplyTransportSSLVerify(transport, p.config.SSLVerify)
	timeout := time.Duration(p.config.Timeout) * time.Millisecond
	transport.DialContext = (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = timeout
	transport.ResponseHeaderTimeout = timeout
	return pxy.NewProgressTimeoutTransport(transport, timeout, timeout)
}
