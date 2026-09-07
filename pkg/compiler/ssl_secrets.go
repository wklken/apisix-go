package compiler

import (
	"context"
	"maps"
	"slices"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/util"
)

func (prepared *PreparedGeneration) materializeFrontendSSLs(ctx context.Context, ssls map[string]resource.SSL) error {
	occurrences := make(map[string]FactoryOccurrence)
	for _, occurrence := range prepared.preparation.Occurrences(capability.SecretSSLConfig) {
		occurrences[occurrence.Resource().ID] = occurrence
	}
	for _, id := range slices.Sorted(maps.Keys(ssls)) {
		ssl := ssls[id]
		if ssl.Status != 1 || (ssl.Type != "" && ssl.Type != "server") {
			continue
		}
		occurrence, ok := occurrences[id]
		if !ok {
			return secret.ErrCredentialUnavailable
		}
		encoded, err := json.Marshal(ssl)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		var document map[string]any
		if err := json.Unmarshal(encoded, &document); err != nil {
			return secret.ErrCredentialUnavailable
		}
		if err := prepared.catalog.TransformDeclaredFields(
			capability.SSLResourceFactory, capability.SecretSSLConfig, document,
			func(declaration capability.SecretDeclaration, _ string, raw any) (any, error) {
				value, ok := raw.(string)
				if !ok {
					return nil, secret.ErrCredentialUnavailable
				}
				materialized, err := prepared.preparation.MaterializeSecret(ctx, occurrence, declaration.Field, value)
				if err != nil {
					return nil, secret.ErrCredentialUnavailable
				}
				var plaintext string
				if err := materialized.Use(func(value string) error { plaintext = value; return nil }); err != nil {
					return nil, secret.ErrCredentialUnavailable
				}
				return plaintext, nil
			},
		); err != nil {
			return secret.ErrCredentialUnavailable
		}
		var materialized resource.SSL
		if err := util.Parse(document, &materialized); err != nil {
			return secret.ErrCredentialUnavailable
		}
		ssl.Cert, ssl.Key = materialized.Cert, materialized.Key
		ssl.Certs, ssl.Keys = materialized.Certs, materialized.Keys
		ssls[id] = ssl
	}
	return nil
}
