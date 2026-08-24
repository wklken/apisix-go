package ai_aliyun_content_moderation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
)

var errAliyunCredentialsUnavailable = errors.New(
	"ai-aliyun-content-moderation credentials are unavailable",
)

// MaterializeSecrets is the transitional process-local preparation path. It
// keeps resolved credentials private and publishes content-only descriptors.
func (p *Plugin) MaterializeSecrets() error {
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.stopped {
		return errAliyunCredentialsUnavailable
	}
	if p.scopedCredentialsSet || (p.accessKeyID != nil && p.accessKeySecret != nil) {
		return nil
	}

	accessKeyID, err := store.MaterializeSecret(p.config.AccessKeyID)
	if err != nil {
		return errAliyunCredentialsUnavailable
	}
	accessKeySecret, err := store.MaterializeSecret(p.config.AccessKeySecret)
	if err != nil {
		accessKeyID.Destroy()
		return errAliyunCredentialsUnavailable
	}
	idDescriptor, err := legacyAliyunDescriptor(accessKeyID)
	if err != nil {
		accessKeyID.Destroy()
		accessKeySecret.Destroy()
		return errAliyunCredentialsUnavailable
	}
	secretDescriptor, err := legacyAliyunDescriptor(accessKeySecret)
	if err != nil {
		accessKeyID.Destroy()
		accessKeySecret.Destroy()
		return errAliyunCredentialsUnavailable
	}

	oldAccessKeyID := p.accessKeyID
	oldAccessKeySecret := p.accessKeySecret
	p.accessKeyID = accessKeyID
	p.accessKeySecret = accessKeySecret
	p.scopedAccessKeyID = secret.Value{}
	p.scopedAccessKeySecret = secret.Value{}
	p.scopedCredentialsSet = false
	p.config.AccessKeyID = idDescriptor
	p.config.AccessKeySecret = secretDescriptor
	oldAccessKeyID.Destroy()
	oldAccessKeySecret.Destroy()
	return nil
}

// MaterializeScopedSecrets resolves exactly the two manifest-owned Aliyun
// credentials for one immutable attempt. Values and descriptors are staged
// before either value is installed, so a failure cannot publish partial state.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.stopped {
		return errAliyunCredentialsUnavailable
	}
	if p.scopedCredentialsSet {
		return nil
	}

	rawAccessKeyID := p.config.AccessKeyID
	rawAccessKeySecret := p.config.AccessKeySecret
	accessKeyID, err := access.Materialize(ctx, "access_key_id", rawAccessKeyID)
	if err != nil {
		return errAliyunCredentialsUnavailable
	}
	idDescriptor, err := accessKeyID.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return errAliyunCredentialsUnavailable
	}
	accessKeySecret, err := access.Materialize(ctx, "access_key_secret", rawAccessKeySecret)
	if err != nil {
		return errAliyunCredentialsUnavailable
	}
	secretDescriptor, err := accessKeySecret.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return errAliyunCredentialsUnavailable
	}

	oldAccessKeyID := p.accessKeyID
	oldAccessKeySecret := p.accessKeySecret
	p.accessKeyID = nil
	p.accessKeySecret = nil
	p.scopedAccessKeyID = accessKeyID
	p.scopedAccessKeySecret = accessKeySecret
	p.scopedCredentialsSet = true
	p.config.AccessKeyID = idDescriptor.String()
	p.config.AccessKeySecret = secretDescriptor.String()
	oldAccessKeyID.Destroy()
	oldAccessKeySecret.Destroy()
	return nil
}

