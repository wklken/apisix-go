package data_encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"strings"
	"testing"
)

func TestDecryptPluginConfigsUsesKeyringAndNestedFields(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"openid-connect": map[string]any{
			"client_secret": encryptForTest(t, key, "client-secret"),
			"session": map[string]any{
				"redis": map[string]any{"password": encryptForTest(t, key, "redis-password")},
			},
		},
	}

	DecryptPluginConfigs(configs, []string{"old-keyring-item", key}, mustTestDeclarationCatalog())
	oidc := configs["openid-connect"].(map[string]any)
	if got := oidc["client_secret"]; got != "client-secret" {
		t.Fatalf("client_secret = %v, want plaintext", got)
	}
	if got := oidc["session"].(map[string]any)["redis"].(map[string]any)["password"]; got != "redis-password" {
		t.Fatalf("session.redis.password = %v, want plaintext", got)
	}
}

func TestDecryptPluginConfigWithResolverLookupResults(t *testing.T) {
	key := "qeddd145sfvddff3"
	resolver := NewResolver(true, []string{key})

	t.Run("registered plugin fields decrypted", func(t *testing.T) {
		configs := map[string]any{
			"key-auth":   map[string]any{"key": encryptForTest(t, key, "api-secret")},
			"basic-auth": map[string]any{"password": encryptForTest(t, key, "pw")},
		}
		for name, config := range configs {
			DecryptPluginConfigWithResolver(config, name, resolver, mustTestDeclarationCatalog())
		}
		if got := configs["key-auth"].(map[string]any)["key"]; got != "api-secret" {
			t.Fatalf("key-auth.key = %v, want decrypted", got)
		}
		if got := configs["basic-auth"].(map[string]any)["password"]; got != "pw" {
			t.Fatalf("basic-auth.password = %v, want decrypted", got)
		}
	})

	t.Run("unknown plugin left untouched", func(t *testing.T) {
		ciphertext := encryptForTest(t, key, "secret")
		config := map[string]any{"token": ciphertext}
		DecryptPluginConfigWithResolver(
			config,
			"not-a-plugin",
			resolver,
			mustTestDeclarationCatalog(),
		)
		if got := config["token"]; got != ciphertext {
			t.Fatalf("unknown plugin token = %v, want ciphertext untouched", got)
		}
	})

	t.Run("non-map config skipped", func(t *testing.T) {
		config := "plain string"
		DecryptPluginConfigWithResolver(config, "key-auth", resolver, mustTestDeclarationCatalog())
		if config != "plain string" {
			t.Fatalf("non-map config mutated to %v", config)
		}
	})

	t.Run("nil and empty configs are no-ops", func(t *testing.T) {
		DecryptPluginConfigs(nil, []string{key}, mustTestDeclarationCatalog())
		DecryptPluginConfigs(map[string]any{}, []string{key}, mustTestDeclarationCatalog())
		DecryptPluginConfigs(
			map[string]any{"key-auth": map[string]any{}},
			nil,
			mustTestDeclarationCatalog(),
		)
	})
}

func TestDecryptPluginConfigsDecryptsAIRateLimitingPasswords(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{"ai-rate-limiting": map[string]any{
		"redis_password":    encryptForTest(t, key, "redis-secret"),
		"sentinel_password": encryptForTest(t, key, "sentinel-secret"),
	}}

	DecryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog())
	for field, want := range map[string]string{
		"redis_password":    "redis-secret",
		"sentinel_password": "sentinel-secret",
	} {
		if got := configs["ai-rate-limiting"].(map[string]any)[field]; got != want {
			t.Errorf("ai-rate-limiting.%s = %v, want %q", field, got, want)
		}
	}
}

