package ai_protocols

import "fmt"

// ValidateProviderProtocol accepts native protocols and the converters supported
// by APISIX 3.17. Passthrough preserves the catch-all provider path.
func ValidateProviderProtocol(provider string, protocol Protocol) error {
	if protocol == Passthrough {
		return nil
	}
	var supported string
	switch provider {
	case "", "openai", "openai-compatible":
		if protocol == OpenAIChat || protocol == OpenAIEmbeddings || protocol == OpenAIResponses ||
			protocol == AnthropicMessages {
			return nil
		}
		supported = "openai-chat, openai-responses, openai-embeddings"
	case "anthropic":
		if protocol == OpenAIChat || protocol == AnthropicMessages {
			return nil
		}
		supported = "openai-chat, anthropic-messages"
	case "vertex-ai":
		if protocol == OpenAIChat || protocol == AnthropicMessages || protocol == OpenAIEmbeddings {
			return nil
		}
		supported = "openai-chat, vertex-predict"
	case "bedrock":
		if protocol == BedrockConverse {
			return nil
		}
		supported = "bedrock-converse"
	default:
		if protocol == OpenAIChat || protocol == AnthropicMessages && ProviderUsesOpenAIChat(provider) {
			return nil
		}
		supported = "openai-chat"
	}
	return fmt.Errorf(
		"provider %s does not support %s protocol (supported: %s)",
		provider,
		protocol.OverrideKey,
		supported,
	)
}
