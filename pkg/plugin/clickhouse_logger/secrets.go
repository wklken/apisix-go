package clickhouse_logger

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

var errClickHouseCredentialUnavailable = errors.New("clickhouse-logger: credential unavailable")

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

func scopedSecretDescriptor(value secret.Value) (string, error) {
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
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
	return use(p.config.User)
}

func (p *Plugin) resolvedPassword(use func(string) error) error {
	if use == nil {
		return errClickHouseCredentialUnavailable
	}
	if p.scopedPasswordSet {
		return p.scopedPassword.Use(use)
	}
	return use(p.config.Password)
}

func (p *Plugin) userIdentity() string {
	if p.scopedUserSet {
		return mustScopedSecretDescriptor(p.scopedUser)
	}
	return literalSecretIdentity(p.config.User)
}

func (p *Plugin) passwordIdentity() string {
	if p.scopedPasswordSet {
		return mustScopedSecretDescriptor(p.scopedPassword)
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
