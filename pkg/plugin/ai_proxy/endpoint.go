package ai_proxy

import (
	"fmt"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"net/url"
)

func (p *Plugin) endpoint(protocol ai_protocols.Protocol, body []byte) (string, error) {
	document, err := ai_protocols.DecodeDocument(body)
	if err != nil {
		return "", err
	}
	return p.endpointDocument(protocol, document)
}

func (p *Plugin) endpointDocument(protocol ai_protocols.Protocol, document ai_protocols.Document) (string, error) {
	if p.config.Override.Endpoint != "" {
		if protocol == ai_protocols.Passthrough {
			return p.config.Override.Endpoint, nil
		}
		if p.config.Provider == "openai" || p.config.Provider == "openai-compatible" {
			return ai_protocols.AppendProtocolEndpoint(p.config.Override.Endpoint, protocol)
		}
		if p.config.Provider == "bedrock" {
			return ai_protocols.AppendBedrockEndpoint(
				p.config.Override.Endpoint,
				p.requestModelDocument(document),
				document.IsStreaming(protocol),
			)
		}
		return p.config.Override.Endpoint, nil
	}

	switch p.config.Provider {
	case "openai":
		return "https://api.openai.com" + protocol.Endpoint, nil
	case "deepseek":
		return "https://api.deepseek.com/chat/completions", nil
	case "aimlapi":
		return "https://api.aimlapi.com/v1/chat/completions", nil
	case "openrouter":
		return "https://openrouter.ai/api/v1/chat/completions", nil
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", nil
	case "anthropic":
		return "https://api.anthropic.com" + protocol.Endpoint, nil
	case "bedrock":
		region, _ := p.config.ProviderConf["region"].(string)
		return ai_protocols.AppendBedrockEndpoint(
			"https://bedrock-runtime."+region+".amazonaws.com",
			p.requestModelDocument(document),
			document.IsStreaming(protocol),
		)
	case "vertex-ai":
		return p.vertexEndpoint(protocol, document)
	default:
		return "", fmt.Errorf("provider %q requires override.endpoint in apisix-go", p.config.Provider)
	}
}

func (p *Plugin) vertexEndpoint(protocol ai_protocols.Protocol, document ai_protocols.Document) (string, error) {
	projectID, _ := p.config.ProviderConf["project_id"].(string)
	region, _ := p.config.ProviderConf["region"].(string)
	if protocol != ai_protocols.OpenAIEmbeddings {
		return fmt.Sprintf(
			"https://%s-aiplatform.googleapis.com/v1beta1/projects/%s/locations/%s/endpoints/openapi/chat/completions",
			region,
			url.PathEscape(projectID),
			url.PathEscape(region),
		), nil
	}
	model := p.requestModelDocument(document)
	if model == "" {
		return "", fmt.Errorf("vertex-ai embeddings requires options.model or request body model")
	}
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
		region,
		url.PathEscape(projectID),
		url.PathEscape(region),
		url.PathEscape(model),
	), nil
}

func (p *Plugin) requestModel(body []byte) string {
	document, err := ai_protocols.DecodeDocument(body)
	if err != nil {
		return ""
	}
	return p.requestModelDocument(document)
}

func (p *Plugin) requestModelDocument(document ai_protocols.Document) string {
	if model, _ := p.config.Options["model"].(string); model != "" {
		return model
	}
	return document.Model()
}
