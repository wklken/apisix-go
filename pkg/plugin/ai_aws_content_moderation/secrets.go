package ai_aws_content_moderation

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
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

	accessKeyID     *store.ResolvedSecret
	secretAccessKey *store.ResolvedSecret
	sessionToken    *store.ResolvedSecret
	legacySet       bool

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
	awsPreparationLegacy awsPreparationKind = iota
	awsPreparationScoped
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

	accessKeyIDSet     bool
	secretAccessKeySet bool
	sessionTokenSet    bool
	legacySet          bool

	scopedAccessKeyIDSet     bool
	scopedSecretAccessKeySet bool
	scopedSessionTokenSet    bool
	scopedSet                bool
	scopedSessionTokenRawSet bool

	activeUses int
	retired    bool
}

type awsCredentialSnapshot struct {
	legacy bool

	accessKeyID     *store.ResolvedSecret
	secretAccessKey *store.ResolvedSecret
	sessionToken    *store.ResolvedSecret

	scopedAccessKeyID     secret.Value
	scopedSecretAccessKey secret.Value
	scopedSessionToken    secret.Value
	scopedSessionTokenSet bool
}

func (p *Plugin) MaterializeSecrets() error {
	p.beginAWSPreparation(awsPreparationLegacy)
	defer p.endAWSPreparation()
	if prepared, err := p.preparationState(); err != nil || prepared {
		return err
	}

	accessKeyID, err := store.MaterializeSecret(p.config.Comprehend.AccessKeyID)
	if err != nil || !validLegacyAWSSecret(accessKeyID) {
		accessKeyID.Destroy()
		return errAWSCredentialsUnavailable
	}
	secretAccessKey, err := store.MaterializeSecret(p.config.Comprehend.SecretAccessKey)
	if err != nil || !validLegacyAWSSecret(secretAccessKey) {
		accessKeyID.Destroy()
		secretAccessKey.Destroy()
		return errAWSCredentialsUnavailable
	}
	var sessionToken *store.ResolvedSecret
	if p.config.Comprehend.SessionToken != "" {
		sessionToken, err = store.MaterializeSecret(p.config.Comprehend.SessionToken)
		if err != nil || !validLegacyAWSSecret(sessionToken) {
			destroyLegacyAWSCredentials(accessKeyID, secretAccessKey, sessionToken)
			return errAWSCredentialsUnavailable
		}
	}

	accessDescriptor, err := legacyAWSSecretDescriptor(accessKeyID)
	if err != nil {
		destroyLegacyAWSCredentials(accessKeyID, secretAccessKey, sessionToken)
		return errAWSCredentialsUnavailable
	}
	secretDescriptor, err := legacyAWSSecretDescriptor(secretAccessKey)
	if err != nil {
		destroyLegacyAWSCredentials(accessKeyID, secretAccessKey, sessionToken)
		return errAWSCredentialsUnavailable
	}
	var sessionDescriptor string
	if sessionToken != nil {
		sessionDescriptor, err = legacyAWSSecretDescriptor(sessionToken)
		if err != nil {
			destroyLegacyAWSCredentials(accessKeyID, secretAccessKey, sessionToken)
			return errAWSCredentialsUnavailable
		}
	}

	p.credentialMu.Lock()
	if p.retired {
		p.credentialMu.Unlock()
		destroyLegacyAWSCredentials(accessKeyID, secretAccessKey, sessionToken)
		return errAWSCredentialsUnavailable
	}
	p.accessKeyID = accessKeyID
	p.secretAccessKey = secretAccessKey
	p.sessionToken = sessionToken
	p.legacySet = true
	p.config.Comprehend.AccessKeyID = accessDescriptor
	p.config.Comprehend.SecretAccessKey = secretDescriptor
	if sessionToken != nil {
		p.config.Comprehend.SessionToken = sessionDescriptor
	}
	p.credentialMu.Unlock()
	return nil
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
	snapshot.accessKeyIDSet = p.accessKeyID != nil
	snapshot.secretAccessKeySet = p.secretAccessKey != nil
	snapshot.sessionTokenSet = p.sessionToken != nil
	snapshot.legacySet = p.legacySet
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
	return p.legacySet || p.scopedSet, nil
}

func validScopedAWSSecret(value secret.Value) bool {
	valid := false
	_ = value.Use(func(plaintext string) error {
		valid = strings.TrimSpace(plaintext) != ""
		return nil
	})
	return valid
}

func validLegacyAWSSecret(value *store.ResolvedSecret) bool {
	if value == nil {
		return false
	}
	plaintext := value.Bytes()
	defer clear(plaintext)
	return strings.TrimSpace(string(plaintext)) != ""
}

func scopedAWSSecretDescriptor(value secret.Value) (string, error) {
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func legacyAWSSecretDescriptor(value *store.ResolvedSecret) (string, error) {
	if value == nil {
		return "", errAWSCredentialsUnavailable
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

func (p *Plugin) useAWSCredentials(use func(accessKeyID, secretAccessKey, sessionToken string) error) error {
	if use == nil {
		return errAWSCredentialsUnavailable
	}
	snapshot, release, err := p.acquireAWSCredentials()
	if err != nil {
		return err
	}
	defer release()

	if !snapshot.legacy {
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
	accessKeyID := snapshot.accessKeyID.Bytes()
	secretAccessKey := snapshot.secretAccessKey.Bytes()
	var sessionToken []byte
	if snapshot.sessionToken != nil {
		sessionToken = snapshot.sessionToken.Bytes()
	}
	defer clear(accessKeyID)
	defer clear(secretAccessKey)
	defer clear(sessionToken)
	return use(string(accessKeyID), string(secretAccessKey), string(sessionToken))
}

func (p *Plugin) acquireAWSCredentials() (awsCredentialSnapshot, func(), error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired || (!p.scopedSet && !p.legacySet) {
		return awsCredentialSnapshot{}, nil, errAWSCredentialsUnavailable
	}
	if p.activeUses == 0 {
		p.usesDone = make(chan struct{})
	}
	p.activeUses++
	snapshot := awsCredentialSnapshot{
		legacy:                p.legacySet,
		accessKeyID:           p.accessKeyID,
		secretAccessKey:       p.secretAccessKey,
		sessionToken:          p.sessionToken,
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
	accessKeyID := p.accessKeyID
	secretAccessKey := p.secretAccessKey
	sessionToken := p.sessionToken
	p.accessKeyID = nil
	p.secretAccessKey = nil
	p.sessionToken = nil
	p.legacySet = false
	p.scopedAccessKeyID = secret.Value{}
	p.scopedSecretAccessKey = secret.Value{}
	p.scopedSessionToken = secret.Value{}
	p.scopedSet = false
	p.scopedSessionTokenSet = false
	p.credentialMu.Unlock()
	destroyLegacyAWSCredentials(accessKeyID, secretAccessKey, sessionToken)
}

func destroyLegacyAWSCredentials(accessKeyID, secretAccessKey, sessionToken *store.ResolvedSecret) {
	accessKeyID.Destroy()
	secretAccessKey.Destroy()
	sessionToken.Destroy()
}