func legacyAliyunDescriptor(value *store.ResolvedSecret) (string, error) {
	if value == nil {
		return "", errAliyunCredentialsUnavailable
	}
	plaintext := value.Bytes()
	defer clear(plaintext)
	digest := sha256.Sum256(plaintext)
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func (p *Plugin) credentialsReady() bool {
	p.secretMu.RLock()
	defer p.secretMu.RUnlock()
	return !p.stopped && (p.scopedCredentialsSet ||
		(p.accessKeyID != nil && p.accessKeySecret != nil))
}

// useAliyunCredentials keeps secret values available for the complete
// callback, including request signing and transport submission. Legacy bytes
// are cloned by ResolvedSecret.Bytes and cleared after the callback returns.
func (p *Plugin) useAliyunCredentials(use func(id, accessKeySecret string) error) error {
	if use == nil {
		return errAliyunCredentialsUnavailable
	}
	p.secretMu.RLock()
	defer p.secretMu.RUnlock()
	if p.stopped {
		return errAliyunCredentialsUnavailable
	}
	if p.scopedCredentialsSet {
		return p.scopedAccessKeyID.Use(func(accessKeyID string) error {
			return p.scopedAccessKeySecret.Use(func(accessKeySecret string) error {
				if accessKeyID == "" || accessKeySecret == "" {
					return errAliyunCredentialsUnavailable
				}
				return use(accessKeyID, accessKeySecret)
			})
		})
	}
	if p.accessKeyID == nil || p.accessKeySecret == nil {
		return errAliyunCredentialsUnavailable
	}
	accessKeyID := p.accessKeyID.Bytes()
	accessKeySecret := p.accessKeySecret.Bytes()
	defer clear(accessKeyID)
	defer clear(accessKeySecret)
	if len(accessKeyID) == 0 || len(accessKeySecret) == 0 {
		return errAliyunCredentialsUnavailable
	}
	return use(string(accessKeyID), string(accessKeySecret))
}

func (p *Plugin) buildFormBodyWithCredentials(
	accessKeyID, accessKeySecret, sessionID, content, serviceName string,
) ([]byte, error) {
	serviceParameters, err := json.Marshal(serviceParameters{SessionID: sessionID, Content: content})
	if err != nil {
		return nil, fmt.Errorf("failed to encode service parameters: %w", err)
	}
	params := map[string]string{
		"AccessKeyId":       accessKeyID,
		"Action":            "TextModerationPlus",
		"Format":            "JSON",
		"RegionId":          p.config.RegionID,
		"Service":           serviceName,
		"ServiceParameters": string(serviceParameters),
		"SignatureMethod":   "HMAC-SHA1",
		"SignatureNonce":    p.nonce(),
		"SignatureVersion":  "1.0",
		"Timestamp":         p.now().UTC().Format("2006-01-02T15:04:05Z"),
		"Version":           "2022-03-02",
	}
	params["Signature"] = aliyunSignature(params, accessKeySecret+"&")

	keys := sortedKeys(params)
	values := make(url.Values, len(params))
	for _, key := range keys {
		values.Set(key, params[key])
	}
	return []byte(values.Encode()), nil
}

// sendModerationRequest signs and submits one request while the credential
// callback owns the plaintext. Stop therefore waits for this boundary before
// destroying either legacy or scoped credential owners.
func (p *Plugin) sendModerationRequest(
	ctx context.Context, sessionID, content, serviceName string,
) (int, []byte, error) {
	var (
		statusCode   int
		responseBody []byte
	)
	err := p.useAliyunCredentials(func(accessKeyID, accessKeySecret string) error {
		formBytes, err := p.buildFormBodyWithCredentials(
			accessKeyID, accessKeySecret, sessionID, content, serviceName,
		)
		if err != nil {
			return err
		}
		defer clear(formBytes)
		req, err := http.NewRequestWithContext(
			ctx, http.MethodPost, p.config.Endpoint, bytes.NewReader(formBytes),
		)
		if err != nil {
			return errors.New("failed to create Aliyun moderation request")
		}
		// Do not retain a replay closure containing the signed form beyond the
		// credential callback's request lifetime.
		req.GetBody = nil
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if p.client == nil {
			return errors.New("aliyun moderation client is unavailable")
		}
		response, err := p.client.Do(req)
		if err != nil {
			return err
		}
		statusCode = response.StatusCode
		defer func() { _ = response.Body.Close() }()
		responseBody, err = io.ReadAll(response.Body)
		return err
	})
	return statusCode, responseBody, err
}

func (p *Plugin) Stop() {
	p.stopOnce.Do(func() {
		if p.stopStarted != nil {
			p.stopStarted()
		}
		p.secretMu.Lock()
		p.stopped = true
		accessKeyID := p.accessKeyID
		accessKeySecret := p.accessKeySecret
		p.accessKeyID = nil
		p.accessKeySecret = nil
		p.scopedAccessKeyID = secret.Value{}
		p.scopedAccessKeySecret = secret.Value{}
		p.scopedCredentialsSet = false
		p.secretMu.Unlock()
		accessKeyID.Destroy()
		accessKeySecret.Destroy()
	})
}
