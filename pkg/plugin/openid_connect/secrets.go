package openid_connect

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"sync"
	"sync/atomic"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

var errOIDCCredentialsUnavailable = secret.ErrCredentialUnavailable

type oidcSecretState struct {
	lifecycleMu sync.Mutex
	activeWork  int
	workDone    chan struct{}
	retired     atomic.Bool
	ready       atomic.Bool

	beforeReadyPublish func()

	preparationMu     sync.Mutex
	preparationCond   *sync.Cond
	preparationActive bool

	credentialMu sync.Mutex
	stopOnce     sync.Once

	scopedClientSecret  secret.Value
	scopedRedisPassword secret.Value
	scopedSet           bool

	clientSecretPresent  bool
	privateKeyPresent    bool
	publicKeyPresent     bool
	sessionSecretPresent bool
	redisPasswordPresent bool
	sessionSecretKey     [sha256.Size]byte
	sessionSecretLength  int
	redisPasswordDigest  [sha256.Size]byte

	activeUses int
	usesDone   chan struct{}
}

type stagedOIDCSecrets struct {
	scopedClientSecret  secret.Value
	scopedRedisPassword secret.Value

	present     [5]bool
	descriptors [5]string
	privateKey  *rsa.PrivateKey
	publicKey   crypto.PublicKey
	sessionKey  [sha256.Size]byte
	sessionLen  int
	digests     [5][sha256.Size]byte
}

type oidcSecretSnapshot struct {
	scopedClientSecret secret.Value
	scopedRedisSecret  secret.Value
	privateKey         *rsa.PrivateKey
	publicKey          crypto.PublicKey
	sessionKey         [sha256.Size]byte

	clientSecretPresent  bool
	privateKeyPresent    bool
	publicKeyPresent     bool
	sessionSecretPresent bool
	redisPasswordPresent bool
}

func (p *Plugin) MaterializeSecrets() error {
	releaseWork, err := p.acquireOIDCWork()
	if err != nil {
		return err
	}
	defer releaseWork()
	p.beginOIDCPreparation()
	defer p.endOIDCPreparation()
	if prepared, err := p.oidcPreparationState(); err != nil || prepared {
		return err
	}

	for _, raw := range p.oidcSecretRaws() {
		if raw != "" {
			return errOIDCCredentialsUnavailable
		}
	}
	return nil
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	releaseWork, err := p.acquireOIDCWork()
	if err != nil {
		return err
	}
	defer releaseWork()
	p.beginOIDCPreparation()
	defer p.endOIDCPreparation()
	if prepared, err := p.oidcPreparationState(); err != nil || prepared {
		return err
	}

	raws := p.oidcSecretRaws()
	fields := [...]string{
		"client_secret",
		"client_rsa_private_key",
		"public_key",
		"session.secret",
		"session.redis.password",
	}
	staged := stagedOIDCSecrets{}
	for index, raw := range raws {
		if raw == "" {
			continue
		}
		value, err := access.Materialize(ctx, fields[index], raw)
		if err != nil {
			return errOIDCCredentialsUnavailable
		}
		descriptor, err := value.Descriptor(capability.SecretPluginConfig)
		if err != nil {
			return errOIDCCredentialsUnavailable
		}
		valid := false
		err = value.Use(func(plaintext string) error {
			if plaintext == "" {
				return errOIDCCredentialsUnavailable
			}
			valid = true
			return staged.derive(index, []byte(plaintext))
		})
		if err != nil || !valid {
			return errOIDCCredentialsUnavailable
		}
		staged.present[index] = true
		staged.descriptors[index] = descriptor.String()
		staged.digests[index] = descriptor.Digest()
		switch index {
		case 0:
			staged.scopedClientSecret = value
		case 4:
			staged.scopedRedisPassword = value
		}
	}
	return p.installOIDCSecrets(staged)
}

func (p *Plugin) oidcSecretRaws() [5]string {
	raws := [5]string{
		p.config.ClientSecret,
		p.config.ClientRSAPrivateKey,
		p.config.PublicKey,
		p.config.Session.Secret,
	}
	if p.config.Session.Redis != nil {
		raws[4] = p.config.Session.Redis.Password
	}
	return raws
}

