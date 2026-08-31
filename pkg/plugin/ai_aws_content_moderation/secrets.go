package ai_aws_content_moderation

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

var errAWSCredentialsUnavailable = errors.New("ai-aws-content-moderation credentials are unavailable")

type awsCredentialState struct {
	preparationMu sync.Mutex
	credentialMu  sync.Mutex

	scopedAccessKeyID     secret.Value
	scopedSecretAccessKey secret.Value
	scopedSessionToken    secret.Value
	scopedSet             bool
	scopedSessionTokenSet bool

	activeUses int
	usesDone   chan struct{}
	retired    bool
}

type awsCredentialSnapshot struct {
	scopedAccessKeyID     secret.Value
	scopedSecretAccessKey secret.Value
	scopedSessionToken    secret.Value
	scopedSessionTokenSet bool
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.preparationMu.Lock()
	defer p.preparationMu.Unlock()
	if prepared, err := p.preparationState(); err != nil || prepared {
		return err
	}

	rawAccessKeyID := p.config.Comprehend.AccessKeyID
	rawSecretAccessKey := p.config.Comprehend.SecretAccessKey
	rawSessionToken := p.config.Comprehend.SessionToken

	accessKeyID, err := access.Materialize(ctx, "comprehend.access_key_id", rawAccessKeyID)
	if err != nil || !validScopedAWSSecret(accessKeyID) {
		return errAWSCredentialsUnavailable
	}
	secretAccessKey, err := access.Materialize(ctx, "comprehend.secret_access_key", rawSecretAccessKey)
	if err != nil || !validScopedAWSSecret(secretAccessKey) {
		return errAWSCredentialsUnavailable
	}
	var sessionToken secret.Value
	sessionTokenSet := rawSessionToken != ""
	if sessionTokenSet {
		sessionToken, err = access.Materialize(ctx, "comprehend.session_token", rawSessionToken)
		if err != nil || !validScopedAWSSecret(sessionToken) {
			return errAWSCredentialsUnavailable
		}
	}

	accessDescriptor, err := scopedAWSSecretDescriptor(accessKeyID)
	if err != nil {
		return errAWSCredentialsUnavailable
	}
	secretDescriptor, err := scopedAWSSecretDescriptor(secretAccessKey)
	if err != nil {
		return errAWSCredentialsUnavailable
	}
	var sessionDescriptor string
	if sessionTokenSet {
		sessionDescriptor, err = scopedAWSSecretDescriptor(sessionToken)
		if err != nil {
			return errAWSCredentialsUnavailable
		}
	}

	p.credentialMu.Lock()
	if p.retired {
		p.credentialMu.Unlock()
		return errAWSCredentialsUnavailable
	}
	p.scopedAccessKeyID = accessKeyID
	p.scopedSecretAccessKey = secretAccessKey
	p.scopedSessionToken = sessionToken
	p.scopedSet = true
	p.scopedSessionTokenSet = sessionTokenSet
	p.config.Comprehend.AccessKeyID = accessDescriptor
	p.config.Comprehend.SecretAccessKey = secretDescriptor
	if sessionTokenSet {
		p.config.Comprehend.SessionToken = sessionDescriptor
	}
	p.credentialMu.Unlock()
	return nil
}

func (p *Plugin) preparationState() (bool, error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired {
		return false, errAWSCredentialsUnavailable
	}
	return p.scopedSet, nil
}

func validScopedAWSSecret(value secret.Value) bool {
	valid := false
	_ = value.Use(func(plaintext string) error {
		valid = strings.TrimSpace(plaintext) != ""
		return nil
	})
	return valid
}

func scopedAWSSecretDescriptor(value secret.Value) (string, error) {
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func (p *Plugin) useAWSCredentials(use func(accessKeyID, secretAccessKey, sessionToken string) error) error {
	if use == nil {
		return errAWSCredentialsUnavailable
	}
	snapshot, release, err := p.acquireAWSCredentials()
	if err != nil {
		return err
	}
	defer release()

	return snapshot.scopedAccessKeyID.Use(func(accessKeyID string) error {
		return snapshot.scopedSecretAccessKey.Use(func(secretAccessKey string) error {
			if !snapshot.scopedSessionTokenSet {
				return use(accessKeyID, secretAccessKey, "")
			}
			return snapshot.scopedSessionToken.Use(func(sessionToken string) error {
				return use(accessKeyID, secretAccessKey, sessionToken)
			})
		})
	})
}

func (p *Plugin) acquireAWSCredentials() (awsCredentialSnapshot, func(), error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired || !p.scopedSet {
		return awsCredentialSnapshot{}, nil, errAWSCredentialsUnavailable
	}
	if p.activeUses == 0 {
		p.usesDone = make(chan struct{})
	}
	p.activeUses++
	snapshot := awsCredentialSnapshot{
		scopedAccessKeyID:     p.scopedAccessKeyID,
		scopedSecretAccessKey: p.scopedSecretAccessKey,
		scopedSessionToken:    p.scopedSessionToken,
		scopedSessionTokenSet: p.scopedSessionTokenSet,
	}
	return snapshot, p.releaseAWSCredentialUse, nil
}

func (p *Plugin) releaseAWSCredentialUse() {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	p.activeUses--
	if p.activeUses == 0 {
		close(p.usesDone)
		p.usesDone = nil
	}
}

func (p *Plugin) Stop() {
	p.credentialMu.Lock()
	p.retired = true
	wait := p.usesDone
	p.credentialMu.Unlock()
	if wait != nil {
		<-wait
	}

	p.credentialMu.Lock()
	p.scopedAccessKeyID = secret.Value{}
	p.scopedSecretAccessKey = secret.Value{}
	p.scopedSessionToken = secret.Value{}
	p.scopedSet = false
	p.scopedSessionTokenSet = false
	p.credentialMu.Unlock()
}
