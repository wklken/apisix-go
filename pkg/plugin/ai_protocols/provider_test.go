package ai_protocols

import (
	"strings"
	"testing"
)

func TestProviderProtocolCapabilities(t *testing.T) {
	for _, provider := range []string{"gemini", "deepseek", "aimlapi", "openrouter", "azure-openai"} {
		for _, protocol := range []Protocol{OpenAIEmbeddings, OpenAIResponses, BedrockConverse} {
			err := ValidateProviderProtocol(provider, protocol)
			if err == nil || !strings.Contains(err.Error(), "(supported: openai-chat)") {
				t.Fatalf("%s/%s error=%v", provider, protocol.OverrideKey, err)
			}
		}
		for _, protocol := range []Protocol{OpenAIChat, AnthropicMessages, Passthrough} {
			if err := ValidateProviderProtocol(provider, protocol); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, test := range []struct {
		provider string
		protocol Protocol
	}{
		{"openai", OpenAIEmbeddings},
		{"openai-compatible", OpenAIResponses},
		{"vertex-ai", OpenAIEmbeddings},
		{"anthropic", AnthropicMessages},
		{"bedrock", BedrockConverse},
	} {
		if err := ValidateProviderProtocol(test.provider, test.protocol); err != nil {
			t.Fatal(err)
		}
	}
}
