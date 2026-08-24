package ai_rag

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
)

var errRAGCredentialsUnavailable = errors.New("ai-rag provider credentials are unavailable")

type ragCredentialState struct {
	preparationMu      sync.Mutex
	preparationCond    *sync.Cond
	preparationActive  bool
	preparationWaiters int
	credentialMu       sync.Mutex
	stopOnce           sync.Once

	embeddingAPIKey *store.ResolvedSecret
	searchAPIKey    *store.ResolvedSecret
	legacySet       bool

	scopedEmbeddingAPIKey secret.Value
	scopedSearchAPIKey    secret.Value
	scopedSet             bool

	activeUses int
	usesDone   chan struct{}
	retired    bool
}

type ragCredentialSnapshot struct {
	legacy bool

	embeddingAPIKey *store.ResolvedSecret
	searchAPIKey    *store.ResolvedSecret

	scopedEmbeddingAPIKey secret.Value
	scopedSearchAPIKey    secret.Value
}

// MaterializeSecrets is the transitional process-local preparation path used
// by the current Builder. It remains separate from attempt-scoped resolution.
func (p *Plugin) MaterializeSecrets() error {
	p.beginRAGPreparation()
	defer p.endRAGPreparation()
	if prepared, err := p.ragPreparationState(); err != nil || prepared {
		return err
	}

	embeddingAPIKey, err := store.MaterializeSecret(
		p.config.EmbeddingsProvider.AzureOpenAI.APIKey,
	)
	if err != nil || !validLegacyRAGKey(embeddingAPIKey) {
		embeddingAPIKey.Destroy()
		return errRAGCredentialsUnavailable
	}
	searchAPIKey, err := store.MaterializeSecret(
		p.config.VectorSearchProvider.AzureAISearch.APIKey,
	)
	if err != nil || !validLegacyRAGKey(searchAPIKey) {
		destroyLegacyRAGKeys(embeddingAPIKey, searchAPIKey)
		return errRAGCredentialsUnavailable
	}
	embeddingDescriptor, err := legacyRAGDescriptor(embeddingAPIKey)
	if err != nil {
		destroyLegacyRAGKeys(embeddingAPIKey, searchAPIKey)
		return errRAGCredentialsUnavailable
	}
	searchDescriptor, err := legacyRAGDescriptor(searchAPIKey)
	if err != nil {
		destroyLegacyRAGKeys(embeddingAPIKey, searchAPIKey)
		return errRAGCredentialsUnavailable
	}

	p.credentialMu.Lock()
	if p.retired {
		p.credentialMu.Unlock()
		destroyLegacyRAGKeys(embeddingAPIKey, searchAPIKey)
		return errRAGCredentialsUnavailable
	}
	p.embeddingAPIKey = embeddingAPIKey
	p.searchAPIKey = searchAPIKey
	p.legacySet = true
	p.config.EmbeddingsProvider.AzureOpenAI.APIKey = embeddingDescriptor
	p.config.VectorSearchProvider.AzureAISearch.APIKey = searchDescriptor
	p.credentialMu.Unlock()
	return nil
}

// MaterializeScopedSecrets resolves exactly the two manifest-owned provider
// keys for one attempt. Both values and descriptors are staged before either
// credential becomes visible to provider requests.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.beginRAGPreparation()
	defer p.endRAGPreparation()
	if prepared, err := p.ragPreparationState(); err != nil || prepared {
		return err
	}

	rawEmbeddingAPIKey := p.config.EmbeddingsProvider.AzureOpenAI.APIKey
	rawSearchAPIKey := p.config.VectorSearchProvider.AzureAISearch.APIKey
	embeddingAPIKey, err := access.Materialize(
		ctx, "embeddings_provider.azure_openai.api_key", rawEmbeddingAPIKey,
	)
	if err != nil || !validScopedRAGKey(embeddingAPIKey) {
		return errRAGCredentialsUnavailable
	}
	searchAPIKey, err := access.Materialize(
		ctx, "vector_search_provider.azure_ai_search.api_key", rawSearchAPIKey,
	)
	if err != nil || !validScopedRAGKey(searchAPIKey) {
		return errRAGCredentialsUnavailable
	}
	embeddingDescriptor, err := embeddingAPIKey.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return errRAGCredentialsUnavailable
	}
	searchDescriptor, err := searchAPIKey.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return errRAGCredentialsUnavailable
	}

	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired {
		return errRAGCredentialsUnavailable
	}
	p.scopedEmbeddingAPIKey = embeddingAPIKey
	p.scopedSearchAPIKey = searchAPIKey
	p.scopedSet = true
	p.config.EmbeddingsProvider.AzureOpenAI.APIKey = embeddingDescriptor.String()
	p.config.VectorSearchProvider.AzureAISearch.APIKey = searchDescriptor.String()
	return nil
}

