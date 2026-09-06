package secret

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
)

func cloudSecretField(value, key string) (string, error) {
	_, field, hasField := strings.Cut(key, "/")
	if !hasField {
		return value, nil
	}
	var document map[string]any
	encoded := []byte(value)
	defer clear(encoded)
	if err := json.Unmarshal(encoded, &document); err != nil {
		return "", ErrCredentialUnavailable
	}
	result, ok := document[field].(string)
	if !ok {
		return "", ErrCredentialUnavailable
	}
	return result, nil
}

func backendEnvironment(ctx context.Context, value string) (string, error) {
	if hasGenerationEnvironmentPrefix(value) {
		return resolveGenerationEnvironmentSecret(ctx, value)
	}
	return value, nil
}

func readCloudSecretResponse(ctx context.Context, client *http.Client, request *http.Request) ([]byte, error) {
	defer func() {
		request.Header.Del("Authorization")
		request.Header.Del("X-Amz-Security-Token")
		request.Body = http.NoBody
		request.GetBody = nil
	}()
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrCredentialUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 || response.StatusCode != http.StatusOK || ctx.Err() != nil {
		clear(body)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrCredentialUnavailable
	}
	return body, nil
}
