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
	preparationMu      sync.Mutex
	preparationCond    *sync.Cond
	preparationActive  bool
	preparationWaiters int
	credentialMu       sync.Mutex
	testHooksMu        sync.Mutex
	testHooks          awsCredentialTestHooks

	scopedAccessKeyID     secret.Value
	scopedSecretAccessKey secret.Value
	scopedSessionToken    secret.Value
	scopedSet             bool
	scopedSessionTokenSet bool

	activeUses int
	usesDone   chan struct{}
	retired    bool
}

type awsPreparationKind uint8

const (
	awsPreparationScoped awsPreparationKind = iota
)

type awsPreparationPhase uint8

const (
	awsPreparationWaiting awsPreparationPhase = iota
	awsPreparationAcquired
)

type awsCredentialLifecycleEvent uint8

const awsCredentialDrainStarted awsCredentialLifecycleEvent = iota

type awsCredentialTestHooks struct {
	preparation func(awsPreparationKind, awsPreparationPhase)
	lifecycle   func(awsCredentialLifecycleEvent)
}

type awsCredentialLifecycleSnapshot struct {
	preparationActive  bool
	preparationWaiters int

	scopedAccessKeyIDSet     bool
	scopedSecretAccessKeySet bool
	scopedSessionTokenSet    bool
	scopedSet                bool
	scopedSessionTokenRawSet bool

	activeUses int
	retired    bool
}

type awsCredentialSnapshot struct {
	scopedAccessKeyID     secret.Value
	scopedSecretAccessKey secret.Value
	scopedSessionToken    secret.Value
	scopedSessionTokenSet bool
}

func (p *Plugin) MaterializeSecrets() error {
	return errAWSCredentialsUnavailable
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.beginAWSPreparation(awsPreparationScoped)
	defer p.endAWSPreparation()
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

func (p *Plugin) beginAWSPreparation(kind awsPreparationKind) {
	p.preparationMu.Lock()
	if p.preparationCond == nil {
		p.preparationCond = sync.NewCond(&p.preparationMu)
	}
	if p.preparationActive {
		p.preparationWaiters++
		p.preparationMu.Unlock()
		p.notifyAWSPreparation(kind, awsPreparationWaiting)
		p.preparationMu.Lock()
		for p.preparationActive {
			p.preparationCond.Wait()
		}
		p.preparationWaiters--
	}
	p.preparationActive = true
	p.preparationMu.Unlock()
	p.notifyAWSPreparation(kind, awsPreparationAcquired)
}

func (p *Plugin) endAWSPreparation() {
	p.preparationMu.Lock()
	p.preparationActive = false
	p.preparationCond.Broadcast()
	p.preparationMu.Unlock()
}

func (p *Plugin) setAWSCredentialTestHooks(hooks awsCredentialTestHooks) {
	p.testHooksMu.Lock()
	p.testHooks = hooks
	p.testHooksMu.Unlock()
}

func (p *Plugin) notifyAWSPreparation(kind awsPreparationKind, phase awsPreparationPhase) {
	p.testHooksMu.Lock()
	hook := p.testHooks.preparation
	p.testHooksMu.Unlock()
	if hook != nil {
		hook(kind, phase)
	}
}

func (p *Plugin) notifyAWSCredentialLifecycle(event awsCredentialLifecycleEvent) {
	p.testHooksMu.Lock()
	hook := p.testHooks.lifecycle
	p.testHooksMu.Unlock()
	if hook != nil {
		hook(event)
	}
}

func (p *Plugin) awsCredentialLifecycleSnapshot() awsCredentialLifecycleSnapshot {
	p.preparationMu.Lock()
	snapshot := awsCredentialLifecycleSnapshot{
		preparationActive:  p.preparationActive,
		preparationWaiters: p.preparationWaiters,
	}
	p.preparationMu.Unlock()

	p.credentialMu.Lock()
	snapshot.scopedAccessKeyIDSet = p.scopedAccessKeyID != (secret.Value{})
	snapshot.scopedSecretAccessKeySet = p.scopedSecretAccessKey != (secret.Value{})
	snapshot.scopedSessionTokenSet = p.scopedSessionToken != (secret.Value{})
	snapshot.scopedSet = p.scopedSet
	snapshot.scopedSessionTokenRawSet = p.scopedSessionTokenSet
	snapshot.activeUses = p.activeUses
	snapshot.retired = p.retired
	p.credentialMu.Unlock()
	return snapshot
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
	p.notifyAWSCredentialLifecycle(awsCredentialDrainStarted)
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