func (p *Plugin) beginRAGPreparation() {
	p.preparationMu.Lock()
	if p.preparationCond == nil {
		p.preparationCond = sync.NewCond(&p.preparationMu)
	}
	if p.preparationActive {
		p.preparationWaiters++
		for p.preparationActive {
			p.preparationCond.Wait()
		}
		p.preparationWaiters--
	}
	p.preparationActive = true
	p.preparationMu.Unlock()
}

func (p *Plugin) endRAGPreparation() {
	p.preparationMu.Lock()
	p.preparationActive = false
	p.preparationCond.Broadcast()
	p.preparationMu.Unlock()
}

func (p *Plugin) ragPreparationState() (bool, error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired {
		return false, errRAGCredentialsUnavailable
	}
	return p.legacySet || p.scopedSet, nil
}

func validScopedRAGKey(value secret.Value) bool {
	valid := false
	_ = value.Use(func(plaintext string) error {
		valid = strings.TrimSpace(plaintext) != ""
		return nil
	})
	return valid
}

func validLegacyRAGKey(value *store.ResolvedSecret) bool {
	if value == nil {
		return false
	}
	plaintext := value.Bytes()
	defer clear(plaintext)
	return strings.TrimSpace(string(plaintext)) != ""
}

func legacyRAGDescriptor(value *store.ResolvedSecret) (string, error) {
	if value == nil {
		return "", errRAGCredentialsUnavailable
	}
	plaintext := value.Bytes()
	defer clear(plaintext)
	descriptor, err := secret.NewDescriptor(
		capability.SecretPluginConfig,
		sha256.Sum256(plaintext),
	)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func (p *Plugin) installRAGClient(client *http.Client) error {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired || (!p.legacySet && !p.scopedSet) {
		client.CloseIdleConnections()
		return errRAGCredentialsUnavailable
	}
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	p.client = client
	return nil
}

func (p *Plugin) withEmbeddingKey(use func(string) error) error {
	return p.withRAGKey(true, use)
}

func (p *Plugin) withSearchKey(use func(string) error) error {
	return p.withRAGKey(false, use)
}

func (p *Plugin) withRAGKey(embedding bool, use func(string) error) error {
	if use == nil {
		return errRAGCredentialsUnavailable
	}
	snapshot, release, err := p.acquireRAGCredentials()
	if err != nil {
		return err
	}
	defer release()

	if !snapshot.legacy {
		value := snapshot.scopedSearchAPIKey
		if embedding {
			value = snapshot.scopedEmbeddingAPIKey
		}
		return value.Use(func(plaintext string) error {
			if strings.TrimSpace(plaintext) == "" {
				return errRAGCredentialsUnavailable
			}
			return use(plaintext)
		})
	}
	value := snapshot.searchAPIKey
	if embedding {
		value = snapshot.embeddingAPIKey
	}
	plaintext := value.Bytes()
	defer clear(plaintext)
	if strings.TrimSpace(string(plaintext)) == "" {
		return errRAGCredentialsUnavailable
	}
	return use(string(plaintext))
}

func (p *Plugin) acquireRAGCredentials() (ragCredentialSnapshot, func(), error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired || (!p.legacySet && !p.scopedSet) || p.client == nil {
		return ragCredentialSnapshot{}, nil, errRAGCredentialsUnavailable
	}
	if p.activeUses == 0 {
		p.usesDone = make(chan struct{})
	}
	p.activeUses++
	return ragCredentialSnapshot{
		legacy:                p.legacySet,
		embeddingAPIKey:       p.embeddingAPIKey,
		searchAPIKey:          p.searchAPIKey,
		scopedEmbeddingAPIKey: p.scopedEmbeddingAPIKey,
		scopedSearchAPIKey:    p.scopedSearchAPIKey,
	}, p.releaseRAGCredentialUse, nil
}

func (p *Plugin) releaseRAGCredentialUse() {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	p.activeUses--
	if p.activeUses == 0 {
		close(p.usesDone)
		p.usesDone = nil
	}
}

func (p *Plugin) Stop() {
	p.stopOnce.Do(func() {
		p.credentialMu.Lock()
		p.retired = true
		client := p.client
		wait := p.usesDone
		p.credentialMu.Unlock()
		if client != nil {
			client.CloseIdleConnections()
		}
		if wait != nil {
			<-wait
		}

		p.credentialMu.Lock()
		embeddingAPIKey := p.embeddingAPIKey
		searchAPIKey := p.searchAPIKey
		p.embeddingAPIKey = nil
		p.searchAPIKey = nil
		p.legacySet = false
		p.scopedEmbeddingAPIKey = secret.Value{}
		p.scopedSearchAPIKey = secret.Value{}
		p.scopedSet = false
		p.client = nil
		p.credentialMu.Unlock()
		destroyLegacyRAGKeys(embeddingAPIKey, searchAPIKey)
	})
}

func destroyLegacyRAGKeys(embeddingAPIKey, searchAPIKey *store.ResolvedSecret) {
	embeddingAPIKey.Destroy()
	searchAPIKey.Destroy()
}
