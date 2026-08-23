package data_encryption

import (
	"reflect"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

type legacyDeclarationKey struct {
	factory string
	field   string
}

var expectedPluginFields = map[string][]string{
	"ai-aliyun-content-moderation": {"access_key_id", "access_key_secret"},
	"ai-aws-content-moderation": {
		"comprehend.access_key_id", "comprehend.secret_access_key", "comprehend.session_token",
	},
	"ai-proxy": {
		"auth.header", "auth.query", "auth.gcp.service_account_json", "auth.aws.secret_access_key",
		"auth.aws.session_token",
	},
	"ai-proxy-multi": {
		"instances.*.auth.header", "instances.*.auth.query", "instances.*.auth.gcp.service_account_json",
		"instances.*.auth.aws.secret_access_key", "instances.*.auth.aws.session_token",
	},
	"ai-rag": {
		"embeddings_provider.azure_openai.api_key", "vector_search_provider.azure_ai_search.api_key",
	},
	"ai-request-rewrite": {
		"auth.header", "auth.query", "auth.gcp.service_account_json", "auth.aws.secret_access_key",
		"auth.aws.session_token",
	},
	"ai-rate-limiting": {"redis_password", "sentinel_password"},
	"authz-keycloak":   {"client_secret"},
	"authz-casdoor":    {"client_secret", "client_secret_fallbacks"},
	"aws-lambda": {
		"authorization.apikey",
		"authorization.iam.accesskey",
		"authorization.iam.secretkey",
	},
	"azure-functions": {"authorization.apikey"},
	"basic-auth":      {"password"},
	"cas-auth":        {"cookie.secret"},
	"dingtalk-auth":   {"app_secret", "secret"},
	"feishu-auth":     {"app_secret", "secret", "secret_fallbacks"},
	"hmac-auth":       {"secret"},
	"http-logger":     {"auth_header"},
	"jwe-decrypt":     {"key", "secret"},
	"jwt-auth":        {"secret", "private_key"},
	"kafka-logger":    {"brokers.*.sasl_config.password"},
	"kafka-proxy":     {"sasl.password"},
	"key-auth":        {"key"},
	"ldap-auth":       {"user_dn"},
	"openid-connect": {
		"client_secret",
		"client_rsa_private_key",
		"public_key",
		"session.secret",
		"session.redis.password",
	},
	"openfunction":         {"authorization.service_token"},
	"openwhisk":            {"service_token"},
	"clickhouse-logger":    {"password", "user"},
	"csrf":                 {"key"},
	"elasticsearch-logger": {"auth.password", "headers.Authorization"},
	"error-log-logger":     {"clickhouse.password", "kafka.brokers.*.sasl_config.password"},
	"google-cloud-logging": {"auth_config.private_key"},
	"lago":                 {"token"},
	"loggly":               {"customer_token"},
	"response-rewrite":     {"body", "body_secret"},
	"rocketmq-logger":      {"secret_key"},
	"saml-auth":            {"sp_private_key", "secret"},
	"sls-logger":           {"access_key_secret"},
	"splunk-hec-logging":   {"endpoint.token"},
	"tencent-cloud-cls":    {"secret_key"},
	"limit-count": {
		"key",
		"redis_host",
		"redis_config.redis_host",
		"redis_cluster_nodes",
		"redis_cluster_config.redis_cluster_nodes",
	},
	"oas-validator": {"spec", "spec_url_request_headers"},
}

var expectedStrictPluginFields = map[string][]string{
	"ai-rate-limiting":     {"redis_password", "sentinel_password"},
	"clickhouse-logger":    {"password"},
	"csrf":                 {"key"},
	"elasticsearch-logger": {"auth.password"},
	"error-log-logger":     {"clickhouse.password", "kafka.brokers.*.sasl_config.password"},
	"google-cloud-logging": {"auth_config.private_key"},
	"http-logger":          {"auth_header"},
	"kafka-logger":         {"brokers.*.sasl_config.password"},
	"kafka-proxy":          {"sasl.password"},
	"lago":                 {"token"},
	"loggly":               {"customer_token"},
	"rocketmq-logger":      {"secret_key"},
	"response-rewrite":     {"body_secret"},
	"sls-logger":           {"access_key_secret"},
	"splunk-hec-logging":   {"endpoint.token"},
	"tencent-cloud-cls":    {"secret_key"},
}

var expectedPluginMetadataFields = map[string][]string{
	"azure-functions":  {"master_apikey"},
	"error-log-logger": {"clickhouse.password", "kafka.brokers.*.sasl_config.password"},
}

var expectedStrictPluginMetadataFields = map[string][]string{
	"error-log-logger": {"clickhouse.password", "kafka.brokers.*.sasl_config.password"},
}

var expectedConsumerFields = map[string][]string{
	"basic-auth":  {"username", "password"},
	"key-auth":    {"key"},
	"jwt-auth":    {"key", "secret", "public_key", "private_key", "algorithm"},
	"hmac-auth":   {"key_id", "secret_key"},
	"ldap-auth":   {"user_dn"},
	"jwe-decrypt": {"key", "secret"},
	"wolf-rbac":   {"appid", "header_prefix", "server", "wolf_url"},
}

func TestSecretDeclarationCatalogParity(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}

	wantConfig := make(map[legacyDeclarationKey]struct{})
	wantOptionalConfig := make(map[legacyDeclarationKey]struct{})
	wantStrictConfig := make(map[legacyDeclarationKey]struct{})
	for factory, fields := range expectedPluginFields {
		for _, field := range fields {
			key := legacyDeclarationKey{factory: factory, field: field}
			wantConfig[key] = struct{}{}
			if !slices.Contains(expectedStrictPluginFields[factory], field) {
				wantOptionalConfig[key] = struct{}{}
			}
		}
	}
	for factory, fields := range expectedStrictPluginFields {
		for _, field := range fields {
			wantStrictConfig[legacyDeclarationKey{factory: factory, field: field}] = struct{}{}
		}
	}

	wantMetadata := make(map[legacyDeclarationKey]struct{})
	wantOptionalMetadata := make(map[legacyDeclarationKey]struct{})
	wantStrictMetadata := make(map[legacyDeclarationKey]struct{})
	for factory, fields := range expectedPluginMetadataFields {
		for _, field := range fields {
			key := legacyDeclarationKey{factory: factory, field: field}
			wantMetadata[key] = struct{}{}
			if !slices.Contains(expectedStrictPluginMetadataFields[factory], field) {
				wantOptionalMetadata[key] = struct{}{}
			}
		}
	}
	for factory, fields := range expectedStrictPluginMetadataFields {
		for _, field := range fields {
			wantStrictMetadata[legacyDeclarationKey{factory: factory, field: field}] = struct{}{}
		}
	}

	gotConfig := make(map[legacyDeclarationKey]struct{})
	gotOptionalConfig := make(map[legacyDeclarationKey]struct{})
	gotStrictConfig := make(map[legacyDeclarationKey]struct{})
	gotMetadata := make(map[legacyDeclarationKey]struct{})
	gotOptionalMetadata := make(map[legacyDeclarationKey]struct{})
	gotStrictMetadata := make(map[legacyDeclarationKey]struct{})
	wantConsumer := make(map[legacyDeclarationKey]struct{})
	for factory, fields := range expectedConsumerFields {
		for _, field := range fields {
			wantConsumer[legacyDeclarationKey{factory: factory, field: field}] = struct{}{}
		}
	}
	gotConsumer := make(map[legacyDeclarationKey]struct{})
	for _, declaration := range catalog.Declarations() {
		key := legacyDeclarationKey{factory: declaration.Factory, field: declaration.Field}
		switch declaration.Source {
		case capability.SecretPluginConfig:
			gotConfig[key] = struct{}{}
			if declaration.Strict {
				gotStrictConfig[key] = struct{}{}
			} else {
				gotOptionalConfig[key] = struct{}{}
			}
		case capability.SecretPluginMetadata:
			gotMetadata[key] = struct{}{}
			if declaration.Strict {
				gotStrictMetadata[key] = struct{}{}
			} else {
				gotOptionalMetadata[key] = struct{}{}
			}
		case capability.SecretConsumerConfig:
			gotConsumer[key] = struct{}{}
			if declaration.Strict {
				t.Errorf("consumer declaration %s/%s unexpectedly uses strict at-rest policy", key.factory, key.field)
			}
		default:
			t.Fatalf("catalog contains unknown declaration source %q", declaration.Source)
		}
	}

	checks := []struct {
		name string
		got  map[legacyDeclarationKey]struct{}
		want map[legacyDeclarationKey]struct{}
	}{
		{name: "config", got: gotConfig, want: wantConfig},
		{name: "config optional", got: gotOptionalConfig, want: wantOptionalConfig},
		{name: "config strict", got: gotStrictConfig, want: wantStrictConfig},
		{name: "metadata", got: gotMetadata, want: wantMetadata},
		{name: "metadata optional", got: gotOptionalMetadata, want: wantOptionalMetadata},
		{name: "metadata strict", got: gotStrictMetadata, want: wantStrictMetadata},
		{name: "consumer", got: gotConsumer, want: wantConsumer},
	}
	for _, check := range checks {
		if !reflect.DeepEqual(check.got, check.want) {
			t.Errorf(
				"%s declarations differ from expected manifest parity: got %#v, want %#v",
				check.name,
				check.got,
				check.want,
			)
		}
	}
}