func TestEncryptPluginConfigsEncryptsRegisteredFieldsAtRest(t *testing.T) {
	key := "edd1c9f0985e76a2"
	ciphertextShapedPlaintext := "OqkDYcQx4FvgBsxFCybRzg=="
	configs := map[string]any{
		"ai-rate-limiting": map[string]any{
			"redis_password":    ciphertextShapedPlaintext,
			"sentinel_password": "sentinel-secret",
		},
		"basic-auth": map[string]any{"password": "basic-secret"},
		"loggly":     map[string]any{"customer_token": "loggly-secret"},
	}

	if err := EncryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog()); err != nil {
		t.Fatalf("EncryptPluginConfigs() error = %v", err)
	}
	config := configs["ai-rate-limiting"].(map[string]any)
	for field, plaintext := range map[string]string{
		"redis_password":    ciphertextShapedPlaintext,
		"sentinel_password": "sentinel-secret",
	} {
		ciphertext, ok := config[field].(string)
		if !ok || ciphertext == plaintext {
			t.Fatalf("%s = %v, want ciphertext", field, config[field])
		}
		decrypted, err := NewResolver(
			true,
			[]string{key},
		).ResolveForContext(ciphertext, "ai-rate-limiting."+field)
		if err != nil || decrypted != plaintext {
			t.Fatalf("Decrypt(%s) = (%q, %v), want %q", field, decrypted, err, plaintext)
		}
	}
	for pluginName, field := range map[string]string{
		"basic-auth": "password",
		"loggly":     "customer_token",
	} {
		ciphertext := configs[pluginName].(map[string]any)[field].(string)
		if ciphertext == pluginName+"-secret" {
			t.Fatalf("%s.%s remained plaintext", pluginName, field)
		}
	}

	DecryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog())
	if got := configs["basic-auth"].(map[string]any)["password"]; got != "basic-secret" {
		t.Fatalf("basic-auth.password = %v, want runtime plaintext", got)
	}
	if got := configs["loggly"].(map[string]any)["customer_token"]; got != "loggly-secret" {
		t.Fatalf("loggly.customer_token = %v, want runtime plaintext", got)
	}
}

func TestEncryptPluginConfigsRecursivelyEncryptsRegisteredContainers(t *testing.T) {
	key := "edd1c9f0985e76a2"
	alreadyEncrypted := encryptForTest(t, key, "already-encrypted")
	ciphertextShapedPlaintext := "OqkDYcQx4FvgBsxFCybRzg=="
	configs := map[string]any{
		"ai-proxy": map[string]any{"auth": map[string]any{
			"header": map[string]any{
				"Authorization": ciphertextShapedPlaintext,
				"X-Encrypted":   "$encrypted://" + alreadyEncrypted,
			},
		}},
		"feishu-auth": map[string]any{
			"secret_fallbacks": []any{"old-secret-1", "old-secret-2"},
		},
	}

	if err := EncryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog()); err != nil {
		t.Fatalf("EncryptPluginConfigs() error = %v", err)
	}
	header := configs["ai-proxy"].(map[string]any)["auth"].(map[string]any)["header"].(map[string]any)
	if header["Authorization"] == ciphertextShapedPlaintext {
		t.Fatal("ai-proxy.auth.header.Authorization remained plaintext")
	}
	rewrapped, ok := header["X-Encrypted"].(string)
	if !ok ||
		!strings.HasPrefix(rewrapped, encryptedValuePrefix+v2CiphertextPrefix) ||
		rewrapped == encryptedValuePrefix+alreadyEncrypted {
		t.Fatalf(
			"APISIX CBC container leaf = %v, want explicit v2 ciphertext",
			header["X-Encrypted"],
		)
	}
	fallbacks := configs["feishu-auth"].(map[string]any)["secret_fallbacks"].([]any)
	if fallbacks[0] == "old-secret-1" || fallbacks[1] == "old-secret-2" {
		t.Fatalf("feishu-auth.secret_fallbacks remained plaintext: %#v", fallbacks)
	}

	DecryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog())
	if header["Authorization"] != ciphertextShapedPlaintext ||
		header["X-Encrypted"] != "already-encrypted" {
		t.Fatalf("ai-proxy runtime header = %#v, want plaintext leaves", header)
	}
	if fallbacks[0] != "old-secret-1" || fallbacks[1] != "old-secret-2" {
		t.Fatalf("feishu-auth runtime fallbacks = %#v, want plaintext leaves", fallbacks)
	}
}

func TestEncryptPluginConfigsEncryptsElasticsearchAuthorizationHeaderAtRest(t *testing.T) {
	key := "qeddd145sfvddff3"
	for _, headerName := range []string{"Authorization", "authorization", "aUtHoRiZaTiOn"} {
		t.Run(headerName, func(t *testing.T) {
			configs := map[string]any{
				"elasticsearch-logger": map[string]any{
					"headers": map[string]any{
						headerName:  "Basic ZWxhc3RpYzoxMjM0NTY=",
						"X-Cluster": "logs",
					},
				},
			}

			if err := EncryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog()); err != nil {
				t.Fatalf("EncryptPluginConfigs() error = %v", err)
			}
			headers := configs["elasticsearch-logger"].(map[string]any)["headers"].(map[string]any)
			if headers[headerName] == "Basic ZWxhc3RpYzoxMjM0NTY=" {
				t.Fatalf("elasticsearch-logger.headers.%s remained plaintext", headerName)
			}
			if headers["X-Cluster"] != "logs" {
				t.Fatalf(
					"elasticsearch-logger.headers.X-Cluster = %v, want unchanged",
					headers["X-Cluster"],
				)
			}

			DecryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog())
			if headers[headerName] != "Basic ZWxhc3RpYzoxMjM0NTY=" {
				t.Fatalf("runtime %s = %v, want plaintext", headerName, headers[headerName])
			}
		})
	}
}