func (staged *stagedOIDCSecrets) derive(index int, plaintext []byte) error {
	switch index {
	case 1:
		privateKey, err := parseRSAPrivateKey(plaintext)
		if err != nil {
			return err
		}
		staged.privateKey = privateKey
	case 2:
		publicKey, err := parsePublicKey(plaintext)
		if err != nil {
			return err
		}
		staged.publicKey = publicKey
	case 3:
		staged.sessionKey = sha256.Sum256(plaintext)
		staged.sessionLen = len(plaintext)
	}
	return nil
}

func (p *Plugin) beginOIDCPreparation() {
	p.preparationMu.Lock()
	if p.preparationCond == nil {
		p.preparationCond = sync.NewCond(&p.preparationMu)
	}
	for p.preparationActive {
		p.preparationCond.Wait()
	}
	p.preparationActive = true
	p.preparationMu.Unlock()
}

func (p *Plugin) endOIDCPreparation() {
	p.preparationMu.Lock()
	p.preparationActive = false
	p.preparationCond.Broadcast()
	p.preparationMu.Unlock()
}

func (p *Plugin) oidcPreparationState() (bool, error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired.Load() {
		return false, errOIDCCredentialsUnavailable
	}
	return p.scopedSet, nil
}

func (p *Plugin) installOIDCSecrets(staged stagedOIDCSecrets) error {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired.Load() {
		return errOIDCCredentialsUnavailable
	}
	p.scopedClientSecret = staged.scopedClientSecret
	p.scopedRedisPassword = staged.scopedRedisPassword
	p.scopedSet = true
	p.clientSecretPresent = staged.present[0]
	p.privateKeyPresent = staged.present[1]
	p.publicKeyPresent = staged.present[2]
	p.sessionSecretPresent = staged.present[3]
	p.redisPasswordPresent = staged.present[4]
	p.clientRSAPrivateKey = staged.privateKey
	p.staticPublicKey = staged.publicKey
	p.sessionSecretKey = staged.sessionKey
	p.sessionSecretLength = staged.sessionLen
	p.redisPasswordDigest = staged.digests[4]

	if staged.present[0] {
		p.config.ClientSecret = staged.descriptors[0]
	}
	if staged.present[1] {
		p.config.ClientRSAPrivateKey = staged.descriptors[1]
	}
	if staged.present[2] {
		p.config.PublicKey = staged.descriptors[2]
	}
	if staged.present[3] {
		p.config.Session.Secret = staged.descriptors[3]
	}
	if staged.present[4] {
		p.config.Session.Redis.Password = staged.descriptors[4]
	}
	return nil
}

func (p *Plugin) acquireOIDCSecrets() (oidcSecretSnapshot, func(), error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired.Load() || !p.scopedSet {
		return oidcSecretSnapshot{}, nil, errOIDCCredentialsUnavailable
	}
	if p.activeUses == 0 {
		p.usesDone = make(chan struct{})
	}
	p.activeUses++
	return oidcSecretSnapshot{
		scopedClientSecret:   p.scopedClientSecret,
		scopedRedisSecret:    p.scopedRedisPassword,
		privateKey:           p.clientRSAPrivateKey,
		publicKey:            p.staticPublicKey,
		sessionKey:           p.sessionSecretKey,
		clientSecretPresent:  p.clientSecretPresent,
		privateKeyPresent:    p.privateKeyPresent,
		publicKeyPresent:     p.publicKeyPresent,
		sessionSecretPresent: p.sessionSecretPresent,
		redisPasswordPresent: p.redisPasswordPresent,
	}, p.releaseOIDCSecretUse, nil
}

func (p *Plugin) releaseOIDCSecretUse() {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	p.activeUses--
	if p.activeUses == 0 {
		close(p.usesDone)
		p.usesDone = nil
	}
}

func (p *Plugin) withClientSecret(use func(string) error) error {
	if use == nil {
		return errOIDCCredentialsUnavailable
	}
	snapshot, release, err := p.acquireOIDCSecrets()
	if err != nil {
		return err
	}
	defer release()
	if !snapshot.clientSecretPresent {
		return use("")
	}
	return snapshot.scopedClientSecret.Use(use)
}

func (p *Plugin) withRedisPassword(use func(string) error) error {
	if use == nil {
		return errOIDCCredentialsUnavailable
	}
	snapshot, release, err := p.acquireOIDCSecrets()
	if err != nil {
		return err
	}
	defer release()
	if !snapshot.redisPasswordPresent {
		return use("")
	}
	return snapshot.scopedRedisSecret.Use(use)
}

