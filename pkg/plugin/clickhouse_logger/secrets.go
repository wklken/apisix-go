package clickhouse_logger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
)

var errClickHouseCredentialUnavailable = errors.New("clickhouse-logger: credential unavailable")

func (p *Plugin) MaterializeSecrets() error {
	resolver := p.DataEncryption()
	if !resolver.Configured() {
		return errors.New("data-encryption resolver is required")
	}

	resolvedPassword, err := resolver.ResolveForContext(
		p.config.Password,
		"clickhouse-logger.password",
	)
	if err != nil {
		return errClickHouseCredentialUnavailable
	}
	passwordSecret, err := store.MaterializeSecret(resolvedPassword)
	if err != nil {
		return errClickHouseCredentialUnavailable
	}
	userSecret, err := materializeSecretReference(p.config.User)
	if err != nil {
		passwordSecret.Destroy()
		return errClickHouseCredentialUnavailable
	}
	userDescriptor, err := legacySecretDescriptor(userSecret)
	if err != nil {
		userSecret.Destroy()
		passwordSecret.Destroy()
		return errClickHouseCredentialUnavailable
	}
	passwordDescriptor, err := legacySecretDescriptor(passwordSecret)
	if err != nil {
		userSecret.Destroy()
		passwordSecret.Destroy()
		return errClickHouseCredentialUnavailable
	}

	oldUserSecret := p.userSecret
	oldPasswordSecret := p.passwordSecret
	p.userSecret = userSecret
	p.passwordSecret = passwordSecret
	if userSecret != nil {
		p.config.User = userDescriptor
	}
	p.config.Password = passwordDescriptor
	p.scopedUser = secret.Value{}
	p.scopedPassword = secret.Value{}
	p.scopedUserSet = false
	p.scopedPasswordSet = false
	oldUserSecret.Destroy()
	oldPasswordSecret.Destroy()
	return nil
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	rawUser := p.config.User
	rawPassword := p.config.Password

	var (
		userValue      secret.Value
		userDescriptor string
		userSet        = rawUser != ""
	)
	if userSet {
		var err error
		userValue, err = access.Materialize(ctx, "user", rawUser)
		if err != nil {
			return errClickHouseCredentialUnavailable
		}
		userDescriptor, err = scopedSecretDescriptor(userValue)
		if err != nil {
			return errClickHouseCredentialUnavailable
		}
	}

	passwordValue, err := access.Materialize(ctx, "password", rawPassword)
	if err != nil {
		return errClickHouseCredentialUnavailable
	}
	passwordDescriptor, err := scopedSecretDescriptor(passwordValue)
	if err != nil {
		return errClickHouseCredentialUnavailable
	}

	p.scopedUser = userValue
	p.scopedPassword = passwordValue
	p.scopedUserSet = userSet
	p.scopedPasswordSet = true
	if userSet {
		p.config.User = userDescriptor
	}
	p.config.Password = passwordDescriptor
	return nil
}

func materializeSecretReference(value string) (*store.ResolvedSecret, error) {
	upper := strings.ToUpper(value)
	if !strings.HasPrefix(upper, "$ENV://") && !strings.HasPrefix(value, "$secret://") {
		return nil, nil
	}
	return store.MaterializeSecret(value)
}

func scopedSecretDescriptor(value secret.Value) (string, error) {
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func legacySecretDescriptor(value *store.ResolvedSecret) (string, error) {
	if value == nil {
		return "", nil
	}
	fingerprint, err := hex.DecodeString(value.Fingerprint())
	if err != nil || len(fingerprint) != 32 {
		return "", errClickHouseCredentialUnavailable
	}
	var digest [32]byte
	copy(digest[:], fingerprint)
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func (p *Plugin) resolvedUser(use func(string) error) error {
	if use == nil {
		return errClickHouseCredentialUnavailable
	}
	if p.scopedUserSet {
		return p.scopedUser.Use(use)
	}
	if p.userSecret != nil {
		return use(string(p.userSecret.Bytes()))
	}
	return use(p.config.User)
}

func (p *Plugin) resolvedPassword(use func(string) error) error {
	if use == nil {
		return errClickHouseCredentialUnavailable
	}
	if p.scopedPasswordSet {
		return p.scopedPassword.Use(use)
	}
	if p.passwordSecret != nil {
		return use(string(p.passwordSecret.Bytes()))
	}
	return use(p.config.Password)
}

func (p *Plugin) userIdentity() string {
	if p.scopedUserSet {
		return mustScopedSecretDescriptor(p.scopedUser)
	}
	if p.userSecret != nil {
		return mustLegacySecretDescriptor(p.userSecret)
	}
	return literalSecretIdentity(p.config.User)
}

func (p *Plugin) passwordIdentity() string {
	if p.scopedPasswordSet {
		return mustScopedSecretDescriptor(p.scopedPassword)
	}
	if p.passwordSecret != nil {
		return mustLegacySecretDescriptor(p.passwordSecret)
	}
	return literalSecretIdentity(p.config.Password)
}

func literalSecretIdentity(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		return ""
	}
	return descriptor.String()
}

func mustScopedSecretDescriptor(value secret.Value) string {
	descriptor, err := scopedSecretDescriptor(value)
	if err != nil {
		return ""
	}
	return descriptor
}

func mustLegacySecretDescriptor(value *store.ResolvedSecret) string {
	descriptor, err := legacySecretDescriptor(value)
	if err != nil {
		return ""
	}
	return descriptor
}