func TestEncryptRegisteredFieldRejectsInvalidExplicitCiphertext(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"ai-rate-limiting": map[string]any{"redis_password": "$encrypted://not-base64"},
	}
	if err := EncryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog()); err == nil {
		t.Fatal("EncryptPluginConfigs() accepted invalid explicit ciphertext")
	}

	metadata := map[string]any{"master_apikey": "$encrypted://not-base64"}
	if err := EncryptPluginMetadata(
		"azure-functions",
		metadata,
		[]string{key},
		mustTestDeclarationCatalog(),
	); err == nil {
		t.Fatal("EncryptPluginMetadata() accepted invalid explicit ciphertext")
	}
}

func TestEncryptPluginConfigsPreservesValidV2CiphertextWithPrefix(t *testing.T) {
	key := "qeddd145sfvddff3"
	context := "kafka-proxy.sasl.password"
	ciphertext, err := EncryptForContext("kafka-secret", key, context)
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	value := "$encrypted://" + ciphertext
	configs := map[string]any{
		"kafka-proxy": map[string]any{
			"sasl": map[string]any{"password": value},
		},
	}
	if err := EncryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog()); err != nil {
		t.Fatalf("EncryptPluginConfigs() error = %v", err)
	}
	got := configs["kafka-proxy"].(map[string]any)["sasl"].(map[string]any)["password"]
	if got != value {
		t.Fatalf("kafka-proxy.sasl.password = %v, want original v2 value", got)
	}
}

func TestEncryptPluginConfigsRejectsV2CiphertextWithWrongContext(t *testing.T) {
	key := "qeddd145sfvddff3"
	ciphertext, err := EncryptForContext(
		"kafka-secret",
		key,
		"kafka-logger.brokers.*.sasl_config.password",
	)
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	configs := map[string]any{
		"kafka-proxy": map[string]any{
			"sasl": map[string]any{"password": "$encrypted://" + ciphertext},
		},
	}
	if err := EncryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog()); err == nil {
		t.Fatal("EncryptPluginConfigs() accepted v2 ciphertext with wrong context")
	}
}

func TestEncryptPluginConfigsWritesV2ContextualCiphertext(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"kafka-proxy": map[string]any{
			"sasl": map[string]any{"password": "kafka-secret"},
		},
	}
	if err := EncryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog()); err != nil {
		t.Fatalf("EncryptPluginConfigs() error = %v", err)
	}
	ciphertext := configs["kafka-proxy"].(map[string]any)["sasl"].(map[string]any)["password"].(string)
	if !strings.HasPrefix(ciphertext, encryptedValuePrefix+v2CiphertextPrefix) {
		t.Fatalf("ciphertext = %q, want explicit v2 envelope", ciphertext)
	}
	resolver := NewResolver(true, []string{key})
	plaintext, err := resolver.ResolveForContext(ciphertext, "kafka-proxy.sasl.password")
	if err != nil || plaintext != "kafka-secret" {
		t.Fatalf("ResolveForContext() = (%q, %v), want kafka-secret", plaintext, err)
	}
	if err := EncryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog()); err != nil {
		t.Fatalf("EncryptPluginConfigs() second error = %v", err)
	}
	if got := configs["kafka-proxy"].(map[string]any)["sasl"].(map[string]any)["password"]; got != ciphertext {
		t.Fatalf("second EncryptPluginConfigs() = %v, want ciphertext preserved", got)
	}
}