func (p *Plugin) withOIDCPrivateKey(use func(*rsa.PrivateKey) error) error {
	if use == nil {
		return errOIDCCredentialsUnavailable
	}
	snapshot, release, err := p.acquireOIDCSecrets()
	if err != nil {
		return err
	}
	defer release()
	if !snapshot.privateKeyPresent || snapshot.privateKey == nil {
		return errOIDCCredentialsUnavailable
	}
	return use(snapshot.privateKey)
}

func (p *Plugin) withOIDCPublicKey(use func(crypto.PublicKey) error) error {
	if use == nil {
		return errOIDCCredentialsUnavailable
	}
	snapshot, release, err := p.acquireOIDCSecrets()
	if err != nil {
		return err
	}
	defer release()
	if !snapshot.publicKeyPresent || snapshot.publicKey == nil {
		return errOIDCCredentialsUnavailable
	}
	return use(snapshot.publicKey)
}

func (p *Plugin) withOIDCSessionKey(use func([]byte) error) error {
	if use == nil {
		return errOIDCCredentialsUnavailable
	}
	snapshot, release, err := p.acquireOIDCSecrets()
	if err != nil {
		return err
	}
	defer release()
	if !snapshot.sessionSecretPresent {
		return errOIDCCredentialsUnavailable
	}
	key := cloneSessionKey(snapshot.sessionKey)
	defer clear(key)
	return use(key)
}

func (p *Plugin) requirePreparedOIDCSecrets() error {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired.Load() || !p.scopedSet {
		return errOIDCCredentialsUnavailable
	}
	return nil
}

func (p *Plugin) oidcSecretPresence() (bool, bool, bool, bool, bool, int, error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired.Load() || !p.scopedSet {
		return false, false, false, false, false, 0, errOIDCCredentialsUnavailable
	}
	return p.clientSecretPresent, p.privateKeyPresent, p.publicKeyPresent,
		p.sessionSecretPresent, p.redisPasswordPresent, p.sessionSecretLength, nil
}

func cloneSessionKey(key [sha256.Size]byte) []byte {
	return append([]byte(nil), key[:]...)
}

func (p *Plugin) derivedSessionKey() ([]byte, error) {
	p.credentialMu.Lock()
	if p.retired.Load() {
		p.credentialMu.Unlock()
		return nil, errOIDCCredentialsUnavailable
	}
	if !p.scopedSet {
		p.credentialMu.Unlock()
		return nil, errOIDCCredentialsUnavailable
	}
	if !p.sessionSecretPresent {
		p.credentialMu.Unlock()
		return nil, errOIDCCredentialsUnavailable
	}
	defer p.credentialMu.Unlock()
	return cloneSessionKey(p.sessionSecretKey), nil
}

func (p *Plugin) redisPasswordDigestSnapshot() [sha256.Size]byte {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	return p.redisPasswordDigest
}

func (p *Plugin) acquireOIDCWork() (func(), error) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired.Load() {
		return nil, errOIDCCredentialsUnavailable
	}
	if p.activeWork == 0 {
		p.workDone = make(chan struct{})
	}
	p.activeWork++
	return p.releaseOIDCWork, nil
}

func (p *Plugin) acquireReadyOIDCWork() (func(), error) {
	release, err := p.acquireOIDCWork()
	if err != nil {
		return nil, err
	}
	if !p.ready.Load() {
		release()
		return nil, errOIDCCredentialsUnavailable
	}
	return release, nil
}

func (p *Plugin) releaseOIDCWork() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.activeWork--
	if p.activeWork == 0 {
		close(p.workDone)
		p.workDone = nil
	}
}

func (p *Plugin) retireOIDCWork() <-chan struct{} {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.ready.Store(false)
	p.retired.Store(true)
	return p.workDone
}

func (p *Plugin) oidcUsesDone() <-chan struct{} {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	return p.usesDone
}

func (p *Plugin) dropOIDCSecrets() {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	p.scopedClientSecret = secret.Value{}
	p.scopedRedisPassword = secret.Value{}
	p.scopedSet = false
	p.clientSecretPresent = false
	p.privateKeyPresent = false
	p.publicKeyPresent = false
	p.sessionSecretPresent = false
	p.redisPasswordPresent = false
	p.sessionSecretKey = [sha256.Size]byte{}
	p.sessionSecretLength = 0
	p.redisPasswordDigest = [sha256.Size]byte{}
	p.clientRSAPrivateKey = nil
	p.staticPublicKey = nil
}
