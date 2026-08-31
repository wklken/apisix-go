package ai_aliyun_content_moderation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

var errAliyunCredentialsUnavailable = errors.New(
	"ai-aliyun-content-moderation credentials are unavailable",
)

// MaterializeScopedSecrets resolves exactly the two catalog-declared Aliyun
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

	p.scopedAccessKeyID = accessKeyID
	p.scopedAccessKeySecret = accessKeySecret
	p.scopedCredentialsSet = true
	p.config.AccessKeyID = idDescriptor.String()
	p.config.AccessKeySecret = secretDescriptor.String()
	return nil
}

func (p *Plugin) credentialsReady() bool {
	p.secretMu.RLock()
	defer p.secretMu.RUnlock()
	return !p.stopped && p.scopedCredentialsSet
}

// useAliyunCredentials keeps secret values available for the complete
// callback, including request signing and transport submission.
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
	return errAliyunCredentialsUnavailable
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
// destroying the generation-scoped credential owners.
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
		p.secretMu.Lock()
		p.stopped = true
		p.scopedAccessKeyID = secret.Value{}
		p.scopedAccessKeySecret = secret.Value{}
		p.scopedCredentialsSet = false
		p.secretMu.Unlock()
	})
}