func TestEncryptPluginConfigsTreatsBareV2AsPlaintext(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"kafka-proxy": map[string]any{
			"sasl": map[string]any{"password": "v2:not-ciphertext"},
		},
	}

	if err := EncryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog()); err != nil {
		t.Fatalf("EncryptPluginConfigs() error = %v", err)
	}
	persisted := configs["kafka-proxy"].(map[string]any)["sasl"].(map[string]any)["password"].(string)
	if !strings.HasPrefix(persisted, encryptedValuePrefix+v2CiphertextPrefix) {
		t.Fatalf("persisted password = %q, want explicit v2 wrapper", persisted)
	}

	plaintext, err := NewResolver(true, []string{key}).ResolveForContext(
		persisted,
		"kafka-proxy.sasl.password",
	)
	if err != nil || plaintext != "v2:not-ciphertext" {
		t.Fatalf("ResolveForContext() = (%q, %v), want v2:not-ciphertext", plaintext, err)
	}
}

func TestEncryptPluginConfigsUsesCanonicalContextForWildcardEntries(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"kafka-logger": map[string]any{
			"brokers": []any{
				map[string]any{"sasl_config": map[string]any{"password": "first-secret"}},
				map[string]any{"sasl_config": map[string]any{"password": "second-secret"}},
			},
		},
	}
	if err := EncryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog()); err != nil {
		t.Fatalf("EncryptPluginConfigs() error = %v", err)
	}
	brokers := configs["kafka-logger"].(map[string]any)["brokers"].([]any)
	resolver := NewResolver(true, []string{key})
	for i, want := range []string{"first-secret", "second-secret"} {
		password := brokers[i].(map[string]any)["sasl_config"].(map[string]any)["password"].(string)
		got, err := resolver.ResolveForContext(
			password,
			"kafka-logger.brokers.*.sasl_config.password",
		)
		if err != nil || got != want {
			t.Fatalf("broker[%d] ResolveForContext() = (%q, %v), want %q", i, got, err, want)
		}
		if _, err := resolver.ResolveForContext(password, "kafka-logger.brokers.0.sasl_config.password"); err == nil {
			t.Fatalf("broker[%d] accepted runtime-index context", i)
		}
	}
}

func TestEncryptPluginMetadataDecryptsErrorLogLoggerPasswords(
	t *testing.T,
) {
	key := "qeddd145sfvddff3"
	metadata := map[string]any{
		"clickhouse": map[string]any{
			"password": "clickhouse-secret",
			"user":     "default",
		},
		"kafka": map[string]any{
			"brokers": []any{
				map[string]any{
					"host": "127.0.0.1",
					"sasl_config": map[string]any{
						"user":     "kafka-user",
						"password": "kafka-secret",
					},
				},
			},
		},
	}

	if err := EncryptPluginMetadata(
		"error-log-logger",
		metadata,
		[]string{key},
		mustTestDeclarationCatalog(),
	); err != nil {
		t.Fatalf("EncryptPluginMetadata() error = %v", err)
	}
	clickhouse := metadata["clickhouse"].(map[string]any)
	broker := metadata["kafka"].(map[string]any)["brokers"].([]any)[0].(map[string]any)
	sasl := broker["sasl_config"].(map[string]any)
	if clickhouse["password"] == "clickhouse-secret" {
		t.Fatal("clickhouse.password remained plaintext")
	}
	if sasl["password"] == "kafka-secret" {
		t.Fatal("kafka.brokers[].sasl_config.password remained plaintext")
	}
	if clickhouse["user"] != "default" || broker["host"] != "127.0.0.1" ||
		sasl["user"] != "kafka-user" {
		t.Fatalf("non-secret metadata changed: clickhouse=%#v broker=%#v", clickhouse, broker)
	}

	DecryptPluginMetadata("error-log-logger", metadata, []string{key}, mustTestDeclarationCatalog())
	if clickhouse["password"] != "clickhouse-secret" || sasl["password"] != "kafka-secret" {
		t.Fatalf("runtime passwords = %v/%v, want plaintext", clickhouse["password"], sasl["password"])
	}
}

func TestDecryptPluginConfigsSupportsFeishuAuthSecretFallbacks(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"feishu-auth": map[string]any{
			"secret_fallbacks": []any{
				encryptForTest(t, key, "old-secret-1"),
				encryptForTest(t, key, "old-secret-2"),
			},
		},
	}

	DecryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog())
	fallbacks := configs["feishu-auth"].(map[string]any)["secret_fallbacks"].([]any)
	if fallbacks[0] != "old-secret-1" || fallbacks[1] != "old-secret-2" {
		t.Fatalf("feishu-auth secret_fallbacks = %#v, want plaintext values", fallbacks)
	}
}

