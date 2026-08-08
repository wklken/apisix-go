package data_encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
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

	DecryptPluginConfigs(configs, []string{"old-keyring-item", key})
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
			DecryptPluginConfigWithResolver(config, name, resolver)
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
		DecryptPluginConfigWithResolver(config, "not-a-plugin", resolver)
		if got := config["token"]; got != ciphertext {
			t.Fatalf("unknown plugin token = %v, want ciphertext untouched", got)
		}
	})

	t.Run("non-map config skipped", func(t *testing.T) {
		config := "plain string"
		DecryptPluginConfigWithResolver(config, "key-auth", resolver)
		if config != "plain string" {
			t.Fatalf("non-map config mutated to %v", config)
		}
	})

	t.Run("nil and empty configs are no-ops", func(t *testing.T) {
		DecryptPluginConfigs(nil, []string{key})
		DecryptPluginConfigs(map[string]any{}, []string{key})
		DecryptPluginConfigs(map[string]any{"key-auth": map[string]any{}}, nil)
	})
}

func TestDecryptPluginConfigsPreservesAIRateLimitingRedisPassword(t *testing.T) {
	key := "qeddd145sfvddff3"
	ciphertext := encryptForTest(t, key, "redis-secret")
	sentinelCiphertext := encryptForTest(t, key, "sentinel-secret")
	configs := map[string]any{"ai-rate-limiting": map[string]any{
		"redis_password":    ciphertext,
		"sentinel_password": sentinelCiphertext,
	}}

	DecryptPluginConfigs(configs, []string{key})
	if got := configs["ai-rate-limiting"].(map[string]any)["redis_password"]; got != ciphertext {
		t.Fatalf("ai-rate-limiting.redis_password = %v, want ciphertext retained for runtime resolution", got)
	}
	if got := configs["ai-rate-limiting"].(map[string]any)["sentinel_password"]; got != sentinelCiphertext {
		t.Fatalf("ai-rate-limiting.sentinel_password = %v, want ciphertext retained for runtime resolution", got)
	}
	if !IsStrictPluginField("ai-rate-limiting", "redis_password") {
		t.Fatal("ai-rate-limiting.redis_password must be strict to remain encrypted at rest")
	}
	if !IsStrictPluginField("ai-rate-limiting", "sentinel_password") {
		t.Fatal("ai-rate-limiting.sentinel_password must be strict to remain encrypted at rest")
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

	if err := EncryptPluginConfigs(configs, []string{key}); err != nil {
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
		decrypted, err := Decrypt(ciphertext, []string{key})
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

	DecryptPluginConfigs(configs, []string{key})
	if got := configs["basic-auth"].(map[string]any)["password"]; got != "basic-secret" {
		t.Fatalf("basic-auth.password = %v, want runtime plaintext", got)
	}
	if got := configs["loggly"].(map[string]any)["customer_token"]; got == "loggly-secret" {
		t.Fatalf("loggly.customer_token = %v, want strict ciphertext for plugin-local resolution", got)
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

	if err := EncryptPluginConfigs(configs, []string{key}); err != nil {
		t.Fatalf("EncryptPluginConfigs() error = %v", err)
	}
	header := configs["ai-proxy"].(map[string]any)["auth"].(map[string]any)["header"].(map[string]any)
	if header["Authorization"] == ciphertextShapedPlaintext {
		t.Fatal("ai-proxy.auth.header.Authorization remained plaintext")
	}
	if header["X-Encrypted"] != alreadyEncrypted {
		t.Fatal("already encrypted container leaf was encrypted again")
	}
	fallbacks := configs["feishu-auth"].(map[string]any)["secret_fallbacks"].([]any)
	if fallbacks[0] == "old-secret-1" || fallbacks[1] == "old-secret-2" {
		t.Fatalf("feishu-auth.secret_fallbacks remained plaintext: %#v", fallbacks)
	}

	DecryptPluginConfigs(configs, []string{key})
	if header["Authorization"] != ciphertextShapedPlaintext || header["X-Encrypted"] != "already-encrypted" {
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

			if err := EncryptPluginConfigs(configs, []string{key}); err != nil {
				t.Fatalf("EncryptPluginConfigs() error = %v", err)
			}
			headers := configs["elasticsearch-logger"].(map[string]any)["headers"].(map[string]any)
			if headers[headerName] == "Basic ZWxhc3RpYzoxMjM0NTY=" {
				t.Fatalf("elasticsearch-logger.headers.%s remained plaintext", headerName)
			}
			if headers["X-Cluster"] != "logs" {
				t.Fatalf("elasticsearch-logger.headers.X-Cluster = %v, want unchanged", headers["X-Cluster"])
			}

			DecryptPluginConfigs(configs, []string{key})
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
	if err := EncryptPluginConfigs(configs, []string{key}); err == nil {
		t.Fatal("EncryptPluginConfigs() accepted invalid explicit ciphertext")
	}

	metadata := map[string]any{"master_apikey": "$encrypted://not-base64"}
	if err := EncryptPluginMetadata("azure-functions", metadata, []string{key}); err == nil {
		t.Fatal("EncryptPluginMetadata() accepted invalid explicit ciphertext")
	}
}

func TestEncryptPluginMetadataPreservesStrictErrorLogLoggerPasswordsForPluginResolution(t *testing.T) {
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

	if err := EncryptPluginMetadata("error-log-logger", metadata, []string{key}); err != nil {
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
	clickhouseCiphertext := clickhouse["password"]
	kafkaCiphertext := sasl["password"]
	if clickhouse["user"] != "default" || broker["host"] != "127.0.0.1" || sasl["user"] != "kafka-user" {
		t.Fatalf("non-secret metadata changed: clickhouse=%#v broker=%#v", clickhouse, broker)
	}

	DecryptPluginMetadata("error-log-logger", metadata, []string{key})
	if clickhouse["password"] != clickhouseCiphertext || sasl["password"] != kafkaCiphertext {
		t.Fatalf(
			"strict runtime passwords = %v/%v, want ciphertext retained for plugin resolution",
			clickhouse["password"],
			sasl["password"],
		)
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

	DecryptPluginConfigs(configs, []string{key})
	fallbacks := configs["feishu-auth"].(map[string]any)["secret_fallbacks"].([]any)
	if fallbacks[0] != "old-secret-1" || fallbacks[1] != "old-secret-2" {
		t.Fatalf("feishu-auth secret_fallbacks = %#v, want plaintext values", fallbacks)
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

	DecryptPluginConfigs(configs, []string{key})
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

	DecryptPluginConfigs(configs, []string{key})
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

func TestDecryptPluginConfigsPreservesStrictPluginFields(t *testing.T) {
	key := "qeddd145sfvddff3"
	clickhousePassword := encryptForTest(t, key, "clickhouse-secret")
	csrfKey := encryptForTest(t, key, "csrf-secret")
	googlePrivateKey := encryptForTest(t, key, "google-private-key")
	httpLoggerAuthHeader := encryptForTest(t, key, "Bearer logger")
	kafkaLoggerPassword := encryptForTest(t, key, "kafka-secret")
	kafkaProxyPassword := encryptForTest(t, key, "proxy-secret")
	elasticsearchPassword := encryptForTest(t, key, "elasticsearch-secret")
	errorLogClickhousePassword := encryptForTest(t, key, "error-clickhouse-secret")
	errorLogKafkaPassword := encryptForTest(t, key, "error-kafka-secret")
	rocketMQSecretKey := encryptForTest(t, key, "rocketmq-secret")
	slsAccessKeySecret := encryptForTest(t, key, "sls-secret")
	logglyCustomerToken := encryptForTest(t, key, "loggly-token")
	lagoToken := encryptForTest(t, key, "lago-token")
	splunkToken := encryptForTest(t, key, "splunk-token")
	tencentCLSSecretKey := encryptForTest(t, key, "cls-secret")
	configs := map[string]any{
		"clickhouse-logger": map[string]any{
			"password": clickhousePassword,
		},
		"csrf": map[string]any{
			"key": csrfKey,
		},
		"google-cloud-logging": map[string]any{
			"auth_config": map[string]any{"private_key": googlePrivateKey},
		},
		"elasticsearch-logger": map[string]any{
			"auth": map[string]any{"password": elasticsearchPassword},
		},
		"error-log-logger": map[string]any{
			"clickhouse": map[string]any{"password": errorLogClickhousePassword},
			"kafka": map[string]any{"brokers": []any{map[string]any{
				"sasl_config": map[string]any{"password": errorLogKafkaPassword},
			}}},
		},
		"http-logger": map[string]any{
			"auth_header": httpLoggerAuthHeader,
		},
		"kafka-logger": map[string]any{
			"brokers": []any{map[string]any{
				"sasl_config": map[string]any{"password": kafkaLoggerPassword},
			}},
		},
		"kafka-proxy": map[string]any{
			"sasl": map[string]any{"password": kafkaProxyPassword},
		},
		"response-rewrite": map[string]any{
			"body": encryptForTest(t, key, "rewritten-body"),
		},
		"rocketmq-logger": map[string]any{
			"secret_key": rocketMQSecretKey,
		},
		"sls-logger": map[string]any{
			"access_key_secret": slsAccessKeySecret,
		},
		"loggly": map[string]any{
			"customer_token": logglyCustomerToken,
		},
		"lago": map[string]any{
			"token": lagoToken,
		},
		"splunk-hec-logging": map[string]any{
			"endpoint": map[string]any{"token": splunkToken},
		},
		"tencent-cloud-cls": map[string]any{
			"secret_key": tencentCLSSecretKey,
		},
	}

	DecryptPluginConfigs(configs, []string{key})
	if got := configs["clickhouse-logger"].(map[string]any)["password"]; got != clickhousePassword {
		t.Fatalf("clickhouse-logger.password = %v, want ciphertext preserved", got)
	}
	if got := configs["csrf"].(map[string]any)["key"]; got != csrfKey {
		t.Fatalf("csrf.key = %v, want ciphertext preserved", got)
	}
	if got := configs["elasticsearch-logger"].(map[string]any)["auth"].(map[string]any)["password"]; got != elasticsearchPassword {
		t.Fatalf("elasticsearch-logger.auth.password = %v, want ciphertext preserved", got)
	}
	if got := configs["google-cloud-logging"].(map[string]any)["auth_config"].(map[string]any)["private_key"]; got != googlePrivateKey {
		t.Fatalf("google-cloud-logging.auth_config.private_key = %v, want ciphertext preserved", got)
	}
	errorLog := configs["error-log-logger"].(map[string]any)
	if got := errorLog["clickhouse"].(map[string]any)["password"]; got != errorLogClickhousePassword {
		t.Fatalf("error-log-logger.clickhouse.password = %v, want ciphertext preserved", got)
	}
	if got := errorLog["kafka"].(map[string]any)["brokers"].([]any)[0].(map[string]any)["sasl_config"].(map[string]any)["password"]; got != errorLogKafkaPassword {
		t.Fatalf("error-log-logger.kafka broker password = %v, want ciphertext preserved", got)
	}
	if got := configs["sls-logger"].(map[string]any)["access_key_secret"]; got != slsAccessKeySecret {
		t.Fatalf("sls-logger.access_key_secret = %v, want ciphertext preserved", got)
	}
	if got := configs["rocketmq-logger"].(map[string]any)["secret_key"]; got != rocketMQSecretKey {
		t.Fatalf("rocketmq-logger.secret_key = %v, want ciphertext preserved", got)
	}
	if got := configs["loggly"].(map[string]any)["customer_token"]; got != logglyCustomerToken {
		t.Fatalf("loggly.customer_token = %v, want ciphertext preserved", got)
	}
	if got := configs["lago"].(map[string]any)["token"]; got != lagoToken {
		t.Fatalf("lago.token = %v, want ciphertext preserved", got)
	}
	if got := configs["splunk-hec-logging"].(map[string]any)["endpoint"].(map[string]any)["token"]; got != splunkToken {
		t.Fatalf("splunk-hec-logging.endpoint.token = %v, want ciphertext preserved", got)
	}
	if got := configs["tencent-cloud-cls"].(map[string]any)["secret_key"]; got != tencentCLSSecretKey {
		t.Fatalf("tencent-cloud-cls.secret_key = %v, want ciphertext preserved", got)
	}
	if got := configs["http-logger"].(map[string]any)["auth_header"]; got != httpLoggerAuthHeader {
		t.Fatalf("http-logger.auth_header = %v, want ciphertext preserved", got)
	}
	kafka := configs["kafka-logger"].(map[string]any)
	brokers := kafka["brokers"].([]any)
	sasl := brokers[0].(map[string]any)["sasl_config"].(map[string]any)
	if got := sasl["password"]; got != kafkaLoggerPassword {
		t.Fatalf("kafka-logger broker password = %v, want ciphertext preserved", got)
	}
	proxySASL := configs["kafka-proxy"].(map[string]any)["sasl"].(map[string]any)
	if got := proxySASL["password"]; got != kafkaProxyPassword {
		t.Fatalf("kafka-proxy password = %v, want ciphertext preserved", got)
	}
	if got := configs["response-rewrite"].(map[string]any)["body"]; got != "rewritten-body" {
		t.Fatalf("response-rewrite.body = %v, want decrypted value", got)
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
