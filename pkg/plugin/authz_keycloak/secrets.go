package authz_keycloak

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

var errKeycloakCredentialsUnavailable = errors.New("credential unavailable")

type keycloakCredentialState struct {
	preparationMu      sync.Mutex
	preparationCond    *sync.Cond
	preparationActive  bool
	preparationWaiters int

	credentialMu sync.Mutex
	stopOnce     sync.Once

	scopedClientSecret secret.Value
	clientSecretDigest [sha256.Size]byte
	scopedSet          bool

	activeUses int
	usesDone   chan struct{}
	retired    bool
}

type keycloakCredentialSnapshot struct {
	scopedValue secret.Value
}

// MaterializeScopedSecrets resolves only the exact catalog-declared optional
// client_secret field for the current generation attempt.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.beginKeycloakPreparation()
	defer p.endKeycloakPreparation()
	if prepared, err := p.keycloakPreparationState(); err != nil || prepared {
		return err
	}

	raw := p.config.ClientSecret
	if raw == "" {
		return p.installScopedClientSecret(secret.Value{}, sha256.Sum256(nil), "")
	}
	clientSecret, err := access.Materialize(ctx, "client_secret", raw)
	if err != nil || !validScopedClientSecret(clientSecret) {
		return errKeycloakCredentialsUnavailable
	}
	descriptor, err := clientSecret.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return errKeycloakCredentialsUnavailable
	}
	return p.installScopedClientSecret(clientSecret, descriptor.Digest(), descriptor.String())
}

func (p *Plugin) beginKeycloakPreparation() {
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

func (p *Plugin) endKeycloakPreparation() {
	p.preparationMu.Lock()
	p.preparationActive = false
	p.preparationCond.Broadcast()
	p.preparationMu.Unlock()
}

func (p *Plugin) keycloakPreparationState() (bool, error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired {
		return false, errKeycloakCredentialsUnavailable
	}
	return p.scopedSet, nil
}

func (p *Plugin) installScopedClientSecret(
	clientSecret secret.Value, digest [sha256.Size]byte, descriptor string,
) error {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired {
		return errKeycloakCredentialsUnavailable
	}
	p.scopedClientSecret = clientSecret
	p.clientSecretDigest = digest
	p.scopedSet = true
	p.config.ClientSecret = descriptor
	return nil
}

func validScopedClientSecret(value secret.Value) bool {
	valid := false
	_ = value.Use(func(plaintext string) error {
		valid = strings.TrimSpace(plaintext) != ""
		return nil
	})
	return valid
}

func (p *Plugin) withClientSecret(call func(string) error) error {
	if call == nil {
		return errKeycloakCredentialsUnavailable
	}
	snapshot, release, err := p.acquireKeycloakCredential()
	if err != nil {
		return err
	}
	defer release()
	if snapshot.scopedValue == (secret.Value{}) {
		return call("")
	}
	return snapshot.scopedValue.Use(func(plaintext string) error {
		if strings.TrimSpace(plaintext) == "" {
			return errKeycloakCredentialsUnavailable
		}
		return call(plaintext)
	})
}

func (p *Plugin) acquireKeycloakCredential() (keycloakCredentialSnapshot, func(), error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired || !p.scopedSet {
		return keycloakCredentialSnapshot{}, nil, errKeycloakCredentialsUnavailable
	}
	if p.activeUses == 0 {
		p.usesDone = make(chan struct{})
	}
	p.activeUses++
	return keycloakCredentialSnapshot{
		scopedValue: p.scopedClientSecret,
	}, p.releaseKeycloakCredentialUse, nil
}

func (p *Plugin) releaseKeycloakCredentialUse() {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	p.activeUses--
	if p.activeUses == 0 {
		close(p.usesDone)
		p.usesDone = nil
	}
}

func (p *Plugin) clientSecretDigestSnapshot() [sha256.Size]byte {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	return p.clientSecretDigest
}

func (p *Plugin) keycloakHTTPClient() (*http.Client, error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.client == nil {
		return nil, errKeycloakCredentialsUnavailable
	}
	return p.client.GetClient(), nil
}

func (p *Plugin) keycloakRestyClient() (*resty.Client, error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired || p.client == nil {
		return nil, errKeycloakCredentialsUnavailable
	}
	return p.client, nil
}

func (p *Plugin) keycloakRuntimeReady() error {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired || !p.scopedSet || p.client == nil {
		return errKeycloakCredentialsUnavailable
	}
	return nil
}

func (p *Plugin) installKeycloakClient(client *resty.Client, clientRelease func()) error {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired || !p.scopedSet {
		clientRelease()
		return errKeycloakCredentialsUnavailable
	}
	p.client = client
	p.clientRelease = clientRelease
	return nil
}

func (p *Plugin) Stop() {
	p.stopOnce.Do(func() {
		p.credentialMu.Lock()
		p.retired = true
		releaseClient := p.clientRelease
		p.clientRelease = nil
		wait := p.usesDone
		p.credentialMu.Unlock()

		if releaseClient != nil {
			releaseClient()
		}
		if wait != nil {
			<-wait
		}

		p.credentialMu.Lock()
		p.scopedClientSecret = secret.Value{}
		p.clientSecretDigest = [sha256.Size]byte{}
		p.scopedSet = false
		p.client = nil
		p.credentialMu.Unlock()
	})
}