func TestDecryptPluginConfigsSupportsCasdoorClientSecretFallbacks(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"authz-casdoor": map[string]any{
			"client_secret_fallbacks": []any{
				encryptForTest(t, key, "old-secret-1"),
				encryptForTest(t, key, "old-secret-2"),
			},
		},
	}

	DecryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog())
	fallbacks := configs["authz-casdoor"].(map[string]any)["client_secret_fallbacks"].([]any)
	if fallbacks[0] != "old-secret-1" || fallbacks[1] != "old-secret-2" {
		t.Fatalf("authz-casdoor client_secret_fallbacks = %#v, want plaintext values", fallbacks)
	}
}

func TestDecryptPluginConfigsSupportsAIMapsAndInstanceArrays(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"ai-proxy": map[string]any{"auth": map[string]any{
			"header": map[string]any{"Authorization": encryptForTest(t, key, "Bearer secret")},
			"aws":    map[string]any{"secret_access_key": encryptForTest(t, key, "aws-secret")},
		}},
		"ai-proxy-multi": map[string]any{"instances": []any{
			map[string]any{"auth": map[string]any{
				"query": map[string]any{"api-key": encryptForTest(t, key, "query-secret")},
			}},
		}},
		"ai-rag": map[string]any{
			"embeddings_provider": map[string]any{"azure_openai": map[string]any{
				"api_key": encryptForTest(t, key, "embedding-secret"),
			}},
		},
	}

	DecryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog())
	proxyAuth := configs["ai-proxy"].(map[string]any)["auth"].(map[string]any)
	if proxyAuth["header"].(map[string]any)["Authorization"] != "Bearer secret" ||
		proxyAuth["aws"].(map[string]any)["secret_access_key"] != "aws-secret" {
		t.Fatalf("ai-proxy auth = %#v", proxyAuth)
	}
	instance := configs["ai-proxy-multi"].(map[string]any)["instances"].([]any)[0].(map[string]any)
	if instance["auth"].(map[string]any)["query"].(map[string]any)["api-key"] != "query-secret" {
		t.Fatalf("ai-proxy-multi instance = %#v", instance)
	}
	rawRAG := configs["ai-rag"].(map[string]any)
	if rawRAG["embeddings_provider"].(map[string]any)["azure_openai"].(map[string]any)["api_key"] !=
		"embedding-secret" {
		t.Fatalf("ai-rag config = %#v", rawRAG)
	}
}

func TestDecryptPluginConfigsSupportsServerlessCredentials(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"aws-lambda": map[string]any{"authorization": map[string]any{
			"apikey": encryptForTest(t, key, "aws-api-key"),
			"iam": map[string]any{
				"accesskey": encryptForTest(t, key, "aws-access-key"),
				"secretkey": encryptForTest(t, key, "aws-secret-key"),
			},
		}},
		"azure-functions": map[string]any{"authorization": map[string]any{
			"apikey": encryptForTest(t, key, "azure-api-key"),
		}},
		"openfunction": map[string]any{"authorization": map[string]any{
			"service_token": encryptForTest(t, key, "openfunction-token"),
		}},
		"openwhisk": map[string]any{
			"service_token": encryptForTest(t, key, "openwhisk-token"),
		},
	}

	DecryptPluginConfigs(configs, []string{key}, mustTestDeclarationCatalog())
	aws := configs["aws-lambda"].(map[string]any)["authorization"].(map[string]any)
	if aws["apikey"] != "aws-api-key" ||
		aws["iam"].(map[string]any)["accesskey"] != "aws-access-key" ||
		aws["iam"].(map[string]any)["secretkey"] != "aws-secret-key" {
		t.Fatalf("aws-lambda authorization = %#v", aws)
	}
	azure := configs["azure-functions"].(map[string]any)["authorization"].(map[string]any)
	if azure["apikey"] != "azure-api-key" {
		t.Fatalf("azure-functions authorization = %#v", azure)
	}
	openFunction := configs["openfunction"].(map[string]any)["authorization"].(map[string]any)
	if openFunction["service_token"] != "openfunction-token" {
		t.Fatalf("openfunction authorization = %#v", openFunction)
	}
	openWhisk := configs["openwhisk"].(map[string]any)
	if openWhisk["service_token"] != "openwhisk-token" {
		t.Fatalf("openwhisk config = %#v", openWhisk)
	}
}

func encryptForTest(t *testing.T, key string, value string) string {
	t.Helper()
	padding := aes.BlockSize - len(value)%aes.BlockSize
	padded := append([]byte(value), make([]byte, padding)...)
	for i := len(padded) - padding; i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}
